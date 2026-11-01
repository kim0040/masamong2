"""Synthetic ranking ablation.

R0 = hybrid only, R1 = hybrid + heuristic, R2 = hybrid + Cohere only.
R2 does not include R1. Combined Cohere+heuristic is R3 (not in this harness).
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass
from typing import Any, Callable, Mapping, Sequence

from .gold_schema import GoldQuestion, score_retrieval
from .rerank_cohere import CohereRerankClient, RerankResult, apply_rerank_order


Tokenize = Callable[[str], set[str]]


def _default_tokenize(text: str) -> set[str]:
    return {t for t in re.findall(r"[\w가-힣]+", text.lower()) if len(t) >= 2}


@dataclass(frozen=True)
class Doc:
    doc_id: str
    text: str
    observation_ids: tuple[str, ...]


def hybrid_rank(
    query: str,
    docs: Sequence[Doc],
    *,
    tokenize: Tokenize | None = None,
) -> list[str]:
    """Tiny lexical RRF-like ranker for offline tests (no embeddings)."""

    tok = tokenize or _default_tokenize
    q = tok(query)
    scored: list[tuple[float, str]] = []
    for d in docs:
        dt = tok(d.text)
        overlap = len(q & dt)
        # prefer denser overlap / length-normalized
        score = overlap / math.sqrt(1 + len(dt))
        scored.append((score, d.doc_id))
    scored.sort(key=lambda x: (-x[0], x[1]))
    return [doc_id for _s, doc_id in scored]


def heuristic_rerank(
    query: str,
    docs: Mapping[str, Doc],
    ranked: Sequence[str],
) -> list[str]:
    """Stand-in for BETA person/recency heuristics: boost exact query token hits."""

    q = set(_default_tokenize(query))
    boosted: list[tuple[float, int, str]] = []
    for i, doc_id in enumerate(ranked):
        doc = docs[doc_id]
        dt = set(_default_tokenize(doc.text))
        boost = len(q & dt) * 0.1
        boosted.append((-boost, i, doc_id))
    boosted.sort()
    return [doc_id for _b, _i, doc_id in boosted]


def expand_docs_to_observations(
    docs: Mapping[str, Doc], ranked_doc_ids: Sequence[str]
) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for doc_id in ranked_doc_ids:
        for oid in docs[doc_id].observation_ids:
            if oid not in seen:
                seen.add(oid)
                out.append(oid)
    return out


def run_variant(
    *,
    variant: str,
    query: str,
    docs: Sequence[Doc],
    question: GoldQuestion,
    rerank_client: CohereRerankClient | None = None,
    k_list: Sequence[int] = (1, 3, 5, 10),
) -> dict[str, Any]:
    by_id = {d.doc_id: d for d in docs}
    ranked = hybrid_rank(query, docs)
    meta: dict[str, Any] = {"variant": variant, "hybrid_top": ranked[:10]}

    if variant == "R0":
        pass
    elif variant == "R1":
        ranked = heuristic_rerank(query, by_id, ranked)
    elif variant == "R2":
        if rerank_client is None:
            raise ValueError("R2 requires rerank_client")
        texts = [by_id[i].text for i in ranked]
        result: RerankResult = rerank_client.rerank(query, texts, top_n=len(texts))
        ranked = apply_rerank_order(ranked, result)
        meta["rerank_cost_usd"] = result.cost_usd
        meta["rerank_latency_ms"] = result.latency_ms
    else:
        raise ValueError(f"unknown variant {variant}")

    obs = expand_docs_to_observations(by_id, ranked)
    metrics = {f"recall@{k}": score_retrieval(question, obs, k=k) for k in k_list}
    meta["ranked_docs"] = ranked
    meta["ranked_observations"] = obs
    meta["metrics"] = metrics
    return meta


def aggregate_mrr(results: Sequence[Mapping[str, Any]], *, k: int = 10) -> float:
    if not results:
        return 0.0
    total = 0.0
    for row in results:
        total += float(row["metrics"][f"recall@{k}"]["mrr_contrib"])
    return total / len(results)
