#!/usr/bin/env python3
"""Aggregate run_retrieval_v2_eval outputs into a dev-vs-holdout generalization table.

Reads `<runs>/<domain>-<config>-<variant>/per_question_metrics.json` and prints (JSON):
- per domain × config × variant: n, R@1/5/10, MRR, pool recall (answerable questions only),
  abstention correctness (expected-abstention questions, R2 only meaningful)
- paired differences vs a reference config with a percentile bootstrap 95% CI (same questions)
- macro average over holdout domains and the worst holdout domain

Only ids and numbers are read; no question text is printed.
"""

from __future__ import annotations

import argparse
import json
import random
from pathlib import Path
from typing import Any


def _rows(path: Path) -> list[dict[str, Any]]:
    return json.loads(path.read_text(encoding="utf-8"))


def _hit(rank: Any, k: int) -> float:
    return 1.0 if isinstance(rank, int) and 1 <= rank <= k else 0.0


def _rr(rank: Any) -> float:
    return 1.0 / rank if isinstance(rank, int) and rank >= 1 else 0.0


def per_question(rows: list[dict[str, Any]], variant: str) -> dict[str, dict[str, float]]:
    out: dict[str, dict[str, float]] = {}
    for r in rows:
        v = (r.get("variants") or {}).get(variant)
        if not v:
            continue
        if r.get("expected_abstention"):
            out[r["question_id"]] = {"abstain_case": 1.0,
                                     "fallback": 1.0 if (variant in ("R2", "auto") and str(v.get("applied") or "") == "hybrid_fallback") else 0.0,
                                     "abstained": 1.0 if (v.get("abstained") or v.get("rag_empty") or not v.get("n_hits")) else 0.0,
                                     "type": r.get("question_type")}
            continue
        rank = v.get("hit_rank")
        fell_back = variant in ("R2", "auto") and str(v.get("applied") or "") == "hybrid_fallback"
        out[r["question_id"]] = {"fallback": 1.0 if fell_back else 0.0, "r1": _hit(rank, 1), "r5": _hit(rank, 5), "r10": _hit(rank, 10), "mrr": _rr(rank),
                                 "pool": 1.0 if v.get("pool_hit") else 0.0, "type": r.get("question_type")}
    return out


def summarize(pq: dict[str, dict[str, float]]) -> dict[str, Any]:
    ans = [x for x in pq.values() if "r1" in x]
    abst = [x for x in pq.values() if "abstain_case" in x]
    n = len(ans) or 1
    out: dict[str, Any] = {"n": len(ans)}
    for k in ("r1", "r5", "r10", "mrr", "pool"):
        out[k] = round(sum(x[k] for x in ans) / n, 4)
    if abst:
        out["abstain_n"] = len(abst)
        out["abstain_correct"] = round(sum(x["abstained"] for x in abst) / len(abst), 4)
    types: dict[str, list[dict[str, float]]] = {}
    for x in ans:
        types.setdefault(str(x["type"]), []).append(x)
    out["by_type_r1"] = {t: round(sum(y["r1"] for y in v) / len(v), 3) for t, v in sorted(types.items())}
    return out


