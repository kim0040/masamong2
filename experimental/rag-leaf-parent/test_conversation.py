#!/usr/bin/env python3
"""Sandbox thread store. Not Discord RAG."""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

from conversation import (
    append_turn,
    compact,
    get_conversation,
    history_for_llm,
    needs_compact,
    new_conversation,
    public_list,
    select_conversation,
)


class ConversationStoreTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_new_select_and_list(self) -> None:
        first = new_conversation(self.data, "ch-a")
        second = new_conversation(self.data, "ch-a")
        listing = public_list(self.data, "ch-a")
        self.assertEqual(listing["active_id"], second["id"])
        self.assertEqual(len(listing["conversations"]), 2)
        selected = select_conversation(self.data, "ch-a", first["id"])
        assert selected is not None
        self.assertEqual(selected["id"], first["id"])
        self.assertEqual(public_list(self.data, "ch-a")["active_id"], first["id"])

    def test_append_titles_and_keep_recent(self) -> None:
        conv = new_conversation(self.data, "ch-a")
        for i in range(8):
            conv = append_turn(
                self.data,
                "ch-a",
                conv["id"],
                user=f"질문 {i}",
                assistant=f"답 {i}",
                keep=4,
                char_limit=80,
                summarizer=lambda body: "요약됨",
            )
        self.assertEqual(len(conv["messages"]), 4)
        self.assertTrue(conv["summary"])
        self.assertNotEqual(conv["title"], "새 대화")
        summary, history = history_for_llm(conv, keep=4)
        self.assertTrue(summary)
        self.assertEqual(len(history), 4)
        self.assertEqual(history[-1]["content"], "답 7")

    def test_needs_compact_by_chars(self) -> None:
        # Two long messages exceed the char limit, but there is nothing older to fold:
        # summarizing text that stays verbatim in the prompt is a wasted LLM call.
        conv = {
            "messages": [
                {"role": "user", "content": "x" * 40},
                {"role": "assistant", "content": "y" * 40},
            ],
            "summary": "",
        }
        self.assertTrue(needs_compact(conv, keep=12, char_limit=50))
        calls = []
        out = compact(conv, keep=12, char_limit=50, summarizer=lambda body: calls.append(body) or "짧음")
        self.assertEqual(out["summary"], "")
        self.assertEqual(len(out["messages"]), 2)
        self.assertEqual(calls, [])

    def test_compact_folds_to_half_keep_with_hysteresis(self) -> None:
        calls = []

        def summarizer(body: str) -> str:
            calls.append(body)
            return f"요약{len(calls)}"

        conv = {"id": "c", "messages": [], "summary": ""}
        for t in range(1, 21):
            conv["messages"].append({"role": "user", "content": f"q{t} " + "가" * 280})
            conv["messages"].append({"role": "assistant", "content": f"a{t} " + "나" * 280})
            if needs_compact(conv, keep=12, char_limit=4500):
                conv = compact(conv, keep=12, char_limit=4500, summarizer=summarizer)
        # 20 turns: folds at 14, 18, 22... messages → roughly every 4 turns, not every turn.
        self.assertLessEqual(len(calls), 5)
        self.assertGreaterEqual(len(calls), 3)
        self.assertLessEqual(len(conv["messages"]), 12)
        self.assertGreaterEqual(len(conv["messages"]), 6)
        # no nested "이전 요약:" chains in the stored summary — the summarizer output is stored as-is
        self.assertEqual(conv["summary"], f"요약{len(calls)}")

    def test_summarizer_failure_note_does_not_accumulate(self) -> None:
        def boom(body: str) -> str:
            raise RuntimeError("api down")

        conv = {"id": "c", "messages": [], "summary": ""}
        for t in range(1, 15):
            conv["messages"].append({"role": "user", "content": f"q{t}"})
            conv["messages"].append({"role": "assistant", "content": f"a{t}"})
            if needs_compact(conv, keep=4, char_limit=4500):
                conv = compact(conv, keep=4, char_limit=4500, summarizer=boom)
        self.assertEqual(conv["summary"].count("접었다"), 1)

    def test_get_creates_when_empty(self) -> None:
        conv = get_conversation(self.data, "ch-empty", None)
        self.assertTrue(str(conv.get("id") or "").startswith("conv_"))
        self.assertEqual(public_list(self.data, "ch-empty")["active_id"], conv["id"])

    def test_get_without_create_is_empty(self) -> None:
        conv = get_conversation(self.data, "ch-none", None, create=False)
        self.assertEqual(conv.get("id"), "")
        self.assertEqual(public_list(self.data, "ch-none")["conversations"], [])


if __name__ == "__main__":
    unittest.main()
