"""Network-free tests for the soft-event synthetic gold generator."""

from __future__ import annotations

import json
import sys
import unittest
from dataclasses import dataclass
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "experimental" / "rag-leaf-parent"))

from rag_upgrade import generate_soft_event_gold as g  # noqa: E402
from soft_events import SoftEvent, build_soft_events  # noqa: E402


@dataclass
class FakeIndex:
    leaves: list[dict]
    messages: list[dict]

    def __post_init__(self) -> None:
        self.leaf_by_id = {str(row["doc_id"]): row for row in self.leaves}


def _index(*, rooms: tuple[str, ...] = ("room-a", "room-b")) -> FakeIndex:
    leaves: list[dict] = []
    messages: list[dict] = []
    message_no = 0
    for room_no, room in enumerate(rooms):
        for leaf_no in range(3):
            start = len(messages)
            for offset in range(2):
                messages.append(
                    {
                        "observation_id": f"obs-{room_no}-{leaf_no}-{offset}",
                        "content": f"사건 {leaf_no} 사실 {offset}",
                        "parsed_time": f"2026-09-{10 + room_no:02d}T{10 + leaf_no:02d}:{offset:02d}:00+00:00",
                    }
                )
                message_no += 1
            leaves.append(
                {
                    "doc_id": f"leaf-{room_no}-{leaf_no}",
                    "source_room_id": room,
                    "start": start,
                    "end": len(messages),
                    "observation_ids": [messages[i]["observation_id"] for i in range(start, len(messages))],
                    "body": " ".join(messages[i]["content"] for i in range(start, len(messages))),
                }
            )
    return FakeIndex(leaves, messages)


def _events(index: FakeIndex):
    return build_soft_events(
        index.leaves,
        recipe={"version": "test", "gap_seconds": None, "max_leaves": 2, "max_chars": 10000},
        leaf_times=g.build_leaf_times(index),
    ).events


class FakeUsage:
    def __init__(self) -> None:
        self.cost_usd = 0.01
        self.calls = 1
        self.attempts = 1
        self.errors = {}

    def as_dict(self):
        return {"cost_usd": self.cost_usd, "calls": self.calls, "attempts": self.attempts, "errors": {}}


class FakeClient:
    llm_model = "fake/model"
    embed_model = "fake/embed"
    budget_usd = 0.22

    def __init__(self, responses: list[str]) -> None:
        self.responses = list(responses)
        self.prompts: list[str] = []
        self.usage = FakeUsage()

    def chat(self, messages, **kwargs):
        self.prompts.append(messages[0]["content"])
        return self.responses.pop(0)


