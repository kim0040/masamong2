"""Planner contract: memory_requirement gate, query rewrite, legacy fallback."""

from __future__ import annotations

import unittest

from bot_plan import (
    PLAN_PROMPT,
    intent_from_plan,
    memory_query,
    needs_local_rag,
    parse_plan,
    parse_planner_output,
    parse_tool_plan,
    render_tool_section,
)


class ParsePlanTests(unittest.TestCase):
    def test_none_requirement_with_empty_plan_means_no_rag(self) -> None:
        parsed = parse_plan('{"memory_requirement":"none","tool_plan":[]}')
        self.assertEqual(parsed["memory_requirement"], "none")
        self.assertEqual(parsed["tool_plan"], [])
        self.assertFalse(needs_local_rag(parsed["tool_plan"], parsed["memory_requirement"]))

    def test_none_requirement_overrides_stray_memory_search(self) -> None:
        parsed = parse_plan('{"memory_requirement":"none","tool_plan":[{"tool_name":"memory.search"}]}')
        self.assertFalse(needs_local_rag(parsed["tool_plan"], parsed["memory_requirement"]))

    def test_required_adds_memory_search_step(self) -> None:
        parsed = parse_plan('{"memory_requirement":"required","tool_plan":[{"tool_name":"weather.query"}]}')
        names = [s["tool_name"] for s in parsed["tool_plan"]]
        self.assertIn("memory.search", names)
        self.assertTrue(needs_local_rag(parsed["tool_plan"], "required"))

    def test_optional_only_searches_when_planned(self) -> None:
        self.assertFalse(needs_local_rag([{"tool_name": "weather.query", "parameters": {}}], "optional"))
        self.assertTrue(needs_local_rag([{"tool_name": "memory.search", "parameters": {}}], "optional"))

    def test_legacy_output_without_requirement_keeps_old_heuristic(self) -> None:
        parsed = parse_plan('{"tool_plan":[{"tool_name":"finance.quote","parameters":{"symbol":"AAPL"}}]}')
        self.assertIsNone(parsed["memory_requirement"])
        self.assertFalse(needs_local_rag(parsed["tool_plan"], None))
        legacy = parse_plan('{"tool_plan":[]}')
        self.assertEqual(legacy["tool_plan"][0]["tool_name"], "memory.search")

    def test_garbage_degrades_to_memory_search(self) -> None:
        parsed = parse_plan("not json")
        self.assertFalse(parsed["parsed"])
        self.assertEqual(parse_tool_plan("not json")[0]["tool_name"], "memory.search")

    def test_invalid_requirement_value_is_ignored(self) -> None:
        parsed = parse_plan('{"memory_requirement":"maybe","tool_plan":[{"tool_name":"search.web"}]}')
        self.assertIsNone(parsed["memory_requirement"])

    def test_memory_query_rewrite(self) -> None:
        plan = parse_tool_plan('{"tool_plan":[{"tool_name":"local_rag","parameters":{"query":"민석 최근 근황"}}]}')
        self.assertEqual(memory_query(plan), "민석 최근 근황")
        self.assertEqual(memory_query([{"tool_name": "memory.search", "parameters": {}}]), "")
        self.assertEqual(memory_query([{"tool_name": "memory.search", "parameters": {"query": "x"}}]), "")

    def test_prompt_uses_options_for_polls_and_forbids_placeholders(self) -> None:
        self.assertIn("options", PLAN_PROMPT)
        self.assertNotIn("answers 2", PLAN_PROMPT)
        self.assertIn("자리표시자", PLAN_PROMPT)
        self.assertNotIn("빈 plan 대신 memory.search", PLAN_PROMPT)

    def test_legacy_prompt_still_single_user_template(self) -> None:
        text = PLAN_PROMPT.format(message="테스트 질문")
        self.assertTrue(text.rstrip().endswith("사용자 메시지:\n테스트 질문"))
        self.assertIn('"memory_requirement":"none|optional|required"', text)
        self.assertIn("- polls.create:", text)
        self.assertIn("unresolved", text)

    def test_render_tool_section_none_empty_subset(self) -> None:
        self.assertIn("- help.version:", render_tool_section(None))
        self.assertIn("사용할 수 있는 도구가 없다", render_tool_section([]))
        sub = render_tool_section(["local_rag", "get_weather_forecast", "nope"])
        self.assertIn("- memory.search:", sub)
        self.assertIn("- weather.query:", sub)
        self.assertNotIn("nope", sub)
        self.assertNotIn("- finance.fx:", sub)

    def test_unresolved_field_optional_and_bounded(self) -> None:
        self.assertIsNone(parse_plan('{"memory_requirement":"none","tool_plan":[]}')["unresolved"])
        parsed = parse_plan('{"memory_requirement":"required","tool_plan":[],"unresolved":["거기: 부산/강릉", "", "거기: 부산/강릉", 7]}')
        self.assertEqual(parsed["unresolved"], ["거기: 부산/강릉", "7"])
        self.assertEqual(intent_from_plan(parsed)["unresolved"], ["거기: 부산/강릉", "7"])
        many = parse_plan('{"memory_requirement":"none","tool_plan":[],"unresolved":' + str([f"u{i}" for i in range(10)]).replace("'", '"') + "}")
        self.assertEqual(len(many["unresolved"]), 6)

    def test_v1_v2_parse_unchanged_by_v3_fields(self) -> None:
        v1 = parse_plan('{"tool_plan":[{"tool_name":"memory.search","parameters":{"query":"민석"}}]}')
        self.assertIsNone(v1["memory_requirement"])
        self.assertIsNone(v1["time"])
        self.assertEqual(v1["parse_state"], "ok")
        v2 = parse_plan('{"memory_requirement":"required","time_scope":"recent","time":{"kind":"yesterday"},"question_kind":"event","person_targets":["민석"],"semantic_query":"어제 민석이 한 말","key_terms":["민석","약속"],"sensitive":false,"tool_plan":[]}')
        self.assertEqual(v2["time"], {"kind": "yesterday", "from": None, "to": None})
        self.assertEqual(v2["time_scope"], "recent")
        self.assertEqual([s["tool_name"] for s in v2["tool_plan"]], ["memory.search"])
        self.assertIsNone(v2["unresolved"])
        self.assertTrue(intent_from_plan(v2)["recent"])

    def test_strict_parse_states(self) -> None:
        self.assertEqual(parse_planner_output("")["parse_state"], "empty")
        self.assertEqual(parse_planner_output("응 알겠어")["parse_state"], "invalid")
        self.assertEqual(parse_planner_output('{"memory_requirement":"none"')["parse_state"], "truncated")
        self.assertEqual(parse_planner_output('["a"]')["parse_state"], "invalid")
        ok = parse_planner_output('```json\n{"memory_requirement":"none","tool_plan":[]}\n```')
        self.assertEqual(ok["parse_state"], "ok")
        self.assertEqual(ok["tool_plan"], [])
        for raw in ("", "응 알겠어", "x {"):
            self.assertEqual(parse_planner_output(raw)["tool_plan"], [], raw)
            self.assertIsNone(parse_planner_output(raw)["memory_requirement"], raw)

    def test_strict_truncated_salvages_only_requirement(self) -> None:
        cut = parse_planner_output('{"memory_requirement":"required","time_scope":"recent","person_targets":["민석"],"tool_plan":[{"tool_name":"weather.query","parameters":{"region":"서')
        self.assertEqual(cut["parse_state"], "truncated")
        self.assertEqual(cut["memory_requirement"], "required")
        self.assertEqual([s["tool_name"] for s in cut["tool_plan"]], ["memory.search"])
        self.assertIsNone(cut["person_targets"])
        self.assertIsNone(cut["time_scope"])
        none_cut = parse_planner_output('{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"서')
        self.assertEqual(none_cut["memory_requirement"], "none")
        self.assertEqual(none_cut["tool_plan"], [])
        self.assertFalse(needs_local_rag(none_cut["tool_plan"], none_cut["memory_requirement"]))
        # a value that is not the enum is not salvaged
        self.assertIsNone(parse_planner_output('{"memory_requirement":"maybe","tool_plan":[')["memory_requirement"])


if __name__ == "__main__":
    unittest.main()


class ContractParityTests(unittest.TestCase):
    def test_planner_enums_match_contract(self) -> None:
        import json
        from bot_plan import MEMORY_REQUIREMENTS, QUESTION_KINDS, TIME_SCOPES
        from tool_parity import _repo_root

        spec = json.loads((_repo_root() / "contracts" / "sandbox" / "tool-parity.json").read_text(encoding="utf-8"))["planner_output"]
        self.assertEqual(list(MEMORY_REQUIREMENTS), spec["memory_requirement"])
        self.assertEqual(list(TIME_SCOPES), spec["time_scope"])
        self.assertEqual(list(QUESTION_KINDS), spec["question_kind"])
