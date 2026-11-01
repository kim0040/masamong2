"""Tests for shared Action IDs and ! command mapping."""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from bot_plan import needs_local_rag, parse_tool_plan
from bot_tools import execute_named_tool
from tool_parity import canonicalize_tool, default_enabled_ids, parse_bang_command


class ToolParityTests(unittest.TestCase):
    def test_legacy_aliases_map_to_catalog_ids(self):
        self.assertEqual(canonicalize_tool("local_rag"), "memory.search")
        self.assertEqual(canonicalize_tool("get_weather_forecast"), "weather.query")
        self.assertEqual(canonicalize_tool("web_search"), "search.web")
        self.assertEqual(canonicalize_tool("get_stock_price"), "finance.quote")
        self.assertIn("weather.query", default_enabled_ids())
        self.assertIn("memory.search", default_enabled_ids())

    def test_bang_weather_and_quote(self):
        weather = parse_bang_command("!날씨 전주")
        self.assertIsNotNone(weather)
        self.assertEqual(weather["tool_name"], "weather.query")
        self.assertEqual(weather["parameters"]["region"], "전주")
        quote = parse_bang_command("!시세 AAPL")
        self.assertEqual(quote["tool_name"], "finance.quote")
        self.assertEqual(quote["parameters"]["symbol"], "AAPL")
        self.assertIsNone(parse_bang_command("날씨만 말해"))

    def test_plan_canonicalizes_legacy_names(self):
        plan = parse_tool_plan(
            '{"tool_plan":[{"tool_name":"get_weather_forecast","parameters":{"location":"전주"}}]}'
        )
        self.assertEqual(plan[0]["tool_name"], "weather.query")
        self.assertFalse(needs_local_rag(plan))
        self.assertTrue(needs_local_rag([{"tool_name": "local_rag", "parameters": {}}]))

    def test_poll_writes_only_under_data_dir(self):
        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            result = execute_named_tool(
                "polls.create",
                {"question": "점심?", "options": ["국밥", "라면"]},
                data_dir=data,
            )
            self.assertTrue(result["ok"])
            self.assertTrue((data / "sandbox_polls.json").is_file())
            poll = result["data"]["poll"]
            self.assertEqual(poll["question"]["text"], "점심?")
            self.assertEqual(poll["duration"], 24)
            self.assertFalse(poll["allow_multiselect"])
            self.assertEqual(poll["layout_type"], 1)
            self.assertEqual(poll["answers"][0]["poll_media"]["text"], "국밥")

    def test_bang_poll_pipe_answers(self):
        parsed = parse_bang_command("!투표 점심? | 김밥, 라면")
        self.assertEqual(parsed["tool_name"], "polls.create")
        self.assertEqual(parsed["parameters"]["question"], "점심?")
        self.assertEqual(parsed["parameters"]["options"], ["김밥", "라면"])
        self.assertEqual(parsed["parameters"]["duration_hours"], 24)


if __name__ == "__main__":
    unittest.main()
