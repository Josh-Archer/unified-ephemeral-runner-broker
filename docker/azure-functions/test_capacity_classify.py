"""Unit tests for Azure Functions capacity classification and status reaper."""

from __future__ import annotations

import importlib.util
import pathlib
import sys
import types
import unittest
from datetime import datetime, timezone
from typing import Any


def _load_function_app_helpers():
    """Load classify helpers from function_app.py without requiring azure packages."""
    root = pathlib.Path(__file__).resolve().parent
    path = root / "function_app.py"

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

                    def timer_trigger(self, *args, **kwargs):
                        def decorator(fn):
                            return fn

                        return decorator

                mod.FunctionApp = _FunctionApp
                mod.HttpRequest = object
                mod.HttpResponse = lambda *a, **k: None
                mod.QueueMessage = object
                mod.TimerRequest = object
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


class _FakeBlob:
    def __init__(
        self,
        name: str,
        *,
        state: str = "",
        updated_epoch: str | None = None,
        last_modified: datetime | None = None,
    ) -> None:
        self.name = name
        meta: dict[str, str] = {}
        if state:
            meta["state"] = state
        if updated_epoch is not None:
            meta["updated_epoch"] = updated_epoch
        self.metadata = meta
        self.last_modified = last_modified


class _FakeContainer:
    def __init__(self, blobs: list[_FakeBlob]) -> None:
        self.blobs = list(blobs)
        self.deleted: list[str] = []

    def list_blobs(self, include: Any = None) -> list[_FakeBlob]:
        return list(self.blobs)

    def delete_blob(self, name: str) -> None:
        self.deleted.append(name)
        self.blobs = [b for b in self.blobs if b.name != name]


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

    def test_should_reap_decision(self) -> None:
        self.assertTrue(self.mod.should_reap_decision("delete_stale"))
        self.assertTrue(self.mod.should_reap_decision("delete_terminal"))
        self.assertFalse(self.mod.should_reap_decision("active"))
        self.assertFalse(self.mod.should_reap_decision("pending"))
        self.assertFalse(self.mod.should_reap_decision("skip"))