def paired_diff(a: dict[str, dict[str, float]], b: dict[str, dict[str, float]], metric: str,
                *, iters: int = 4000, seed: int = 7) -> dict[str, Any]:
    ids = sorted(q for q in a if q in b and metric in a[q] and metric in b[q])
    if not ids:
        return {"n": 0}
    d = [b[q][metric] - a[q][metric] for q in ids]
    rng = random.Random(seed)
    means = []
    for _ in range(iters):
        s = [d[rng.randrange(len(d))] for _ in d]
        means.append(sum(s) / len(s))
    means.sort()
    return {"n": len(ids), "diff": round(sum(d) / len(d), 4),
            "ci95": [round(means[int(0.025 * iters)], 4), round(means[int(0.975 * iters) - 1], 4)],
            "wins": sum(1 for x in d if x > 0), "losses": sum(1 for x in d if x < 0)}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--runs", type=Path, required=True)
    ap.add_argument("--domains", default="dev,discord,kakao-a,kakao-b")
    ap.add_argument("--holdouts", default="discord,kakao-a,kakao-b")
    ap.add_argument("--configs", default="B0,B1,B2,B3,B4")
    ap.add_argument("--variant", default="R0")
    ap.add_argument("--run-variant-tag", default="", help="suffix in run dir names (default = variant)")
    ap.add_argument("--reference", default="B0")
    ap.add_argument("--gold-dir", type=Path, default=None, help="holdout gold dir (<domain>.json) for --exclude-flag")
    ap.add_argument("--exclude-flag", default="", help="drop holdout questions whose gold row has this truthy flag")
    ap.add_argument("--drop-fallback", action="store_true",
                    help="drop questions whose reranker fell back to R0 in ANY config of that domain (paired)")
    ap.add_argument("--out", type=Path, default=None)
    args = ap.parse_args()

    domains = [d for d in args.domains.split(",") if d]
    holdouts = [d for d in args.holdouts.split(",") if d]
    configs = [c for c in args.configs.split(",") if c]
    tag = args.run_variant_tag or args.variant
    table: dict[str, dict[str, Any]] = {}
    pq_all: dict[tuple[str, str], dict[str, dict[str, float]]] = {}
    for d in domains:
        table[d] = {}
        for c in configs:
            path = args.runs / f"{d}-{c}-{tag}" / "per_question_metrics.json"
            if not path.exists():
                continue
            pq = per_question(_rows(path), args.variant)
            if args.exclude_flag and args.gold_dir and (args.gold_dir / f"{d}.json").exists():
                gold = json.loads((args.gold_dir / f"{d}.json").read_text(encoding="utf-8"))
                drop = {q["question_id"] for q in gold.get("questions") or [] if q.get(args.exclude_flag)}
                pq = {k: v for k, v in pq.items() if k not in drop}
            pq_all[(d, c)] = pq
            table[d][c] = summarize(pq)
    dropped: dict[str, int] = {}
    if args.drop_fallback:
        for d in domains:
            bad = {q for c in configs for q, x in (pq_all.get((d, c)) or {}).items() if x.get("fallback")}
            dropped[d] = len(bad)
            for c in configs:
                if (d, c) in pq_all:
                    pq_all[(d, c)] = {q: x for q, x in pq_all[(d, c)].items() if q not in bad}
                    table[d][c] = summarize(pq_all[(d, c)])
    diffs: dict[str, dict[str, Any]] = {}
    for d in domains:
        ref = pq_all.get((d, args.reference))
        if ref is None:
            continue
        diffs[d] = {}
        for c in configs:
            if c == args.reference or (d, c) not in pq_all:
                continue
            diffs[d][c] = {m: paired_diff(ref, pq_all[(d, c)], m) for m in ("r1", "r5", "mrr")}
    macro: dict[str, Any] = {}
    for c in configs:
        vals = [table[d][c] for d in holdouts if c in table.get(d, {})]
        if len(vals) == len(holdouts) and vals:
            macro[c] = {m: round(sum(v[m] for v in vals) / len(vals), 4) for m in ("r1", "r5", "r10", "mrr", "pool")}
            macro[c]["worst_r1"] = min(v["r1"] for v in vals)
            macro[c]["worst_r5"] = min(v["r5"] for v in vals)
            if "dev" in table and c in table["dev"]:
                macro[c]["dev_minus_holdout_r1"] = round(table["dev"][c]["r1"] - macro[c]["r1"], 4)
                macro[c]["dev_minus_holdout_r5"] = round(table["dev"][c]["r5"] - macro[c]["r5"], 4)
    result = {"variant": args.variant, "reference": args.reference, "table": table, "paired_vs_reference": diffs,
              "holdout_macro": macro,
              "dropped_fallback": dropped}
    text = json.dumps(result, ensure_ascii=False, indent=1)
    if args.out:
        args.out.write_text(text, encoding="utf-8")
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
