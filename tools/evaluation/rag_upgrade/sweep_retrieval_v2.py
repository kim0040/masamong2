#!/usr/bin/env python3
"""Run a named set of run_retrieval_v2_eval.py configurations sequentially (2026-09-17 sweep).

Each entry: tag, variants, query source, config override. Outputs land in
.masamong-scratch/retrieval-v2/<tag>/summary.json. Reranker cost only for R2/auto entries.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
OUT = REPO / ".masamong-scratch" / "retrieval-v2"
HARNESS = REPO / "tools" / "evaluation" / "rag_upgrade" / "run_retrieval_v2_eval.py"

NOWIN = {"recent_window_days": 100000, "recent_window_leaves": 100000}
V1_OFF = {"retrieval": {"person": {"enabled": False}, "alias": {"enabled": False}, "multi_query": {"enabled": False}}}
CHOSEN = {"retrieval": {
    "person": {"rrf_weight": 0.3},
    "alias": {"rrf_weight": 0.15},
    "candidate_pool": {"per_source_min": 5, "reserve_max_frac": 0.2},
}}

SWEEPS: dict[str, list[dict]] = {
    # candidate generation only (no reranker cost)
    "r0": [
        {"tag": "r0v1-nowin-original", "variants": "R0", "query": "original", "override": {**NOWIN, **V1_OFF}},
        {"tag": "r0v2-nowin-original", "variants": "R0", "query": "original", "override": NOWIN},
        {"tag": "r0v2-nowin-planner", "variants": "R0", "query": "planner", "override": NOWIN},
        {"tag": "r0v2-win-original", "variants": "R0", "query": "original", "override": {}},
        {"tag": "r0v2-person-only-nowin", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"multi_query": {"enabled": False}}}},
        {"tag": "r0v2-mq-only-nowin", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"enabled": False}, "alias": {"enabled": False}}}},
    ],
    # full cascade incl. abstention questions (reranker cost)
    "autolive": [
        {"tag": "full-g-on-nowin", "variants": "R0,R2,auto", "query": "original",
         "override": {**NOWIN, **CHOSEN}, "all": True, "budget": 2.0},
    ],
    "r2live": [
        {"tag": f"r2on-{fmt}", "variants": "R2", "query": "original", "budget": 2.0,
         "override": {**NOWIN, "retrieval": {**CHOSEN["retrieval"], "rerank": {"doc_format": fmt}}}}
        for fmt in ("body", "summary_body", "speaker_body", "time_body", "full")
    ] + [
        {"tag": "r2on-prededup", "variants": "R2", "query": "original", "budget": 2.0,
         "override": {**NOWIN, "retrieval": {**CHOSEN["retrieval"], "rerank": {"pre_dedup": True}}}},
    ],
    # failed live (Voyage omitted usage.cost → hybrid_fallback). Kept as history.
    "auto": [
        {"tag": "full-g-nowin-original", "variants": "R0,R2,auto", "query": "original",
         "override": {**NOWIN, **CHOSEN}, "all": True},
        {"tag": "full-g-win-original", "variants": "R0,R2,auto", "query": "original",
         "override": dict(CHOSEN), "all": True},
    ],
    # guarantee_pool: keep fusion ranking, reserve slots for low-weight retrievers
    "guarantee": [
        {"tag": "r0v2-g-p0.3-a0.15-m3-r20", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 0.3}, "alias": {"rrf_weight": 0.15},
                                            "candidate_pool": {"per_source_min": 3, "reserve_max_frac": 0.2}}}},
        {"tag": "r0v2-g-p0.3-a0.15-m5-r20", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 0.3}, "alias": {"rrf_weight": 0.15},
                                            "candidate_pool": {"per_source_min": 5, "reserve_max_frac": 0.2}}}},
        {"tag": "r0v2-g-p0.3-a0.15-m5-r30", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 0.3}, "alias": {"rrf_weight": 0.15},
                                            "candidate_pool": {"per_source_min": 5, "reserve_max_frac": 0.3}}}},
        {"tag": "r0v2-g-p0.3-a0.0-m5-r20", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 0.3}, "alias": {"enabled": False},
                                            "candidate_pool": {"per_source_min": 5, "reserve_max_frac": 0.2}}}},
        {"tag": "r0v2-g-off-p0.3-a0.15", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 0.3}, "alias": {"rrf_weight": 0.15},
                                            "candidate_pool": {"per_source_min": 0}}}},
        {"tag": "r0v2-g-p1.0-a0.5-m5-r30", "variants": "R0", "query": "original",
         "override": {**NOWIN, "retrieval": {"person": {"rrf_weight": 1.0}, "alias": {"rrf_weight": 0.5},
                                            "candidate_pool": {"per_source_min": 5, "reserve_max_frac": 0.3}}}},
    ],
    # R2 input representation (§24) and pre-dedup (§26)
    "r2doc": [
        {"tag": f"r2doc-{fmt}", "variants": "R2", "query": "original",
         "override": {**NOWIN, "retrieval": {**CHOSEN["retrieval"], "rerank": {"doc_format": fmt}}}}
        for fmt in ("body", "summary_body", "speaker_body", "time_body", "full")
    ] + [
        {"tag": "r2-prededup", "variants": "R2", "query": "original",
         "override": {**NOWIN, "retrieval": {**CHOSEN["retrieval"], "rerank": {"pre_dedup": True}}}},
    ],
}


def main() -> int:
    import os
    from concurrent.futures import ThreadPoolExecutor, as_completed

    args = [a for a in sys.argv[1:] if a != "--jobs"]
    jobs = 1
    if "--jobs" in sys.argv:
        i = sys.argv.index("--jobs")
        jobs = int(sys.argv[i + 1]) if i + 1 < len(sys.argv) else 4
        args = [a for a in sys.argv[1:] if a not in {"--jobs", str(jobs)}]
    names = args or ["r0"]
    runs = [run for name in names for run in SWEEPS[name]]

    def _one(run: dict) -> tuple[str, int, str]:
        cache = OUT / run["tag"] / "embed-cache.json"
        cache.parent.mkdir(parents=True, exist_ok=True)
        src = OUT / "embed-cache.json"
        if src.exists() and not cache.exists():
            cache.write_bytes(src.read_bytes())
        cmd = [sys.executable, str(HARNESS), "--variants", run["variants"], "--query-source", run["query"],
               "--plans", str(OUT / "plans.v3.json"), "--embed-cache", str(cache if cache.exists() else src),
               "--budget-usd", str(run.get("budget", 1.5)), "--tag", run["tag"], "--out", str(OUT / run["tag"]),
               "--config-override", json.dumps(run["override"], ensure_ascii=False)]
        if not run.get("all"):
            cmd.append("--retrieval-only")
        proc = subprocess.run(cmd, cwd=str(REPO), capture_output=True, text=True, env={"PYTHONDONTWRITEBYTECODE": "1", **os.environ})
        last = [ln for ln in proc.stdout.splitlines() if ln.startswith('{"stage": "done"')]
        return run["tag"], proc.returncode, (last[-1] if last else (proc.stdout[-800:] or proc.stderr[-800:]))

    if jobs <= 1:
        for run in runs:
            print("==>", run["tag"], flush=True)
            tag, code, out = _one(run)
            print(out, flush=True)
            if code != 0:
                return code
        return 0
    print(f"parallel jobs={jobs} n={len(runs)}", flush=True)
    failed = 0
    with ThreadPoolExecutor(max_workers=jobs) as pool:
        futs = {pool.submit(_one, run): run["tag"] for run in runs}
        for fut in as_completed(futs):
            tag, code, out = fut.result()
            print("==>", tag, "exit", code, flush=True)
            print(out, flush=True)
            if code != 0:
                failed = code
    return failed


if __name__ == "__main__":
    raise SystemExit(main())
