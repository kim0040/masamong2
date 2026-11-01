"""Offline tests for retrieval-v2 evidence metrics and question selection."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.run_retrieval_v2_eval import (  # noqa: E402
    CachingClient,
    _filter_questions,
    _filter_questions_by_fold,
    _parse_csv_filter,
    _question_fold,
    _attach_rerank_budget_ledger,
    _rerank_budget_summary,
    _resolve_rerank_budget_usd,
    _visible_evidence_metrics,
    build_arg_parser,
)
from rag_upgrade.rerank_client import RerankBudgetLedger  # noqa: E402


class VisibleEvidenceMetricTests(unittest.TestCase):
    def test_legacy_want_ids_separate_any_from_all(self) -> None:
        question = {
            "want_observation_ids": ["obs-a", "obs-b"],
            "forbidden_observation_ids": ["obs-forbidden"],
        }
        hits = [
            {"visible_observation_ids": ["obs-a"]},
            {"visible_observation_ids": ["obs-b"]},
            {"visible_observation_ids": ["obs-forbidden"]},
        ]

        result = _visible_evidence_metrics(hits, question)

        self.assertEqual(result["required_evidence_mode"], "set")
        self.assertEqual(result["visible_any_rank"], 1)
        self.assertEqual(result["visible_all_rank"], 2)
        self.assertTrue(result["visible_recall@1"])
        self.assertFalse(result["all_required_visible@1"])
        self.assertTrue(result["all_required_visible@3"])
        self.assertTrue(result["forbidden_visible_hit"])
        self.assertEqual(result["forbidden_visible_rank"], 3)

    def test_groups_are_and_of_or_alternatives(self) -> None:
        question = {
            "required_evidence_groups": [
                {"group_id": "date", "alternatives": ["date-a", "date-b"]},
                {"group_id": "place", "alternatives": ["place-a", "place-b"]},
            ]
        }
        hits = [
            {"visible_observation_ids": ["date-b"]},
            {"visible_observation_ids": ["place-a"]},
        ]

        result = _visible_evidence_metrics(hits, question)

        self.assertEqual(result["required_evidence_mode"], "groups")
        self.assertEqual(result["visible_any_rank"], 1)
        self.assertEqual(result["visible_all_rank"], 2)
        self.assertTrue(result["all_required_visible@3"])

    def test_new_metrics_only_use_visible_ids(self) -> None:
        question = {"want_observation_ids": ["obs-a", "obs-b"]}
        hits = [
            {"observation_ids": ["obs-a"], "leaf_observation_ids": ["obs-a"]},
            {"visible_observation_ids": ["obs-b"]},
        ]

        result = _visible_evidence_metrics(hits, question)

        self.assertEqual(result["visible_any_rank"], 2)
        self.assertEqual(result["visible_all_rank"], 0)
        self.assertFalse(result["visible_recall@1"])
        self.assertFalse(result["all_required_visible@10"])


class SelectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.questions = [
            {"question_id": "q-dev-person", "split": "dev", "question_type": "person"},
            {"question_id": "q-hold-event", "split": "holdout", "question_type": "event"},
            {"question_id": "q-dev-casual", "split": "dev", "question_type": "casual"},
        ]

    def test_csv_filters_and_paired_subset(self) -> None:
        selected = _filter_questions(
            self.questions,
            splits=_parse_csv_filter("dev, dev"),
            question_types=_parse_csv_filter("person,event"),
        )
        self.assertEqual([q["question_id"] for q in selected], ["q-dev-person"])

    def test_fold_is_stable_and_partitions_questions(self) -> None:
        self.assertEqual(_question_fold("same-question", 5), _question_fold("same-question", 5))
        parts = [
            _filter_questions_by_fold(self.questions, 3, index)
            for index in range(3)
        ]
        observed = [q["question_id"] for part in parts for q in part]
        self.assertEqual(sorted(observed), sorted(q["question_id"] for q in self.questions))
        self.assertEqual(len(observed), len(set(observed)))
        with self.assertRaises(ValueError):
            _filter_questions_by_fold(self.questions, 2, 2)

    def test_argparse_exposes_optional_filters_and_fold(self) -> None:
        args = build_arg_parser().parse_args(
            [
                "--out",
                "out",
                "--splits",
                "dev,holdout",
                "--question-types",
                "person,event",
                "--fold-count",
                "3",
                "--fold-index",
                "1",
                "--rerank-budget-usd",
                "0.25",
            ]
        )
        self.assertEqual(args.splits, "dev,holdout")
        self.assertEqual(args.question_types, "person,event")
        self.assertEqual(args.fold_count, 3)
        self.assertEqual(args.fold_index, 1)
        self.assertEqual(args.rerank_budget_usd, 0.25)

    def test_default_parser_keeps_filters_unset(self) -> None:
        args = build_arg_parser().parse_args(["--out", "out"])
        self.assertEqual(args.splits, "")
        self.assertEqual(args.question_types, "")
        self.assertIsNone(args.fold_count)
        self.assertIsNone(args.fold_index)
        self.assertIsNone(args.rerank_budget_usd)


class EmbeddingCacheTests(unittest.TestCase):
    def test_batch_prewarm_then_row_lookup_uses_one_inner_call(self) -> None:
        class Inner:
            embed_model = "embed-model"
            usage = SimpleNamespace(cost_usd=0.0)

            def __init__(self) -> None:
                self.calls: list[list[str]] = []

            def embed(self, texts, **_kwargs):
                self.calls.append(list(texts))
                return [[float(index), 1.0] for index, _text in enumerate(texts)]

        inner = Inner()
        client = CachingClient(inner, None)
        client.embed(["q1", "q2", "q3"], kind="query", instruction="i")
        client.embed(["q1"], kind="query", instruction="i")
        client.embed(["q2"], kind="query", instruction="i")
        self.assertEqual(inner.calls, [["q1", "q2", "q3"]])
        self.assertEqual(client.misses, 3)
        self.assertEqual(client.hits, 2)


class RerankBudgetTests(unittest.TestCase):
    def test_default_and_explicit_budget_are_positive(self) -> None:
        self.assertEqual(_resolve_rerank_budget_usd(1.0, None), 1.0)
        self.assertEqual(_resolve_rerank_budget_usd(1.0, 0.25), 0.25)
        self.assertEqual(_resolve_rerank_budget_usd(1.0, 1.01), 1.01)

    def test_non_positive_or_non_finite_budget_is_rejected(self) -> None:
        for total, requested in ((0.0, None), (-1.0, None), (1.0, 0.0), (1.0, -0.1), (1.0, float("nan"))):
            with self.subTest(total=total, requested=requested):
                with self.assertRaises(ValueError):
                    _resolve_rerank_budget_usd(total, requested)

    def test_attach_creates_one_shared_ledger_for_fake_index(self) -> None:
        index = SimpleNamespace()
        ledger = _attach_rerank_budget_ledger(index, 0.1)
        self.assertIs(index.rerank_budget_ledger, ledger)
        self.assertIsInstance(ledger, RerankBudgetLedger)
        self.assertEqual(ledger.spent_usd, 0.0)
        self.assertEqual(ledger.unknown_cost_attempts, 0)

    def test_summary_reports_verified_and_unknown_accounting(self) -> None:
        ledger = RerankBudgetLedger(0.1)
        verified = ledger.reserve(0.02)
        ledger.settle(verified, 0.01, basis="provider")
        unknown = ledger.reserve(0.02)
        ledger.settle(unknown, 0.02, basis="reservation", unknown=True)

        summary = _rerank_budget_summary(ledger, play_budget_usd=0.1)

        self.assertEqual(
            set(summary),
            {
                "rerank_budget_usd",
                "play_budget_usd",
                "authorized_combined_cap_usd",
                "spent_usd",
                "reserved_usd",
                "verified_cost_usd",
                "estimated_cost_usd",
                "unknown_cost_usd",
                "unknown_cost_attempts",
                "state",
            },
        )
        self.assertEqual(summary["rerank_budget_usd"], 0.1)
        self.assertEqual(summary["play_budget_usd"], 0.1)
        self.assertEqual(summary["authorized_combined_cap_usd"], 0.2)
        self.assertEqual(summary["spent_usd"], 0.03)
        self.assertEqual(summary["reserved_usd"], 0.0)
        self.assertEqual(summary["verified_cost_usd"], 0.01)
        self.assertEqual(summary["estimated_cost_usd"], 0.02)
        self.assertEqual(summary["unknown_cost_usd"], 0.02)
        self.assertEqual(summary["unknown_cost_attempts"], 1)
        self.assertEqual(summary["state"], "ready")


if __name__ == "__main__":
    unittest.main()
