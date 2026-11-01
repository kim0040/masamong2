"""Prepare progress percent/ETA helpers. No message bodies."""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from prepare_progress import idle_payload, snapshot, write_progress


class PrepareProgressTests(unittest.TestCase):
    def test_phase_and_overall(self):
        row = snapshot(phase="summaries", done=36, total=100, started_at=1.0)
        self.assertEqual(row["remaining"], 64)
        self.assertEqual(row["phase_pct"], 36.0)
        self.assertGreater(row["overall_pct"], 8)
        self.assertLess(row["overall_pct"], 80)
        self.assertIn("남음", row["label"])

    def test_write_and_idle(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "prepare_progress.json"
            payload = snapshot(phase="embeddings", done=10, total=20, status="running")
            self.assertTrue(write_progress(path, payload, force=True))
            saved = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(saved["done"], 10)
        idle = idle_payload(ready=False)
        self.assertEqual(idle["status"], "idle")
        ready = idle_payload(ready=True)
        self.assertEqual(ready["status"], "ready")


if __name__ == "__main__":
    unittest.main()
