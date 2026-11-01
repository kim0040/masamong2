"""Tests for the 07 quality-gate adapter (synthetic fixtures, network-free)."""

from __future__ import annotations

import json
import socket
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from evidence_metrics import GoldContractError, SplitLeakageError, load_gold  # noqa: E402
from rag_upgrade import quality_gate as qg  # noqa: E402


def _no_network(*args, **kwargs):  # pragma: no cover - guard
    raise AssertionError("network call attempted in an offline test")


ROOM_A = "room:a"
ROOM_B = "room:b"


def _gold(*, split_b: str = "dev") -> dict:
    """Two rooms, three positives (one multi-group), one abstain question."""

    return {
        "schema_version": 1,
        "dataset_id": "qg-fixture",
        "gold_kind": "synthetic",
        "review_status": "unreviewed",
        "reviewer": None,
        "observations": [
            {"observation_id": "a1", "source_provenance_id": "p-a1", "source_channel_id": ROOM_A, "text": "alpha body one"},
            {"observation_id": "a2", "source_provenance_id": "p-a2", "source_channel_id": ROOM_A, "text": "alpha body two"},
            {"observation_id": "a3", "source_provenance_id": "p-a3", "source_channel_id": ROOM_A, "text": "alpha forbidden"},
            {"observation_id": "b1", "source_provenance_id": "p-b1", "source_channel_id": ROOM_B, "text": "beta body one"},
            {"observation_id": "b2", "source_provenance_id": "p-b2", "source_channel_id": ROOM_B, "text": "beta body two"},
        ],
        "questions": [
            {
                "question_id": "q-single",
                "split": "dev",
                "question_split_group": "ep-a-1",
                "episode_group": "ep-a-1",
                "allowed_source_channel_ids": [ROOM_A],
                "required_evidence_groups": [{"group_id": "g1", "alternatives": ["a1"]}],
                "forbidden_observation_ids": ["a3"],
                "abstain_expected": False,
                "review_status": "unreviewed",
                "reviewer": None,
            },
            {
                "question_id": "q-multi",
                "split": "dev",
                "question_split_group": "ep-a-2",
                "episode_group": "ep-a-2",
                "allowed_source_channel_ids": [ROOM_A],
                "required_evidence_groups": [
                    {"group_id": "g1", "alternatives": ["a1"]},
                    {"group_id": "g2", "alternatives": ["a2"]},
                ],
                "forbidden_observation_ids": [],
                "abstain_expected": False,
                "review_status": "unreviewed",
                "reviewer": None,
            },
            {
                "question_id": "q-b",
                "split": split_b,
                "question_split_group": "ep-b-1",
                "episode_group": "ep-b-1",
                "allowed_source_channel_ids": [ROOM_B],
                "required_evidence_groups": [{"group_id": "g1", "alternatives": ["b1", "b2"]}],
                "forbidden_observation_ids": [],
                "abstain_expected": False,
                "review_status": "unreviewed",
                "reviewer": None,
            },
            {
                "question_id": "q-abstain",
                "split": "dev",
                "question_split_group": "ep-none",
                "episode_group": "ep-none",
                "allowed_source_channel_ids": [ROOM_A],
                "required_evidence_groups": [],
                "forbidden_observation_ids": ["b1"],
                "abstain_expected": True,
                "review_status": "unreviewed",
                "reviewer": None,
            },
        ],
    }


def _case(qid: str, candidates, spans=(), **extra) -> dict:
    return {"question_id": qid, "candidate_observation_ids": list(candidates), "visible_spans": list(spans), **extra}


def _span(oid: str, start: int, end: int, source: str = "raw", complete=None) -> dict:
    span = {"observation_id": oid, "start": start, "end": end, "source": source}
    if complete is not None:
        span["complete"] = complete
    return span


def _full(oid: str, gold: dict, source: str = "raw") -> dict:
    text = next(o["text"] for o in gold["observations"] if o["observation_id"] == oid)
    return _span(oid, 0, len(text), source)


