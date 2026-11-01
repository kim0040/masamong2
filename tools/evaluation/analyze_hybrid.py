"""하이브리드 융합 방식 비교 (API 호출 0회, 캐시 재사용).

1차 파일럿에서 단순 RRF 하이브리드가 dense 단독보다 Recall@1이 낮았다.
약한 retriever(B0)에 동일 가중치를 주는 것이 원인인지 확인한다.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).parent))

from lexical import LexicalIndex  # noqa: E402
from run_retrieval_pilot import build_windows
from windows import WindowConfig
from artifacts import load_manifest, load_json_cache, load_array_cache, output_dir, resolve_manifest_artifact, documents_fingerprint, ArtifactValidationError  # noqa: E402

OUT = output_dir()
TAG = os.environ.get("TAG", "w20o0")
WINDOW_SIZE = int(TAG.split("o")[0][1:])


def weighted_rrf(rank_lists: list[list[str]], weights: list[float], k: int = 60) -> list[str]:
    scores: dict[str, float] = {}
    for ranks, w in zip(rank_lists, weights):
        for pos, doc_id in enumerate(ranks):
            scores[doc_id] = scores.get(doc_id, 0.0) + w / (k + pos + 1)
    return [d for d, _ in sorted(scores.items(), key=lambda x: -x[1])]


def metrics(ranks: list[int], n: int) -> tuple[float, float, float]:
    at1 = sum(1 for x in ranks if x == 1) / n
    at10 = sum(1 for x in ranks if x >= 1) / n
    mrr = sum(1 / x for x in ranks if x >= 1) / n
    return at1, at10, mrr


def main() -> int:
    path = Path(os.environ.get("EVAL_MANIFEST", str(OUT / f"run_{TAG}.json")))
    manifest = load_manifest(path)
    if manifest.get("status") != "complete":
        raise ArtifactValidationError("run is not complete")
    source = manifest["source"]
    docs = build_windows(source["collection"], set(source["rooms"]), source["limit"], WindowConfig.from_mapping(manifest["config"]))
    if documents_fingerprint(docs) != manifest["recipe"]["documents"]:
        raise ArtifactValidationError("current source documents differ from run")
    common = dict(provider=manifest["provider"], manifest_ref=path.name)
    gold = load_json_cache(resolve_manifest_artifact(path,manifest["artifacts"]["gold"]), input_fingerprint=manifest["gold_fingerprint"], model=manifest["llm_model"], recipe=manifest["gold_recipe"], **common)
    doc_vecs = load_array_cache(resolve_manifest_artifact(path,manifest["artifacts"]["documents"]), input_fingerprint=manifest["doc_fingerprint"], model=manifest["embed_model"], recipe=manifest["recipe"], **common)
    q_vecs = load_array_cache(resolve_manifest_artifact(path,manifest["artifacts"]["queries"]), input_fingerprint=manifest["query_fingerprint"], model=manifest["embed_model"], recipe=manifest["recipe"], **common)
    if doc_vecs.ndim != 2 or q_vecs.shape != (2*len(gold),doc_vecs.shape[1]) or len(doc_vecs)!=len(docs) or not gold:
        raise ArtifactValidationError("evaluation artifact shape mismatch")
    ids = [d["doc_id"] for d in docs]
    fts = LexicalIndex("bigram")
    fts.add_many([(d["doc_id"], d["body"]) for d in docs])

    n = len(gold)
    print(f"[{TAG}] gold {n}개, window {len(docs):,}개\n")

    configs = [
        ("B0 keyword 단독", None),
        ("B1 dense 단독", None),
        ("RRF 1:1 (기본)", (1.0, 1.0)),
        ("RRF 1:2 (dense 2배)", (1.0, 2.0)),
        ("RRF 1:4 (dense 4배)", (1.0, 4.0)),
        ("RRF 1:8 (dense 8배)", (1.0, 8.0)),
    ]

    for style, offset in (("literal", 0), ("paraphrase", n)):
        print(f"=== {style} ===")
        print(f"{'융합 방식':<24}{'Hit@1':>10}{'Hit@10':>11}{'MRR@10':>8}")
        print("-" * 53)
        cached = []
        for i, g in enumerate(gold):
            b0 = [d for d, _ in fts.search(g[style], limit=50)]
            sims = doc_vecs @ q_vecs[offset + i]
            b1 = [ids[j] for j in np.argsort(-sims)[:50]]
            cached.append((g["doc_id"], b0, b1))

        for label, weights in configs:
            ranks = []
            for want, b0, b1 in cached:
                if label.startswith("B0"):
                    fused = b0
                elif label.startswith("B1"):
                    fused = b1
                else:
                    fused = weighted_rrf([b0, b1], list(weights))
                ranks.append(fused.index(want) + 1 if want in fused[:10] else 0)
            a1, a10, mrr = metrics(ranks, n)
            print(f"{label:<24}{a1*100:>9.1f}%{a10*100:>10.1f}%{mrr:>8.3f}")
        print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
