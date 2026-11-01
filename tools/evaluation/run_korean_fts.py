"""한국어 FTS tokenizer 비교 실험 (API 호출 0회).

실제 raw-corpus 관찰로 window를 만들고, 각 tokenizer 변형에서
부분 문자열 질의가 원문을 회수하는지 측정한다.
본문은 출력하지 않고 집계만 보고한다.
"""
from __future__ import annotations

import os
import random
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from windows import WindowConfig, build_windows as bounded_windows
from corpus import iter_observations  # noqa: E402
from lexical import VARIANTS, LexicalIndex  # noqa: E402

WINDOW_SIZE = 20
SAMPLE_DOCS = 4000
PROBES = 400
SEED = 20260914

_TOKEN = re.compile(r"[가-힣]{4,8}")


def build_windows(collection: str, rooms: set[str] | None, limit: int) -> list[tuple[str, str]]:
    """semantic_eligible 관찰만 모아 고정 크기 window로 묶는다."""
    if not rooms:
        raise ValueError("explicit source rooms required")
    return [(d["doc_id"],d["body"]) for d in bounded_windows((o for o in iter_observations(collection,kind="dated",rooms=rooms) if o.semantic_eligible), WindowConfig(size=WINDOW_SIZE),limit=limit)]



def main() -> int:
    rng = random.Random(SEED)
    collection = os.environ.get("EVAL_COLLECTION", "").strip()
    room = os.environ.get("EVAL_ROOM", "").strip()
    if not collection or not room:
        raise ValueError("EVAL_COLLECTION and EVAL_ROOM must be configured")
    docs = build_windows(collection, {room}, SAMPLE_DOCS)
    print(f"색인 대상 window: {len(docs):,}개 (크기 {WINDOW_SIZE})")

    # 질의 생성: 한 window에만 나타나는 4~8자 한글 토큰을 고른다.
    freq: dict[str, set[str]] = {}
    for doc_id, body in docs:
        for tok in sorted(set(_TOKEN.findall(body))):
            freq.setdefault(tok, set()).add(doc_id)
    unique = [(t, next(iter(d))) for t, d in freq.items() if len(d) == 1]
    unique.sort()
    rng.shuffle(unique)
    probes = unique[:PROBES]
    print(f"질의 후보(단일 window에만 등장하는 토큰): {len(unique):,}개 중 {len(probes)}개 사용\n")

    forms = {
        "exact      (원형 그대로)": lambda t: t,
        "minus1     (조사 1자 제거)": lambda t: t[:-1],
        "minus2     (2자 제거)": lambda t: t[:-2],
        "prefix2    (앞 2자만)": lambda t: t[:2],
    }

    indexes = {}
    for name in VARIANTS:
        idx = LexicalIndex(name)
        idx.add_many(docs)
        indexes[name] = idx

    header = f"{'tokenizer':<16}" + "".join(f"{f.split()[0]:>10}" for f in forms)
    print(header)
    print("-" * len(header))
    results = {}
    for name, idx in indexes.items():
        row = f"{name:<16}"
        results[name] = {}
        for fname, fn in forms.items():
            hit = 0
            for tok, want in probes:
                q = fn(tok)
                if len(q) < 1:
                    continue
                if want in [d for d, _ in idx.search(q, limit=10)]:
                    hit += 1
            rate = hit / len(probes)
            results[name][fname] = rate
            row += f"{rate*100:>9.1f}%"
        print(row)

    print("\n=== 알려진 실패 사례 재현 ===")
    probe_idx = {}
    for name in VARIANTS:
        i = LexicalIndex(name)
        i.add_many([("d1", "오늘 초코우유를 사왔어 진짜 맛있더라"), ("d2", "다음주에 부산 내려간다")])
        probe_idx[name] = i
    cases = [("초코우유", "d1", "조사 붙은 어간 검색"), ("부산", "d2", "2글자 검색")]
    print(f"{'tokenizer':<16}" + "".join(f"{c[0]:>12}" for c in cases))
    print("-" * 40)
    for name, i in probe_idx.items():
        row = f"{name:<16}"
        for q, want, _ in cases:
            ok = want in [d for d, _ in i.search(q, limit=5)]
            row += f"{'OK' if ok else 'FAIL':>12}"
        print(row)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
