from __future__ import annotations

from copy import deepcopy
from datetime import datetime, timedelta, timezone
import unittest

from adaptive_context import build_chunks, pack_context


def row(
    oid: str,
    room: str,
    content: str,
    *,
    actor: str = "A",
    account: str | None = "acct-a",
    block: str = "sample-1",
    when: datetime | None = None,
) -> dict:
    return {
        "observation_id": oid,
        "source_room_id": room,
        "source_room_original": room,
        "actor_account": account,
        "actor_label": actor,
        "content": content,
        "parsed_time": when.isoformat() if when else None,
        "sample_block": block,
    }


class AdaptiveContextTests(unittest.TestCase):
    def test_fixed_modes_preserve_room_block_and_tail(self) -> None:
        messages = [
            row("a0", "room-a", "one"),
            row("a1", "room-a", "two"),
            row("b0", "room-b", "three"),
            row("b1", "room-b", "four"),
            row("a2", "room-a", "new block", block="sample-2"),
        ]
        for mode in ("fixed20", "fixed8"):
            chunks = build_chunks(messages, mode)
            self.assertEqual(
                [oid for chunk in chunks for oid in chunk["all_observation_ids"]],
                [message["observation_id"] for message in messages],
            )
            for chunk in chunks:
                rows = [messages[index] for index in chunk["message_indices"]]
                self.assertEqual({m["source_room_id"] for m in rows}, {chunk["room"]})
                self.assertEqual({m["sample_block"] for m in rows}, {chunk["block"]})
        self.assertEqual([len(c["message_indices"]) for c in build_chunks(messages, "fixed8")], [2, 2, 1])

    def test_adaptive_idle_and_raw_message_cutoff(self) -> None:
        base = datetime(2026, 1, 1, tzinfo=timezone.utc)
        messages = [
            row(f"m{i}", "room-a", f"message-{i}", when=base + timedelta(seconds=i))
            for i in range(3)
        ]
        messages += [
            row("m3", "room-a", "after idle", when=base + timedelta(seconds=1000)),
            row("m4", "room-a", "tail", when=base + timedelta(seconds=1001)),
        ]
        chunks = build_chunks(messages, "adaptive", idle_seconds=300, target_chars=1000, max_chars=2000)
        self.assertEqual([c["message_indices"] for c in chunks], [[0, 1, 2], [3, 4]])

        many = [row(f"x{i}", "room-a", f"x-{i}", when=base + timedelta(seconds=i)) for i in range(5)]
        cut = build_chunks(many, "adaptive", idle_seconds=10_000, target_chars=1000, max_chars=2000, max_messages=2)
        self.assertEqual([c["message_indices"] for c in cut], [[0, 1], [2, 3], [4]])

    def test_adaptive_display_group_limit_does_not_split_same_actor_spam(self) -> None:
        messages = [
            row("plan", "room-a", "initial plan", actor="A", account="acct-a"),
            *[
                row(f"laugh-{i}", "room-a", "ㅋㅋ", actor="A", account="acct-a")
                for i in range(100)
            ],
            row("correction", "room-a", "아니, 방금 계획을 취소할게", actor="A", account="acct-a"),
        ]
        chunks = build_chunks(
            messages,
            "adaptive",
            target_chars=1000,
            max_chars=2000,
            max_messages=40,
            max_raw_messages=400,
        )
        self.assertEqual(len(chunks), 1)
        self.assertEqual(len(chunks[0]["message_indices"]), 102)
        self.assertEqual(len(chunks[0]["all_observation_ids"]), 102)
        self.assertIn("initial plan", chunks[0]["body"])
        self.assertIn("repeat x100", chunks[0]["body"])
        self.assertIn("취소", chunks[0]["body"])

    def test_adaptive_raw_safety_cap_still_bounds_uncompressed_input(self) -> None:
        messages = [row(f"raw-{i}", "room-a", f"turn-{i}") for i in range(401)]
        chunks = build_chunks(
            messages,
            "adaptive",
            target_chars=100_000,
            max_chars=200_000,
            max_messages=1000,
            max_raw_messages=400,
        )
        self.assertEqual([len(c["message_indices"]) for c in chunks], [400, 1])
        self.assertEqual(
            [oid for c in chunks for oid in c["all_observation_ids"]],
            [m["observation_id"] for m in messages],
        )

    def test_adaptive_compaction_keeps_provenance_and_short_or_correction_text(self) -> None:
        messages = [
            row("r0", "room-a", "spam", actor="A", account="acct-a"),
            row("r1", "room-a", "spam", actor="A", account="acct-a"),
            row("r2", "room-a", "ㅇㅇ", actor="A", account="acct-a"),
            row("r3", "room-a", "아니, 3시가 아니라 4시야", actor="A", account="acct-a"),
            row("r4", "room-a", "취소", actor="A", account="acct-a"),
            row("r5", "room-a", "spam", actor="B", account="acct-b"),
        ]
        chunks = build_chunks(messages, "adaptive", target_chars=1000, max_chars=2000)
        self.assertEqual(len(chunks), 1)
        chunk = chunks[0]
        self.assertEqual(chunk["all_observation_ids"], ["r0", "r1", "r2", "r3", "r4", "r5"])
        self.assertEqual(chunk["message_indices"], [0, 1, 2, 3, 4, 5])
        self.assertEqual(chunk["representative_indices"], [0, 2, 3, 4, 5])
        self.assertIn("spam", chunk["body"])
        self.assertIn("repeat x2", chunk["body"])
        self.assertIn("ㅇㅇ", chunk["body"])
        self.assertIn("아니, 3시가 아니라 4시야", chunk["body"])
        self.assertIn("취소", chunk["body"])
        self.assertIn("[5] B: spam", chunk["body"])

    def test_reactions_require_same_actor_and_question_mark_stays_visible(self) -> None:
        messages = [
            row("p0", "room-a", "ㅋ", actor="A", account="acct-a"),
            row("p1", "room-a", "ㅋㅋ", actor="B", account="acct-b"),
            row("p2", "room-a", "?", actor="A", account="acct-a"),
            row("p3", "room-a", "?", actor="A", account="acct-a"),
        ]
        chunks = build_chunks(messages, "adaptive", target_chars=1000, max_chars=2000)
        body = chunks[0]["body"]
        self.assertIn("[0] A: ㅋ", body)
        self.assertIn("[1] B: ㅋㅋ", body)
        self.assertIn("[2] A: ?", body)
        self.assertIn("[3] A: ?", body)
        self.assertNotIn("reaction burst", body)

        interrupted = [
            row("q0", "room-a", "ㅋㅋ", actor="A", account="acct-a"),
            row("q1", "room-a", "?", actor="A", account="acct-a"),
            row("q2", "room-a", "ㅋㅋ", actor="A", account="acct-a"),
        ]
        interrupted_body = build_chunks(
            interrupted, "adaptive", target_chars=1000, max_chars=2000
        )[0]["body"]
        self.assertIn("[0] A: ㅋㅋ", interrupted_body)
        self.assertIn("[1] A: ?", interrupted_body)
        self.assertIn("[2] A: ㅋㅋ", interrupted_body)
        self.assertNotIn("reaction burst", interrupted_body)

    def test_reaction_burst_and_huge_message_are_marked_without_truncation(self) -> None:
        huge = "붙여넣기" * 100
        messages = [
            row("p0", "room-a", "ㅋ", actor="A"),
            row("p1", "room-a", "ㅋㅋ", actor="A", account="acct-a"),
            row("p2", "room-a", huge, actor="A"),
            row("p3", "room-a", "tail", actor="A"),
        ]
        chunks = build_chunks(messages, "adaptive", target_chars=20, max_chars=40, max_messages=40)
        self.assertEqual([c["message_indices"] for c in chunks], [[0, 1], [2], [3]])
        self.assertIn("reaction burst x2", chunks[0]["body"])
        self.assertIn(huge, chunks[1]["body"])
        self.assertIn("oversized", chunks[1]["body"])
        self.assertEqual(chunks[1]["oversized_indices"], [2])

    def test_pack_context_respects_room_block_and_budget_visibility(self) -> None:
        messages = [
            row("a0", "room-a", "short-0"),
            row("a1", "room-a", "target-a"),
            row("b0", "room-b", "other-room"),
            row("a2", "room-a", "short-2"),
            row("c0", "room-a", "other-block", block="sample-2"),
        ]
        chunks = build_chunks(messages, "fixed8")
        packed = pack_context(messages, chunks, [chunks[0]["doc_id"]], budget_chars=45, expand_messages=4)
        self.assertNotIn("other-room", packed["text"])
        self.assertNotIn("other-block", packed["text"])
        self.assertEqual(packed["used_chars"], len(packed["text"]))
        self.assertEqual(
            packed["visible_observation_ids"],
            ["a0", "a1", "a2"],
        )
        self.assertLessEqual(packed["used_chars"], 45)

    def test_pack_context_does_not_count_invisible_or_truncated_rows(self) -> None:
        long = "L" * 100
        messages = [row("a0", "room-a", long), row("a1", "room-a", "visible")]
        chunks = build_chunks(messages, "fixed8")
        packed = pack_context(messages, chunks, [chunks[0]["doc_id"]], budget_chars=20, expand_messages=0)
        self.assertNotIn("a0", packed["visible_observation_ids"])
        self.assertNotIn(long, packed["text"])
        self.assertEqual(packed["visible_observation_ids"], ["a1"])
        self.assertEqual(packed["used_chars"], len(packed["text"]))

    def test_pack_context_keeps_adaptive_markers_and_only_representatives_visible(self) -> None:
        messages = [
            row("s0", "room-a", "same", actor="A", account="acct-a"),
            row("s1", "room-a", "same", actor="A", account="acct-a"),
            row("s2", "room-a", "target", actor="A", account="acct-a"),
        ]
        chunks = build_chunks(messages, "adaptive", target_chars=1000, max_chars=2000)
        packed = pack_context(messages, chunks, [chunks[0]["doc_id"]], budget_chars=200, expand_messages=0)
        self.assertIn("repeat x2", packed["text"])
        self.assertNotIn("[1] A: same", packed["text"])
        self.assertEqual(packed["visible_observation_ids"], ["s0", "s2"])

    def test_compaction_does_not_bridge_an_interleaved_source_row(self) -> None:
        messages = [
            row("a0", "room-a", "same", actor="A", account="acct-a"),
            row("b0", "room-b", "other", actor="B", account="acct-b"),
            row("a1", "room-a", "same", actor="A", account="acct-a"),
        ]
        chunks = build_chunks(messages, "adaptive", target_chars=1000, max_chars=2000)
        packed = pack_context(messages, chunks, [chunks[0]["doc_id"]], budget_chars=200, expand_messages=4)
        self.assertIn("[0] A: same", packed["text"])
        self.assertIn("[2] A: same", packed["text"])
        self.assertNotIn("repeat x2", packed["text"])

    def test_input_is_unchanged(self) -> None:
        messages = [row("a0", "room-a", "one"), row("a1", "room-a", "two")]
        original = deepcopy(messages)
        chunks = build_chunks(messages, "adaptive")
        pack_context(messages, chunks, [chunks[0]["doc_id"]])
        self.assertEqual(messages, original)

    def test_idle_time_does_not_assume_timezone_for_mixed_values(self) -> None:
        naive = datetime(2026, 1, 1, 12, 0, 0)
        aware = naive.replace(tzinfo=timezone.utc) + timedelta(seconds=1000)
        messages = [
            row("t0", "room-a", "first", when=naive),
            row("t1", "room-a", "mixed", when=aware),
        ]
        chunks = build_chunks(messages, "adaptive", idle_seconds=300, target_chars=1000, max_chars=2000)
        self.assertEqual([c["message_indices"] for c in chunks], [[0, 1]])

        local_later = naive + timedelta(seconds=1000)
        messages[1]["parsed_time"] = local_later.isoformat()
        chunks = build_chunks(messages, "adaptive", idle_seconds=300, target_chars=1000, max_chars=2000)
        self.assertEqual([c["message_indices"] for c in chunks], [[0], [1]])

        # Aware values are compared in UTC; these represent the same instant.
        messages[0]["parsed_time"] = "2026-01-01T12:00:00+09:00"
        messages[1]["parsed_time"] = "2026-01-01T03:00:00+00:00"
        chunks = build_chunks(messages, "adaptive", idle_seconds=300, target_chars=1000, max_chars=2000)
        self.assertEqual([c["message_indices"] for c in chunks], [[0, 1]])


if __name__ == "__main__":
    unittest.main()
