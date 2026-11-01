"""Offline tool-adapter contract tests (no HTTP; any real network call fails the test)."""

from __future__ import annotations

import io
import json
import tempfile
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest import mock

from bot_tools import (
    OK_STATUSES,
    TOOL_STATUSES,
    disabled_result,
    error_result_from_exception,
    execute_named_tool,
    is_placeholder,
    is_success,
    make_result,
    resolve_region,
)

SECRET = "tok_sh0uld_n0t_leak"


def _network_guard(*args, **kwargs):
    raise AssertionError("unexpected network call from tool adapter")


class NoNetworkCase(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch.object(urllib.request, "urlopen", _network_guard)
        patcher.start()
        self.addCleanup(patcher.stop)


def _geo_then_forecast(url: str, **_: object):
    if "geocoding" in url:
        return {"results": [{"name": "전주", "country": "KR", "latitude": 35.8, "longitude": 127.1}]}
    return {
        "current": {"time": "2026-09-21T10:00", "temperature_2m": 21.5, "apparent_temperature": 20.0, "weather_code": 1},
        "daily": {"temperature_2m_max": [25, 24, 23], "temperature_2m_min": [15, 14, 13], "precipitation_probability_max": [10, 20, 30]},
    }


class EnvelopeTests(NoNetworkCase):
    def test_ok_only_for_ok_and_empty(self) -> None:
        for status in TOOL_STATUSES:
            res = make_result("x.y", status, data={"a": 1})
            self.assertEqual(res["ok"], status in OK_STATUSES, status)
            if res["ok"]:
                self.assertIn("data", res)
                self.assertNotIn("error", res)
                self.assertTrue(is_success(res))
            else:
                self.assertNotIn("data", res)
                self.assertIn("error", res)
                self.assertIn("error_kind", res)
                self.assertFalse(is_success(res))
        with self.assertRaises(ValueError):
            make_result("x.y", "bogus")

    def test_disabled_shape_matches_engine_gate(self) -> None:
        res = disabled_result("weather.query")
        self.assertEqual(res["tool"], "weather.query")
        self.assertFalse(res["ok"])
        self.assertEqual(res["error"], "disabled")
        self.assertEqual(res["status"], "disabled")

    def test_unknown_and_deferred_are_not_success(self) -> None:
        self.assertEqual(execute_named_tool("nope.tool", {})["status"], "unknown_tool")
        mem = execute_named_tool("memory.search", {})
        self.assertEqual((mem["ok"], mem["status"], mem["deferred"]), (False, "deferred", "engine_rag"))
        img = execute_named_tool("images.generate", {"prompt": "x"})
        self.assertEqual((img["ok"], img["status"], img["failure_kind"]), (False, "skipped", "skipped"))


class ProviderExceptionTests(NoNetworkCase):
    def test_kinds_without_message_body(self) -> None:
        cases = [
            (urllib.error.HTTPError("https://x/?key=" + SECRET, 503, "Service Unavailable " + SECRET, {}, io.BytesIO(b"")), "http_error", 503),
            (urllib.error.URLError(TimeoutError("timed out " + SECRET)), "timeout", None),
            (urllib.error.URLError("nodename " + SECRET), "url_error", None),
            (TimeoutError(SECRET), "timeout", None),
            (json.JSONDecodeError("bad " + SECRET, SECRET, 0), "invalid_response", None),
            (KeyError(SECRET), "invalid_response", None),
            (ValueError(SECRET), "invalid_param", None),
            (OSError(SECRET), "io_error", None),
        ]
        for exc, kind, http_status in cases:
            res = error_result_from_exception("finance.quote", exc)
            self.assertEqual(res["status"], "error", kind)
            self.assertEqual(res["error_kind"], kind, type(exc).__name__)
            self.assertFalse(res["ok"])
            self.assertEqual(res.get("http_status"), http_status)
            self.assertNotIn(SECRET, json.dumps(res, ensure_ascii=False))

    def test_timeout_vs_http_vs_empty_are_distinct(self) -> None:
        def timeout(url, **_):
            raise urllib.error.URLError(TimeoutError("timed out"))

        def http404(url, **_):
            raise urllib.error.HTTPError(url, 404, "Not Found", {}, io.BytesIO(b""))

        t = execute_named_tool("finance.quote", {"symbol": "AAPL"}, providers={"json": timeout})
        h = execute_named_tool("finance.quote", {"symbol": "AAPL"}, providers={"json": http404})
        e = execute_named_tool("search.web", {"query": "zzz"}, providers={"text": lambda url, **_: "<html></html>"})
        self.assertEqual((t["ok"], t["status"], t["error_kind"]), (False, "error", "timeout"))
        self.assertEqual((h["ok"], h["status"], h["error_kind"], h["http_status"]), (False, "error", "http_error", 404))
        self.assertEqual((e["ok"], e["status"], e["data"]["results"]), (True, "empty", []))
        self.assertNotIn("error", e)

    def test_invalid_provider_payload_is_error_not_ok(self) -> None:
        res = execute_named_tool("finance.quote", {"symbol": "AAPL"}, providers={"json": lambda url, **_: "not a dict"})
        self.assertEqual((res["ok"], res["status"]), (False, "error"))
        res = execute_named_tool("weather.query", {"region": "전주"}, providers={"json": lambda url, **_: {"results": [{"name": "전주"}]}})
        self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "invalid_response"))


