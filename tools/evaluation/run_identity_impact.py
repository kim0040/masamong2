"""신원(별칭) 해소가 인물 질의 검색에 주는 영향 측정.

실사용 로그상 장기기억 질문 최다 범주는 인물 질의(23.7%)다. 그런데 Discord 계정의
32%가 표시명을 바꿨고 한 계정은 표시명이 9개다. 표시명 하나로 질의하면 같은 사람의
발화 대부분에 도달하지 못한다.

정답(ground truth)을 표시명이 아니라 **계정 ID** 기준으로 잡고,
  A. 표시명 1개로 질의 (현재 상태)
  B. 계정의 모든 표시명으로 질의 확장 (단순 별칭 확장)
를 비교한다. recipe는 1차 파일럿에서 고정한 값을 그대로 쓴다.
"""
from __future__ import annotations

import os
import sys
from collections import Counter, defaultdict
from math import sqrt
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).parent))

from corpus import iter_observations  # noqa: E402
from lexical import LexicalIndex  # noqa: E402
from openrouter import OpenRouter
from artifacts import output_dir, documents_fingerprint, cache_input_fingerprint
from windows import WindowConfig, build_windows as bounded_windows
from run_retrieval_pilot import embed_all  # noqa: E402

OUT = output_dir()
ROOM = os.environ.get("ROOM", "")
WINDOW_SIZE = 20
N_WINDOWS = 3000
TOP_K = 10
RRF_DENSE_WEIGHT = 4.0
MIN_ACCOUNT_MSGS = 150


def wilson(k: int, n: int) -> tuple[float, float]:
    if n == 0:
        return 0.0, 0.0
    p, z = k / n, 1.96
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return max(0.0, c - h), min(1.0, c + h)


def rrf(lists, weights, k=60):
    scores = {}
    for ranks, w in zip(lists, weights):
        for pos, d in enumerate(ranks):
            scores[d] = scores.get(d, 0.0) + w / (k + pos + 1)
    return [d for d, _ in sorted(scores.items(), key=lambda x: -x[1])]


def main() -> int:
    if not ROOM.strip():
        raise ValueError("ROOM must explicitly select one source channel")
    docs = bounded_windows((o for o in iter_observations("discord",kind="dated",rooms={ROOM}) if o.semantic_eligible),WindowConfig(size=WINDOW_SIZE),limit=N_WINDOWS)
    for doc in docs:
        doc["accounts"] = sorted({a for a in doc["speaker_accounts"] if a})
    ids = [d["doc_id"] for d in docs]
    print(f"window {len(docs):,}개")

    names_of = defaultdict(Counter)
    for doc in docs:
        for account, label in zip(doc["speaker_accounts"],doc["speakers"]):
            if account and label != "?":
                names_of[account][label] += 1
    accounts = [a for a, c in names_of.items() if sum(c.values()) >= MIN_ACCOUNT_MSGS]
    multi = [a for a in accounts if len(names_of[a]) > 1]
    print(f"대상 계정 {len(accounts)}개 (표시명 2개 이상인 계정 {len(multi)}개)\n")

    if not accounts:
        raise ValueError("no eligible accounts in selected window range")
    client = OpenRouter(budget_usd=1.0)
    recipe = {"documents": documents_fingerprint(docs), "room": ROOM, "window": WindowConfig(size=WINDOW_SIZE).as_dict(), "normalization":"l2-float32-v1","char_limit":4000}
    def vectors(texts,name):
        fp = cache_input_fingerprint(texts, provider={"name":"openrouter","base_url":client.base},model=client.embed_model,recipe=recipe,kind=name)
        return embed_all(client,texts,OUT/f"identity_{name}.npy",input_fingerprint=fp,recipe=recipe,manifest_ref="identity_run.json")
    doc_vecs = vectors([d["body"][:4000] for d in docs], "documents")
    fts = LexicalIndex("bigram")
    fts.add_many([(d["doc_id"], d["body"]) for d in docs])
    truth = {d["doc_id"]: d["accounts"] for d in docs}

    # A: 대표 표시명 1개 / B: 모든 표시명
    variants = {}
    for acct in accounts:
        names = names_of[acct]
        top = names.most_common(1)[0][0]
        variants[acct] = {
            "A_single": [f"{top}이 누구야"],
            "B_aliases": [f"{n}이 누구야" for n, _ in names.most_common()],
        }

    all_q = sorted({q for v in variants.values() for qs in v.values() for q in qs})
    qv = vectors(all_q,"queries")
    qidx = {q: i for i, q in enumerate(all_q)}

    def retrieve(q):
        b0 = [d for d, _ in fts.search(q, limit=50)]
        b1 = [ids[j] for j in np.argsort(-(doc_vecs @ qv[qidx[q]]))[:50]]
        return rrf([b0, b1], [1.0, RRF_DENSE_WEIGHT])

    print(f"{'조건':<24}{'account presence@10':>14}{'pooled CI':>18}{'회수 window':>12}")
    print("-" * 70)
    summary = {}
    for label in ("A_single", "B_aliases"):
        hits = tot = 0
        reached = []
        for acct in accounts:
            # 순차 연결하면 첫 질의가 top-K를 다 채워 별칭이 기여하지 못한다.
            # 별칭별 순위를 RRF로 융합해 모든 표시명이 반영되게 한다.
            per_alias = [retrieve(q) for q in variants[acct][label]]
            top = rrf(per_alias, [1.0] * len(per_alias))[:TOP_K]
            h = sum(1 for d in top if acct in truth[d])
            hits += h
            tot += TOP_K
            own = {d for d in ids if acct in truth[d]}
            reached.append(len(own & set(top)) / len(own) if own else 0)
        lo, hi = wilson(hits, tot)
        name = "A 표시명 1개 (현재)" if label == "A_single" else "B 별칭 전체 (별칭 확장)"
        print(f"{name:<24}{hits/tot*100:>13.1f}%{f'[{lo*100:.1f}, {hi*100:.1f}]':>18}{np.mean(reached)*100:>11.1f}%")
        summary[label] = hits / tot

    print(f"\n표시명이 2개 이상인 계정 {len(multi)}개만 따로:")
    for label in ("A_single", "B_aliases"):
        if not multi:
            continue
        hits = tot = 0
        for acct in multi:
            # 순차 연결하면 첫 질의가 top-K를 다 채워 별칭이 기여하지 못한다.
            # 별칭별 순위를 RRF로 융합해 모든 표시명이 반영되게 한다.
            per_alias = [retrieve(q) for q in variants[acct][label]]
            top = rrf(per_alias, [1.0] * len(per_alias))[:TOP_K]
            hits += sum(1 for d in top if acct in truth[d])
            tot += TOP_K
        lo, hi = wilson(hits, tot)
        name = "A 표시명 1개" if label == "A_single" else "B 별칭 전체"
        print(f"  {name:<22}{hits/tot*100:>13.1f}%  [{lo*100:.1f}, {hi*100:.1f}]")
    print(f"\nAPI: {client.usage.summary()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
