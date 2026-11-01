"""질의 임베딩 캐시·선행 요청·헤지 (query_embed.py) — 네트워크 없음."""

from __future__ import annotations

import threading
import time
import unittest

import numpy as np

import engine as E
from query_embed import QueryEmbedder, query_embed_config
from test_engine_retrieval import ROOM_OLD, _unit, build_index


class _CountingClient:
    embed_model = "test-embed"

    def __init__(self, vec: np.ndarray, *, delays: list[float] | None = None, fail: int = 0) -> None:
        self._vec = vec
        self._delays = list(delays or [])
        self._fail = fail
        self.calls: list[str] = []
        self._lock = threading.Lock()

    def embed(self, texts, **_kwargs):  # noqa: ANN001
        with self._lock:
            self.calls.append(texts[0])
            n = len(self.calls)
            delay = self._delays[n - 1] if n - 1 < len(self._delays) else 0.0
            fail = n <= self._fail
        time.sleep(delay)
        if fail:
            raise RuntimeError("provider down")
        return [(self._vec * 3.0).tolist() for _ in texts]


class QueryEmbedderTests(unittest.TestCase):
    def test_config_defaults_and_override(self) -> None:
        self.assertTrue(query_embed_config({})["prefetch"])
        self.assertEqual(query_embed_config({"query_embed": {"hedge_after_ms": 0}})["hedge_after_ms"], 0)

    def test_cache_hit_skips_second_call_and_returns_normalized(self) -> None:
        q = _unit(3)
        client = _CountingClient(q)
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 0}})
        v1, i1 = emb.get(client, "질문", instruction="i")
        v2, i2 = emb.get(client, "질문", instruction="i")
        self.assertEqual((i1["source"], i2["source"]), ("miss", "hit"))
        self.assertEqual(len(client.calls), 1)
        self.assertAlmostEqual(float(np.linalg.norm(v1)), 1.0, places=5)
        np.testing.assert_allclose(v1, q, atol=1e-6)
        np.testing.assert_allclose(v1, v2)

    def test_cache_key_includes_instruction_and_model(self) -> None:
        client = _CountingClient(_unit(3))
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 0}})
        emb.get(client, "질문", instruction="a")
        emb.get(client, "질문", instruction="b")
        other = _CountingClient(_unit(3))
        other.embed_model = "other-embed"
        emb.get(other, "질문", instruction="a")
        self.assertEqual(len(client.calls) + len(other.calls), 3)

    def test_cache_disabled_calls_every_time(self) -> None:
        client = _CountingClient(_unit(3))
        emb = QueryEmbedder({"query_embed": {"cache": False, "hedge_after_ms": 0}})
        emb.get(client, "질문", instruction="i")
        emb.get(client, "질문", instruction="i")
        self.assertEqual(len(client.calls), 2)

    def test_cache_evicts_oldest(self) -> None:
        client = _CountingClient(_unit(3))
        emb = QueryEmbedder({"query_embed": {"cache_entries": 1, "hedge_after_ms": 0}})
        emb.get(client, "a", instruction="i")
        emb.get(client, "b", instruction="i")
        emb.get(client, "a", instruction="i")
        self.assertEqual(client.calls, ["a", "b", "a"])

    def test_prefetch_is_joined_not_repeated(self) -> None:
        client = _CountingClient(_unit(3), delays=[0.15])
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 0}})
        emb.prefetch(client, "질문", instruction="i")
        emb.prefetch(client, "질문", instruction="i")  # 진행 중이면 다시 보내지 않는다
        _, info = emb.get(client, "질문", instruction="i")
        self.assertEqual(info["source"], "joined")
        self.assertEqual(len(client.calls), 1)

    def test_blank_prefetch_is_ignored(self) -> None:
        client = _CountingClient(_unit(3))
        emb = QueryEmbedder()
        emb.prefetch(client, "  ", instruction="i")
        self.assertEqual(client.calls, [])

    def test_hedge_takes_faster_duplicate_and_reports_late_loser(self) -> None:
        client = _CountingClient(_unit(3), delays=[0.6, 0.0])
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 50}})
        late = threading.Event()
        started = time.perf_counter()
        _, info = emb.get(client, "질문", instruction="i", on_late=late.set)
        self.assertLess(time.perf_counter() - started, 0.45)
        self.assertTrue(info["hedged"])
        self.assertEqual(len(client.calls), 2)
        self.assertTrue(late.wait(2.0))  # 진 요청도 끝나면 장부 훅이 불린다

    def test_no_hedge_when_first_is_fast(self) -> None:
        client = _CountingClient(_unit(3))
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 500}})
        _, info = emb.get(client, "질문", instruction="i")
        self.assertFalse(info["hedged"])
        self.assertEqual(len(client.calls), 1)

    def test_hedge_survives_one_failure(self) -> None:
        client = _CountingClient(_unit(3), delays=[0.3, 0.0], fail=1)
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 50}})
        vec, info = emb.get(client, "질문", instruction="i")
        self.assertTrue(info["hedged"])
        self.assertAlmostEqual(float(np.linalg.norm(vec)), 1.0, places=5)

    def test_failure_propagates_and_is_not_cached(self) -> None:
        client = _CountingClient(_unit(3), fail=1)
        emb = QueryEmbedder({"query_embed": {"hedge_after_ms": 0}})
        with self.assertRaises(RuntimeError):
            emb.get(client, "질문", instruction="i")
        _, info = emb.get(client, "질문", instruction="i")
        self.assertEqual(info["source"], "miss")


class EngineQueryVectorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.index = build_index()
        self.index.query_embedder = QueryEmbedder({"query_embed": {"hedge_after_ms": 0}})

    def _retrieve(self, client, query: str, intent: dict | None = None) -> tuple[list[dict], dict]:
        trace: dict = {}
        hits = E.retrieve(
            self.index, client, query, top_k=5, mode="leaf_parent", ranking="hybrid",
            trace=trace, source_room_id=ROOM_OLD, intent=intent,
        )
        return hits, trace

    def test_repeat_query_reuses_vector_and_same_hits(self) -> None:
        client = _CountingClient(_unit(1))
        first, t1 = self._retrieve(client, "가명 뭐해")
        second, t2 = self._retrieve(client, "가명 뭐해")
        self.assertEqual([h["doc_id"] for h in first], [h["doc_id"] for h in second])
        self.assertEqual((t1["embed_source"], t2["embed_source"]), ("miss", "hit"))
        self.assertEqual(client.calls.count("가명 뭐해"), 1)

    def test_cached_vector_is_not_mutated_by_retrieval(self) -> None:
        client = _CountingClient(_unit(1))
        self._retrieve(client, "가명 뭐해")
        key = ("test-embed", E._embed_instruction(self.index.cfg), "가명 뭐해")
        before = self.index.query_embedder._cache[key].copy()
        self._retrieve(client, "가명 뭐해")
        np.testing.assert_array_equal(self.index.query_embedder._cache[key], before)

    def test_semantic_query_is_embedded_once_and_prefetched(self) -> None:
        client = _CountingClient(_unit(1))
        intent = {"semantic_query": "가명이 요즘 하는 일"}
        _, trace = self._retrieve(client, "가명 뭐해", intent)
        self.assertEqual(sorted(client.calls), sorted(["가명 뭐해", "가명이 요즘 하는 일"]))
        self.assertIn(trace.get("q1_embed_source"), {"joined", "hit"})

    def test_same_hits_with_and_without_embedder(self) -> None:
        with_cache, _ = self._retrieve(_CountingClient(_unit(1)), "최근에 무슨 얘기 했어")
        self.index.query_embedder = None
        direct, trace = self._retrieve(_CountingClient(_unit(1)), "최근에 무슨 얘기 했어")
        self.assertEqual(trace["embed_source"], "direct")
        self.assertEqual([h["doc_id"] for h in with_cache], [h["doc_id"] for h in direct])

    def test_vector_dims_applied_to_cached_vector(self) -> None:
        E.apply_vector_dims(self.index, 4)
        vec, _ = E.query_vector(self.index, _CountingClient(_unit(1)), "가명 뭐해")
        self.assertEqual(vec.shape, (4,))
        self.assertAlmostEqual(float(np.linalg.norm(vec)), 1.0, places=5)


if __name__ == "__main__":
    unittest.main()
