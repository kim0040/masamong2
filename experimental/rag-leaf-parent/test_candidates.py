"""Candidate fusion: order-identical to engine.weighted_rrf, provenance preserved, deterministic."""

from __future__ import annotations

import random
import unittest

import engine as E
from candidates import fuse, guarantee_pool, per_source_hit, pool_necessary_sources, source_family, unique_contribution
from candidates_assess import assess, signals_for
from test_engine_retrieval import ROOM_NEW, ROOM_OLD, _FakeClient, _FakeRerankResult, _unit, build_index


class FusionOrderTests(unittest.TestCase):
    def test_matches_weighted_rrf_on_random_runs(self) -> None:
        rng = random.Random(7)
        for _ in range(50):
            universe = [f"d{i}" for i in range(30)]
            runs = [rng.sample(universe, rng.randint(0, 25)) for _ in range(3)]
            weights = [rng.choice([0.5, 1.0, 4.0]) for _ in runs]
            want = E.weighted_rrf(runs, weights)
            got = fuse([(f"r{i}", ids, w) for i, (ids, w) in enumerate(zip(runs, weights))]).ids()
            self.assertEqual(got, want)

    def test_nested_matches_nested_weighted_rrf(self) -> None:
        a, b = ["x", "y", "z", "w"], ["y", "q", "x"]
        c, d = ["w", "q", "k"], ["k", "y"]
        inner1 = E.weighted_rrf([a, b], [1.0, 4.0])
        inner2 = E.weighted_rrf([c, d], [1.0, 4.0])
        want = E.weighted_rrf([inner1, inner2], [1.0, 0.7])
        f1 = fuse([("leaf_lexical", a, 1.0), ("leaf_dense", b, 4.0)])
        f2 = fuse([("summary_lexical", c, 1.0), ("summary_dense", d, 4.0)])
        outer = fuse([("leaf", f1.ids(), 1.0), ("summary", f2.ids(), 0.7)], children={"leaf": f1, "summary": f2})
        self.assertEqual(outer.ids(), want)
        # provenance is per retriever, not per aggregate
        y = outer.by_id["y"]
        self.assertEqual(y.sources, {"leaf_lexical": 2, "leaf_dense": 1, "summary_dense": 2})
        self.assertEqual(y.families, {"lexical", "dense"})
        self.assertNotIn("leaf", outer.runs)
        self.assertEqual(set(outer.runs), {"leaf_lexical", "leaf_dense", "summary_lexical", "summary_dense"})

    def test_duplicate_union_and_determinism(self) -> None:
        runs = [("lexical", ["a", "b", "a"], 1.0), ("dense", ["b", "a"], 1.0)]
        r1, r2 = fuse(runs), fuse(runs)
        self.assertEqual(r1.ids(), r2.ids())
        self.assertEqual(len(r1.ordered), 2)  # union, no duplicates
        self.assertEqual(r1.by_id["a"].sources, {"lexical": 1, "dense": 2})
        self.assertEqual([c.rank for c in r1.ordered], [1, 2])

    def test_raw_scores_and_families(self) -> None:
        f = fuse([("leaf_lexical", ["a"], 1.0), ("speaker_dense", ["a"], 1.0), ("alias_lexical", ["b"], 1.0)],
                 raw_scores={"leaf_lexical": {"a": 3.5}})
        self.assertEqual(f.by_id["a"].raw, {"leaf_lexical": 3.5})
        self.assertEqual(f.by_id["a"].families, {"lexical", "speaker"})
        self.assertEqual(source_family("q1_dense"), "dense")
        self.assertEqual(source_family("alias_lexical"), "alias")

    def test_soft_event_retrieval_modes_share_one_derived_family(self) -> None:
        self.assertEqual(source_family("soft_event_lexical"), "soft_event")
        self.assertEqual(source_family("soft_event_dense"), "soft_event")


class AssessmentSafetyTests(unittest.TestCase):
    def test_soft_event_does_not_inflate_agreement_or_diversity_by_default(self) -> None:
        fusion = fuse(
            [
                ("leaf_lexical", ["a", "b"], 1.0),
                ("soft_event_lexical", ["a", "c"], 0.5),
                ("soft_event_dense", ["a", "d"], 0.5),
            ]
        )
        safe = signals_for(fusion, top_n=10, targets_requested=False, targets_resolved=False)
        unrestricted = signals_for(
            fusion,
            top_n=10,
            targets_requested=False,
            targets_resolved=False,
            excluded_families=set(),
        )
        self.assertEqual(safe["agreement"], 0.0)
        self.assertEqual(safe["source_diversity"], 1)
        self.assertGreater(unrestricted["agreement"], safe["agreement"])
        self.assertGreater(unrestricted["source_diversity"], safe["source_diversity"])
        self.assertEqual(assess(fusion).signals, safe)


