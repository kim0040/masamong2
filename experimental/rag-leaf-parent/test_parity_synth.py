"""The synthetic sandbox of the parity gate (`make parity`): the same bytes every run, and an intent table that
reaches the paths the gate exists for. Nothing here touches the network or a stored index."""

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

import parity_synth


def digest(folder: Path) -> dict[str, str]:
    return {str(p.relative_to(folder)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(folder.rglob("*")) if p.is_file()}


class ParitySynthTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.tmp = tempfile.TemporaryDirectory()
        cls.first = Path(cls.tmp.name) / "first"
        cls.second = Path(cls.tmp.name) / "second"
        parity_synth.build(cls.first)
        parity_synth.build(cls.second)
        cls.cases = json.loads((cls.first / "intents.json").read_text(encoding="utf-8"))
        cls.leaves = json.loads((cls.first / "data" / "leaves.json").read_text(encoding="utf-8"))
        cls.messages = json.loads((cls.first / "data" / "messages.json").read_text(encoding="utf-8"))

    @classmethod
    def tearDownClass(cls) -> None:
        cls.tmp.cleanup()

    def test_two_builds_are_byte_identical(self) -> None:
        self.assertEqual(digest(self.first), digest(self.second))

    def test_every_case_names_a_leaf_that_exists(self) -> None:
        ids = {leaf["doc_id"] for leaf in self.leaves}
        names = [case["name"] for case in self.cases]
        self.assertEqual(len(names), len(set(names)), "case names are question ids")
        for case in self.cases:
            self.assertIn(case["leaf"], ids, case["name"])
            self.assertIn(case["scope"], ("all", "room"), case["name"])

    def test_the_table_reaches_every_intent_path(self) -> None:
        kinds = {(case["plan"].get("time") or {}).get("kind") for case in self.cases}
        kinds.add("recent" if any(case["plan"].get("time_scope") == "recent" for case in self.cases) else None)
        self.assertGreaterEqual(kinds, {"between", "before", "after", "today", "yesterday", "last_week", "this_month", "recent", "any"})
        refs = [case["reference"] for case in self.cases if case["reference"]]
        self.assertTrue(any(r["naive_tz"] for r in refs) and any(not r["naive_tz"] for r in refs), "naive records with and without a declared zone")
        self.assertTrue(any(not r["tz"] for r in refs), "a naive reference clock")
        self.assertTrue(any(not case["reference"] and (case["plan"].get("time") or {}).get("kind") == "last_week" for case in self.cases),
                        "a relative range with no reference clock")
        targets = [t for case in self.cases for t in case["plan"].get("person_targets") or []]
        self.assertTrue(targets and any(case["plan"].get("unresolved") for case in self.cases), "people, and one that cannot be found")
        self.assertTrue(any(case["plan"].get("person_targets") and (case["plan"].get("time") or case["plan"].get("time_scope")) for case in self.cases),
                        "a person and a time range together")

    def test_the_corpus_has_both_time_frames(self) -> None:
        aware = [m for m in self.messages if m["parsed_time"].endswith("+09:00")]
        naive = [m for m in self.messages if "+" not in m["parsed_time"]]
        self.assertTrue(aware and naive, "Discord-style (with an offset) and Kakao-style (without) records")
        self.assertTrue(all(m["source_room_id"].startswith("kakao:") for m in naive))

    def test_the_sandbox_artifacts_the_loaders_need_exist(self) -> None:
        for name in ("messages.json", "leaves.json", "summaries.json", "leaf_vecs.npy", "summary_vecs.npy", "meta.json"):
            self.assertTrue((self.first / "data" / name).is_file(), name)
        cfg = json.loads((self.first / "config.json").read_text(encoding="utf-8"))
        self.assertTrue(cfg["summary_enabled"])
        self.assertEqual(cfg["retrieval"]["person"]["identity"], "both")


if __name__ == "__main__":
    unittest.main()
