"""Discord rooms must not share a leaf or parent expand window."""

from __future__ import annotations

import unittest

from segment import expand_segment, order_messages_by_room, slice_segments_per_room


def _msg(i: int, room: str) -> dict:
    return {
        "observation_id": f"{room}-{i}",
        "source_room_id": room,
        "source_room_original": room,
        "actor_label": "a",
        "actor_account": "acct",
        "content": f"m{i}",
        "parsed_time": f"2026-01-01T00:{i:02d}:00",
    }


class RoomSliceTests(unittest.TestCase):
    def test_leaf_does_not_cross_rooms(self):
        messages = order_messages_by_room(
            [_msg(i, "discord:1:aaa") for i in range(5)]
            + [_msg(i, "discord:2:bbb") for i in range(5)]
        )
        leaves = slice_segments_per_room(
            messages, size=4, overlap=0, limit=20, kind="leaf", with_prefix=False
        )
        self.assertTrue(leaves)
        for leaf in leaves:
            rooms = {messages[j]["source_room_id"] for j in range(leaf["start"], leaf["end"])}
            self.assertEqual(len(rooms), 1, leaf)

    def test_parent_expand_stays_in_room(self):
        messages = order_messages_by_room(
            [_msg(i, "room-a") for i in range(8)] + [_msg(i, "room-b") for i in range(8)]
        )
        leaves = slice_segments_per_room(
            messages, size=4, overlap=0, limit=20, kind="leaf", with_prefix=False
        )
        last_a = [x for x in leaves if x["source_room_id"] == "room-a"][-1]
        expanded = expand_segment(
            messages, last_a, before=12, after=12, with_prefix=False, char_limit=4000
        )
        rooms = {messages[j]["source_room_id"] for j in range(expanded["start"], expanded["end"])}
        self.assertEqual(rooms, {"room-a"})


class TimeGapSliceTests(unittest.TestCase):
    def test_gap_breaks_leaf_inside_room(self):
        from segment import slice_segments_time_aware, order_messages_by_room

        rows = []
        for i in range(6):
            minute = i if i < 3 else i + 40
            rows.append(
                {
                    "observation_id": f"r-{i}",
                    "source_room_id": "room-a",
                    "source_room_original": "room-a",
                    "actor_label": "a",
                    "actor_account": "acct",
                    "content": f"m{i}",
                    "parsed_time": f"2026-01-01T00:{minute:02d}:00",
                }
            )
        messages = order_messages_by_room(rows)
        segs = slice_segments_time_aware(
            messages, max_size=8, gap_minutes=30, min_size=2, limit=20, kind="leaf", with_prefix=False
        )
        self.assertGreaterEqual(len(segs), 2)
        self.assertLessEqual(segs[0]["n"], 3)
        rooms = {messages[j]["source_room_id"] for s in segs for j in range(s["start"], s["end"])}
        self.assertEqual(rooms, {"room-a"})

    def test_per_room_time_gap_flag(self):
        from segment import slice_segments_per_room, order_messages_by_room

        messages = order_messages_by_room(
            [_msg(i, "discord:1:aaa") for i in range(5)]
            + [_msg(i, "discord:2:bbb") for i in range(5)]
        )
        leaves = slice_segments_per_room(
            messages,
            size=8,
            overlap=0,
            limit=20,
            kind="leaf",
            with_prefix=False,
            split="time_gap",
            gap_minutes=1,
        )
        for leaf in leaves:
            rooms = {messages[j]["source_room_id"] for j in range(leaf["start"], leaf["end"])}
            self.assertEqual(len(rooms), 1)
            self.assertTrue(str(leaf["doc_id"]).startswith("leaf:"))


if __name__ == "__main__":
    unittest.main()
