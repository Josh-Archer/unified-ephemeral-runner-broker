#!/usr/bin/env python3
"""Unit tests for finalize-allocation action helpers and HTTP behavior."""

from __future__ import annotations

import io
import json
import threading
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, Tuple

import finalize


class MapResultToStateTests(unittest.TestCase):
    def test_github_job_results(self) -> None:
        self.assertEqual(finalize.map_result_to_state(result="success"), "completed")
        self.assertEqual(finalize.map_result_to_state(result="failure"), "failed")
        self.assertEqual(finalize.map_result_to_state(result="cancelled"), "canceled")
        self.assertEqual(finalize.map_result_to_state(result="canceled"), "canceled")
        self.assertEqual(finalize.map_result_to_state(result="skipped"), "canceled")

    def test_broker_aliases(self) -> None:
        self.assertEqual(finalize.map_result_to_state(result="completed"), "completed")
        self.assertEqual(finalize.map_result_to_state(result="failed"), "failed")
        self.assertEqual(finalize.map_result_to_state(result="error"), "failed")
        self.assertEqual(finalize.map_result_to_state(result="cancel"), "canceled")

    def test_empty_defaults_to_completed(self) -> None:
        self.assertEqual(finalize.map_result_to_state(), "completed")
        self.assertEqual(finalize.map_result_to_state(result=""), "completed")
        self.assertEqual(finalize.map_result_to_state(state=""), "completed")

    def test_explicit_state_overrides_result(self) -> None:
        self.assertEqual(
            finalize.map_result_to_state(result="success", state="failed"),
            "failed",
        )
        self.assertEqual(
            finalize.map_result_to_state(result="failure", state="canceled"),
            "canceled",
        )

    def test_case_and_whitespace_insensitive(self) -> None:
        self.assertEqual(finalize.map_result_to_state(result=" Success "), "completed")
        self.assertEqual(finalize.map_result_to_state(state=" FAILED "), "failed")

    def test_unknown_result_raises(self) -> None:
        with self.assertRaises(ValueError):
            finalize.map_result_to_state(result="unknown-result")
        with self.assertRaises(ValueError):
            finalize.map_result_to_state(state="running")


class BuildPayloadTests(unittest.TestCase):
    def test_completed_payload(self) -> None:
        self.assertEqual(
            finalize.build_completion_payload("completed", reason="job done"),
            {"state": "completed", "reason": "job done"},
        )

    def test_failed_maps_reason_to_error_when_error_empty(self) -> None:
        self.assertEqual(
            finalize.build_completion_payload("failed", reason="boom"),
            {"state": "failed", "reason": "boom", "error": "boom"},
        )

    def test_failed_prefers_explicit_error(self) -> None:
        self.assertEqual(
            finalize.build_completion_payload(
                "failed", reason="ignored", error="runner crashed", failure_class="wait-timeout"
            ),
            {
                "state": "failed",
                "reason": "ignored",
                "error": "runner crashed",
                "failure_class": "wait-timeout",
            },
        )


class BackoffAndTransientTests(unittest.TestCase):
    def test_transient_status_codes(self) -> None:
        for code in (408, 425, 429, 500, 502, 503, 599):
            self.assertTrue(finalize.is_transient_http_status(code), code)
        for code in (200, 201, 400, 401, 403, 404, 409, 422):
            self.assertFalse(finalize.is_transient_http_status(code), code)

    def test_backoff_caps(self) -> None:
        self.assertEqual(finalize.backoff_seconds(0, 1, 30), 1)
        self.assertEqual(finalize.backoff_seconds(1, 1, 30), 2)
        self.assertEqual(finalize.backoff_seconds(2, 1, 30), 4)
        self.assertEqual(finalize.backoff_seconds(10, 1, 30), 30)


class ClassifyResponseTests(unittest.TestCase):
    def test_success_statuses(self) -> None:
        for code in (200, 201, 204):
            finalize.classify_complete_response(code, "{}")

    def test_permanent_statuses(self) -> None:
        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.classify_complete_response(400, "bad")
        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.classify_complete_response(401, "auth")
        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.classify_complete_response(403, "forbidden")
        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.classify_complete_response(404, "missing")

    def test_transient_statuses(self) -> None:
        with self.assertRaises(finalize.TransientFinalizeError):
            finalize.classify_complete_response(503, "unavailable")
        with self.assertRaises(finalize.TransientFinalizeError):
            finalize.classify_complete_response(429, "slow down")


