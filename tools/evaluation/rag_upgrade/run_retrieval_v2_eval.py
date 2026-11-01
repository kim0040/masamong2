#!/usr/bin/env python3
"""Retrieval v2 evaluation: runs the *runtime* `engine.retrieve()` per variant and separates
candidate-generation metrics from ranking metrics.

Unlike run_sample_dataset_ablation.py (which re-implements R0 as leaf-only hybrid and sends
body-only docs to the reranker), this harness calls the same code path the sandbox bot uses:
summary RRF, alias expansion, person/alias retrievers, multi-query, recency window, auto v2
cascade, relevance floor, span merge. Numbers here are therefore what the bot would see.

Measures (per variant, overall and by gold `question_type`):
  Recall@1/5/10, MRR                          ranking quality on the final evidence
  pool_recall                                 wanted leaf inside the R2-eligible candidate pool
                                              (fusion head + guarantee_pool extras)
  fusion_pool_recall                          wanted leaf inside fusion top-pool (no guarantee)
  guarantee_rescue_rate                       gold in pool only because of guarantee extras
  per-retriever recall / unique contribution  which candidate source found it (R0 v2 §35)
  auto: rerank/recovery invocation rate, unnecessary rerank rate, recovery success,
        route accuracy, latency p50/p95, cost per query / per successful retrieval (§37)

Planner intent: `--plans cache.json` (built with `--plan`, one planner call per question,
cached) so evaluation uses the same structured intent as the runtime; without it the engine's
regex fallback is used (documented as weaker).

Gold must be bound to the current index (08-gold-index-binding); unbound gold exits 2.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import statistics
import sys
import threading
import time
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

import numpy as np

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
sys.path.insert(0, str(CODE))
sys.path.insert(0, str(EVAL))

from bot_plan import PLAN_PROMPT, intent_from_plan, memory_query, parse_plan  # noqa: E402
from client import PlayClient, map_parallel  # noqa: E402
from engine import (  # noqa: E402
    DATA,
    LEAF_VECS_PATH,
    DEFAULT_EMBED_QUERY_INSTRUCTION,
    apply_vector_dims,
    configured_models,
    load_config,
    load_index,
    retrieve,
)
from openrouter import load_secrets  # noqa: E402
from rag_upgrade.index_binding import (  # noqa: E402
    BindingError,
    assert_gold_bound_to_index,
    compute_index_binding,
    segmentation_version_from_cfg,
    stable_leaf_id_from_playground_leaf,
    validate_question_wants_against_leaves,
)
from rag_upgrade.rerank_client import RerankBudgetLedger  # noqa: E402

VARIANT_TO_RANKING = {"R0": "hybrid", "R1": "heuristic", "R2": "rerank", "auto": "auto"}
VISIBLE_CUTOFFS = (1, 3, 5, 10)


class CachingClient:
    """PlayClient wrapper that memoises query embeddings on disk (same model+instruction+text →
    same vector), so a sweep of configs does not pay embedding latency/cost repeatedly."""

    def __init__(self, inner: PlayClient, path: Path | None) -> None:
        self.inner = inner
        self.path = path
        self.cache: dict[str, list[float]] = {}
        if path is not None and path.exists():
            self.cache = json.loads(path.read_text(encoding="utf-8"))
        self.hits = 0
        self.misses = 0

    @property
    def usage(self):  # noqa: ANN201
        return self.inner.usage

    @property
    def embed_model(self) -> str:
        return self.inner.embed_model

    def _key(self, text: str, kind: str, instruction: str) -> str:
        return f"{self.inner.embed_model}\x1f{kind}\x1f{instruction}\x1f{text}"

    def embed(self, texts: list[str], *, kind: str = "query", instruction: str = "", **kw: Any) -> list[list[float]]:
        out: list[list[float] | None] = [None] * len(texts)
        todo: list[int] = []
        for i, t in enumerate(texts):
            v = self.cache.get(self._key(t, kind, instruction))
            if v is not None:
                out[i] = v
                self.hits += 1
            else:
                todo.append(i)
        if todo:
            vecs = self.inner.embed([texts[i] for i in todo], kind=kind, instruction=instruction, **kw)
            for i, v in zip(todo, vecs):
                out[i] = list(map(float, v))
                self.cache[self._key(texts[i], kind, instruction)] = out[i]
                self.misses += 1
        return [v for v in out if v is not None]

    def chat(self, *a: Any, **kw: Any) -> str:
        return self.inner.chat(*a, **kw)

    def save(self) -> None:
        if self.path is not None:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self.path.write_text(json.dumps(self.cache), encoding="utf-8")


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


def _hit_rank(hits: list[dict[str, Any]], want_obs: set[str], index: Any) -> int:
    """Rank of the first hit whose *leaf* (plus merged adjacent leaves) covers a wanted obs id.
    Leaf-level, not parent-expanded span — same strictness as the legacy ablation."""

    for i, hit in enumerate(hits, start=1):
        obs = {str(x) for x in (hit.get("leaf_observation_ids") or hit.get("observation_ids") or [])}
        for extra in hit.get("merged_leaf_ids") or []:
            obs |= {str(x) for x in (index.leaf_by_id.get(extra) or {}).get("observation_ids") or []}
        if obs & want_obs:
            return i
    return 0


def _id_set(value: Any) -> set[str]:
    """Normalize an observation-id field without retaining any message text."""

    if value is None:
        return set()
    if isinstance(value, (str, bytes, bytearray)):
        return {str(value)} if str(value) else set()
    try:
        return {str(item) for item in value if str(item)}
    except TypeError:
        return {str(value)} if str(value) else set()


def _required_evidence_groups(question: Mapping[str, Any]) -> tuple[tuple[frozenset[str], ...], str]:
    """Return AND-groups of OR alternatives and the gold representation used.

    Newer evidence gold uses ``required_evidence_groups``.  Older retrieval-v2
    gold uses ``want_observation_ids``; that entire list is one required set so
    any-hit and all-required-hit remain distinguishable.
    """

    groups_raw = question.get("required_evidence_groups")
    if groups_raw is not None:
        groups: list[frozenset[str]] = []
        try:
            iterator = iter(groups_raw)
        except TypeError:
            iterator = iter(())
        for group in iterator:
            if isinstance(group, Mapping):
                alternatives = group.get("alternatives")
                if alternatives is None:
                    alternatives = group.get("observation_ids")
                if alternatives is None:
                    alternatives = group.get("required_observation_ids")
            else:
                alternatives = group
            ids = frozenset(_id_set(alternatives))
            if ids:
                groups.append(ids)
        return tuple(groups), "groups"

    legacy = question.get("want_observation_ids")
    if legacy is None:
        legacy = question.get("expected_evidence_ids")
    ids = frozenset(_id_set(legacy))
    return ((ids,) if ids else ()), "set"


def _forbidden_observation_ids(question: Mapping[str, Any]) -> frozenset[str]:
    forbidden = question.get("forbidden_observation_ids")
    if forbidden is None:
        forbidden = question.get("forbidden_evidence_ids")
    return frozenset(_id_set(forbidden))


def _visible_ids_from_hit(hit: Mapping[str, Any]) -> frozenset[str]:
    """Use only the runtime hit's packed visible IDs for new evidence metrics."""

    return frozenset(_id_set(hit.get("visible_observation_ids")))