class StatusReaperTests(unittest.TestCase):
    """Reaper rules: terminal past TTL; non-terminal past timeout+grace."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.mod = _load_function_app_helpers()

    def _now_epoch(self) -> float:
        return datetime(2026, 1, 15, 12, 0, 0, tzinfo=timezone.utc).timestamp()

    def _blob(
        self,
        name: str,
        *,
        state: str,
        age_seconds: float,
    ) -> _FakeBlob:
        epoch = str(int(self._now_epoch() - age_seconds))
        return _FakeBlob(name, state=state, updated_epoch=epoch)

    def test_scan_reaps_terminal_past_ttl_and_stale_non_terminal(self) -> None:
        now = datetime.fromtimestamp(self._now_epoch(), tz=timezone.utc)
        container = _FakeContainer(
            [
                self._blob("keep-running.json", state="running", age_seconds=30),
                self._blob("keep-accepted.json", state="accepted", age_seconds=60),
                self._blob("keep-completed.json", state="completed", age_seconds=100),
                self._blob("reap-stale-running.json", state="running", age_seconds=2000),
                self._blob("reap-stale-accepted.json", state="accepted", age_seconds=2000),
                self._blob("reap-terminal.json", state="failed", age_seconds=90000),
                _FakeBlob("ignore.txt", state="running", updated_epoch=str(int(self._now_epoch()))),
            ]
        )
        counts = self.mod.scan_status_blobs(
            container,
            stale_seconds=900,
            terminal_ttl_seconds=86400,
            now=now,
            delete=True,
        )
        self.assertEqual(counts["scanned"], 6)
        self.assertEqual(counts["active"], 1)
        self.assertEqual(counts["pending"], 1)
        self.assertEqual(counts["skipped"], 1)
        self.assertEqual(counts["delete_stale"], 2)
        self.assertEqual(counts["delete_terminal"], 1)
        self.assertEqual(counts["reaped"], 3)
        self.assertEqual(counts["errors"], 0)
        self.assertEqual(
            sorted(container.deleted),
            [
                "reap-stale-accepted.json",
                "reap-stale-running.json",
                "reap-terminal.json",
            ],
        )
        remaining = sorted(b.name for b in container.blobs)
        self.assertEqual(
            remaining,
            ["ignore.txt", "keep-accepted.json", "keep-completed.json", "keep-running.json"],
        )

    def test_scan_delete_false_counts_without_mutating(self) -> None:
        now = datetime.fromtimestamp(self._now_epoch(), tz=timezone.utc)
        container = _FakeContainer(
            [
                self._blob("old-failed.json", state="failed", age_seconds=90000),
                self._blob("old-running.json", state="running", age_seconds=2000),
            ]
        )
        counts = self.mod.scan_status_blobs(
            container,
            stale_seconds=900,
            terminal_ttl_seconds=86400,
            now=now,
            delete=False,
        )
        self.assertEqual(counts["delete_terminal"], 1)
        self.assertEqual(counts["delete_stale"], 1)
        self.assertEqual(counts["reaped"], 0)
        self.assertEqual(container.deleted, [])
        self.assertEqual(len(container.blobs), 2)

    def test_reap_status_blobs_entrypoint(self) -> None:
        now = datetime.fromtimestamp(self._now_epoch(), tz=timezone.utc)
        container = _FakeContainer(
            [
                self._blob("live.json", state="running", age_seconds=10),
                self._blob("dead.json", state="completed", age_seconds=100000),
            ]
        )
        counts = self.mod.reap_status_blobs(
            container,
            stale_seconds=900,
            terminal_ttl=86400,
            now=now,
        )
        self.assertEqual(counts["reaped"], 1)
        self.assertEqual(counts["delete_terminal"], 1)
        self.assertEqual(counts["active"], 1)
        self.assertEqual(container.deleted, ["dead.json"])

    def test_reaper_uses_metadata_only_no_body_download(self) -> None:
        """Ensure reaper path never needs blob body download (compat with #82)."""
        now = datetime.fromtimestamp(self._now_epoch(), tz=timezone.utc)

        class _NoDownloadBlob(_FakeBlob):
            def download_blob(self) -> None:  # pragma: no cover - must not be called
                raise AssertionError("status reaper must not download blob bodies")

        class _NoDownloadContainer(_FakeContainer):
            def get_blob_client(self, name: str) -> None:  # pragma: no cover
                raise AssertionError("status reaper must not open blob clients")

        container = _NoDownloadContainer(
            [
                _NoDownloadBlob(
                    "meta-only.json",
                    state="completed",
                    updated_epoch=str(int(self._now_epoch() - 100000)),
                )
            ]
        )
        counts = self.mod.scan_status_blobs(
            container,
            stale_seconds=900,
            terminal_ttl_seconds=86400,
            now=now,
            delete=True,
        )
        self.assertEqual(counts["reaped"], 1)

    def test_timer_handler_logs_and_reaps(self) -> None:
        now = datetime.fromtimestamp(self._now_epoch(), tz=timezone.utc)
        container = _FakeContainer(
            [
                self._blob("stale.json", state="running", age_seconds=5000),
            ]
        )
        original = self.mod.reap_status_blobs
        try:
            def _fake_reap(*_a: Any, **_k: Any) -> dict[str, int]:
                return original(container, stale_seconds=900, terminal_ttl=86400, now=now)

            self.mod.reap_status_blobs = _fake_reap  # type: ignore[method-assign]
            timer = types.SimpleNamespace(past_due=False)
            self.mod.reap_status_timer(timer)
            self.assertEqual(container.deleted, ["stale.json"])
        finally:
            self.mod.reap_status_blobs = original  # type: ignore[method-assign]

    def test_default_stale_is_runner_timeout_plus_grace(self) -> None:
        self.assertEqual(
            self.mod.DEFAULT_STATUS_STALE_SECONDS,
            self.mod.MAX_RUNNER_TIMEOUT_SECONDS + 300,
        )
        self.assertEqual(self.mod.DEFAULT_TERMINAL_TTL_SECONDS, 24 * 60 * 60)
        self.assertTrue(self.mod.DEFAULT_STATUS_REAPER_SCHEDULE)


if __name__ == "__main__":
    unittest.main()
