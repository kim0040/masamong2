"""Matched 원문 우선, span offset과 방 범위를 검증한다."""

from __future__ import annotations

import socket
import unittest
from unittest import mock

import evidence_packing as ep
from segment import expand_segment, order_messages_by_room


def _row(i: int, room: str, content: str, *, original: str | None = None, label: str = "a", ts: str | None = "") -> dict:
    return {
        "observation_id": f"{room}-{i}",
        "source_room_id": room,
        "source_room_original": original or room,
        "actor_label": label,
        "actor_account": "acct",
        "content": content,
        "parsed_time": f"2026-01-01T00:{i:02d}:00" if ts == "" else ts,
    }


def _stream(n: int, room: str = "r", size: int = 20) -> list[dict]:
    return order_messages_by_room([_row(i, room, f"{i:02d}" + "x" * (size - 2)) for i in range(n)])


def _leaf(messages: list[dict], start: int, end: int) -> dict:
    rows = messages[start:end]
    return {
        "doc_id": f"leaf:{start}-{end - 1}",
        "kind": "leaf",
        "start": start,
        "end": end,
        "source_room_id": rows[0]["source_room_id"],
        "observation_ids": [r["observation_id"] for r in rows],
        "speakers": sorted({r["actor_label"] for r in rows}),
    }


def _no_network(*_a, **_k):  # pragma: no cover - guard
    raise AssertionError("unexpected network call in offline test")


