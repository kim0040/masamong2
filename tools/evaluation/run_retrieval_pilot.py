"""B0/B1/B2 검색 비교 파일럿.

B0 = bigram FTS(keyword), B1 = dense(qwen3-embedding-8b), B2 = hybrid(RRF).

gold 질문은 LLM이 window에서 생성한다. 두 가지 문체를 만든다.
  literal    : 사용자가 키워드로 검색하듯 원문 표현을 일부 포함
  paraphrase : 원문 표현을 쓰지 않고 의미만 묻는다
paraphrase가 dense의 기여를 드러내는 조건이다.

중간 산출물은 .private/eval/에 저장해 재실행 시 재사용한다(원문 본문 포함되므로 Git 제외).
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).parent))

from corpus import iter_observations  # noqa: E402
from lexical import LexicalIndex  # noqa: E402
from openrouter import OpenRouter, BudgetError  # noqa: E402
from artifacts import (  # noqa: E402
    ArtifactValidationError,
    cache_input_fingerprint,
    documents_fingerprint,
    load_array_cache,
    load_json_cache,
    output_dir,
    save_array_cache,
    save_json_cache,
    write_manifest,
)
from windows import WindowConfig, build_windows as build_bounded_windows  # noqa: E402

OUT = output_dir()
# 한 요인씩만 바꿔 비교하려고 환경변수로 받는다.
WINDOW_SIZE = int(os.environ.get("WINDOW_SIZE", "20"))
WINDOW_OVERLAP = int(os.environ.get("WINDOW_OVERLAP", "0"))
N_WINDOWS = int(os.environ.get("N_WINDOWS", "3000"))
N_QUESTIONS = int(os.environ.get("N_QUESTIONS", "60"))
EMBED_BATCH = 32
SEED = 20260914
TAG = f"w{WINDOW_SIZE}o{WINDOW_OVERLAP}"
COLLECTION = os.environ.get("EVAL_COLLECTION", os.environ.get("COLLECTION", "friends-archive"))


def configured_rooms() -> set[str]:
    """Return an explicit room set; retain the original pilot's one-room default."""

    raw = os.environ.get("EVAL_ROOMS")
    if raw is None:
        raw = os.environ.get("ROOM")
    if raw is None:
        raise ValueError("EVAL_ROOMS or ROOM must be explicitly configured")
    rooms = {item.strip() for item in raw.split(",") if item.strip()}
    if not rooms:
        raise ValueError("EVAL_ROOMS/ROOM must name at least one source room")
    return rooms

GEN_PROMPT = """다음은 한국어 단체 채팅방의 대화 일부다.

<대화>
{body}
</대화>

이 대화에서만 답할 수 있는 질문 2개를 만들어라. JSON만 출력한다.

{{"literal": "...", "paraphrase": "..."}}

- literal: 사용자가 검색하듯 대화 속 핵심 단어를 포함한 질문
- paraphrase: 대화 속 표현을 그대로 쓰지 않고 의미만으로 묻는 질문
- 둘 다 한국어 한 문장. 대화에 실제로 있는 내용만 묻는다.
- 인사말이나 'ㅋㅋ' 같은 감탄사만으로는 질문을 만들지 마라."""


def build_windows(
    collection: str,
    rooms: set[str],
    limit: int,
    config: WindowConfig | dict | None = None,
) -> list[dict]:
    """Compatibility wrapper around the shared source-room bounded builder."""

    recipe = config or WindowConfig(size=WINDOW_SIZE, overlap=WINDOW_OVERLAP)
    observations = (
        obs
        for obs in iter_observations(collection, kind="dated", rooms=set(rooms))
        if obs.semantic_eligible
    )
    return build_bounded_windows(observations, recipe, limit=limit)