class VisibleEvidenceTests(unittest.TestCase):
    def setUp(self):
        self._orig = socket.socket.connect
        socket.socket.connect = _no_network  # type: ignore[assignment]
        self.gold = _gold()

    def tearDown(self):
        socket.socket.connect = self._orig  # type: ignore[assignment]

    def _run(self, cases, **kw):
        return evaluate(self.gold, cases, **kw)

    def test_matched_id_with_fully_truncated_body_is_not_visible(self):
        report = self._run([_case("q-single", ["a1"], [])])
        case = report.cases[0]
        self.assertTrue(case.candidate["complete_group_recovery"])
        self.assertFalse(case.visible["visible_complete_group_recovery"])
        self.assertEqual(case.visible["groups"][0]["status"], "candidate_only")
        self.assertEqual(case.visible["truncated_candidate_ids"], ["a1"])
        self.assertEqual(report.overall["visible"]["candidate_hit_but_not_visible_case_count"], 1)

    def test_half_span_is_partial_not_complete(self):
        report = self._run([_case("q-single", ["a1"], [_span("a1", 0, 7)])])
        case = report.cases[0]
        self.assertEqual(case.visible["groups"][0]["status"], "visible_partial_only")
        self.assertFalse(case.visible["visible_complete_group_recovery"])
        self.assertEqual(case.visible["visible_partial_observation_ids"], ["a1"])
        # two partial spans that together cover the body count as complete
        report2 = self._run([_case("q-single", ["a1"], [_span("a1", 0, 7), _span("a1", 5, 14)])])
        self.assertTrue(report2.cases[0].visible["visible_complete_group_recovery"])

    def test_summary_only_evidence_is_not_raw_evidence(self):
        report = self._run([_case("q-single", ["a1"], [_full("a1", self.gold, source="summary")])])
        case = report.cases[0]
        self.assertEqual(case.visible["groups"][0]["status"], "summary_only")
        self.assertFalse(case.visible["visible_complete_group_recovery"])
        self.assertEqual(case.visible["summary_only_observation_ids"], ["a1"])
        self.assertEqual(report.overall["visible"]["summary_only_group_count"], 1)

    def test_repeated_observation_counts_once(self):
        spans = [_full("a1", self.gold), _full("a1", self.gold, source="parent"), _full("a1", self.gold, source="leaf")]
        report = self._run([_case("q-multi", ["a1", "a1", "a1"], spans)])
        case = report.cases[1]
        self.assertEqual(case.visible["visible_observation_count"], 1)
        self.assertEqual(case.visible["duplicate_raw_span_count"], 2)
        self.assertEqual(case.visible["visible_complete_group_count"], 1)
        self.assertEqual(case.candidate["recovered_group_count"], 1)
        self.assertFalse(case.visible["visible_complete_group_recovery"])

    def test_multi_group_requires_every_group_visible(self):
        spans = [_full("a1", self.gold), _span("a2", 0, 3)]
        report = self._run([_case("q-multi", ["a1", "a2"], spans)])
        case = report.cases[1]
        self.assertTrue(case.candidate["complete_group_recovery"])
        self.assertEqual(case.visible["visible_complete_group_count"], 1)
        self.assertEqual(case.visible["visible_partial_only_group_count"], 1)
        self.assertFalse(case.visible["visible_complete_group_recovery"])
        full = [_full("a1", self.gold), _full("a2", self.gold)]
        self.assertTrue(self._run([_case("q-multi", ["a1", "a2"], full)]).cases[1].visible["visible_complete_group_recovery"])

    def test_forbidden_and_out_of_scope_are_separate_for_candidate_and_visible(self):
        # forbidden a3 only in candidates; out-of-room b1 shown visibly
        report = self._run([_case("q-single", ["a1", "a3", "b1"], [_full("a1", self.gold), _full("b1", self.gold)])])
        case = report.cases[0]
        self.assertEqual(case.candidate["forbidden_hit_ids"], ["a3"])
        self.assertEqual(case.visible["forbidden_visible_ids"], [])
        self.assertEqual(case.candidate["out_of_scope_ids"], ["b1"])
        self.assertEqual(case.visible["out_of_scope_visible_ids"], ["b1"])
        self.assertTrue(case.visible["visible_complete_group_recovery"])
        self.assertEqual(report.overall["candidate"]["forbidden_case_count"], 1)
        self.assertEqual(report.overall["visible"]["forbidden_visible_case_count"], 0)
        self.assertEqual(report.overall["visible"]["out_of_scope_visible_case_count"], 1)

    def test_time_violation_is_separate_and_respects_naive_vs_aware(self):
        times = {"a1": "2026-01-02T10:00:00+09:00", "a2": "2026-01-05T10:00:00+09:00", "b1": "2026-01-02T10:00:00"}
        bounds = {"hard_bounds": {"start": "2026-01-01T00:00:00+09:00", "end": "2026-01-03T00:00:00+09:00"}}
        spans = [_full("a1", self.gold), _full("a2", self.gold)]
        report = self._run([_case("q-multi", ["a1", "a2"], spans, time=bounds)], observation_times=times)
        case = report.cases[1]
        self.assertEqual(case.time["status"], "violation")
        self.assertEqual(case.time["visible_violation_ids"], ["a2"])
        self.assertTrue(case.visible["visible_complete_group_recovery"])  # visibility is not reduced by time
        # naive observation time against aware bounds is undetermined, never silently localized
        report_b = self._run([_case("q-b", ["b1"], [_full("b1", self.gold)], time=bounds)], observation_times=times)
        self.assertEqual(report_b.cases[2].time["status"], "undetermined")
        self.assertEqual(report_b.cases[2].time["undetermined_observation_ids"], ["b1"])
        # runtime-reported violations are kept even without observation times
        report_c = self._run([_case("q-single", ["a1"], [], time={"hard_bounds": {"start": None, "end": "2026-01-03T00:00:00"}, "violation_observation_ids": ["a1"]})])
        self.assertEqual(report_c.cases[0].time["violation_ids"], ["a1"])
        self.assertEqual(report_c.cases[0].time["status"], "violation")

    def test_person_scope_none_vs_empty(self):
        no_scope = self._run([_case("q-single", ["a1"], [], person={"allowed_person_ids": None, "resolved_person_ids": ["p1"]})])
        self.assertEqual(no_scope.cases[0].person["scope_status"], "no_scope_provided")
        empty = self._run([_case("q-single", ["a1"], [], person={"allowed_person_ids": [], "resolved_person_ids": ["p1"]})])
        self.assertEqual(empty.cases[0].person["scope_status"], "violation")
        self.assertEqual(empty.cases[0].person["out_of_scope_person_ids"], ["p1"])
        self.assertEqual(empty.cases[0].person["attribution"], qg.NOT_EVALUATED)
        ok = self._run([_case("q-single", ["a1"], [], person={"allowed_person_ids": ["p1"], "resolved_person_ids": ["p1"]})])
        self.assertEqual(ok.cases[0].person["scope_status"], "ok")

    def test_contract_rejects_offsets_beyond_body_and_lying_complete_flag(self):
        with self.assertRaises(qg.TraceContractError):
            self._run([_case("q-single", ["a1"], [_span("a1", 0, 99)])])
        with self.assertRaises(qg.TraceContractError):
            self._run([_case("q-single", ["a1"], [_span("a1", 0, 5, complete=True)])])
        with self.assertRaises(qg.TraceContractError):
            self._run([_case("q-single", ["a1"], [_span("zz", 0, 1)])])
        with self.assertRaises(qg.TraceContractError):
            self._run([_case("q-single", ["a1"], [_span("a1", 0, 5, source="excerpt")])])
        with self.assertRaises(qg.TraceContractError):
            self._run([_case("q-unknown", ["a1"], [])])


