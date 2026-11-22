"""The engine's paths and prepare state live in one context object, not in rebindable module globals."""

from __future__ import annotations

import os
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

import engine as E
import engine_index as EI
import sandbox_env
from engine_context import EngineContext, SandboxPaths


class SandboxPathsTests(unittest.TestCase):
    def test_every_artifact_sits_under_the_data_dir(self) -> None:
        paths = SandboxPaths.under("/synthetic/data")
        self.assertEqual(paths.data, Path("/synthetic/data"))
        self.assertEqual(paths.ckpt_state, Path("/synthetic/data/ckpt/state.json"))
        for name in ("messages", "leaves", "baseline", "leaf_vecs", "baseline_vecs", "summaries", "summary_vecs",
                     "meta", "ledger", "ckpt_dir", "ckpt_state", "progress"):
            self.assertIn(paths.data, getattr(paths, name).parents, name)

    def test_the_paths_are_frozen(self) -> None:
        with self.assertRaises(Exception):
            SandboxPaths.under("/synthetic/data").data = Path("/elsewhere")  # type: ignore[misc]


class ContextTests(unittest.TestCase):
    def test_rebinding_swaps_the_whole_set_and_the_old_names_follow(self) -> None:
        ctx = EI.context()
        before = ctx.paths
        with tempfile.TemporaryDirectory() as tmp:
            try:
                E._bind_data_dir(Path(tmp))
                self.assertIs(EI.context(), ctx)  # same context, new paths
                self.assertEqual(ctx.paths.leaves, Path(tmp) / "leaves.json")
                # the names the old engine exported are read from the context, never copied
                self.assertEqual(E.DATA, Path(tmp))
                self.assertEqual(E.LEDGER_PATH, Path(tmp) / "session_ledger.json")
                self.assertEqual(E.CKPT_STATE_PATH, Path(tmp) / "ckpt" / "state.json")
            finally:
                ctx.paths = before
        self.assertEqual(E.DATA, before.data)

    def test_prepare_state_is_read_through_the_old_names(self) -> None:
        ctx = EI.context()
        saved = (ctx.prepare.summary_on, ctx.prepare.cost_usd)
        try:
            ctx.prepare.summary_on, ctx.prepare.cost_usd = False, 0.25
            self.assertFalse(E._prepare_summary_on)
            self.assertEqual(E._prepare_cost_usd, 0.25)
        finally:
            ctx.prepare.summary_on, ctx.prepare.cost_usd = saved

    def test_an_unknown_name_is_still_an_attribute_error(self) -> None:
        with self.assertRaises(AttributeError):
            E.not_a_name  # noqa: B018

    def test_reading_the_ledger_waits_for_a_writer(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = EI.context()
            before = ctx.paths
            ctx.paths = SandboxPaths.under(tmp)
            try:
                done = threading.Event()
                with ctx.ledger_lock:
                    reader = threading.Thread(target=lambda: (E.load_ledger(), done.set()), daemon=True)
                    reader.start()
                    self.assertFalse(done.wait(0.2), "load_ledger must wait while a writer holds the ledger lock")
                reader.join(2)
                self.assertTrue(done.is_set())
            finally:
                ctx.paths = before

    def test_two_contexts_do_not_share_state(self) -> None:
        a, b = EngineContext(SandboxPaths.under("/a")), EngineContext(SandboxPaths.under("/b"))
        a.prepare.cost_usd = 1.0
        self.assertIsNone(b.prepare.cost_usd)
        self.assertIsNot(a.ledger_lock, b.ledger_lock)


class SandboxEnvTests(unittest.TestCase):
    def test_ensure_sets_defaults_without_overriding_and_puts_eval_first(self) -> None:
        with mock.patch.dict(os.environ, {"EVAL_SECRETS": "/synthetic/secrets.env"}), mock.patch.object(sys, "path", ["/x"]):
            os.environ.pop("RAW_CORPUS_ROOT", None)
            sandbox_env.ensure()
            self.assertEqual(os.environ["EVAL_SECRETS"], "/synthetic/secrets.env")
            self.assertTrue(os.environ["RAW_CORPUS_ROOT"].endswith("raw-corpus"))
            self.assertEqual(sys.path[0], str(sandbox_env.EVAL))


if __name__ == "__main__":
    unittest.main()


class LayeringTests(unittest.TestCase):
    """The engine modules import only downward, so the experiments cannot leak into the ported path by accident."""

    LAYERS = ["engine_context", "sandbox_env", "engine_index", "engine_query", "engine_experiments", "engine_retrieve", "engine_turn"]

    def test_modules_import_only_lower_layers(self) -> None:
        import ast

        here = Path(__file__).resolve().parent
        for rank, name in enumerate(self.LAYERS):
            tree = ast.parse((here / f"{name}.py").read_text(encoding="utf-8"))
            for node in ast.walk(tree):
                mods = [node.module] if isinstance(node, ast.ImportFrom) and node.module else []
                mods += [a.name for a in node.names] if isinstance(node, ast.Import) else []
                for mod in mods:
                    if mod in self.LAYERS:
                        self.assertLess(self.LAYERS.index(mod), rank, f"{name} imports {mod}")

    def test_the_ported_path_reaches_the_experiments_only_through_two_calls(self) -> None:
        import ast

        here = Path(__file__).resolve().parent
        tree = ast.parse((here / "engine_retrieve.py").read_text(encoding="utf-8"))
        imported = {
            alias.name
            for node in ast.walk(tree)
            if isinstance(node, ast.ImportFrom) and node.module == "engine_experiments"
            for alias in node.names
        }
        self.assertEqual(imported, {"_rerank_leaves", "_soft_event_candidates", "_soft_event_recipe"})
