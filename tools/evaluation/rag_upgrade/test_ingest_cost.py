"""Tests for ingest cost estimator."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.ingest_cost import estimate_ingest, segment_count  # noqa: E402


class IngestCostTests(unittest.TestCase):
    def test_segment_count(self):
        self.assertEqual(segment_count(0, size=8), 0)
        self.assertEqual(segment_count(7, size=8), 1)
        self.assertEqual(segment_count(8, size=8), 1)
        self.assertEqual(segment_count(16, size=8), 2)
        self.assertEqual(segment_count(40, size=8, limit=3), 3)

    def test_estimate_has_range(self):
        got = estimate_ingest(
            rooms=[{"id": "kakao:x", "messages": 8000, "avg_chars": 10}],
            summary_enabled=True,
        )
        self.assertGreater(got["leaves"], 0)
        self.assertGreater(got["usd"]["high"], got["usd"]["mid"])
        self.assertGreater(got["usd"]["mid"], got["usd"]["low"])
        self.assertIn("예상치", got["disclaimer"])


if __name__ == "__main__":
    unittest.main()