class SoftEventGoldTests(unittest.TestCase):
    def test_explicit_leaf_times_and_soft_event_builder(self):
        index = _index(rooms=("room-a",))
        times = g.build_leaf_times(index)
        self.assertEqual(set(times), {"leaf-0-0", "leaf-0-1", "leaf-0-2"})
        self.assertIsNotNone(times["leaf-0-0"]["start_time"])
        self.assertIsNotNone(times["leaf-0-0"]["end_time"])
        result = build_soft_events(
            index.leaves,
            recipe={"version": "test", "gap_seconds": None, "max_leaves": 2, "max_chars": 10000},
            leaf_times=times,
        )
        self.assertTrue(result.events)
        self.assertTrue(all(event.start_time is not None and event.end_time is not None for event in result.events))

    def test_selection_is_room_balanced_and_deterministic(self):
        index = _index(rooms=("room-a", "room-b", "room-c"))
        events = _events(index)
        first = g.select_candidates(events, index, questions=6, seed=77, max_per_room=2)
        second = g.select_candidates(events, index, questions=6, seed=77, max_per_room=2)
        self.assertEqual(
            [(item.event.event_id, item.split) for item in first],
            [(item.event.event_id, item.split) for item in second],
        )
        counts = {}
        for item in first:
            counts[item.event.source_room_id] = counts.get(item.event.source_room_id, 0) + 1
            self.assertGreaterEqual(len(item.evidence_leaf_ids), 2)
        self.assertTrue(all(value <= 2 for value in counts.values()))

    def test_split_does_not_depend_on_selection_limit(self):
        index = _index(rooms=("room-a", "room-b"))
        events = _events(index)
        all_rows = {item.event.event_id: item.split for item in g.select_candidates(events, index, questions=20, seed=11, max_per_room=20)}
        short_rows = {item.event.event_id: item.split for item in g.select_candidates(events, index, questions=2, seed=11, max_per_room=20)}
        for event_id, split in short_rows.items():
            self.assertEqual(split, all_rows[event_id])

    def test_split_hash_is_independent_from_selection_order(self):
        event_ids = [f"event-{number:04d}" for number in range(1000)]
        first_hundred = sorted(
            event_ids, key=lambda event_id: g._selection_hash(event_id, seed=11)
        )[:100]
        lock_count = sum(
            g.assign_split(event_id, seed=11, lock_fraction=0.25) == "lock"
            for event_id in first_hundred
        )
        self.assertGreaterEqual(lock_count, 15)
        self.assertLessEqual(lock_count, 35)

    def test_prompt_uses_opaque_labels_and_no_source_ids(self):
        index = _index(rooms=("private-room",))
        candidate = g.select_candidates(_events(index), index, questions=1, seed=3, max_per_room=2)[0]
        prompt = g.make_generation_prompt(candidate)
        self.assertIn("m000:", prompt)
        self.assertIn("[g00]", prompt)
        self.assertIn("[g01]", prompt)
        self.assertNotIn("private-room", prompt)
        self.assertNotIn(candidate.event.event_id, prompt)
        self.assertNotIn("leaf-0-0", prompt)
        self.assertNotIn("obs-0-0-0", prompt)

    def test_validation_rejects_short_copy_unknown_label_and_same_leaf(self):
        index = _index(rooms=("room-a",))
        candidate = g.select_candidates(_events(index), index, questions=1, seed=1, max_per_room=2)[0]
        valid = {
            "question": "서로 다른 사건의 원인과 대응은 무엇인가요?",
            "expected_answer": "두 메시지에 나타난 원인과 대응을 함께 확인해야 합니다.",
            "evidence_labels": ["m000", "m001"],
            "question_type": "causal",
            "answerable": True,
        }
        checked = g.validate_generated_question(valid, candidate)
        self.assertEqual(checked["evidence_labels"], ["m000", "m001"])
        with self.assertRaises(g.SoftEventGoldError) as short:
            g.validate_generated_question({**valid, "question": "왜?"}, candidate)
        self.assertEqual(str(short.exception), "question_too_short")
        with self.assertRaises(g.SoftEventGoldError) as unknown:
            g.validate_generated_question({**valid, "evidence_labels": ["m000", "m999"]}, candidate)
        self.assertEqual(str(unknown.exception), "label_outside_prompt")
        with self.assertRaises(g.SoftEventGoldError) as copied:
            g.validate_generated_question({**valid, "question": candidate.prompt_messages[0].text}, candidate)
        self.assertEqual(str(copied.exception), "query_evidence_overlap")
        same_leaf = {**valid, "evidence_labels": ["m000", "m002"]}
        # m000 and m002 are both in the first leaf under this fixture.
        with self.assertRaises(g.SoftEventGoldError) as same:
            g.validate_generated_question(same_leaf, candidate)
        self.assertEqual(str(same.exception), "same_leaf_only")

    def test_validation_rejects_message_and_group_label_leaks(self):
        index = _index(rooms=("room-a",))
        candidate = g.select_candidates(_events(index), index, questions=1, seed=1, max_per_room=2)[0]
        valid = {
            "question": "두 시점의 상황은 어떻게 달라졌나요?",
            "expected_answer": "처음에는 문제가 있었고 나중에는 해결됐습니다.",
            "evidence_labels": ["m000", "m001"],
            "question_type": "temporal_update",
            "answerable": True,
        }
        for leaked in ("m000", "g00"):
            with self.subTest(leaked=leaked):
                with self.assertRaises(g.SoftEventGoldError) as error:
                    g.validate_generated_question(
                        {**valid, "question": f"{valid['question']} {leaked}"},
                        candidate,
                    )
                self.assertEqual(str(error.exception), "opaque_label_leaked")

    def test_unanswerable_response_is_rejected(self):
        index = _index(rooms=("room-a",))
        candidate = g.select_candidates(_events(index), index, questions=1, seed=1, max_per_room=2)[0]
        response = {
            "question": "이 사건의 결정은 무엇인가요?",
            "expected_answer": "확인할 수 없습니다.",
            "evidence_labels": ["m000", "m001"],
            "question_type": "decision",
            "answerable": False,
        }
        with self.assertRaisesRegex(g.SoftEventGoldError, "unanswerable"):
            g.validate_generated_question(response, candidate)

    def test_generation_records_only_safe_reason_codes_and_schema(self):
        index = _index(rooms=("room-a",))
        candidate = g.select_candidates(_events(index), index, questions=1, seed=1, max_per_room=2)[0]
        client = FakeClient(
            [
                json.dumps(
                    {
                        "question": "사건의 원인과 후속 대응은 무엇이었나요?",
                        "expected_answer": "원인과 대응은 서로 다른 대화 조각에서 확인됩니다.",
                        "evidence_labels": ["m000", "m001"],
                        "question_type": "event",
                        "answerable": True,
                    },
                    ensure_ascii=False,
                )
            ]
        )
        questions, reasons = g.generate_questions([candidate], client, target=1)
        self.assertEqual(len(questions), 1)
        self.assertEqual(reasons, {})
        self.assertIn("want_observation_ids", questions[0])
        self.assertEqual(len(questions[0]["required_evidence_groups"]), 2)
        self.assertTrue(all(set(group) == {"group_id", "alternatives"} for group in questions[0]["required_evidence_groups"]))

    def test_client_budget_and_per_request_cap_are_explicit(self):
        cfg = {"models": {"response": "model-x", "embed": "embed-x"}, "api_workers": 3}
        with patch.object(g, "load_secrets", return_value={"OPENROUTER_API_KEY": "fake"}), patch.object(g, "PlayClient") as ctor:
            g._build_client(cfg, budget_usd=0.22, max_request_cost_usd=0.02)
        kwargs = ctor.call_args.kwargs
        self.assertEqual(kwargs["budget_usd"], 0.22)
        self.assertEqual(kwargs["max_request_cost_usd"], 0.02)
        self.assertEqual(kwargs["llm_model"], "model-x")

    def test_defaults_and_private_output_path(self):
        args = g.build_parser().parse_args([])
        self.assertEqual(args.questions, 48)
        self.assertAlmostEqual(args.budget_usd, 0.22)
        self.assertAlmostEqual(args.max_request_cost_usd, 0.02)
        self.assertEqual(args.max_per_room, 20)
        self.assertEqual(args.model, "")
        self.assertEqual(g.build_parser().parse_args(["--model", "provider/model"]).model, "provider/model")
        good = g.validate_output_path(g.REPO / ".masamong-scratch" / "x" / "gold.json")
        self.assertEqual(
            good.relative_to(g.REPO).parts,
            (".masamong-scratch", "x", "gold.json"),
        )
        with self.assertRaises(g.OutputPathError):
            g.validate_output_path(Path("/tmp/unsafe-gold.json"))

    def test_payload_binding_manifest_and_log_shape_do_not_print_text(self):
        payload = g.build_payload(
            questions=[{"question": "비공개 질문", "expected_answer": "비공개 답", "question_type": "event"}],
            binding={"index_fingerprint": "idx", "corpus_binding": {"corpus_fingerprint": "corpus"}},
            manifest={"model": "fake", "seed": 1, "usage": {"cost_usd": 0.01}},
        )
        self.assertEqual(payload["schema_version"], 2)
        self.assertEqual(payload["gold_kind"], g.GOLD_KIND)
        self.assertEqual(payload["review_status"], "unreviewed")
        summary = g._summary_output(questions=payload["questions"], reasons={"question_too_short": 1}, client=FakeClient([]), out=Path("x"))
        encoded = json.dumps(summary, ensure_ascii=False)
        self.assertNotIn("비공개", encoded)
        self.assertIn("question_too_short", encoded)


if __name__ == "__main__":
    unittest.main()
