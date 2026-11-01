"""`retrieval` config block for R0 v2 / auto v2 (defaults + deep merge, no I/O).

Everything the candidate generator and the auto cascade tune lives under `cfg["retrieval"]`
so it does not collide with the flat legacy keys (`retrieve_pool`, `lexical_limit`, …), which
remain the R0 baseline knobs. Numbers here are starting points, not truths — 07 experiment log
records what each was measured against.
"""

from __future__ import annotations

from copy import deepcopy
from typing import Any, Mapping

DEFAULTS: dict[str, Any] = {
    "person": {
        "enabled": True,
        "limit": 60,  # speaker-scoped dense/lexical run length
        "rrf_weight": 0.3,  # low: ranking stays multi-query-led; pool membership via guarantee_pool
        "max_speakers_per_target": 3,  # more distinct speakers than this → ambiguous, skip
        # observed (default): labels one native account used, derived from the index — no hand table ·
        # config: legacy hand-written person_aliases · both
        "identity": "observed",
        # resolve targets only among speakers inside the caller's allowed scope
        "resolve_in_scope": True,
    },
    "alias": {
        "enabled": True,
        "limit": 60,  # leaves mentioning the person's labels (lexical)
        "rrf_weight": 0.15,
    },
    "summaries": {"enabled": True},
    # Candidate run over single-message vectors (message_vecs.npy + message_leaf.npy next to the other
    # artifacts): the leaves are ranked by their best message and the run joins the finished fusion as one
    # more weighted list. Mirrors retrieval.message_dense in Go (internal/memory/search). Off: nothing
    # is loaded, run or traced.
    "message_dense": {
        "enabled": False,
        "limit": 200,  # leaves in the run
        # weight of the run in the final fusion (the base ranking has weight 1). Measured on real data:
        # 1.0 pushed flow (span) questions down by 3-5 questions per set without a reranker, 0.1 kept
        # them within +-1 and kept the gains after reranking. Mirrors search.DefaultMessageDenseWeight.
        "rrf_weight": 0.1,
    },
    # Runtime-only derived event candidates.  These never rewrite the stable
    # leaf artifacts and are deliberately opt-in until an offline gold set
    # shows a regression-free gain.
    "soft_event": {
        "enabled": False,
        "version": "soft-event-v1",
        "gap_minutes": 60,
        "min_leaves": 1,
        "max_leaves": 8,
        "max_chars": 4000,
        "event_limit": 40,
        "max_leaf_ids_per_event": 8,
        "rrf_weight": 0.2,
        "partial_scope": "skip",
        "counts_for_confidence": False,
        "reserve_in_pool": False,
        "pooling": "leaf_mean",
    },
    "multi_query": {
        "enabled": True,
        "max_queries": 3,  # original + semantic_query + key_terms
        "semantic_rrf_weight": 1.0,
        "key_terms_rrf_weight": 0.5,
    },
    "candidate_pool": {
        "default": 100,
        "min": 40,
        "max": 160,
        # pool membership guarantee: top-N of every retriever run enters the pool even when its
        # fusion weight is low (order stays fusion-driven). 0 = off.
        "per_source_min": 5,
        "reserve_max_frac": 0.2,
    },
    "auto": {
        "enabled": True,
        "assess_top_n": 10,
        # rule-based confidence thresholds (see candidates_assess.assess)
        "high_agreement": 0.6,
        "high_margin": 0.15,
        "low_agreement": 0.2,
        "min_candidates": 3,
        "min_target_coverage": 0.3,
    },
    "recovery": {
        "enabled": True,
        "max_attempts": 1,
        "pool_scale": 1.5,
        "limit_scale": 1.5,
        "time_window_scale": 2.0,
    },
    "rerank": {
        "enabled": True,
        "doc_format": "summary_body",  # body | summary_body | speaker_body | time_body | full
        "pre_dedup": False,  # cheap adjacent-leaf dedup before the reranker call
    },
}


def _merge(base: dict[str, Any], over: Mapping[str, Any]) -> dict[str, Any]:
    out = deepcopy(base)
    for key, value in over.items():
        if isinstance(value, Mapping) and isinstance(out.get(key), dict):
            out[key] = _merge(out[key], value)
        else:
            out[key] = deepcopy(value)
    return out


def retrieval_config(cfg: Mapping[str, Any] | None) -> dict[str, Any]:
    raw = (cfg or {}).get("retrieval")
    if not isinstance(raw, Mapping):
        return deepcopy(DEFAULTS)
    return _merge(DEFAULTS, raw)


def pool_size(cfg: Mapping[str, Any] | None, *, top_k: int, recovery: bool = False) -> int:
    """Candidate pool for one attempt. Legacy `retrieve_pool` wins when set (R0 baseline knob);
    recovery scales it up to `candidate_pool.max`."""

    rc = retrieval_config(cfg)["candidate_pool"]
    legacy = (cfg or {}).get("retrieve_pool")
    base = int(legacy) if legacy else int(rc.get("default") or max(40, top_k * 8))
    lo, hi = int(rc.get("min") or 1), int(rc.get("max") or base)
    if recovery:
        base = int(round(base * float(retrieval_config(cfg)["recovery"].get("pool_scale") or 1.5)))
    return max(lo, min(hi, base))
