"""인물 중심 검색 파일럿.

레거시 실사용 분석 결과 장기기억 질문의 최다 범주는 `person`(23.7%)이고
`episode`(7.7%)의 3배다. 1차 파일럿은 episode만 측정했으므로 그 공백을 메운다.

과적합 방지:
  - recipe(bigram / window 20 / dense 4배 RRF)는 1차 파일럿에서 고정한 값을 그대로 쓴다.
    이 스크립트에서 재튜닝하지 않는다.
  - 1차와 다른 source room으로 검증한다.
  - 질의 문구는 실사용 로그에서 관찰된 형태만 쓰고 특정 인물에 맞추지 않는다.
  - Wilson 구간은 반복 인물/window의 상관을 반영하지 못하는 pooled 진단이다.

인물 질의는 "정답 window 1개"가 아니라 "그 인물의 근거를 모으는" 과제이므로
Recall@k가 아니라 precision@k와 화자/언급 구분을 측정한다.
"""
from __future__ import annotations

import json
import os
import re
import sys
from collections import Counter
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
COLLECTION = os.environ.get("COLLECTION", "discord")
ROOM = os.environ.get("ROOM", "")
WINDOW_SIZE = 20           # 1차 파일럿에서 고정
N_WINDOWS = int(os.environ.get("N_WINDOWS", "3000"))
MIN_MESSAGES = 150         # 인물 선정 하한
TOP_K = 10
RRF_DENSE_WEIGHT = 4.0     # 1차 파일럿에서 고정
TAG = f"person_{COLLECTION}"

# 실사용 형태를 참고한 고정 합성 문형이다. 실제 질문 재현이 아니다. 특정 인물에 맞춘 문구를 넣지 않는다.
QUERY_TEMPLATES = [
    "{name}이 누구야",
    "우리 서버 {name} 어때",
    "{name} 성격 분석 해줘",
    "{name}는 어떤 사람이야",
    "{name} 뭐 좋아함",
]


def wilson(k: int, n: int) -> tuple[float, float]:
    if n == 0:
        return 0.0, 0.0
    p, z = k / n, 1.96
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return max(0.0, c - h), min(1.0, c + h)


def typo(name: str) -> str | None:
    """한글 이름의 첫 글자 초성을 바꿔 실제 로그에서 본 오타(김하늘->긴하늘)를 흉내낸다."""
    for i, ch in enumerate(name):
        if "가" <= ch <= "힣":
            code = ord(ch) - 0xAC00
            lead, rest = code // 588, code % 588
            return name[:i] + chr(0xAC00 + ((lead + 1) % 19) * 588 + rest) + name[i + 1:]
    return None


def build_windows(limit: int) -> list[dict]:
    if not ROOM.strip():
        raise ValueError("ROOM must explicitly select one source channel")
    return bounded_windows((o for o in iter_observations(COLLECTION, kind="dated", rooms={ROOM}) if o.semantic_eligible), WindowConfig(size=WINDOW_SIZE), limit=limit)



def rrf(rank_lists: list[list[str]], weights: list[float], k: int = 60) -> list[str]:
    scores: dict[str, float] = {}
    for ranks, w in zip(rank_lists, weights):
        for pos, doc_id in enumerate(ranks):
            scores[doc_id] = scores.get(doc_id, 0.0) + w / (k + pos + 1)
    return [d for d, _ in sorted(scores.items(), key=lambda x: -x[1])]