def _visible_evidence_metrics(
    hits: list[dict[str, Any]], question: Mapping[str, Any]
) -> dict[str, Any]:
    """Calculate message-level visibility metrics without copying raw/query text.

    Ranks are hit ranks (1-based); zero means the evidence was not visible in
    the returned hits.  ``visible_any_rank`` needs one required observation,
    while ``visible_all_rank`` needs every AND-group satisfied by one of its
    alternatives.  For legacy ``want_observation_ids`` all IDs form one group,
    making the two ranks intentionally different.
    """

    groups, mode = _required_evidence_groups(question)
    required = set().union(*(set(group) for group in groups)) if groups else set()
    forbidden = _forbidden_observation_ids(question)
    visible_any_rank = 0
    visible_all_rank = 0
    forbidden_visible_rank = 0
    cumulative: set[str] = set()
    for rank, hit in enumerate(hits, start=1):
        cumulative.update(_visible_ids_from_hit(hit))
        if required and not visible_any_rank and cumulative & required:
            visible_any_rank = rank
        if groups and not visible_all_rank:
            all_satisfied = (
                required <= cumulative
                if mode == "set"
                else all(cumulative & set(group) for group in groups)
            )
            if all_satisfied:
                visible_all_rank = rank
        if forbidden and not forbidden_visible_rank and cumulative & forbidden:
            forbidden_visible_rank = rank

    result: dict[str, Any] = {
        "required_evidence_mode": mode,
        "required_evidence_count": len(required),
        "required_evidence_group_count": len(groups),
        "visible_any_rank": visible_any_rank,
        "visible_all_rank": visible_all_rank,
        # Descriptive aliases make the distinction explicit to downstream readers.
        "any_evidence_rank": visible_any_rank,
        "all_evidence_rank": visible_all_rank,
        "forbidden_visible_hit": bool(forbidden_visible_rank),
        "forbidden_visible_rank": forbidden_visible_rank,
    }
    for cutoff in VISIBLE_CUTOFFS:
        result[f"visible_recall@{cutoff}"] = bool(visible_any_rank and visible_any_rank <= cutoff)
        result[f"all_required_visible@{cutoff}"] = bool(visible_all_rank and visible_all_rank <= cutoff)
    return result


def _parse_csv_filter(value: str | None) -> frozenset[str]:
    if not value:
        return frozenset()
    return frozenset(part.strip() for part in str(value).split(",") if part.strip())


def _question_matches_filters(
    question: Mapping[str, Any],
    *,
    splits: frozenset[str] = frozenset(),
    question_types: frozenset[str] = frozenset(),
) -> bool:
    if splits and str(question.get("split") or "") not in splits:
        return False
    if question_types:
        types = _id_set(question.get("types"))
        qtype = str(question.get("question_type") or "")
        if qtype:
            types.add(qtype)
        if not types & set(question_types):
            return False
    return True


