#!/usr/bin/env bash
# Create (or update) a GitOps pin pull request in one or more consumer repos.
#
# Required env:
#   VERSION          Published version tag, e.g. v0.1.20
#   SOURCE_REF       Public source tag or SHA used for the release
#   SOURCE_SHA       Resolved full SHA for SOURCE_REF
#   GH_TOKEN         Token with contents:write + pull_requests:write on consumers
#
# Optional env:
#   CONSUMERS_FILE   Path to config/consumers.yaml (default: config/consumers.yaml)
#   CONSUMER_REPOS   Comma-separated owner/repo filter (empty = all in config)
#   RELEASE_RUN_URL  Link back to the release workflow run
#   WORK_DIR         Scratch directory (default: $RUNNER_TEMP/gitops-pin or /tmp)
#   DRY_RUN          If "true", print planned edits and skip push/PR
#
# Exit codes:
#   0  all requested consumers succeeded (or no-op when already pinned)
#   1  configuration / input error
#   2  one or more consumers failed (partial success reported in summary)

set -euo pipefail

VERSION="${VERSION:-}"
SOURCE_REF="${SOURCE_REF:-}"
SOURCE_SHA="${SOURCE_SHA:-}"
CONSUMERS_FILE="${CONSUMERS_FILE:-config/consumers.yaml}"
CONSUMER_REPOS="${CONSUMER_REPOS:-}"
RELEASE_RUN_URL="${RELEASE_RUN_URL:-${PROMOTE_RUN_URL:-}}"
DRY_RUN="${DRY_RUN:-false}"
WORK_DIR="${WORK_DIR:-${RUNNER_TEMP:-/tmp}/gitops-pin-$$}"

if [[ -z "${VERSION}" || -z "${SOURCE_REF}" || -z "${SOURCE_SHA}" ]]; then
  echo "::error::VERSION, SOURCE_REF, and SOURCE_SHA are required"
  exit 1
fi

