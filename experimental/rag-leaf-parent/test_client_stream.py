"""PlayClient.chat_stream 종료 상태: SSE error·빈 스트림·손상·끊김·정상 종료·usage-only·취소 (fake transport only)."""

from __future__ import annotations

import json
import unittest
from typing import Any

import client as C
from test_client_usage import MSG, FakeResponse, NoNetworkMixin, http_error, make_client


def sse(*chunks: Any) -> list[bytes]:
    out: list[bytes] = []
    for c in chunks:
        if isinstance(c, bytes):
            out.append(c)
        elif c == "[DONE]":
            out.append(b"data: [DONE]\n")
        else:
            out.append(b"data: " + json.dumps(c).encode("utf-8") + b"\n")
        out.append(b"\n")
    return out


def delta(text: str, finish: str | None = None) -> dict[str, Any]:
    choice: dict[str, Any] = {"index": 0, "delta": {"content": text}}
    if finish:
        choice["finish_reason"] = finish
    return {"choices": [choice]}


def usage_only(cost: float | None = 0.003) -> dict[str, Any]:
    u: dict[str, Any] = {"prompt_tokens": 20, "completion_tokens": 7}
    if cost is not None:
        u["cost"] = cost
    return {"choices": [], "usage": u}


def stream_response(*chunks: Any, **kw: Any) -> FakeResponse:
    return FakeResponse(lines=sse(*chunks), **kw)


def run(cli: C.PlayClient) -> tuple[list[dict[str, Any]], BaseException | None]:
    events: list[dict[str, Any]] = []
    try:
        for ev in cli.chat_stream(MSG):
            events.append(ev)
    except C.ClientError as exc:
        return events, exc
    return events, None