class FakeHTTPResponse:
    def __init__(self, status: int, body: bytes) -> None:
        self.status = status
        self._body = body

    def read(self) -> bytes:
        return self._body

    def __enter__(self) -> "FakeHTTPResponse":
        return self

    def __exit__(self, *args: Any) -> None:
        return None


class RetryTests(unittest.TestCase):
    def test_retries_transient_then_succeeds(self) -> None:
        calls: List[int] = []
        responses = [
            (503, b"try later"),
            (503, b"still down"),
            (200, json.dumps({"allocation_id": "a1", "state": "completed"}).encode()),
        ]

        def urlopen(request: urllib.request.Request, timeout: float = 30) -> FakeHTTPResponse:
            del timeout
            idx = len(calls)
            calls.append(idx)
            status, body = responses[idx]
            if status >= 400:
                raise urllib.error.HTTPError(
                    url=request.full_url,
                    code=status,
                    msg="error",
                    hdrs=None,  # type: ignore[arg-type]
                    fp=io.BytesIO(body),
                )
            return FakeHTTPResponse(status, body)

        sleeps: List[float] = []
        status, body = finalize.finalize_with_retries(
            broker_url="http://broker.example",
            allocation_id="a1",
            payload={"state": "completed"},
            bearer_token="token",
            max_retries=5,
            initial_backoff_seconds=1,
            max_backoff_seconds=30,
            urlopen=urlopen,
            sleeper=sleeps.append,
            logger=lambda _msg: None,
        )
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["state"], "completed")
        self.assertEqual(len(calls), 3)
        self.assertEqual(sleeps, [1.0, 2.0])

    def test_permanent_failure_does_not_retry(self) -> None:
        calls = 0

        def urlopen(request: urllib.request.Request, timeout: float = 30) -> FakeHTTPResponse:
            del request, timeout
            nonlocal calls
            calls += 1
            raise urllib.error.HTTPError(
                url="http://broker.example/v1/allocations/a1/complete",
                code=400,
                msg="bad",
                hdrs=None,  # type: ignore[arg-type]
                fp=io.BytesIO(b'{"error":"already terminal"}'),
            )

        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.finalize_with_retries(
                broker_url="http://broker.example",
                allocation_id="a1",
                payload={"state": "completed"},
                max_retries=5,
                urlopen=urlopen,
                sleeper=lambda _s: None,
                logger=lambda _msg: None,
            )
        self.assertEqual(calls, 1)

    def test_exhausted_retries_raises_permanent(self) -> None:
        def urlopen(request: urllib.request.Request, timeout: float = 30) -> FakeHTTPResponse:
            del request, timeout
            raise urllib.error.HTTPError(
                url="http://broker.example/v1/allocations/a1/complete",
                code=503,
                msg="unavailable",
                hdrs=None,  # type: ignore[arg-type]
                fp=io.BytesIO(b"down"),
            )

        with self.assertRaises(finalize.PermanentFinalizeError) as ctx:
            finalize.finalize_with_retries(
                broker_url="http://broker.example",
                allocation_id="a1",
                payload={"state": "completed"},
                max_retries=2,
                initial_backoff_seconds=0.01,
                max_backoff_seconds=0.01,
                urlopen=urlopen,
                sleeper=lambda _s: None,
                logger=lambda _msg: None,
            )
        self.assertIn("exhausted", str(ctx.exception))


class AuthTests(unittest.TestCase):
    def test_missing_oidc_env_is_permanent(self) -> None:
        with self.assertRaises(finalize.PermanentFinalizeError) as ctx:
            finalize.resolve_auth_token(
                allow_unauthenticated=False,
                oidc_audience="uecb-broker",
                environ={},
            )
        self.assertIn("OIDC", str(ctx.exception))

    def test_allow_unauthenticated_skips_token(self) -> None:
        token = finalize.resolve_auth_token(
            allow_unauthenticated=True,
            oidc_audience="uecb-broker",
            environ={},
        )
        self.assertIsNone(token)

    def test_obtain_id_token_reads_value(self) -> None:
        def urlopen(request: urllib.request.Request, timeout: float = 30) -> FakeHTTPResponse:
            del timeout
            self.assertIn("audience=uecb-broker", request.full_url)
            self.assertEqual(request.get_header("Authorization"), "bearer req-token")
            return FakeHTTPResponse(200, json.dumps({"value": "jwt-token"}).encode())

        token = finalize.obtain_id_token(
            audience="uecb-broker",
            request_url="https://token.actions.githubusercontent.com?foo=1",
            request_token="req-token",
            urlopen=urlopen,
        )
        self.assertEqual(token, "jwt-token")


