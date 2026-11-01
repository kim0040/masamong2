"""Retrieval-layer regression tests on a synthetic two-room index (no network, no raw corpus).

Covers the sandbox gaps found in the 2026-09-16 audit:
- R1 (heuristic) must never inject leaves from another room (channel isolation).
- R1 on a general query must keep the hybrid order (recency is a tiebreaker, not a sort key).
- Room-filtered lexical retrieval must not be starved by a pre-filter LIMIT.
- load_index must refuse an index embedded with a different model.
"""

from __future__ import annotations

import json
import os
import random
import tempfile
import threading
import unittest
from concurrent.futures import ProcessPoolExecutor, ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from pathlib import Path

import numpy as np

import engine as E
from segment import slice_segments_per_room

ROOM_OLD = "discord:1:100"  # ended ~200 days before ROOM_NEW
ROOM_NEW = "discord:2:200"
DIM = 8
LEAF = 4


def _write_catalog_process(args: tuple[str, int]) -> None:
    data_dir, number = args
    from memory_blocks import write_catalog

    write_catalog(
        Path(data_dir),
        [{
            "block_id": f"process-{number}",
            "source_room": f"discord:1:{number}",
            "leaf_ids": [f"leaf-{number}"],
            "status": "ready",
        }],
    )


def _messages() -> list[dict]:
    """Two rooms, 40 messages each. ROOM_NEW is much newer; both mention the person token."""

    base_new = datetime(2026, 9, 1, tzinfo=timezone.utc)
    base_old = base_new - timedelta(days=200)
    rows: list[dict] = []
    speakers = ["가명", "을동", "병수", "정수"]
    topics = ["점심 뭐 먹을까", "게임 같이 할 사람", "시험 언제야", "코딩 과제 냈어",
              "김가명 D대회 본선 갔대", "요즘 날씨 좋다", "노트북 추천", "영화 볼래"]
    for room, base in ((ROOM_OLD, base_old), (ROOM_NEW, base_new)):
        for i in range(40):
            rows.append(
                {
                    "observation_id": f"{room}:{i}",
                    "source_room_id": room,
                    "source_room_original": room,
                    "actor_label": speakers[i % 4],
                    "actor_account": None,
                    "content": topics[i % 8] + f" #{i}",
                    "parsed_time": (base + timedelta(hours=i)).isoformat(),
                }
            )
    for i, r in enumerate(rows):
        r["index"] = i
    return rows


def build_index(*, with_summaries: bool = True, seed: int = 0) -> E.IndexBundle:
    cfg = {
        "lexical": "bigram",
        "rrf_weights": [1.0, 4.0],
        "document_char_limit": 4000,
        "retrieve_pool": 40,
        "top_k": 5,
        "leaf_size": LEAF,
        "parent_expand_before": 2,
        "parent_expand_after": 2,
        "context_prefix": True,
        "summary_enabled": with_summaries,
        "summary_rrf_weight": 1.0,
        "recency_half_life_days": 40,
        "person_aliases": {"가명": ["김가명"], "김가명": ["김가명"]},
        "rooms": [ROOM_OLD, ROOM_NEW],
        "max_history_turns": 8,
    }
    messages = _messages()
    leaves = slice_segments_per_room(
        messages, size=LEAF, overlap=0, limit=10_000, kind="leaf", with_prefix=True
    )
    baseline = slice_segments_per_room(
        messages, size=8, overlap=0, limit=10_000, kind="win", with_prefix=True
    )
    rng = np.random.default_rng(seed)

    def _vecs(n: int) -> np.ndarray:
        v = rng.standard_normal((n, DIM)).astype(np.float32)
        return v / np.linalg.norm(v, axis=1, keepdims=True)

    leaf_fts = E.ThreadSafeLexicalIndex("bigram")
    leaf_fts.add_many([(d["doc_id"], d["body"]) for d in leaves])
    baseline_fts = E.ThreadSafeLexicalIndex("bigram")
    baseline_fts.add_many([(d["doc_id"], d["body"]) for d in baseline])

    summaries: list[dict] = []
    summary_fts = None
    summary_vectors = None
    if with_summaries:
        summaries = [
            {"doc_id": d["doc_id"], "summary": "김가명 D대회 본선 이야기" if "D대회" in d["body"] else "잡담"}
            for d in leaves
        ]
        summary_fts = E.ThreadSafeLexicalIndex("bigram")
        summary_fts.add_many([(d["doc_id"], d["summary"]) for d in summaries])
        summary_vectors = _vecs(len(summaries))

    leaf_end_ts: dict[str, float] = {}
    for leaf in leaves:
        ts = E._parse_ts(messages[int(leaf["end"]) - 1]["parsed_time"])
        assert ts is not None
        leaf_end_ts[leaf["doc_id"]] = ts
    corpus_tmax = max(leaf_end_ts.values())
    return E.IndexBundle(
        cfg=cfg,
        messages=messages,
        leaves=leaves,
        leaf_vectors=_vecs(len(leaves)),
        leaf_ids=[d["doc_id"] for d in leaves],
        leaf_by_id={d["doc_id"]: d for d in leaves},
        leaf_fts=leaf_fts,
        baseline=baseline,
        baseline_vectors=_vecs(len(baseline)),
        baseline_ids=[d["doc_id"] for d in baseline],
        baseline_by_id={d["doc_id"]: d for d in baseline},
        baseline_fts=baseline_fts,
        summaries=summaries,
        summary_by_id={d["doc_id"]: d for d in summaries},
        summary_vectors=summary_vectors,
        summary_ids=[d["doc_id"] for d in summaries],
        summary_fts=summary_fts,
        meta={"recipe": {"models": {"embed": "test-embed"}}},
        leaf_end_ts=leaf_end_ts,
        leaves_by_recency=sorted(leaf_end_ts, key=lambda d: leaf_end_ts[d], reverse=True),
        corpus_tmax=corpus_tmax,
        lock=threading.Lock(),
    )


class _FakeClient:
    def __init__(self, vec: np.ndarray) -> None:
        self._vec = vec

    def embed(self, texts, **_kwargs):  # noqa: ANN001
        return [self._vec.tolist() for _ in texts]


def _unit(seed: int) -> np.ndarray:
    v = np.random.default_rng(seed).standard_normal(DIM).astype(np.float32)
    return v / np.linalg.norm(v)


def _spearman(a: list[str], b: list[str]) -> float:
    pos = {d: i for i, d in enumerate(b)}
    n = len(a)
    d2 = sum((i - pos[d]) ** 2 for i, d in enumerate(a))
    return 1 - 6 * d2 / (n * (n * n - 1))


class ChannelIsolationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.index = build_index()
        self.old_ids = {d["doc_id"] for d in self.index.leaves if d["source_room_id"] == ROOM_OLD}

    def _hits(self, query: str, ranking: str) -> list[dict]:
        q = _unit(1)
        return E.retrieve(
            self.index,
            _FakeClient(q),
            query,
            top_k=5,
            mode="leaf_parent",
            q_vec=q,
            ranking=ranking,
            trace={},
            source_room_id=ROOM_OLD,
        )

    def test_recent_intent_heuristic_stays_in_room(self) -> None:
        # ROOM_NEW is 200 days fresher; the old recency injection pulled it in.
        for query in ("요즘 가명 뭐해", "최근에 무슨 얘기 했어"):
            hits = self._hits(query, "heuristic")
            self.assertTrue(hits, query)
            self.assertTrue(all(h["source_room_id"] == ROOM_OLD for h in hits), query)

    def test_intro_intent_heuristic_stays_in_room(self) -> None:
        hits = self._hits("가명 소개해줘", "heuristic")
        self.assertTrue(hits)
        self.assertTrue(all(h["source_room_id"] == ROOM_OLD for h in hits))

    def test_hybrid_stays_in_room(self) -> None:
        hits = self._hits("요즘 가명 뭐해", "hybrid")
        self.assertTrue(all(h["source_room_id"] == ROOM_OLD for h in hits))

    def test_rerank_leaves_filters_allowed_ids_directly(self) -> None:
        ranked = [d for d in self.index.leaf_ids if d in self.old_ids][:6]
        picked = E._rerank_leaves(
            self.index, ranked, query="최근 얘기", pool=40, top_k=5, allowed_ids=self.old_ids
        )
        self.assertTrue(picked)
        self.assertTrue(set(picked) <= self.old_ids)


class HeuristicScaleTests(unittest.TestCase):
    def test_general_query_keeps_hybrid_order(self) -> None:
        index = build_index()
        query = "그때 무슨 얘기 했었지"
        it = E._query_intent(query)
        self.assertFalse(it["recent"] or it["intro"] or it["politics"], "query must be intent-free")
        rhos = []
        rng = random.Random(0)
        for _ in range(20):
            pool = rng.sample(index.leaf_ids, 16)
            picked = E._rerank_leaves(index, pool, query=query, pool=16, top_k=16)
            self.assertEqual(set(picked), set(pool))
            rhos.append(_spearman(pool, picked))
        self.assertGreaterEqual(sum(rhos) / len(rhos), 0.8, rhos)

    def test_recent_intent_still_prefers_fresh_leaves(self) -> None:
        index = build_index()
        index.cfg["intent_fallback"] = "regex"  # R1 is a legacy ablation that reads the keyword fallback
        new_ids = [d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_NEW]
        # Hybrid says oldest-first; "최근" intent should flip that toward the newest leaves.
        pool = sorted(new_ids, key=lambda d: index.leaf_end_ts[d])
        picked = E._rerank_leaves(index, pool, query="최근에 뭐 했어", pool=len(pool), top_k=3, allowed_ids=set(new_ids))
        newest = set(sorted(new_ids, key=lambda d: -index.leaf_end_ts[d])[:5])
        self.assertTrue(set(picked) & newest, picked)


class LexicalRoomFilterTests(unittest.TestCase):
    def test_room_filter_does_not_starve_lexical_hits(self) -> None:
        index = build_index()
        old_ids = {d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD}
        q = _unit(2)
        trace: dict = {}
        ranked = E._hybrid_rank(
            fts=index.leaf_fts,
            vectors=index.leaf_vectors,
            ids=index.leaf_ids,
            query="점심 뭐 먹을까",
            q_vec=q,
            weights=[1.0, 4.0],
            lock=index.lock,
            trace=trace,
            allowed_ids=old_ids,
        )
        self.assertTrue(set(ranked) <= old_ids)
        # Every ROOM_OLD leaf containing the phrase must be a lexical candidate,
        # regardless of how many ROOM_NEW leaves also match.
        expected = {d for d in old_ids if "점심 뭐 먹을까" in index.leaf_by_id[d]["body"]}
        got = {row["doc_id"] for row in trace["leaf_lexical"]}
        self.assertTrue(expected, "fixture must contain the phrase")
        self.assertTrue(expected <= got, expected - got)

    def test_lexical_limit_is_applied_after_filter(self) -> None:
        # Shrink the cut so ROOM_NEW matches alone would exhaust it; in-room matches must survive.
        index = build_index()
        old_ids = {d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD}
        with index.lock:
            in_room = [d for d, _ in index.leaf_fts.search("영화 볼래", limit=100_000) if d in old_ids]
        self.assertGreaterEqual(len(in_room), 2)
        saved = E.LEXICAL_LIMIT
        E.LEXICAL_LIMIT = 2
        try:
            trace: dict = {}
            E._hybrid_rank(
                fts=index.leaf_fts, vectors=index.leaf_vectors, ids=index.leaf_ids,
                query="영화 볼래", q_vec=_unit(3), weights=[1.0, 4.0], lock=index.lock,
                trace=trace, allowed_ids=old_ids,
            )
        finally:
            E.LEXICAL_LIMIT = saved
        got = [row["doc_id"] for row in trace["leaf_lexical"]]
        self.assertEqual(len(got), 2)
        self.assertTrue(set(got) <= old_ids)


class EmbedModelBindingTests(unittest.TestCase):
    def test_load_index_rejects_other_embed_model(self) -> None:
        index = build_index(with_summaries=False)
        with tempfile.TemporaryDirectory() as td:
            data = Path(td)
            saved = (E.DATA, dict(E.__dict__))
            try:
                E._bind_data_dir(data)
                (data / "meta.json").write_text(
                    json.dumps({"recipe": {"models": {"embed": "other/embed-model"}}}), encoding="utf-8"
                )
                (data / "messages.json").write_text(json.dumps(index.messages), encoding="utf-8")
                (data / "leaves.json").write_text(json.dumps(index.leaves), encoding="utf-8")
                (data / "baseline_windows.json").write_text(json.dumps(index.baseline), encoding="utf-8")
                np.save(data / "leaf_vecs.npy", index.leaf_vectors)
                np.save(data / "baseline_vecs.npy", index.baseline_vectors)
                orig_cfg, orig_model = E.load_config, E._configured_embed_model
                E.load_config = lambda: dict(index.cfg)
                E._configured_embed_model = lambda: "qwen/qwen3-embedding-8b"
                try:
                    with self.assertRaises(E.PlaygroundError) as ctx:
                        E.load_index()
                    self.assertIn("임베딩 모델", str(ctx.exception))
                    # same model → loads
                    E._configured_embed_model = lambda: "other/embed-model"
                    loaded = E.load_index()
                    self.assertEqual(len(loaded.leaves), len(index.leaves))
                finally:
                    E.load_config, E._configured_embed_model = orig_cfg, orig_model
            finally:
                E._bind_data_dir(saved[0])