class AnswerAbstentionToolTests(unittest.TestCase):
    def setUp(self):
        self.gold = _gold()

    def test_empty_retrieval_and_abstaining_answer_are_separate(self):
        # abstain expected: empty retrieval + answered → not a structural match
        answered = evaluate(self.gold, [_case("q-abstain", [], [], answer={"outcome": "answered", "text": "..."})])
        ab = answered.cases[3].abstention
        self.assertTrue(ab["retrieval_empty"])
        self.assertFalse(ab["answer_abstained"])
        self.assertFalse(ab["structural_match"])
        self.assertTrue(ab["answered_on_expected_abstain"])
        self.assertEqual(ab["correct"], qg.NOT_EVALUATED)
        # abstained answer with non-empty retrieval → structural match, correctness still not evaluated
        abstained = evaluate(self.gold, [_case("q-abstain", ["a1"], [], answer={"outcome": "abstained"})])
        ab2 = abstained.cases[3].abstention
        self.assertFalse(ab2["retrieval_empty"])
        self.assertTrue(ab2["structural_match"])
        self.assertEqual(ab2["correct"], qg.NOT_EVALUATED)
        # positive question that abstained
        pos = evaluate(self.gold, [_case("q-single", ["a1"], [], answer={"outcome": "abstained"})])
        self.assertTrue(pos.cases[0].abstention["abstained_on_positive"])
        self.assertIsNone(pos.cases[0].abstention["structural_match"])

    def test_answer_support_requires_human_review_and_string_check_is_separate(self):
        cases = [_case("q-single", ["a1"], [_full("a1", self.gold)], answer={"outcome": "answered", "text": "alpha said x"})]
        report = evaluate(self.gold, cases, expected_answer_strings={"q-single": ["alpha"]})
        answer = report.cases[0].answer
        self.assertEqual(answer["string_check"]["status"], "matched")
        self.assertEqual(answer["string_check"]["kind"], "string_only")
        self.assertEqual(answer["support"], qg.NOT_EVALUATED)
        self.assertFalse(report.qualification["semantic_review_complete"])
        self.assertEqual(report.qualification["gate_decision"], "not_decided")
        reviews = [{"question_id": "q-single", "reviewer": "human-1", "answer_support": "unsupported", "person_attribution": "incorrect"}]
        reviewed = evaluate(self.gold, cases, reviews=reviews)
        self.assertEqual(reviewed.cases[0].answer["support"], "unsupported")
        self.assertEqual(reviewed.cases[0].answer["support_reviewer"], "human-1")
        self.assertEqual(reviewed.cases[0].person["attribution"], "incorrect")
        self.assertEqual(reviewed.overall["human_review"]["semantic_reviewed_case_count"], 1)
        self.assertFalse(reviewed.qualification["semantic_review_complete"])
        self.assertEqual(reviewed.qualification["gate_decision"], "not_decided")

    def test_review_without_reviewer_is_rejected(self):
        with self.assertRaises(qg.ReviewContractError):
            qg.load_reviews([{"question_id": "q-single", "answer_support": "supported"}])
        with self.assertRaises(qg.ReviewContractError):
            qg.load_reviews([{"question_id": "q-single", "reviewer": "h", "answer_support": "yes"}])
        with self.assertRaises(qg.ReviewContractError):
            evaluate(self.gold, [], reviews=[{"question_id": "nope", "reviewer": "h"}])

    def test_tool_outcomes_only_success_counts(self):
        tools = [
            {"name": "weather", "outcome": "success"},
            {"name": "polls", "outcome": "unknown_outcome"},
            {"name": "help", "outcome": "disabled"},
            {"name": "search", "outcome": "not_called"},
        ]
        report = evaluate(self.gold, [_case("q-single", ["a1"], [], tools=tools)])
        block = report.cases[0].tools
        self.assertEqual(block["called_count"], 3)
        self.assertEqual(block["success_count"], 1)
        self.assertFalse(block["all_called_succeeded"])
        self.assertTrue(block["any_unknown_outcome"])
        self.assertEqual(block["non_success"], {"unknown_outcome": 1, "disabled": 1})
        with self.assertRaises(qg.TraceContractError):
            qg.ToolOutcome.from_mapping({"name": "x", "outcome": "ok"})
        with self.assertRaises(qg.TraceContractError):
            qg.RunCase.from_mapping(_case("q", [], [], answer={"outcome": "done"}))