class _Base(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.object(socket, "socket", side_effect=_no_network)
        patcher.start()
        self.addCleanup(patcher.stop)

    def assertSpansMatch(self, messages, packed) -> None:
        self.assertEqual(ep.verify_visible_spans(messages, packed), [])
        self.assertLessEqual(len(packed["excerpt"]), packed["char_limit"])
        self.assertEqual(
            packed["visible_observation_ids"], [s["observation_id"] for s in packed["visible_spans"]]
        )


class MatchedFirstTests(_Base):
    def test_long_before_message_does_not_evict_target(self):
        messages = order_messages_by_room(
            [_row(0, "r", "p" * 5000), _row(1, "r", "target-line " + "t" * 40), _row(2, "r", "after " + "n" * 30)]
        )
        leaf = _leaf(messages, 1, 2)
        packed = ep.pack_segment_evidence(messages, leaf, before=12, after=12, with_prefix=True, char_limit=4000)
        self.assertEqual(packed["status"], "complete")
        self.assertEqual(packed["matched_observation_ids"], ["r-1"])
        self.assertIn("r-1", packed["visible_observation_ids"])
        spans = {s["observation_id"]: s for s in packed["visible_spans"]}
        self.assertTrue(spans["r-1"]["complete"])
        self.assertEqual(spans["r-1"]["char_end"], len(messages[1]["content"]))
        # 앞 5,000자 메시지는 뒷부분만 부분 포함되고 그 사실이 span에 드러난다
        self.assertIn("r-0", spans)
        self.assertFalse(spans["r-0"]["complete"])
        self.assertGreater(spans["r-0"]["char_start"], 0)
        self.assertEqual(spans["r-0"]["char_end"], 5000)
        self.assertIn(ep.HEAD_CUT_MARKER.strip(), packed["excerpt"])
        self.assertFalse(packed["context_complete"])
        self.assertSpansMatch(messages, packed)
        # legacy 동작이라면 target이 사라졌을 것이다
        legacy = ep._legacy_matched_loss(messages, leaf, before=12, after=12, with_prefix=True, char_limit=4000)
        self.assertEqual(legacy["matched_lost"], 1)

    def test_output_keeps_original_order_and_marks_elision(self):
        messages = _stream(12)
        leaf = _leaf(messages, 5, 7)
        packed = ep.pack_segment_evidence(messages, leaf, before=4, after=4, with_prefix=False, char_limit=140)
        self.assertEqual(packed["status"], "complete")
        idx = {oid: i for i, oid in enumerate(packed["visible_observation_ids"])}
        self.assertEqual(sorted(idx, key=idx.get), sorted(idx, key=lambda o: int(o.split("-")[1])))
        self.assertIn("생략", packed["excerpt"])
        self.assertTrue(set(packed["omitted_observation_ids"]) <= set(packed["observation_ids"]))
        self.assertSpansMatch(messages, packed)

    def test_prefix_counts_toward_budget(self):
        messages = _stream(6)
        leaf = _leaf(messages, 2, 3)
        with_p = ep.pack_segment_evidence(messages, leaf, before=1, after=1, with_prefix=True, char_limit=60)
        without = ep.pack_segment_evidence(messages, leaf, before=1, after=1, with_prefix=False, char_limit=60)
        self.assertGreater(with_p["prefix_chars"], 0)
        self.assertEqual(with_p["used_chars"], len(with_p["excerpt"]))
        self.assertLessEqual(with_p["used_chars"], 60)
        self.assertGreaterEqual(len(without["visible_observation_ids"]), len(with_p["visible_observation_ids"]))
        self.assertSpansMatch(messages, with_p)

    def test_prefix_alone_near_limit_is_insufficient_not_silent(self):
        messages = order_messages_by_room([_row(i, "discord:1:123456", "hello " * 5, label=f"speaker{i}") for i in range(3)])
        leaf = _leaf(messages, 1, 2)
        packed = ep.pack_segment_evidence(messages, leaf, before=1, after=1, with_prefix=True, char_limit=30)
        self.assertEqual(packed["status"], "insufficient")
        self.assertEqual(packed["insufficient_reason"], "prefix_exhausts_budget")
        self.assertEqual(packed["excerpt"], "")
        self.assertEqual(packed["visible_spans"], [])
        # 후보 집합은 남는다: ID만 보고 보였다고 채점하면 안 된다
        self.assertIn("discord:1:123456-1", packed["observation_ids"])


class LongMessageTests(_Base):
    def test_long_matched_message_partial_with_exact_offsets(self):
        content = "".join(f"{i % 10}" for i in range(6000))
        messages = order_messages_by_room([_row(0, "r", "c" * 10), _row(1, "r", content), _row(2, "r", "d" * 10)])
        leaf = _leaf(messages, 1, 2)
        packed = ep.pack_segment_evidence(messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000)
        self.assertEqual(packed["status"], "partial")
        self.assertIsNone(packed["insufficient_reason"])
        spans = packed["visible_spans"]
        self.assertEqual(len(spans), 1)
        span = spans[0]
        self.assertEqual(span["observation_id"], "r-1")
        self.assertFalse(span["complete"])
        self.assertEqual(span["char_start"], 0)
        self.assertLess(span["char_end"], 6000)
        self.assertEqual(span["content_len"], 6000)
        self.assertEqual(packed["excerpt"][span["excerpt_start"] : span["excerpt_end"]], content[: span["char_end"]])
        self.assertTrue(packed["excerpt"].endswith(ep.TAIL_CUT_MARKER))
        self.assertEqual(packed["matched_counts"]["visible_complete"], 0)
        self.assertSpansMatch(messages, packed)

    def test_required_observation_must_be_fully_visible(self):
        messages = order_messages_by_room([_row(0, "r", "q" * 300), _row(1, "r", "z" * 5)])
        leaf = _leaf(messages, 0, 1)
        packed = ep.pack_segment_evidence(
            messages, leaf, before=0, after=1, with_prefix=False, char_limit=100, required_observation_ids=["r-0"]
        )
        self.assertEqual(packed["status"], "insufficient")
        self.assertEqual(packed["insufficient_reason"], "required_not_visible")
        # 부분 본문은 여전히 정확한 offset으로 보고된다
        self.assertSpansMatch(messages, packed)
        self.assertFalse(packed["visible_spans"][0]["complete"])

    def test_per_message_cap_marks_partial(self):
        messages = _stream(3, size=50)
        leaf = _leaf(messages, 1, 2)
        packed = ep.pack_segment_evidence(
            messages, leaf, before=1, after=1, with_prefix=False, char_limit=1000, per_message_cap=10
        )
        self.assertEqual(packed["status"], "partial")
        for span in packed["visible_spans"]:
            self.assertEqual(span["char_end"] - span["char_start"], 10)
            self.assertFalse(span["complete"])
        self.assertSpansMatch(messages, packed)

    def test_matched_omitted_in_middle_is_marked(self):
        messages = order_messages_by_room([_row(0, "r", "a" * 20), _row(1, "r", "b" * 500), _row(2, "r", "c" * 20)])
        leaf = _leaf(messages, 0, 3)
        packed = ep.pack_segment_evidence(messages, leaf, before=0, after=0, with_prefix=False, char_limit=60)
        self.assertEqual(packed["status"], "partial")
        self.assertIn("r-1", packed["omitted_observation_ids"])
        self.assertIn(ep.ELIDED_MIDDLE.format(n=1), packed["excerpt"])
        self.assertEqual(packed["visible_observation_ids"], ["r-0", "r-2"])
        self.assertSpansMatch(messages, packed)


class ScopeTests(_Base):
    def _two_room_stream(self):
        return order_messages_by_room([_row(i, "room-a") if False else _row(i, "room-a", f"a{i:02d}" + "x" * 10) for i in range(6)]
                                      + [_row(i, "room-b", f"b{i:02d}" + "y" * 10) for i in range(6)])

    def test_parent_never_crosses_logical_room(self):
        messages = self._two_room_stream()
        leaf = _leaf(messages, 4, 6)  # room-a 마지막 두 개
        packed = ep.pack_segment_evidence(messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000)
        rooms = {messages[i]["source_room_id"] for i in range(packed["start"], packed["end"])}
        self.assertEqual(rooms, {"room-a"})
        self.assertTrue(all(o.startswith("room-a") for o in packed["observation_ids"]))
        self.assertSpansMatch(messages, packed)

    def test_original_room_restriction_on_merged_logical_room(self):
        rows = [_row(i, "friends-archive", f"o{i:02d}" + "x" * 10, original="old" if i < 3 else "new") for i in range(6)]
        messages = order_messages_by_room(rows)
        leaf = _leaf(messages, 3, 4)
        packed = ep.pack_segment_evidence(
            messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000, allowed_rooms={"new"}
        )
        self.assertEqual(packed["status"], "complete")
        self.assertEqual(set(packed["excluded"]["room"]), {"friends-archive-0", "friends-archive-1", "friends-archive-2"})
        self.assertNotIn("friends-archive-0", packed["observation_ids"])
        self.assertNotIn("o00", packed["excerpt"])
        self.assertTrue(packed["scope_applied"]["rooms"])
        self.assertSpansMatch(messages, packed)

    def test_allowed_set_excludes_context_but_none_means_no_restriction(self):
        messages = _stream(8)
        leaf = _leaf(messages, 3, 4)
        allowed = {"r-2", "r-3", "r-4"}
        packed = ep.pack_segment_evidence(
            messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000, allowed_observation_ids=allowed
        )
        self.assertEqual(set(packed["observation_ids"]), allowed)
        self.assertEqual(set(packed["visible_observation_ids"]), allowed)
        self.assertEqual(len(packed["excluded"]["allowed"]), 5)
        unrestricted = ep.pack_segment_evidence(messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000)
        self.assertEqual(len(unrestricted["observation_ids"]), 8)
        self.assertFalse(unrestricted["scope_applied"]["observation_ids"])
        self.assertSpansMatch(messages, packed)

    def test_empty_allowed_set_is_insufficient(self):
        messages = _stream(5)
        leaf = _leaf(messages, 2, 3)
        packed = ep.pack_segment_evidence(
            messages, leaf, before=1, after=1, with_prefix=False, char_limit=4000, allowed_observation_ids=set()
        )
        self.assertEqual(packed["status"], "insufficient")
        self.assertEqual(packed["insufficient_reason"], "empty_allowed_set")
        self.assertEqual(packed["excerpt"], "")
        self.assertEqual(packed["observation_ids"], [])
        self.assertEqual(packed["matched_excluded_observation_ids"], ["r-2"])
        self.assertTrue(packed["scope_applied"]["observation_ids"])

    def test_hard_time_bound_applies_to_parent_and_matched(self):
        messages = _stream(8)  # parsed_time 00:00 .. 00:07
        leaf = _leaf(messages, 3, 5)

        def ts_of(row):
            return float(str(row["parsed_time"])[14:16]) * 60.0

        allows = ep.make_time_filter(3 * 60.0, 6 * 60.0, ts_of=ts_of)  # [00:03, 00:06)
        packed = ep.pack_segment_evidence(
            messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000, time_allows=allows
        )
        self.assertEqual(set(packed["observation_ids"]), {"r-3", "r-4", "r-5"})
        self.assertEqual(set(packed["excluded"]["time"]), {"r-0", "r-1", "r-2", "r-6", "r-7"})
        self.assertEqual(packed["status"], "complete")
        # matched 자체가 범위 밖이면 insufficient
        outside = ep.make_time_filter(6 * 60.0, None, ts_of=ts_of)
        packed2 = ep.pack_segment_evidence(
            messages, leaf, before=12, after=12, with_prefix=False, char_limit=4000, time_allows=outside
        )
        self.assertEqual(packed2["status"], "insufficient")
        self.assertEqual(packed2["insufficient_reason"], "matched_out_of_scope")
        self.assertEqual(packed2["excerpt"], "")
        self.assertEqual(sorted(packed2["matched_excluded_observation_ids"]), ["r-3", "r-4"])

    def test_unknown_time_excluded_by_default(self):
        messages = order_messages_by_room([_row(0, "r", "k" * 10, ts=None), _row(1, "r", "m" * 10)])
        leaf = _leaf(messages, 1, 2)
        allows = ep.make_time_filter(None, None, ts_of=lambda r: None if not r["parsed_time"] else 1.0)
        packed = ep.pack_segment_evidence(messages, leaf, before=1, after=0, with_prefix=False, char_limit=4000, time_allows=allows)
        self.assertEqual(packed["excluded"]["time"], ["r-0"])
        inclusive = ep.make_time_filter(None, None, ts_of=lambda r: None, unknown_time="include")
        packed2 = ep.pack_segment_evidence(messages, leaf, before=1, after=0, with_prefix=False, char_limit=4000, time_allows=inclusive)
        self.assertEqual(packed2["excluded"]["time"], [])
        with self.assertRaises(ValueError):
            ep.make_time_filter(None, None, ts_of=lambda r: None, unknown_time="maybe")


class MergedSpanTests(_Base):
    def test_merged_matched_span_counts_whole_span(self):
        messages = _stream(12)
        base = _leaf(messages, 2, 4)
        merged = dict(base)
        merged["start"], merged["end"] = 2, 8  # retrieval_post.merged_span 처럼 observation_ids는 base 그대로
        packed = ep.pack_segment_evidence(messages, merged, before=2, after=2, with_prefix=False, char_limit=4000)
        self.assertEqual(packed["matched_observation_ids"], [f"r-{i}" for i in range(2, 8)])
        self.assertEqual(packed["leaf_observation_ids"], ["r-2", "r-3"])
        self.assertEqual(packed["matched_span"], [2, 8])
        self.assertTrue(set(packed["leaf_observation_ids"]) <= set(packed["observation_ids"]))
        self.assertSpansMatch(messages, packed)


class ContractTests(_Base):
    def test_truncated_target_cannot_be_scored_by_id_alone(self):
        messages = order_messages_by_room([_row(0, "r", "w" * 200), _row(1, "r", "target " + "v" * 300)])
        leaf = _leaf(messages, 0, 2)
        packed = ep.pack_segment_evidence(messages, leaf, before=0, after=0, with_prefix=False, char_limit=150)
        self.assertIn("r-1", packed["observation_ids"])  # 후보 집합에는 있다
        self.assertIn("r-1", packed["leaf_observation_ids"])
        # 하지만 visible 계약은 잘림/누락을 드러낸다
        span = {s["observation_id"]: s for s in packed["visible_spans"]}.get("r-1")
        self.assertTrue(span is None or not span["complete"])
        self.assertNotEqual(packed["status"], "complete")

    def test_expand_segment_keeps_legacy_keys_and_adds_contract(self):
        messages = _stream(6)
        leaf = _leaf(messages, 2, 3)
        out = expand_segment(messages, leaf, before=1, after=1, with_prefix=True, char_limit=4000)
        for key in ("doc_id", "kind", "matched_leaf", "start", "end", "source_room_id", "observation_ids", "speakers", "excerpt", "leaf_observation_ids"):
            self.assertIn(key, out)
        self.assertEqual(out["kind"], "parent_expand")
        self.assertEqual(out["observation_ids"], ["r-1", "r-2", "r-3"])
        self.assertTrue(set(out["leaf_observation_ids"]) <= set(out["observation_ids"]))
        self.assertIn("visible_spans", out)
        self.assertEqual(out["status"], "complete")
        self.assertSpansMatch(messages, out)

    def test_verify_detects_tampered_excerpt(self):
        messages = _stream(3)
        leaf = _leaf(messages, 1, 2)
        packed = ep.pack_segment_evidence(messages, leaf, before=1, after=1, with_prefix=False, char_limit=4000)
        broken = dict(packed)
        broken["excerpt"] = packed["excerpt"].replace("01x", "01Q", 1)
        self.assertEqual(ep.verify_visible_spans(messages, broken), ["r-1"])

    def test_zero_char_limit(self):
        messages = _stream(3)
        packed = ep.pack_segment_evidence(messages, _leaf(messages, 1, 2), before=1, after=1, with_prefix=False, char_limit=0)
        self.assertEqual(packed["status"], "insufficient")
        self.assertEqual(packed["insufficient_reason"], "char_limit")
        self.assertEqual(packed["excerpt"], "")


class PackSetTests(_Base):
    def test_overlapping_parents_do_not_spend_budget_twice(self):
        messages = _stream(20)
        leaves = [_leaf(messages, 6, 8), _leaf(messages, 8, 10)]
        result = ep.pack_evidence_set(messages, leaves, before=2, after=2, with_prefix=False, doc_char_limit=4000)
        first, second = result["docs"]
        self.assertEqual(first["status"], "complete")
        self.assertEqual(second["status"], "complete")
        self.assertEqual(set(first["visible_observation_ids"]) & set(second["visible_observation_ids"]), set())
        self.assertTrue(set(second["excluded"]["already_visible"]) <= set(first["visible_observation_ids"]))
        self.assertEqual(second["covered_by"], [first["doc_id"]])
        ids = result["visible_observation_ids"]
        self.assertEqual(len(ids), len(set(ids)))
        # 후보 집합(observation_ids)에는 겹친 원문이 그대로 남는다 (candidate hit용)
        self.assertIn("r-8", first["observation_ids"])
        self.assertIn("r-8", second["observation_ids"])
        self.assertEqual(result["used_chars"], first["used_chars"] + second["used_chars"])

    def test_fully_covered_doc_is_duplicate(self):
        messages = _stream(20)
        leaves = [_leaf(messages, 6, 10), _leaf(messages, 7, 9)]
        result = ep.pack_evidence_set(messages, leaves, before=1, after=1, with_prefix=False, doc_char_limit=4000)
        self.assertEqual(result["docs"][1]["status"], "duplicate")
        self.assertEqual(result["docs"][1]["insufficient_reason"], "matched_already_visible")
        self.assertEqual(result["docs"][1]["excerpt"], "")
        self.assertEqual(result["duplicate_doc_ids"], ["leaf:7-8"])
        self.assertEqual(result["docs"][1]["covered_by"], ["leaf:6-9"])

    def test_dedupe_can_be_disabled(self):
        messages = _stream(20)
        leaves = [_leaf(messages, 6, 8), _leaf(messages, 8, 10)]
        result = ep.pack_evidence_set(
            messages, leaves, before=2, after=2, with_prefix=False, doc_char_limit=4000, dedupe_visible=False
        )
        self.assertTrue(set(result["docs"][0]["visible_observation_ids"]) & set(result["docs"][1]["visible_observation_ids"]))

    def test_total_cap_and_per_doc_cap_are_distinct(self):
        messages = _stream(60)
        leaves = [_leaf(messages, 5, 7), _leaf(messages, 25, 27), _leaf(messages, 45, 47)]
        result = ep.pack_evidence_set(
            messages, leaves, before=2, after=2, with_prefix=False, doc_char_limit=200, total_char_limit=330, doc_overhead_chars=10
        )
        statuses = [d["status"] for d in result["docs"]]
        self.assertEqual(statuses[0], "complete")
        self.assertIn(statuses[1], {"complete", "partial"})
        self.assertLessEqual(result["docs"][0]["used_chars"], 200)
        self.assertEqual(statuses[2], "omitted")
        self.assertEqual(result["omitted_doc_ids"], ["leaf:45-46"])
        self.assertLessEqual(result["used_chars"], 330)
        for d in result["docs"][:2]:
            self.assertSpansMatch(messages, d)
        flat_docs = {s["doc_id"] for s in result["visible_spans"]}
        self.assertEqual(flat_docs, {"leaf:5-6", "leaf:25-26"})

    def test_scope_applies_to_every_doc(self):
        messages = _stream(20)
        leaves = [_leaf(messages, 6, 8), _leaf(messages, 14, 16)]
        result = ep.pack_evidence_set(
            messages, leaves, before=3, after=3, with_prefix=False, doc_char_limit=4000,
            allowed_observation_ids={"r-6", "r-7", "r-15", "r-16"},
        )
        self.assertEqual(result["docs"][0]["visible_observation_ids"], ["r-6", "r-7"])
        self.assertEqual(result["docs"][1]["visible_observation_ids"], ["r-15", "r-16"])
        self.assertEqual(result["docs"][1]["matched_excluded_observation_ids"], ["r-14"])
        self.assertEqual(result["docs"][1]["status"], "complete")


class AuditTests(_Base):
    def test_audit_aggregates_only(self):
        messages = order_messages_by_room(
            [_row(0, "r", "p" * 5000)] + [_row(i, "r", f"{i:02d}" + "x" * 10) for i in range(1, 16)]
        )
        leaves = [_leaf(messages, i, min(i + 4, len(messages))) for i in range(0, len(messages), 4)]
        report = ep.audit_expansion(messages, leaves, before=12, after=12, with_prefix=True, char_limit=4000)
        self.assertEqual(report["leaf_count"], len(leaves))
        self.assertGreaterEqual(report["legacy_body_truncation"]["any_matched_partial_or_lost"], 1)
        self.assertEqual(report["evidence_packing"]["span_verify_failures"], 0)
        self.assertEqual(report["evidence_packing"]["over_limit"], 0)
        text = str(report)
        self.assertNotIn("xxxxx", text)
        self.assertNotIn("r-1", text)


if __name__ == "__main__":
    unittest.main()
