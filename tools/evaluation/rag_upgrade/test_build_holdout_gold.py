from __future__ import annotations

import random
import unittest

from rag_upgrade import build_holdout_gold as b


class ContainmentTests(unittest.TestCase):
    def test_copied_question_is_high(self) -> None:
        self.assertGreater(b.containment("내일 강남역에서 만나자", "내일 강남역에서 만나자 ㅋㅋ"), 0.9)

    def test_paraphrase_is_low(self) -> None:
        self.assertLess(b.containment("약속 장소가 어디였지?", "내일 강남역에서 만나자"), 0.3)

    def test_names_are_ignored(self) -> None:
        with_name = b.containment("철수 뭐 샀대?", "철수: 그래픽카드 샀다")
        self.assertLess(with_name, b.containment("철수 뭐 샀대?", "철수: 그래픽카드 샀다") + 1e-9)
        self.assertLess(b.containment("철수 뭐 샀대?", "철수: 그래픽카드 샀다", ignore=["철수"]), with_name)

    def test_common_run(self) -> None:
        self.assertEqual(b.longest_common_substring("abcdef", "zzcdezz"), 3)


class EligibilityTests(unittest.TestCase):
    def test_placeholders_and_noise(self) -> None:
        self.assertFalse(b.eligible_anchor("사진"))
        self.assertFalse(b.eligible_anchor("ㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋㅋ"))
        self.assertFalse(b.eligible_anchor("https://example.com/a/very/long/path"))
        self.assertFalse(b.eligible_anchor("<@123456789012345678> 짧음"))
        self.assertTrue(b.eligible_anchor("다음 주 토요일에 부산으로 여행 가기로 했어"))