class OracleBindingPromotionTests(unittest.TestCase):
    def setUp(self):
        self.gold = _gold()

    def test_oracle_versus_actual_comparison(self):
        actual = [
            _case("q-single", ["a1"], []),
            _case("q-multi", ["a1"], [_full("a1", self.gold)]),
            _case("q-b", [], []),
            _case("q-abstain", [], []),
        ]
        cmp = qg.compare_runs(self.gold, actual, k=5)
        self.assertEqual(cmp["reference"]["visible"]["visible_complete_group_recovery_rate"], 1.0)
        self.assertEqual(cmp["reference"]["candidate"]["complete_group_recovery_rate"], 1.0)
        self.assertEqual(cmp["actual"]["visible"]["visible_complete_group_recovery_rate"], 0.0)
        self.assertAlmostEqual(cmp["actual"]["candidate"]["complete_group_recovery_rate"], 1 / 3)
        self.assertAlmostEqual(cmp["delta_actual_minus_reference"]["visible.visible_complete_group_recovery_rate"], -1.0)
        self.assertNotIn("cases", cmp["actual"])
        oracle = qg.oracle_cases(self.gold)
        self.assertEqual(oracle["q-abstain"].candidate_observation_ids, ())
        self.assertEqual(oracle["q-multi"].label, "oracle")
        self.assertEqual(qg.evaluate_run(self.gold, oracle).overall["answer"]["support"], {qg.NOT_EVALUATED: 4})

    def test_by_room_keeps_small_room_visible(self):
        actual = [
            _case("q-single", ["a1"], [_full("a1", self.gold)]),
            _case("q-multi", ["a1", "a2"], [_full("a1", self.gold), _full("a2", self.gold)]),
            _case("q-b", ["b2"], []),
            _case("q-abstain", [], []),
        ]
        report = evaluate(self.gold, actual)
        self.assertEqual(report.by_room[ROOM_A]["visible"]["visible_complete_group_recovery_rate"], 1.0)
        self.assertEqual(report.by_room[ROOM_B]["visible"]["visible_complete_group_recovery_rate"], 0.0)
        self.assertEqual(report.by_room[ROOM_B]["candidate"]["complete_group_recovery_rate"], 1.0)
        self.assertEqual(report.by_split["dev"]["question_count"], 4)
        self.assertEqual(report.overall["case_present_count"], 4)
        summary = report.public_summary()
        self.assertNotIn("cases", summary)
        self.assertNotIn("a1", json.dumps(summary))

    def test_missing_cases_are_marked_not_present(self):
        report = evaluate(self.gold, [_case("q-single", ["a1"], [])])
        self.assertEqual(report.overall["case_present_count"], 1)
        self.assertFalse(report.cases[1].case_present)
        self.assertEqual(report.cases[1].answer["outcome"], "not_run")
        with self.assertRaises(qg.TraceContractError):
            qg.evaluate_run(self.gold, [_case("q-single", ["a1"], [])], require_all_cases=True)

    def test_dev_holdout_same_episode_is_rejected(self):
        leaked = _gold(split_b="holdout")
        leaked["questions"][2]["question_split_group"] = "ep-a-1"
        leaked["questions"][2]["episode_group"] = "ep-a-1"
        with self.assertRaises(SplitLeakageError):
            load_gold(leaked)
        clean = _gold(split_b="holdout")
        self.assertEqual(evaluate(clean, []).by_split["holdout"]["question_count"], 1)

    def test_stale_body_behind_same_id_is_rejected(self):
        messages = [
            {"observation_id": o["observation_id"], "content": o["text"], "source_room_id": o["source_channel_id"]}
            for o in self.gold["observations"]
        ]
        ok = qg.verify_gold_corpus_binding(self.gold, messages)
        self.assertTrue(ok["bound"])
        messages[0]["content"] = "alpha body one (edited)"
        with self.assertRaises(qg.StaleGoldError):
            qg.verify_gold_corpus_binding(self.gold, messages)
        report = qg.verify_gold_corpus_binding(self.gold, messages, strict=False)
        self.assertEqual(report["body_changed_ids"], ["a1"])
        messages[0]["content"] = "alpha body one"
        messages[1]["source_room_id"] = ROOM_B
        self.assertEqual(qg.verify_gold_corpus_binding(self.gold, messages, strict=False)["room_changed_ids"], ["a2"])
        self.assertEqual(qg.verify_gold_corpus_binding(self.gold, messages[2:], strict=False)["missing_ids"], ["a1", "a2"])
        # a different leaf split does not change message-level binding
        self.assertTrue(qg.verify_gold_corpus_binding(self.gold, list(reversed(messages[:1] + messages[2:] + [dict(messages[1], source_room_id=ROOM_A)])))["bound"])

    def test_index_binding_reuses_fingerprint_check(self):
        gold = dict(self.gold)
        gold["index_binding"] = {"index_fingerprint": "abc", "leaf_count": 1, "message_count": 5, "leaves_sha256": "x"}
        binding = {"index_fingerprint": "abc", "leaf_count": 1, "message_count": 5, "leaves_sha256": "x"}
        self.assertTrue(qg.check_index_binding(gold, binding)["bound"])
        self.assertFalse(qg.check_index_binding(gold, {**binding, "index_fingerprint": "zzz"})["bound"])
        self.assertFalse(qg.check_index_binding(self.gold, binding)["bound"])

    def test_unreviewed_promotion_is_refused(self):
        reviews = [{"question_id": "q-single", "reviewer": "human-1", "gold_verdict": "accept"}]
        with self.assertRaises(qg.PromotionRefusedError):
            qg.apply_reviews(self.gold, reviews)
        partial = qg.apply_reviews(self.gold, reviews, require_complete=False)
        self.assertEqual(partial["gold_kind"], "synthetic")
        self.assertEqual(partial["review_status"], "unreviewed")
        self.assertIsNone(partial["reviewer"])
        self.assertEqual(partial["review_summary"], {"reviewed_count": 1, "unreviewed_count": 3, "rejected_count": 0})
        by_id = {q["question_id"]: q for q in partial["questions"]}
        self.assertEqual(by_id["q-single"]["review_status"], "reviewed")
        self.assertEqual(by_id["q-multi"]["review_status"], "unreviewed")
        self.assertEqual(load_gold(partial).qualification_status, "ineligible_synthetic")
        full = [{"question_id": q["question_id"], "reviewer": "human-1", "gold_verdict": "accept"} for q in self.gold["questions"]]
        promoted = qg.apply_reviews(self.gold, full)
        self.assertEqual(promoted["gold_kind"], "human_reviewed")
        self.assertTrue(load_gold(promoted).qualification_eligible)
        rejected = full[:3] + [{"question_id": "q-abstain", "reviewer": "human-1", "gold_verdict": "reject"}]
        with self.assertRaises(qg.PromotionRefusedError):
            qg.apply_reviews(self.gold, rejected)
        self.assertEqual(qg.apply_reviews(self.gold, rejected, require_complete=False)["review_summary"]["rejected_count"], 1)


