import importlib.util
import json
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch


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
            token = handler._github_installation_token()

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
        self.assertEqual(request.data, b"{}")
        self.assertEqual(urlopen.call_args.kwargs, {"timeout": 10})

    def test_rejects_secret_without_required_github_app_fields(self):
        with (
            patch.dict(handler.os.environ, {"RUNNER_GITHUB_APP_SECRET_ARN": "REPLACE_ME_ARN"}),
            patch.object(handler, "_secret_value", return_value='{"app_id":"12345"}'),
        ):
            with self.assertRaisesRegex(RuntimeError, "app_id, installation_id, and private_key"):
                handler._github_installation_token()


if __name__ == "__main__":
    unittest.main()
