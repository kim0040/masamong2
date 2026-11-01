"""Tests for sandbox bot runtime import and planner."""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from bot_plan import needs_local_rag, parse_tool_plan
from bot_runtime import (
    channel_id_from_room,
    compose_system_prompt,
    ensure_runtime,
    import_from_legacy_prompts,
    update_channel,
)


class BotRuntimeTests(unittest.TestCase):
    def test_room_to_channel_id(self):
        self.assertEqual(
            channel_id_from_room("discord:100100000000000101:100200000000000102"),
            "100200000000000102",
        )

    def test_import_and_enable_indexed_only(self):
        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            prompts = data / "prompts.json"
            prompts.write_text(
                json.dumps(
                    {
                        "prompts": {"agent_system_prompt": "너는 마사몽이야."},
                        "channels": {
                            "100200000000000102": {
                                "allowed": True,
                                "persona": "친구 페르소나",
                                "rules": "태그 숨김",
                            },
                            "999": {"allowed": True, "persona": "다른 채널", "rules": ""},
                        },
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            imported = import_from_legacy_prompts(prompts)
            self.assertIn("100200000000000102", imported["channels"])
            runtime = {
                "schema_version": 1,
                "agent_system_prompt": imported["agent_system_prompt"],
                "lite_system_prompt": "",
                "channels": imported["channels"],
                "mention_required": False,
                "tools_enabled": ["memory.search"],
            }
            (data / "bot_runtime.json").write_text(
                json.dumps(runtime, ensure_ascii=False), encoding="utf-8"
            )
            got = ensure_runtime(
                data,
                cfg={},
                indexed_rooms=["discord:1:100200000000000102"],
            )
            self.assertTrue(got["channels"]["100200000000000102"]["enabled"])
            self.assertFalse(got["channels"]["999"]["enabled"])
            self.assertTrue(got["channels"]["999"]["indexed"] is False)
            # F3: a stored restriction is kept as-is; defaults are not re-added on load.
            self.assertEqual(got["tools_enabled"], ["memory.search"])
            self.assertEqual(got["tools_enabled_source"], "stored")

    def test_persona_edit_roundtrip(self):
        runtime = {"channels": {}}
        ch = update_channel(runtime, "1", {"persona": "새 말투", "rules": "짧게"})
        self.assertEqual(ch["persona"], "새 말투")
        prompt = compose_system_prompt({"agent_system_prompt": "봇"}, ch)
        self.assertIn("새 말투", prompt)
        self.assertIn("말투 유지 계약", prompt)
        self.assertNotIn("채팅상 농담", prompt)
        from engine import ANSWER_SYSTEM

        self.assertNotIn("채팅상 농담", ANSWER_SYSTEM)
        self.assertNotIn("확인되지 않는다", ANSWER_SYSTEM)

    def test_compact_timing_sums_stages(self):
        from engine import _compact_timing

        got = _compact_timing(
            {"plan_ms": 100, "retrieve_ms": 999},
            {"embed_ms": 50, "rerank_ms": 660},
            {"generate_ms": 2000},
        )
        self.assertEqual(got["plan_ms"], 100)
        self.assertNotIn("retrieve_ms", got)
        self.assertEqual(got["total_ms"], 2810)
        plan = parse_tool_plan(
            '{"tool_plan":[{"tool_name":"get_weather_forecast","parameters":{"location":"전주"}}]}'
        )
        self.assertEqual(plan[0]["tool_name"], "weather.query")
        self.assertFalse(needs_local_rag(plan))
        self.assertTrue(needs_local_rag([{"tool_name": "local_rag", "parameters": {}}]))

    def test_public_list_includes_explicit_extra_room(self):
        from bot_runtime import public_channel_list

        rows = public_channel_list(
            {"channels": {}, "indexed_rooms": []},
            extra_rooms=["kakao:friends-old", "kakao:friends-new"],
        )
        labels = {r["label"]: r for r in rows}
        self.assertIn("friends-new", labels)
        self.assertFalse(labels["friends-new"]["indexed"])
        self.assertEqual(labels["friends-new"]["channel_id"], "kakao:friends-new")
        self.assertEqual(labels["friends-old"]["channel_id"], "kakao:friends-old")


if __name__ == "__main__":
    unittest.main()


class RespondPolicyTests(unittest.TestCase):
    """Sandbox channel policy uses the product's respond enum (config.RespondModes)."""

    def test_modes_match_product_and_contract(self) -> None:
        import json
        from bot_runtime import RESPOND_MODES
        from tool_parity import _repo_root

        go = (_repo_root() / "internal" / "config" / "document.go").read_text(encoding="utf-8")
        for mode in RESPOND_MODES:
            self.assertIn(f'"{mode}"', go)
        self.assertEqual(set(RESPOND_MODES), {"command", "mention", "always", "off"})

    def test_gate_rules(self) -> None:
        from bot_runtime import respond_allows

        self.assertFalse(respond_allows("off", is_command=True, mentioned=True)[0])
        self.assertTrue(respond_allows("command", is_command=True, mentioned=False)[0])
        self.assertFalse(respond_allows("command", is_command=False, mentioned=True)[0])
        self.assertFalse(respond_allows("mention", is_command=False, mentioned=False)[0])
        self.assertTrue(respond_allows("mention", is_command=False, mentioned=True)[0])
        self.assertTrue(respond_allows("mention", is_command=True, mentioned=False)[0])
        self.assertTrue(respond_allows("always", is_command=False, mentioned=False)[0])

    def test_legacy_allowed_and_mention_migrate(self) -> None:
        from bot_runtime import ensure_runtime, save_runtime

        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            save_runtime(data, {
                "schema_version": 1, "agent_system_prompt": "x", "mention_required": True,
                "channels": {"111": {"channel_id": "111", "allowed": True}, "222": {"channel_id": "222", "allowed": False}},
            })
            rt = ensure_runtime(data, cfg={"legacy_prompts_path": str(data / "none.json")}, indexed_rooms=["discord:1:111", "discord:1:222"])
            self.assertEqual(rt["channels"]["111"]["respond"], "mention")
            self.assertEqual(rt["channels"]["222"]["respond"], "off")
            self.assertFalse(rt["channels"]["222"]["enabled"])

    def test_update_channel_validates_respond(self) -> None:
        rt = {"channels": {}}
        ch = update_channel(rt, "9", {"respond": "command"})
        self.assertEqual((ch["respond"], ch["allowed"]), ("command", True))
        with self.assertRaises(ValueError):
            update_channel(rt, "9", {"respond": "sometimes"})
        ch = update_channel(rt, "9", {"allowed": False})
        self.assertEqual(ch["respond"], "off")


class ToolPolicyTests(unittest.TestCase):
    """F3: stored tool restrictions survive load; [] is all-off, never allow-all."""

    def _reload(self, payload):
        from bot_runtime import load_runtime, save_runtime

        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            if isinstance(payload, str):
                (data / "bot_runtime.json").write_text(payload, encoding="utf-8")
            else:
                save_runtime(data, payload)
            return load_runtime(data)

    def test_missing_file_uses_catalog_default(self) -> None:
        from bot_runtime import load_runtime
        from tool_parity import default_enabled_ids

        with tempfile.TemporaryDirectory() as td:
            rt = load_runtime(Path(td))
        self.assertEqual(rt["tools_enabled"], default_enabled_ids())
        self.assertEqual(rt["tools_enabled_source"], "default")

    def test_single_tool_stays_single_after_reload(self) -> None:
        rt = self._reload({"schema_version": 1, "tools_enabled": ["help.version"]})
        self.assertEqual(rt["tools_enabled"], ["help.version"])
        self.assertEqual(rt["tools_enabled_source"], "stored")
        rt2 = self._reload(rt)  # save → load round trip keeps the restriction
        self.assertEqual(rt2["tools_enabled"], ["help.version"])

    def test_explicit_empty_list_disables_all(self) -> None:
        from bot_runtime import gate_tool, is_tool_enabled, tool_policy

        rt = self._reload({"schema_version": 1, "tools_enabled": []})
        self.assertEqual(rt["tools_enabled"], [])
        self.assertEqual(rt["tools_enabled_source"], "stored_empty")
        pol = tool_policy(rt)
        self.assertTrue(pol["all_disabled"])
        self.assertEqual(pol["enabled"], ())
        self.assertFalse(is_tool_enabled(rt, "weather.query"))
        denied = gate_tool(rt, "weather.query")
        self.assertEqual((denied["tool"], denied["ok"], denied["status"], denied["error"]), ("weather.query", False, "disabled", "disabled"))

    def test_field_absent_vs_null_vs_empty(self) -> None:
        from bot_runtime import tool_policy
        from tool_parity import default_enabled_ids

        self.assertEqual(self._reload({"schema_version": 1})["tools_enabled_source"], "default")
        self.assertEqual(self._reload({"schema_version": 1, "tools_enabled": None})["tools_enabled_source"], "default")
        self.assertEqual(self._reload({"schema_version": 1, "tools_enabled": []})["tools_enabled_source"], "stored_empty")
        # a runtime dict without the field (caller gave no scope) → documented default
        self.assertEqual(tool_policy({})["enabled"], tuple(default_enabled_ids()))
        self.assertEqual(tool_policy({"tools_enabled": []})["enabled"], ())

    def test_legacy_alias_migrates_without_widening(self) -> None:
        rt = self._reload({"schema_version": 1, "tools_enabled": ["local_rag", "get_weather_forecast", "local_rag"]})
        self.assertEqual(rt["tools_enabled"], ["memory.search", "weather.query"])

    def test_unknown_ids_never_add_known_tools(self) -> None:
        rt = self._reload({"schema_version": 1, "tools_enabled": ["nope.tool", 42]})
        self.assertEqual(rt["tools_enabled"], [])
        self.assertEqual(rt["tools_enabled_source"], "stored_empty")
        self.assertIn("nope.tool", rt["tools_enabled_unknown"])

    def test_corrupt_field_or_file_fails_closed(self) -> None:
        rt = self._reload({"schema_version": 1, "tools_enabled": "all"})
        self.assertEqual((rt["tools_enabled"], rt["tools_enabled_source"]), ([], "invalid_field"))
        rt = self._reload({"schema_version": 1, "tools_enabled": {"weather.query": True}})
        self.assertEqual((rt["tools_enabled"], rt["tools_enabled_source"]), ([], "invalid_field"))
        rt = self._reload("{not json")
        self.assertEqual((rt["tools_enabled"], rt["tools_enabled_source"]), ([], "invalid_file"))
        rt = self._reload("[1, 2]")
        self.assertEqual((rt["tools_enabled"], rt["tools_enabled_source"]), ([], "invalid_file"))

    def test_ensure_runtime_keeps_restriction(self) -> None:
        from bot_runtime import ensure_runtime

        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            (data / "bot_runtime.json").write_text(
                json.dumps({"schema_version": 1, "tools_enabled": []}), encoding="utf-8"
            )
            rt = ensure_runtime(data, cfg={"legacy_prompts_path": str(data / "none.json")}, indexed_rooms=["discord:1:111"])
            self.assertEqual(rt["tools_enabled"], [])
            saved = json.loads((data / "bot_runtime.json").read_text(encoding="utf-8"))
            self.assertEqual(saved["tools_enabled"], [])

    def test_gate_accepts_aliases_and_rejects_unknown(self) -> None:
        from bot_runtime import gate_tool, is_tool_enabled

        rt = {"tools_enabled": ["weather.query"]}
        self.assertIsNone(gate_tool(rt, "get_weather_forecast"))
        self.assertTrue(is_tool_enabled(rt, "get_weather_forecast"))
        self.assertIsNotNone(gate_tool(rt, "finance.quote"))
        self.assertIsNotNone(gate_tool(rt, "made.up"))
        self.assertIsNotNone(gate_tool(rt, ""))

    def test_memory_search_gate(self) -> None:
        from bot_runtime import memory_search_gate

        off = {"tools_enabled": ["help.version"]}
        on = {"tools_enabled": ["memory.search"]}
        self.assertEqual(memory_search_gate(off, "required")["action"], "refuse")
        self.assertEqual(memory_search_gate(off, "optional")["action"], "skip")
        self.assertEqual(memory_search_gate(off, "none")["action"], "skip")
        self.assertEqual(memory_search_gate(on, "required")["action"], "search")
        self.assertEqual(memory_search_gate(on, None)["action"], "search")
        self.assertEqual(memory_search_gate(on, "none")["action"], "skip")
        self.assertEqual(memory_search_gate({"tools_enabled": []}, "required")["action"], "refuse")

    def test_resolve_tool_defaults_is_explicit_and_sourced(self) -> None:
        from bot_runtime import resolve_tool_defaults

        self.assertEqual(resolve_tool_defaults({}, {}), {})
        self.assertEqual(resolve_tool_defaults(None, None), {})
        got = resolve_tool_defaults({"tool_defaults": {"weather.query": {"region": "서울"}}}, {})
        self.assertEqual(got["weather.query"], {"region": "서울", "source": "runtime.tool_defaults"})
        got = resolve_tool_defaults(
            {"tool_defaults": {"weather.query": {"region": "서울"}}},
            {"tool_defaults": {"get_weather_forecast": {"region": "부산"}}},
        )
        self.assertEqual(got["weather.query"], {"region": "부산", "source": "channel.tool_defaults"})
        # garbage shapes are ignored, not invented
        self.assertEqual(resolve_tool_defaults({"tool_defaults": "서울"}, {"tool_defaults": {"weather.query": "부산"}}), {})
