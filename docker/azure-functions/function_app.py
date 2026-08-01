import json
import logging
import os
import shutil
import signal
import subprocess
import tempfile
import time
import traceback
import urllib.parse
import uuid
from datetime import datetime, timezone
from typing import Any

import azure.functions as func
from azure.core.exceptions import ResourceExistsError, ResourceNotFoundError
from azure.storage.blob import BlobServiceClient
from azure.storage.queue import QueueClient

app = func.FunctionApp(http_auth_level=func.AuthLevel.ANONYMOUS)

BACKEND_NAME = "azure-functions"
DEFAULT_DISPATCH_QUEUE = "uecb-azure-functions-dispatch"
DEFAULT_STATUS_CONTAINER = "uecb-azure-functions-status"
MAX_RUNNER_TIMEOUT_SECONDS = 8 * 60
STATUS_UPDATE_INTERVAL_SECONDS = 15
TERMINAL_STATES = frozenset({"completed", "failed", "canceled", "cancelled", "expired"})
PENDING_STATES = frozenset({"accepted", "queued", "pending"})
# Non-terminal statuses older than this are treated as free (and deleted).
# DEFAULT = max runner timeout + 5m grace (issue: expire non-terminal past timeout+grace).
DEFAULT_STATUS_STALE_SECONDS = MAX_RUNNER_TIMEOUT_SECONDS + 300
# Terminal status blobs older than this are deleted by capacity probes and the timer reaper.
DEFAULT_TERMINAL_TTL_SECONDS = 24 * 60 * 60
# Periodic reaper so cleanup does not depend only on capacity GET traffic.
# NCRONTAB: second minute hour day month day-of-week
DEFAULT_STATUS_REAPER_SCHEDULE = "0 */5 * * * *"


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def env(name: str, default: str = "") -> str:
    return str(os.environ.get(name) or default)


def _int_env(name: str, default: int) -> int:
    try:
        return int(env(name) or str(default))
    except ValueError:
        return default


def status_stale_seconds() -> int:
    return max(60, _int_env("UECB_STATUS_STALE_SECONDS", DEFAULT_STATUS_STALE_SECONDS))


def terminal_ttl_seconds() -> int:
    return max(60, _int_env("UECB_STATUS_TERMINAL_TTL_SECONDS", DEFAULT_TERMINAL_TTL_SECONDS))


def classify_capacity_entry(
    state: str,
    age_seconds: float | None,
    *,
    stale_seconds: int,
    terminal_ttl_seconds: int,
) -> str:
    """Return how capacity / reaper should treat one status blob.

    Values: active | pending | skip | delete_stale | delete_terminal
    """
    normalized = (state or "").strip().lower()
    age = age_seconds if age_seconds is not None and age_seconds >= 0 else 0.0

    if normalized in TERMINAL_STATES:
        if age >= terminal_ttl_seconds:
            return "delete_terminal"
        return "skip"

    if age >= stale_seconds:
        # Orphaned accepted/running must not pin free_slots at zero forever.
        return "delete_stale"

    if not normalized:
        # Legacy blob without metadata: conservative active until stale TTL.
        return "active"

    if normalized in PENDING_STATES:
        return "pending"
    return "active"


def should_reap_decision(decision: str) -> bool:
    """True when classify_capacity_entry says the blob should be deleted."""
    return decision in ("delete_stale", "delete_terminal")


