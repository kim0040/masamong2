import json
import unittest

import engine


class ToolBlockTest(unittest.TestCase):
    """The answer prompt's tool block stays valid JSON when clipped (same rule as Go chat.toolJSON)."""

    def test_long_result_is_clipped_per_tool(self) -> None:
        long = "검색 결과 본문 \"인용\" " * 400
        usable = [
            {"tool": "search.web", "ok": True, "status": "ok", "data": {"results": long}},
            {"tool": "weather.query", "ok": True, "status": "ok", "data": {"temp_c": 21}},
        ]
        other = [{"tool": "images.generate", "ok": False, "status": "failed", "error": "quota"}]
        out = engine._tool_block(usable, other)
        self.assertLessEqual(len(out), engine.TOOL_BLOCK_MAX)
        got = json.loads(out)
        self.assertEqual(len(got["usable"]), 2)
        self.assertTrue(got["usable"][0]["truncated"])
        self.assertTrue(got["usable"][0]["data"]["results"].endswith("…"))
        self.assertEqual(got["usable"][1]["data"], {"temp_c": 21})
        self.assertEqual(got["not_success"][0]["error"], "quota")

    def test_first_hits_stay_whole(self) -> None:
        items = [{"title": "기사", "snippet": chr(ord("가") + i) * 700} for i in range(5)]
        usable = [{"tool": "search.web", "ok": True, "status": "ok", "data": {"results": items, "answer": "요약"}},
                  {"tool": "weather.query", "ok": True, "status": "ok", "data": {"temp_c": 18}}]
        got = json.loads(engine._tool_block(usable, []))
        res = got["usable"][0]["data"]["results"]
        self.assertEqual(res[0]["snippet"], "가" * 700)
        self.assertTrue(res[-1]["snippet"].endswith("…"))
        self.assertEqual(got["usable"][0]["data"]["answer"], "요약")
        self.assertEqual(got["usable"][1]["data"], {"temp_c": 18})

    def test_small_block_is_unchanged(self) -> None:
        usable = [{"tool": "weather.query", "ok": True, "status": "ok", "data": {"temp_c": 21}}]
        self.assertEqual(engine._tool_block(usable, []), json.dumps({"usable": usable, "not_success": []}, ensure_ascii=False))


if __name__ == "__main__":
    unittest.main()