class StreamTerminalStateTests(NoNetworkMixin):
    def assertNoDone(self, events: list[dict[str, Any]]) -> None:
        self.assertNotIn("done", [e["type"] for e in events])

    def test_sse_error_with_done_marker_is_not_success(self) -> None:
        # F5 재현: HTTP 200 + SSE error + [DONE] → 정상 done 아님.
        cli, transport = make_client([stream_response(delta("partial"), {"error": {"code": 502, "message": "x"}}, "[DONE]")])
        events, exc = run(cli)
        self.assertEqual([e["type"] for e in events], ["delta"])
        self.assertIsInstance(exc, C.StreamError)
        self.assertEqual(exc.status, "provider_error")
        self.assertEqual(exc.partial_chars, len("partial"))
        self.assertNotIn("x", str(exc))
        self.assertEqual(len(transport.sent), 1, "응답이 시작된 요청은 재시도하지 않는다")
        self.assertEqual(transport.sent[0]["accept"], "text/event-stream")
        self.assertEqual(cli.usage.calls, 0)
        self.assertEqual(cli.usage.unknown_cost_attempts, 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)
        self.assertEqual(cli.usage.events[0]["status"], "failed")
        self.assertIn("sse_error_502", cli.usage.errors)
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_sse_error_with_usage_settles_verified_cost(self) -> None:
        cli, _ = make_client([stream_response({"error": {"code": 429}}, usage_only(0.0025), "[DONE]")])
        events, exc = run(cli)
        self.assertNoDone(events)
        self.assertEqual(exc.status, "provider_error")
        self.assertEqual(cli.usage.unknown_cost_attempts, 0)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.0025)
        self.assertEqual(cli.usage.events[0]["status"], "failed")

    def test_finish_reason_error_is_not_success(self) -> None:
        cli, _ = make_client([stream_response(delta("a"), {"choices": [{"index": 0, "delta": {}, "finish_reason": "error"}]}, "[DONE]")])
        events, exc = run(cli)
        self.assertEqual(len(events), 1)
        self.assertEqual(exc.status, "provider_error")
        self.assertIn("finish_error", cli.usage.errors)

    def test_truncated_after_tokens_has_no_done(self) -> None:
        cli, transport = make_client([stream_response(delta("par"), delta("t"))])
        events, exc = run(cli)
        self.assertEqual([e["type"] for e in events], ["delta", "delta"])
        self.assertEqual(exc.status, "truncated")
        self.assertEqual(exc.partial_chars, 4)
        self.assertEqual(exc.attempt_id, cli.usage.events[0]["attempt_id"])
        self.assertEqual(len(transport.sent), 1)
        self.assertEqual(cli.usage.unknown_cost_attempts, 1, "usage 를 못 받았으면 미확정으로 남긴다")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)

    def test_transport_error_mid_stream(self) -> None:
        cli, transport = make_client([stream_response(delta("ab"), fail_after=2)])
        events, exc = run(cli)
        self.assertEqual([e["type"] for e in events], ["delta"])
        self.assertEqual(exc.status, "transport")
        self.assertEqual(len(transport.sent), 1)
        self.assertEqual(cli.usage.errors.get("ConnectionResetError"), 1)
        self.assertEqual(cli.usage.unknown_cost_attempts, 1)

    def test_empty_stream_is_error(self) -> None:
        cli, _ = make_client([stream_response("[DONE]")])
        events, exc = run(cli)
        self.assertEqual(events, [])
        self.assertEqual(exc.status, "empty")
        self.assertEqual(cli.usage.calls, 0)

    def test_malformed_chunk_is_error(self) -> None:
        cli, _ = make_client([stream_response(delta("ok"), b"data: {broken\n", "[DONE]")])
        events, exc = run(cli)
        self.assertEqual(len(events), 1)
        self.assertEqual(exc.status, "malformed")

    def test_success_with_late_usage_charges_once(self) -> None:
        cli, transport = make_client([stream_response(b": OPENROUTER PROCESSING\n", delta("he"), delta("llo", finish="stop"),
                                                       usage_only(0.0031), "[DONE]")])
        events, exc = run(cli)
        self.assertIsNone(exc)
        self.assertEqual([e["type"] for e in events], ["delta", "delta", "done"])
        done = events[-1]
        self.assertEqual(done["end"], "done_marker")
        self.assertEqual(done["finish_reason"], "stop")
        self.assertEqual(done["cost_basis"], "provider")
        self.assertAlmostEqual(done["call_cost"], 0.0031)
        self.assertAlmostEqual(done["cost"], 0.0031)
        self.assertEqual(done["chars"], 5)
        self.assertEqual(cli.usage.calls, 1)
        self.assertEqual(len(cli.usage.events), 1, "usage-only 이벤트로 이중 기록 없음")
        self.assertAlmostEqual(cli.usage.cost_usd, 0.0031)
        self.assertEqual(cli.usage.events[0]["completion_tokens"], 7)
        self.assertEqual(len(transport.sent), 1)
        self.assertEqual(transport.sent[0]["payload"]["stream_options"], {"include_usage": True})

    def test_success_without_usage_is_estimated(self) -> None:
        cli, _ = make_client([stream_response(delta("x"), "[DONE]")])
        events, exc = run(cli)
        self.assertIsNone(exc)
        self.assertEqual(events[-1]["cost_basis"], "estimate")
        self.assertAlmostEqual(cli.usage.estimated_cost_usd, 0.01)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.0)

    def test_consumer_close_cleans_up_and_keeps_unknown_cost(self) -> None:
        resp = stream_response(delta("a"), delta("b"), usage_only(), "[DONE]")
        cli, _ = make_client([resp])
        gen = cli.chat_stream(MSG)
        self.assertEqual(next(gen)["text"], "a")
        gen.close()
        self.assertTrue(resp.closed)
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)
        self.assertEqual(cli.usage.unknown_cost_attempts, 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.01)
        self.assertEqual(cli.usage.events[0]["status"], "cancelled")
        self.assertEqual(cli.usage.errors.get("stream_cancelled"), 1)
        self.assertEqual(cli.usage.calls, 0)

    def test_consumer_close_after_usage_settles_verified(self) -> None:
        resp = stream_response(usage_only(0.002), delta("a"), delta("b"), "[DONE]")
        cli, _ = make_client([resp])
        gen = cli.chat_stream(MSG)
        next(gen)
        gen.close()
        self.assertTrue(resp.closed)
        self.assertEqual(cli.usage.unknown_cost_attempts, 0)
        self.assertAlmostEqual(cli.usage.verified_cost_usd, 0.002)
        self.assertEqual(cli.usage.events[0]["status"], "cancelled")
        self.assertAlmostEqual(cli.usage.reserved_cost_usd, 0.0)

    def test_retryable_http_error_before_stream_is_retried_unbilled(self) -> None:
        cli, transport = make_client([http_error(429), stream_response(delta("x"), "[DONE]")])
        events, exc = run(cli)
        self.assertIsNone(exc)
        self.assertEqual(len(transport.sent), 2)
        self.assertEqual([e["status"] for e in cli.usage.events], ["failed", "ok"])
        self.assertAlmostEqual(cli.usage.events[0]["cost"], 0.0)
        self.assertEqual(cli.usage.calls, 1)

    def test_non_retryable_http_error_before_stream(self) -> None:
        cli, transport = make_client([http_error(401)])
        events, exc = run(cli)
        self.assertEqual(events, [])
        self.assertIsInstance(exc, C.ClientError)
        self.assertNotIsInstance(exc, C.StreamError)
        self.assertEqual(len(transport.sent), 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.0)

    def test_budget_blocks_before_send(self) -> None:
        cli, transport = make_client([stream_response(delta("x"), "[DONE]")], budget=0.005)
        events, exc = run(cli)
        self.assertIsInstance(exc, C.BudgetError)
        self.assertEqual(transport.sent, [])
        self.assertEqual(cli.usage.attempts, 0)

    def test_overrun_recorded_then_next_stream_blocked(self) -> None:
        cli, transport = make_client([stream_response(delta("x"), usage_only(0.02), "[DONE]")] * 2, budget=0.01)
        events, exc = run(cli)
        self.assertIsNone(exc)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.02)
        events, exc = run(cli)
        self.assertIsInstance(exc, C.BudgetError)
        self.assertEqual(len(transport.sent), 1)
        self.assertAlmostEqual(cli.usage.cost_usd, 0.02)


if __name__ == "__main__":
    unittest.main()