class _FakeRerankResult:
    def __init__(self, indices, scores):
        self.indices = indices
        self.scores = scores
        self.model = "fake-rerank"
        self.cost_usd = 0.0
        self.latency_ms = 1.0
        self.billed = None
        self.generation_id = ""


class RelevanceFloorTests(unittest.TestCase):
    """R2 must not pad top_k with candidates below `rerank_min_score`."""

    def _run(self, scores: list[float], min_score: float | None = None) -> tuple[list[str], dict]:
        index = build_index(with_summaries=False)
        if min_score is not None:
            index.cfg["rerank_min_score"] = min_score
        pool = index.leaf_ids[: len(scores)]
        order = sorted(range(len(scores)), key=lambda i: -scores[i])

        class _Client:
            def __init__(self, *a, **k):  # noqa: ANN002, ANN003
                pass

            def rerank(self, query, docs, top_n=None):  # noqa: ANN001
                return _FakeRerankResult(order, [scores[i] for i in order])

        saved = (E.RerankClient, E.load_secrets)
        E.RerankClient = _Client
        E.load_secrets = lambda *a, **k: {}
        try:
            trace: dict = {}
            picked = E._rank_leaf_ids(
                index, pool, query="q", pool=len(pool), top_k=5, ranking="rerank",
                remaining_budget=1.0, trace=trace,
            )
        finally:
            E.RerankClient, E.load_secrets = saved
        return picked, trace

    def test_all_below_floor_abstains(self) -> None:
        picked, trace = self._run([0.44, 0.40, 0.36, 0.30, 0.21, 0.2])
        self.assertEqual(picked, [])
        self.assertTrue(trace["abstained"])
        self.assertEqual(trace["applied"], "rerank")

    def test_partial_floor_trims_instead_of_padding(self) -> None:
        picked, trace = self._run([0.74, 0.71, 0.49, 0.30, 0.21, 0.2])
        self.assertEqual(len(picked), 2)
        self.assertFalse(trace["abstained"])
        self.assertEqual(trace["rerank_kept_n"], 2)

    def test_floor_is_configurable(self) -> None:
        picked, _ = self._run([0.44, 0.40, 0.36], min_score=0.35)
        self.assertEqual(len(picked), 3)

    def test_hybrid_r0_is_unchanged(self) -> None:
        index = build_index(with_summaries=False)
        pool = index.leaf_ids[:10]
        picked = E._rank_leaf_ids(index, pool, query="q", pool=10, top_k=5, ranking="hybrid",
                                  remaining_budget=1.0, trace={})
        self.assertEqual(picked, pool[:5])


class IntentAndAliasTests(unittest.TestCase):
    def test_politics_regex_false_positives(self) -> None:
        for q in ("지지난주에 뭐 했지", "보수 공사 언제 끝나", "성향 테스트 결과 어때", "보수를 받았어"):
            self.assertFalse(E._query_intent(q)["politics"], q)
        for q in ("대통령 선거 언제야", "그 사람 정치 성향 어때", "가명 지지하는 정당", "보수 성향이야?"):
            self.assertTrue(E._query_intent(q)["politics"], q)

    def test_alias_targets_respect_boundaries_and_case(self) -> None:
        al = E._alias_table({"person_aliases": {"제일": ["한제일"], "latte": ["latte"], "정수": ["이정수", "정수"], "abc": ["abc408"]}})
        # Common-word collisions are a per-corpus config concern (person_alias_ignore), not code:
        self.assertEqual(E.DEFAULT_ALIAS_IGNORE, frozenset())
        self.assertEqual(E._query_targets("제일 좋아하는 음식이 뭐야", al), ["제일"])
        self.assertEqual(E._query_targets("제일 좋아하는 음식이 뭐야", al, ignore={"제일"}), [])
        self.assertEqual(E._query_targets("카페라떼 마실래? iced latte", al, ignore={"latte"}), [])
        self.assertEqual(E._query_targets("현가명이 왔다", al), [])
        self.assertEqual(E._query_targets("정수이 요즘 뭐하냐", al), ["정수"])
        self.assertEqual(E._query_targets("이정수 뭐해", al), ["정수"])
        self.assertEqual(E._query_targets("ABC 갈래", al), ["abc"])
        self.assertEqual(E._query_targets("제일이 말했다", al), ["제일"])

    def test_identity_terms_come_from_config_only(self) -> None:
        # No corpus-specific words in code: without config there is no identity regex at all.
        self.assertIsNone(E._identity_re({}))
        custom = E._identity_re({"identity_terms": ["로스쿨"]})
        self.assertTrue(custom.search("로스쿨 다녀"))
        self.assertFalse(custom.search("D대회 본선"))

    def test_intro_path_without_identity_terms_does_not_crash(self) -> None:
        index = build_index()
        index.cfg.pop("identity_terms", None)
        pool = index.leaf_ids[:10]
        picked = E._rerank_leaves(index, pool, query="가명 소개해줘", pool=10, top_k=3)
        self.assertEqual(len(picked), 3)


class PlannerIntentTests(unittest.TestCase):
    """Runtime intent comes from the planner LLM; regex is only the fallback."""

    def test_intent_from_plan_mapping_and_legacy_none(self) -> None:
        from bot_plan import intent_from_plan, parse_plan

        parsed = parse_plan(
            '{"memory_requirement":"required","time_scope":"recent","question_kind":"person_fact",'
            '"person_targets":["을동"],"sensitive":true,"tool_plan":[{"tool_name":"memory.search"}]}'
        )
        it = intent_from_plan(parsed)
        self.assertEqual(it["source"], "planner")
        self.assertTrue(it["recent"]); self.assertFalse(it["intro"])
        self.assertTrue(it["prefer_missing_over_guess"])
        self.assertEqual(it["targets"], ["을동"])
        legacy = parse_plan('{"tool_plan":[{"tool_name":"memory.search"}]}')
        self.assertIsNone(intent_from_plan(legacy))
        self.assertIsNone(intent_from_plan(parse_plan("garbage")))

    def test_planner_targets_replace_alias_regex(self) -> None:
        # "을동" is not in person_aliases; the regex path cannot find it, the planner can.
        index = build_index()
        pool = index.leaf_ids[:12]
        trace_regex: dict = {}
        E._rerank_leaves(index, pool, query="그 사람 뭐래", pool=12, top_k=12, trace=trace_regex)
        self.assertEqual(trace_regex["heuristic"]["targets"], [])
        trace_plan: dict = {}
        intent = {"recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
                  "kind": "person_fact", "targets": ["을동"], "source": "planner"}
        picked = E._rerank_leaves(index, pool, query="그 사람 뭐래", pool=12, top_k=12, trace=trace_plan, intent=intent)
        self.assertEqual(trace_plan["heuristic"]["targets"], ["을동"])
        top = index.leaf_by_id[picked[0]]
        self.assertIn("을동", top["speakers"])

    def test_planner_recent_without_keyword_triggers_recency(self) -> None:
        index = build_index()
        new_ids = [d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_NEW]
        pool = sorted(new_ids, key=lambda d: index.leaf_end_ts[d])  # oldest first
        q = "그 얘기 어떻게 됐어"  # no 최근/요즘 keyword
        self.assertFalse(E._query_intent(q)["recent"])
        intent = {"recent": True, "intro": False, "politics": False, "prefer_missing_over_guess": False,
                  "kind": "event", "targets": [], "source": "planner"}
        picked = E._rerank_leaves(index, pool, query=q, pool=len(pool), top_k=3, allowed_ids=set(new_ids), intent=intent)
        newest = set(sorted(new_ids, key=lambda d: -index.leaf_end_ts[d])[:5])
        self.assertTrue(set(picked) & newest)

    def test_retrieve_records_intent_source(self) -> None:
        index = build_index()
        q = _unit(4)
        trace: dict = {}
        E.retrieve(index, _FakeClient(q), "요즘 뭐해", top_k=3, q_vec=q, ranking="hybrid", trace=trace, source_room_id=ROOM_OLD)
        self.assertEqual(trace["intent"]["source"], "none")  # default: no corpus keyword fallback
        index.cfg["intent_fallback"] = "regex"
        legacy: dict = {}
        E.retrieve(index, _FakeClient(q), "요즘 뭐해", top_k=3, q_vec=q, ranking="hybrid", trace=legacy, source_room_id=ROOM_OLD)
        self.assertEqual(legacy["intent"]["source"], "regex")
        trace2: dict = {}
        E.retrieve(index, _FakeClient(q), "요즘 뭐해", top_k=3, q_vec=q, ranking="hybrid", trace=trace2,
                   source_room_id=ROOM_OLD, intent={"recent": False, "intro": False, "politics": False,
                                                   "prefer_missing_over_guess": False, "kind": "casual",
                                                   "targets": [], "source": "planner"})
        self.assertEqual(trace2["intent"]["source"], "planner")
        self.assertFalse(trace2["intent"]["recent"])


