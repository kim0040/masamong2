"""Fingerprint reuse: existing artifacts are compared before writing the new recipe."""

from __future__ import annotations

import unittest

import index_artifacts as ia


class DecideReuseTests(unittest.TestCase):
    def test_leaf_size_change_does_not_reuse_old_segments(self) -> None:
        cfg4 = {"collection": "c", "rooms": ["r"], "leaf_size": 4, "leaf_overlap": 0, "leaf_limit": 10, "lexical": "bigram", "document_char_limit": 4000}
        cfg2 = dict(cfg4, leaf_size=2)
        layers4 = ia.wanted_layers(cfg4)
        layers2 = ia.wanted_layers(cfg2)
        stored = {"layers": layers4, "digest": ia.layers_digest(layers4)}
        plan = ia.decide_reuse(stored, layers2, files_ready=True)
        self.assertFalse(plan.segments)
        self.assertEqual(plan.reason, "segmentation_changed")

    def test_match_reuses_all_layers(self) -> None:
        cfg = {"collection": "c", "rooms": ["r"], "leaf_size": 4, "lexical": "bigram", "document_char_limit": 100}
        layers = ia.wanted_layers(cfg)
        stored = {"layers": layers, "digest": ia.layers_digest(layers)}
        plan = ia.decide_reuse(stored, layers, files_ready=True)
        self.assertTrue(plan.segments and plan.summaries and plan.embeddings)
        self.assertEqual(plan.reason, "match")

    def test_missing_manifest_never_authenticates_files(self) -> None:
        cfg = {"collection": "c", "rooms": ["r"], "leaf_size": 4, "lexical": "bigram"}
        plan = ia.decide_reuse(None, ia.wanted_layers(cfg), files_ready=True)
        self.assertFalse(plan.segments)
        self.assertEqual(plan.reason, "missing_manifest_or_files")

    def test_weight_only_change_keeps_embedding_layer(self) -> None:
        cfg = {"collection": "c", "rooms": ["r"], "leaf_size": 4, "lexical": "bigram", "document_char_limit": 100, "rrf_weights": [1, 4]}
        other = dict(cfg, rrf_weights=[1, 8], top_k=9)
        layers = ia.wanted_layers(cfg)
        stored = {"layers": layers, "digest": ia.layers_digest(layers)}
        plan = ia.decide_reuse(stored, ia.wanted_layers(other), files_ready=True)
        self.assertTrue(plan.segments)
        self.assertTrue(plan.embeddings)

    def test_writing_wanted_first_is_not_used_as_evidence(self) -> None:
        """The F1 bug: a checkpoint already containing the *new* recipe must not
        certify old leaves. decide_reuse looks at stored layers, not the caller's wanted."""
        old = ia.wanted_layers({"collection": "c", "rooms": ["r"], "leaf_size": 4, "lexical": "x"})
        new = ia.wanted_layers({"collection": "c", "rooms": ["r"], "leaf_size": 2, "lexical": "x"})
        poisoned_ckpt = {"fingerprint": new, "layers": old, "digest": ia.layers_digest(old)}
        plan = ia.decide_reuse(poisoned_ckpt, new, files_ready=True)
        self.assertFalse(plan.segments)

    def test_leaf_spans_reject_old_size_four_under_size_two(self) -> None:
        leaves = [{"start": 0, "end": 4}, {"start": 4, "end": 8}]
        self.assertTrue(ia.segmentation_matches_leaves(leaves, {"leaf_size": 4, "leaf_overlap": 0}))
        self.assertFalse(ia.segmentation_matches_leaves(leaves, {"leaf_size": 2, "leaf_overlap": 0}))


if __name__ == "__main__":
    unittest.main()
