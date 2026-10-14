import json
import os
import shutil
import subprocess
import time
import traceback
from pathlib import Path
from urllib.request import Request, urlopen

import boto3
import jwt


dynamodb = boto3.resource("dynamodb")
secrets = boto3.client("secretsmanager")


def _table():
    return dynamodb.Table(os.environ["STATUS_TABLE"])


def _merge_status(execution_id, **changes):
    table = _table()
    existing = table.get_item(Key={"execution_id": execution_id}).get("Item", {"execution_id": execution_id})
    existing.update(changes)
    existing["ttl"] = int(time.time()) + 86400
    table.put_item(Item=existing)


def _secret_value(secret_id):
    payload = secrets.get_secret_value(SecretId=secret_id)
    if "SecretString" in payload:
        return payload["SecretString"]
    return payload["SecretBinary"].decode("utf-8")


def _github_installation_token():
    """Exchange Lambda's GitHub App credentials for a short-lived token."""
    try:
        app = json.loads(_secret_value(os.environ["RUNNER_GITHUB_APP_SECRET_ARN"]))
        app_id = str(app["app_id"]).strip()
        installation_id = str(app["installation_id"]).strip()
        private_key = app["private_key"]
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise RuntimeError("GitHub App secret must contain app_id, installation_id, and private_key") from exc
    if not app_id or not installation_id or not isinstance(private_key, str) or not private_key.strip():
        raise RuntimeError("GitHub App secret must contain app_id, installation_id, and private_key")

    now = int(time.time())
    app_jwt = jwt.encode(
        {"iat": now - 60, "exp": now + 540, "iss": app_id},
        private_key,
        algorithm="RS256",
    )
    request = Request(
        f"https://api.github.com/app/installations/{installation_id}/access_tokens",
        data=b"{}",
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {app_jwt}",
            "X-GitHub-Api-Version": "2022-11-28",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urlopen(request, timeout=10) as response:
        payload = json.loads(response.read().decode("utf-8"))
    token = str(payload.get("token") or "").strip()
    if not token:
        raise RuntimeError("GitHub App installation token response did not contain a token")
    return token


def _tail(path, limit=120):
    if not path.exists():
        return ""
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    return "\n".join(lines[-limit:])


def lambda_handler(event, _context):
    execution_id = str(event.get("execution_id") or "").strip()
    if not execution_id:
        raise ValueError("execution_id is required")

    runner_name = str(event.get("runner_name") or "").strip()
    runner_labels = str(event.get("runner_labels") or "").strip()
    github = event.get("github") or {}
    scope_type = str(github.get("scope_type") or "").strip()
    target_url = str(github.get("target_url") or "").strip()

    if not runner_name or not runner_labels or not scope_type or not target_url:
        raise ValueError("runner payload is missing required fields")

    details_url = str(event.get("details_url") or "").strip()
    work_root = Path(f"/tmp/uecb-lambda-{execution_id}")
    log_path = work_root / "runner.log"
    shutil.rmtree(work_root, ignore_errors=True)
    work_root.mkdir(parents=True, exist_ok=True)

    _merge_status(
        execution_id,
        provider="lambda",
        status="IN_PROGRESS",
        complete=False,
        details_url=details_url,
        metadata={
            "function_name": os.environ.get("AWS_LAMBDA_FUNCTION_NAME", ""),
            "region": os.environ.get("AWS_REGION", ""),
        },
        started_at=int(time.time()),
    )

    env = os.environ.copy()
    env.update(
        {
            "GITHUB_SCOPE_TYPE": scope_type,
            "GITHUB_ORGANIZATION": str(github.get("organization") or "").strip(),
            "GITHUB_OWNER": str(github.get("owner") or "").strip(),
            "GITHUB_REPOSITORY": str(github.get("repository") or "").strip(),
            "GITHUB_TARGET_URL": target_url,
            "RUNNER_GROUP": str(github.get("runner_group") or "").strip(),
            "RUNNER_NAME": runner_name,
            "RUNNER_LABELS": runner_labels,
            "RUNNER_ROOT": str(work_root / "runner"),
            "RUNNER_WORKDIR": "_work",
            "RUNNER_VERSION": os.environ.get("RUNNER_VERSION", "2.333.1"),
            "RUNNER_ALLOW_RUNASROOT": "1",
            "UECB_PROVIDER": "lambda",
        }
    )

    try:
        env["GITHUB_TOKEN"] = _github_installation_token()
        with log_path.open("w", encoding="utf-8") as log_file:
            completed = subprocess.run(
                ["bash", "/opt/uecb/runner-entrypoint.sh"],
                cwd=str(work_root),
                env=env,
                stdout=log_file,
                stderr=subprocess.STDOUT,
                text=True,
                check=False,
            )

        if completed.returncode != 0:
            message = _tail(log_path) or f"runner exited with status {completed.returncode}"
            _merge_status(
                execution_id,
                status="FAILED",
                complete=True,
                success=False,
                error={"message": message},
                finished_at=int(time.time()),
            )
            raise RuntimeError(message)

        _merge_status(
            execution_id,
            status="SUCCEEDED",
            complete=True,
            success=True,
            finished_at=int(time.time()),
        )
        return {"ok": True, "execution_id": execution_id}
    except Exception as exc:
        message = _tail(log_path) or "".join(traceback.format_exception_only(type(exc), exc)).strip()
        _merge_status(
            execution_id,
            status="FAILED",
            complete=True,
            success=False,
            error={"message": message},
            finished_at=int(time.time()),
        )
        raise