class PlannerTargetCleanupTests(unittest.TestCase):
    def test_particles_and_pronouns(self) -> None:
        self.assertEqual(E._clean_planner_targets(["민준이", "걔", "서연", "그 사람", "Min Jun", "민준"]),
                         ["민준", "서연", "Min Jun"])
        self.assertEqual(E._clean_planner_targets(["가명이가"]), ["가명"])
        self.assertEqual(E._clean_planner_targets(["이"]), [])  # 한 글자 조사만 → 버림


class AutoRankingTests(unittest.TestCase):
    """auto v2: observe the R0 v2 pool, then finish / rerank / recover. Never R1."""

    def _intent(self, **kw):
        base = {"recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
                "kind": "other", "targets": [], "source": "planner"}
        base.update(kw)
        return base

    def test_decide_table(self) -> None:
        from tool_parity import decide_ranking, fallback_ranking

        d = decide_ranking("high", rerank_available=True, remaining_budget_usd=1.0)
        self.assertEqual((d["mode"], d["reason"], d["recover"]), ("hybrid", "r0_confident", False))
        d = decide_ranking("high", rerank_available=False, remaining_budget_usd=0.0)
        self.assertEqual(d["mode"], "hybrid")
        d = decide_ranking("medium", rerank_available=True, remaining_budget_usd=1.0)
        self.assertEqual((d["mode"], d["reason"]), ("rerank", "rerank_uncertain"))
        d = decide_ranking("medium", rerank_available=False, remaining_budget_usd=1.0)
        self.assertEqual((d["mode"], d["reason"]), ("hybrid", "no_rerank"))
        d = decide_ranking("medium", rerank_available=True, remaining_budget_usd=0.01)  # budget floor
        self.assertEqual((d["mode"], d["reason"]), ("hybrid", "no_rerank"))
        d = decide_ranking("low", rerank_available=True, remaining_budget_usd=1.0)
        self.assertTrue(d["recover"]); self.assertIsNone(d["mode"])
        d = decide_ranking("low", rerank_available=True, remaining_budget_usd=1.0, recovery_done=True)
        self.assertEqual((d["mode"], d["reason"]), ("rerank", "recovery_rerank"))
        d = decide_ranking("low", rerank_available=False, remaining_budget_usd=1.0, recovery_done=True)
        self.assertEqual((d["mode"], d["reason"]), ("hybrid", "recovery_no_rerank"))
        self.assertEqual(fallback_ranking(self._intent(intro=True, targets=["철수"])), ("hybrid", "rerank_failed"))

    def test_policy_matches_contract_file(self) -> None:
        from candidates_assess import level_from_signals
        from tool_parity import AUTO_REASONS, parity_spec, ranking_policy

        pol = ranking_policy()
        spec = parity_spec()["ranking"]
        self.assertEqual(spec["auto"], "auto")
        self.assertEqual(spec["auto_policy"]["kind"], "cascade_v2")
        for k in ("on_high", "on_medium", "on_low", "no_rerank", "rerank_failed"):
            self.assertIn(pol[k], ("hybrid", "rerank"), k)  # never "heuristic"
            self.assertEqual(pol[k], spec["auto_policy"][k])
        self.assertEqual(set(spec["auto_policy"]["reasons"]), set(AUTO_REASONS))
        th = spec["auto_policy"]["thresholds"]
        from retrieval_config import DEFAULTS
        for k, v in th.items():
            self.assertEqual(DEFAULTS["auto"][k], v, k)
        for case in spec["auto_policy"]["cases"]:
            level, _ = level_from_signals(case["signals"], th)
            self.assertEqual(level, case["level"], case["name"])

    def test_policy_rejects_r1_routes(self) -> None:
        import tool_parity as TP

        bad = dict(TP.ranking_policy())
        bad["no_rerank"] = "heuristic"
        saved = TP.parity_spec
        TP.parity_spec = lambda: {"ranking": {"auto_policy": bad}}
        try:
            with self.assertRaises(ValueError):
                TP.ranking_policy()
        finally:
            TP.parity_spec = saved

    def test_memory_requirement_none_never_retrieves(self) -> None:
        from bot_plan import needs_local_rag

        self.assertFalse(needs_local_rag([{"tool_name": "memory.search", "parameters": {}}], "none"))
        self.assertTrue(needs_local_rag([], "required"))

    def _with_fixed_level(self, level: str):
        from candidates_assess import Assessment

        return lambda fusion, **kw: Assessment(level=level, signals={"candidate_count": len(fusion.ordered)}, reasons=["forced"])

    def test_retrieve_auto_without_rerank_key_lands_on_r0(self) -> None:
        index = build_index()
        saved = (E._rerank_available, E.assess)
        E._rerank_available = lambda: False
        try:
            q = _unit(5)
            for level, want_reason in (("high", "r0_confident"), ("medium", "no_rerank"), ("low", "recovery_no_rerank")):
                E.assess = self._with_fixed_level(level)
                trace: dict = {}
                E.retrieve(index, _FakeClient(q), "요즘 가명 뭐해", top_k=3, q_vec=q, ranking="auto", trace=trace,
                           source_room_id=ROOM_NEW, intent=self._intent(recent=True, targets=["가명"]))
                self.assertEqual(trace["ranking_requested"], "auto")
                self.assertEqual(trace["ranking"], "hybrid", level)
                self.assertEqual(trace["ranking_auto_reason"], want_reason, level)
                self.assertNotIn("heuristic", trace)
                self.assertTrue(trace["recent_window_applied"])
                self.assertEqual(trace["recovery_performed"], level == "low")
                self.assertFalse(trace["rerank_performed"])
        finally:
            E._rerank_available, E.assess = saved

    def test_retrieve_auto_cascade_with_reranker(self) -> None:
        index = build_index(with_summaries=False)
        calls: list[int] = []

        class _Rerank:
            def __init__(self, *a, **k):  # noqa: ANN002, ANN003
                pass

            def rerank(self, query, docs, top_n=None):  # noqa: ANN001
                calls.append(len(docs))
                order = list(range(len(docs)))
                return _FakeRerankResult(order, [0.9 - 0.01 * i for i in order])

        saved = (E._rerank_available, E.RerankClient, E.load_secrets, E.assess)
        E._rerank_available = lambda: True
        E.RerankClient = _Rerank
        E.load_secrets = lambda *a, **k: {}
        try:
            q = _unit(6)
            # high → finish on R0, reranker never called
            E.assess = self._with_fixed_level("high")
            t1: dict = {}
            E.retrieve(index, _FakeClient(q), "그때 무슨 얘기", top_k=3, q_vec=q, ranking="auto", remaining_budget=1.0,
                       trace=t1, source_room_id=ROOM_OLD)
            self.assertEqual((t1["ranking"], t1["ranking_auto_reason"], calls), ("hybrid", "r0_confident", []))
            self.assertFalse(t1["rerank_performed"])
            # medium → R2
            E.assess = self._with_fixed_level("medium")
            t2: dict = {}
            E.retrieve(index, _FakeClient(q), "그때 무슨 얘기", top_k=3, q_vec=q, ranking="auto", remaining_budget=1.0,
                       trace=t2, source_room_id=ROOM_OLD)
            self.assertEqual((t2["ranking"], t2["ranking_auto_reason"], t2["applied"]), ("rerank", "rerank_uncertain", "rerank"))
            self.assertEqual(len(calls), 1); self.assertFalse(t2["recovery_performed"])
            # low → one recovery pass (bigger pool/limits), then R2
            E.assess = self._with_fixed_level("low")
            t3: dict = {}
            E.retrieve(index, _FakeClient(q), "그때 무슨 얘기", top_k=3, q_vec=q, ranking="auto", remaining_budget=1.0,
                       trace=t3, source_room_id=ROOM_OLD)
            self.assertEqual((t3["ranking"], t3["ranking_auto_reason"]), ("rerank", "recovery_rerank"))
            self.assertTrue(t3["recovery_performed"]); self.assertEqual(len(calls), 2)
            self.assertIn("after", t3["recovery"])
            self.assertGreater(t3["recovery"]["limits"]["pool"], 40)
            # recovery disabled → straight to R2 with a distinct reason
            index.cfg["retrieval"] = {"recovery": {"enabled": False}}
            t4: dict = {}
            E.retrieve(index, _FakeClient(q), "그때 무슨 얘기", top_k=3, q_vec=q, ranking="auto", remaining_budget=1.0,
                       trace=t4, source_room_id=ROOM_OLD)
            self.assertEqual((t4["ranking_auto_reason"], t4["recovery_performed"]), ("rerank_low_confidence", False))
        finally:
            E._rerank_available, E.RerankClient, E.load_secrets, E.assess = saved

    def test_retrieve_auto_rerank_failure_lands_on_r0(self) -> None:
        index = build_index(with_summaries=False)

        class _Boom:
            def __init__(self, *a, **k):  # noqa: ANN002, ANN003
                pass

            def rerank(self, *a, **k):  # noqa: ANN002, ANN003
                raise E.RerankError("down")

        saved = (E._rerank_available, E.RerankClient, E.load_secrets, E.assess)
        E._rerank_available = lambda: True
        E.RerankClient = _Boom
        E.load_secrets = lambda *a, **k: {}
        E.assess = self._with_fixed_level("medium")
        try:
            q = _unit(6)
            trace: dict = {}
            hits = E.retrieve(index, _FakeClient(q), "철수 소개해줘", top_k=3, q_vec=q, ranking="auto",
                              remaining_budget=1.0, trace=trace, source_room_id=ROOM_OLD,
                              intent=self._intent(intro=True, targets=["철수"]))
            self.assertTrue(hits)
            self.assertEqual(trace["ranking"], "rerank")                # chosen
            self.assertEqual(trace["applied"], "hybrid_fallback")       # R2 failed → R0, not R1
            self.assertEqual(trace["ranking_auto_reason"], "rerank_failed")
            self.assertEqual([h["doc_id"] for h in hits][:1], trace["fusion_ids"][:1])
            self.assertNotIn("rerank_error_text", trace)                # privacy: no error body
            self.assertNotIn("heuristic", trace)
        finally:
            E._rerank_available, E.RerankClient, E.load_secrets, E.assess = saved

    def test_natural_assessment_levels_from_thresholds(self) -> None:
        """No monkeypatch: extreme thresholds must move the same pool between levels."""

        index = build_index()
        saved = E._rerank_available
        E._rerank_available = lambda: False
        try:
            q = _unit(7)
            index.cfg["retrieval"] = {"auto": {"high_agreement": 0.0, "high_margin": 0.0, "low_agreement": 0.0}}
            t1: dict = {}
            E.retrieve(index, _FakeClient(q), "노트북 추천", top_k=3, q_vec=q, ranking="auto", trace=t1, source_room_id=ROOM_OLD)
            self.assertEqual(t1["auto_confidence"]["level"], "high")
            index.cfg["retrieval"] = {"auto": {"low_agreement": 1.1}}
            t2: dict = {}
            E.retrieve(index, _FakeClient(q), "노트북 추천", top_k=3, q_vec=q, ranking="auto", trace=t2, source_room_id=ROOM_OLD)
            self.assertEqual(t2["auto_confidence"]["level"], "low")
            self.assertTrue(t2["recovery_performed"])
            for key in ("agreement", "top_margin", "candidate_count", "lexical_dense_overlap", "source_diversity"):
                self.assertIn(key, t2["auto_confidence"])
        finally:
            E._rerank_available = saved


