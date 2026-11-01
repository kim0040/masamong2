"""Tests for prepare_review_pack (offline, no network)."""

from __future__ import annotations

import json
import socket
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.prepare_review_pack import prepare_pack  # noqa: E402
from rag_upgrade.test_quality_gate import _gold  # noqa: E402


def _no_network(*_a, **_k):
    raise AssertionError("network")


class PackTests(unittest.TestCase):
    def setUp(self) -> None:
        self._orig = socket.socket.connect
        socket.socket.connect = _no_network  # type: ignore[assignment]

    def tearDown(self) -> None:
        socket.socket.connect = self._orig  # type: ignore[assignment]

    def test_sample_has_blank_review_and_no_promotion(self) -> None:
        pack = prepare_pack(_gold(), sample_size=20)
        self.assertEqual(pack["summary"]["sample_size"], 4)
        self.assertEqual(pack["summary"]["review_status"], "unreviewed")
        for item in pack["items"]:
            self.assertIsNone(item["review"]["reviewer"])
            self.assertEqual(item["review"]["review_status"], "unreviewed")
            self.assertIsNone(item["review"]["answer_support"])
            for ref in item["observation_refs"]:
                self.assertIsNone(ref["body"])

    def test_private_out_dir_has_bodies_summary_does_not(self) -> None:
        gold = _gold()
        with tempfile.TemporaryDirectory() as tmp:
            pack = prepare_pack(gold, sample_size=20, out_dir=tmp)
            summary = json.loads((Path(tmp) / "pack_summary.json").read_text())
            private = json.loads((Path(tmp) / "review_pack.json").read_text())
            dumped = json.dumps(summary)
            self.assertNotIn("alpha body one", dumped)
            self.assertTrue(any("alpha body" in json.dumps(row) for row in private))
            self.assertEqual(pack["summary"]["dataset_id"], "qg-fixture")

    def test_split_leak_is_rejected(self) -> None:
        leaked = _gold(split_b="holdout")
        leaked["questions"][2]["question_split_group"] = "ep-a-1"
        leaked["questions"][2]["episode_group"] = "ep-a-1"
        with self.assertRaises(Exception):
            prepare_pack(leaked, sample_size=20)


if __name__ == "__main__":
    unittest.main()