def scan_status_blobs(
    container: Any,
    *,
    stale_seconds: int,
    terminal_ttl_seconds: int,
    now: datetime | None = None,
    delete: bool = True,
) -> dict[str, int]:
    """List status blobs via metadata, optionally delete expired ones.

    Compatible with metadata-based capacity (#82): never downloads blob bodies.
    Used by the capacity probe (opportunistic reap) and the timer reaper.

    Returns counters: scanned, active, pending, skipped, delete_stale,
    delete_terminal, reaped, errors.
    """
    counts = {
        "scanned": 0,
        "active": 0,
        "pending": 0,
        "skipped": 0,
        "delete_stale": 0,
        "delete_terminal": 0,
        "reaped": 0,
        "errors": 0,
    }
    now = now or datetime.now(timezone.utc)
    for blob in container.list_blobs(include=["metadata"]):
        name = str(getattr(blob, "name", None) or "")
        if not name.endswith(".json"):
            continue
        counts["scanned"] += 1
        decision = classify_capacity_entry(
            _blob_state(blob),
            _blob_age_seconds(blob, now=now),
            stale_seconds=stale_seconds,
            terminal_ttl_seconds=terminal_ttl_seconds,
        )
        if should_reap_decision(decision):
            counts[decision] = counts.get(decision, 0) + 1
            if delete:
                try:
                    container.delete_blob(name)
                    counts["reaped"] += 1
                except Exception:
                    counts["errors"] += 1
                    logging.exception("failed to reap status blob %s", name)
            continue
        if decision == "pending":
            counts["pending"] += 1
        elif decision == "active":
            counts["active"] += 1
        else:
            counts["skipped"] += 1
    return counts


def reap_status_blobs(
    container: Any | None = None,
    *,
    stale_seconds: int | None = None,
    terminal_ttl: int | None = None,
    now: datetime | None = None,
) -> dict[str, int]:
    """Delete terminal-past-TTL and non-terminal-past-stale status blobs.

    Timer path entrypoint; capacity probe calls scan_status_blobs directly so
    it can also report active/pending counts in the same pass.
    """
    client = container if container is not None else status_container_client()
    return scan_status_blobs(
        client,
        stale_seconds=status_stale_seconds() if stale_seconds is None else stale_seconds,
        terminal_ttl_seconds=terminal_ttl_seconds() if terminal_ttl is None else terminal_ttl,
        now=now,
        delete=True,
    )


def storage_connection_string() -> str:
    value = env("AzureWebJobsStorage")
    if not value:
        raise RuntimeError("AzureWebJobsStorage is required")
    return value


def dispatch_token() -> str:
    return env("UECB_DISPATCH_TOKEN")


def dispatch_queue_name() -> str:
    return DEFAULT_DISPATCH_QUEUE


def status_container_name() -> str:
    return env("UECB_STATUS_CONTAINER", DEFAULT_STATUS_CONTAINER)


def status_container_client():
    client = BlobServiceClient.from_connection_string(storage_connection_string()).get_container_client(
        status_container_name()
    )
    try:
        client.create_container()
    except ResourceExistsError:
        pass
    return client


def queue_client() -> QueueClient:
    client = QueueClient.from_connection_string(
        storage_connection_string(),
        dispatch_queue_name(),
    )
    try:
        client.create_queue()
    except ResourceExistsError:
        pass
    return client


def resource_details_url() -> str:
    subscription_id = env("UECB_AZURE_SUBSCRIPTION_ID")
    resource_group = env("UECB_AZURE_RESOURCE_GROUP")
    app_name = env("WEBSITE_SITE_NAME") or env("UECB_AZURE_FUNCTION_APP_NAME")
    if not subscription_id or not resource_group or not app_name:
        return ""
    resource_path = (
        f"/subscriptions/{subscription_id}/resourceGroups/{resource_group}"
        f"/providers/Microsoft.Web/sites/{app_name}"
    )
    return f"https://portal.azure.com/#resource{resource_path}/overview"


def request_action(req: func.HttpRequest, payload: dict[str, Any]) -> str:
    return str(req.params.get("action") or payload.get("action") or "").strip().lower()


def authorized(req: func.HttpRequest) -> bool:
    expected = dispatch_token().strip()
    if not expected:
        return True
    return req.headers.get("Authorization", "") == f"Bearer {expected}"


def json_response(payload: dict[str, Any], status_code: int = 200) -> func.HttpResponse:
    return func.HttpResponse(
        body=json.dumps(payload),
        status_code=status_code,
        mimetype="application/json",
    )


def read_status(execution_id: str) -> dict[str, Any] | None:
    blob_client = status_container_client().get_blob_client(f"{execution_id}.json")
    try:
        return json.loads(blob_client.download_blob().readall())
    except ResourceNotFoundError:
        return None


