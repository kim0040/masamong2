"""PlayClient 비용 회계: 응답 후 기록 우선, 원자적 예약, 시도별 과금 상태 (fake transport only)."""

from __future__ import annotations

import io
import json
import threading
import unittest
import urllib.error
import urllib.request
from typing import Any, Callable
from unittest import mock

import client as C

SECRETS = {"OPENROUTER_API_KEY": "synthetic-key", "OPENROUTER_BASE_URL": "http://fake.invalid/v1"}


def _no_network(*args: Any, **kwargs: Any) -> Any:
    raise AssertionError("unexpected real network call")


class FakeResponse:
    """urllib 응답 흉내: read() 와 줄 단위 iter, context manager, closed 추적."""

    def __init__(self, body: bytes | None = None, *, lines: list[bytes] | None = None, fail_after: int | None = None,
                 fail_with: BaseException | None = None) -> None:
        self._body = body or b""
        self._lines = list(lines or [])
        self._fail_after = fail_after
        self._fail_with = fail_with or ConnectionResetError("reset")
        self.closed = False

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *exc: Any) -> bool:
        self.close()
        return False

    def close(self) -> None:
        self.closed = True

    def read(self) -> bytes:
        return self._body

    def __iter__(self):
        for i, line in enumerate(self._lines):
            if self._fail_after is not None and i >= self._fail_after:
                raise self._fail_with
            yield line
        if self._fail_after is not None and self._fail_after >= len(self._lines):
            raise self._fail_with


class FakeTransport:
    """스크립트된 결과를 순서대로 돌려주는 urlopen. 항목은 FakeResponse 또는 예외(raise)다."""

    def __init__(self, script: list[Any]) -> None:
        self.script = list(script)
        self.sent: list[dict[str, Any]] = []
        self.lock = threading.Lock()

    def __call__(self, req: urllib.request.Request, timeout: float = 0) -> FakeResponse:
        with self.lock:
            self.sent.append({"url": req.full_url, "payload": json.loads(req.data.decode("utf-8")),
                              "accept": req.get_header("Accept")})
            if not self.script:
                raise AssertionError("transport script exhausted: more requests than expected")
            item = self.script.pop(0)
        if isinstance(item, BaseException):
            raise item
        if callable(item):
            item = item()
        return item


def json_response(obj: dict[str, Any]) -> FakeResponse:
    return FakeResponse(json.dumps(obj).encode("utf-8"))


def chat_body(content: str = "ok", *, cost: float | None = 0.02, usage: bool = True) -> dict[str, Any]:
    body: dict[str, Any] = {"choices": [{"message": {"content": content}}]}
    if usage:
        u: dict[str, Any] = {"prompt_tokens": 10, "completion_tokens": 5}
        if cost is not None:
            u["cost"] = cost
        body["usage"] = u
    return body


def http_error(code: int) -> urllib.error.HTTPError:
    return urllib.error.HTTPError("http://fake.invalid/v1/x", code, "err", {}, io.BytesIO(b""))


def make_client(script: list[Any], *, budget: float = 1.0, **kwargs: Any) -> tuple[C.PlayClient, FakeTransport]:
    transport = FakeTransport(script)
    cli = C.PlayClient(budget_usd=budget, secrets=SECRETS, urlopen=transport, sleep=lambda s: None, **kwargs)
    return cli, transport


MSG = [{"role": "user", "content": "hi"}]