class ContributionTests(unittest.TestCase):
    def test_per_source_hit_and_unique(self) -> None:
        f = fuse([("lexical", ["a", "b"], 1.0), ("dense", ["c"], 1.0), ("speaker", ["c", "d"], 1.0)])
        self.assertEqual(per_source_hit(f, {"a"}), {"lexical": True, "dense": False, "speaker": False})
        self.assertEqual(unique_contribution(f, {"a"}), ["lexical"])
        self.assertEqual(unique_contribution(f, {"c"}), [])  # two sources found it
        self.assertEqual(unique_contribution(f, {"zz"}), [])


class EngineProvenanceTests(unittest.TestCase):
    def test_retrieve_exposes_provenance(self) -> None:
        index = build_index()
        q = _unit(3)
        trace: dict = {}
        hits = E.retrieve(index, _FakeClient(q), "D대회 본선", top_k=3, q_vec=q, ranking="hybrid",
                          trace=trace, source_room_id=ROOM_OLD)
        self.assertIn("candidate_provenance", trace)
        self.assertEqual(set(trace["candidate_sources"]),
                         {"leaf_lexical", "leaf_dense", "summary_lexical", "summary_dense"})
        self.assertEqual(trace["retrievers_used"], ["leaf_lexical", "leaf_dense", "summary_lexical", "summary_dense"])
        self.assertGreater(trace["candidate_union_n"], 0)
        self.assertTrue(all(h["sources"] for h in hits))
        # every provenance entry is in the allowed room
        for row in trace["candidate_provenance"]:
            self.assertEqual(index.leaf_by_id[row["doc_id"]]["source_room_id"], ROOM_OLD)


class GuaranteePoolTests(unittest.TestCase):
    def test_low_weight_source_gets_pool_slots_without_changing_top_order(self) -> None:
        strong = [f"s{i}" for i in range(20)]
        weak = [f"w{i}" for i in range(20)]
        f = fuse([("leaf_dense", strong, 4.0), ("speaker_dense", weak, 0.1)])
        ranked, extras, by_src = guarantee_pool(f, pool=12, per_source_min=3, reserve_max_frac=0.3)
        self.assertEqual(f.ids()[:9], strong[:9])            # fusion order itself is untouched
        self.assertEqual(ranked[:9], strong[:9])            # pool prefix starts with fused head
        self.assertEqual(extras, ["w0", "w1", "w2"])
        self.assertEqual(by_src["speaker_dense"], ["w0", "w1", "w2"])
        self.assertEqual(len(set(ranked[:12])), 12)
        self.assertTrue(set(extras) <= set(ranked[:12]))
        self.assertEqual(len(ranked), 40)
        self.assertEqual(len(ranked), len(set(ranked)))
        same, none, empty = guarantee_pool(f, pool=12, per_source_min=0)
        self.assertEqual((same, none, empty), (f.ids(), [], {}))

    def test_reserve_round_robin_does_not_drain_one_source(self) -> None:
        strong = [f"s{i}" for i in range(20)]
        alias = [f"a{i}" for i in range(10)]
        speaker = [f"p{i}" for i in range(10)]
        f = fuse([("leaf_dense", strong, 4.0), ("alias_lexical", alias, 0.1), ("speaker_dense", speaker, 0.1)])
        ranked, extras, by_src = guarantee_pool(f, pool=10, per_source_min=5, reserve_max_frac=0.3)
        # pool=10, reserve=3, base=7. Round-robin: alias a0, speaker p0, alias a1.
        self.assertEqual(ranked[:7], strong[:7])
        self.assertEqual(len(extras), 3)
        self.assertTrue(by_src.get("alias_lexical"))
        self.assertTrue(by_src.get("speaker_dense"))
        self.assertLessEqual(len(by_src["alias_lexical"]), 2)

    def test_excluded_derived_family_cannot_consume_reserve(self) -> None:
        strong = [f"s{i}" for i in range(20)]
        soft = [f"e{i}" for i in range(20)]
        speaker = [f"p{i}" for i in range(20)]
        f = fuse(
            [
                ("leaf_dense", strong, 4.0),
                ("soft_event_lexical", soft, 0.1),
                ("speaker_dense", speaker, 0.1),
            ]
        )
        ranked, extras, by_src = guarantee_pool(
            f, pool=12, per_source_min=3, reserve_max_frac=0.3, excluded_sources={"soft_event"}
        )
        self.assertEqual(extras, ["p0", "p1", "p2"])
        self.assertNotIn("soft_event_lexical", by_src)
        self.assertNotIn("e0", extras)
        self.assertEqual(len(ranked), len(set(ranked)))
        again = guarantee_pool(
            f, pool=12, per_source_min=3, reserve_max_frac=0.3, excluded_sources={"soft_event"}
        )
        self.assertEqual((ranked, extras, by_src), again)

    def test_reserve_allow_list_is_optional_and_deterministic(self) -> None:
        f = fuse(
            [
                ("leaf_dense", [f"s{i}" for i in range(20)], 4.0),
                ("soft_event_dense", [f"e{i}" for i in range(20)], 0.1),
                ("speaker_dense", [f"p{i}" for i in range(20)], 0.1),
            ]
        )
        _, extras, by_src = guarantee_pool(
            f, pool=12, per_source_min=3, reserve_max_frac=0.3, reserve_sources={"speaker"}
        )
        self.assertEqual(extras, ["p0", "p1", "p2"])
        self.assertEqual(set(by_src), {"speaker_dense"})

    def test_pool_necessary_sources_ignores_fusion_head(self) -> None:
        f = fuse([("leaf_dense", ["a", "b"], 4.0), ("speaker_dense", ["z", "a"], 0.1)])
        self.assertEqual(pool_necessary_sources(f, ["a", "b", "z"], {"a"}, fusion_head=["a", "b"]), [])
        self.assertEqual(pool_necessary_sources(f, ["a", "b", "z"], {"z"}, fusion_head=["a", "b"]), ["speaker_dense"])


