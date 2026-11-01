#!/usr/bin/env python3
"""Regenerate evaluation gold bound to the *current* index fingerprint.

This is the supported path when observation ids or segmentation change.
Temporary remaps / unique-token probes are not used.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

import numpy as np

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
sys.path.insert(0, str(CODE))
sys.path.insert(0, str(EVAL))

from client import map_parallel  # noqa: E402
from engine import DATA, LEAF_VECS_PATH, load_config, load_index, make_client  # noqa: E402
from rag_upgrade.index_binding import (  # noqa: E402
    BindingError,
    compute_index_binding,
    segmentation_version_from_cfg,
    stable_leaf_id_from_playground_leaf,
    validate_question_wants_against_leaves,
)

GEN_PROMPT = """다음은 한국어 단체 채팅 일부다.

<대화>
{body}
</대화>

이 대화에서만 답할 수 있는 질문 1개를 JSON으로만 출력하라.
{{"question": "...", "must_include_substring": "대화에 실제로 나온 짧은 핵심 어구"}}

- 질문은 한 문장 한국어.
- must_include_substring은 대화 본문에 그대로 있는 2~12자 조각.
- 인사/ㅋㅋ만으로 묻지 마라.
"""


def _parse_json(raw: str) -> dict[str, Any]:
    text = (raw or "").strip()
    if "```" in text:
        text = text.split("```")[1].lstrip("json").strip()
    start = text.find("{")
    end = text.rfind("}")
    if start < 0 or end < 0:
        raise ValueError("json object missing")
    return json.loads(text[start : end + 1])


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--questions", type=int, default=20)
    ap.add_argument("--budget-usd", type=float, default=1.5)
    ap.add_argument(
        "--out",
        type=Path,
        default=REPO
        / ".masamong-scratch"
        / "rag-upgrade-ablation-20260916"
        / "gold.bound.json",
    )
    ap.add_argument("--seed", type=int, default=20260916)
    args = ap.parse_args()
    # os.chdir 전에 절대화 — 상대 --out/--gold가 샌드박스 폴더로 떨어지지 않게.
    args.out = Path(args.out).resolve()

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

    # Annotate leaves with stable ids for this run (not written back to playground).
    for leaf in index.leaves:
        leaf["stable_leaf_id"] = stable_leaf_id_from_playground_leaf(
            leaf, segmentation_version=seg
        )

    n_q = max(1, args.questions)
    client = make_client(cfg, budget_usd=float(args.budget_usd))
    rng = np.random.default_rng(args.seed)
    cands = [
        leaf
        for leaf in index.leaves
        if sum(
            len(index.messages[i]["content"])
            for i in range(int(leaf["start"]), int(leaf["end"]))
        )
        >= 40
    ]
    if len(cands) < n_q:
        cands = list(index.leaves)
    pool_n = min(len(cands), max(n_q * 2, n_q + 12))
    chosen = [cands[int(i)] for i in rng.permutation(len(cands))[:pool_n]]
    workers = int(cfg.get("api_workers", 8))

    def _gen_one(leaf: dict[str, Any]) -> dict[str, Any] | None:
        try:
            raw = client.chat(
                [{"role": "user", "content": GEN_PROMPT.format(body=leaf["body"][:2500])}],
                max_tokens=250,
                temperature=0.2,
                reasoning="low",
            )
            obj = _parse_json(raw)
            question = (obj.get("question") or "").strip()
            needle = (obj.get("must_include_substring") or "").strip()
            if not question or len(question) < 4:
                return None
            want = {str(x) for x in (leaf.get("observation_ids") or [])}
            if needle:
                matched = {
                    str(index.messages[i]["observation_id"])
                    for i in range(int(leaf["start"]), int(leaf["end"]))
                    if needle in str(index.messages[i].get("content") or "")
                }
                if matched:
                    want = matched
            if not want:
                return None
            return {
                "question_id": f"q-{leaf['stable_leaf_id'][:12]}",
                "query": question,
                "split": "dev",
                "expected_memory_requirement": "required",
                "expected_abstention": False,
                "want_observation_ids": sorted(want),
                "expected_evidence_ids": sorted(want),
                "forbidden_evidence_ids": [],
                "source_leaf_doc_id": str(leaf["doc_id"]),
                "stable_leaf_id": str(leaf["stable_leaf_id"]),
                "needle": needle,
                "types": ["memory_only"],
                "review_status": "unreviewed",
            }
        except Exception:  # noqa: BLE001
            return None

    print(
        json.dumps(
            {
                "stage": "regen_start",
                "candidates": len(chosen),
                "target": n_q,
                "index_fingerprint": binding["index_fingerprint"][:16],
            }
        )
    )
    generated = map_parallel(chosen, _gen_one, workers=workers)
    questions = [g for g in generated if g is not None][:n_q]
    if len(questions) < max(4, n_q // 2):
        print(json.dumps({"error": "gold_insufficient", "n": len(questions)}))
        return 2

    try:
        validate_question_wants_against_leaves(questions, index.leaves)
    except BindingError as exc:
        print(json.dumps({"error": "binding_validation_failed", "detail": str(exc)}))
        return 2

    payload = {
        "schema_version": 2,
        "dataset_id": f"bound-gold-{binding['index_fingerprint'][:12]}",
        "gold_kind": "synthetic",
        "review_status": "unreviewed",
        "index_binding": binding,
        "corpus_binding": binding.get("corpus_binding"),
        "questions": questions,
        "generator": {
            "name": "regen_bound_gold",
            "seed": args.seed,
            "not_qualification": True,
        },
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(
        json.dumps(
            {
                "stage": "regen_done",
                "out": str(args.out),
                "n_questions": len(questions),
                "index_fingerprint": binding["index_fingerprint"],
                "embed_or_chat_cost_usd": client.usage.cost_usd,
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
