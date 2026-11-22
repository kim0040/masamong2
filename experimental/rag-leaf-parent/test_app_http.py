"""Sandbox HTTP layer (app.py, the playground — not the product console): request
size and JSON guards, the not-ready chat path, and /api/status over a synthetic
config in a temp data dir. No index, no model calls, no secrets."""

from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import app
import engine as E
import engine_index as EI
import engine_turn as ET

CFG = {
    "experiment": "synthetic-http",
    "collection": "synthetic",
    "rooms": ["synthetic:room:a"],
    "room_merge": {},
    "leaf_size": 8,
    "leaf_limit": 100,
    "message_limit": 50,
    "parent_expand_before": 2,
    "parent_expand_after": 2,
    "summary_enabled": False,
    "top_k": 5,
    "retrieve_pool": 40,
    "lexical_limit": 50,
    "dense_limit": 50,
    "ranking": "heuristic",
    "models": {
        "response": "z-ai/glm-5.3-flash",
        "embed": "qwen/qwen3-embedding-8b",
        "rerank": "voyageai/rerank-2.5-lite",
    },
    "rrf_weights": [1.0, 4.0],
    "lexical": "bigram",
    "session_budget_usd": 1.0,
    "data_dir": "",
}


class AppHttpTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.tmp = tempfile.TemporaryDirectory()
        cls.data = Path(cls.tmp.name)
        cls.cfg = dict(CFG, data_dir=str(cls.data))
        cls.prev_cfg_fn = EI.load_config
        cls.prev_data = EI.context().paths.data
        cls.prev_index = app.STATE.index
        cls.prev_env = {k: os.environ.get(k) for k in ("MASAMONG_SANDBOX_DATA", "MASAMONG_SANDBOX_CONFIG", "EVAL_SECRETS")}
        os.environ.pop("MASAMONG_SANDBOX_DATA", None)
        # the handlers that read the config through app.load_config() read this file, not the patched function
        (cls.data / "config.json").write_text(json.dumps(cls.cfg), encoding="utf-8")
        os.environ["MASAMONG_SANDBOX_CONFIG"] = str(cls.data / "config.json")
        # Keep secrets lookup inside the temp dir: the ambient EVAL_SECRETS path is never read.
        (cls.data / "secrets.env").write_text("OPENROUTER_API_KEY=synthetic\n", encoding="utf-8")
        os.environ["EVAL_SECRETS"] = str(cls.data / "secrets.env")
        EI.load_config = ET.load_config = lambda: dict(cls.cfg)  # status_payload lives in engine_turn
        E._bind_data_dir(cls.data)
        app.STATE.index = None
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), app.Handler)
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.thread.join(timeout=5)
        cls.server.server_close()
        EI.load_config = ET.load_config = cls.prev_cfg_fn
        E._bind_data_dir(cls.prev_data)
        app.STATE.index = cls.prev_index
        for k, v in cls.prev_env.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        cls.tmp.cleanup()

    def _get(self, path: str) -> tuple[int, dict]:
        with urllib.request.urlopen(self.base + path, timeout=10) as res:
            return res.status, json.loads(res.read().decode("utf-8"))

    def _post(self, path: str, body: bytes) -> tuple[int, dict]:
        req = urllib.request.Request(
            self.base + path, data=body, headers={"Content-Type": "application/json"}, method="POST"
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as res:
                return res.status, json.loads(res.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            return exc.code, json.loads(exc.read().decode("utf-8"))

    def test_status_reports_not_ready_over_the_synthetic_config(self) -> None:
        code, body = self._get("/api/status")
        self.assertEqual(code, 200)
        self.assertFalse(body["ready"])
        self.assertEqual(body["experiment"], "synthetic-http")
        self.assertEqual(body["config"]["collection"], "synthetic")
        self.assertEqual(body["config"]["rooms"], ["synthetic:room:a"])
        self.assertEqual(body["session_spent_usd"], 0.0)
        self.assertEqual(body["prepare"]["status"], "idle")

    def test_prepare_progress_is_idle_without_artifacts(self) -> None:
        code, body = self._get("/api/prepare/progress")
        self.assertEqual(code, 200)
        self.assertEqual(body["status"], "idle")

    def test_chat_is_refused_until_the_index_is_ready(self) -> None:
        code, body = self._post("/api/chat", b'{"message": "hi"}')
        self.assertEqual(code, 503)

    def test_oversized_body_is_refused_before_reading(self) -> None:
        code, body = self._post("/api/chat", b"x" * 100_001)
        self.assertEqual(code, 413)
        self.assertIn("error", body)

    def test_broken_json_is_a_400_not_a_500(self) -> None:
        code, body = self._post("/api/chat", b"{not json")
        self.assertEqual(code, 400)
        self.assertIn("error", body)


    def _get_status_code(self, path: str) -> int:
        try:
            with urllib.request.urlopen(self.base + path, timeout=10) as res:
                return res.status
        except urllib.error.HTTPError as exc:
            return exc.code

    def test_the_route_tables_name_every_endpoint(self) -> None:
        self.assertEqual(
            sorted(app.GET_ROUTES),
            sorted([
                "/api/status", "/api/prepare/progress", "/api/lab/inventory", "/api/lab/export", "/api/lab/config",
                "/api/debug/turns", "/api/debug/run", "/api/review/sets", "/api/review/items", "/api/debug/turn",
                "/api/blocks", "/api/bot", "/api/conversations",
            ]),
        )
        self.assertEqual(
            sorted(app.POST_ROUTES),
            sorted([
                "/api/chat", "/api/chat/stream", "/api/debug/flag", "/api/debug/client", "/api/review/verdict",
                "/api/lab/estimate", "/api/lab/config", "/api/lab/import", "/api/conversations", "/api/bot",
            ]),
        )

    def test_read_endpoints_answer_without_an_index(self) -> None:
        for path in ("/api/blocks", "/api/debug/turns", "/api/bot", "/api/conversations", "/api/lab/config"):
            code, body = self._get(path)
            self.assertEqual(code, 200, path)
            self.assertIsInstance(body, dict, path)
        self.assertEqual(self._get("/api/blocks")[1], {"blocks": []})
        self.assertEqual(self._get_status_code("/api/debug/turn?id=missing"), 404)

    def test_unknown_paths_are_404_after_the_body_is_checked(self) -> None:
        self.assertEqual(self._get_status_code("/api/nope"), 404)
        self.assertEqual(self._get_status_code("/../app.py"), 404)
        self.assertEqual(self._post("/api/nope", b"{}")[0], 404)
        self.assertEqual(self._post("/api/nope", b"{broken")[0], 400)

    def test_status_carries_the_room_catalog_state(self) -> None:
        code, body = self._get("/api/status")
        self.assertEqual(code, 200)
        self.assertEqual(body["room_catalog"], {"state": "idle", "error": ""})


class AppStateTests(unittest.TestCase):
    def test_the_catalog_thread_is_owned_and_a_failure_is_kept(self) -> None:
        state = app.AppState()
        with mock.patch.object(app, "room_catalog", side_effect=RuntimeError("synthetic scan failure")), \
                mock.patch("traceback.print_exc"):
            state.start_catalog_refresh()
            state.start_catalog_refresh()  # a second call does not start a second thread
            state.stop()
        status = state.catalog_status()
        self.assertEqual(status["state"], "error")
        self.assertIn("synthetic scan failure", status["error"])

    def test_a_stopped_state_does_not_start_the_scan(self) -> None:
        state = app.AppState()
        state.stop()
        with mock.patch.object(app, "room_catalog") as scan:
            state.start_catalog_refresh()
            state._catalog_thread.join(2)
        scan.assert_not_called()
        self.assertEqual(state.catalog_status()["state"], "idle")

    def test_a_finished_scan_is_ready(self) -> None:
        state = app.AppState()
        with mock.patch.object(app, "room_catalog", return_value={}):
            state.start_catalog_refresh()
            state._catalog_thread.join(2)
        self.assertEqual(state.catalog_status(), {"state": "ready", "error": ""})


if __name__ == "__main__":
    unittest.main()