def gen_questions(
    client: OpenRouter,
    docs: list[dict],
    n: int,
    rng: np.random.Generator,
    *,
    cache: Path,
    input_fingerprint: str,
    recipe: dict,
    manifest_ref: str,
) -> list[dict]:
    # 부분 실패해도 진행분을 잃지 않도록 성공할 때마다 저장하고 재실행 시 이어받는다.
    gold: list[dict] = []
    if cache.exists():
        try:
            loaded = load_json_cache(
                cache,
                input_fingerprint=input_fingerprint,
                provider={"name": "openrouter", "base_url": client.base},
                model=client.llm_model,
                recipe=recipe,
                manifest_ref=manifest_ref,
            )
            if isinstance(loaded, list):
                gold = loaded
                print(f"  기존 gold {len(gold)}개 이어받음")
        except ArtifactValidationError:
            # Legacy/unmanifested or stale artifacts are never reused.
            print("  기존 gold 캐시 무효화")
    if len(gold) >= n:
        return gold[:n]

    done = {g["doc_id"] for g in gold}
    cands = [d for d in docs if d["body"].strip() and d["doc_id"] not in done]
    print(f"   질문 소스 후보: {len(cands):,}개 (길이 하위 절반도 포함)")
    order = rng.permutation(len(cands))
    attempted = 0
    for j in order:
        if len(gold) >= n:
            break
        doc = cands[j]
        attempted += 1
        try:
            raw = client.chat(
                [{"role": "user", "content": GEN_PROMPT.format(body=doc["body"][:3000])}],
                max_tokens=300,
            )
            # glm-5.3-flash는 reasoning만 내보내고 content=None을 반환할 때가 있다.
            if not raw:
                raise ValueError("빈 응답")
            text = raw.strip()
            if "```" in text:
                text = text.split("```")[1].lstrip("json").strip()
            obj = json.loads(text[text.index("{"): text.rindex("}") + 1])
            if obj.get("literal") and obj.get("paraphrase"):
                gold.append({"doc_id": doc["doc_id"],
                             "literal": obj["literal"], "paraphrase": obj["paraphrase"]})
                save_json_cache(
                    cache,
                    gold,
                    input_fingerprint=input_fingerprint,
                    provider={"name": "openrouter", "base_url": client.base},
                    model=client.llm_model,
                    recipe=recipe,
                    kind="gold_questions",
                    manifest_ref=manifest_ref,
                )
        except BudgetError:
            raise
        except Exception as exc:
            print(f"    시도 {attempted}: 생성 실패 {type(exc).__name__}")
        if attempted % 10 == 0:
            print(f"    진행 {len(gold)}/{n} (시도 {attempted}, {client.usage.summary()})")
    return gold


