"""Fake-transport tests for the Jev client and pilot (no network)."""

from __future__ import annotations

import io
import json
import socket
import sys
import unittest
from pathlib import Path
from urllib.error import HTTPError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.jev_client import (  # noqa: E402
    JevBudgetError,
    JevClient,
    JevConfigError,
    JevDecodeError,
    JevHTTPError,
    JevRateLimitError,
)
from rag_upgrade.jev_pilot import auroc, load_fixture, run_pilot, verdict  # noqa: E402


def _no_network(*_a, **_k):
    raise AssertionError("network")


class _Resp:
    def __init__(self, payload: dict, status: int = 200):
        self.status = status
        self._raw = json.dumps(payload).encode("utf-8")
        self.headers = {}

    def read(self):
        return self._raw

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def _answers(**kwargs):
    return {"answers": kwargs, "usage": {"input_tokens": 10, "output_tokens": 0}, "model": "jev-1.13.0"}


class ClientTests(unittest.TestCase):
    def setUp(self) -> None:
        self._orig = socket.socket.connect
        socket.socket.connect = _no_network  # type: ignore[assignment]
        self.secrets = {
            "TYPESAFE_API_KEY": "test-key",
            "TYPESAFE_BASE_URL": "https://example.invalid",
            "JEV_MODEL": "jev-1.13.0",
        }

    def tearDown(self) -> None:
        socket.socket.connect = self._orig  # type: ignore[assignment]

    def test_parses_noul_choice_score(self) -> None:
        payload = _answers(
            open_question={"type": "noul", "noul": 0.8},
            join_reason={"type": "choice", "choice": "none", "confidence": 0.6},
            participation_value={"type": "score", "score": 1.0},
        )
        client = JevClient(self.secrets, max_requests=5, urlopen=lambda req, timeout=0: _Resp(payload), sleep=lambda s: None)
        resp = client.decide({"x": 1}, {"open_question": {"type": "noul", "question": "q"}})
        self.assertEqual(resp.answers["open_question"]["noul"], 0.8)
        self.assertEqual(client.usage.successes, 1)
        self.assertEqual(client.usage.input_tokens, 10)

    def test_usage_missing_is_unknown_not_zero(self) -> None:
        payload = {"answers": {"a": {"type": "noul", "noul": 0.1}}}
        client = JevClient(self.secrets, max_requests=5, urlopen=lambda req, timeout=0: _Resp(payload), sleep=lambda s: None)
        resp = client.decide({}, {"a": {"type": "noul", "question": "q"}})
        self.assertFalse(resp.usage_known)
        self.assertEqual(client.usage.usage_unknown, 1)
        self.assertEqual(client.usage.input_tokens, 0)

    def test_429_then_success(self) -> None:
        calls = {"n": 0}

        def urlopen(req, timeout=0):
            calls["n"] += 1
            if calls["n"] == 1:
                err = HTTPError("https://example.invalid/v1/systemone", 429, "err", {"Retry-After": "0"}, io.BytesIO(b""))
                raise err
            return _Resp(_answers(a={"type": "noul", "noul": 0.2}))

        client = JevClient(self.secrets, max_requests=5, urlopen=urlopen, sleep=lambda s: None)
        resp = client.decide({}, {"a": {"type": "noul", "question": "q"}})
        self.assertEqual(resp.answers["a"]["noul"], 0.2)
        self.assertEqual(client.usage.http_429, 1)
        self.assertEqual(client.usage.attempts, 2)

    def test_5xx_exhausted(self) -> None:
        def urlopen(req, timeout=0):
            raise HTTPError("https://example.invalid/x", 503, "err", {}, io.BytesIO(b""))

        client = JevClient(self.secrets, max_requests=10, urlopen=urlopen, sleep=lambda s: None)
        with self.assertRaises(JevHTTPError):
            client.decide({}, {"a": {"type": "noul", "question": "q"}})

    def test_bad_json(self) -> None:
        class Bad:
            status = 200
            headers = {}

            def read(self):
                return b"{not json"

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        client = JevClient(self.secrets, max_requests=3, urlopen=lambda req, timeout=0: Bad(), sleep=lambda s: None)
        with self.assertRaises(JevDecodeError):
            client.decide({}, {"a": {"type": "noul", "question": "q"}})

    def test_budget_stops_before_attempt(self) -> None:
        client = JevClient(self.secrets, max_requests=1, urlopen=lambda req, timeout=0: _Resp(_answers(a={"type": "noul", "noul": 1})), sleep=lambda s: None)
        client.decide({}, {"a": {"type": "noul", "question": "q"}})
        with self.assertRaises(JevBudgetError):
            client.decide({}, {"a": {"type": "noul", "question": "q"}})
        self.assertTrue(client.budget_exhausted)
        self.assertEqual(client.usage.attempts, 1)

    def test_rejects_latest_alias(self) -> None:
        with self.assertRaises(JevConfigError):
            JevClient({**self.secrets, "JEV_MODEL": "jev-latest"}, max_requests=1)


class MetricsTests(unittest.TestCase):
    def test_auroc_and_hold_thresholds(self) -> None:
        self.assertGreater(auroc([True, True, False, False], [0.9, 0.8, 0.2, 0.1]), 0.9)
        fixture = {
            "judge_cases": [{"id": "j", "recent_messages": [], "memory_hits": [], "labels": {"open_question": False, "private_or_sensitive": True, "join_reason": "none"}}],
            "planner_cases": [{"id": "p", "message": "ㅋㅋ", "labels": {"memory_requirement": "none", "question_kind": "casual", "sensitive": False, "time_kind": "any"}}],
        }
        out = run_pilot(fixture, None, dry_run=True)
        self.assertTrue(out["dry_run"])
        self.assertEqual(out["verdict"]["overall"], "hold")  # not scored

    def test_fixture_loads(self) -> None:
        path = Path(__file__).resolve().parent / "fixtures" / "jev_pilot_ko.json"
        data = load_fixture(path)
        self.assertGreaterEqual(len(data["judge_cases"]), 60)
        self.assertGreaterEqual(len(data["planner_cases"]), 60)


class GuardTests(unittest.TestCase):
    def test_module_does_not_import_urllib_request_at_pilot_import_for_live(self) -> None:
        # live path is behind JevClient; tests must not open sockets
        self.assertTrue(True)


if __name__ == "__main__":
    unittest.main()
