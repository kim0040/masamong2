"""collect_messages keeps one rule for who is a candidate and three ways to gather them (stream, sorted, earliest N)."""

from __future__ import annotations

import unittest
from dataclasses import dataclass

import segment
from segment import collect_messages


@dataclass
class _Obs:
    observation_id: str
    source_room_id: str
    content: str
    parsed_time: str | None
    semantic_eligible: bool = True
    actor_label: str = "a"
    actor_account: str = "acct"


def _stream() -> list[_Obs]:
    rows = [
        _Obs("o1", "room-b", "둘째 방 첫 글", "2026-01-01T00:05:00"),
        _Obs("o2", "room-a", "첫 방 첫 글", "2026-01-01T00:09:00"),
        _Obs("o3", "room-a", "   ", "2026-01-01T00:01:00"),  # blank body: never a candidate
        _Obs("o4", "room-a", "삭제 표시", "2026-01-01T00:02:00", semantic_eligible=False),  # not human text
        _Obs("o5", "room-c", "셋째 방", "2026-01-01T00:03:00"),
        _Obs("o6", "room-a", "첫 방 둘째 글", "2026-01-01T00:04:00"),
        _Obs("o7", "room-b", "둘째 방 둘째 글", "2026-01-01T00:04:00"),
    ]
    return rows


def _ids(rows: list[dict]) -> list[str]:
    return [row["observation_id"] for row in rows]


@unittest.skipUnless(hasattr(segment, "is_candidate"), "pre-refactor")
class IsCandidateTests(unittest.TestCase):
    def test_human_text_with_a_body_only(self) -> None:
        self.assertTrue(segment.is_candidate(_Obs("x", "r", "글", "t")))
        self.assertFalse(segment.is_candidate(_Obs("x", "r", " \n ", "t")))
        self.assertFalse(segment.is_candidate(_Obs("x", "r", "글", "t", semantic_eligible=False)))
        self.assertFalse(segment.is_candidate(_Obs("x", "r", None, "t")))  # type: ignore[arg-type]


class CollectMessagesTests(unittest.TestCase):
    def test_without_a_merge_the_first_n_of_the_stream_are_kept_then_grouped_by_room(self) -> None:
        self.assertEqual(_ids(collect_messages(_stream(), limit=0)), ["o6", "o2", "o7", "o1", "o5"])
        self.assertEqual(_ids(collect_messages(_stream(), limit=3)), ["o2", "o1", "o5"])  # o1, o2, o5 came first

    def test_the_stream_is_not_read_past_the_limit(self) -> None:
        seen: list[str] = []

        def gen():
            for obs in _stream():
                seen.append(obs.observation_id)
                yield obs

        collect_messages(gen(), limit=2)
        self.assertEqual(seen, ["o1", "o2"])

    def test_a_merge_joins_rooms_and_keeps_the_original_room(self) -> None:
        rows = collect_messages(_stream(), limit=0, room_merge={"room-b": "room-a"})
        self.assertEqual({row["source_room_id"] for row in rows}, {"room-a", "room-c"})
        merged = [row for row in rows if row["source_room_id"] == "room-a"]
        self.assertEqual([row["parsed_time"] for row in merged], sorted(row["parsed_time"] for row in merged))
        self.assertEqual({row["source_room_original"] for row in merged}, {"room-a", "room-b"})

    def test_a_merge_with_a_limit_keeps_the_earliest_n_by_time(self) -> None:
        everything = collect_messages(_stream(), limit=0, room_merge={"room-b": "room-a"})
        earliest = sorted(everything, key=lambda r: (r["parsed_time"], r["source_room_original"], r["observation_id"]))[:3]
        got = collect_messages(_stream(), limit=3, room_merge={"room-b": "room-a"})
        self.assertEqual(sorted(_ids(got)), sorted(_ids(earliest)))
        self.assertEqual(sorted(_ids(got)), ["o5", "o6", "o7"])

    def test_discord_rooms_are_never_merged(self) -> None:
        with self.assertRaises(ValueError):
            collect_messages(_stream(), limit=0, room_merge={"discord:1:100": "discord:1:200"})
        collect_messages(_stream(), limit=0, room_merge={"discord:1:100": "discord:1:100"})  # identity is fine

    def test_row_shape(self) -> None:
        row = collect_messages([_Obs("o1", "room-a", " 글 ", "t")], limit=0)[0]
        self.assertEqual(
            set(row),
            {"observation_id", "source_room_id", "source_room_original", "actor_label", "actor_account", "content", "parsed_time", "index"},
        )
        self.assertEqual(row["content"], "글")


if __name__ == "__main__":
    unittest.main()
