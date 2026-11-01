"""레거시 봇의 실제 사용자 질문 분포 분석.

analytics_log의 AI_INTERACTION.user_query는 구 마사몽에 실제로 들어온 질문이다.
RAW_DATA_POLICY는 legacy 가공물을 V2 입력으로 쓰는 것을 금지하지만, 기존 기능의
동작 기준을 parity 조사에 참고하는 것은 허용한다. 이 스크립트는 평가 설계 근거로만
읽으며 어떤 결과도 V2 파이프라인 입력이 되지 않는다.

손으로 쓴 정규식은 분석자의 선입견이 들어가므로 LLM 분류를 기준으로 삼고,
정규식은 교차 확인용으로만 쓴다.
"""
from __future__ import annotations

import json
import os
import random
import sqlite3
import sys
from collections import Counter
from math import sqrt
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from openrouter import OpenRouter, BudgetError
from artifacts import output_dir, cache_input_fingerprint, load_json_cache, save_json_cache, ArtifactValidationError  # noqa: E402

SNAPSHOT = Path(os.environ.get("LEGACY_SNAPSHOT", ".private/legacy-snapshot.sqlite3"))
OUT = output_dir()
SAMPLE = int(os.environ.get("SAMPLE", "300"))
BATCH = 20
SEED = 20260914

CATEGORIES = {
    "person": "서버 멤버 개인에 대한 질문 (정체·성격·취향·능력·평가)",
    "episode": "과거 대화의 특정 사건이나 발언 회상",
    "followup": "직전 대화 맥락에 의존하는 후속 발화 (그거/아까 그 얘기 등)",
    "teach": "사용자가 봇에게 사실을 알려주거나 기억하라고 지시",
    "external": "날씨·뉴스·주가·검색 등 외부 실시간 정보 요청",
    "knowledge": "대화 기록과 무관한 일반 지식 질문",
    "chitchat": "인사·감탄·잡담·욕설 등 정보 요구가 없는 발화",
    "botmeta": "봇 자신의 정체·기능·설정에 대한 질문이나 지시",
}

PROMPT = """아래 한국어 질문들을 정해진 범주 하나씩으로 분류하라.

범주:
{cats}

질문:
{items}

JSON 배열만 출력한다. 형식: [{{"i": 1, "c": "person"}}, ...]
각 질문에 대해 가장 잘 맞는 범주 하나만 고른다."""


def load_queries() -> list[str]:
    db = sqlite3.connect(f"file:{SNAPSHOT}?mode=ro", uri=True)
    meta = json.loads(
        db.execute("SELECT metadata_json FROM source_schema WHERE table_name='analytics_log'")
        .fetchone()[0])
    cols = [c["name"] for c in meta["columns"]]
    out = []
    for (rj,) in db.execute("SELECT row_json FROM source_rows WHERE table_name='analytics_log' ORDER BY row_json"):
        row = dict(zip(cols, json.loads(rj)))
        if row["event_type"] != "AI_INTERACTION" or not row.get("details"):
            continue
        try:
            d = json.loads(row["details"])
        except Exception:
            continue
        q = (d.get("user_query") or "").strip()
        if q:
            out.append(q)
    db.close()
    return out


def wilson(k: int, n: int) -> tuple[float, float]:
    """이항 비율의 95% Wilson 신뢰구간."""
    if n == 0:
        return 0.0, 0.0
    p, z = k / n, 1.96
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return max(0.0, c - h), min(1.0, c + h)


def validate_labels(parsed, n):
    if not isinstance(parsed,list) or len(parsed)!=n:
        raise ValueError("classification batch incomplete")
    indexes=[]
    for obj in parsed:
        if not isinstance(obj,dict) or type(obj.get("i")) is not int or obj.get("c") not in CATEGORIES:
            raise ValueError("classification batch invalid")
        indexes.append(obj["i"])
    if sorted(indexes)!=list(range(1,n+1)):
        raise ValueError("classification indexes missing or duplicated")


def main() -> int:
    OUT.mkdir(parents=True, exist_ok=True)
    queries = load_queries()
    print(f"실제 user_query 총 {len(queries):,}건 (고유 {len(set(queries)):,})")

    rng = random.Random(SEED)
    sample = rng.sample(queries, min(SAMPLE, len(queries)))
    print(f"무작위 표본 {len(sample)}건 분류 시작\n")

    cache = OUT / "real_query_labels.json"
    client = OpenRouter(budget_usd=1.0)
    recipe = {"prompt":PROMPT,"categories":CATEGORIES,"seed":SEED,"sample":SAMPLE,"char_limit":160,"reasoning":"low"}
    provider = {"name":"openrouter","base_url":client.base}
    fp = cache_input_fingerprint(sample,provider=provider,model=client.llm_model,recipe=recipe,kind="query_labels")
    labels = {}
    if cache.exists():
        try:
            labels = load_json_cache(cache,input_fingerprint=fp,provider=provider,model=client.llm_model,recipe=recipe)
        except ArtifactValidationError:
            pass
    cats = "\n".join(f"- {k}: {v}" for k, v in CATEGORIES.items())
    todo = [q for q in sample if q not in labels]
    for start in range(0, len(todo), BATCH):
        chunk = todo[start : start + BATCH]
        items = "\n".join(f"{i+1}. {q[:160]}" for i, q in enumerate(chunk))
        try:
            raw = client.chat(
                [{"role": "user", "content": PROMPT.format(cats=cats, items=items)}],
                max_tokens=900)
            if not raw:
                raise ValueError("빈 응답")
            text = raw.strip()
            if "```" in text:
                text = text.split("```")[1].lstrip("json").strip()
            parsed = json.loads(text[text.index("["): text.rindex("]") + 1])
            validate_labels(parsed, len(chunk))
            for obj in parsed:
                idx = int(obj["i"]) - 1
                if 0 <= idx < len(chunk) and obj.get("c") in CATEGORIES:
                    labels[chunk[idx]] = obj["c"]
            save_json_cache(cache,labels,input_fingerprint=fp,provider=provider,model=client.llm_model,recipe=recipe,kind="query_labels")
        except BudgetError:
            raise
        except Exception as exc:
            print(f"  배치 {start//BATCH+1} 실패: {type(exc).__name__}")
        print(f"  진행 {len([q for q in sample if q in labels])}/{len(sample)} ({client.usage.summary()})")

    got = [labels[q] for q in sample if q in labels]
    n = len(got)
    if not n or n != len(sample):
        raise ValueError("incomplete classification; cannot report completed distribution")
    counts = Counter(got)
    print(f"\n=== 실제 질문 범주 분포 (n={n}, 95% CI) ===")
    print(f"{'범주':<12}{'건수':>6}{'비율':>8}{'95% CI':>18}  설명")
    print("-" * 92)
    for cat in CATEGORIES:
        k = counts.get(cat, 0)
        lo, hi = wilson(k, n)
        print(f"{cat:<12}{k:>6}{k/n*100:>7.1f}%{f'[{lo*100:.1f}, {hi*100:.1f}]':>18}  {CATEGORIES[cat]}")

    mem = sum(counts.get(c, 0) for c in ("person", "episode"))
    lo, hi = wilson(mem, n)
    print(f"\nperson+episode 범주 합계(검색 필요 여부를 뜻하지 않음): {mem}/{n} = {mem/n*100:.1f}% "
          f"[{lo*100:.1f}, {hi*100:.1f}]")
    print(f"API: {client.usage.summary()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