def _filter_questions(
    questions: list[dict[str, Any]],
    *,
    splits: frozenset[str] = frozenset(),
    question_types: frozenset[str] = frozenset(),
) -> list[dict[str, Any]]:
    return [
        question
        for question in questions
        if _question_matches_filters(question, splits=splits, question_types=question_types)
    ]


def _question_fold(question_id: str, fold_count: int) -> int:
    if fold_count < 1:
        raise ValueError("fold_count must be >= 1")
    digest = hashlib.sha256(str(question_id).encode("utf-8")).hexdigest()
    return int(digest, 16) % fold_count


def _filter_questions_by_fold(
    questions: list[dict[str, Any]], fold_count: int | None, fold_index: int | None
) -> list[dict[str, Any]]:
    if fold_count is None and fold_index is None:
        return list(questions)
    if fold_count is None or fold_index is None:
        raise ValueError("fold_count and fold_index must be provided together")
    if fold_count < 1 or fold_index < 0 or fold_index >= fold_count:
        raise ValueError("fold_index must satisfy 0 <= fold_index < fold_count")
    return [
        question
        for question in questions
        if _question_fold(str(question.get("question_id") or ""), fold_count) == fold_index
    ]


def build_arg_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser()
    ap.add_argument("--gold", type=Path, default=REPO / ".masamong-scratch" / "rag-fix-20260917" / "gold.v3.json")
    ap.add_argument("--variants", default="R0,R2,auto", help="comma list of R0,R1,R2,auto")
    ap.add_argument("--top-k", type=int, default=10)
    ap.add_argument("--pool", type=int, default=0, help="0 = config retrieve_pool")
    ap.add_argument("--budget-usd", type=float, default=1.0)
    ap.add_argument(
        "--rerank-budget-usd",
        type=float,
        default=None,
        help="cumulative rerank cap; defaults to --budget-usd (separate from that cap)",
    )
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--retrieval-only", action="store_true", help="drop expected_abstention questions")
    ap.add_argument("--no-room-scope", action="store_true", help="global search (runtime is room-scoped)")
    ap.add_argument("--plans", type=Path, default=None, help="planner output cache (json)")
    ap.add_argument("--plan", action="store_true", help="run the planner for missing questions and cache")
    ap.add_argument("--plan-workers", type=int, default=1, help="parallel planner calls")
    ap.add_argument("--q-workers", type=int, default=8,
                    help="questions evaluated in parallel (1 = sequential, the pre-2026-09-23 behaviour)")
    ap.add_argument("--plan-model", default="", help="planner model override for the cache (default: config planner)")
    ap.add_argument("--plan-only", action="store_true", help="build the planner cache and exit")
    ap.add_argument("--config-override", default="", help="JSON merged over config.json (e.g. retrieval knobs)")
    ap.add_argument("--query-source", choices=["planner", "original"], default="original",
                    help="original = user message as q0 (runtime). planner = replace q0 with memory.search.query (ablation)")
    ap.add_argument("--embed-cache", type=Path, default=None, help="query-embedding cache (json)")
    ap.add_argument("--tag", default="")
    ap.add_argument("--time-now", default="", help="ISO-8601 replay clock for relative time kinds; empty = unresolved/no newest-leaf fallback")
    ap.add_argument("--splits", default="", help="comma-separated split filter; empty = all")
    ap.add_argument("--question-types", default="", help="comma-separated question_type filter; empty = all")
    ap.add_argument("--fold-count", type=int, default=None, help="deterministic question_id hash fold count")
    ap.add_argument("--fold-index", type=int, default=None, help="deterministic question_id hash fold index")
    ap.add_argument("--out", type=Path, required=True)
    return ap


def _resolve_rerank_budget_usd(budget_usd: Any, requested: Any | None) -> float:
    """Resolve the run-wide rerank cap without allowing an invalid spend cap."""

    try:
        total = float(budget_usd)
    except (TypeError, ValueError) as exc:
        raise ValueError("budget_usd must be a positive finite number") from exc
    if not math.isfinite(total) or total <= 0:
        raise ValueError("budget_usd must be a positive finite number")
    if requested is None:
        return total
    try:
        rerank = float(requested)
    except (TypeError, ValueError) as exc:
        raise ValueError("rerank_budget_usd must be a positive finite number") from exc
    if not math.isfinite(rerank) or rerank <= 0:
        raise ValueError("rerank_budget_usd must be a positive finite number")
    return rerank


def _attach_rerank_budget_ledger(index: Any, budget_usd: float) -> RerankBudgetLedger:
    """Attach one cumulative ledger to the index for this whole evaluation."""

    ledger = RerankBudgetLedger(budget_usd)
    # ``setattr`` keeps this runner compatible while the engine field lands in
    # parallel, and also works with the lightweight fake index used by tests.
    setattr(index, "rerank_budget_ledger", ledger)
    return ledger