class CliTests(unittest.TestCase):
    def test_cli_writes_private_reports_and_prints_aggregates_only(self):
        gold = _gold()
        cases = [
            _case("q-single", ["a1"], [_full("a1", gold)]),
            _case("q-multi", ["a1"], []),
            _case("q-b", ["b1"], [_full("b1", gold)]),
            _case("q-abstain", [], [], answer={"outcome": "abstained"}),
        ]
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "gold.json").write_text(json.dumps(gold), encoding="utf-8")
            (root / "run.json").write_text(json.dumps({"cases": cases}), encoding="utf-8")
            messages = [
                {"observation_id": o["observation_id"], "content": o["text"], "source_room_id": o["source_channel_id"]}
                for o in gold["observations"]
            ]
            (root / "messages.json").write_text(json.dumps(messages), encoding="utf-8")
            import io
            from contextlib import redirect_stdout

            buf = io.StringIO()
            with redirect_stdout(buf):
                rc = qg.main([
                    "--gold", str(root / "gold.json"),
                    "--run", str(root / "run.json"),
                    "--corpus-messages", str(root / "messages.json"),
                    "--oracle",
                    "--out", str(root / "out"),
                ])
            self.assertEqual(rc, 0)
            out = buf.getvalue()
            self.assertNotIn('"a1"', out)
            self.assertIn("delta_actual_minus_oracle", out)
            self.assertTrue((root / "out" / "actual.report.json").exists())
            self.assertTrue((root / "out" / "oracle.summary.json").exists())
            report = json.loads((root / "out" / "actual.report.json").read_text(encoding="utf-8"))
            self.assertEqual(report["qualification"]["gate_decision"], "not_decided")
            self.assertEqual(len(report["cases"]), 4)


def evaluate(gold, cases, **kw):
    return qg.evaluate_run(gold, cases, **kw)


if __name__ == "__main__":
    unittest.main()