class QuotaTests(unittest.TestCase):
    def test_sqrt_share_limits_giant_room(self) -> None:
        q = b.room_quota({"big": 40000, "mid": 10000, "small": 3500, "tiny": 50}, 60, min_per_room=3, min_room_msgs=300)
        self.assertNotIn("tiny", q)
        self.assertLess(q["big"] / sum(q.values()), 0.6)  # raw share would be 0.75
        self.assertGreaterEqual(q["small"], 3)
        self.assertLessEqual(sum(q.values()), 60)

    def test_stratified_spreads_over_time(self) -> None:
        rows = [{"i": i} for i in range(1000)]
        picks = b.stratified_anchors(rows, 10, random.Random(1))
        self.assertEqual(len(set(p * 10 // 1000 for p in picks)), 10)


class ReviewTests(unittest.TestCase):
    def _review(self, obj):
        return b.review_question(obj, anchor_text="다음 주 토요일에 부산으로 여행 가기로 했어",
                                 speaker_labels=["영희"], max_containment=0.5, max_common_run=7)

    def test_accepts_paraphrase(self) -> None:
        ok, why, _ = self._review({"question": "영희 여행 어디로 간다고 했지?", "answer": "부산"})
        self.assertTrue(ok, why)

    def test_rejects_copy_and_scaffold_and_skip(self) -> None:
        self.assertEqual(self._review({"question": "다음 주 토요일에 부산으로 여행 가기로 했어?", "answer": "응"})[1], "copied_bigrams")
        self.assertEqual(self._review({"question": "[*] 줄에서 뭐라 했어?", "answer": "x"})[1], "scaffold_leak")
        self.assertEqual(self._review({"skip": True})[1], "model_skip")
        self.assertEqual(self._review(None)[1], "unparsable")

    def test_parse_json_fenced(self) -> None:
        self.assertEqual(b.parse_json_object('```json\n{"question": "q", "answer": "a"}\n```')["answer"], "a")


def _msgs(n: int, *, speakers=("가", "나"), gap_min: int = 1, text="오늘 저녁에 같이 밥 먹을 곳 정하자") -> list[dict]:
    from datetime import datetime, timedelta

    t0 = datetime(2026, 1, 1, 12, 0, 0)
    return [{"observation_id": f"o{i:03d}", "actor_label": speakers[i % len(speakers)], "content": f"{text} {i}",
             "parsed_time": (t0 + timedelta(minutes=i * gap_min)).isoformat(), "index": i} for i in range(n)]


class SpanTests(unittest.TestCase):
    def test_valid_span_needs_two_speakers_and_short_gaps(self) -> None:
        rows = _msgs(10)
        self.assertTrue(b.valid_span(rows, 2, 4))
        self.assertFalse(b.valid_span(rows, 8, 4))  # runs past the end
        self.assertFalse(b.valid_span(_msgs(10, speakers=("가",)), 2, 4))  # one speaker
        self.assertFalse(b.valid_span(_msgs(10, gap_min=45), 2, 3))  # pause longer than 30 min
        self.assertFalse(b.valid_span(rows, 2, 4, exclude={"o003"}))  # already gold evidence

    def test_valid_span_needs_substantive_messages(self) -> None:
        rows = _msgs(6, text="ㅋㅋ")
        self.assertFalse(b.valid_span(rows, 0, 4))
        rows[1]["content"] = rows[2]["content"] = "토요일에 부산으로 가기로 했어"
        self.assertTrue(b.valid_span(rows, 0, 4))

    def test_pick_spans_are_valid_distinct_and_spread(self) -> None:
        rows = _msgs(1000)
        picks = b.pick_spans(rows, 10, random.Random(3))
        self.assertEqual(len(picks), 10)
        used: set[int] = set()
        for start, length in picks:
            self.assertTrue(3 <= length <= 6)
            self.assertTrue(b.valid_span(rows, start, length))
            span = set(range(start, start + length))
            self.assertFalse(used & span)  # non-overlapping
            used |= span
        self.assertGreaterEqual(len({s * 10 // 1000 for s, _ in picks}), 8)  # time deciles

    def test_pick_spans_skips_excluded(self) -> None:
        rows = _msgs(200)
        banned = {f"o{i:03d}" for i in range(0, 100)}
        for start, length in b.pick_spans(rows, 8, random.Random(1), exclude=banned):
            self.assertGreaterEqual(start, 100)

    def test_span_snippet_marks_only_span_lines(self) -> None:
        rows = _msgs(30)
        lines = b.span_snippet(rows, 10, 4).split("\n")
        self.assertEqual(len(lines), 6 + 4 + 6)
        self.assertEqual([i for i, l in enumerate(lines) if l.startswith("[*")], [6, 7, 8, 9])
        self.assertTrue(lines[6].startswith("[*1]") and lines[9].startswith("[*4]"))

    def test_prompt_for_dispatches_on_span(self) -> None:
        rows = _msgs(30)
        span_job = {"rows": rows, "pos": 10, "length": 4, "qtype": "plan_change"}
        self.assertIn("[*1]", b._prompt_for(span_job))
        self.assertIn("lines_used", b._prompt_for(span_job))
        self.assertNotIn("lines_used", b._prompt_for({"rows": rows, "pos": 10, "qtype": "fact"}))

    def test_span_jobs_cycle_types_and_stay_in_quota_rooms(self) -> None:
        class A:
            n_answerable, min_per_room, min_room_msgs, oversample = 8, 3, 300, 2.0
            span_min_len, span_max_len, span_max_gap_min = 3, 6, 30.0

        jobs = b._span_jobs({"r1": _msgs(900), "tiny": _msgs(20)}, A, random.Random(5), set())
        self.assertTrue(jobs)
        self.assertEqual({j["room"] for j in jobs}, {"r1"})
        self.assertEqual({j["qtype"] for j in jobs}, set(b.SPAN_TYPES))
        self.assertTrue(all(3 <= j["length"] <= 6 for j in jobs))

    def _review(self, obj):
        spans = ["다음 주 토요일에 부산으로 여행 가기로 했어", "아 근데 토요일은 비 온대 일요일로 바꾸자", "좋아 그럼 일요일에 출발"]
        return b.review_span_question(obj, span_texts=spans, speaker_labels=["영희"], max_containment=0.5, max_common_run=7)

    def test_review_span_accepts_multi_line_paraphrase(self) -> None:
        ok, why, stats = self._review({"question": "여행 날짜 결국 언제로 정했어?", "answer": "일요일", "lines_used": [1, 2, 3]})
        self.assertTrue(ok, why)
        self.assertEqual(stats["lines_used"], 3)

    def test_review_span_rejects_single_line_scaffold_and_copy(self) -> None:
        good = {"question": "여행 날짜 결국 언제로 정했어?", "answer": "일요일"}
        self.assertEqual(self._review({**good, "lines_used": [2]})[1], "single_line")
        self.assertEqual(self._review({**good, "lines_used": [2, 2]})[1], "single_line")
        self.assertEqual(self._review({**good, "lines_used": [1, 9]})[1], "single_line")  # 9 is not a span line
        self.assertEqual(self._review(good)[1], "single_line")
        self.assertEqual(self._review({**good, "question": "[*2] 여행 날짜?", "lines_used": [1, 2]})[1], "scaffold_leak")
        self.assertEqual(self._review({**good, "question": "2번째 줄에서 뭐라고 했지 여행", "lines_used": [1, 2]})[1], "scaffold_leak")
        copied = {"question": "토요일은 비 온대 일요일로 바꾸자 맞지?", "answer": "응", "lines_used": [1, 2]}
        self.assertIn(self._review(copied)[1], ("copied_span", "copied_bigrams"))
        self.assertEqual(self._review({"skip": True})[1], "model_skip")


if __name__ == "__main__":
    unittest.main()