def _rerank_budget_summary(
    ledger: RerankBudgetLedger, *, play_budget_usd: Any | None = None
) -> dict[str, Any]:
    """Return privacy-safe cumulative rerank accounting for run markers."""

    summary: dict[str, Any] = {
        "rerank_budget_usd": round(float(ledger.budget_usd), 6),
        "spent_usd": round(float(ledger.spent_usd), 6),
        "reserved_usd": round(float(ledger.reserved_usd), 6),
        "verified_cost_usd": round(float(ledger.verified_cost_usd), 6),
        "estimated_cost_usd": round(float(ledger.estimated_cost_usd), 6),
        "unknown_cost_usd": round(float(ledger.unknown_cost_usd), 6),
        "unknown_cost_attempts": int(ledger.unknown_cost_attempts),
        "state": str(ledger.budget_state),
    }
    if play_budget_usd is not None:
        play_budget = float(play_budget_usd)
        summary["play_budget_usd"] = round(play_budget, 6)
        summary["authorized_combined_cap_usd"] = round(
            play_budget + float(ledger.budget_usd), 6
        )
    return summary


def _pct(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = max(0, min(len(s) - 1, int(round(q * (len(s) - 1)))))
    return float(s[k])


def _build_plans(
    questions: list[dict[str, Any]],
    play: PlayClient,
    cfg: dict[str, Any],
    path: Path,
    *,
    workers: int = 1,
    model: str | None = None,
) -> dict[str, Any]:
    plans: dict[str, Any] = {}
    if path.exists():
        plans = json.loads(path.read_text(encoding="utf-8"))
    model = model or configured_models(cfg)["planner"]
    todo = [q for q in questions if str(q.get("question_id")) not in plans]

    def _one(q: dict[str, Any]) -> tuple[str, dict[str, Any]]:
        try:
            raw = play.chat(
                [{"role": "user", "content": PLAN_PROMPT.format(message=str(q.get("query") or "")[:1500])}],
                max_tokens=320,
                temperature=0.0,
                reasoning="low",
                model=model,
            )
        except Exception as exc:  # noqa: BLE001 — a failed plan = no planner intent (regex/none fallback)
            return str(q.get("question_id")), {"raw": "", "parsed": None, "model": model, "error": type(exc).__name__}
        return str(q.get("question_id")), {"raw": raw[:4000], "parsed": parse_plan(raw), "model": model}

    if workers > 1 and todo:
        from concurrent.futures import ThreadPoolExecutor

        with ThreadPoolExecutor(max_workers=workers) as pool:
            for qid, plan in pool.map(_one, todo):
                plans[qid] = plan
    else:
        for i, q in enumerate(todo):
            qid, plan = _one(q)
            plans[qid] = plan
            if (i + 1) % 10 == 0:
                path.write_text(json.dumps(plans, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
                print(json.dumps({"stage": "plan", "done": i + 1, "of": len(todo)}))
    path.write_text(json.dumps(plans, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    return plans


def main() -> int:
    ap = build_arg_parser()
    args = ap.parse_args()
    args.out = Path(args.out).resolve()
    args.gold = Path(args.gold).resolve()
    if args.plans is not None:
        args.plans = Path(args.plans).resolve()

    split_filter = _parse_csv_filter(args.splits)
    question_type_filter = _parse_csv_filter(args.question_types)
    try:
        fold_preview = (args.fold_count, args.fold_index)
        if (args.fold_count is None) != (args.fold_index is None):
            raise ValueError("fold_count and fold_index must be provided together")
        if args.fold_count is not None and (args.fold_count < 1 or args.fold_index < 0 or args.fold_index >= args.fold_count):
            raise ValueError("fold_index must satisfy 0 <= fold_index < fold_count")
    except ValueError as exc:
        print(json.dumps({"error": "bad_fold", "detail": str(exc)}, ensure_ascii=False))
        return 2

    try:
        rerank_budget_usd = _resolve_rerank_budget_usd(
            args.budget_usd, args.rerank_budget_usd
        )
    except ValueError as exc:
        print(json.dumps({"error": "bad_budget", "detail": str(exc)}, ensure_ascii=False))
        return 2

    if not args.gold.exists():
        print(json.dumps({"error": "gold_missing", "gold": str(args.gold)}))
        return 2
    gold = json.loads(args.gold.read_text(encoding="utf-8"))
    os.chdir(CODE)
    cfg = load_config()
    if args.config_override:
        from retrieval_config import _merge

        cfg = _merge(cfg, json.loads(args.config_override))
    index = load_index()
    index.cfg = cfg
    apply_vector_dims(index, int(cfg.get("vector_dims") or 0))
    if args.pool > 0:
        cfg["retrieve_pool"] = args.pool
    meta_path = DATA / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    binding = compute_index_binding(leaves=index.leaves, messages=index.messages, leaf_vecs_path=LEAF_VECS_PATH, cfg=cfg, meta=meta)
    seg = segmentation_version_from_cfg(cfg)
    for leaf in index.leaves:
        leaf["stable_leaf_id"] = stable_leaf_id_from_playground_leaf(leaf, segmentation_version=seg)
    try:
        assert_gold_bound_to_index(gold, binding)
        questions = list(gold.get("questions") or [])
        if args.retrieval_only:
            questions = [q for q in questions if not bool(q.get("expected_abstention"))]
        questions = _filter_questions(
            questions, splits=split_filter, question_types=question_type_filter
        )
        questions = _filter_questions_by_fold(questions, *fold_preview)
        if args.limit > 0:
            questions = questions[: args.limit]
        validate_question_wants_against_leaves(questions, index.leaves)
    except BindingError as exc:
        print(json.dumps({"error": "gold_not_bound", "detail": str(exc), "current_fingerprint": binding.get("index_fingerprint")}, ensure_ascii=False))
        return 2

    secrets = load_secrets(REPO / ".private" / "eval" / "secrets.env")
    os.environ.setdefault("EVAL_SECRETS", str(REPO / ".private" / "eval" / "secrets.env"))
    models = configured_models(cfg)
    play = CachingClient(
        PlayClient(secrets=secrets, budget_usd=args.budget_usd, llm_model=models["response"], embed_model=models["embed"]),
        Path(args.embed_cache).resolve() if args.embed_cache else None,
    )
    time_reference = None
    if str(args.time_now or "").strip():
        from retrieval_post import TimeReference as RetrievalTimeReference

        text = str(args.time_now).strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        now = datetime.fromisoformat(text)
        if now.tzinfo is None:
            now = now.replace(tzinfo=timezone.utc)
        time_reference = RetrievalTimeReference(mode="replay", now=now, source="eval:cli")

    plans: dict[str, Any] = {}
    if args.plans is not None:
        if args.plan:
            plans = _build_plans(questions, play, cfg, args.plans, workers=args.plan_workers,
                                 model=args.plan_model or None)
            if args.plan_only:
                ok = sum(1 for q in questions if (plans.get(str(q.get("question_id"))) or {}).get("parsed"))
                print(json.dumps({"stage": "plans_done", "n": len(questions), "parsed": ok,
                                  "cost_usd": round(play.usage.cost_usd, 5)}))
                return 0
        elif args.plans.exists():
            plans = json.loads(args.plans.read_text(encoding="utf-8"))

    room_by_obs: dict[str, str] = {}
    leaf_by_obs: dict[str, str] = {}
    for leaf in index.leaves:
        room = str(leaf.get("source_room_id") or "")
        for oid in leaf.get("observation_ids") or []:
            room_by_obs.setdefault(str(oid), room)
            leaf_by_obs.setdefault(str(oid), str(leaf["doc_id"]))

    variants = [v.strip() for v in args.variants.split(",") if v.strip()]
    for v in variants:
        if v not in VARIANT_TO_RANKING:
            print(json.dumps({"error": "bad_variant", "variant": v}))
            return 2
    rerank_ledger = _attach_rerank_budget_ledger(index, rerank_budget_usd)
    print(json.dumps({"stage": "start", "n_questions": len(questions),
                      "selected_n_questions": len(questions), "variants": variants,
                      "index_fingerprint": binding["index_fingerprint"][:16], "planner_cached": len(plans),
                      "pool": cfg.get("retrieve_pool"), "tag": args.tag,
                      "filters": {"splits": sorted(split_filter), "question_types": sorted(question_type_filter)},
                      "fold": {"count": args.fold_count, "index": args.fold_index},
                      "rerank_budget": _rerank_budget_summary(
                          rerank_ledger, play_budget_usd=args.budget_usd
                      )}, ensure_ascii=False))

    rerank_spent_before = 0.0
    t0 = time.perf_counter()
    instruction = str(cfg.get("embed_query_instruction") or DEFAULT_EMBED_QUERY_INSTRUCTION)
    # Prewarm original queries in one embeddings request. The row loop then
    # hits the cache, avoiding dozens of sequential network round trips for a
    # fresh gold set. Planner rewrites remain lazy because they vary per plan.
    if args.query_source == "original" and questions:
        play.embed(
            [str(row.get("query") or row.get("question") or "") for row in questions],
            kind="query",
            instruction=instruction,
        )
    def _run_question(qrow: dict[str, Any]) -> dict[str, Any]:
        qid = str(qrow.get("question_id"))
        question = str(qrow.get("query") or qrow.get("question") or "")
        want = {str(x) for x in (qrow.get("want_observation_ids") or qrow.get("expected_evidence_ids") or [])}
        want_leaves = {leaf_by_obs[o] for o in want if o in leaf_by_obs}
        room = ""
        if not args.no_room_scope:
            src_leaf = index.leaf_by_id.get(str(qrow.get("source_leaf_doc_id") or ""))
            room = str((src_leaf or {}).get("source_room_id") or "")
            if not room:
                room = next((room_by_obs[o] for o in want if o in room_by_obs), "")
        plan = plans.get(qid)
        intent = None
        search_query = question
        if plan and plan.get("parsed"):
            parsed = plan["parsed"]
            intent = intent_from_plan(parsed)
            if args.query_source == "planner":
                search_query = memory_query(parsed.get("tool_plan") or []) or question
        q_vec = np.asarray(play.embed([search_query], kind="query", instruction=instruction)[0], dtype=np.float32)
        q_vec /= np.linalg.norm(q_vec) + 1e-9
        row: dict[str, Any] = {
            "question_id": qid,
            "question_type": qrow.get("question_type"),
            "expected_abstention": bool(qrow.get("expected_abstention")),
            "search_query_rewritten": search_query != question,
            "intent_source": (intent or {}).get("source") if intent else "regex",
            "variants": {},
        }
        for variant in variants:
            trace: dict[str, Any] = {}
            t_v = time.perf_counter()
            try:
                hits = retrieve(
                    index, play, search_query, top_k=args.top_k, mode="leaf_parent", q_vec=q_vec,
                    ranking=VARIANT_TO_RANKING[variant], remaining_budget=max(0.05, args.budget_usd - play.usage.cost_usd),
                    trace=trace, source_room_id=room or None, intent=dict(intent) if intent else None,
                    time_reference=time_reference,
                )
                err = ""
            except Exception as exc:  # noqa: BLE001
                hits, err = [], type(exc).__name__
            wall_ms = (time.perf_counter() - t_v) * 1000.0
            rank = _hit_rank(hits, want, index)
            pool_ids = list(trace.get("candidate_pool_ids") or trace.get("hybrid_ids") or [])
            fusion_ids = list(trace.get("fusion_ids") or [])
            guaranteed = set(trace.get("pool_guaranteed_ids") or [])
            pool_hit = bool(want_leaves & set(pool_ids))
            fusion_pool_hit = bool(want_leaves & set(fusion_ids))
            guarantee_rescued = pool_hit and not fusion_pool_hit
            runs = trace.get("candidate_runs") or {}
            src_hit = {name: bool(want_leaves & set(ids)) for name, ids in runs.items()}
            union_hit = any(src_hit.values())
            only = [n for n, h in src_hit.items() if h]
            pool_only = [n for n, h in src_hit.items() if h] if guarantee_rescued else []
            if len(pool_only) != 1:
                pool_only = []
            auto_sig = trace.get("auto_confidence") if isinstance(trace.get("auto_confidence"), dict) else {}
            visible_metrics = _visible_evidence_metrics(hits, qrow)
            row["variants"][variant] = {
                "hit_rank": rank,
                "n_hits": len(hits),
                "pool_hit": pool_hit,
                "fusion_pool_hit": fusion_pool_hit,
                "guarantee_rescued": guarantee_rescued,
                "pool_n": len(pool_ids),
                "pool_guaranteed_n": len(guaranteed),
                "union_hit": union_hit,
                "union_n": trace.get("candidate_union_n"),
                "source_hit": src_hit,
                "unique_source": only[0] if len(only) == 1 else "",
                "pool_unique_source": pool_only[0] if pool_only else "",
                "applied": trace.get("applied"),
                "ranking": trace.get("ranking"),
                "auto_reason": trace.get("ranking_auto_reason"),
                "auto_level": auto_sig.get("level") if auto_sig else None,
                "auto_signals": auto_sig or None,
                "r0_correct1": rank == 1,
                "r0_correct5": 1 <= rank <= 5,
                "rerank_performed": bool(trace.get("rerank_performed", trace.get("applied") == "rerank")),
                "rerank_error": trace.get("rerank_error") or "",
                "recovery_performed": bool(trace.get("recovery_performed")),
                "rerank_cost_usd": float(trace.get("rerank_cost_usd") or 0.0),
                "rerank_latency_ms": trace.get("rerank_latency_ms"),
                "retrieve_ms": trace.get("retrieve_ms"),
                "wall_ms": round(wall_ms, 1),
                "abstained": bool(trace.get("abstained")),
                "rag_empty": not hits,
                "error": err,
                "retrievers_used": trace.get("retrievers_used"),
                "recent_window_applied": trace.get("recent_window_applied"),
                "persons": trace.get("persons"),
                **visible_metrics,
            }
        return row

    # 문항은 서로 독립이라 병렬로 돈다(API 대기가 대부분). 결과는 입력 순서로 모은다.
    # 재순위 예산이 바닥나는 경계에서는 어떤 문항이 R2를 받는지가 실행 순서에 따라 달라질 수 있다.
    done_lock = threading.Lock()
    done_n = [0]

    def _run_and_report(qrow: dict[str, Any]) -> dict[str, Any]:
        row = _run_question(qrow)
        with done_lock:
            done_n[0] += 1
            n = done_n[0]
        if n % 10 == 0 or n == 1 or n == len(questions):
            print(json.dumps({"stage": "progress", "done": n, "of": len(questions), "spent_usd": round(play.usage.cost_usd, 4)}), flush=True)
        return row

    per_q = map_parallel(questions, _run_and_report, workers=max(1, args.q_workers))

    # ---- aggregate ----
    def _agg(rows: list[dict[str, Any]]) -> dict[str, Any]:
        out: dict[str, Any] = {"n": len(rows), "variants": {}}
        scored = [r for r in rows if not r["expected_abstention"]]
        abst = [r for r in rows if r["expected_abstention"]]
        for variant in variants:
            vr = [r["variants"][variant] for r in scored]
            ranks = [x["hit_rank"] for x in vr]
            m = _metrics_from_ranks(ranks)
            m["pool_recall"] = (sum(1 for x in vr if x["pool_hit"]) / len(vr)) if vr else 0.0
            m["fusion_pool_recall"] = (sum(1 for x in vr if x.get("fusion_pool_hit")) / len(vr)) if vr else 0.0
            m["guarantee_rescue_rate"] = (sum(1 for x in vr if x.get("guarantee_rescued")) / len(vr)) if vr else 0.0
            m["union_recall"] = (sum(1 for x in vr if x["union_hit"]) / len(vr)) if vr else 0.0
            src_recall: dict[str, list[int]] = defaultdict(list)
            uniq: Counter = Counter()
            for x in vr:
                for name, hit in (x["source_hit"] or {}).items():
                    src_recall[name].append(1 if hit else 0)
                if x["unique_source"]:
                    uniq[x["unique_source"]] += 1
            m["source_recall"] = {k: (sum(v) / len(v) if v else 0.0) for k, v in sorted(src_recall.items())}
            m["unique_contribution"] = dict(uniq)
            pool_uniq: Counter = Counter()
            for x in vr:
                if x.get("pool_unique_source"):
                    pool_uniq[x["pool_unique_source"]] += 1
            m["unique_contribution_pool"] = dict(pool_uniq)
            m["rerank_invocation_rate"] = (sum(1 for x in vr if x["rerank_performed"]) / len(vr)) if vr else 0.0
            m["recovery_invocation_rate"] = (sum(1 for x in vr if x["recovery_performed"]) / len(vr)) if vr else 0.0
            m["abstain_rate_on_scored"] = (sum(1 for x in vr if x["abstained"]) / len(vr)) if vr else 0.0
            if abst:
                va = [r["variants"][variant] for r in abst]
                m["abstention_correct_rate"] = sum(1 for x in va if x["rag_empty"]) / len(va)
            lat = [float(x["wall_ms"]) for x in vr]
            m["latency_ms_p50"] = round(_pct(lat, 0.5), 1)
            m["latency_ms_p95"] = round(_pct(lat, 0.95), 1)
            cost = sum(float(x["rerank_cost_usd"]) for x in vr)
            m["rerank_cost_usd"] = round(cost, 5)
            m["cost_per_query_usd"] = round(cost / len(vr), 6) if vr else 0.0
            succ = sum(1 for x in vr if 1 <= x["hit_rank"] <= args.top_k)
            m["cost_per_successful_retrieval_usd"] = round(cost / succ, 6) if succ else None
            m["errors"] = sum(1 for x in vr if x["error"])
            for cutoff in VISIBLE_CUTOFFS:
                m[f"visible_recall@{cutoff}"] = (
                    sum(1 for x in vr if x.get(f"visible_recall@{cutoff}")) / len(vr)
                    if vr else 0.0
                )
                m[f"all_required_visible@{cutoff}"] = (
                    sum(1 for x in vr if x.get(f"all_required_visible@{cutoff}")) / len(vr)
                    if vr else 0.0
                )
            any_ranks = [int(x.get("visible_any_rank") or 0) for x in vr]
            all_ranks = [int(x.get("visible_all_rank") or 0) for x in vr]
            any_found = [rank for rank in any_ranks if rank > 0]
            all_found = [rank for rank in all_ranks if rank > 0]
            m["visible_any_rank_mean"] = round(sum(any_found) / len(any_found), 3) if any_found else None
            m["visible_any_rank_p50"] = round(_pct([float(rank) for rank in any_found], 0.5), 1) if any_found else None
            m["visible_any_rank_p95"] = round(_pct([float(rank) for rank in any_found], 0.95), 1) if any_found else None
            m["visible_any_rank_found_rate"] = len(any_found) / len(vr) if vr else 0.0
            m["visible_all_rank_mean"] = round(sum(all_found) / len(all_found), 3) if all_found else None
            m["visible_all_rank_p50"] = round(_pct([float(rank) for rank in all_found], 0.5), 1) if all_found else None
            m["visible_all_rank_p95"] = round(_pct([float(rank) for rank in all_found], 0.95), 1) if all_found else None
            m["visible_all_rank_found_rate"] = len(all_found) / len(vr) if vr else 0.0
            all_rows_for_safety = [r["variants"][variant] for r in rows]
            m["forbidden_visible_case_count"] = sum(
                1 for x in all_rows_for_safety if x.get("forbidden_visible_hit")
            )
            m["forbidden_visible_hit_rate"] = (
                m["forbidden_visible_case_count"] / len(all_rows_for_safety)
                if all_rows_for_safety else 0.0
            )
            levels = Counter(str(x.get("auto_level")) for x in vr if x.get("auto_level"))
            if levels:
                m["auto_levels"] = dict(levels)
            out["variants"][variant] = m
        # auto vs R0 / R2 route analysis
        if "auto" in variants and "R0" in variants:
            va = [(r["variants"]["auto"], r["variants"]["R0"], r["variants"].get("R2")) for r in scored]
            reranked = [t for t in va if t[0]["rerank_performed"]]
            unnecessary = [t for t in reranked if t[1]["hit_rank"] == 1]
            r0_miss = [t for t in va if not (1 <= t[1]["hit_rank"] <= args.top_k)]
            recovered = [t for t in r0_miss if 1 <= t[0]["hit_rank"] <= args.top_k]
            recovered_rerank = [t for t in recovered if t[0]["rerank_performed"] and not t[0]["recovery_performed"]]
            recovered_recovery = [t for t in recovered if t[0]["recovery_performed"]]
            skipped = [t for t in va if not t[0]["rerank_performed"]]
            skipped_ok = [t for t in skipped if t[0]["hit_rank"] == 1]
            r0_hit1 = [t for t in va if t[1]["hit_rank"] == 1]
            route: dict[str, Any] = {
                "rerank_invocation_rate": len(reranked) / len(va) if va else 0.0,
                "unnecessary_rerank_rate": (len(unnecessary) / len(reranked)) if reranked else 0.0,
                "unnecessary_rerank_given_r0_hit1": (sum(1 for t in r0_hit1 if t[0]["rerank_performed"]) / len(r0_hit1)) if r0_hit1 else None,
                "r0_miss_n": len(r0_miss),
                "auto_recovered_from_r0_miss": len(recovered),
                "r0_miss_recovery_rate": (len(recovered) / len(r0_miss)) if r0_miss else None,
                "recovered_by_r2_only": len(recovered_rerank),
                "recovered_by_recovery_and_r2": len(recovered_recovery),
                "skipped_rerank_n": len(skipped),
                "skipped_rerank_hit1_rate": (len(skipped_ok) / len(skipped)) if skipped else None,
            }
            if all(t[2] for t in va):
                # route accuracy: auto's hit rank equals the better of (R0, R2) at @1
                best = sum(1 for t in va if t[0]["hit_rank"] == 1 or (t[1]["hit_rank"] != 1 and t[2]["hit_rank"] != 1))
                route["auto_route_accuracy@1"] = best / len(va) if va else 0.0
                route["auto_vs_r2_recall@1_gap"] = round(
                    out["variants"]["R2"]["recall@1"] - out["variants"]["auto"]["recall@1"], 4)
            out["auto_route"] = route
        return out

    by_type: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for r in per_q:
        by_type[str(r.get("question_type") or "unknown")].append(r)
    summary = {
        "tag": args.tag,
        "dataset": str(args.gold),
        "gold_kind": gold.get("gold_kind"),
        "not_human_reviewed": gold.get("gold_kind") != "human_reviewed",
        "index_fingerprint": binding["index_fingerprint"],
        "n_questions": len(per_q),
        "selected_n_questions": len(per_q),
        "variants": variants,
        "top_k": args.top_k,
        "pool": cfg.get("retrieve_pool"),
        "room_scoped": not args.no_room_scope,
        "planner_intent": bool(plans),
        "query_source": args.query_source,
        "config_override": json.loads(args.config_override) if args.config_override else None,
        "models": {k: models[k] for k in ("embed", "rerank", "planner")},
        "overall": _agg(per_q),
        "by_type": {t: _agg(rows) for t, rows in sorted(by_type.items())},
        "embed_cost_usd": round(play.usage.cost_usd, 5),
        "elapsed_sec": round(time.perf_counter() - t0, 2),
        "rerank_budget": _rerank_budget_summary(
            rerank_ledger, play_budget_usd=args.budget_usd
        ),
        "promote": "needs more evidence",
        "binding": "enforced",
        "filters": {"splits": sorted(split_filter), "question_types": sorted(question_type_filter)},
        "fold": {"count": args.fold_count, "index": args.fold_index},
    }
    play.save()
    summary["embed_cache"] = {"hits": play.hits, "misses": play.misses}
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (args.out / "per_question_metrics.json").write_text(json.dumps(per_q, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    brief = {v: {k: summary["overall"]["variants"][v][k] for k in ("recall@1", "recall@5", "recall@10", "mrr", "pool_recall", "fusion_pool_recall", "guarantee_rescue_rate") if k in summary["overall"]["variants"][v]}
             for v in variants}
    print(json.dumps({"stage": "done", "overall": brief, "auto_route": summary["overall"].get("auto_route"),
                      "elapsed_sec": summary["elapsed_sec"], "embed_cost_usd": summary["embed_cost_usd"],
                      "rerank_budget": summary["rerank_budget"]}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