def write_status(execution_id: str, **changes: Any) -> dict[str, Any]:
    status = read_status(execution_id) or {"execution_id": execution_id}
    status.update(changes)
    status["execution_id"] = execution_id
    status["updated_at"] = utc_now()
    if "details_url" not in status:
        status["details_url"] = resource_details_url()

    state = str(status.get("state") or "").strip().lower() or "unknown"
    # Metadata keeps capacity O(list) — no body download per blob.
    metadata = {
        "state": state,
        "updated_epoch": str(int(time.time())),
    }

    blob_client = status_container_client().get_blob_client(f"{execution_id}.json")
    blob_client.upload_blob(json.dumps(status), overwrite=True, metadata=metadata)
    return status


def _blob_age_seconds(blob: Any, now: datetime | None = None) -> float | None:
    """Age of a status blob for capacity / TTL decisions."""
    now = now or datetime.now(timezone.utc)
    meta = getattr(blob, "metadata", None) or {}
    epoch_raw = str(meta.get("updated_epoch") or meta.get("Updated_epoch") or "").strip()
    if epoch_raw:
        try:
            return max(0.0, now.timestamp() - float(epoch_raw))
        except ValueError:
            pass
    last_modified = getattr(blob, "last_modified", None)
    if last_modified is None:
        return None
    if last_modified.tzinfo is None:
        last_modified = last_modified.replace(tzinfo=timezone.utc)
    return max(0.0, (now - last_modified).total_seconds())


def _blob_state(blob: Any) -> str:
    meta = getattr(blob, "metadata", None) or {}
    return str(meta.get("state") or meta.get("State") or "").strip().lower()


def tail_log(path: str, limit: int = 120) -> str:
    if not os.path.exists(path):
        return ""
    with open(path, encoding="utf-8", errors="replace") as handle:
        return "\n".join(handle.read().splitlines()[-limit:])


def status_url_for(req: func.HttpRequest, execution_id: str) -> str:
    base_url = req.url.split("?", 1)[0]
    return f"{base_url}?action=status&execution_id={urllib.parse.quote(execution_id, safe='')}"


def normalize_dispatch(payload: dict[str, Any]) -> dict[str, Any]:
    execution_id = f"azf-{uuid.uuid4().hex[:24]}"
    runner_label = str(payload.get("runner_label") or "").strip() or f"uecb-azure-functions-{execution_id}"
    runner_name = str(payload.get("runner_name") or "").strip() or runner_label
    github = payload.get("github") or {}
    normalized = dict(payload)
    normalized["backend"] = BACKEND_NAME
    normalized["execution_id"] = execution_id
    normalized["runner_label"] = runner_label
    normalized["runner_name"] = runner_name
    normalized["details_url"] = resource_details_url()
    normalized["github"] = {
        "scope_type": str(github.get("scope_type") or "").strip(),
        "organization": str(github.get("organization") or "").strip(),
        "owner": str(github.get("owner") or "").strip(),
        "repository": str(github.get("repository") or "").strip(),
        "target_url": str(github.get("target_url") or "").strip(),
        "runner_group": str(github.get("runner_group") or "").strip(),
    }
    normalized["runner_labels"] = [str(value).strip() for value in normalized.get("runner_labels") or [] if str(value).strip()]
    normalized["requested_labels"] = [
        str(value).strip() for value in normalized.get("requested_labels") or [] if str(value).strip()
    ]
    return normalized


def build_runner_environment(payload: dict[str, Any], work_root: str) -> dict[str, str]:
    github = payload.get("github") or {}
    github_pat = env("UECB_GITHUB_PAT")
    if not github_pat:
        raise RuntimeError("UECB_GITHUB_PAT is required")
    if not github.get("scope_type"):
        raise RuntimeError("dispatch payload is missing github.scope_type")
    if not github.get("target_url"):
        raise RuntimeError("dispatch payload is missing github.target_url")

    return {
        "GITHUB_PAT": github_pat,
        "GITHUB_SCOPE_TYPE": str(github.get("scope_type") or ""),
        "GITHUB_ORGANIZATION": str(github.get("organization") or ""),
        "GITHUB_OWNER": str(github.get("owner") or ""),
        "GITHUB_REPOSITORY": str(github.get("repository") or ""),
        "GITHUB_TARGET_URL": str(github.get("target_url") or ""),
        "RUNNER_GROUP": str(github.get("runner_group") or ""),
        "RUNNER_NAME": str(payload.get("runner_name") or payload.get("runner_label") or payload.get("execution_id") or ""),
        "RUNNER_LABELS": ",".join(payload.get("runner_labels") or []),
        "RUNNER_ROOT": os.path.join(work_root, "runner"),
        "RUNNER_VERSION": env("UECB_RUNNER_VERSION", "2.333.1"),
        "RUNNER_ALLOW_RUNASROOT": "1",
        "UECB_PROVIDER": BACKEND_NAME,
    }


