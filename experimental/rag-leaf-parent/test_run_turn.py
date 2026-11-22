"""chat() and chat_stream() are one turn (engine_turn._run_turn): same checks, same prompt, same answer payload."""

from __future__ import annotations

import json
import os
import re
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import engine as E
import engine_index as EI
from client import ClientError
from test_engine_retrieval import build_index
from test_runtime_hardening import _planner_required, _TurnClient


class _RecordingClient(_TurnClient):
    """Keeps what the answer was asked with on either transport."""

    def __init__(self, *args, **kwargs) -> None:  # noqa: ANN002, ANN003
        super().__init__(*args, **kwargs)
        self.answer_prompts: list[list[dict]] = []

    def chat(self, messages, **kwargs):  # noqa: ANN001
        if self.chats:  # the first call is the planner
            self.answer_prompts.append(list(messages))
        return super().chat(messages, **kwargs)

    def chat_stream(self, messages, **kwargs):  # noqa: ANN001
        self.answer_prompts.append(list(messages))
        return super().chat_stream(messages, **kwargs)


def _without_clock(messages: list[dict]) -> str:
    """The prompt text with the wall-clock line ("기준 시각: ...") taken out."""
    return re.sub(r"기준 시각: \S+", "기준 시각: <now>", json.dumps(messages, ensure_ascii=False))


class RunTurnTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name)
        self.prev = EI.context().paths.data
        self.prev_secrets = os.environ.get("EVAL_SECRETS")
        self.prev_legacy = os.environ.get("MASAMONG_LEGACY_ROOT")
        E._bind_data_dir(self.data)
        os.environ["MASAMONG_LEGACY_ROOT"] = str(self.data / "no-legacy")
        os.environ["EVAL_SECRETS"] = str(self.data / "secrets.env")
        (self.data / "secrets.env").write_text("OPENROUTER_API_KEY=synthetic\n", encoding="utf-8")
        runtime = {
            "schema_version": 1, "agent_system_prompt": "test", "lite_system_prompt": "", "channels": {},
            "active_channel_id": "", "tools_enabled": ["memory.search"], "tool_defaults": {},
        }
        (self.data / "bot_runtime.json").write_text(json.dumps(runtime), encoding="utf-8")
        self.index = build_index(with_summaries=False)
        self.index.cfg.update(session_budget_usd=5.0, turn_budget_usd=1.0, ranking="heuristic", top_k=3)

    def tearDown(self) -> None:
        E._bind_data_dir(self.prev)
        for key, value in (("EVAL_SECRETS", self.prev_secrets), ("MASAMONG_LEGACY_ROOT", self.prev_legacy)):
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self.tmp.cleanup()

    def _run(self, transport: str, **kwargs):
        client = _RecordingClient(plan_json=_planner_required(), answer="답")
        with mock.patch("engine_turn.make_client", side_effect=lambda _cfg, **_k: client):
            if transport == "chat":
                return E.chat(self.index, message="마사몽 가명이 누구야", **kwargs), client
            events = list(E.chat_stream(self.index, message="마사몽 가명이 누구야", **kwargs))
        return events, client

    def test_both_transports_send_the_same_prompt_and_return_the_same_answer(self) -> None:
        result, chat_client = self._run("chat")
        events, stream_client = self._run("stream")
        self.assertEqual(len(chat_client.answer_prompts), 1)
        self.assertEqual(len(stream_client.answer_prompts), 1)
        self.assertEqual(_without_clock(chat_client.answer_prompts[0]), _without_clock(stream_client.answer_prompts[0]))
        self.assertEqual(_without_clock(chat_client.chats[0]), _without_clock(stream_client.chats[0]))  # the planner
        done = events[-1]
        self.assertEqual(done["event"], "done")
        volatile = {"event", "session_summary", "debug_id", "timing", "diagnosis"}
        self.assertEqual(
            {k: v for k, v in result.items() if k not in volatile and k != "hits"},
            {k: v for k, v in done.items() if k not in volatile and k != "hits"},
        )
        self.assertEqual(result["hits"], done["hits"])
        self.assertIn("session_summary", result)
        self.assertNotIn("session_summary", done)
        # the order of the stream: status, status, debug, hits, tools, status, delta, done
        self.assertEqual(
            [e["event"] for e in events],
            ["status", "status", "debug", "hits", "tools", "status", "delta", "done"],
        )

    def test_both_transports_refuse_the_same_requests(self) -> None:
        for bad in ("  ", "가" * 2001):
            with self.assertRaises(E.PlaygroundError) as chat_err:
                E.chat(self.index, message=bad)
            with self.assertRaises(E.PlaygroundError) as stream_err:
                list(E.chat_stream(self.index, message=bad))
            self.assertEqual(str(chat_err.exception), str(stream_err.exception))
        with self.assertRaises(E.PlaygroundError):
            E.chat(self.index, message="안녕", ranking="nope")
        with self.assertRaises(E.PlaygroundError):
            list(E.chat_stream(self.index, message="안녕", ranking="nope"))

    def test_a_failed_generation_is_logged_and_raised_the_same_way(self) -> None:
        class _Down(_TurnClient):
            def chat(self, messages, **kwargs):  # noqa: ANN001
                if self.chats:
                    raise ClientError("down")
                return super().chat(messages, **kwargs)

            def chat_stream(self, messages, **kwargs):  # noqa: ANN001
                raise ClientError("down")

        for transport in ("chat", "stream"):
            client = _Down(plan_json=_planner_required())
            with mock.patch("engine_turn.make_client", side_effect=lambda _cfg, **_k: client):
                with self.assertRaises(E.PlaygroundError) as err:
                    if transport == "chat":
                        E.chat(self.index, message="마사몽 가명이 누구야")
                    else:
                        list(E.chat_stream(self.index, message="마사몽 가명이 누구야"))
            self.assertEqual(str(err.exception), "down")
        lines = (self.data / "debug_turns.jsonl").read_text(encoding="utf-8").splitlines()
        errors = [row for row in map(json.loads, lines) if row.get("kind") == "error"]
        self.assertEqual([row["path"] for row in errors], ["chat", "stream"])
        self.assertNotIn("partial_chars", errors[0])
        self.assertEqual(errors[1]["partial_chars"], 0)


if __name__ == "__main__":
    unittest.main()