class SearchLimitsAndAliasTests(unittest.TestCase):
    def test_hybrid_limit_args_override_module_constants(self) -> None:
        index = build_index()
        old_ids = {d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD}
        trace: dict = {}
        E._hybrid_rank(
            fts=index.leaf_fts, vectors=index.leaf_vectors, ids=index.leaf_ids,
            query="영화 볼래", q_vec=_unit(3), weights=[1.0, 4.0], lock=index.lock,
            trace=trace, allowed_ids=old_ids, lexical_limit=2, dense_limit=2,
        )
        self.assertEqual(len(trace["leaf_lexical"]), 2)

    def test_lexical_query_gets_alias_labels(self) -> None:
        index = build_index()
        index.cfg["retrieval"] = {"person": {"identity": "config"}}  # legacy hand-written alias table
        q, extra = E._lexical_query_expanded(
            index, "가명 뭐래", {"recent": False, "intro": False, "targets": ["가명"], "source": "planner"}
        )
        self.assertIn("김가명", extra)
        self.assertIn("김가명", q)
        index.cfg["alias_expand_lexical"] = False
        q2, extra2 = E._lexical_query_expanded(
            index, "가명 뭐래", {"recent": False, "intro": False, "targets": ["가명"], "source": "planner"}
        )
        self.assertEqual(extra2, [])
        self.assertEqual(q2, "가명 뭐래")


