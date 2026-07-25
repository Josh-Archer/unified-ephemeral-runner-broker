"""Unit tests for Azure Functions capacity classification (no Azure SDK required)."""

from __future__ import annotations

import importlib.util
import pathlib
import sys
import types
import unittest


def _load_function_app_helpers():
    """Load classify helpers from function_app.py without requiring azure packages."""
    root = pathlib.Path(__file__).resolve().parent
    path = root / "function_app.py"
    source = path.read_text(encoding="utf-8")

    # Stub azure modules so import succeeds in bare CI/dev environments.
    for name in (
        "azure",
        "azure.functions",
        "azure.core",
        "azure.core.exceptions",
        "azure.storage",
        "azure.storage.blob",
        "azure.storage.queue",
    ):
        if name not in sys.modules:
            mod = types.ModuleType(name)
            if name == "azure.functions":
                class _FunctionApp:
                    def __init__(self, *args, **kwargs):
                        pass

                    def route(self, *args, **kwargs):
                        def decorator(fn):
                            return fn

                        return decorator

                    def queue_trigger(self, *args, **kwargs):
                        def decorator(fn):
                            return fn

                        return decorator

                mod.FunctionApp = _FunctionApp
                mod.HttpRequest = object
                mod.HttpResponse = lambda *a, **k: None
                mod.QueueMessage = object
                mod.AuthLevel = types.SimpleNamespace(ANONYMOUS="anonymous")
            if name == "azure.core.exceptions":
                mod.ResourceExistsError = type("ResourceExistsError", (Exception,), {})
                mod.ResourceNotFoundError = type("ResourceNotFoundError", (Exception,), {})
            if name == "azure.storage.blob":
                mod.BlobServiceClient = object
            if name == "azure.storage.queue":
                mod.QueueClient = object
            sys.modules[name] = mod

    # Parent package stubs
    if "azure.core" not in sys.modules:
        sys.modules["azure.core"] = types.ModuleType("azure.core")
    sys.modules["azure.core"].exceptions = sys.modules["azure.core.exceptions"]

    spec = importlib.util.spec_from_file_location("uecb_azure_function_app", path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    # Avoid executing FunctionApp decorator side effects if any hard-fail —
    # the source only constructs FunctionApp at import which we stubbed.
    spec.loader.exec_module(module)
    return module


class CapacityClassifyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mod = _load_function_app_helpers()

    def test_terminal_skip_within_ttl(self) -> None:
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "completed", 60, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "skip",
        )

    def test_terminal_delete_after_ttl(self) -> None:
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "failed", 90000, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "delete_terminal",
        )

    def test_pending_and_active(self) -> None:
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "accepted", 10, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "pending",
        )
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "running", 10, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "active",
        )

    def test_stale_non_terminal_deleted(self) -> None:
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "running", 2000, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "delete_stale",
        )
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "accepted", 2000, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "delete_stale",
        )

    def test_legacy_empty_state_conservative_active(self) -> None:
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "", 10, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "active",
        )
        self.assertEqual(
            self.mod.classify_capacity_entry(
                "", 2000, stale_seconds=900, terminal_ttl_seconds=86400
            ),
            "delete_stale",
        )


if __name__ == "__main__":
    unittest.main()
