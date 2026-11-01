"""턴 진단·검수 시트·판정 로그·보고서 (네트워크·원문 없음)."""

from __future__ import annotations

import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import engine  # noqa: F401  (tools/evaluation을 sys.path에 넣는다)
import debug_report
import review_sheets
from rag_upgrade.playground_debug import append_client_event, append_debug_flag, append_debug_turn
from turn_diagnosis import build_diagnosis


def _trace(**extra):
    base = {
        "applied": "rerank",
        "rerank_performed": True,
        "ranking_requested": "auto",
        "ranking_auto_reason": "rerank_uncertain",
        "auto_confidence": {"level": "medium", "reasons": ["low_margin"]},
        "rerank_min_score": 0.5,
        "rerank_order": ["a", "b", "c"],
        "rerank_scores": [0.53, 0.49, 0.2],
        "rerank_kept_n": 1,
        "queries_used": {"q0": "원문", "q1": "의미"},
        "candidate_union_n": 40,
        "pool_n": 20,
        "retrievers_used": ["leaf_lexical", "leaf_dense"],
        "embed_source": "joined",
    }
    base.update(extra)
    return base


class DiagnosisTests(unittest.TestCase):
    def test_scores_floor_and_near_floor_warning(self) -> None:
        d = build_diagnosis(
            rank_trace=_trace(), intent={"source": "planner", "kind": "fact"},
            hits=[{"doc_id": "a", "rank": 1, "speakers": ["x"]}], rag_used=True,
            rag_state="candidates", memory_requirement="required",
        )
        self.assertEqual([t["above_floor"] for t in d["rerank"]["top"]], [True, False, False])
        self.assertEqual(d["hits"][0]["score"], 0.53)
        self.assertIn("near_floor", [w["code"] for w in d["warnings"]])
        self.assertEqual(d["ranking"]["confidence"], "medium")

    def test_floor_margin_comes_from_config(self) -> None:
        d = build_diagnosis(
            rank_trace=_trace(), intent={"source": "planner"}, hits=[{"doc_id": "a"}], rag_used=True,
            rag_state="candidates", memory_requirement="required", cfg={"diagnosis": {"floor_margin": 0.0}},
        )
        self.assertNotIn("near_floor", [w["code"] for w in d["warnings"]])

    def test_abstained_and_fallback_intent_and_person(self) -> None:
        d = build_diagnosis(
            rank_trace=_trace(abstained=True, persons={"targets": ["p"], "speakers": [], "unresolved": ["p"], "ambiguous": []}),
            intent={"source": "neutral", "targets": ["p"]}, hits=[], rag_used=True,
            rag_state="insufficient_authorized_candidates", memory_requirement="required", answer="모르겠어",
        )
        codes = {w["code"] for w in d["warnings"]}
        self.assertTrue({"abstained_floor", "planner_fallback", "person_unresolved"} <= codes)
        self.assertEqual(d["persons"]["unresolved"], ["p"])

    def test_no_rag_turn_has_no_search_warnings(self) -> None:
        d = build_diagnosis(rank_trace={}, intent={}, hits=[], rag_used=False, rag_state="not_attempted", memory_requirement="none")
        self.assertEqual(d["warnings"], [])
        self.assertFalse(d["rerank"]["performed"])

    def test_misaligned_scores_are_ignored(self) -> None:
        d = build_diagnosis(rank_trace=_trace(rerank_scores=[0.9]), intent={}, hits=[{"doc_id": "a"}], rag_used=True,
                            rag_state="candidates", memory_requirement="required")
        self.assertIsNone(d["hits"][0]["score"])
        self.assertFalse(d["rerank"]["performed"])


class ReviewSheetTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        (root / ".masamong-scratch" / "g").mkdir(parents=True)
        self.sheet = root / ".masamong-scratch" / "g" / "x.review.json"
        self.sheet.write_text(json.dumps([
            {"question_id": "q1", "query": "질문", "type": "fact", "reference_answer": "답", "expected_abstention": False, "verdict": "", "note": ""},
            {"question_id": "q2", "query": "없는 것", "type": "abstain", "reference_answer": "", "expected_abstention": True, "verdict": "", "note": ""},
        ]), encoding="utf-8")
        (root / ".masamong-scratch" / "g" / "x.json").write_text(json.dumps({"questions": [
            {"question_id": "q1", "source_leaf_doc_id": "leaf:1-3", "want_observation_ids": ["o2"], "room": "discord:1:9"},
            {"question_id": "q2", "expected_abstention": True},
        ]}), encoding="utf-8")
        patcher = mock.patch.object(review_sheets, "repo_root", return_value=root)
        patcher.start()
        self.addCleanup(patcher.stop)
        msgs = [
            {"observation_id": f"o{i}", "source_room_id": "discord:1:9", "actor_label": f"u{i}", "content": f"m{i}", "parsed_time": "t"}
            for i in range(6)
        ]
        self.index = SimpleNamespace(
            leaf_by_id={"leaf:1-3": {"start": 1, "end": 3, "source_room_id": "discord:1:9"}}, messages=msgs
        )

    def test_list_marks_binding(self) -> None:
        sets = review_sheets.list_sheets(self.index)
        self.assertEqual(Path(sets[0]["set"]).as_posix(), "g/x.review.json")
        self.assertEqual(sets[0]["bound_share"], 1.0)
        self.assertEqual(review_sheets.list_sheets(SimpleNamespace(leaf_by_id={}))[0]["bound_share"], 0.0)
        # 같은 위치 id라도 다른 방의 leaf면 일치로 보지 않는다(인덱스마다 leaf id가 위치 기반).
        other = SimpleNamespace(leaf_by_id={"leaf:1-3": {"start": 1, "end": 3, "source_room_id": "kakao:other"}}, messages=[])
        self.assertEqual(review_sheets.list_sheets(other)[0]["bound_share"], 0.0)
        self.assertFalse(review_sheets.sheet_items(other, "g/x.review.json")["items"][0]["evidence"]["bound"])

    def test_items_show_context_and_wanted_line(self) -> None:
        data = review_sheets.sheet_items(self.index, "g/x.review.json", context=1)
        ev = data["items"][0]["evidence"]
        self.assertTrue(ev["bound"])
        self.assertEqual([line["text"] for line in ev["lines"]], ["m0", "m1", "m2", "m3"])
        self.assertEqual([line["wanted"] for line in ev["lines"]], [False, False, True, False])
        self.assertNotIn("evidence", data["items"][1])

    def test_save_verdict_only_touches_verdict_and_note(self) -> None:
        out = review_sheets.save_verdict("g/x.review.json", question_id="q1", verdict="fix", note="기준 답 모호")
        self.assertEqual(out["reviewed"], 1)
        rows = json.loads(self.sheet.read_text(encoding="utf-8"))
        self.assertEqual((rows[0]["verdict"], rows[0]["note"], rows[0]["query"]), ("fix", "기준 답 모호", "질문"))
        with self.assertRaises(ValueError):
            review_sheets.save_verdict("g/x.review.json", question_id="q1", verdict="maybe", note="")

    def test_paths_outside_scratch_are_refused(self) -> None:
        outside = Path(self.tmp.name) / "evil.review.json"
        outside.write_text("[]", encoding="utf-8")
        for rel in ("../evil.review.json", "g/x.json", "g/missing.review.json"):
            with self.assertRaises(ValueError):
                review_sheets.sheet_items(self.index, rel)


class LogAndReportTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.data = Path(self.tmp.name) / "test-sandbox" / "data-x"
        self.data.mkdir(parents=True)

    def _turn(self, run_id: str, query: str) -> str:
        return append_debug_turn(
            self.data, query=query, ranking="auto", hits=[{"doc_id": "a", "rank": 1, "excerpt": "근거"}],
            extra={
                "answer": "답", "run": {"server_run_id": run_id, "dataset": "exp"},
                "rank_trace": _trace(), "intent": {"source": "planner"}, "rag_used": True,
                "timing": {"plan_ms": 1000, "generate_ms": 2000, "total_ms": 3000}, "spent_usd": 0.001,
            },
        )

    def test_flag_tags_client_event_and_report(self) -> None:
        self._turn("run-old", "예전 질문")
        did = self._turn("run-new", "새 질문")
        append_debug_flag(self.data, debug_id=did, ok=False, note="사람 틀림", tags=["wrong_person", " ", "x" * 80])
        append_client_event(self.data, event={"type": "js_error", "message": "boom", "secret": "drop me"})
        rows = [json.loads(line) for line in (self.data / "debug_turns.jsonl").read_text(encoding="utf-8").splitlines()]
        flag = next(r for r in rows if r["kind"] == "flag")
        self.assertEqual(flag["tags"], ["wrong_person", "x" * 40])
        client = next(r for r in rows if r["kind"] == "client_error")
        self.assertNotIn("secret", client)

        buf = io.StringIO()
        with redirect_stdout(buf):
            debug_report.main(["--log", str(self.data / "debug_turns.jsonl")])
        text = buf.getvalue()
        self.assertIn("run-new", text)
        self.assertIn("새 질문", text)
        self.assertNotIn("예전 질문", text)
        self.assertIn("틀림 [wrong_person", text)
        self.assertIn("near_floor", text)
        self.assertIn("브라우저 오류", text)

        buf = io.StringIO()
        with redirect_stdout(buf):
            debug_report.main(["--log", str(self.data / "debug_turns.jsonl"), "--run", "all", "--only", "bad", "--format", "json"])
        payload = json.loads(buf.getvalue())
        self.assertEqual([t["query"] for t in payload["turns"]], ["새 질문"])
        self.assertEqual(payload["turns"][0]["diagnosis"]["rerank"]["floor"], 0.5)

        summary = debug_report.summarize_run([self.data / "debug_turns.jsonl"], "run-new")
        self.assertEqual(
            {k: summary[k] for k in ("turns", "ok", "bad", "unjudged", "warned", "errors")},
            {"turns": 1, "ok": 0, "bad": 1, "unjudged": 0, "warned": 1, "errors": 0},
        )
        append_debug_flag(self.data, debug_id=did, ok=True, note="")
        self.assertEqual(debug_report.summarize_run([self.data / "debug_turns.jsonl"], "run-new")["ok"], 1)


if __name__ == "__main__":
    unittest.main()