class DedupAndFloorTests(unittest.TestCase):
    def test_finalize_skips_adjacent_leaves(self) -> None:
        from retrieval_post import finalize_ranked_ids

        index = build_index(with_summaries=False)
        room_ids = [d["doc_id"] for d in index.leaves if d["source_room_id"] == ROOM_NEW]
        room_ids.sort(key=lambda d: int(index.leaf_by_id[d]["start"]))
        picked, merged = finalize_ranked_ids(
            index, room_ids, top_k=3, intent={"recent": True}, mode="skip"
        )
        starts = [int(index.leaf_by_id[i]["start"]) for i in picked]
        self.assertTrue(all(abs(a - b) >= 12 for i, a in enumerate(starts) for b in starts[i + 1 :]))
        self.assertEqual(len(picked), 3)
        self.assertTrue(all(not extras for extras in merged.values()))

    def test_retrieve_merge_keeps_leaf_observation_ids(self) -> None:
        index = build_index()
        index.cfg["dedup_mode"] = "merge"
        q = _unit(7)
        hits = E.retrieve(
            index, _FakeClient(q), "점심 뭐 먹을까", top_k=3, q_vec=q,
            ranking="hybrid", source_room_id=ROOM_NEW, trace={},
        )
        self.assertTrue(hits)
        for hit in hits:
            self.assertIn("leaf_observation_ids", hit)
            self.assertTrue(set(hit["leaf_observation_ids"]) <= set(hit["observation_ids"]))

    def test_rerank_floor_model_map_and_none(self) -> None:
        from retrieval_post import resolve_rerank_min_score

        self.assertEqual(resolve_rerank_min_score(None, "x"), 0.5)
        self.assertEqual(resolve_rerank_min_score({"rerank_min_score": None}, "x"), 0.5)
        self.assertEqual(
            resolve_rerank_min_score(
                {"rerank_min_score": {"voyageai/rerank-2.5-lite": 0.4, "_default": 0.6}},
                "voyageai/rerank-2.5-lite",
            ),
            0.4,
        )
        self.assertEqual(
            resolve_rerank_min_score({"rerank_min_score": {"_default": 0.7}}, "other/rerank"),
            0.7,
        )

    def test_hybrid_cos_floor_abstains(self) -> None:
        index = build_index(with_summaries=False)
        index.cfg["hybrid_min_cos"] = 0.99
        q = _unit(8)
        trace: dict = {}
        hits = E.retrieve(
            index, _FakeClient(q), "그때 무슨 얘기 했었지", top_k=5, q_vec=q,
            ranking="hybrid", source_room_id=ROOM_OLD, trace=trace,
        )
        self.assertEqual(hits, [])
        self.assertTrue(trace.get("abstained"))

    def test_person_term_does_not_outrank_hybrid_scale(self) -> None:
        from retrieval_post import scaled_person

        self.assertLessEqual(scaled_person(6.0), 1.0001)
        index = build_index()
        query = "가명 뭐래"
        pool = index.leaf_ids[:12]
        picked = E._rerank_leaves(index, pool, query=query, pool=12, top_k=12)
        self.assertEqual(set(picked), set(pool))