def runner_timeout_seconds(payload: dict[str, Any]) -> int:
    try:
        timeout = int(payload.get("job_timeout_seconds") or 0)
    except (TypeError, ValueError):
        timeout = 0
    if timeout <= 0:
        timeout = 900
    return min(timeout, MAX_RUNNER_TIMEOUT_SECONDS)


def terminate_process(process: subprocess.Popen[Any]) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    except Exception:
        logging.exception("failed to send SIGTERM to runner process group")
    try:
        process.wait(timeout=30)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        return
    except Exception:
        logging.exception("failed to send SIGKILL to runner process group")


@app.route(route="dispatch", methods=["GET", "POST"], auth_level=func.AuthLevel.ANONYMOUS)
def dispatch(req: func.HttpRequest) -> func.HttpResponse:
    if not authorized(req):
        return json_response({"ok": False, "error": "unauthorized"}, status_code=401)

    try:
        payload = req.get_json()
    except ValueError:
        payload = {}
    if not isinstance(payload, dict):
        payload = {}

    action = request_action(req, payload)

    if action == "verify":
        queue_client().get_queue_properties()
        status_container_client().get_container_properties()
        return json_response({"ok": True, "backend": BACKEND_NAME})

    if action == "status":
        execution_id = str(payload.get("execution_id") or req.params.get("execution_id") or "").strip()
        if not execution_id:
            return json_response({"ok": False, "error": "missing execution_id"}, status_code=400)
        status = read_status(execution_id)
        if status is None:
            return json_response({"ok": False, "error": "execution not found", "execution_id": execution_id}, status_code=404)
        return json_response(status)

    if action == "capacity":
        # Align with home broker-config-patch azure-functions maxRunners (2).
        # Use blob metadata only (no body download) so capacity stays fast with
        # large status history. Opportunistically delete terminal/stale blobs
        # (same rules as the timer reaper).
        try:
            max_runners = int(env("MAX_RUNNERS") or env("UECB_MAX_RUNNERS") or "2")
        except ValueError:
            max_runners = 2
        try:
            counts = scan_status_blobs(
                status_container_client(),
                stale_seconds=status_stale_seconds(),
                terminal_ttl_seconds=terminal_ttl_seconds(),
                delete=True,
            )
        except Exception as exc:
            logging.exception("capacity probe failed")
            return json_response({"ok": False, "error": f"capacity probe failed: {exc}"}, status_code=500)

        free_slots = max(0, max_runners - counts["active"] - counts["pending"])
        return json_response(
            {
                "max_runners": max_runners,
                "active_runners": counts["active"],
                "pending_runners": counts["pending"],
                "warm_runners": 0,
                "free_slots": free_slots,
                "reaped_status_blobs": counts["reaped"],
            }
        )

    if action != "dispatch":
        return json_response({"ok": False, "error": "unsupported action"}, status_code=400)

    normalized = normalize_dispatch(payload)
    execution_id = normalized["execution_id"]
    queue_client().send_message(json.dumps(normalized))
    write_status(
        execution_id,
        ok=True,
        state="accepted",
        backend=BACKEND_NAME,
        runner_label=normalized["runner_label"],
        details_url=normalized["details_url"],
        accepted_at=utc_now(),
    )

    return json_response(
        {
            "ok": True,
            "execution_id": execution_id,
            "runner_label": normalized["runner_label"],
            "status_url": status_url_for(req, execution_id),
            "details_url": normalized["details_url"],
            "metadata": {
                "provider": BACKEND_NAME,
                "queue_name": dispatch_queue_name(),
            },
        },
        status_code=202,
    )