class WeatherTests(NoNetworkCase):
    def test_placeholder_without_default_needs_input_and_no_network(self) -> None:
        for region in ("사용자 위치", "{region}", "", "지역", None):
            res = execute_named_tool("weather.query", {"region": region})
            self.assertEqual((res["ok"], res["status"], res["missing"]), (False, "needs_input", ["location"]), region)
        # no region key at all (planner omitted it)
        res = execute_named_tool("weather.query", {})
        self.assertEqual(res["status"], "needs_input")

    def test_configured_default_is_used_with_source(self) -> None:
        res = execute_named_tool(
            "weather.query",
            {"region": "현재 위치"},
            tool_defaults={"weather.query": {"region": "전주", "source": "channel.tool_defaults"}},
            providers={"json": _geo_then_forecast},
        )
        self.assertTrue(res["ok"])
        self.assertEqual(res["data"]["region_source"], "channel.tool_defaults")
        self.assertEqual(res["data"]["region"], "전주")
        self.assertEqual(res["data"]["observed_at"], "2026-09-21T10:00:00+09:00")

    def test_explicit_region_wins_over_default(self) -> None:
        self.assertEqual(resolve_region("부산", tool_defaults={"region": "전주", "source": "x"}), ("부산", "argument"))
        self.assertEqual(resolve_region("{city}", tool_defaults={"region": "전주"}), ("전주", "configured_default"))
        self.assertEqual(resolve_region("{city}", tool_defaults={"region": "사용자 위치"}), ("", ""))
        self.assertEqual(resolve_region("{city}", tool_defaults=None), ("", ""))

    def test_geocode_miss_is_not_found_not_ok(self) -> None:
        res = execute_named_tool("weather.query", {"region": "없는동네xyz"}, providers={"json": lambda url, **_: {"results": []}})
        self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "not_found"))

    def test_bad_day_offset(self) -> None:
        res = execute_named_tool("weather.query", {"region": "전주", "day_offset": "내일"}, providers={"json": _geo_then_forecast})
        self.assertEqual((res["status"], res["error_kind"]), ("needs_input", "invalid_param"))
        res = execute_named_tool("weather.query", {"region": "전주", "day_offset": 1}, providers={"json": _geo_then_forecast})
        self.assertEqual(res["data"]["forecast"]["tmax"], 24)


class DisastersTests(NoNetworkCase):
    def test_unconfigured_is_not_success(self) -> None:
        res = execute_named_tool("disasters.query", {})
        self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "unconfigured", "provider_unconfigured"))
        self.assertNotIn("data", res)

    def test_fake_provider_empty_vs_alerts(self) -> None:
        empty = execute_named_tool("disasters.query", {}, providers={"disasters": lambda: {"earthquakes": [], "warnings": []}})
        self.assertEqual((empty["ok"], empty["status"]), (True, "empty"))
        self.assertEqual(empty["data"]["note"], "no_active_alert_not_safety_claim")
        hit = execute_named_tool("disasters.query", {}, providers={"disasters": lambda: {"warnings": [{"kind": "heavy_rain"}]}})
        self.assertEqual((hit["ok"], hit["status"], len(hit["data"]["warnings"])), (True, "ok", 1))


