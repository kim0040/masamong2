from __future__ import annotations

import unittest

from incremental_leaves import ChatEvent, IncrementalLeafBuilder, kakao_native_keys


def _creates(room: str, times: list[float], prefix: str = "m") -> list[ChatEvent]:
    return [ChatEvent("create", room, f"{prefix}{i}", t, "u", f"text {i}") for i, t in enumerate(times)]


class BoundaryTests(unittest.TestCase):
    def test_size_close(self) -> None:
        b = IncrementalLeafBuilder(size=3).apply_all(_creates("r", [0, 1, 2, 3, 4]))
        self.assertEqual(b.boundaries()["r"], [("m0", "m1", "m2"), ("m3", "m4")])
        self.assertEqual(b.stats["closed_size"], 1)

    def test_idle_gap_close_at_next_message(self) -> None:
        b = IncrementalLeafBuilder(size=8, idle_gap_s=3600).apply_all(_creates("r", [0, 10, 20, 5000, 5010]))
        self.assertEqual(b.boundaries()["r"], [("m0", "m1", "m2"), ("m3", "m4")])

    def test_tick_close_equals_gap_close(self) -> None:
        events = _creates("r", [0, 10, 20, 5000, 5010])
        live = IncrementalLeafBuilder(size=8, idle_gap_s=3600)
        for ev in events:
            live.tick(ev.ts)  # wall-clock ticks between messages
            live.apply(ev)
        batch = IncrementalLeafBuilder(size=8, idle_gap_s=3600).apply_all(events)
        self.assertEqual(live.boundaries(), batch.boundaries())

    def test_rooms_are_independent(self) -> None:
        ev = _creates("a", [0, 1]) + _creates("b", [2], prefix="n") + _creates("a", [3], prefix="z")
        b = IncrementalLeafBuilder(size=2).apply_all(ev)
        self.assertEqual(b.boundaries()["a"], [("m0", "m1"), ("z0",)])
        self.assertEqual(b.boundaries()["b"], [("n0",)])


class RevisionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.b = IncrementalLeafBuilder(size=2).apply_all(_creates("r", [0, 1, 2, 3]))
        self.b.mark_embedded(self.b.pending_embed_jobs())

    def test_edit_dirties_exactly_one_leaf(self) -> None:
        self.b.apply(ChatEvent("edit", "r", "m2", 9, "u", "changed"))
        self.assertEqual(len(self.b.pending_embed_jobs()), 1)
        self.assertIn("changed", self.b.leaf_text(self.b.pending_embed_jobs()[0]))

    def test_delete_is_tombstone_and_keeps_boundaries(self) -> None:
        before = self.b.boundaries()
        self.b.apply(ChatEvent("delete", "r", "m1", 9))
        self.assertEqual(self.b.boundaries(), before)
        texts = " ".join(d["text"] for d in self.b.searchable_docs())
        self.assertNotIn("text 1", texts)
        self.assertEqual(len(self.b.pending_embed_jobs()), 1)

    def test_reimport_same_content_is_noop_and_changed_is_edit(self) -> None:
        self.b.apply(ChatEvent("create", "r", "m0", 0, "u", "text 0"))
        self.assertEqual(self.b.pending_embed_jobs(), [])
        self.b.apply(ChatEvent("create", "r", "m0", 0, "u", "text 0 v2"))
        self.assertEqual(len(self.b.pending_embed_jobs()), 1)

    def test_open_leaf_is_hot_tail_until_embedded(self) -> None:
        self.b.apply(ChatEvent("create", "r", "m4", 5, "u", "fresh"))
        modes = {d["leaf_id"]: d["mode"] for d in self.b.searchable_docs()}
        self.assertIn("lexical_hot", modes.values())

    def test_as_of_hides_future_messages(self) -> None:
        docs = list(self.b.searchable_docs(as_of=1.5))
        self.assertTrue(all(d["max_ts"] <= 1.5 for d in docs))
        self.assertNotIn("text 2", " ".join(d["text"] for d in docs))


class KakaoKeyTests(unittest.TestCase):
    def test_overlapping_reexport_same_keys(self) -> None:
        rows = [{"source_room_id": "k", "parsed_time": f"2026-01-01T00:0{i // 2}", "actor_label": "a",
                 "content": "ㅋㅋ" if i % 2 else f"msg{i}"} for i in range(10)]
        full = kakao_native_keys(rows)
        self.assertEqual(len(set(full)), len(full))
        # a later export that starts mid-way must reproduce the same keys for the shared rows only
        # when the first shared minute is complete — adapters cut exports at minute boundaries.
        self.assertEqual(kakao_native_keys(rows[4:]), full[4:])

    def test_repeated_identical_lines_stay_distinct(self) -> None:
        rows = [{"source_room_id": "k", "parsed_time": "2026-01-01T00:00", "actor_label": "a", "content": "ㅋㅋ"}] * 3
        self.assertEqual(len(set(kakao_native_keys(rows))), 3)


if __name__ == "__main__":
    unittest.main()
