import importlib.util
import io
import json
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError, URLError


HANDLER_PATH = Path(__file__).with_name("handler.py")
boto3_stub = types.ModuleType("boto3")
boto3_stub.resource = lambda *_args, **_kwargs: None
boto3_stub.client = lambda *_args, **_kwargs: None
jwt_stub = types.ModuleType("jwt")
jwt_stub.encode = lambda *_args, **_kwargs: "test-app-jwt"
with patch.dict(sys.modules, {"boto3": boto3_stub, "jwt": jwt_stub}):
    spec = importlib.util.spec_from_file_location("lambda_handler_under_test", HANDLER_PATH)
    handler = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(handler)


class Response:
    def __init__(self, payload):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self):
        return json.dumps(self.payload).encode("utf-8")


REPO_SCOPE = {"scope_type": "repository", "owner": "acme", "repository": "widgets"}
APP_SECRET = json.dumps({"app_id": "12345", "installation_id": "67890", "private_key": "REPLACE_ME"})


def _http_error(code):
    return HTTPError("https://api.github.com/x", code, "err", {}, io.BytesIO(b"{}"))


class GithubInstallationTokenTests(unittest.TestCase):
    def test_exchanges_github_app_credentials_for_installation_token(self):
        app_secret = json.dumps(
            {"app_id": "12345", "installation_id": "67890", "private_key": "REPLACE_ME"}
        )
        with (
            patch.dict(handler.os.environ, {"RUNNER_GITHUB_APP_SECRET_ARN": "REPLACE_ME_ARN"}),
            patch.object(handler, "_secret_value", return_value=app_secret),
            patch.object(handler.time, "time", return_value=1_700_000_000),
            patch.object(handler.jwt, "encode", return_value="signed-app-jwt") as encode,
            patch.object(handler, "urlopen", return_value=Response({"token": "short-lived-token"})) as urlopen,
        ):
            token = handler._github_installation_token(REPO_SCOPE)

        self.assertEqual(token, "short-lived-token")
        encode.assert_called_once_with(
            {"iat": 1_699_999_940, "exp": 1_700_000_540, "iss": "12345"},
            "REPLACE_ME",
            algorithm="RS256",
        )
        request = urlopen.call_args.args[0]
        self.assertEqual(
            request.full_url,
            "https://api.github.com/app/installations/67890/access_tokens",
        )
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(request.get_header("Authorization"), "Bearer signed-app-jwt")
        self.assertEqual(
            json.loads(request.data),
            {"repositories": ["widgets"], "permissions": {"administration": "write"}},
        )
        self.assertEqual(urlopen.call_args.kwargs, {"timeout": 10})

    def test_rejects_secret_without_required_github_app_fields(self):
        with (
            patch.dict(handler.os.environ, {"RUNNER_GITHUB_APP_SECRET_ARN": "REPLACE_ME_ARN"}),
            patch.object(handler, "_secret_value", return_value='{"app_id":"12345"}'),
        ):
            with self.assertRaisesRegex(RuntimeError, "app_id, installation_id, and private_key"):
                handler._github_installation_token(REPO_SCOPE)

    def _call(self, side_effect, github=REPO_SCOPE):
        with (
            patch.dict(handler.os.environ, {"RUNNER_GITHUB_APP_SECRET_ARN": "REPLACE_ME_ARN"}),
            patch.object(handler, "_secret_value", return_value=APP_SECRET),
            patch.object(handler.time, "sleep") as sleep,
            patch.object(handler, "urlopen", side_effect=side_effect) as urlopen,
        ):
            try:
                return handler._github_installation_token(github), urlopen, sleep
            except Exception as exc:
                exc.urlopen, exc.sleep = urlopen, sleep
                raise

    def test_org_scope_requests_only_self_hosted_runner_permission(self):
        _, urlopen, _ = self._call([Response({"token": "t"})], github={"scope_type": "organization", "organization": "acme"})
        self.assertEqual(
            json.loads(urlopen.call_args.args[0].data),
            {"permissions": {"organization_self_hosted_runners": "write"}},
        )

    def test_rejects_unknown_scope_without_calling_github(self):
        with self.assertRaisesRegex(RuntimeError, "unsupported github scope_type"):
            handler._github_installation_token({"scope_type": "enterprise"})
        with self.assertRaisesRegex(RuntimeError, "requires github.repository"):
            handler._github_installation_token({"scope_type": "repository"})

    def test_client_error_is_not_retried(self):
        with self.assertRaisesRegex(RuntimeError, "HTTP 401") as ctx:
            self._call([_http_error(401)])
        self.assertEqual(ctx.exception.urlopen.call_count, 1)
        ctx.exception.sleep.assert_not_called()

    def test_server_errors_retry_with_backoff_then_succeed(self):
        token, urlopen, sleep = self._call([_http_error(502), URLError("reset"), Response({"token": "ok"})])
        self.assertEqual(token, "ok")
        self.assertEqual(urlopen.call_count, 3)
        self.assertEqual([c.args[0] for c in sleep.call_args_list], [1.0, 2.0])

    def test_retries_are_bounded(self):
        with self.assertRaisesRegex(RuntimeError, "HTTP 503") as ctx:
            self._call([_http_error(503)] * 5)
        self.assertEqual(ctx.exception.urlopen.call_count, handler.TOKEN_REQUEST_ATTEMPTS)

    def test_response_missing_token_is_rejected(self):
        for payload in ({}, {"token": ""}, {"expires_at": "x"}, []):
            with self.subTest(payload=payload):
                with self.assertRaisesRegex(RuntimeError, "did not contain a token"):
                    self._call([Response(payload)])


if __name__ == "__main__":
    unittest.main()
