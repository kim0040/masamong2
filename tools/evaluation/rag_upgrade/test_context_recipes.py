"""Recipe comparison stays structural, deterministic, and room-separated."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade import compare_context_recipes as ccr  # noqa: E402

HERE = Path(__file__).resolve().parent


class RecipeCompareTests(unittest.TestCase):
    def setUp(self) -> None:
        self.rows = ccr._synthetic()

    def test_rooms_never_merge(self) -> None:
        for name, builder in ccr.BUILDERS.items():
            for win in builder(self.rows):
                rooms = {ccr._room(self.rows[i]) for i in range(win["start"], win["end"])}
                self.assertEqual(len(rooms), 1, name)

    def test_report_does_not_claim_meaning_quality(self) -> None:
        report = ccr.structure_report(self.rows)
        self.assertEqual(report["meaning_quality"], "not_evaluated")
        self.assertEqual(report["room_mix_violations"], 0)
        self.assertEqual(set(report["recipes"]), set(ccr.RECIPES))
        self.assertTrue(report["recipes"]["gap30_max8"]["hypothesis"])

    def test_sample_is_deterministic(self) -> None:
        a = [m["observation_id"] for m in ccr.sample_messages(self.rows, seed="s", limit=10)]
        b = [m["observation_id"] for m in ccr.sample_messages(self.rows, seed="s", limit=10)]
        self.assertEqual(a, b)

    def test_cli_dry_run(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            out = Path(td) / "report.json"
            cmd = [
                sys.executable,
                str(HERE / "compare_context_recipes.py"),
                "--dry-run",
                "--out",
                str(out),
            ]
            proc = subprocess.run(cmd, cwd=str(HERE.parents[3]), capture_output=True, text=True, check=False)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            payload = json.loads(out.read_text(encoding="utf-8"))
            self.assertEqual(payload["meaning_quality"], "not_evaluated")


if __name__ == "__main__":
    unittest.main()