def main() -> int:
    docs = build_windows(N_WINDOWS)
    OUT.mkdir(parents=True, exist_ok=True)
    client = OpenRouter(budget_usd=2.0)
    ids = [d["doc_id"] for d in docs]
    print(f"[{COLLECTION}] window {len(docs):,}개 (크기 {WINDOW_SIZE})")

    speaker_count = Counter(s for d in docs for s in d["speakers"])
    persons = [n for n, c in speaker_count.items() if c >= MIN_MESSAGES and n != "?"]
    print(f"대상 인물: {len(persons)}명 (해당 구간 {MIN_MESSAGES}발화 이상)\n")
    if not persons:
        print("인물 부족. 중단."); return 1

    # 각 window가 인물에 대해 어떤 근거를 갖는지 사전 계산
    spoke = {d["doc_id"]: set(d["speakers"]) for d in docs}
    mentioned: dict[str, set[str]] = {d["doc_id"]: set() for d in docs}
    for d in docs:
        joined = "\n".join(d["texts"])
        for p in persons:
            if p in joined:
                mentioned[d["doc_id"]].add(p)

    fts = LexicalIndex("bigram")
    fts.add_many([(d["doc_id"], d["body"]) for d in docs])

    recipe = {"documents": documents_fingerprint(docs), "collection": COLLECTION,
              "room": ROOM, "window": WindowConfig(size=WINDOW_SIZE).as_dict(), "normalization": "l2-float32-v1", "char_limit":4000}
    def vectors(texts, name):
        fp = cache_input_fingerprint(texts, provider={"name":"openrouter","base_url":client.base}, model=client.embed_model, recipe=recipe, kind=name)
        return embed_all(client, texts, OUT/f"{name}_{TAG}.npy", input_fingerprint=fp, recipe=recipe, manifest_ref=f"person_{TAG}.json")
    doc_vecs = vectors([d["body"][:4000] for d in docs], "documents")

    # 질의 생성 + 임베딩
    queries = []
    for p in persons:
        for t in QUERY_TEMPLATES:
            queries.append({"person": p, "q": t.format(name=p), "variant": "normal"})
        tp = typo(p)
        if tp:
            queries.append({"person": p, "q": f"{tp}이 누구야", "variant": "typo"})
    q_vecs = vectors([x["q"] for x in queries], "queries")
    print(f"질의 {len(queries)}개 (인물 {len(persons)} x 문형 {len(QUERY_TEMPLATES)} + 오타)\n")

    stats: dict[tuple[str, str], list[int]] = {}

    def record(key, hits, total):
        stats.setdefault(key, [0, 0])
        stats[key][0] += hits
        stats[key][1] += total

    for i, item in enumerate(queries):
        p, variant = item["person"], item["variant"]
        b0 = [d for d, _ in fts.search(item["q"], limit=50)]
        b1 = [ids[j] for j in np.argsort(-(doc_vecs @ q_vecs[i]))[:50]]
        b2 = rrf([b0, b1], [1.0, RRF_DENSE_WEIGHT])

        for name, ranked in (("B0 keyword", b0), ("B1 dense", b1), ("B2 hybrid", b2)):
            top = ranked[:TOP_K]
            rel = sum(1 for d in top if p in spoke[d] or p in mentioned[d])
            sp = sum(1 for d in top if p in spoke[d])
            mn = sum(1 for d in top if p in mentioned[d])
            record((name, f"{variant}:관련"), rel, TOP_K)
            record((name, f"{variant}:화자"), sp, TOP_K)
            record((name, f"{variant}:언급"), mn, TOP_K)

    print(f"=== 합성 질의 표시명 존재율@{TOP_K} (답변 품질 아님; pooled CI는 독립성 미보장) ===")
    print(f"{'베이스라인':<12}{'조건':<14}{'precision':>11}{'95% CI':>18}")
    print("-" * 56)
    for name in ("B0 keyword", "B1 dense", "B2 hybrid"):
        for cond in ("normal:관련", "normal:화자", "normal:언급", "typo:관련"):
            k, n = stats.get((name, cond), [0, 0])
            if n == 0:
                continue
            lo, hi = wilson(k, n)
            print(f"{name:<12}{cond:<14}{k/n*100:>10.1f}%{f'[{lo*100:.1f}, {hi*100:.1f}]':>18}")
        print()

    base = len([d for d in docs]) or 1
    print("=== 참고: 무작위 기준선 (인물별 window 점유율 평균) ===")
    avg = np.mean([
        sum(1 for d in ids if p in spoke[d] or p in mentioned[d]) / base for p in persons])
    print(f"  무작위로 10개 뽑았을 때 기대 precision: {avg*100:.1f}%")
    print(f"\nAPI: {client.usage.summary()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