class EngineGuaranteePoolTests(unittest.TestCase):
    def _index(self) -> E.IndexBundle:
        index = build_index(with_summaries=False)
        index.cfg["lexical_limit"] = 2
        index.cfg["dense_limit"] = 2
        index.cfg["retrieve_pool"] = 8
        index.cfg["retrieval"] = {
            "person": {"enabled": True, "rrf_weight": 0.1, "limit": 20},
            "alias": {"enabled": True, "rrf_weight": 0.05, "limit": 20},
            "multi_query": {"enabled": False},
            "candidate_pool": {"per_source_min": 2, "reserve_max_frac": 0.5, "default": 8, "min": 4, "max": 16},
        }
        return index

    def test_r0_top_follows_fusion_not_person_guarantee(self) -> None:
        index = self._index()
        q = _unit(9)
        trace: dict = {}
        hits = E.retrieve(index, _FakeClient(q), "그 사람 시험 얘기", top_k=3, q_vec=q, ranking="hybrid",
                          trace=trace, source_room_id=ROOM_OLD, intent={"targets": ["병수"], "kind": "person_fact",
                          "recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
                          "source": "test"})
        fusion_ids = trace["fusion_ids"]
        self.assertEqual(trace["hybrid_top10"][:3], fusion_ids[:3])
        self.assertEqual(len(trace["candidate_pool_ids"]), len(set(trace["candidate_pool_ids"])))
        self.assertLessEqual(len(trace["candidate_pool_ids"]), 8)
        self.assertTrue(hits)

    def test_reranker_receives_guaranteed_person_candidates(self) -> None:
        index = self._index()

        class _Rerank:
            def __init__(self, *a, **k):  # noqa: ANN002, ANN003
                pass

            def rerank(self, query, docs, top_n=None):  # noqa: ANN001
                return _FakeRerankResult(list(range(len(docs))), [0.9] * len(docs))

        saved = (E.RerankClient, E.load_secrets, E._rerank_available)
        E.RerankClient = _Rerank
        E.load_secrets = lambda *a, **k: {}
        E._rerank_available = lambda: True
        try:
            q = _unit(9)
            trace: dict = {}
            E.retrieve(index, _FakeClient(q), "그 사람 시험 얘기", top_k=3, q_vec=q, ranking="rerank",
                       remaining_budget=1.0, trace=trace, source_room_id=ROOM_OLD,
                       intent={"targets": ["병수"], "kind": "person_fact", "recent": False, "intro": False,
                               "politics": False, "prefer_missing_over_guess": False, "source": "test"})
        finally:
            E.RerankClient, E.load_secrets, E._rerank_available = saved
        pool = set(trace["candidate_pool_ids"])
        self.assertEqual(pool, set(trace["hybrid_ids"]))
        self.assertEqual(trace["pool_ids"], trace["candidate_pool_ids"])
        runs = trace["candidate_runs"]
        speaker_ids = set(runs.get("speaker_dense") or []) | set(runs.get("speaker_lexical") or [])
        leaf_ids = set(runs.get("leaf_dense") or []) | set(runs.get("leaf_lexical") or [])
        only_speaker = speaker_ids - leaf_ids
        self.assertTrue(only_speaker)
        self.assertTrue(only_speaker & pool)

    def test_guaranteed_candidates_stay_in_room_and_unique(self) -> None:
        index = self._index()
        q = _unit(3)
        trace: dict = {}
        E.retrieve(index, _FakeClient(q), "김가명 D대회", top_k=3, q_vec=q, ranking="hybrid",
                   trace=trace, source_room_id=ROOM_OLD, intent={"targets": ["가명"], "kind": "person_fact",
                   "recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
                   "source": "test"})
        pool = trace["candidate_pool_ids"]
        self.assertEqual(len(pool), len(set(pool)))
        for doc_id in pool + list(trace.get("pool_guaranteed_ids") or []):
            self.assertEqual(index.leaf_by_id[doc_id]["source_room_id"], ROOM_OLD)
            self.assertNotEqual(index.leaf_by_id[doc_id]["source_room_id"], ROOM_NEW)
        self.assertEqual(trace["pool_guaranteed_n"], len(trace["pool_guaranteed_ids"]))
        self.assertEqual(trace["candidate_pool_n"], len(pool))
