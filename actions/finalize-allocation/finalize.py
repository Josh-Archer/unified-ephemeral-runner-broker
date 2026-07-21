#!/usr/bin/env python3
"""Report an allocation terminal state to the broker complete endpoint.

Mirrors OIDC auth used by actions/allocate-runner and POSTs to
POST /v1/allocations/{id}/complete with bounded retries for transient failures.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Mapping, Optional, Sequence, Tuple

# GitHub job.result values and broker aliases → canonical terminal states.
# Server-side parseCompletionState accepts the same aliases.
RESULT_TO_STATE = {
    "success": "completed",
    "succeeded": "completed",
    "complete": "completed",
    "completed": "completed",
    "failure": "failed",
    "failed": "failed",
    "error": "failed",
    "cancelled": "canceled",
    "canceled": "canceled",
    "cancel": "canceled",
    # Job never ran (for example a skipped dependent); still release capacity.
    "skipped": "canceled",
    "expired": "expired",
    "quarantined": "quarantined",
    "quarantine": "quarantined",
}

CANONICAL_TERMINAL_STATES = frozenset(
    {"completed", "failed", "canceled", "expired", "quarantined"}
)

# HTTP statuses treated as transient (retry with backoff).
TRANSIENT_STATUS_CODES = frozenset({408, 425, 429})


class PermanentFinalizeError(Exception):
    """Non-retryable finalize failure (auth, validation, not found, etc.)."""


class TransientFinalizeError(Exception):
    """Retryable finalize failure (network or 5xx/429/408)."""


def map_result_to_state(result: str = "", state: str = "") -> str:
    """Map a GitHub job result or explicit state input to a broker terminal state.

    Prefer ``state`` when non-empty. Empty inputs default to ``completed``,
    matching the broker completion API.
    """
    explicit = (state or "").strip().lower()
    if explicit:
        mapped = RESULT_TO_STATE.get(explicit, explicit)
        if mapped not in CANONICAL_TERMINAL_STATES:
            raise ValueError(
                f"unsupported terminal state {state!r}; "
                f"expected one of {sorted(CANONICAL_TERMINAL_STATES)}"
            )
        return mapped

    raw = (result or "").strip().lower()
    if not raw:
        return "completed"

    if raw in RESULT_TO_STATE:
        return RESULT_TO_STATE[raw]

    raise ValueError(
        f"unsupported job result {result!r}; "
        f"expected success, failure, cancelled, or a broker terminal state"
    )


def build_completion_payload(
    state: str,
    reason: str = "",
    error: str = "",
    failure_class: str = "",
) -> dict[str, str]:
    """Build the JSON body for POST /v1/allocations/{id}/complete."""
    payload: dict[str, str] = {"state": state}

    reason = (reason or "").strip()
    error = (error or "").strip()
    failure_class = (failure_class or "").strip()

    if reason:
        payload["reason"] = reason
    if error:
        payload["error"] = error
    elif state == "failed" and reason:
        # Broker uses the error field for failed messages; map reason when error is empty.
        payload["error"] = reason
    if failure_class:
        payload["failure_class"] = failure_class

    return payload


def is_transient_http_status(status_code: int) -> bool:
    """Return True when the status should be retried with backoff."""
    if status_code in TRANSIENT_STATUS_CODES:
        return True
    return 500 <= status_code <= 599


def backoff_seconds(
    attempt: int,
    initial_backoff_seconds: float,
    max_backoff_seconds: float,
) -> float:
    """Exponential backoff for zero-based attempt index (delay after that attempt fails)."""
    if attempt < 0:
        attempt = 0
    delay = float(initial_backoff_seconds) * (2**attempt)
    return min(float(max_backoff_seconds), delay)


def obtain_id_token(
    audience: str,
    request_url: str,
    request_token: str,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> str:
    """Fetch a GitHub Actions OIDC ID token for the given audience."""
    if not request_url or not request_token:
        raise PermanentFinalizeError(
            "GitHub OIDC environment variables are not available "
            "(ACTIONS_ID_TOKEN_REQUEST_URL / ACTIONS_ID_TOKEN_REQUEST_TOKEN). "
            "Grant id-token: write, or set allow_unauthenticated=true for local brokers."
        )

    parsed = urllib.parse.urlparse(request_url)
    query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
    query["audience"] = [audience]
    encoded_query = urllib.parse.urlencode(query, doseq=True)
    token_url = urllib.parse.urlunparse(parsed._replace(query=encoded_query))

    request = urllib.request.Request(
        token_url,
        headers={"Authorization": f"bearer {request_token}"},
        method="GET",
    )
    try:
        with urlopen(request, timeout=30) as response:
            body = response.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise PermanentFinalizeError(
            f"failed to obtain GitHub OIDC token: HTTP {exc.code}: {detail}"
        ) from exc
    except urllib.error.URLError as exc:
        raise PermanentFinalizeError(f"failed to obtain GitHub OIDC token: {exc}") from exc

    try:
        payload = json.loads(body)
        token = payload["value"]
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise PermanentFinalizeError(
            "failed to parse GitHub OIDC token response"
        ) from exc

    if not isinstance(token, str) or not token.strip():
        raise PermanentFinalizeError("GitHub OIDC token response was empty")
    return token.strip()


def post_completion(
    broker_url: str,
    allocation_id: str,
    payload: Mapping[str, str],
    bearer_token: Optional[str] = None,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> Tuple[int, str]:
    """POST the completion payload. Returns (status_code, response_body)."""
    base = broker_url.rstrip("/")
    url = f"{base}/v1/allocations/{urllib.parse.quote(allocation_id, safe='')}/complete"
    data = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if bearer_token:
        headers["Authorization"] = f"Bearer {bearer_token}"

    request = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urlopen(request, timeout=30) as response:
            body = response.read().decode("utf-8", errors="replace")
            return int(response.status), body
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        return int(exc.code), body
    except urllib.error.URLError as exc:
        raise TransientFinalizeError(f"network error calling broker complete: {exc}") from exc


def classify_complete_response(status_code: int, body: str) -> None:
    """Raise Permanent/Transient errors based on the complete HTTP response.

    2xx is success (including idempotent duplicate same-state callbacks).
    """
    if 200 <= status_code < 300:
        return

    detail = body.strip() or f"HTTP {status_code}"
    if is_transient_http_status(status_code):
        raise TransientFinalizeError(
            f"transient broker complete failure: HTTP {status_code}: {detail}"
        )

    if status_code == 401:
        raise PermanentFinalizeError(
            f"authentication failed (HTTP 401). Check OIDC audience and id-token permissions: {detail}"
        )
    if status_code == 403:
        raise PermanentFinalizeError(
            f"authorization denied (HTTP 403). Caller may not own this allocation: {detail}"
        )
    if status_code == 404:
        raise PermanentFinalizeError(
            f"allocation not found (HTTP 404): {detail}"
        )
    if status_code == 400:
        raise PermanentFinalizeError(
            f"invalid completion request or conflicting terminal state (HTTP 400): {detail}"
        )

    raise PermanentFinalizeError(
        f"broker complete failed permanently (HTTP {status_code}): {detail}"
    )


def finalize_with_retries(
    broker_url: str,
    allocation_id: str,
    payload: Mapping[str, str],
    bearer_token: Optional[str] = None,
    max_retries: int = 5,
    initial_backoff_seconds: float = 1.0,
    max_backoff_seconds: float = 30.0,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
    sleeper: Callable[[float], None] = time.sleep,
    logger: Callable[[str], None] = lambda msg: print(msg, file=sys.stderr),
) -> Tuple[int, str]:
    """POST complete with bounded exponential backoff on transient failures."""
    if max_retries < 0:
        raise ValueError("max_retries must be >= 0")

    attempts = max_retries + 1
    last_error: Optional[Exception] = None

    for attempt in range(attempts):
        try:
            status_code, body = post_completion(
                broker_url=broker_url,
                allocation_id=allocation_id,
                payload=payload,
                bearer_token=bearer_token,
                urlopen=urlopen,
            )
            classify_complete_response(status_code, body)
            return status_code, body
        except PermanentFinalizeError:
            raise
        except TransientFinalizeError as exc:
            last_error = exc
            if attempt >= attempts - 1:
                break
            delay = backoff_seconds(attempt, initial_backoff_seconds, max_backoff_seconds)
            logger(
                f"transient complete failure (attempt {attempt + 1}/{attempts}): {exc}; "
                f"retrying in {delay:.1f}s"
            )
            sleeper(delay)

    assert last_error is not None
    raise PermanentFinalizeError(
        f"exhausted {attempts} complete attempts after transient failures: {last_error}"
    ) from last_error


def resolve_auth_token(
    allow_unauthenticated: bool,
    oidc_audience: str,
    environ: Mapping[str, str],
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> Optional[str]:
    """Return a bearer token, or None when unauthenticated mode is enabled."""
    if allow_unauthenticated:
        return None
    return obtain_id_token(
        audience=oidc_audience,
        request_url=environ.get("ACTIONS_ID_TOKEN_REQUEST_URL", ""),
        request_token=environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN", ""),
        urlopen=urlopen,
    )


def run_finalize(
    environ: Optional[Mapping[str, str]] = None,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
    sleeper: Callable[[float], None] = time.sleep,
    logger: Callable[[str], None] = lambda msg: print(msg, file=sys.stderr),
) -> int:
    """Entry point used by the composite action and unit tests. Returns exit code."""
    env: Mapping[str, str] = environ if environ is not None else os.environ

    broker_url = env.get("INPUT_BROKER_URL", "").strip()
    allocation_id = env.get("INPUT_ALLOCATION_ID", "").strip()
    if not broker_url:
        raise PermanentFinalizeError("broker_url is required")
    if not allocation_id:
        raise PermanentFinalizeError("allocation_id is required")

    state = map_result_to_state(
        result=env.get("INPUT_RESULT", ""),
        state=env.get("INPUT_STATE", ""),
    )
    payload = build_completion_payload(
        state=state,
        reason=env.get("INPUT_REASON", ""),
        error=env.get("INPUT_ERROR", ""),
        failure_class=env.get("INPUT_FAILURE_CLASS", ""),
    )

    allow_unauthenticated = env.get("INPUT_ALLOW_UNAUTHENTICATED", "false").strip().lower() in {
        "1",
        "true",
        "yes",
        "on",
    }
    oidc_audience = env.get("INPUT_OIDC_AUDIENCE", "uecb-broker").strip() or "uecb-broker"
    max_retries = int(env.get("INPUT_MAX_RETRIES", "5") or "5")
    initial_backoff = float(env.get("INPUT_INITIAL_BACKOFF_SECONDS", "1") or "1")
    max_backoff = float(env.get("INPUT_MAX_BACKOFF_SECONDS", "30") or "30")

    if max_retries < 0:
        raise PermanentFinalizeError("max_retries must be >= 0")
    if initial_backoff < 0 or max_backoff < 0:
        raise PermanentFinalizeError("backoff seconds must be >= 0")

    token = resolve_auth_token(
        allow_unauthenticated=allow_unauthenticated,
        oidc_audience=oidc_audience,
        environ=env,
        urlopen=urlopen,
    )

    logger(
        f"finalizing allocation {allocation_id} as state={state!r} "
        f"(auth={'none' if token is None else 'oidc'})"
    )
    status_code, body = finalize_with_retries(
        broker_url=broker_url,
        allocation_id=allocation_id,
        payload=payload,
        bearer_token=token,
        max_retries=max_retries,
        initial_backoff_seconds=initial_backoff,
        max_backoff_seconds=max_backoff,
        urlopen=urlopen,
        sleeper=sleeper,
        logger=logger,
    )

    # Best-effort surface of response for workflow logs / action outputs.
    try:
        parsed = json.loads(body) if body else {}
        response_state = parsed.get("state", state)
        print(f"allocation_id={parsed.get('allocation_id', allocation_id)}")
        print(f"state={response_state}")
        output_path = env.get("GITHUB_OUTPUT") or os.environ.get("GITHUB_OUTPUT")
        if output_path:
            with open(output_path, "a", encoding="utf-8") as handle:
                handle.write(f"allocation_id={parsed.get('allocation_id', allocation_id)}\n")
                handle.write(f"state={response_state}\n")
                handle.write(f"http_status={status_code}\n")
    except json.JSONDecodeError:
        print(f"allocation_id={allocation_id}")
        print(f"state={state}")
        print(body, file=sys.stderr)

    logger(f"allocation {allocation_id} finalized with HTTP {status_code}")
    return 0


def main(argv: Optional[Sequence[str]] = None) -> int:
    del argv  # composite action uses environment inputs only
    try:
        return run_finalize()
    except PermanentFinalizeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