def embed_all(
    client: OpenRouter,
    texts: list[str],
    cache: Path,
    *,
    input_fingerprint: str,
    recipe: dict,
    manifest_ref: str,
) -> np.ndarray:
    provider = {"name": "openrouter", "base_url": client.base}
    if cache.exists():
        try:
            arr = load_array_cache(
                cache,
                input_fingerprint=input_fingerprint,
                provider=provider,
                model=client.embed_model,
                recipe=recipe,
                manifest_ref=manifest_ref,
            )
            if len(arr) == len(texts) and arr.ndim == 2:
                print(f"  임베딩 재사용: {arr.shape}")
                return arr
        except ArtifactValidationError:
            print("  기존 임베딩 캐시 무효화")
    vecs: list[list[float]] = []
    for i in range(0, len(texts), EMBED_BATCH):
        vecs.extend(client.embed(texts[i : i + EMBED_BATCH]))
        if (i // EMBED_BATCH) % 20 == 0:
            print(f"    임베딩 {len(vecs)}/{len(texts)} ({client.usage.summary()})")
    arr = np.asarray(vecs, dtype=np.float32)
    arr /= np.linalg.norm(arr, axis=1, keepdims=True) + 1e-9
    if arr.ndim != 2 or arr.shape[0] != len(texts):
        raise ValueError("embedding provider returned an unexpected matrix shape")
    save_array_cache(
        cache,
        arr,
        input_fingerprint=input_fingerprint,
        provider=provider,
        model=client.embed_model,
        recipe=recipe,
        kind="embedding",
        manifest_ref=manifest_ref,
    )
    return arr


def rrf(rank_lists: list[list[str]], k: int = 60) -> list[str]:
    scores: dict[str, float] = {}
    for ranks in rank_lists:
        for pos, doc_id in enumerate(ranks):
            scores[doc_id] = scores.get(doc_id, 0.0) + 1.0 / (k + pos + 1)
    return [d for d, _ in sorted(scores.items(), key=lambda x: -x[1])]


def main() -> int:
    rooms = configured_rooms()
    config = WindowConfig(size=WINDOW_SIZE, overlap=WINDOW_OVERLAP).validate()
    if N_QUESTIONS <= 0 or N_WINDOWS <= 0:
        raise ValueError("question/window counts must be positive")
    docs = build_windows(COLLECTION, rooms, N_WINDOWS, config)
    if not docs:
        raise ValueError("no eligible documents in explicit source scope")
    OUT.mkdir(parents=True, exist_ok=True)
    client = OpenRouter(budget_usd=3.0)
    provider = {"name": "openrouter", "base_url": client.base}
    recipe = {"window": config.as_dict(), "collection": COLLECTION, "rooms": sorted(rooms),
              "documents": documents_fingerprint(docs), "normalization": "l2-float32-v1",
              "document_char_limit": 4000, "seed": SEED}
    gold_recipe = {**recipe, "prompt": GEN_PROMPT, "question_count": N_QUESTIONS,
                   "sampling": "all-nonempty-v2", "max_tokens": 300, "reasoning": "low"}
    manifest_ref = f"run_{TAG}.json"
    gold_fp = cache_input_fingerprint(recipe["documents"], provider=provider,
                                    model=client.llm_model, recipe=gold_recipe, kind="gold")
    gold = gen_questions(client, docs, N_QUESTIONS, np.random.default_rng(SEED),
                         cache=OUT/f"gold_{TAG}.json", input_fingerprint=gold_fp,
                         recipe=gold_recipe, manifest_ref=manifest_ref)
    if len(gold) != N_QUESTIONS:
        raise ValueError("incomplete synthetic question generation; no completed evaluation")
    doc_texts = [d["body"][:4000] for d in docs]
    q_texts = [g["literal"] for g in gold] + [g["paraphrase"] for g in gold]
    doc_fp = cache_input_fingerprint(doc_texts, provider=provider, model=client.embed_model, recipe=recipe, kind="documents")
    query_fp = cache_input_fingerprint(q_texts, provider=provider, model=client.embed_model, recipe=recipe, kind="queries")
    doc_vecs = embed_all(client, doc_texts, OUT/f"doc_vecs_{TAG}.npy", input_fingerprint=doc_fp, recipe=recipe, manifest_ref=manifest_ref)
    q_vecs = embed_all(client, q_texts, OUT/f"q_vecs_{TAG}.npy", input_fingerprint=query_fp, recipe=recipe, manifest_ref=manifest_ref)
    manifest = dict(status="complete", measurement_scope="synthetic_single_target_window_hit",
                    source=dict(collection=COLLECTION, rooms=sorted(rooms), limit=N_WINDOWS),
                    config=config.as_dict(), recipe=recipe, gold_recipe=gold_recipe,
                    provider=provider, embed_model=client.embed_model, llm_model=client.llm_model,
                    gold_fingerprint=gold_fp, doc_fingerprint=doc_fp, query_fingerprint=query_fp,
                    artifacts=dict(gold=f"gold_{TAG}.json", documents=f"doc_vecs_{TAG}.npy", queries=f"q_vecs_{TAG}.npy"))

    print("4) 색인")
    fts = LexicalIndex("bigram")
    fts.add_many([(d["doc_id"], d["body"]) for d in docs])
    ids = [d["doc_id"] for d in docs]

    print("5) 평가\n")
    results: dict[str, dict[str, list[int]]] = {}
    for style, offset in (("literal", 0), ("paraphrase", len(gold))):
        results[style] = {"B0": [], "B1": [], "B2": []}
        for i, g in enumerate(gold):
            want = g["doc_id"]
            qv = q_vecs[offset + i]

            b0 = [d for d, _ in fts.search(g[style], limit=50)]
            sims = doc_vecs @ qv
            b1 = [ids[j] for j in np.argsort(-sims)[:50]]
            b2 = rrf([b0, b1])

            for name, ranked in (("B0", b0), ("B1", b1), ("B2", b2)):
                results[style][name].append(
                    ranked.index(want) + 1 if want in ranked[:10] else 0
                )

    n = len(gold)
    print(f"{'조건':<12}{'베이스라인':<10}{'Hit@1':>10}{'Hit@5':>10}{'Hit@10':>11}{'MRR@10':>8}")
    print("-" * 61)
    label = {"B0": "B0 keyword", "B1": "B1 dense", "B2": "B2 hybrid"}
    for style in ("literal", "paraphrase"):
        for name in ("B0", "B1", "B2"):
            r = results[style][name]
            at1 = sum(1 for x in r if x == 1) / n
            at5 = sum(1 for x in r if 1 <= x <= 5) / n
            at10 = sum(1 for x in r if x >= 1) / n
            mrr = sum(1 / x for x in r if x >= 1) / n
            print(f"{style:<12}{label[name]:<10}{at1*100:>9.1f}%{at5*100:>9.1f}%{at10*100:>10.1f}%{mrr:>8.3f}")
        print()

    print(f"API 사용량: {client.usage.summary()}")
    print(f"오류 분포: {client.usage.errors}")
    (OUT / f"results_{TAG}.json").write_text(
        json.dumps({"n": n, "results": results, "usage": client.usage.summary()},
                   ensure_ascii=False, indent=2), encoding="utf-8")
    manifest["results"] = results
    manifest["usage"] = client.usage.summary()
    write_manifest(OUT / manifest_ref, manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
