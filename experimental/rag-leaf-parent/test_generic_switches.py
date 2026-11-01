"""Corpus-independent switches (2026-09-23 generalization audit).

- `intent_fallback: none` → no Korean keyword regex decides intent when the planner is absent.
- `retrieval.person.identity: observed` → aliases come from labels one native account used,
  not from a hand-written `person_aliases` table; label-only (Kakao) speakers stay unlinked.
- `vector_dims` → Matryoshka truncation of stored and query vectors, re-normalised.
"""

from __future__ import annotations

import unittest
from types import SimpleNamespace

import numpy as np

import engine


def _index(cfg: dict, messages: list[dict], leaves: list[dict]) -> SimpleNamespace:
    return SimpleNamespace(cfg=cfg, messages=messages, leaves=leaves)


class IntentFallbackTests(unittest.TestCase):
    def test_default_is_neutral_and_regex_is_opt_in(self) -> None:
        self.assertEqual(engine.resolve_intent("요즘 뭐해", None, {})["source"], "none")
        out = engine.resolve_intent("요즘 뭐해", None, {"intent_fallback": "regex"})
        self.assertEqual(out["source"], "regex")
        self.assertTrue(out["recent"])

    def test_none_ignores_keywords_and_alias_matching(self) -> None:
        out = engine.resolve_intent("요즘 정치 성향 어때", None, {"intent_fallback": "none"})
        self.assertEqual(out["source"], "none")
        self.assertFalse(out["recent"] or out["politics"] or out["intro"])
        self.assertEqual(out["targets"], [])  # [] = no regex alias lookup on the raw query

    def test_planner_intent_still_wins(self) -> None:
        plan = {"recent": True, "targets": ["a"], "source": "planner"}
        out = engine.resolve_intent("x", plan, {"intent_fallback": "none"})
        self.assertEqual(out["source"], "planner")
        self.assertTrue(out["recent"])


class ObservedIdentityTests(unittest.TestCase):
    def setUp(self) -> None:
        messages = [
            {"actor_account": "111", "actor_label": "alpha", "source_room_id": "discord:1:2"},
            {"actor_account": "111", "actor_label": "alpha-old", "source_room_id": "discord:1:2"},
            {"actor_account": None, "actor_label": "kakao-kim", "source_room_id": "kakao:r"},
            {"actor_account": None, "actor_label": "kakao-kim2", "source_room_id": "kakao:r"},
        ]
        leaves = [{"doc_id": "L0", "start": 0, "end": 2}, {"doc_id": "L1", "start": 2, "end": 4}]
        self.cfg = {"person_aliases": {"hand": ["written"]}, "retrieval": {"person": {"identity": "observed"}}}
        self.index = _index(self.cfg, messages, leaves)

    def test_observed_links_only_native_account_labels(self) -> None:
        table = engine._alias_table_for(self.index)
        self.assertEqual(table["alpha"], {"alpha", "alpha-old"})
        self.assertNotIn("kakao-kim", table)  # label-only speakers are never linked
        self.assertNotIn("hand", table)  # hand table ignored in observed mode

    def test_default_is_observed(self) -> None:
        self.index.cfg = {"person_aliases": {"hand": ["written"]}}
        self.assertNotIn("hand", engine._alias_table_for(self.index))

    def test_config_mode_keeps_hand_table(self) -> None:
        self.index.cfg = {"person_aliases": {"hand": ["written"]}, "retrieval": {"person": {"identity": "config"}}}
        self.assertEqual(engine._alias_table_for(self.index), {"hand": {"hand", "written"}})

    def test_both_merges(self) -> None:
        self.index.cfg = dict(self.cfg, retrieval={"person": {"identity": "both"}})
        table = engine._alias_table_for(self.index)
        self.assertIn("hand", table)
        self.assertIn("alpha", table)


class VectorDimsTests(unittest.TestCase):
    def test_truncate_and_renormalise(self) -> None:
        rng = np.random.default_rng(0)
        vecs = rng.normal(size=(5, 16)).astype(np.float32)
        vecs /= np.linalg.norm(vecs, axis=1, keepdims=True)
        idx = SimpleNamespace(leaf_vectors=vecs, summary_vectors=None, baseline_vectors=np.zeros((0, 16), np.float32))
        engine.apply_vector_dims(idx, 4)
        self.assertEqual(idx.leaf_vectors.shape, (5, 4))
        np.testing.assert_allclose(np.linalg.norm(idx.leaf_vectors, axis=1), 1.0, rtol=1e-5)
        q = engine.fit_query_vector(idx, rng.normal(size=16).astype(np.float32))
        self.assertEqual(q.shape, (4,))
        self.assertAlmostEqual(float(np.linalg.norm(q)), 1.0, places=5)

    def test_zero_dims_is_noop(self) -> None:
        vecs = np.eye(3, dtype=np.float32)
        idx = SimpleNamespace(leaf_vectors=vecs, summary_vectors=None, baseline_vectors=None)
        engine.apply_vector_dims(idx, 0)
        self.assertIs(idx.leaf_vectors, vecs)


if __name__ == "__main__":
    unittest.main()
