#!/usr/bin/env python3
"""R0/R1/R2 ablation that only scores gold bound to the current index.

R0 = hybrid only, R1 = hybrid + heuristic, R2 = hybrid + Cohere only (not R1).
If gold is missing or fingerprint mismatches, exit and require regen_bound_gold.py.
No unique-token probes, no silent observation-id remapping.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
sys.path.insert(0, str(CODE))
sys.path.insert(0, str(EVAL))

from client import PlayClient  # noqa: E402
from engine import (  # noqa: E402
    DATA,
    LEAF_VECS_PATH,
    _hybrid_rank,
    _rerank_leaves,
    load_config,
    load_index,
)
from openrouter import load_secrets  # noqa: E402
from rag_upgrade import RERANK_MODEL  # noqa: E402
from rag_upgrade.index_binding import (  # noqa: E402
    BindingError,
    assert_gold_bound_to_index,
    compute_index_binding,
    segmentation_version_from_cfg,
    stable_leaf_id_from_playground_leaf,
    validate_question_wants_against_leaves,
)
from rag_upgrade.rerank_client import RerankClient  # noqa: E402


def _rank_of_want(hits: list[dict[str, Any]], want: set[str]) -> int:
    for i, hit in enumerate(hits, start=1):
        obs = {str(x) for x in (hit.get("observation_ids") or [])}
        if obs & want:
            return i
    return 0


def _metrics_from_ranks(ranks: list[int]) -> dict[str, float]:
    n = len(ranks) or 1
    return {
        "n": float(len(ranks)),
        "recall@1": sum(1 for r in ranks if r == 1) / n,
        "recall@3": sum(1 for r in ranks if 1 <= r <= 3) / n,
        "recall@5": sum(1 for r in ranks if 1 <= r <= 5) / n,
        "recall@10": sum(1 for r in ranks if 1 <= r <= 10) / n,
        "mrr": sum((1.0 / r) for r in ranks if r > 0) / n,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--gold",
        type=Path,
        default=REPO
        / ".masamong-scratch"
        / "rag-upgrade-ablation-20260916"
        / "gold.bound.json",
    )
    ap.add_argument("--pool", type=int, default=40)
    ap.add_argument("--top-k", type=int, default=10)
    ap.add_argument("--budget-usd", type=float, default=1.0)
    ap.add_argument("--limit", type=int, default=0, help="0 = all gold questions")
    ap.add_argument("--reviewed-only", action="store_true")
    ap.add_argument(
        "--retrieval-only",
        action="store_true",
        help="expected_abstention 문항(정답 leaf 없음)은 검색 지표에서 제외한다",
    )
    ap.add_argument(
        "--room-scoped",
        action="store_true",
        help="런타임(_prepare_bot_turn)처럼 gold leaf의 source_room_id 안에서만 검색·채점한다",
    )
    ap.add_argument(
        "--out",
        type=Path,
        default=REPO / ".masamong-scratch" / "rag-upgrade-ablation-20260916",
    )
    args = ap.parse_args()
    # os.chdir 전에 절대화 — 상대 --out/--gold가 샌드박스 폴더로 떨어지지 않게.
    args.out = Path(args.out).resolve()

    if not args.gold.exists():
        print(
            json.dumps(
                {
                    "error": "gold_missing",
                    "hint": "python3 tools/evaluation/rag_upgrade/regen_bound_gold.py --questions 20",
                    "gold": str(args.gold),
                }
            )
        )
        return 2

    gold = json.loads(args.gold.read_text(encoding="utf-8"))
    os.chdir(CODE)
    cfg = load_config()
    index = load_index()
    meta_path = DATA / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    binding = compute_index_binding(
        leaves=index.leaves,
        messages=index.messages,
        leaf_vecs_path=LEAF_VECS_PATH,
        cfg=cfg,
        meta=meta,
    )
    seg = segmentation_version_from_cfg(cfg)
    for leaf in index.leaves:
        leaf["stable_leaf_id"] = stable_leaf_id_from_playground_leaf(
            leaf, segmentation_version=seg
        )

    try:
        assert_gold_bound_to_index(gold, binding)
        questions = list(gold.get("questions") or [])
        if args.reviewed_only:
            questions = [
                q
                for q in questions
                if str(q.get("review_status") or "") == "reviewed"
                and str(q.get("review_verdict") or "") == "accept"
            ]
        if args.retrieval_only:
            questions = [q for q in questions if not bool(q.get("expected_abstention"))]
        if args.limit > 0:
            questions = questions[: args.limit]
        validate_question_wants_against_leaves(questions, index.leaves)
    except BindingError as exc:
        print(
            json.dumps(
                {
                    "error": "gold_not_bound",
                    "detail": str(exc),
                    "hint": "python3 tools/evaluation/rag_upgrade/regen_bound_gold.py --questions 20",
                    "current_fingerprint": binding.get("index_fingerprint"),
                },
                ensure_ascii=False,
            )
        )
        return 2

    secrets = load_secrets(REPO / ".private" / "eval" / "secrets.env")
    play = PlayClient(secrets=secrets, budget_usd=args.budget_usd * 0.55)
    rerank = RerankClient(
        secrets,
        model=RERANK_MODEL,
        budget_usd=args.budget_usd * 0.45,
        max_request_cost_usd=0.05,
        base_url=secrets.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
    )
    weights = [float(w) for w in cfg["rrf_weights"]]
    print(
        json.dumps(
            {
                "stage": "start",
                "n_questions": len(questions),
                "index_fingerprint": binding["index_fingerprint"][:16],
                "pool": args.pool,
                "top_k": args.top_k,
            }
        )
    )

    ranks = {"R0": [], "R1": [], "R2": []}
    per_q: list[dict[str, Any]] = []
    t0 = time.perf_counter()
    # gold가 다른 분할(corpus-only binding)로 만들어졌으면 source_leaf_doc_id가 없다.
    # 그때는 want observation id가 속한 leaf의 방으로 범위를 잡는다.
    room_by_obs: dict[str, str] = {}
    if args.room_scoped:
        for leaf in index.leaves:
            room = str(leaf.get("source_room_id") or "")
            for oid in leaf.get("observation_ids") or []:
                room_by_obs.setdefault(str(oid), room)

    for qi, qrow in enumerate(questions):
        q = str(qrow.get("query") or qrow.get("question") or "")
        want = {str(x) for x in (qrow.get("want_observation_ids") or qrow.get("expected_evidence_ids") or [])}
        q_vec = np.asarray(
            play.embed(
                [q],
                kind="query",
                instruction=str(cfg.get("embed_query_instruction") or ""),
            )[0],
            dtype=np.float32,
        )
        q_vec /= np.linalg.norm(q_vec) + 1e-9
        allowed: set[str] | None = None
        if args.room_scoped:
            src_leaf = index.leaf_by_id.get(str(qrow.get("source_leaf_doc_id") or ""))
            room = str((src_leaf or {}).get("source_room_id") or "")
            if not room:
                room = next((room_by_obs[o] for o in want if o in room_by_obs), "")
            if room:
                allowed = {d["doc_id"] for d in index.leaves if d.get("source_room_id") == room}
        hybrid_ids = _hybrid_rank(
            fts=index.leaf_fts,
            vectors=index.leaf_vectors,
            ids=index.leaf_ids,
            query=q,
            q_vec=q_vec,
            weights=weights,
            lock=index.lock,
            allowed_ids=allowed,
        )
        r0_ids = hybrid_ids[: args.pool]
        r1_ids = _rerank_leaves(
            index, hybrid_ids, query=q, pool=args.pool, top_k=args.pool, allowed_ids=allowed
        )[: args.pool]

        def _hits(doc_ids: list[str], *, k: int | None = None) -> list[dict[str, Any]]:
            out = []
            cut = args.top_k if k is None else k
            for doc_id in doc_ids[:cut]:
                leaf = index.leaf_by_id[doc_id]
                out.append(
                    {
                        "doc_id": doc_id,
                        "observation_ids": list(leaf.get("observation_ids") or []),
                    }
                )
            return out

        docs = [str(index.leaf_by_id[i].get("body") or "")[:2000] for i in r0_ids]
        try:
            rr = rerank.rerank(q, docs, top_n=min(len(docs), args.pool))
            r2_ids = [r0_ids[j] for j in rr.indices]
            extra = {"rerank_cost_usd": rr.cost_usd, "rerank_latency_ms": rr.latency_ms}
        except Exception as exc:  # noqa: BLE001
            r2_ids = list(r0_ids)
            extra = {"rerank_error": type(exc).__name__}

        row = {
            "question_id": qrow.get("question_id"),
            "variants": {},
            **extra,
        }
        for name, ids in ("R0", r0_ids), ("R1", r1_ids), ("R2", r2_ids):
            rank = _rank_of_want(_hits(ids), want)
            ranks[name].append(rank)
            row["variants"][name] = {"hit_rank": rank}
        pool_rank = _rank_of_want(_hits(r0_ids, k=len(r0_ids)), want)
        row["pool_hit"] = pool_rank > 0
        row["pool_rank"] = pool_rank
        per_q.append(row)
        if (qi + 1) % 5 == 0 or qi == 0:
            print(json.dumps({"stage": "progress", "done": qi + 1, "of": len(questions)}))

    summary = {
        "dataset": str(args.gold),
        "index_fingerprint": binding["index_fingerprint"],
        "n_questions": len(per_q),
        "pool": args.pool,
        "top_k": args.top_k,
        "room_scoped": bool(args.room_scoped),
        "retrieval_only": bool(args.retrieval_only),
        "segmentation_version": binding.get("segmentation_version"),
        "variants": {k: _metrics_from_ranks(v) for k, v in ranks.items()},
        "pool_recall": (sum(1 for r in per_q if r.get("pool_hit")) / len(per_q)) if per_q else 0.0,
        "embed_cost_usd": play.usage.cost_usd,
        "rerank_cost_usd": rerank.spent_usd,
        "elapsed_sec": round(time.perf_counter() - t0, 2),
        "not_human_reviewed": gold.get("gold_kind") != "human_reviewed",
        "promote": "needs more evidence",
        "binding": "enforced",
    }
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "summary.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    (args.out / "per_question_metrics.json").write_text(
        json.dumps(per_q, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps({"stage": "done", "summary": summary}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
