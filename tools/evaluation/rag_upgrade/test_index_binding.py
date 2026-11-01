"""Tests for index↔gold binding (rejects stale/unbound gold)."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.index_binding import (  # noqa: E402
    BindingError,
    assert_gold_bound_to_index,
    compute_index_binding,
    segmentation_version_from_cfg,
    stable_leaf_id_from_playground_leaf,
    validate_question_wants_against_leaves,
)


class IndexBindingTests(unittest.TestCase):
    def _leaf(self, *, room="discord:1:2", start=0, end=8, oids=None, doc="leaf:0-8"):
        return {
            "doc_id": doc,
            "source_room_id": room,
            "start": start,
            "end": end,
            "observation_ids": oids or [f"obs-{start}", f"obs-{start+1}"],
            "body": "hello",
        }

    def test_stable_leaf_id_depends_on_span_not_obs_hash(self):
        seg = "leaf8-o0"
        a = stable_leaf_id_from_playground_leaf(
            self._leaf(oids=["aaa", "bbb"]), segmentation_version=seg
        )
        b = stable_leaf_id_from_playground_leaf(
            self._leaf(oids=["ccc", "ddd"]), segmentation_version=seg
        )
        self.assertEqual(a, b)
        c = stable_leaf_id_from_playground_leaf(
            self._leaf(start=8, end=16, doc="leaf:8-16"), segmentation_version=seg
        )
        self.assertNotEqual(a, c)

    def test_fingerprint_changes_when_obs_ids_change(self):
        cfg = {
            "leaf_size": 8,
            "leaf_overlap": 0,
            "leaf_limit": 100,
            "context_prefix": True,
            "lexical": "bigram",
            "rrf_weights": [1, 4],
            "rooms": ["discord:1:2"],
            "message_limit": 1000,
            "summary_enabled": True,
        }
        messages = [{"observation_id": "x", "content": "c"} for _ in range(16)]
        with tempfile.TemporaryDirectory() as td:
            vec = Path(td) / "leaf_vecs.npy"
            np.save(vec, np.zeros((2, 8), dtype=np.float32))
            b1 = compute_index_binding(
                leaves=[self._leaf(oids=["a1", "a2"]), self._leaf(start=8, end=16, doc="leaf:8-16", oids=["b1", "b2"])],
                messages=messages,
                leaf_vecs_path=vec,
                cfg=cfg,
            )
            b2 = compute_index_binding(
                leaves=[self._leaf(oids=["z1", "z2"]), self._leaf(start=8, end=16, doc="leaf:8-16", oids=["b1", "b2"])],
                messages=messages,
                leaf_vecs_path=vec,
                cfg=cfg,
            )
        self.assertNotEqual(b1["index_fingerprint"], b2["index_fingerprint"])
        self.assertNotEqual(b1["leaves_sha256"], b2["leaves_sha256"])
        self.assertIn("corpus_binding", b1)
        self.assertEqual(b1["corpus_binding"]["corpus_fingerprint"], b2["corpus_binding"]["corpus_fingerprint"])

    def test_corpus_binding_allows_index_mismatch(self):
        binding = {
            "index_fingerprint": "abc",
            "leaf_count": 1,
            "message_count": 1,
            "leaves_sha256": "x",
            "corpus_binding": {"corpus_fingerprint": "corp-1"},
        }
        gold = {
            "index_binding": {
                "index_fingerprint": "zzz",
                "leaf_count": 9,
                "message_count": 1,
                "leaves_sha256": "y",
            },
            "corpus_binding": {"corpus_fingerprint": "corp-1"},
        }
        assert_gold_bound_to_index(gold, binding)
        with self.assertRaises(BindingError):
            assert_gold_bound_to_index(
                {**gold, "corpus_binding": {"corpus_fingerprint": "other"}},
                binding,
            )

    def test_assert_rejects_unbound_and_mismatched_gold(self):
        binding = {
            "index_fingerprint": "abc",
            "leaf_count": 1,
            "message_count": 1,
            "leaves_sha256": "x",
        }
        with self.assertRaises(BindingError):
            assert_gold_bound_to_index({"questions": []}, binding)
        with self.assertRaises(BindingError):
            assert_gold_bound_to_index(
                {
                    "index_binding": {
                        "index_fingerprint": "zzz",
                        "leaf_count": 1,
                        "message_count": 1,
                        "leaves_sha256": "x",
                    }
                },
                binding,
            )

    def test_validate_wants_rejects_missing_ids(self):
        leaves = [self._leaf(oids=["keep-1", "keep-2"])]
        with self.assertRaises(BindingError):
            validate_question_wants_against_leaves(
                [{"question_id": "q1", "want_observation_ids": ["missing"]}],
                leaves,
            )
        validate_question_wants_against_leaves(
            [{"question_id": "q1", "want_observation_ids": ["keep-1"]}],
            leaves,
        )

    def test_segmentation_version_stable(self):
        self.assertIn(
            "leaf8",
            segmentation_version_from_cfg({"leaf_size": 8, "leaf_overlap": 0, "leaf_limit": 1}),
        )


if __name__ == "__main__":
    unittest.main()
