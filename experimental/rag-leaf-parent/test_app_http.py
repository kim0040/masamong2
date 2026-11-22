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
        cls.prev_data = EI.DATA
        cls.prev_index = app.INDEX
        cls.prev_env = {k: os.environ.get(k) for k in ("MASAMONG_SANDBOX_DATA", "MASAMONG_SANDBOX_CONFIG", "EVAL_SECRETS")}
        os.environ.pop("MASAMONG_SANDBOX_DATA", None)
        os.environ.pop("MASAMONG_SANDBOX_CONFIG", None)
        # Keep secrets lookup inside the temp dir: the ambient EVAL_SECRETS path is never read.
        (cls.data / "secrets.env").write_text("OPENROUTER_API_KEY=synthetic\n", encoding="utf-8")
        os.environ["EVAL_SECRETS"] = str(cls.data / "secrets.env")
        EI.load_config = ET.load_config = lambda: dict(cls.cfg)  # status_payload lives in engine_turn
        E._bind_data_dir(cls.data)
        app.INDEX = None
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
        app.INDEX = cls.prev_index
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


if __name__ == "__main__":
    unittest.main()