class MockBrokerHandler(BaseHTTPRequestHandler):
    """In-process mock broker for duplicate callback coverage."""

    complete_calls: List[Dict[str, Any]] = []
    complete_responses: List[Tuple[int, dict]] = []

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A003
        return

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        body = json.loads(raw.decode("utf-8") or "{}")
        auth = self.headers.get("Authorization")
        MockBrokerHandler.complete_calls.append(
            {"path": self.path, "body": body, "auth": auth}
        )
        if MockBrokerHandler.complete_responses:
            status, payload = MockBrokerHandler.complete_responses.pop(0)
        else:
            status, payload = 200, {
                "allocation_id": "alloc-dup",
                "state": body.get("state", "completed"),
            }
        encoded = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)


class EndToEndMockBrokerTests(unittest.TestCase):
    def setUp(self) -> None:
        MockBrokerHandler.complete_calls = []
        MockBrokerHandler.complete_responses = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), MockBrokerHandler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    def test_duplicate_callbacks_are_safe(self) -> None:
        """Two successful completes for the same terminal state both exit 0."""
        broker_url = f"http://127.0.0.1:{self.port}"
        for _ in range(2):
            status, body = finalize.finalize_with_retries(
                broker_url=broker_url,
                allocation_id="alloc-dup",
                payload={"state": "completed"},
                bearer_token="test-token",
                max_retries=0,
                urlopen=urllib.request.urlopen,
                sleeper=lambda _s: None,
                logger=lambda _msg: None,
            )
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["state"], "completed")

        self.assertEqual(len(MockBrokerHandler.complete_calls), 2)
        for call in MockBrokerHandler.complete_calls:
            self.assertEqual(call["path"], "/v1/allocations/alloc-dup/complete")
            self.assertEqual(call["body"]["state"], "completed")
            self.assertEqual(call["auth"], "Bearer test-token")

    def test_run_finalize_maps_failure_and_posts(self) -> None:
        broker_url = f"http://127.0.0.1:{self.port}"
        env = {
            "INPUT_BROKER_URL": broker_url,
            "INPUT_ALLOCATION_ID": "alloc-fail",
            "INPUT_RESULT": "failure",
            "INPUT_ERROR": "job failed",
            "INPUT_ALLOW_UNAUTHENTICATED": "true",
            "INPUT_MAX_RETRIES": "0",
            "INPUT_INITIAL_BACKOFF_SECONDS": "0",
            "INPUT_MAX_BACKOFF_SECONDS": "0",
        }
        code = finalize.run_finalize(
            environ=env,
            urlopen=urllib.request.urlopen,
            sleeper=lambda _s: None,
            logger=lambda _msg: None,
        )
        self.assertEqual(code, 0)
        self.assertEqual(len(MockBrokerHandler.complete_calls), 1)
        call = MockBrokerHandler.complete_calls[0]
        self.assertEqual(call["body"]["state"], "failed")
        self.assertEqual(call["body"]["error"], "job failed")
        self.assertIsNone(call["auth"])

    def test_run_finalize_requires_auth_by_default(self) -> None:
        broker_url = f"http://127.0.0.1:{self.port}"
        env = {
            "INPUT_BROKER_URL": broker_url,
            "INPUT_ALLOCATION_ID": "alloc-1",
            "INPUT_RESULT": "success",
            "INPUT_ALLOW_UNAUTHENTICATED": "false",
        }
        with self.assertRaises(finalize.PermanentFinalizeError):
            finalize.run_finalize(
                environ=env,
                urlopen=urllib.request.urlopen,
                sleeper=lambda _s: None,
                logger=lambda _msg: None,
            )
        self.assertEqual(MockBrokerHandler.complete_calls, [])


if __name__ == "__main__":
    unittest.main()
