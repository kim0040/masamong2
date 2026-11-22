"""prepare_index reuse policy: on-disk artifacts are trusted only when the stored
fingerprint, the manifest digest and the leaf spans all match the wanted recipe.
Synthetic corpus only; nothing here embeds, calls a model or touches the network."""

from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path

import numpy as np

import engine as E
from index_artifacts import MANIFEST_NAME, wanted_layers, write_manifest

CFG = {
    "experiment": "synthetic-reuse",
    "collection": "synthetic",
    "rooms": ["synthetic:room:a"],
    "message_limit": 50,
    "leaf_size": 8,
    "leaf_overlap": 0,
    "leaf_limit": 100,
    "leaf_split": "fixed",
    "gap_minutes": 30,
    "context_prefix": True,
    "summary_enabled": False,
    "baseline_enabled": False,
    "lexical": "bigram",
    "document_char_limit": 4000,
    "data_dir": "",
    "models": {"embed": "qwen/qwen3-embedding-8b"},
}

MESSAGES = [
    {
        "observation_id": f"synthetic:room:a:{i}",
        "source_room_id": "synthetic:room:a",
        "source_room_original": "synthetic:room:a",
        "actor_label": "가명",
        "actor_account": None,
        "content": f"합성 메시지 {i}",
        "parsed_time": f"2026-01-01T00:{i:02d}:00+00:00",
        "index": i,
    }
    for i in range(8)
]
LEAVES = [{"doc_id": "leaf-0", "start": 0, "end": 8, "body": "합성 leaf", "source_room_id": "synthetic:room:a"}]


class PrepareIndexTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.data = Path(self.tmp.name)
        self.cfg = dict(CFG, data_dir=str(self.data))
        self.prev_cfg_fn = E.load_config
        self.prev_data = E.DATA
        self.prev_env = {k: os.environ.get(k) for k in ("MASAMONG_SANDBOX_DATA", "MASAMONG_SANDBOX_CONFIG")}
        os.environ.pop("MASAMONG_SANDBOX_DATA", None)
        os.environ.pop("MASAMONG_SANDBOX_CONFIG", None)
        E.load_config = lambda: dict(self.cfg)

    def tearDown(self) -> None:
        E.load_config = self.prev_cfg_fn
        E._bind_data_dir(self.prev_data)
        for k, v in self.prev_env.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        self.tmp.cleanup()

    def _write_artifacts(self, cfg: dict, *, manifest_layers: dict | None, leaves: list | None = None) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        (self.data / "meta.json").write_text(
            json.dumps({"index_fingerprint": E.index_fingerprint(cfg)}), encoding="utf-8"
        )
        (self.data / "messages.json").write_text(json.dumps(MESSAGES), encoding="utf-8")
        (self.data / "leaves.json").write_text(json.dumps(LEAVES if leaves is None else leaves), encoding="utf-8")
        np.save(self.data / "leaf_vecs.npy", np.ones((1, 4), dtype=np.float32))
        if manifest_layers is not None:
            write_manifest(self.data / MANIFEST_NAME, manifest_layers)

    def test_matching_artifacts_are_reused_without_work(self) -> None:
        self._write_artifacts(self.cfg, manifest_layers=wanted_layers(self.cfg))
        out = E.prepare_index()
        self.assertEqual(out["status"], "ready")
        self.assertTrue(out["reused"])
        self.assertFalse(out["resumed"])
        self.assertEqual(out["meta"]["index_fingerprint"], E.index_fingerprint(self.cfg))

    def test_missing_manifest_is_bootstrapped_not_rebuilt(self) -> None:
        self._write_artifacts(self.cfg, manifest_layers=None)
        out = E.prepare_index()
        self.assertEqual(out["status"], "ready")
        self.assertTrue(out["reused"])
        self.assertEqual(out.get("manifest"), "bootstrapped")
        manifest = json.loads((self.data / MANIFEST_NAME).read_text(encoding="utf-8"))
        self.assertEqual(manifest["status"], "bootstrapped")

    def test_changed_recipe_manifest_is_refused_not_silently_reindexed(self) -> None:
        stale = dict(self.cfg, leaf_size=4)
        self._write_artifacts(self.cfg, manifest_layers=wanted_layers(stale))
        with self.assertRaises(E.PlaygroundError) as ctx:
            E.prepare_index()
        self.assertIn("재사용 불가", str(ctx.exception))

    def test_leaves_wider_than_the_recipe_are_refused(self) -> None:
        wide = [{"doc_id": "leaf-0", "start": 0, "end": 20, "body": "너무 큰 leaf"}]
        self._write_artifacts(self.cfg, manifest_layers=wanted_layers(self.cfg), leaves=wide)
        with self.assertRaises(E.PlaygroundError) as ctx:
            E.prepare_index()
        self.assertIn("leaf 크기", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
