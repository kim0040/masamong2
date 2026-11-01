"""Structured time constraint (planner v2 `time`) is applied as scope, per room, before ranking."""

from __future__ import annotations

import unittest
from datetime import datetime, timedelta, timezone

import engine as E
from bot_plan import intent_from_plan, parse_plan
from retrieval_post import TimeReference, narrow_time_ids, time_bounds
from test_engine_retrieval import ROOM_NEW, ROOM_OLD, _FakeClient, _unit, build_index


def _intent(kind: str, frm=None, to=None, recent=False):
    return {"recent": recent, "intro": False, "politics": False, "prefer_missing_over_guess": False,
            "kind": "event", "targets": [], "source": "planner",
            "time": {"kind": kind, "from": frm, "to": to}}


class ParsePlanV2Tests(unittest.TestCase):
    def test_v2_fields_and_v1_derivation(self) -> None:
        parsed = parse_plan('{"memory_requirement":"required","time_scope":"any","question_kind":"person_fact",'
                            '"person_targets":["현민"],"semantic_query":"현민이 서버 구성에 대해 말한 내용",'
                            '"key_terms":["서버","구성","서버"],"time":{"kind":"last_week"},"tool_plan":[]}')
        self.assertEqual(parsed["time"], {"kind": "last_week", "from": None, "to": None})
        self.assertEqual(parsed["key_terms"], ["서버", "구성"])
        it = intent_from_plan(parsed)
        self.assertFalse(it["recent"]); self.assertEqual(it["time"]["kind"], "last_week")
        self.assertEqual(it["semantic_query"], "현민이 서버 구성에 대해 말한 내용")
        self.assertEqual(it["memory_search_query"], "")
        # v1 output: time derived from time_scope
        v1 = intent_from_plan(parse_plan('{"memory_requirement":"required","time_scope":"recent","tool_plan":[]}'))
        self.assertEqual(v1["time"]["kind"], "recent"); self.assertTrue(v1["recent"])
        self.assertEqual(v1["semantic_query"], ""); self.assertEqual(v1["key_terms"], [])
        # time.kind=recent without time_scope flips v1 flag
        v2 = parse_plan('{"memory_requirement":"required","time":{"kind":"recent"},"tool_plan":[]}')
        self.assertEqual(v2["time_scope"], "recent")
        # bad kind → derived from scope; from/to only kept for absolute kinds
        bad = parse_plan('{"memory_requirement":"required","time":{"kind":"sometime","from":"2026-01-01"},"tool_plan":[]}')
        self.assertEqual(bad["time"], {"kind": "any", "from": None, "to": None})
        abs_ = parse_plan('{"memory_requirement":"required","time":{"kind":"between","from":"2026-01-01","to":"2026-02-01"},"tool_plan":[]}')
        self.assertEqual(abs_["time"]["from"], "2026-01-01")


class TimeBoundsTests(unittest.TestCase):
    def test_relative_and_absolute(self) -> None:
        anchor = datetime(2026, 9, 10, 15, 0).timestamp()
        lo, hi = time_bounds("today", anchor_ts=anchor)
        self.assertEqual(datetime.fromtimestamp(lo).hour, 0)
        self.assertEqual(datetime.fromtimestamp(lo).day, 10)
        self.assertEqual(datetime.fromtimestamp(hi).day, 11)
        lo, hi = time_bounds("yesterday", anchor_ts=anchor)
        self.assertEqual(datetime.fromtimestamp(lo).day, 9)
        self.assertEqual(datetime.fromtimestamp(hi).day, 10)
        lo, hi = time_bounds("last_week", anchor_ts=anchor)
        self.assertEqual(datetime.fromtimestamp(lo).day, 3)
        self.assertEqual(datetime.fromtimestamp(hi).day, 11)
        lo, _ = time_bounds("this_month", anchor_ts=anchor)
        self.assertEqual(datetime.fromtimestamp(lo).day, 1)
        lo, hi = time_bounds("between", anchor_ts=anchor, frm="2026-02-01", to="2026-01-01")
        self.assertLess(lo, hi)  # swapped bounds normalised
        self.assertIsNone(time_bounds("between", anchor_ts=anchor, frm="??", to="2026-01-01"))
        lo1, hi1 = time_bounds("last_week", anchor_ts=anchor, scale=2.0)
        lo2, hi2 = time_bounds("last_week", anchor_ts=anchor, scale=1.0)
        self.assertEqual((lo1, hi1), (lo2, hi2))  # hard window ignores recovery scale


class NarrowTimeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.index = build_index()
        self.new = {d for d in self.index.leaf_ids if self.index.leaf_by_id[d]["source_room_id"] == ROOM_NEW}
        self.old = {d for d in self.index.leaf_ids if self.index.leaf_by_id[d]["source_room_id"] == ROOM_OLD}

    def _yesterday_of(self, ids: set[str]):
        anchor = max(self.index.leaf_end_ts[d] for d in ids)
        return (datetime.fromtimestamp(anchor) - timedelta(days=1)).date()  # local-tz day boundaries

    def test_yesterday_needs_injected_reference_not_newest_leaf(self) -> None:
        empty, meta = narrow_time_ids(self.index, self.new, _intent("yesterday"))
        self.assertEqual(empty, set())
        self.assertTrue(meta.get("time_unresolved"))
        now = datetime.fromtimestamp(max(self.index.leaf_end_ts[d] for d in self.new), tz=timezone.utc)
        ref = TimeReference(mode="replay", now=now, source="eval:room_newest")
        window, meta2 = narrow_time_ids(self.index, self.new, _intent("yesterday"), reference=ref)
        self.assertTrue(meta2["time_window_applied"])
        self.assertTrue(window)
        want_day = (now.date() - timedelta(days=1))
        for d in window:
            self.assertEqual(datetime.fromtimestamp(self.index.leaf_end_ts[d], tz=timezone.utc).date(), want_day)
        old_now = datetime.fromtimestamp(max(self.index.leaf_end_ts[d] for d in self.old), tz=timezone.utc)
        window_old, _ = narrow_time_ids(
            self.index, self.old, _intent("yesterday"),
            reference=TimeReference(mode="replay", now=old_now, source="eval:old_room"),
        )
        self.assertTrue(window_old)
        self.assertTrue(window_old <= self.old)

    def test_hard_yesterday_ignores_scale(self) -> None:
        now = datetime.fromtimestamp(max(self.index.leaf_end_ts[d] for d in self.new), tz=timezone.utc)
        ref = TimeReference(mode="replay", now=now, source="eval")
        w1, m1 = narrow_time_ids(self.index, self.new, _intent("yesterday"), reference=ref, scale=1.0)
        w2, m2 = narrow_time_ids(self.index, self.new, _intent("yesterday"), reference=ref, scale=2.0)
        self.assertEqual(w1, w2)
        self.assertFalse(m2.get("time_window_scale_applied"))

    def test_absolute_between_and_invalid(self) -> None:
        base = datetime(2026, 9, 1, tzinfo=timezone.utc)
        frm, to = (base + timedelta(hours=10)).isoformat(), (base + timedelta(hours=20)).isoformat()
        window, meta = narrow_time_ids(self.index, None, _intent("between", frm, to))
        self.assertTrue(window <= self.new)
        self.assertGreater(len(window), 0); self.assertLess(len(window), len(self.new))
        same, meta2 = narrow_time_ids(self.index, self.new, _intent("before", "not-a-date"))
        self.assertEqual(same, self.new); self.assertTrue(meta2["time_window_invalid"])

    def test_recent_v1_path_unchanged_and_any_passthrough(self) -> None:
        w1, m1 = narrow_time_ids(self.index, self.old, _intent("recent", recent=True))
        self.assertTrue(m1["recent_window_applied"]); self.assertTrue(w1 <= self.old)
        w2, m2 = narrow_time_ids(self.index, self.old, _intent("any"))
        self.assertEqual(w2, self.old); self.assertFalse(m2["time_window_applied"])
        w3, _ = narrow_time_ids(self.index, self.old, {"recent": True, "targets": []})  # v1 intent, no time
        self.assertTrue(w3 <= self.old)

    def test_retrieve_without_reference_does_not_invent_now(self) -> None:
        q = _unit(4)
        trace: dict = {}
        E.retrieve(self.index, _FakeClient(q), "어제 무슨 얘기", top_k=5, q_vec=q, ranking="hybrid", trace=trace,
                   source_room_id=ROOM_NEW, intent=_intent("yesterday"))
        self.assertTrue(trace.get("time_unresolved"))
        self.assertEqual(trace.get("time_kind"), "yesterday")
