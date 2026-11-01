"""Multi-query candidate sources: planner semantic_query (dense) and key_terms (lexical)."""

from __future__ import annotations

import unittest

import numpy as np

import engine as E
from test_engine_retrieval import DIM, ROOM_OLD, _FakeClient, _unit, build_index


def _intent(**kw):
    base = {"recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
            "kind": "event", "targets": [], "source": "planner", "time": {"kind": "any", "from": None, "to": None},
            "semantic_query": "", "key_terms": [], "memory_search_query": ""}
    base.update(kw)
    return base


class _TwoVecClient:
    """Embeds the semantic query to a *different* vector than q0 so q1_dense can add candidates."""

    def __init__(self, q1: np.ndarray) -> None:
        self.q1 = q1
        self.calls: list[str] = []

    def embed(self, texts, **_kw):  # noqa: ANN001
        self.calls.extend(texts)
        return [self.q1.tolist() for _ in texts]


class MultiQueryTests(unittest.TestCase):
    def test_semantic_and_key_terms_add_sources(self) -> None:
        index = build_index()
        index.cfg["dense_limit"] = 3  # room has 10 leaves; keep q0 dense from covering all of them
        q0 = _unit(11)
        client = _TwoVecClient(_unit(12))
        trace: dict = {}
        E.retrieve(index, client, "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace, source_room_id=ROOM_OLD,
                   intent=_intent(semantic_query="노트북 추천에 대해 말한 내용", key_terms=["노트북", "추천"]))
        self.assertEqual(client.calls, ["노트북 추천에 대해 말한 내용"])  # one extra embed, no LLM
        self.assertIn("q1_dense", trace["candidate_sources"])
        self.assertIn("q1_summary_dense", trace["candidate_sources"])
        self.assertIn("q2_lexical", trace["candidate_sources"])
        self.assertEqual(trace["queries_used"], {"q0": "노트북", "q1": "노트북 추천에 대해 말한 내용", "q2": "노트북 추천"})
        # q1 found leaves q0 dense did not (different vector)
        q1_only = [r for r in trace["candidate_provenance"] if "q1_dense" in r["sources"] and "leaf_dense" not in r["sources"]]
        self.assertTrue(q1_only)
        for r in trace["candidate_provenance"]:
            self.assertEqual(index.leaf_by_id[r["doc_id"]]["source_room_id"], ROOM_OLD)

    def test_max_queries_and_disabled(self) -> None:
        index = build_index()
        q0 = _unit(11)
        index.cfg["retrieval"] = {"multi_query": {"max_queries": 2}}
        trace: dict = {}
        E.retrieve(index, _TwoVecClient(_unit(12)), "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace,
                   source_room_id=ROOM_OLD, intent=_intent(semantic_query="노트북 얘기", key_terms=["노트북"]))
        self.assertEqual(set(trace["queries_used"]), {"q0", "q1"})  # cap 2 → key_terms dropped
        index.cfg["retrieval"] = {"multi_query": {"enabled": False}}
        trace2: dict = {}
        E.retrieve(index, _TwoVecClient(_unit(12)), "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace2,
                   source_room_id=ROOM_OLD, intent=_intent(semantic_query="노트북 얘기", key_terms=["노트북"]))
        self.assertEqual(set(trace2["queries_used"]), {"q0"})
        self.assertNotIn("q1_dense", trace2["candidate_sources"])

    def test_identical_semantic_query_is_skipped_and_failure_isolated(self) -> None:
        index = build_index()
        q0 = _unit(11)
        trace: dict = {}
        E.retrieve(index, _FakeClient(q0), "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace,
                   source_room_id=ROOM_OLD, intent=_intent(semantic_query="노트북"))
        self.assertEqual(set(trace["queries_used"]), {"q0"})

        trace3: dict = {}
        client = _TwoVecClient(_unit(12))
        E.retrieve(index, client, "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace3,
                   source_room_id=ROOM_OLD, intent=_intent(memory_search_query="노트북 추천에 대해 말한 내용"))
        self.assertEqual(trace3["queries_used"]["q1"], "노트북 추천에 대해 말한 내용")
        self.assertIn("q1_dense", trace3["candidate_sources"])

        class _Boom:
            def embed(self, *a, **k):  # noqa: ANN002, ANN003
                raise RuntimeError("embed down")

        trace2: dict = {}
        hits = E.retrieve(index, _Boom(), "노트북", top_k=5, q_vec=q0, ranking="hybrid", trace=trace2,
                          source_room_id=ROOM_OLD, intent=_intent(semantic_query="노트북 추천 얘기"))
        self.assertTrue(hits)
        self.assertEqual(trace2["multi_query_error"], "RuntimeError")