class NoNetworkMixin(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.object(urllib.request, "urlopen", _no_network)
        patcher.start()
        self.addCleanup(patcher.stop)


class UsageAccountingTests(NoNetworkMixin):
    def test_default_transport_is_guarded_in_tests(self) -> None:
        cli = C.PlayClient(budget_usd=1.0, secrets=SECRETS, sleep=lambda s: None)
        with self.assertRaises(C.ClientError):
            cli.chat(MSG)
        self.assertEqual(cli.usage.errors.get("AssertionError"), 6)

    def test_completed_cost_recorded_then_next_request_blocked(self) -> None:
        # F4 재현: budget 0.01, 완료된 응답 cost 0.02 → 0.02 기록, 다음 요청은 전송 전에 막힘.
        cli, transport = make_client([json_response(chat_body(cost=0.02)), json_response(chat_body(cost=0.02))], budget=0.01)
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertEqual(len(transport.sent), 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.02)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.02)
        self.assertEqual(cli.usage.calls, 1)
        self.assertEqual(cli.usage.attempts, 1)
        self.assertEqual(cli.usage.events[0]["status"], "ok")
        self.assertEqual(cli.usage.events[0]["cost_basis"], "provider")
        with self.assertRaises(C.BudgetError):
            cli.chat(MSG)
        self.assertEqual(len(transport.sent), 1, "예산 초과 요청은 전송되면 안 된다")
        self.assertEqual(cli.usage.attempts, 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.02, msg="실패 후에도 기록이 남는다")
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_parallel_reservation_protects_budget_without_double_settle(self) -> None:
        # 동시에 두 요청: 첫 요청이 전송 중(예약 보유)이면 두 번째는 그 정산을 기다린 뒤
        # 확정 비용 기준으로 판단한다. 0.012 정산 + 0.01 예약 > 0.015 → 전송 없이 막힌다.
        in_flight = threading.Event()
        release = threading.Event()

        def _slow() -> FakeResponse:
            in_flight.set()
            release.wait(5)
            return json_response(chat_body(cost=0.012))

        cli, transport = make_client([_slow, json_response(chat_body(cost=0.012))], budget=0.015)
        results: dict[str, Any] = {}

        def _first() -> None:
            results["first"] = cli.chat(MSG)

        def _second() -> None:
            try:
                results["second"] = cli.chat(MSG)
            except C.BudgetError as exc:
                results["second_error"] = exc

        t1 = threading.Thread(target=_first)
        t1.start()
        self.assertTrue(in_flight.wait(5))
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.01)
        t2 = threading.Thread(target=_second)
        t2.start()
        t2.join(0.2)
        self.assertTrue(t2.is_alive(), "다른 스레드 예약 때문에만 모자라면 기다린다")
        release.set()
        t1.join(5)
        t2.join(5)
        self.assertEqual(results.get("first"), "ok")
        self.assertIsInstance(results.get("second_error"), C.BudgetError)
        self.assertEqual(len(transport.sent), 1)
        self.assertEqual(cli.usage.attempts, 1)
        self.assertEqual(len(cli.usage.events), 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.012)
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_parallel_reservation_proceeds_when_settled_cost_leaves_room(self) -> None:
        # 예약 상한 0.015 두 개는 0.025를 넘지만, 첫 요청이 0.005로 정산되면 두 번째는 보내진다.
        in_flight = threading.Event()
        release = threading.Event()

        def _slow() -> FakeResponse:
            in_flight.set()
            release.wait(5)
            return json_response(chat_body(cost=0.005))

        cli, transport = make_client(
            [_slow, json_response(chat_body(cost=0.005))], budget=0.025, max_request_cost_usd=0.015
        )
        results: dict[str, Any] = {}
        t1 = threading.Thread(target=lambda: results.__setitem__("first", cli.chat(MSG)))
        t1.start()
        self.assertTrue(in_flight.wait(5))
        t2 = threading.Thread(target=lambda: results.__setitem__("second", cli.chat(MSG)))
        t2.start()
        t2.join(0.2)
        self.assertTrue(t2.is_alive())
        release.set()
        t1.join(5)
        t2.join(5)
        self.assertEqual((results.get("first"), results.get("second")), ("ok", "ok"))
        self.assertEqual(len(transport.sent), 2)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_max_request_cost_reservation_cap(self) -> None:
        cli, transport = make_client([json_response(chat_body(cost=0.01))], budget=0.2, max_request_cost_usd=0.15)
        attempt = cli._reserve(kind="chat", model="m", estimate=0.01)
        self.assertAlmostEqual(attempt.reservation, 0.15)
        with self.assertRaises(C.BudgetError):
            cli._reserve(kind="chat", model="m", estimate=0.01)  # 0.15 + 0.15 > 0.2
        cli._fail(attempt, billed="no")
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)
        with self.assertRaises(C.ClientError):
            C.PlayClient(budget_usd=1.0, secrets=SECRETS, max_request_cost_usd=0)

    def test_missing_provider_cost_is_estimated_not_zero(self) -> None:
        cli, _ = make_client([json_response(chat_body(cost=None))])
        cli.chat(MSG)
        ev = cli.usage.events[0]
        self.assertEqual(ev["cost_basis"], "estimate")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)
        self.assertAlmostEqual(cli.usage.estimated_cost_usd, 0.01)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.0)
        self.assertEqual(ev["prompt_tokens"], 10)

    def test_http_error_retry_is_not_billed(self) -> None:
        cli, transport = make_client([http_error(503), json_response(chat_body(cost=0.005))])
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertEqual(len(transport.sent), 2)
        self.assertEqual(cli.usage.attempts, 2)
        self.assertEqual(cli.usage.calls, 1)
        self.assertEqual(cli.usage.failed_attempts, 1)
        self.assertEqual([e["status"] for e in cli.usage.events], ["failed", "ok"])
        self.assertEqual(cli.usage.events[0]["cost_basis"], "none")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.005)
        self.assertEqual(cli.usage.errors, {"HTTP 503": 1})
        self.assertEqual(cli.usage.unknown_cost_attempts, 0)

    def test_non_retryable_http_error_stops(self) -> None:
        cli, transport = make_client([http_error(400)])
        with self.assertRaises(C.ClientError):
            cli.chat(MSG)
        self.assertEqual(len(transport.sent), 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.0)

    def test_timeout_after_send_keeps_unknown_cost(self) -> None:
        cli, transport = make_client([TimeoutError("timed out"), json_response(chat_body(cost=0.004))])
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertEqual(len(transport.sent), 2)
        self.assertEqual(cli.usage.unknown_cost_attempts, 1)
        self.assertAlmostEqual(cli.usage.unknown_cost_usd, 0.01)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.014)
        self.assertEqual(cli.usage.events[0]["cost_basis"], "reservation")
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_connection_refused_is_not_billed(self) -> None:
        cli, _ = make_client([urllib.error.URLError(ConnectionRefusedError()), json_response(chat_body(cost=0.004))])
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertEqual(cli.usage.unknown_cost_attempts, 0)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.004)

    def test_provider_error_object_with_200(self) -> None:
        # usage 없는 error 객체 → 미확정, usage 있는 error 객체 → 실패로 정산(verified).
        cli, transport = make_client([
            json_response({"error": {"code": 502}}),
            json_response({"error": {"code": 502}, "usage": {"cost": 0.003}}),
            json_response(chat_body(cost=0.002)),
        ])
        self.assertEqual(cli.chat(MSG), "ok")
        self.assertEqual(len(transport.sent), 3)
        statuses = [(e["status"], e["cost_basis"]) for e in cli.usage.events]
        self.assertEqual(statuses, [("failed", "reservation"), ("failed", "provider"), ("ok", "provider")])
        self.assertEqual(cli.usage.unknown_cost_attempts, 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01 + 0.003 + 0.002)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.005)
        self.assertEqual(cli.usage.errors, {"provider_error": 2})

    def test_empty_content_retry_keeps_each_attempt_cost(self) -> None:
        # 기존 WIP: 빈 본문은 재시도. 각 물리 호출의 비용은 모두 남는다.
        cli, transport = make_client([json_response(chat_body("  ", cost=0.001)), json_response(chat_body("x", cost=0.001))])
        self.assertEqual(cli.chat(MSG), "x")
        self.assertEqual(len(transport.sent), 2)
        self.assertEqual(cli.usage.calls, 2)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.002)

    def test_embed_charges_through_post(self) -> None:
        cli, transport = make_client([json_response({"data": [{"index": 0, "embedding": [0.1, 0.2]}], "usage": {"cost": 0.0001}})])
        out = cli.embed(["q"], kind="query")
        self.assertEqual(out, [[0.1, 0.2]])
        self.assertEqual(cli.usage.events[0]["kind"], "embed")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.0001)
        self.assertIn("Instruct:", transport.sent[0]["payload"]["input"][0])

    def test_delta_since_cursor_returns_each_event_once(self) -> None:
        cli, _ = make_client([json_response(chat_body(cost=0.001)), json_response(chat_body(cost=0.002))])
        cli.chat(MSG)
        cursor0 = cli.usage.as_dict()["cursor"]
        cli.chat(MSG)
        first = cli.usage.delta_since(0)
        self.assertEqual(len(first["events"]), 2)
        self.assertAlmostEqual(first["cost_usd"], cli.usage.cost_usd)
        self.assertEqual(len(set(first["attempt_ids"])), 2)
        self.assertTrue(all(a.startswith(cli.client_id + ":") for a in first["attempt_ids"]))
        second = cli.usage.delta_since(cursor0)
        self.assertEqual(len(second["events"]), 1)
        self.assertAlmostEqual(second["cost_usd"], 0.002)
        self.assertEqual(cli.usage.delta_since(second["cursor"])["events"], [])
        snap = cli.usage.as_dict()
        self.assertTrue(snap["cost_settled"])
        self.assertEqual(snap["cursor"], 2)
        json.dumps(snap)


if __name__ == "__main__":
    unittest.main()