class FinanceTests(NoNetworkCase):
    def test_empty_or_placeholder_symbol_needs_input(self) -> None:
        for sym in ("", "  ", "{symbol}", "티커", "symbol", None):
            res = execute_named_tool("finance.quote", {"symbol": sym})
            self.assertEqual((res["ok"], res["status"], res["missing"]), (False, "needs_input", ["symbol"]), sym)

    def test_quote_ok_and_unknown_symbol(self) -> None:
        good = {"chart": {"result": [{"meta": {"symbol": "AAPL", "regularMarketPrice": 190.5, "currency": "USD", "regularMarketTime": 1758400000, "exchangeName": "NMS"}}]}}
        res = execute_named_tool("finance.quote", {"symbol": "aapl"}, providers={"json": lambda url, **_: good})
        self.assertEqual((res["ok"], res["status"], res["data"]["price"]), (True, "ok", 190.5))
        res = execute_named_tool("finance.quote", {"symbol": "ZZZZ"}, providers={"json": lambda url, **_: {"chart": {"result": None, "error": {"code": "Not Found"}}}})
        self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "not_found"))

    def test_fx_provider_error_and_bad_base(self) -> None:
        res = execute_named_tool("finance.fx", {"base": "XXX"}, providers={"json": lambda url, **_: {"result": "error", "error-type": "unsupported-code " + SECRET}})
        self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "provider_error"))
        self.assertNotIn(SECRET, json.dumps(res))
        res = execute_named_tool("finance.fx", {"base": "{base}"})
        self.assertEqual(res["status"], "needs_input")
        res = execute_named_tool("finance.fx", {}, providers={"json": lambda url, **_: {"result": "success", "base_code": "USD", "rates": {"KRW": 1390.1}, "time_last_update_utc": "x"}})
        self.assertEqual((res["ok"], res["data"]["rates"]["KRW"]), (True, 1390.1))


class SearchTests(NoNetworkCase):
    def test_empty_query_needs_input(self) -> None:
        for q in ("", "{query}", "query"):
            res = execute_named_tool("search.web", {"query": q})
            self.assertEqual(res["status"], "needs_input", q)

    def test_results_parsed(self) -> None:
        html = '<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fa">Example</a><a class="result__snippet">snip</a>'
        res = execute_named_tool("search.web", {"query": "example"}, providers={"text": lambda url, **_: html})
        self.assertEqual((res["ok"], res["status"]), (True, "ok"))
        self.assertEqual(res["data"]["results"][0]["url"], "https://example.org/a")


class PollsContractTests(NoNetworkCase):
    def _run(self, params: dict, td: str) -> dict:
        return execute_named_tool("polls.create", params, data_dir=Path(td))

    def _labels(self, params: dict) -> list[str]:
        with tempfile.TemporaryDirectory() as td:
            res = self._run(params, td)
        self.assertTrue(res["ok"], res)
        return [a["poll_media"]["text"] for a in res["data"]["poll"]["answers"]]

    def test_options_key(self) -> None:
        self.assertEqual(self._labels({"question": "점심?", "options": ["김밥", "라면"]}), ["김밥", "라면"])

    def test_answers_key_from_llm_plan(self) -> None:
        self.assertEqual(self._labels({"question": "점심?", "answers": ["김밥", "라면", "초밥"]}), ["김밥", "라면", "초밥"])

    def test_empty_defaults_to_yes_no(self) -> None:
        self.assertEqual(self._labels({"question": "갈래?"}), ["예", "아니요"])

    def test_invalid_poll_writes_nothing(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            store = Path(td) / "sandbox_polls.json"
            for params, kind in (
                ({"question": ""}, "missing_param"),
                ({"question": "{question}"}, "missing_param"),
                ({"question": "x" * 301}, "invalid_param"),
                ({"question": "하나만?", "options": ["only"]}, "invalid_param"),
                ({"question": "갈래?", "duration_hours": "내일"}, "invalid_param"),
            ):
                res = self._run(params, td)
                self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "needs_input", kind), params)
                self.assertFalse(store.exists(), params)

    def test_corrupt_store_is_not_overwritten(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            store = Path(td) / "sandbox_polls.json"
            store.write_text("{corrupt", encoding="utf-8")
            res = self._run({"question": "점심?", "options": ["a", "b"]}, td)
            self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "store_corrupt"))
            self.assertEqual(store.read_text(encoding="utf-8"), "{corrupt")

    def test_write_failure_saves_nothing(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            store = Path(td) / "sandbox_polls.json"
            with mock.patch.object(Path, "write_text", side_effect=OSError("disk full")):
                res = self._run({"question": "점심?", "options": ["a", "b"]}, td)
            self.assertEqual((res["ok"], res["status"], res["error_kind"]), (False, "error", "io_error"))
            self.assertFalse(store.exists())
            self.assertFalse(store.with_suffix(".json.tmp").exists())


class PlaceholderTests(unittest.TestCase):
    def test_placeholders(self) -> None:
        for v in ("사용자 지역", "현재 위치", "{region}", "user location", "", None, "지역", "unknown"):
            self.assertTrue(is_placeholder(v), v)

    def test_real_regions(self) -> None:
        for v in ("서울", "전주", "부산 해운대", "Seoul", "New York"):
            self.assertFalse(is_placeholder(v), v)


if __name__ == "__main__":
    unittest.main()
