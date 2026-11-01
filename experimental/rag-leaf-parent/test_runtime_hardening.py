"""Sandbox chat/retrieve wiring — synthetic index, no network, no raw corpus."""

from __future__ import annotations

import json
import os
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

import engine as E
from client import StreamError, Usage
from retrieval_post import TimeReference
from test_engine_retrieval import ROOM_NEW, ROOM_OLD, _FakeClient, _unit, build_index


class _TurnClient:
    def __init__(self, plan_json: str = "", answer: str = "응") -> None:
        self.usage = Usage()
        self.llm_model = "fake-llm"
        self.embed_model = "fake-embed"
        self._plan = plan_json
        self._answer = answer
        self.chats: list[list[dict]] = []
        self.stream_error: StreamError | None = None

    def chat(self, messages, **_kwargs):  # noqa: ANN001
        self.chats.append(list(messages))
        if self._plan and len(self.chats) == 1:
            return self._plan
        return self._answer

    def embed(self, texts, **_kwargs):  # noqa: ANN001
        return [_unit(3).tolist() for _ in texts]

    def chat_stream(self, messages, **_kwargs):  # noqa: ANN001
        if self.stream_error:
            raise self.stream_error
        yield {"type": "delta", "text": self._answer}
        yield {"type": "done", "end": "done_marker"}


def _planner_required() -> str:
    return json.dumps(
        {
            "memory_requirement": "required",
            "time": {"kind": "any"},
            "question_kind": "other",
            "sensitive": False,
            "tool_plan": [{"tool_name": "memory.search", "parameters": {"query": "가명"}}],
        },
        ensure_ascii=False,
    )


class RetrievePackingTests(unittest.TestCase):
    def test_hits_expose_visible_spans_not_just_ids(self) -> None:
        index = build_index()
        q = _unit(1)
        trace: dict = {}
        hits = E.retrieve(
            index,
            _FakeClient(q),
            "김가명 D대회",
            top_k=3,
            mode="leaf_parent",
            q_vec=q,
            ranking="heuristic",
            trace=trace,
            source_room_id=ROOM_NEW,
        )
        self.assertTrue(hits)
        self.assertIn("visible_spans", hits[0])
        self.assertIn("visible_observation_ids", hits[0])
        self.assertIn("visible_spans", trace)
        for hit in hits:
            self.assertEqual(hit.get("source_room_id"), ROOM_NEW)

    def test_hard_yesterday_does_not_take_old_room_parent(self) -> None:
        index = build_index()
        q = _unit(2)
        ref = TimeReference(
            mode="replay",
            now=datetime(2026, 9, 2, 12, 0, tzinfo=timezone.utc),
            source="test",
        )
        trace: dict = {}
        hits = E.retrieve(
            index,
            _FakeClient(q),
            "어제 뭐했어",
            top_k=5,
            q_vec=q,
            ranking="heuristic",
            trace=trace,
            source_room_id=ROOM_OLD,
            intent={"recent": False, "time": {"kind": "yesterday"}, "kind": "event", "source": "plan"},
            time_reference=ref,
        )
        for hit in hits:
            self.assertEqual(hit.get("source_room_id"), ROOM_OLD)
            for span in hit.get("visible_spans") or []:
                oid = str(span.get("observation_id") or "")
                self.assertTrue(oid.startswith(ROOM_OLD) or not oid)


class LedgerAndStreamTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name)
        self.prev = E.DATA
        self.prev_secrets = os.environ.get("EVAL_SECRETS")
        self.prev_legacy = os.environ.get("MASAMONG_LEGACY_ROOT")
        E._bind_data_dir(self.data)
        os.environ["MASAMONG_LEGACY_ROOT"] = str(self.data / "no-legacy")
        os.environ["EVAL_SECRETS"] = str(self.data / "secrets.env")
        (self.data / "secrets.env").write_text("OPENROUTER_API_KEY=synthetic\n", encoding="utf-8")

    def tearDown(self) -> None:
        E._bind_data_dir(self.prev)
        if self.prev_secrets is None:
            os.environ.pop("EVAL_SECRETS", None)
        else:
            os.environ["EVAL_SECRETS"] = self.prev_secrets
        if self.prev_legacy is None:
            os.environ.pop("MASAMONG_LEGACY_ROOT", None)
        else:
            os.environ["MASAMONG_LEGACY_ROOT"] = self.prev_legacy
        self.tmp.cleanup()

    def test_apply_client_usage_dedupes_attempt_id(self) -> None:
        class _C:
            usage = Usage()

        _C.usage.events = [
            {"attempt_id": "a1", "cost": 0.02, "cost_basis": "provider"},
            {"attempt_id": "a1", "cost": 0.02, "cost_basis": "provider"},
        ]
        first = E.apply_client_usage(_C(), cursor=0)
        second = E.apply_client_usage(_C(), cursor=0)
        self.assertAlmostEqual(first["spent_usd"], 0.02)
        self.assertAlmostEqual(second["spent_usd"], 0.0)
        self.assertAlmostEqual(E.load_ledger()["spent_usd"], 0.02)

    def _runtime_index(self, *, tools_enabled):
        index = build_index(with_summaries=False)
        index.cfg["session_budget_usd"] = 5.0
        index.cfg["turn_budget_usd"] = 1.0
        index.cfg["ranking"] = "heuristic"
        index.cfg["top_k"] = 3
        runtime = {
            "schema_version": 1,
            "agent_system_prompt": "test",
            "lite_system_prompt": "",
            "channels": {},
            "active_channel_id": "",
            "tools_enabled": tools_enabled,
            "tool_defaults": {},
        }
        (self.data / "bot_runtime.json").write_text(json.dumps(runtime), encoding="utf-8")
        return index

    def test_empty_tools_bang_weather_is_disabled(self) -> None:
        index = self._runtime_index(tools_enabled=[])
        client = _TurnClient()
        with mock.patch("engine.execute_named_tool", side_effect=AssertionError("tool must not run")):
            bundle = E._prepare_bot_turn(
                index, client, message="!날씨 부산", ranking="heuristic", remaining=1.0
            )
        self.assertTrue(bundle["tool_results"])
        self.assertFalse(bundle["tool_results"][0].get("ok"))
        self.assertEqual(bundle["tool_results"][0].get("status"), "disabled")
        self.assertEqual(bundle["rag_state"], "not_attempted")

    def test_required_plan_with_memory_off_refuses_search(self) -> None:
        index = self._runtime_index(tools_enabled=["help.version"])
        client = _TurnClient(plan_json=_planner_required())
        called = {"retrieve": False}

        def _no_retrieve(*_a, **_k):
            called["retrieve"] = True
            return []

        with mock.patch("engine.retrieve", side_effect=_no_retrieve):
            bundle = E._prepare_bot_turn(
                index,
                client,
                message="마사몽 가명이 누구야",
                ranking="heuristic",
                remaining=1.0,
                history=[{"role": "user", "content": "어제 부산 갔었지"}],
            )
        self.assertFalse(called["retrieve"])
        self.assertEqual(bundle["rag_state"], "memory_search_disabled")
        user = client.chats[0][-1]["content"]
        self.assertIn("어제 부산", user)
        self.assertIn("가명이 누구야", user)

    def test_empty_time_window_is_searched_once_more_without_it(self) -> None:
        # Go: chat.TestEmptyTimeWindowIsSearchedOnceMoreWithoutIt
        index = self._runtime_index(tools_enabled=["memory.search"])
        room = E._indexed_rooms(index)[0]
        calls: list[dict] = []

        def _retrieve(_index, _client, _query, **kw):  # noqa: ANN001
            calls.append(kw["intent"])
            if kw["intent"].get("recent") or (kw["intent"].get("time") or {}).get("kind", "any") != "any":
                return []
            kw["trace"]["rerank_cost_usd"] = 0.0004
            return [{"leaf_id": "l1", "source_room_id": room, "text": "가명"}]

        def _plan(time_kind: str) -> str:
            return json.dumps(
                {
                    "memory_requirement": "required",
                    "time": {"kind": time_kind},
                    "question_kind": "other",
                    "sensitive": False,
                    "tool_plan": [{"tool_name": "memory.search", "parameters": {"query": "가명"}}],
                },
                ensure_ascii=False,
            )

        with mock.patch("engine.retrieve", side_effect=_retrieve):
            bundle = E._prepare_bot_turn(
                index, _TurnClient(plan_json=_plan("recent")), message="요즘 가명 누구야", ranking="heuristic", remaining=1.0
            )
        self.assertEqual(len(calls), 2)
        self.assertTrue(calls[0]["recent"])
        self.assertFalse(calls[1]["recent"])
        self.assertIsNone(calls[1]["time"])
        self.assertEqual(bundle["rag_state"], "candidates")
        self.assertTrue(bundle["rank_trace"]["time_relaxed"])
        self.assertNotIn("abstain_reason", bundle["rank_trace"])
        self.assertAlmostEqual(bundle["rank_trace"]["rerank_cost_usd"], 0.0004)
        self.assertFalse(bundle["intent"]["recent"])  # the answer is not told "recent"
        # an explicit range is relaxed the same way; the rerank floor is not
        calls.clear()
        with mock.patch("engine.retrieve", side_effect=_retrieve):
            E._prepare_bot_turn(
                index, _TurnClient(plan_json=_plan("between")), message="가명 누구야", ranking="heuristic", remaining=1.0
            )
        self.assertEqual(len(calls), 2)

        def _floor(_index, _client, _query, **kw):  # noqa: ANN001
            calls.append(kw["intent"])
            kw["trace"]["abstained"] = True
            return []

        calls.clear()
        with mock.patch("engine.retrieve", side_effect=_floor):
            E._prepare_bot_turn(
                index, _TurnClient(plan_json=_plan("recent")), message="요즘 가명 누구야", ranking="heuristic", remaining=1.0
            )
        self.assertEqual(len(calls), 1)
        # no window: one search, even when it is empty
        calls.clear()
        with mock.patch("engine.retrieve", side_effect=lambda *_a, **kw: calls.append(kw["intent"]) or []):
            bundle = E._prepare_bot_turn(
                index, _TurnClient(plan_json=_plan("any")), message="가명 누구야", ranking="heuristic", remaining=1.0
            )
        self.assertEqual(len(calls), 1)
        self.assertEqual(bundle["rag_state"], "empty_in_allowed_scope")

    def test_chat_stream_error_is_not_done(self) -> None:
        index = self._runtime_index(tools_enabled=[])
        client = _TurnClient()
        client.stream_error = StreamError("cut", status="truncated", partial_chars=2)
        index.cfg["max_history_turns"] = 4

        def _make(_cfg, **_k):
            return client

        events = []
        with mock.patch("engine.make_client", side_effect=_make):
            with mock.patch("engine._prepare_bot_turn", return_value={
                "hits": [],
                "system": "sys",
                "tool_results": [],
                "plan": [],
                "plan_raw": "",
                "memory_requirement": "none",
                "rag_state": "not_attempted",
                "search_query": "",
                "intent": {"kind": "other", "source": "plan"},
                "channel_id": "200",
                "rank_trace": {},
                "runtime": {},
                "channel": {},
                "yield_status": "planning",
                "timing": {},
                "rag_used": False,
            }):
                for ev in E.chat_stream(index, message="!버전", persist=False):
                    events.append(ev)
        kinds = [e.get("event") for e in events]
        self.assertIn("error", kinds)
        self.assertNotIn("done", kinds)


class PromptSeparationTests(unittest.TestCase):
    def test_summary_is_not_in_raw_evidence_block(self) -> None:
        index = build_index()
        hits = [
            {
                "doc_id": "L1",
                "source_room_id": ROOM_NEW,
                "span": "0-1",
                "speakers": ["가명"],
                "date": "2026-09-01",
                "excerpt": "원문만",
                "summary": "요약은 여기",
                "packing_status": "complete",
            }
        ]
        messages = E._build_chat_messages(
            index,
            message="뭐였지",
            history=[{"role": "assistant", "content": "예전에 단정했음"}],
            hits=hits,
            session_summary="세션 요약 문장",
            rag_state="candidates",
        )
        blob = messages[-1]["content"]
        self.assertIn("<근거원문>", blob)
        self.assertIn("원문만", blob)
        self.assertIn("<생성요약 원문아님>", blob)
        self.assertIn("요약은 여기", blob)
        raw = blob.split("<근거원문>")[1].split("</근거원문>")[0]
        self.assertNotIn("요약은 여기", raw)
        self.assertIn("세션 요약 문장", blob.split("<세션요약")[1].split("</세션요약>")[0])


if __name__ == "__main__":
    unittest.main()