@app.timer_trigger(
    schedule=DEFAULT_STATUS_REAPER_SCHEDULE,
    arg_name="timer",
    run_on_startup=False,
    use_monitor=False,
)
def reap_status_timer(timer: func.TimerRequest) -> None:
    """Periodic reaper for status blobs (independent of capacity GET traffic).

    Rules (shared with capacity opportunistic reap via classify_capacity_entry):
    - delete terminal blobs past terminal TTL
    - delete non-terminal blobs past runner timeout + grace (stale seconds)
    """
    past_due = bool(getattr(timer, "past_due", False))
    try:
        counts = reap_status_blobs()
    except Exception:
        logging.exception("status blob timer reaper failed (past_due=%s)", past_due)
        raise
    logging.info(
        "status blob timer reaper finished schedule=%s past_due=%s scanned=%s reaped=%s "
        "delete_stale=%s delete_terminal=%s errors=%s",
        DEFAULT_STATUS_REAPER_SCHEDULE,
        past_due,
        counts.get("scanned", 0),
        counts.get("reaped", 0),
        counts.get("delete_stale", 0),
        counts.get("delete_terminal", 0),
        counts.get("errors", 0),
    )


@app.queue_trigger(arg_name="msg", queue_name=DEFAULT_DISPATCH_QUEUE, connection="AzureWebJobsStorage")
def run_runner(msg: func.QueueMessage) -> None:
    payload = json.loads(msg.get_body().decode("utf-8"))
    execution_id = str(payload.get("execution_id") or "").strip()
    if not execution_id:
        logging.error("queue payload missing execution_id")
        return

    work_root = tempfile.mkdtemp(prefix=f"uecb-azure-functions-{execution_id}-")
    log_path = os.path.join(work_root, "runner.log")
    write_status(
        execution_id,
        ok=True,
        state="running",
        backend=BACKEND_NAME,
        runner_label=payload.get("runner_label", ""),
        details_url=payload.get("details_url", ""),
        started_at=utc_now(),
    )

    try:
        runner_env = os.environ.copy()
        runner_env.update(build_runner_environment(payload, work_root))
        timeout_seconds = runner_timeout_seconds(payload)
        with open(log_path, "w", encoding="utf-8") as log_file:
            process = subprocess.Popen(
                ["/opt/uecb/runner-entrypoint.sh"],
                cwd=work_root,
                env=runner_env,
                stderr=subprocess.STDOUT,
                stdout=log_file,
                text=True,
                start_new_session=True,
            )
            deadline = time.monotonic() + timeout_seconds
            next_status_update = time.monotonic() + STATUS_UPDATE_INTERVAL_SECONDS
            while process.poll() is None:
                now = time.monotonic()
                if now >= deadline:
                    terminate_process(process)
                    break
                if now >= next_status_update:
                    log_file.flush()
                    write_status(execution_id, state="running", runner_log=tail_log(log_path))
                    next_status_update = now + STATUS_UPDATE_INTERVAL_SECONDS
                time.sleep(1)

        exit_code = process.returncode
        if exit_code == 0:
            write_status(
                execution_id,
                ok=True,
                state="completed",
                exit_code=exit_code,
                completed_at=utc_now(),
            )
            return

        timed_out = time.monotonic() >= deadline
        if timed_out:
            last_error = f"runner exceeded Azure Functions timeout budget of {timeout_seconds}s"
        else:
            last_error = f"runner exited with code {exit_code}"
        write_status(
            execution_id,
            ok=False,
            state="failed",
            exit_code=exit_code,
            completed_at=utc_now(),
            last_error=last_error,
            runner_log=tail_log(log_path),
        )
    except Exception as exc:
        logging.exception("azure functions runner execution failed")
        write_status(
            execution_id,
            ok=False,
            state="failed",
            completed_at=utc_now(),
            last_error=str(exc),
            runner_log=tail_log(log_path),
            traceback=traceback.format_exc(limit=20),
        )
    finally:
        shutil.rmtree(work_root, ignore_errors=True)