class ConfigEnvTests(unittest.TestCase):
    def test_load_config_uses_masamong_sandbox_config(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "alt.json"
            path.write_text(json.dumps({"experiment": "from-env", "rooms": []}), encoding="utf-8")
            prev = os.environ.get("MASAMONG_SANDBOX_CONFIG")
            os.environ["MASAMONG_SANDBOX_CONFIG"] = str(path)
            try:
                self.assertEqual(E.load_config().get("experiment"), "from-env")
            finally:
                if prev is None:
                    os.environ.pop("MASAMONG_SANDBOX_CONFIG", None)
                else:
                    os.environ["MASAMONG_SANDBOX_CONFIG"] = prev


class SoftEventRetrievalTests(unittest.TestCase):
    """Soft events are an opt-in candidate view over unchanged leaf evidence."""

    def _soft_cfg(self, *, enabled: bool = True, **overrides):
        cfg = {
            "enabled": enabled,
            "version": "test-soft-v1",
            "gap_minutes": 1000,
            "min_leaves": 1,
            "max_leaves": 2,
            "max_chars": 1000,
            "event_limit": 20,
            "max_leaf_ids_per_event": 2,
            "rrf_weight": 1.0,
            "partial_scope": "skip",
            "counts_for_confidence": False,
            "reserve_in_pool": False,
            "pooling": "leaf_mean",
        }
        cfg.update(overrides)
        return cfg

    def _retrieve(self, index, *, query="점심 뭐 먹을까", room=ROOM_OLD, intent=None):
        q = _unit(1)
        trace: dict = {}
        hits = E.retrieve(
            index,
            _FakeClient(q),
            query,
            top_k=3,
            q_vec=q,
            ranking="hybrid",
            trace=trace,
            source_room_id=room,
            intent=intent,
        )
        return hits, trace

    @staticmethod
    def _without_timing(trace: dict) -> dict:
        return {
            key: value
            for key, value in trace.items()
            if not str(key).endswith("_ms") and key != "retrieve_ms"
        }

    def test_disabled_is_baseline_invariant(self) -> None:
        base = build_index()
        explicit = build_index()
        explicit.cfg["retrieval"] = {"soft_event": self._soft_cfg(enabled=False)}
        hits_a, trace_a = self._retrieve(base)
        hits_b, trace_b = self._retrieve(explicit)
        self.assertEqual(hits_a, hits_b)
        self.assertEqual(self._without_timing(trace_a), self._without_timing(trace_b))
        self.assertNotIn("soft_event", trace_a)
        self.assertNotIn("soft_event", trace_b)

    def test_enabled_projects_only_existing_leaf_ids_and_keeps_provenance(self) -> None:
        index = build_index()
        index.cfg["retrieval"] = {"soft_event": self._soft_cfg()}
        hits, trace = self._retrieve(index)
        self.assertTrue(hits)
        self.assertTrue({hit["doc_id"] for hit in hits} <= set(index.leaf_ids))
        self.assertIn("soft_event", trace)
        self.assertTrue(trace["soft_event"]["hits"]["lexical"])
        self.assertTrue(trace["soft_event"]["hits"]["dense"])
        self.assertTrue(trace["soft_event"]["projected_map"]["lexical"])
        self.assertIn("soft_event_lexical", trace["candidate_runs"])
        self.assertIn("soft_event_dense", trace["candidate_runs"])
        self.assertTrue(
            any("soft_event_" in source for row in trace["candidate_provenance"] for source in row["sources"])
        )

    def test_room_and_time_scope_skip_partial_events(self) -> None:
        index = build_index()
        index.cfg["retrieval"] = {"soft_event": self._soft_cfg()}
        base = datetime(2026, 9, 1, tzinfo=timezone.utc) - timedelta(days=200)
        intent = {
            "recent": False,
            "intro": False,
            "politics": False,
            "prefer_missing_over_guess": False,
            "time": {
                "kind": "between",
                "from": base.isoformat(),
                "to": (base + timedelta(hours=4)).isoformat(),
            },
        }
        hits, trace = self._retrieve(index, intent=intent)
        old_ids = {doc_id for doc_id in index.leaf_ids if index.leaf_by_id[doc_id]["source_room_id"] == ROOM_OLD}
        self.assertTrue(hits)
        self.assertTrue({hit["doc_id"] for hit in hits} <= old_ids)
        self.assertGreater(trace["soft_event"]["partial_scope_skipped"], 0)
        for name in ("soft_event_lexical", "soft_event_dense"):
            self.assertTrue(set(trace["candidate_runs"].get(name, [])) <= old_ids)

    def test_raw_evidence_has_no_event_search_text(self) -> None:
        index = build_index()
        index.cfg["retrieval"] = {"soft_event": self._soft_cfg()}
        hits, trace = self._retrieve(index)
        self.assertTrue(hits)
        self.assertTrue(trace["visible_observation_ids"])
        self.assertTrue(all("soft-event:" not in hit["excerpt"] for hit in hits))
        self.assertTrue(all("김가명 D대회 본선 이야기" not in hit["excerpt"] for hit in hits))
        for hit in hits:
            self.assertTrue(set(hit["observation_ids"]) >= set(hit["leaf_observation_ids"]))

    def test_baseline_mode_ignores_soft_event(self) -> None:
        base = build_index()
        enabled = build_index()
        enabled.cfg["retrieval"] = {"soft_event": self._soft_cfg()}
        q = _unit(1)
        trace_a: dict = {}
        trace_b: dict = {}
        hits_a = E.retrieve(base, _FakeClient(q), "점심 뭐 먹을까", top_k=3, mode="baseline", q_vec=q, ranking="hybrid", trace=trace_a, source_room_id=ROOM_OLD)
        hits_b = E.retrieve(enabled, _FakeClient(q), "점심 뭐 먹을까", top_k=3, mode="baseline", q_vec=q, ranking="hybrid", trace=trace_b, source_room_id=ROOM_OLD)
        self.assertEqual(hits_a, hits_b)
        self.assertNotIn("soft_event", trace_a)
        self.assertNotIn("soft_event", trace_b)

    def test_runtime_config_change_rebuilds_event_view(self) -> None:
        index = build_index()
        index.cfg["retrieval"] = {"soft_event": self._soft_cfg(max_leaves=2)}
        _hits_a, trace_a = self._retrieve(index)
        first = next(iter(index.soft_event_runtime_cache.values()))
        index.cfg["retrieval"]["soft_event"]["max_leaves"] = 4
        _hits_b, trace_b = self._retrieve(index)
        second = next(iter(index.soft_event_runtime_cache.values()))
        self.assertIsNot(first, second)
        self.assertEqual(trace_a["soft_event"]["recipe"]["max_leaves"], 2)
        self.assertEqual(trace_b["soft_event"]["recipe"]["max_leaves"], 4)
        self.assertNotEqual(trace_a["soft_event"]["count"], trace_b["soft_event"]["count"])

    def test_soft_event_does_not_change_auto_confidence_when_excluded(self) -> None:
        q = _unit(1)
        scope = {
            doc_id
            for doc_id in build_index().leaf_ids
            if build_index().leaf_by_id[doc_id]["source_room_id"] == ROOM_OLD
        }

        def generate(enabled: bool, *, counts: bool = False):
            index = build_index(seed=19)
            index.cfg["retrieval"] = {
                "soft_event": self._soft_cfg(
                    enabled=enabled,
                    rrf_weight=100.0,
                    counts_for_confidence=counts,
                )
            }
            fusion, _allowed, _trace, meta = E._generate_candidates(
                index,
                _FakeClient(q),
                "점심 뭐 먹을까",
                q_vec=q,
                intent={},
                scope=scope,
                use_summary=True,
                pool=40,
                lex_lim=200,
                den_lim=150,
            )
            confidence = meta.get("confidence_fusion") or fusion
            return fusion, E.assess(
                confidence,
                thresholds=E.retrieval_config(index.cfg)["auto"],
                targets_requested=False,
                targets_resolved=False,
                excluded_families=set() if counts else None,
            ).to_trace()

        baseline_fusion, baseline_confidence = generate(False)
        event_fusion, excluded_confidence = generate(True, counts=False)
        _counted_fusion, counted_confidence = generate(True, counts=True)
        self.assertEqual(excluded_confidence, baseline_confidence)
        self.assertNotEqual(event_fusion.weights, baseline_fusion.weights)
        self.assertNotEqual(counted_confidence, baseline_confidence)


class RecentWindowTests(unittest.TestCase):
    def test_recent_window_stays_in_old_room(self) -> None:
        from retrieval_post import narrow_recent_ids

        index = build_index()
        old = {d["doc_id"] for d in index.leaves if d["source_room_id"] == ROOM_OLD}
        narrowed, meta = narrow_recent_ids(
            index, old, {"recent": True, "intro": False, "targets": []}
        )
        self.assertTrue(meta["recent_window_applied"])
        self.assertTrue(set(narrowed or ()) <= old)
        self.assertTrue(narrowed)


class BlockViewTests(unittest.TestCase):
    def test_block_ids_filter_union_and_empty(self) -> None:
        index = build_index()
        E.populate_block_catalog(index)
        q = _unit(3)
        old_b = index.block_id_by_room[ROOM_OLD]
        new_b = index.block_id_by_room[ROOM_NEW]
        hits_old = E.retrieve(
            index, _FakeClient(q), "점심", top_k=8, ranking="hybrid", q_vec=q, block_ids=[old_b]
        )
        self.assertTrue(hits_old)
        self.assertTrue(all(h["source_room_id"] == ROOM_OLD for h in hits_old))
        self.assertTrue(all(h["block_id"] == old_b for h in hits_old))
        empty = E.retrieve(
            index, _FakeClient(q), "점심", top_k=8, ranking="hybrid", q_vec=q, block_ids=[]
        )
        self.assertEqual(empty, [])
        both = E.retrieve(
            index, _FakeClient(q), "점심", top_k=8, ranking="hybrid", q_vec=q, block_ids=[old_b, new_b]
        )
        rooms = {h["source_room_id"] for h in both}
        self.assertTrue(rooms <= {ROOM_OLD, ROOM_NEW})

    def test_catalog_write_leaves_vector_mtime(self) -> None:
        from memory_blocks import write_catalog

        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            vec = root / "leaf_vecs.npy"
            np.save(vec, np.zeros(4, dtype=np.float32))
            before = vec.stat().st_mtime_ns
            write_catalog(
                root,
                [
                    {
                        "block_id": "abc",
                        "source_room": "discord:1:1",
                        "leaf_ids": ["x"],
                        "status": "ready",
                    }
                ],
            )
            self.assertEqual(vec.stat().st_mtime_ns, before)
            self.assertTrue((root / "blocks" / "index.json").is_file())

    def test_catalog_write_is_safe_for_parallel_evaluators(self) -> None:
        from concurrent.futures import ThreadPoolExecutor
        from memory_blocks import load_catalog, write_catalog

        with tempfile.TemporaryDirectory() as td:
            root = Path(td)

            def write(number: int) -> None:
                write_catalog(
                    root,
                    [{
                        "block_id": f"block-{number}",
                        "source_room": f"discord:1:{number}",
                        "leaf_ids": [f"leaf-{number}"],
                        "status": "ready",
                    }],
                )

            with ThreadPoolExecutor(max_workers=8) as pool:
                list(pool.map(write, range(32)))
            with ProcessPoolExecutor(max_workers=4) as pool:
                list(pool.map(_write_catalog_process, [(str(root), n) for n in range(12)]))
            rows = load_catalog(root)
            self.assertEqual(len(rows), 1)
            self.assertTrue(str(rows[0]["block_id"]).startswith(("block-", "process-")))
            self.assertEqual(list((root / "blocks").glob("*.tmp")), [])


class EvidenceDateTests(unittest.TestCase):
    """Go search.contextOnlyDate / leafDate와 같은 규칙."""

    def test_context_only_hit_is_dated_by_what_it_shows(self) -> None:
        class Ix:
            messages = [
                {"observation_id": "a", "parsed_time": "2026-07-25T21:00:00"},
                {"observation_id": "b", "parsed_time": "2026-07-25T21:05:00"},
                {"observation_id": "c", "parsed_time": "2026-08-11T00:30:00"},
                {"observation_id": "d", "parsed_time": "2026-09-06T08:00:00+09:00"},
            ]
        leaf = {"start": 0, "end": 2, "observation_ids": ["a", "b"]}
        packed = {"start": 0, "end": 4, "visible_observation_ids": ["c", "d"]}
        self.assertEqual(E._context_only_date(Ix, packed, leaf), "2026-08-11~2026-09-06")
        self.assertEqual(E._leaf_source_date(Ix, leaf), "2026-07-25")  # naive: no host zone shift
        packed["visible_observation_ids"] = ["b", "c"]
        self.assertEqual(E._context_only_date(Ix, packed, leaf), "")
        packed["visible_observation_ids"] = ["c"]
        self.assertEqual(E._context_only_date(Ix, packed, leaf), "2026-08-11")


if __name__ == "__main__":
    unittest.main()


class RerankInputTests(unittest.TestCase):
    """§24 document representation and §26 pre-rerank dedup are config-driven and traceable."""

    def test_doc_formats(self) -> None:
        index = build_index()
        doc_id = next(d for d in index.leaf_ids if "D대회" in index.leaf_by_id[d]["body"])
        body = index.leaf_by_id[doc_id]["body"]
        self.assertEqual(E._rerank_document(index, doc_id, "body"), body[:2000])
        self.assertTrue(E._rerank_document(index, doc_id, "summary_body").startswith("김가명 D대회 본선 이야기\n"))
        self.assertTrue(E._rerank_document(index, doc_id, "speaker_body").startswith("[Speaker]\n"))
        self.assertIn("[Time]\n", E._rerank_document(index, doc_id, "time_body"))
        full = E._rerank_document(index, doc_id, "full")
        for tag in ("[Speaker]", "[Time]", "[Summary]", "[Content]"):
            self.assertIn(tag, full)
        self.assertLessEqual(len(E._rerank_document(index, doc_id, "full", limit=50)), 50)

    def test_pre_dedup_keeps_best_ranked_neighbour(self) -> None:
        index = build_index()
        old = [d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD]
        # leaf_size=4 in the fixture: consecutive leaves start 4 apart → not adjacent under "< leaf_size"
        kept, dropped = E._pre_rerank_dedup(index, old[:4])
        self.assertEqual(kept, old[:4]); self.assertEqual(dropped, [])
        index.cfg["leaf_size"] = 8
        kept, dropped = E._pre_rerank_dedup(index, old[:4])
        # starts 0,4,8,12 with leaf_size 8: 4 is adjacent to 0, 12 is adjacent to 8
        self.assertEqual(kept, [old[0], old[2]]); self.assertEqual(dropped, [old[1], old[3]])

    def test_rerank_receives_configured_format_and_dedup(self) -> None:
        index = build_index()
        index.cfg["leaf_size"] = 8
        index.cfg["retrieval"] = {"rerank": {"doc_format": "speaker_body", "pre_dedup": True}}
        seen: dict = {}

        class _Client:
            def __init__(self, *a, **k):  # noqa: ANN002, ANN003
                pass

            def rerank(self, query, docs, top_n=None):  # noqa: ANN001
                seen["docs"] = docs
                return _FakeRerankResult(list(range(len(docs))), [0.9] * len(docs))

        saved = (E.RerankClient, E.load_secrets)
        E.RerankClient, E.load_secrets = _Client, (lambda *a, **k: {})
        try:
            pool = [d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD][:6]
            trace: dict = {}
            E._rank_leaf_ids(index, pool, query="q", pool=6, top_k=3, ranking="rerank", remaining_budget=1.0, trace=trace)
        finally:
            E.RerankClient, E.load_secrets = saved
        self.assertEqual(trace["rerank_doc_format"], "speaker_body")
        self.assertEqual(trace["rerank_pre_dedup_dropped"], 3)
        self.assertEqual(len(seen["docs"]), 3)
        self.assertTrue(all(d.startswith("[Speaker]") for d in seen["docs"]))