if [[ -z "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
  echo "::error::GH_TOKEN (or GITHUB_TOKEN) is required to open consumer pin PRs"
  exit 1
fi
export GH_TOKEN="${GH_TOKEN:-${GITHUB_TOKEN}}"
export GH_PROMPT_DISABLED=1
export GLAMOUR_STYLE=notty

if [[ ! -f "${CONSUMERS_FILE}" ]]; then
  echo "::error::Consumers file not found: ${CONSUMERS_FILE}"
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "::error::gh CLI is required"
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1 && ! command -v python >/dev/null 2>&1; then
  echo "::error::python3 is required to parse consumers.yaml and apply pins"
  exit 1
fi

PYTHON_BIN="$(command -v python3 || command -v python)"
mkdir -p "${WORK_DIR}"
export WORK_DIR VERSION SOURCE_REF SOURCE_SHA CONSUMERS_FILE CONSUMER_REPOS RELEASE_RUN_URL DRY_RUN

# Python orchestrates parse → apply → git/gh so multi-line pin blocks stay intact.
"${PYTHON_BIN}" - <<'PY'
from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

VERSION = os.environ["VERSION"]
SOURCE_REF = os.environ["SOURCE_REF"]
SOURCE_SHA = os.environ["SOURCE_SHA"]
CONSUMERS_FILE = Path(os.environ["CONSUMERS_FILE"])
CONSUMER_REPOS = os.environ.get("CONSUMER_REPOS", "").strip()
RELEASE_RUN_URL = os.environ.get("RELEASE_RUN_URL", "")
DRY_RUN = os.environ.get("DRY_RUN", "false").lower() == "true"
WORK_DIR = Path(os.environ["WORK_DIR"])
GH_TOKEN = os.environ["GH_TOKEN"]


def run(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=False, text=True, capture_output=True, **kwargs)


def run_checked(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    proc = run(cmd, **kwargs)
    if proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(cmd)}\n"
            f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
    return proc


def parse_consumers(path: Path, filters: set[str]) -> list[dict]:
    text = path.read_text(encoding="utf-8")
    blocks = re.split(r"(?m)^\s*-\s+name:\s*", text)
    consumers: list[dict] = []
    for block in blocks[1:]:
        name_m = re.match(r"(\S+)", block)
        if not name_m:
            continue
        name = name_m.group(1)
        repo_m = re.search(r"(?m)^\s*repository:\s*(\S+)", block)
        base_m = re.search(r"(?m)^\s*base_branch:\s*(\S+)", block)
        if not repo_m:
            raise SystemExit(f"consumer '{name}' missing repository")
        repo = repo_m.group(1)
        base = base_m.group(1) if base_m else "main"
        if filters and repo.lower() not in filters and name.lower() not in filters:
            continue
        pins_m = re.search(r"(?ms)^\s*pins:\s*\n(.*)$", block)
        pins_raw = pins_m.group(1) if pins_m else ""
        consumers.append(
            {
                "name": name,
                "repository": repo,
                "base_branch": base,
                "pins": parse_pins(pins_raw),
            }
        )
    return consumers


def parse_pins(pins_raw: str) -> list[dict]:
    entries: list[dict] = []
    current: dict | None = None
    for line in pins_raw.splitlines():
        m = re.match(r"^\s*-\s+path:\s*(\S+)\s*$", line)
        if m:
            if current:
                entries.append(current)
            current = {"path": m.group(1), "kind": "kustomize", "variable": ""}
            continue
        if current is None:
            continue
        km = re.match(r"^\s*kind:\s*(\S+)\s*$", line)
        if km:
            current["kind"] = km.group(1)
            continue
        vm = re.match(r"^\s*variable:\s*(\S+)\s*$", line)
        if vm:
            current["variable"] = vm.group(1)
    if current:
        entries.append(current)
    return entries


def apply_pins(repo_dir: Path, pins: list[dict]) -> list[str]:
    if not pins:
        raise RuntimeError("no pin entries found for consumer")
    changed: list[str] = []
    for entry in pins:
        path = repo_dir / entry["path"]
        if not path.is_file():
            raise RuntimeError(f"missing pin file: {entry['path']}")
        original = path.read_text(encoding="utf-8")
        text = original
        kind = entry["kind"]
        if kind == "kustomize":
            text = re.sub(r"(\?ref=)[^\s\"']+", rf"\g<1>{VERSION}", text)
            text = re.sub(r"(newTag:\s*)[^\s\"']+", rf"\g<1>{VERSION}", text)
        elif kind == "terraform_default":
            var = entry.get("variable") or ""
            if not var:
                raise RuntimeError(f"terraform_default pin missing variable for {entry['path']}")
            pattern = (
                rf'(variable\s+"{re.escape(var)}"\s*\{{.*?default\s*=\s*")'
                rf'[^"]*'
                rf'(")'
            )
            new_text, n = re.subn(pattern, rf"\g<1>{VERSION}\g<2>", text, count=1, flags=re.S)
            if n != 1:
                raise RuntimeError(
                    f"could not update terraform default for {var} in {entry['path']}"
                )
            text = new_text
        else:
            raise RuntimeError(f"unsupported pin kind: {kind}")
        if text != original:
            path.write_text(text, encoding="utf-8", newline="\n")
            changed.append(entry["path"])
            print(f"updated {entry['path']}")
        else:
            print(f"unchanged {entry['path']}")
    return changed


def write_summary(rows: list[tuple[str, str, str]], failed: int, processed: int) -> None:
    lines = [
        "## GitOps pin PR status",
        "",
        "| Consumer | Result | Detail |",
        "| --- | --- | --- |",
    ]
    for repo, result, detail in rows:
        lines.append(f"| `{repo}` | {result} | {detail} |")
    lines.append("")
    lines.append("### Outcome")
    if failed:
        lines.append(
            f"Release pin step **failed** for {failed}/{processed} consumer(s). "
            "Published artifacts (if any) were not rolled back; resolve pin failures "
            "before merging consumer PRs."
        )
    else:
        lines.append(f"Pin step completed for {processed} consumer(s).")
    body = "\n".join(lines) + "\n"
    print(body)
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as fh:
            fh.write(body)


def main() -> int:
    filters = {x.strip().lower() for x in CONSUMER_REPOS.split(",") if x.strip()}
    try:
        consumers = parse_consumers(CONSUMERS_FILE, filters)
    except Exception as exc:  # noqa: BLE001
        print(f"::error::{exc}")
        return 1

    if not consumers:
        write_summary([("_none_", "failed", "No consumers matched filter")], 1, 0)
        print("::error::No consumers matched")
        return 1

    rows: list[tuple[str, str, str]] = []
    failed = 0
    processed = 0

    for consumer in consumers:
        processed += 1
        name = consumer["name"]
        repository = consumer["repository"]
        base_branch = consumer["base_branch"]
        print(f"::group::Pin consumer {name} ({repository})")
        consumer_dir = WORK_DIR / name
        if consumer_dir.exists():
            shutil.rmtree(consumer_dir)
        branch = f"automation/uecb-pin-{VERSION.replace('/', '-')}"

        try:
            proc = run(
                [
                    "gh",
                    "repo",
                    "clone",
                    repository,
                    str(consumer_dir),
                    "--",
                    "--depth",
                    "1",
                    "--branch",
                    base_branch,
                ]
            )
            if proc.returncode != 0:
                raise RuntimeError(f"clone failed: {proc.stderr or proc.stdout}")

            changed = apply_pins(consumer_dir, consumer["pins"])
            if not changed:
                rows.append((repository, "noop", f"already pinned to {VERSION}"))
                print("::endgroup::")
                continue

            if DRY_RUN:
                rows.append((repository, "dry-run", "changes prepared; push/PR skipped"))
                print("::endgroup::")
                continue

            run_checked(["git", "config", "user.name", "uecb-release-bot"], cwd=consumer_dir)
            run_checked(
                [
                    "git",
                    "config",
                    "user.email",
                    "41898282+github-actions[bot]@users.noreply.github.com",
                ],
                cwd=consumer_dir,
            )
            run_checked(["git", "checkout", "-B", branch], cwd=consumer_dir)
            run_checked(["git", "add", "-A"], cwd=consumer_dir)
            status = run_checked(["git", "status", "--porcelain"], cwd=consumer_dir)
            if not status.stdout.strip():
                rows.append((repository, "noop", f"already pinned to {VERSION}"))
                print("::endgroup::")
                continue

            commit_msg = f"chore(uecb): pin broker to {VERSION}"
            run_checked(["git", "commit", "-m", commit_msg], cwd=consumer_dir)
            remote = f"https://x-access-token:{GH_TOKEN}@github.com/{repository}.git"
            run_checked(["git", "remote", "set-url", "origin", remote], cwd=consumer_dir)
            push = run(
                ["git", "push", "--force-with-lease", "origin", f"HEAD:refs/heads/{branch}"],
                cwd=consumer_dir,
            )
            if push.returncode != 0:
                raise RuntimeError(f"push failed: {push.stderr or push.stdout}")

            pr_body = f"""## UECB GitOps pin

Release update from `{SOURCE_REF}` ({SOURCE_SHA[:7]}).

| Field | Value |
| --- | --- |
| Version | `{VERSION}` |
| Source ref | `{SOURCE_REF}` |
| Source SHA | `{SOURCE_SHA}` |
| Release run | {RELEASE_RUN_URL or '_(not provided)_'} |

### Changes
- Kustomize remote base `?ref=` and broker `newTag` (when present)
- Terraform `uecb_broker_release_version` default (when present)
"""
            existing = run(
                [
                    "gh",
                    "pr",
                    "list",
                    "--repo",
                    repository,
                    "--head",
                    branch,
                    "--base",
                    base_branch,
                    "--json",
                    "url",
                    "--jq",
                    ".[0].url",
                ]
            )
            pr_url = (existing.stdout or "").strip()
            if pr_url:
                run(
                    [
                        "gh",
                        "pr",
                        "edit",
                        pr_url,
                        "--repo",
                        repository,
                        "--title",
                        commit_msg,
                        "--body",
                        pr_body,
                    ]
                )
            else:
                created = run(
                    [
                        "gh",
                        "pr",
                        "create",
                        "--repo",
                        repository,
                        "--base",
                        base_branch,
                        "--head",
                        branch,
                        "--title",
                        commit_msg,
                        "--body",
                        pr_body,
                    ]
                )
                if created.returncode != 0:
                    raise RuntimeError(f"pr create failed: {created.stderr or created.stdout}")
                pr_url = (created.stdout or "").strip()

            rows.append((repository, "created", pr_url))
            print(f"Pin PR: {pr_url}")
        except Exception as exc:  # noqa: BLE001
            failed += 1
            detail = str(exc).replace("\n", "; ")
            rows.append((repository, "failed", detail))
            print(f"::error::Pin failed for {repository}: {exc}")
        print("::endgroup::")

    write_summary(rows, failed, processed)
    if failed:
        print(f"::error::GitOps pin PR step failed for {failed} consumer(s)")
        return 2
    print(f"GitOps pin PR step succeeded for {processed} consumer(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
PY
