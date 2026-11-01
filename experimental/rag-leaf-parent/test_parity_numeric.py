"""숫자 진단과 동점 정렬 회귀(합성 데이터만 사용)."""
import json
import threading
import unittest
from types import SimpleNamespace

import numpy as np
import engine as E
from parity_numeric import platform_metadata, simulate_dot, packing_variants


class ParityNumericTests(unittest.TestCase):
    def test_exact_dense_ties_keep_row_order(self):
        ids = [str(i) for i in range(40)]
        vectors = np.tile(np.array([[1.0, 0.0]], dtype=np.float32), (40, 1))
        q = np.array([1.0, 0.0], dtype=np.float32)
        allowed = set(ids[::2])
        for scope in (None, allowed):
            expected = [i for i in ids if scope is None or i in scope]
            self.assertEqual([i for i, _ in E._dense_hits(vectors, ids, q, allowed_ids=scope, limit=40)], expected)
            fts = SimpleNamespace(search=lambda *a, **kw: [])
            result = E._hybrid_fusion(fts=fts, vectors=vectors, ids=ids, query="",
                                     q_vec=q, weights=[1, 4], lock=threading.Lock(),
                                     allowed_ids=scope, dense_limit=40)
            self.assertEqual(result.ids(), expected)

    def test_metadata_contains_only_safe_fields(self):
        fake = SimpleNamespace(__version__="2", show_config=lambda **kw: {
            "Build Dependencies": {"blas": {"name": "openblas", "version": "1", "lib directory": "secret"}},
            "Compilers": {"path": "secret"}})
        data = platform_metadata(fake)
        self.assertEqual(data["blas"]["blas"], {"name": "openblas", "version": "1"})
        self.assertNotIn("secret", json.dumps(data))

    def test_dot_simulation_preserves_vectors(self):
        vectors = np.array([[1, 2, 3, 4], [4, 3, 2, 1]], dtype=np.float32)
        q = np.array([1, -1, 1, -1], dtype=np.float32)
        for method in ("float64", "reverse32", "blocks1", "blocks4", "ulp1"):
            index = SimpleNamespace(leaf_vectors=vectors.copy(), summary_vectors=None)
            simulate_dot(index, np, method)
            np.testing.assert_array_equal(index.leaf_vectors, vectors)
            np.testing.assert_allclose(index.leaf_vectors @ q, vectors @ q)

    def test_packing_variants_are_independently_repacked(self):
        segments = [{"doc_id": "a"}, {"doc_id": "b"}]
        hits = [{"doc_id": "a", "excerpt": "old a", "merged": []},
                {"doc_id": "b", "excerpt": "old b", "merged": []}]
        def pack(messages, ordered, **kwargs):
            return {"docs": [{"excerpt": str(i), "status": "complete", "visible_observation_ids": [s["doc_id"]]}
                             for i, s in enumerate(ordered)]}
        engine = SimpleNamespace(_parity_pack_call=(([], segments), {}), _parity_original_pack=pack,
                                 _context_only_date=lambda *args: "", _leaf_source_date=lambda *args: "2026-01-01")
        index = SimpleNamespace(leaf_by_id={s["doc_id"]: s for s in segments})
        scores = {"a": {"score": .1, "raw": {"leaf_dense": .7}},
                  "b": {"score": .09, "raw": {"leaf_dense": .7+1e-8}}}
        variants = packing_variants(engine, index, hits, scores)
        self.assertEqual([[h["doc_id"] for h in v] for v in variants], [["b", "a"]])
        self.assertEqual([h["excerpt"] for h in variants[0]], ["0", "1"])
        self.assertEqual(hits[0]["excerpt"], "old a")
        scores["b"]["raw"]["leaf_dense"] = .4
        self.assertEqual(packing_variants(engine, index, hits, scores), [])


if __name__ == "__main__":
    unittest.main()
