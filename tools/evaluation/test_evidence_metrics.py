"""Contract tests for retrieval-only evidence metrics."""

from __future__ import annotations

import copy
import json
from pathlib import Path
import sys
import unittest


sys.path.insert(0, str(Path(__file__).parent))

from evidence_metrics import (  # noqa: E402
    DuplicateIdentifierError,
    EmptyPositiveGoldError,
    SplitLeakageError,
    UnknownObservationReferenceError,
    evaluate_retrieval,
    load_gold,
)


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "fixtures/evaluation/evidence-gold.synthetic.json"


class EvidenceMetricsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.raw_gold = json.loads(FIXTURE.read_text(encoding="utf-8"))
        cls.gold = load_gold(FIXTURE)

    def test_fixture_is_explicitly_synthetic_and_unreviewed(self) -> None:
        self.assertEqual(self.gold.gold_kind, "synthetic")
        self.assertEqual(self.gold.review_status, "unreviewed")
        self.assertFalse(self.gold.qualification_eligible)
        self.assertEqual(self.gold.qualification_status, "ineligible_synthetic")

    def test_overlap_duplicates_are_collapsed_before_k_and_same_speaker_irrelevance_does_not_score(self) -> None:
        retrieved = {
            "q-multi-source-trip": [
                "obs.discord.alpha.trip-date",
                "obs.discord.alpha.trip-date",  # overlapping window duplicate
                "obs.kakao.shared.trip-place",
            ],
            "q-same-speaker-relevance": ["obs.discord.alpha.same-speaker-irrelevant"],
            "q-overlap-dedup": [
                "obs.discord.alpha.speaker-target",
                "obs.discord.alpha.speaker-target",
            ],
            "q-out-of-scope-exact-text": ["obs.kakao.beta.same-text"],
            "q-expected-abstain": [],
            "q-holdout-fact": ["obs.discord.holdout.fact"],
        }

        report = evaluate_retrieval(self.gold, retrieved, k=2)
        by_id = {item.question_id: item for item in report.question_reports}

        # The duplicate occupies one independent-observation rank, so both
        # required groups fit in k=2.
        self.assertEqual(by_id["q-multi-source-trip"].retrieved_observation_ids, (
            "obs.discord.alpha.trip-date",
            "obs.kakao.shared.trip-place",
        ))
        self.assertEqual(by_id["q-multi-source-trip"].recovered_group_count, 2)
        self.assertTrue(by_id["q-multi-source-trip"].complete_group_recovery)
        self.assertEqual(by_id["q-same-speaker-relevance"].recovered_group_count, 0)

        self.assertEqual(report.k, 2)
        self.assertEqual(report.k_unit, "independent_observations")
        self.assertEqual(report.required_group_count, 6)
        self.assertEqual(report.recovered_group_count, 4)
        self.assertAlmostEqual(report.evidence_recall, 4 / 6)
        self.assertEqual(report.complete_group_recovery_count, 3)
        self.assertAlmostEqual(report.complete_group_recovery_rate, 3 / 5)

    def test_multi_evidence_requires_every_group_and_alternatives_are_or(self) -> None:
        retrieved = {
            "q-multi-source-trip": ["obs.discord.alpha.trip-date"],
            "q-same-speaker-relevance": ["obs.discord.alpha.speaker-target"],
            "q-overlap-dedup": ["obs.discord.alpha.speaker-target"],
            "q-out-of-scope-exact-text": ["obs.discord.alpha.same-text"],
            "q-expected-abstain": ["obs.discord.holdout.fact"],
            "q-holdout-fact": ["obs.discord.holdout.fact"],
        }
        report = evaluate_retrieval(self.gold, retrieved, k=10)
        multi = next(item for item in report.question_reports if item.question_id == "q-multi-source-trip")
        self.assertEqual(multi.recovered_group_count, 1)
        self.assertEqual(multi.required_group_count, 2)
        self.assertFalse(multi.complete_group_recovery)

    def test_out_of_scope_exact_text_is_forbidden_and_same_text_channel_is_distinct(self) -> None:
        retrieved = {
            "q-multi-source-trip": [],
            "q-same-speaker-relevance": [],
            "q-overlap-dedup": [],
            "q-out-of-scope-exact-text": ["obs.kakao.beta.same-text"],
            "q-expected-abstain": [],
            "q-holdout-fact": [],
        }
        report = evaluate_retrieval(self.gold, retrieved)
        case = next(item for item in report.question_reports if item.question_id == "q-out-of-scope-exact-text")
        self.assertEqual(case.recovered_group_count, 0)
        self.assertEqual(case.forbidden_observation_ids, ("obs.kakao.beta.same-text",))
        self.assertEqual(case.forbidden_hit_count, 1)
        self.assertEqual(case.out_of_scope_hit_count, 1)
        self.assertEqual(report.forbidden_hit_count, 1)

    def test_expected_abstain_reports_empty_retrieval_only(self) -> None:
        retrieved = {
            "q-multi-source-trip": [],
            "q-same-speaker-relevance": [],
            "q-overlap-dedup": [],
            "q-out-of-scope-exact-text": [],
            "q-expected-abstain": [],
            "q-holdout-fact": [],
        }
        report = evaluate_retrieval(self.gold, retrieved)
        case = next(item for item in report.question_reports if item.question_id == "q-expected-abstain")
        self.assertTrue(case.retrieval_empty)
        self.assertTrue(case.retrieval_empty_on_expected_abstain)
        self.assertEqual(report.abstain_expected_count, 1)
        self.assertEqual(report.retrieval_empty_on_expected_abstain_count, 1)
        self.assertEqual(report.retrieval_empty_on_expected_abstain_rate, 1.0)
        self.assertNotIn("abstain_accuracy", report.as_dict()["metrics"])
        self.assertTrue(any("not evidence" in item for item in report.limitations))

    def test_dev_and_holdout_cannot_share_episode_group(self) -> None:
        raw = copy.deepcopy(self.raw_gold)
        holdout = next(item for item in raw["questions"] if item["question_id"] == "q-holdout-fact")
        holdout["episode_group"] = "episode-trip"
        with self.assertRaises(SplitLeakageError):
            load_gold(raw)

    def test_malformed_empty_positive_duplicate_and_unknown_references_raise(self) -> None:
        empty_positive = copy.deepcopy(self.raw_gold)
        abstain = next(item for item in empty_positive["questions"] if item["question_id"] == "q-expected-abstain")
        abstain["abstain_expected"] = False
        with self.assertRaises(EmptyPositiveGoldError):
            load_gold(empty_positive)

        duplicate = copy.deepcopy(self.raw_gold)
        duplicate["observations"].append(copy.deepcopy(duplicate["observations"][0]))
        with self.assertRaises(DuplicateIdentifierError):
            load_gold(duplicate)

        unknown = copy.deepcopy(self.raw_gold)
        group = unknown["questions"][0]["required_evidence_groups"][0]
        group["alternatives"] = ["obs.unknown"]
        with self.assertRaises(UnknownObservationReferenceError):
            load_gold(unknown)


if __name__ == "__main__":
    unittest.main()
