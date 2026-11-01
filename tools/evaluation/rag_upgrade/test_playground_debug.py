"""Tests for playground ranking aliases and compact traces."""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.playground_debug import (  # noqa: E402
    RankingError,
    append_debug_turn,
    compact_hits,
    normalize_ranking,
    recent_debug_turns,
)


class PlaygroundDebugTests(unittest.TestCase):
    def test_normalize_ranking_aliases(self):
        self.assertEqual(normalize_ranking("R2"), "rerank")
        self.assertEqual(normalize_ranking("cohere"), "rerank")  # 옛 벤더명 별칭
        self.assertEqual(normalize_ranking("heuristic"), "heuristic")
        self.assertEqual(normalize_ranking(None, default="hybrid"), "hybrid")
        with self.assertRaises(RankingError):
            normalize_ranking("unique-token")

    def test_roundtrip_jsonl(self):
        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            debug_id = append_debug_turn(
                data,
                query="민석 소개",
                ranking="rerank",
                hits=[{"doc_id": "leaf:0-8", "date": "2024-01-01", "excerpt": "hello"}],
                extra={"intent": {"intro": True}},
            )
            rows = recent_debug_turns(data, limit=5)
            self.assertEqual(len(rows), 1)
            self.assertEqual(rows[0]["debug_id"], debug_id)
            self.assertEqual(rows[0]["ranking"], "rerank")
            self.assertEqual(rows[0]["hits"][0]["doc_id"], "leaf:0-8")
            dump = data / "debug_dumps" / f"{debug_id}.json"
            self.assertTrue(dump.is_file())

    def test_error_and_load_record(self):
        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            from rag_upgrade.playground_debug import append_debug_error, load_debug_record

            debug_id = append_debug_error(
                data,
                query="실패 질문",
                ranking="rerank",
                error=RuntimeError("boom"),
                extra={"path": "test"},
            )
            row = load_debug_record(data, debug_id)
            self.assertIsNotNone(row)
            self.assertEqual(row["kind"], "error")
            self.assertEqual(row["error_type"], "RuntimeError")

    def test_compact_hits_truncates(self):
        hits = compact_hits(
            [{"excerpt": "x" * 1000, "observation_ids": ["a", "b"]}],
            excerpt_chars=10,
        )
        self.assertEqual(len(hits[0]["excerpt"]), 10)
        self.assertEqual(hits[0]["observation_id_count"], 2)
        self.assertEqual(hits[0]["observation_ids"], ["a", "b"])


if __name__ == "__main__":
    unittest.main()
