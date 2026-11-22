"""Sandbox engine, retrieval half: intent, candidates, fusion, rerank and retrieve() (re-exported by engine.py)."""

from __future__ import annotations

import json
import math
import os
import re
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from engine_index import (
    IndexBundle,
    PlaygroundError,
    ThreadSafeLexicalIndex,
    _elapsed_ms,
    _parse_ts,
    configured_models,
)

from client import (  # noqa: E402
    DEFAULT_EMBED_QUERY_INSTRUCTION,
    PlayClient,
    load_secrets,
)
from retrieval_post import (  # noqa: E402
    TimeReference as RetrievalTimeReference,
    narrow_time_ids,
    resolve_time_scope,
    finalize_ranked_ids,
    merged_span,
    recency_anchor_ts,
    scaled_person,
    search_limits,
    resolve_rerank_min_score,
)
from rag_upgrade.playground_debug import (  # noqa: E402
    RankingError,
    normalize_ranking,
)
from rag_upgrade.rerank_client import (  # noqa: E402
    RerankClient,
    RerankError,
)
from evidence_packing import (  # noqa: E402
    make_time_filter,
    pack_evidence_set,
)
from memory_blocks import (  # noqa: E402
    leaf_ids_for_blocks,
    platform_of_room,
    _label_for_room,
)
from candidates import (  # noqa: E402
    FusionResult,
    fuse,
    guarantee_pool,
    source_family,
)
from candidates_assess import assess  # noqa: E402
from person_retrieval import (  # noqa: E402
    alias_run,
    build_identity_table,
    resolve_persons,
    speaker_fusion,
)
from retrieval_config import (  # noqa: E402
    pool_size,
    retrieval_config,
)
from message_vectors import (  # noqa: E402
    MESSAGE_RRF_WEIGHT,
    MESSAGE_RUN_NAME,
    message_dense_run,
)
from query_embed import query_embed_config  # noqa: E402
from soft_events import (  # noqa: E402
    SoftEventRecipe,
    build_soft_events,
    project_event_ranks,
)
from tool_parity import (  # noqa: E402
    decide_ranking,
    fallback_ranking,
)

def weighted_rrf(
    rank_lists: list[list[str]],
    weights: list[float],
    *,
    k: int = 60,
) -> list[str]:
    """테스트 기준 구현. 검색 경로는 candidates.py를 쓰고, 이 함수는
    test_candidates.py가 순서가 같은지 확인하는 oracle로만 남아 있다."""
    scores: dict[str, float] = {}
    for ranks, weight in zip(rank_lists, weights):
        for pos, doc_id in enumerate(ranks):
            scores[doc_id] = scores.get(doc_id, 0.0) + weight / (k + pos + 1)
    return [doc_id for doc_id, _ in sorted(scores.items(), key=lambda item: -item[1])]


_PARTICLE_CHARS = set("이가은는을를도만의와과야아여요랑")


def _strip_name_particles(word: str) -> str:
    out = word
    for _ in range(3):
        if out and out[-1] in _PARTICLE_CHARS:
            out = out[:-1]
        else:
            break
    return out


def _token_in_text(
    text: str,
    tok: str,
    *,
    labels: set[str] | None = None,
) -> bool:
    """토큰 매칭. 조사(이/가)와 성+이름(김철수⊃철수), 별칭 라벨 포함(닉네임⊃이름)을 허용."""

    if not tok or not text or tok not in text:
        return False
    labels = labels or set()
    start = 0
    n = len(tok)
    while True:
        i = text.find(tok, start)
        if i < 0:
            return False
        before = text[i - 1] if i > 0 else ""
        after = text[i + n] if i + n < len(text) else ""
        hangul_before = bool(before) and "가" <= before <= "힣"
        hangul_after = bool(after) and "가" <= after <= "힣"
        if not hangul_before and not hangul_after:
            return True
        left = i
        right = i + n
        while left > 0 and "가" <= text[left - 1] <= "힣":
            left -= 1
        while right < len(text) and "가" <= text[right] <= "힣":
            right += 1
        word = text[left:right]
        core = _strip_name_particles(word)
        if core == tok or core in labels:
            return True
        # 더 긴 별칭 라벨(긴 닉네임)이 단어에 포함.
        if any(
            lab and lab != tok and len(lab) > len(tok) and lab in core for lab in labels
        ):
            return True
        # 성+이름: 김철수 ⊃ 철수 (성 1~2글자)
        if core.endswith(tok):
            prefix = core[: len(core) - len(tok)]
            if 1 <= len(prefix) <= 2 and all("가" <= c <= "힣" for c in prefix):
                return True
        if hangul_before:
            start = i + 1
            continue
        # 뒤에만 조사
        if hangul_after and all(c in _PARTICLE_CHARS for c in word[n:]):
            return True
        start = i + 1


# 소개형 질문에서 "정체성 단서"로 볼 용어. 기본값은 첫 실험 코퍼스에 맞춘 것이라
# 다른 채널에서는 config `identity_terms` 로 바꿔야 한다 (코드에 인물 특성을 박지 않는다).
DEFAULT_IDENTITY_TERMS: tuple[str, ...] = ()  # 비어 있으면 소개형 정체성 가산 없음


_RECENT_RE = re.compile(
    r"(최근|요즘|요새|요즈음|근래|방금|오늘|어제|이번\s*주|이번주|이번\s*달)"
)


_INTRO_RE = re.compile(r"(소개|누구야|누구\s*야|어떤\s*사람|뭐\s*하는\s*사람|프로필)")


# 오탐 차단: 지지난주(지지), 보수공사·보수를 받다(보수), 성향 테스트(성향)는 정치가 아니다.
_POLITICS_RE = re.compile(
    r"(정치|정치\s*성향|지지(?!난|율)(?=\s*(하|해|층|자|정당|후보|당|율)|$)"
    r"|보수(?!\s*(공사|작업|비|료|를|점검|유지|하|되|공))"
    r"|진보(?!\s*(하|되|된|시|를))"
    r"|좌파|우파|대선|대통령|국힘|민주당|정당)"
)


def _identity_re(cfg: dict[str, Any]) -> re.Pattern[str] | None:
    terms = cfg.get("identity_terms")
    if not isinstance(terms, list) or not terms:
        terms = list(DEFAULT_IDENTITY_TERMS)
    terms = [str(t) for t in terms if str(t).strip()]
    if not terms:
        return None
    return re.compile("|".join(re.escape(t) for t in terms), re.I)


def _source_date(value: Any) -> str:
    """원문 시각의 날짜를 그 원문 기준으로 ("2026-07-25"). 시간대가 없는 카카오 시각을 이 컴퓨터의 시간대로
    읽어 UTC로 바꾸면 새벽 대화가 하루 앞당겨지므로 변환하지 않는다. Go corpus.ParseTime + Format과 같다."""
    if not value or not isinstance(value, str):
        return ""
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text).strftime("%Y-%m-%d")
    except ValueError:
        return ""


def _leaf_source_date(index: Any, leaf: dict[str, Any]) -> str:
    """leaf 마지막 원문(없으면 첫 원문)의 날짜. Go search.leafDate와 같다."""
    messages = index.messages
    for i in (int(leaf.get("end") or 0) - 1, int(leaf.get("start") or 0)):
        if 0 <= i < len(messages):
            d = _source_date(messages[i].get("parsed_time"))
            if d:
                return d
    return ""


def _context_only_date(index: Any, packed: dict[str, Any], leaf: dict[str, Any]) -> str:
    """근거의 matched 원문이 모두 앞 근거에 이미 보여서 앞뒤 문맥만 남았으면, 날짜 라벨을 실제로 보이는
    원문의 날짜("2026-08-11" 또는 "2026-08-11~2026-09-06")로 한다. 그 밖의 근거는 leaf 날짜 그대로("").
    Go internal/memory/search/retrieve.go contextOnlyDate와 같은 규칙."""
    visible = {str(x) for x in (packed.get("visible_observation_ids") or [])}
    if not visible:
        return ""
    if any(str(o) in visible for o in (leaf.get("observation_ids") or [])):
        return ""
    first = last = ""
    messages = index.messages
    start = int(packed.get("start") or 0)
    end = min(int(packed.get("end") or 0), len(messages))
    for i in range(start, end):
        row = messages[i]
        if str(row.get("observation_id")) not in visible:
            continue
        d = _source_date(row.get("parsed_time"))
        if not d:
            continue
        if not first:
            first = d
        last = d
    if not first or first == last:
        return first
    return f"{first}~{last}"


def _query_intent(query: str) -> dict[str, Any]:
    """Regex fallback only. 플래너가 돌지 않는 경로(평가 스크립트·직접 retrieve 호출)에서 쓴다.
    런타임은 플래너 LLM 출력(`intent_from_plan`)을 우선한다 — 키워드는 방마다 다르다."""

    recent = bool(_RECENT_RE.search(query))
    intro = bool(_INTRO_RE.search(query))
    politics = bool(_POLITICS_RE.search(query))
    return {
        "recent": recent,
        "intro": intro,
        "politics": politics,
        "prefer_missing_over_guess": politics or ("어때" in query and "정치" in query),
        "kind": "person_intro" if intro else "other",
        "targets": None,  # None → 별칭 표를 질의에 regex 매칭해서 찾는다
        "source": "regex",
    }


def _neutral_intent() -> dict[str, Any]:
    """`intent_fallback: none` — no corpus keywords. Without planner output the query is treated as a
    plain search (no recency/intro/sensitive flags, no alias-table matching of the raw query)."""

    return {
        "recent": False,
        "intro": False,
        "politics": False,
        "prefer_missing_over_guess": False,
        "kind": "other",
        "targets": [],
        "source": "none",
    }


def resolve_intent(
    query: str, intent: dict[str, Any] | None, cfg: Mapping[str, Any] | None = None
) -> dict[str, Any]:
    """Planner intent wins. Without planner output the default (`intent_fallback: none`) is a neutral
    intent; the legacy Korean keyword regex is opt-in (`intent_fallback: regex`)."""

    # Default is `none` since the 2026-09-23 generalization audit: on holdout gold (Discord + two Kakao
    # rooms) the keyword fallback changed nothing measurable, and its word lists are corpus-specific.
    mode = str((cfg or {}).get("intent_fallback") or "none")
    fallback = _query_intent(query) if mode == "regex" else _neutral_intent()
    if not intent:
        return fallback
    out = dict(fallback)
    out.update(intent)
    return out


# 별칭 키가 흔한 낱말과 겹칠 때 인물 부스트가 잘못 켜지는 걸 막는다 (regex fallback 전용).
# 코퍼스마다 다르므로 코드 기본값은 비우고 config `person_alias_ignore`로만 준다.
DEFAULT_ALIAS_IGNORE: frozenset[str] = frozenset()


# 플래너가 person_targets에 넣을 수 있는 대명사·지시어. 사람 이름이 아니므로 버린다.
_PRONOUNS = frozenset({"걔", "그", "그녀", "그분", "그사람", "그 사람", "얘", "이 사람", "저 사람", "너", "나", "저", "우리", "애", "쟤", "누구", "본인", "자기", "봇", "마사몽"})


def _clean_planner_targets(raw: list[Any]) -> list[str]:
    """플래너 person_targets 정리: 조사 제거(철수가→철수), 대명사 제외, 길이 2~40, 중복 제거."""

    out: list[str] = []
    for item in raw:
        name = str(item or "").strip()
        if not name or name in _PRONOUNS:
            continue
        stripped = _strip_name_particles(name) if all("가" <= c <= "힣" for c in name) else name
        if len(stripped) >= 2:
            name = stripped
        # 한 글자는 이름이 아니라 조사·지시어일 확률이 높다 (예: "이").
        if 2 <= len(name) <= 40 and name not in _PRONOUNS and name not in out:
            out.append(name)
    return out


def _person_identity_mode(cfg: Mapping[str, Any]) -> str:
    """`retrieval.person.identity`: `observed` (default — labels one native account used, derived from
    the index; Kakao labels stay unlinked), `config` (legacy hand-written `person_aliases`) or `both`.
    Holdout R2 results were identical with and without the hand table (2026-09-23 audit)."""

    person = ((cfg or {}).get("retrieval") or {}).get("person") or {}
    mode = str(person.get("identity") or "observed")
    return mode if mode in {"config", "observed", "both"} else "observed"


def _identity_for(index: Any) -> Any:
    """Account-backed identity table, cached on the index (derived from rows, never hand-written)."""

    cached = getattr(index, "_identity_table", None)
    if cached is None:
        cached = build_identity_table(index)
        try:
            index._identity_table = cached
        except AttributeError:
            pass
    return cached


def _observed_alias_table(index: Any) -> dict[str, set[str]]:
    """label → all labels the same *native account* used. Label-only keys (Kakao) add nothing."""

    table = _identity_for(index)
    out: dict[str, set[str]] = {}
    for key, labels in table.labels_by_key.items():
        if not getattr(key, "linked", False) or len(labels) < 2:
            continue
        for lab in labels:
            out.setdefault(lab, set()).update(labels)
    return out


def _alias_table_for(index: Any) -> dict[str, set[str]]:
    mode = _person_identity_mode(index.cfg)
    if mode == "config":
        return _alias_table(index.cfg)
    observed = _observed_alias_table(index)
    if mode == "observed":
        return observed
    merged = {k: set(v) for k, v in _alias_table(index.cfg).items()}
    for k, v in observed.items():
        merged.setdefault(k, set()).update(v)
    return merged


def _alias_table(cfg: dict[str, Any]) -> dict[str, set[str]]:
    """검색 힌트용 별칭. 제품 인물 병합이 아니라 sandbox retrieval boost 전용."""

    raw = cfg.get("person_aliases") or {}
    out: dict[str, set[str]] = {}
    if not isinstance(raw, dict):
        return out
    for key, values in raw.items():
        k = str(key).strip()
        if not k:
            continue
        labels = {k}
        if isinstance(values, list):
            labels.update(str(v).strip() for v in values if str(v).strip())
        out[k] = labels
    return out


def _query_targets(
    query: str,
    aliases: dict[str, set[str]],
    ignore: set[str] | frozenset[str] = DEFAULT_ALIAS_IGNORE,
) -> list[str]:
    """질의 안의 인물 별칭 키. 조사·성+이름 경계를 지키고(철수영 ⊅ 철수), ASCII는 대소문자 무시."""

    hits: list[str] = []
    q_fold = query.casefold()
    for key in sorted(aliases.keys(), key=len, reverse=True):
        if not key or key.casefold() in {x.casefold() for x in ignore}:
            continue
        if key.isascii():
            if re.search(r"(?<![a-z0-9])" + re.escape(key.casefold()) + r"(?![a-z0-9])", q_fold):
                hits.append(key)
            continue
        if _token_in_text(query, key, labels=aliases.get(key) or {key}):
            hits.append(key)
    # 더 긴 별칭에 흡수되는 짧은 키 제거 (예: 김철수 ∩ 철수)
    return [
        key
        for key in hits
        if not any(key != other and key in other for other in hits)
    ]


def _target_labels(targets: list[str], aliases: dict[str, set[str]]) -> set[str]:
    labels: set[str] = set()
    for key in targets:
        labels.update(aliases.get(key) or {key})
        labels.add(key)
    return {x for x in labels if x}


def _intent_targets(index: IndexBundle, query: str, intent: dict[str, Any]) -> list[str]:
    aliases = _alias_table_for(index)
    if intent.get("targets") is not None:
        return _clean_planner_targets(list(intent["targets"]))
    ignore = set(DEFAULT_ALIAS_IGNORE) | {
        str(x).strip() for x in (index.cfg.get("person_alias_ignore") or []) if str(x).strip()
    }
    return _query_targets(query, aliases, ignore)


def _lexical_query_expanded(
    index: IndexBundle, query: str, intent: dict[str, Any]
) -> tuple[str, list[str]]:
    if not bool(index.cfg.get("alias_expand_lexical", True)):
        return query, []
    aliases = _alias_table_for(index)
    targets = _intent_targets(index, query, intent)
    labels = _target_labels(targets, aliases)
    extra = [lab for lab in sorted(labels, key=len, reverse=True) if lab and lab not in query]
    if not extra:
        return query, []
    return query + " " + " ".join(extra), extra


@dataclass
class _SoftEventRuntime:
    """Transient searchable view over stable leaves; never an evidence store."""

    recipe: SoftEventRecipe
    result: Any
    lexical: ThreadSafeLexicalIndex
    event_ids: list[str]
    event_vectors: np.ndarray
    event_vector_ids: list[str]
    search_texts: dict[str, str] = field(default_factory=dict)


def _rerank_available() -> bool:
    """R2를 부를 수 있는가 — OpenRouter 키가 있어야 한다. 예산은 decide_ranking이 본다."""

    try:
        secrets = load_secrets(Path(os.environ["EVAL_SECRETS"]))
    except (OSError, KeyError, ValueError):
        return False
    return bool(str(secrets.get("OPENROUTER_API_KEY") or "").strip())


def fit_query_vector(index: Any, q_vec: np.ndarray) -> np.ndarray:
    """Match a query vector to the stored dimensionality (after `apply_vector_dims`)."""

    vecs = getattr(index, "leaf_vectors", None)
    dims = int(vecs.shape[1]) if vecs is not None and getattr(vecs, "ndim", 0) == 2 else 0
    if dims and q_vec.shape[0] > dims:
        q_vec = np.ascontiguousarray(q_vec[:dims], dtype=np.float32)
        q_vec /= np.linalg.norm(q_vec) + 1e-9
    return q_vec


def _embed_instruction(cfg: Mapping[str, Any]) -> str:
    return str(cfg.get("embed_query_instruction") or DEFAULT_EMBED_QUERY_INSTRUCTION)


def _late_settle(client: Any) -> None:
    """헤지에서 진 요청이 턴 장부 반영 뒤에 끝나면 그 비용을 장부에 더한다."""

    hook = getattr(client, "late_settle", None)
    if callable(hook):
        try:
            hook()
        except Exception:  # noqa: BLE001 — 장부 보강 실패가 응답을 막지 않는다
            pass


def prefetch_query_vector(index: Any, client: Any, text: str) -> None:
    """결과를 기다리지 않고 질의 임베딩을 시작한다(플래너와 겹치기). 설정에서 끄면 아무것도 안 한다."""

    embedder = getattr(index, "query_embedder", None)
    if embedder is None or not bool(query_embed_config(index.cfg).get("prefetch")):
        return
    embedder.prefetch(client, text, instruction=_embed_instruction(index.cfg), on_late=lambda: _late_settle(client))


def query_vector(index: Any, client: Any, text: str) -> tuple[np.ndarray, dict[str, Any]]:
    """정규화·차원 맞춤된 질의 벡터와 출처(hit/joined/miss/direct, hedged)."""

    instruction = _embed_instruction(index.cfg)
    embedder = getattr(index, "query_embedder", None)
    if embedder is None:
        vec = np.asarray(client.embed([text], kind="query", instruction=instruction)[0], dtype=np.float32)
        vec /= np.linalg.norm(vec) + 1e-9
        info: dict[str, Any] = {"source": "direct", "hedged": False}
    else:
        vec, info = embedder.get(client, text, instruction=instruction, on_late=lambda: _late_settle(client))
        vec = vec.copy()
    return fit_query_vector(index, vec), info


def _q1_text(query: str, intent: Mapping[str, Any] | None, mq: Mapping[str, Any]) -> str:
    """multi-query q1(dense) 질의: 플래너 semantic_query, 없으면 memory.search.query. 원문과 같으면 빈 문자열."""

    if not intent or not bool(mq.get("enabled", True)) or int(mq.get("max_queries") or 3) <= 1:
        return ""
    sem_q = str(intent.get("semantic_query") or "").strip()
    rewrite_q = str(intent.get("memory_search_query") or "").strip()
    if sem_q and _canon_query(sem_q) != _canon_query(query):
        return sem_q
    if rewrite_q and _canon_query(rewrite_q) != _canon_query(query):
        return rewrite_q
    return ""


def _hit_provenance(index: IndexBundle, doc: dict[str, Any]) -> dict[str, Any]:
    room = str(doc.get("source_room_id") or "")
    bid = str((index.block_id_by_room or {}).get(room) or "")
    block = (index.block_by_id or {}).get(bid) or {}
    return {
        "block_id": bid,
        "platform": str(block.get("platform") or platform_of_room(room)),
        "source_room_id": room,
        "label": str(block.get("label") or _label_for_room(room)),
    }


# 테스트가 이 모듈 상수를 잠시 덮어쓴다(test_engine_retrieval). retrieval_post의
# 기본값(200/150)과 다른 풀 한도다.
LEXICAL_LIMIT = 80
DENSE_LIMIT = 50


def _dense_hits(
    vectors: np.ndarray, ids: list[str], q_vec: np.ndarray, *, allowed_ids: set[str] | None, limit: int
) -> list[tuple[str, float]]:
    if allowed_ids is not None:
        idx = [i for i, doc_id in enumerate(ids) if doc_id in allowed_ids]
        if not idx:
            return []
        sims = vectors[idx] @ q_vec
        order = np.argsort(-sims, kind="stable")[:limit]
        return [(ids[idx[i]], float(sims[i])) for i in order]
    sims = vectors @ q_vec
    order = np.argsort(-sims, kind="stable")[:limit]
    return [(ids[i], float(sims[i])) for i in order]


def _lexical_hits(
    fts: ThreadSafeLexicalIndex, query: str, *, ids_n: int, allowed_ids: set[str] | None, limit: int, lock: threading.Lock
) -> list[tuple[str, float]]:
    with lock:
        raw = fts.search(query, limit=limit if allowed_ids is None else max(limit, ids_n))
    return [(d, float(sc)) for d, sc in raw if allowed_ids is None or d in allowed_ids][:limit]


def _hybrid_fusion(
    *,
    fts: ThreadSafeLexicalIndex,
    vectors: np.ndarray,
    ids: list[str],
    query: str,
    q_vec: np.ndarray,
    weights: list[float],
    lock: threading.Lock,
    trace: dict[str, Any] | None = None,
    trace_key: str = "leaf",
    allowed_ids: set[str] | None = None,
    lexical_limit: int | None = None,
    dense_limit: int | None = None,
    lexical_query: str | None = None,
) -> FusionResult:
    """One lexical run + one dense run over a doc set, fused with weighted RRF.

    Returns candidates with provenance (`{trace_key}_lexical` / `{trace_key}_dense` ranks and raw
    scores). `_hybrid_rank` is the id-only wrapper kept for callers/tests that predate candidates.
    """

    # 완전 동점은 입력 행 순서로 깬다(Go denseTop과 동일). 기본 quicksort는
    # 같은 코사인의 행 순서를 바꿔 BLAS 오차와 별개의 parity 차이를 만든다.

    # 방 필터가 있으면 FTS LIMIT을 필터 *뒤*에 적용한다. 전역 top-80을 먼저 자르면
    # 다른 방의 매칭이 많을 때 이 방의 어휘 후보가 0이 된다.
    lex_lim = int(lexical_limit if lexical_limit is not None else LEXICAL_LIMIT)
    den_lim = int(dense_limit if dense_limit is not None else DENSE_LIMIT)
    search_q = query if lexical_query is None else lexical_query
    lex_name, den_name = f"{trace_key}_lexical", f"{trace_key}_dense"
    with lock:
        lexical = fts.search(
            search_q, limit=lex_lim if allowed_ids is None else max(lex_lim, len(ids))
        )
    lexical = [
        (doc_id, score)
        for doc_id, score in lexical
        if allowed_ids is None or doc_id in allowed_ids
    ][:lex_lim]
    lexical_hits = [doc_id for doc_id, _ in lexical]
    if allowed_ids is not None:
        idx = [i for i, doc_id in enumerate(ids) if doc_id in allowed_ids]
        if not idx:
            return fuse([(lex_name, [], weights[0]), (den_name, [], weights[1])])
        sims = vectors[idx] @ q_vec
        dense_local = np.argsort(-sims, kind="stable")[:den_lim]
        dense_trace = [(ids[idx[i]], float(sims[i])) for i in dense_local]
    else:
        sims = vectors @ q_vec
        dense_local = np.argsort(-sims, kind="stable")[:den_lim]
        dense_trace = [(ids[i], float(sims[i])) for i in dense_local]
    dense_hits = [doc_id for doc_id, _ in dense_trace]
    fused = fuse(
        [(lex_name, lexical_hits, weights[0]), (den_name, dense_hits, weights[1])],
        raw_scores={lex_name: dict(lexical), den_name: dict(dense_trace)},
    )
    if trace is not None:
        trace[lex_name] = [
            {"doc_id": doc_id, "score": float(score)} for doc_id, score in lexical
        ]
        trace[den_name] = [
            {"doc_id": doc_id, "score": score} for doc_id, score in dense_trace
        ]
        trace[f"{trace_key}_rrf"] = fused.ids()[:lex_lim]
        if lexical_query is not None and lexical_query != query:
            trace["lexical_query"] = lexical_query
    return fused


def _hybrid_rank(**kwargs: Any) -> list[str]:
    """Id-only view of `_hybrid_fusion` (same order as the pre-candidate `weighted_rrf` path)."""

    return _hybrid_fusion(**kwargs).ids()


def _person_signal(
    index: IndexBundle,
    leaf: dict[str, Any],
    *,
    tokens: set[str],
    labels: set[str],
) -> tuple[float, dict[str, Any]]:
    """원문 화자·짧은 멘션을 붙이고, 긴 붙여넣기만의 이름 언급은 낮게 본다."""

    if not tokens:
        return 0.0, {"speaker_hit": False, "short": 0, "paste": 0}

    speakers = {str(s) for s in (leaf.get("speakers") or [])}
    speaker_hit = bool(speakers & labels) or any(
        any(_token_in_text(sp, tok, labels=labels) for tok in tokens) for sp in speakers
    )
    start = int(leaf.get("start") or 0)
    end = int(leaf.get("end") or start)
    short = 0
    paste = 0
    for i in range(start, min(end, len(index.messages))):
        msg = index.messages[i]
        content = str(msg.get("content") or "")
        actor = str(msg.get("actor_label") or "")
        hit = any(_token_in_text(content, tok, labels=labels) for tok in tokens) or any(
            _token_in_text(actor, tok, labels=labels) for tok in tokens
        )
        if not hit:
            if actor in labels:
                speaker_hit = True
            continue
        if actor in labels or any(
            _token_in_text(actor, tok, labels=labels) for tok in tokens
        ):
            speaker_hit = True
        if len(content) >= 380:
            paste += 1
        else:
            short += 1

    score = 0.0
    if speaker_hit:
        score += 2.4
    score += 1.15 * min(short, 3)
    score += 0.12 * min(paste, 2)
    if short == 0 and paste > 0 and not speaker_hit:
        score -= 1.3
    return score, {"speaker_hit": speaker_hit, "short": short, "paste": paste}


def _recency_bonus(
    index: IndexBundle,
    doc_id: str,
    *,
    half_life_days: float,
    strength: float,
) -> float:
    if strength <= 0:
        return 0.0
    ts = index.leaf_end_ts.get(doc_id)
    if ts is None:
        return -0.15 * strength
    tmax = recency_anchor_ts(index)
    age_days = max(0.0, (tmax - ts) / 86400.0)
    # 최근일수록 1에 가깝고, half_life에서 ~0.5
    freshness = math.exp(-math.log(2) * age_days / max(half_life_days, 1.0))
    return strength * (freshness - 0.35)


def _rerank_leaves(
    index: IndexBundle,
    ranked: list[str],
    *,
    query: str,
    pool: int,
    top_k: int,
    trace: dict[str, Any] | None = None,
    allowed_ids: set[str] | None = None,
    intent: dict[str, Any] | None = None,
) -> list[str]:
    """R1 · 최근·인물 가산. `allowed_ids`(방 필터)는 주입 후보에도 강제한다.

    `intent`는 플래너 LLM 출력(time_scope/question_kind/person_targets). 없으면 regex fallback."""

    intent = resolve_intent(query, intent, index.cfg)
    aliases = _alias_table(index.cfg)
    if intent.get("targets") is not None:
        # 플래너가 사람 이름을 골라 줬다. 별칭 표는 그 이름을 라벨로 넓히는 데만 쓴다.
        targets = _clean_planner_targets(list(intent["targets"]))
    else:
        ignore = set(DEFAULT_ALIAS_IGNORE) | {
            str(x).strip() for x in (index.cfg.get("person_alias_ignore") or []) if str(x).strip()
        }
        targets = _query_targets(query, aliases, ignore)
    labels = _target_labels(targets, aliases)
    identity_re = _identity_re(index.cfg)
    tokens = set(targets) | {x for x in labels if len(x) <= 12}
    tokens = {t for t in tokens if 2 <= len(t) <= 12}
    if trace is not None:
        trace["intent"] = intent

    half_life = float(index.cfg.get("recency_half_life_days", 40))
    # 점수 스케일: base(hybrid 순위)는 [0, 1]. 가산항은 그 스케일에 상대적으로 정한다.
    # 일반 질의에서 recency는 동점 정리 수준(≈±0.03)이어야 한다 — 이전 0.35는 hybrid
    # 순위 전체 범위(≈0.05)의 7배라 R1이 "최신순 정렬"로 붕괴했다 (07 실험 로그 R1 악화).
    if intent["recent"]:
        recent_strength = 1.0
    elif intent["intro"]:
        recent_strength = 0.0
    else:
        recent_strength = 0.05
    person_strength = 1.0 if targets else 0.0
    intro_strength = 0.55 if intent["intro"] else 0.0
    base_mult = 1.5 if intent["intro"] else 1.0
    effective_pool = pool
    if intent["recent"]:
        effective_pool = max(pool, 100)
    if intent["intro"]:
        effective_pool = max(pool, 80)

    # 최근 질문: 하이브리드 top에 안 잡힌 최신·인물 leaf를 후보에 섞는다.
    candidate_ids = list(ranked)
    if intent["recent"]:
        extras: list[str] = []
        for doc_id in index.leaves_by_recency:
            if allowed_ids is not None and doc_id not in allowed_ids:
                continue
            leaf = index.leaf_by_id.get(doc_id)
            if leaf is None:
                continue
            if targets:
                person, _meta = _person_signal(
                    index, leaf, tokens=tokens, labels=labels
                )
                if person < 0.8:
                    continue
            extras.append(doc_id)
            if len(extras) >= effective_pool:
                break
        candidate_ids = list(dict.fromkeys(extras + ranked))
    elif intent["intro"] and targets:
        # 소개: config identity_terms(소속·전공 등 정체성 단서)가 있는 인물 leaf를 후보에 섞는다.
        extras_scored: list[tuple[float, str]] = []
        for doc_id, sdoc in index.summary_by_id.items():
            if allowed_ids is not None and doc_id not in allowed_ids:
                continue
            summary = str(sdoc.get("summary") or "")
            leaf = index.leaf_by_id.get(doc_id)
            if leaf is None:
                continue
            blob = f"{leaf.get('body') or ''}\n{summary}"
            if identity_re is None or not identity_re.search(blob):
                continue
            person, _meta = _person_signal(index, leaf, tokens=tokens, labels=labels)
            if person < 0.8:
                continue
            strong = 1.5 if identity_re and identity_re.search(blob) else 0.0
            extras_scored.append((person + strong, doc_id))
        extras_scored.sort(key=lambda item: -item[0])
        extras = [doc_id for _, doc_id in extras_scored[:effective_pool]]
        candidate_ids = list(dict.fromkeys(extras + ranked))

    rrf_rank = {doc_id: i for i, doc_id in enumerate(ranked)}
    if intent["recent"] or intent["intro"]:
        injected = [doc_id for doc_id in candidate_ids if doc_id not in rrf_rank][
            :effective_pool
        ]
        consider = list(dict.fromkeys(injected + ranked[:effective_pool]))
    else:
        consider = ranked[: max(effective_pool, top_k)]

    scored: list[tuple[float, int, str]] = []
    n_consider = max(1, len(consider))
    for cand_i, doc_id in enumerate(consider):
        leaf = index.leaf_by_id.get(doc_id)
        if leaf is None:
            continue
        if allowed_ids is not None and doc_id not in allowed_ids:
            continue
        # rrf_rank에 없는 후보는 가져온 목록 바로 뒤의 순위로 둔다.
        unranked_rank = LEXICAL_LIMIT + cand_i
        rank = rrf_rank.get(doc_id, unranked_rank)
        # hybrid 1위 → 1.0, 풀 마지막 → 0에 가깝게. 주입된 후보(hybrid에 없음)는 0.
        base = max(0.0, 1.0 - rank / n_consider)
        person, meta = _person_signal(index, leaf, tokens=tokens, labels=labels)
        if intent["intro"] and targets and person < 0.8:
            person -= 0.8
        if (intent["politics"] or intent["prefer_missing_over_guess"]) and targets:
            if meta["short"] == 0 and not meta["speaker_hit"]:
                person -= 2.0
        recency = _recency_bonus(
            index, doc_id, half_life_days=half_life, strength=recent_strength
        )
        paste_penalty = 0.0
        if intent["politics"] or intent["prefer_missing_over_guess"]:
            body = str(leaf.get("body") or "")
            if len(body) > 1200:
                paste_penalty -= 0.6
        identity_bonus = 0.0
        if intent["intro"]:
            summary = ""
            if doc_id in index.summary_by_id:
                summary = str(index.summary_by_id[doc_id].get("summary") or "")
            blob = f"{leaf.get('body') or ''}\n{summary}"
            if identity_re is None:
                identity_bonus = 0.0
            elif identity_re.search(blob):
                identity_bonus = 2.4 / 6.0
            elif targets:
                identity_bonus = -1.5 / 6.0
        person_n = scaled_person(person)
        score = (
            base * base_mult
            + person_strength * person_n
            + recency
            + intro_strength * max(person_n, 0.0)
            + paste_penalty
            + identity_bonus
        )
        scored.append((score, rank, doc_id))

    scored.sort(key=lambda item: (-item[0], item[1]))
    leaf_size = int(index.cfg.get("leaf_size", 8))
    min_gap = leaf_size * 3 if intent["recent"] else max(4, leaf_size // 2)
    picked: list[str] = []
    picked_starts: list[int] = []
    adjacent_skipped: list[str] = []
    for score, rank, doc_id in scored:
        leaf = index.leaf_by_id.get(doc_id) or {}
        start = int(leaf["start"]) if leaf.get("start") is not None else -10_000
        if any(abs(start - s) < min_gap for s in picked_starts):
            adjacent_skipped.append(doc_id)
            continue
        picked.append(doc_id)
        picked_starts.append(start)
        if len(picked) >= top_k:
            break
    if len(picked) < top_k:
        for _, _, doc_id in scored:
            if doc_id not in picked:
                picked.append(doc_id)
            if len(picked) >= top_k:
                break
    if trace is not None:
        trace["heuristic"] = {
            "effective_pool": effective_pool,
            "consider_n": len(consider),
            "candidate_n": len(candidate_ids),
            "targets": sorted(targets)[:24],
            "min_gap": min_gap,
            "scored": [
                {"doc_id": doc_id, "score": round(float(score), 6), "rrf_rank": rank}
                for score, rank, doc_id in scored[:80]
            ],
            "picked": picked,
            "adjacent_skipped": adjacent_skipped[:40],
        }
    return picked


RERANK_DOC_FORMATS = ("body", "summary_body", "speaker_body", "time_body", "full")


def _rerank_document(index: IndexBundle, doc_id: str, doc_format: str, *, limit: int = 2000) -> str:
    """Text handed to the reranker for one leaf (§24 representation experiment).

    body          raw leaf text only
    summary_body  summary + body (2026-09-17 default; the ablation harness used body only)
    speaker_body  [Speaker] labels + body
    time_body     [Time] end date + body
    full          speaker + time + summary + body"""

    leaf = index.leaf_by_id.get(doc_id) or {}
    body = str(leaf.get("body") or "")
    summary = str((index.summary_by_id.get(doc_id) or {}).get("summary") or "")
    speakers = ", ".join(str(x) for x in (leaf.get("speakers") or [])[:8])
    end_ts = index.leaf_end_ts.get(doc_id)
    when = datetime.fromtimestamp(end_ts).strftime("%Y-%m-%d %H:%M") if end_ts else ""
    parts: list[str] = []
    if doc_format in {"speaker_body", "full"} and speakers:
        parts.append(f"[Speaker]\n{speakers}")
    if doc_format in {"time_body", "full"} and when:
        parts.append(f"[Time]\n{when}")
    if doc_format in {"summary_body", "full"} and summary:
        parts.append(summary if doc_format == "summary_body" else f"[Summary]\n{summary}")
    if doc_format == "full":
        parts.append(f"[Content]\n{body}")
    else:
        parts.append(body)
    blob = "\n".join(p for p in parts if p).strip()
    return (blob or body)[:limit]


def _pre_rerank_dedup(index: IndexBundle, pool_ids: list[str]) -> tuple[list[str], list[str]]:
    """Keep the best-ranked leaf of each adjacent run (same room, |start diff| < leaf_size)."""

    leaf_size = int(index.cfg.get("leaf_size", 8) or 8)
    kept: list[str] = []
    kept_pos: list[tuple[str, int]] = []
    dropped: list[str] = []
    for doc_id in pool_ids:
        leaf = index.leaf_by_id.get(doc_id) or {}
        room = str(leaf.get("source_room_id") or "")
        start = int(leaf["start"]) if leaf.get("start") is not None else -10_000
        if any(r == room and abs(start - s0) < leaf_size for r, s0 in kept_pos):
            dropped.append(doc_id)
            continue
        kept.append(doc_id)
        kept_pos.append((room, start))
    return kept, dropped


def _rank_leaf_ids(
    index: IndexBundle,
    ranked: list[str],
    *,
    query: str,
    pool: int,
    top_k: int,
    ranking: str,
    remaining_budget: float,
    trace: dict[str, Any] | None,
    allowed_ids: set[str] | None = None,
    intent: dict[str, Any] | None = None,
    auto: bool = False,
) -> list[str]:
    """Apply one ranking variant. Unknown modes are rejected before this call.

    R2 실패 시 R0(hybrid) 풀 순서를 그대로 쓴다. `auto=True`면 reason만 계약 이름(`rerank_failed`)으로
    남긴다. R1(`heuristic`)은 명시 요청 때만 돌며 auto의 fallback이 아니다 (2026-09-17 퇴역)."""

    if ranking == "hybrid":
        if trace is not None:
            trace["applied"] = "hybrid"
        return ranked[:top_k]
    if ranking == "rerank":
        rr_cfg = retrieval_config(index.cfg)["rerank"]
        pool_ids = ranked[:pool]
        if bool(rr_cfg.get("pre_dedup", False)):
            # §26 cheap adjacency dedup before the reranker: neighbouring leaves of one conversation
            # stretch would otherwise occupy several reranker slots. Final span merge still runs.
            pool_ids, pre_dropped = _pre_rerank_dedup(index, pool_ids)
            if trace is not None:
                trace["rerank_pre_dedup_dropped"] = len(pre_dropped)
        doc_format = str(rr_cfg.get("doc_format") or "summary_body")
        docs: list[str] = [_rerank_document(index, doc_id, doc_format) for doc_id in pool_ids]
        try:
            secrets = load_secrets()
            rerank_budget = max(0.02, min(0.08, max(remaining_budget, 0.02) * 0.4))
            requested_model = configured_models(index.cfg)["rerank"]
            rr_client = RerankClient(
                secrets,
                model=requested_model,
                budget_usd=rerank_budget,
                max_request_cost_usd=min(0.05, rerank_budget),
                base_url=secrets.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
                ledger=index.rerank_budget_ledger,
            )
            result = rr_client.rerank(
                query,
                docs,
                top_n=min(len(docs), max(top_k, min(pool, len(docs)))),
            )
            ordered = [pool_ids[i] for i in result.indices if 0 <= i < len(pool_ids)]
            extra = [doc_id for doc_id in pool_ids if doc_id not in ordered]
            # relevance floor: 잡담·오프도메인 질의에 top_k를 억지로 채우지 않는다.
            min_score = resolve_rerank_min_score(index.cfg, result.model or requested_model)
            kept: list[str] = []
            seen: set[str] = set()
            for idx, score in zip(result.indices, result.scores):
                if not (0 <= idx < len(pool_ids)):
                    continue
                if float(score) < min_score:
                    continue
                doc_id = pool_ids[idx]
                if doc_id in seen:
                    continue
                seen.add(doc_id)
                kept.append(doc_id)
            abstained = bool(result.indices) and not kept
            if trace is not None:
                trace["applied"] = "rerank"
                trace["rerank_doc_format"] = doc_format
                trace["rerank_min_score"] = min_score
                trace["rerank_kept_n"] = len(kept)
                trace["abstained"] = abstained
                trace["rerank_model"] = result.model
                trace["rerank_model_requested"] = requested_model
                trace["rerank_cost_usd"] = result.cost_usd
                trace["rerank_latency_ms"] = result.latency_ms
                trace["pool_n"] = len(pool_ids)
                trace["pool_ids"] = pool_ids
                trace["rerank_indices"] = list(result.indices)
                trace["rerank_scores"] = [round(float(s), 6) for s in result.scores]
                trace["rerank_order"] = ordered
                trace["doc_chars"] = [len(d) for d in docs]
                trace["rerank_docs"] = [
                    {"doc_id": pool_ids[i], "chars": len(docs[i]), "preview": docs[i][:400]}
                    for i in range(len(pool_ids))
                ]
                if result.billed:
                    trace["rerank_billed"] = result.billed
                if result.generation_id:
                    try:
                        gen = rr_client.fetch_generation(result.generation_id, retries=2)
                    except Exception:  # noqa: BLE001
                        gen = None
                    if gen:
                        trace["rerank_generation"] = gen
            return kept[:top_k]
        except (RerankError, OSError, TimeoutError, RuntimeError) as exc:
            fb_mode, fb_reason = ("hybrid", "rerank_failed")
            if auto:
                fb_mode, fb_reason = fallback_ranking(intent)
            if trace is not None:
                trace["applied"] = f"{fb_mode}_fallback"
                trace["rerank_error"] = type(exc).__name__
                # 오류 본문에 문서·질의가 섞일 수 있으므로 종류만 남긴다 (privacy).
                trace["rerank_model_requested"] = configured_models(index.cfg)["rerank"]
                trace["pool_n"] = len(pool_ids)
                trace["pool_ids"] = pool_ids
                trace["ranking_auto_reason"] = fb_reason
            # R0 order, not the guarantee-reordered pool prefix.
            fusion_ids = list((trace or {}).get("fusion_ids") or pool_ids)
            return fusion_ids[:top_k]
    if trace is not None:
        trace["applied"] = "heuristic"
    return _rerank_leaves(
        index,
        ranked,
        query=query,
        pool=pool,
        top_k=top_k,
        trace=trace,
        allowed_ids=allowed_ids,
        intent=intent,
    )


def _apply_hybrid_floor(
    index: IndexBundle,
    ranked: list[str],
    *,
    q_vec: np.ndarray,
    rank_mode: str,
) -> tuple[list[str], dict[str, Any]]:
    if rank_mode not in {"hybrid", "heuristic"}:
        return ranked, {}
    try:
        min_cos = float(index.cfg.get("hybrid_min_cos") or 0.0)
    except (TypeError, ValueError):
        min_cos = 0.0
    if min_cos <= 0:
        return ranked, {}
    id_to_idx = {doc_id: i for i, doc_id in enumerate(index.leaf_ids)}
    kept: list[str] = []
    for doc_id in ranked:
        i = id_to_idx.get(doc_id)
        if i is None:
            continue
        if float(index.leaf_vectors[i] @ q_vec) >= min_cos:
            kept.append(doc_id)
    abstained = bool(ranked) and not kept
    return kept, {"hybrid_min_cos": min_cos, "abstained": abstained, "hybrid_kept_n": len(kept)}


def _scope_allowed_ids(
    index: IndexBundle,
    *,
    mode: str,
    source_room_id: str | None,
    block_ids: list[str] | None,
) -> set[str] | None:
    """Channel / block scope. Applied before any retrieval — other rooms are never candidates."""

    allowed: set[str] | None = None
    if block_ids is not None:
        allowed = leaf_ids_for_blocks(index.blocks, block_ids)
        if source_room_id:
            room_ids = {
                str(d["doc_id"])
                for d in (index.baseline if mode == "baseline" else index.leaves)
                if d.get("source_room_id") == source_room_id
            }
            allowed &= room_ids
    elif source_room_id:
        docs = index.baseline if mode == "baseline" else index.leaves
        allowed = {str(d["doc_id"]) for d in docs if d.get("source_room_id") == source_room_id}
    return allowed


def _canon_query(text: str) -> str:
    """Collapse whitespace so duplicate planner rewrites of the same user query are dropped."""

    return " ".join((text or "").split())


def _soft_event_recipe(cfg: Mapping[str, Any]) -> tuple[dict[str, Any], SoftEventRecipe] | None:
    """Return the opt-in runtime recipe without changing the stable leaf recipe."""

    raw = retrieval_config(cfg).get("soft_event") or {}
    if not bool(raw.get("enabled", False)):
        return None
    pooling = str(raw.get("pooling") or "leaf_mean")
    if pooling != "leaf_mean":
        raise PlaygroundError("soft_event.pooling must be 'leaf_mean'")
    partial_scope = str(raw.get("partial_scope") or "skip")
    # A partially scoped event would mix authorized and unauthorized leaves.
    # Until another policy is explicitly designed, fail closed rather than
    # silently returning a mixed event.
    if partial_scope != "skip":
        raise PlaygroundError("soft_event.partial_scope must be 'skip'")
    recipe = SoftEventRecipe.from_value(
        {
            "version": raw.get("version", "soft-event-v1"),
            "gap_minutes": raw.get("gap_minutes", 30),
            "min_leaves": raw.get("min_leaves", 1),
            "max_leaves": raw.get("max_leaves", 8),
            "max_chars": raw.get("max_chars", 4000),
        }
    )
    return dict(raw), recipe


def _soft_event_leaf_times(index: IndexBundle) -> dict[str, dict[str, float | None]]:
    """Build explicit leaf intervals from source messages; never infer missing times."""

    out: dict[str, dict[str, float | None]] = {}
    messages = index.messages
    for leaf in index.leaves:
        doc_id = str(leaf["doc_id"])
        try:
            start = int(leaf.get("start"))
            end = int(leaf.get("end")) - 1
        except (TypeError, ValueError):
            start, end = -1, -1
        start_value = None
        end_value = None
        if 0 <= start < len(messages):
            start_value = _parse_ts(messages[start].get("parsed_time"))
        if 0 <= end < len(messages):
            end_value = _parse_ts(messages[end].get("parsed_time"))
        out[doc_id] = {"start_time": start_value, "end_time": end_value}
    return out


def _soft_event_search_text(index: IndexBundle, event: Any, recipe: SoftEventRecipe) -> str:
    """Create event-only search text; it is never passed to evidence packing."""

    bodies = [
        str((index.leaf_by_id.get(leaf_id) or {}).get("body") or "").strip()
        for leaf_id in event.leaf_ids
    ]
    body = "\n".join(part for part in bodies if part)
    body = body[: recipe.max_chars]
    summary = str(getattr(event, "summary_text", "") or "").strip()
    if summary and len(body) < recipe.max_chars:
        room = recipe.max_chars - len(body)
        suffix = summary[: max(0, room - (1 if body else 0))]
        if suffix:
            body = f"{body}\n{suffix}" if body else suffix
    return body


def _soft_event_runtime(index: IndexBundle, *, use_summary: bool) -> _SoftEventRuntime:
    """Build/cache a runtime-derived event view for the current retrieval recipe."""

    recipe_config = _soft_event_recipe(index.cfg)
    if recipe_config is None:
        raise PlaygroundError("soft event runtime requested while disabled")
    raw, recipe = recipe_config
    summary_enabled = bool(use_summary and retrieval_config(index.cfg)["summaries"].get("enabled", True))
    cache_key = json.dumps(
        {
            "recipe": recipe.as_dict(),
            "summary_enabled": summary_enabled,
            "summary_ids": list(index.summary_ids) if summary_enabled else [],
            "pooling": str(raw.get("pooling") or "leaf_mean"),
        },
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )
    cached = index.soft_event_runtime_cache.get(cache_key)
    if isinstance(cached, _SoftEventRuntime):
        return cached

    summaries = None
    if summary_enabled:
        summaries = {
            str(doc_id): str(row.get("summary") or "")
            for doc_id, row in index.summary_by_id.items()
            if isinstance(row, Mapping) and str(row.get("summary") or "").strip()
        }
    result = build_soft_events(
        index.leaves,
        recipe=recipe,
        leaf_vectors=index.leaf_vectors,
        leaf_times=_soft_event_leaf_times(index),
        summaries=summaries,
    )
    search_texts = {
        event.event_id: _soft_event_search_text(index, event, recipe)
        for event in result.events
    }
    lexical = ThreadSafeLexicalIndex(str(index.cfg["lexical"]))
    lexical.add_many([(event_id, text) for event_id, text in search_texts.items()])
    vector_rows: list[tuple[str, tuple[float, ...]]] = [
        (event.event_id, event.vector)
        for event in result.events
        if event.vector is not None
    ]
    if vector_rows:
        vector_ids = [event_id for event_id, _ in vector_rows]
        vectors = np.asarray([vector for _, vector in vector_rows], dtype=np.float32)
    else:
        vector_ids = []
        vectors = np.zeros((0, int(index.leaf_vectors.shape[1])), dtype=np.float32)
    runtime = _SoftEventRuntime(
        recipe=recipe,
        result=result,
        lexical=lexical,
        event_ids=[event.event_id for event in result.events],
        event_vectors=vectors,
        event_vector_ids=vector_ids,
        search_texts=search_texts,
    )
    # Keep one current recipe only.  This makes a runtime config change a
    # deterministic rebuild and avoids retaining old in-memory FTS databases.
    index.soft_event_runtime_cache.clear()
    index.soft_event_runtime_cache[cache_key] = runtime
    return runtime


def _soft_event_candidates(
    index: IndexBundle,
    *,
    query: str,
    q_vec: np.ndarray,
    allowed: set[str] | None,
    use_summary: bool,
    trace: dict[str, Any],
) -> tuple[list[tuple[str, list[str], float]], dict[str, dict[str, float]]]:
    """Search events and project each hit to dense-ranked stable leaf IDs."""

    raw, _recipe = _soft_event_recipe(index.cfg) or ({}, None)
    runtime = _soft_event_runtime(index, use_summary=use_summary)
    event_by_id = runtime.result.event_by_id
    eligible: list[str] = []
    partial_skipped = 0
    for event_id in runtime.event_ids:
        members = set(event_by_id[event_id].leaf_ids)
        if allowed is None:
            eligible.append(event_id)
        elif members <= allowed:
            eligible.append(event_id)
        elif members & allowed:
            partial_skipped += 1

    event_limit = max(0, int(raw.get("event_limit", 40) or 0))
    max_leaf_ids = max(0, int(raw.get("max_leaf_ids_per_event", 8) or 0))
    eligible_set = set(eligible)
    lexical_hits: list[tuple[str, float]] = []
    if event_limit and eligible:
        with index.lock:
            lexical_hits = runtime.lexical.search(
                query,
                limit=max(event_limit, len(runtime.event_ids)),
            )
        lexical_hits = [
            (event_id, float(score))
            for event_id, score in lexical_hits
            if event_id in eligible_set
        ][:event_limit]
    dense_hits: list[tuple[str, float]] = []
    if event_limit and runtime.event_vector_ids and eligible:
        dense_hits = _dense_hits(
            runtime.event_vectors,
            runtime.event_vector_ids,
            q_vec,
            allowed_ids=eligible_set,
            limit=event_limit,
        )

    id_to_row = {leaf_id: row for row, leaf_id in enumerate(index.leaf_ids)}

    def project(hits: list[tuple[str, float]]) -> tuple[list[str], dict[str, float], dict[str, list[str]]]:
        projected: list[str] = []
        child_scores: dict[str, float] = {}
        projected_map: dict[str, list[str]] = {}
        for event_id, _event_score in hits:
            event = event_by_id.get(event_id)
            if event is None:
                continue
            # Use the soft_events public projection API for membership, then
            # impose the query-child dense order required by this adapter.
            member_ids = project_event_ranks(runtime.result, [event_id])
            children = [
                (leaf_id, float(index.leaf_vectors[id_to_row[leaf_id]] @ q_vec))
                for leaf_id in member_ids
                if allowed is None or leaf_id in allowed
            ]
            child_position = {leaf_id: pos for pos, leaf_id in enumerate(member_ids)}
            children.sort(key=lambda item: (-item[1], child_position[item[0]]))
            child_ids = [leaf_id for leaf_id, _score in children[:max_leaf_ids]]
            projected_map[event_id] = child_ids
            for leaf_id, score in children[:max_leaf_ids]:
                if leaf_id not in projected:
                    projected.append(leaf_id)
                    child_scores[leaf_id] = score
        return projected, child_scores, projected_map

    lexical_projected, lexical_raw, lexical_map = project(lexical_hits)
    dense_projected, dense_raw, dense_map = project(dense_hits)
    trace["soft_event"] = {
        "recipe": runtime.recipe.as_dict(),
        "count": {
            "events": len(runtime.result.events),
            "eligible": len(eligible),
        },
        "hits": {
            "lexical": [event_id for event_id, _score in lexical_hits],
            "dense": [event_id for event_id, _score in dense_hits],
        },
        "projected_map": {
            "lexical": lexical_map,
            "dense": dense_map,
        },
        "partial_scope_skipped": partial_skipped,
    }
    weight = float(raw.get("rrf_weight", 0.35) or 0.0)
    runs: list[tuple[str, list[str], float]] = []
    raw_scores: dict[str, dict[str, float]] = {}
    if lexical_projected and weight > 0:
        runs.append(("soft_event_lexical", lexical_projected, weight))
        raw_scores["soft_event_lexical"] = lexical_raw
    if dense_projected and weight > 0:
        runs.append(("soft_event_dense", dense_projected, weight))
        raw_scores["soft_event_dense"] = dense_raw
    return runs, raw_scores


def _generate_candidates(
    index: IndexBundle,
    client: PlayClient,
    query: str,
    *,
    q_vec: np.ndarray,
    intent: dict[str, Any],
    scope: set[str] | None,
    use_summary: bool,
    pool: int,
    lex_lim: int,
    den_lim: int,
    time_scale: float = 1.0,
    lexical_query: str | None = None,
    time_reference: RetrievalTimeReference | None = None,
    limit_scale: float = 1.0,
) -> tuple[FusionResult, set[str] | None, dict[str, Any], dict[str, Any]]:
    """R0 v2 candidate generation: scope → independent retrievers → fusion with provenance.

    Returns (fusion, allowed_ids_after_time_scope, trace, meta) where meta carries what auto
    needs (targets requested/resolved). Every retriever honours `scope`; person/alias/multi-
    query failures are isolated so lexical+dense always remain."""

    cfg = index.cfg
    rc = retrieval_config(cfg)
    weights = [float(w) for w in cfg["rrf_weights"]]
    trace: dict[str, Any] = {}
    # 시점 제약은 scope다: 창 밖 leaf는 후보가 되지 않는다 (가산 아님). v1 recent / v2 time 모두.
    allowed, time_meta = narrow_time_ids(
        index, scope, intent, scale=time_scale, reference=time_reference
    )
    trace.update(time_meta)
    if lexical_query is None:
        lexical_query, alias_extra = _lexical_query_expanded(index, query, intent)
        if alias_extra:
            trace["lexical_query_expanded"] = True
            trace["alias_labels_added"] = alias_extra[:24]

    leaf_fusion = _hybrid_fusion(
        fts=index.leaf_fts, vectors=index.leaf_vectors, ids=index.leaf_ids, query=query, q_vec=q_vec,
        weights=weights, lock=index.lock, trace=trace, trace_key="leaf", allowed_ids=allowed,
        lexical_limit=lex_lim, dense_limit=den_lim, lexical_query=lexical_query,
    )
    outer_runs: list[tuple[str, list[str], float]] = [("leaf", leaf_fusion.ids(), 1.0)]
    children: dict[str, FusionResult] = {"leaf": leaf_fusion}
    raw_scores: dict[str, dict[str, float]] = {}
    retrievers_used = ["leaf_lexical", "leaf_dense"]
    if (
        use_summary
        and bool(rc["summaries"].get("enabled", True))
        and index.summary_fts is not None
        and index.summary_vectors is not None
        and index.summary_ids
    ):
        summary_fusion = _hybrid_fusion(
            fts=index.summary_fts, vectors=index.summary_vectors, ids=index.summary_ids, query=query, q_vec=q_vec,
            weights=weights, lock=index.lock, trace=trace, trace_key="summary", allowed_ids=allowed,
            lexical_limit=lex_lim, dense_limit=den_lim, lexical_query=lexical_query,
        )
        summary_w = float(cfg.get("summary_rrf_weight", 1.0))
        # 중첩 RRF: (leaf lexical⊕dense) ⊕ (summary lexical⊕dense). 순서는 예전 weighted_rrf와 같고
        # provenance만 retriever 단위로 남는다.
        outer_runs.append(("summary", summary_fusion.ids(), summary_w))
        children["summary"] = summary_fusion
        retrievers_used += ["summary_lexical", "summary_dense"]
        trace["summary_rrf_weight"] = summary_w

    # Soft events are a derived candidate source only.  They search an
    # in-memory event view, then project back to stable leaf IDs before fusion;
    # final ranking, expansion, and evidence packing remain leaf/raw based.
    soft_event_configured = _soft_event_recipe(cfg) is not None
    if soft_event_configured:
        event_runs, event_raw = _soft_event_candidates(
            index,
            query=query,
            q_vec=q_vec,
            allowed=allowed,
            use_summary=use_summary,
            trace=trace,
        )
        if event_runs:
            outer_runs.extend(event_runs)
            raw_scores.update(event_raw)
            retrievers_used.extend(name for name, _ids, _weight in event_runs)

    # person / alias는 가산이 아니라 후보원이다 (R1 person bonus 대체). 실패해도 R0는 남는다.
    speaker_universe_ids: set[str] = set()
    alias_ids: set[str] = set()
    targets = _intent_targets(index, query, intent)
    targets_resolved = False
    if targets and (bool(rc["person"].get("enabled", True)) or bool(rc["alias"].get("enabled", True))):
        try:
            # Resolve only among speakers the caller may see (defect: other rooms' namesakes used to
            # count toward ambiguity). `resolve_in_scope: false` restores the legacy global lookup.
            person_scope = allowed if bool(rc["person"].get("resolve_in_scope", True)) else None
            person_identity = _identity_for(index) if _person_identity_mode(cfg) != "config" else None
            person_res = resolve_persons(
                index, targets, _alias_table_for(index), token_in_text=_token_in_text,
                max_speakers_per_target=int(rc["person"].get("max_speakers_per_target") or 3),
                allowed=person_scope, identity=person_identity,
            )
            trace["persons"] = person_res.to_trace()
            targets_resolved = person_res.resolved
            if bool(rc["person"].get("enabled", True)) and person_res.resolved:
                sp_fusion, speaker_universe_ids = speaker_fusion(
                    index, person_res, query=query, q_vec=q_vec, allowed=allowed,
                    limit=int(rc["person"].get("limit") or 60), weights=weights, lock=index.lock,
                    identity=person_identity,
                )
                if sp_fusion.ordered:
                    outer_runs.append(("speaker", sp_fusion.ids(), float(rc["person"].get("rrf_weight") or 1.0)))
                    children["speaker"] = sp_fusion
                    retrievers_used += ["speaker_lexical", "speaker_dense"]
                trace["speaker_pool_n"] = len(speaker_universe_ids)
            if bool(rc["alias"].get("enabled", True)) and person_res.labels:
                al = alias_run(index, person_res, allowed=allowed,
                               limit=int(rc["alias"].get("limit") or 60), lock=index.lock)
                if al:
                    outer_runs.append(("alias_lexical", [d for d, _ in al], float(rc["alias"].get("rrf_weight") or 0.5)))
                    raw_scores["alias_lexical"] = dict(al)
                    alias_ids = {d for d, _ in al}
                    retrievers_used.append("alias_lexical")
        except Exception as exc:  # noqa: BLE001 — 실패 격리: lexical+dense R0는 남는다
            trace["person_retriever_error"] = type(exc).__name__

    # multi-query (§13): q0는 항상 원문. 플래너 semantic_query 또는 (없을 때) memory.search.query를
    # q1 dense로, key_terms를 q2 lexical로. 원문을 rewrite로 바꾸지 않는다. LLM 추가 호출 없음.
    mq = rc["multi_query"]
    queries_used: dict[str, str] = {"q0": query}
    if bool(mq.get("enabled", True)) and intent and int(mq.get("max_queries") or 3) > 1:
        try:
            sem_q = _q1_text(query, intent, mq)
            if sem_q and sem_q != query and len(queries_used) < int(mq.get("max_queries") or 3):
                t_q1 = time.perf_counter()
                q1_vec, q1_info = query_vector(index, client, sem_q)
                trace["q1_embed_ms"] = _elapsed_ms(t_q1)
                trace["q1_embed_source"] = q1_info["source"]
                q1_runs: list[tuple[str, list[str], float]] = []
                q1_raw: dict[str, dict[str, float]] = {}
                leaf_d = _dense_hits(index.leaf_vectors, index.leaf_ids, q1_vec, allowed_ids=allowed, limit=den_lim)
                q1_runs.append(("q1_dense", [d for d, _ in leaf_d], 1.0))
                q1_raw["q1_dense"] = dict(leaf_d)
                if "summary" in children and index.summary_vectors is not None:
                    sum_d = _dense_hits(index.summary_vectors, index.summary_ids, q1_vec, allowed_ids=allowed, limit=den_lim)
                    q1_runs.append(("q1_summary_dense", [d for d, _ in sum_d], float(cfg.get("summary_rrf_weight", 1.0))))
                    q1_raw["q1_summary_dense"] = dict(sum_d)
                q1_fusion = fuse(q1_runs, raw_scores=q1_raw)
                if q1_fusion.ordered:
                    outer_runs.append(("q1", q1_fusion.ids(), float(mq.get("semantic_rrf_weight") or 1.0)))
                    children["q1"] = q1_fusion
                    retrievers_used += [name for name, _, _ in q1_runs]
                queries_used["q1"] = sem_q
            terms = [str(t).strip() for t in (intent.get("key_terms") or []) if str(t).strip()]
            if terms and len(queries_used) < int(mq.get("max_queries") or 3):
                q2 = " ".join(terms[:12])
                if q2 != query:
                    lex2 = _lexical_hits(index.leaf_fts, q2, ids_n=len(index.leaf_ids), allowed_ids=allowed, limit=lex_lim, lock=index.lock)
                    if lex2:
                        outer_runs.append(("q2_lexical", [d for d, _ in lex2], float(mq.get("key_terms_rrf_weight") or 0.5)))
                        raw_scores["q2_lexical"] = dict(lex2)
                        retrievers_used.append("q2_lexical")
                    queries_used["q2"] = q2
        except Exception as exc:  # noqa: BLE001 — 실패 격리
            trace["multi_query_error"] = type(exc).__name__
    trace["queries_used"] = queries_used

    if len(outer_runs) == 1:
        fusion = leaf_fusion
    else:
        fusion = fuse(outer_runs, children=children, raw_scores=raw_scores)
        trace["merged_rrf"] = fusion.ids()[:80]
    # Single messages find the leaf that holds a one-line fact an averaged leaf vector buries. The run
    # joins the finished fusion as one more weighted list, so every existing candidate keeps the place
    # it came with. Disabled (default) or without artifacts: none of this runs and nothing is traced.
    msg_dense = rc["message_dense"]
    message_on = bool(msg_dense.get("enabled", False)) and index.message_vectors is not None
    msg_raw: dict[str, dict[str, float]] = {}
    msg_ids: list[str] = []
    if message_on:
        t_msg = time.perf_counter()
        msg_ids, msg_scores, msg_scored = message_dense_run(
            index.message_vectors, index.message_leaf, index.leaf_ids, q_vec,
            allowed_ids=allowed, limit=int(int(msg_dense.get("limit") or 200) * limit_scale),
        )
        trace["message_dense"] = {
            "messages_scored": msg_scored, "leaves": len(msg_ids),
            "ms": _elapsed_ms(t_msg), "dims": int(index.message_vectors.shape[1]),
        }
        if msg_ids:
            msg_raw = {MESSAGE_RUN_NAME: msg_scores}
            msg_weight = float(msg_dense.get("rrf_weight") or MESSAGE_RRF_WEIGHT)
            fusion = fuse(
                [("fused", fusion.ids(), 1.0), (MESSAGE_RUN_NAME, msg_ids, msg_weight)],
                children={"fused": fusion}, raw_scores={**raw_scores, **msg_raw},
            )
            retrievers_used.append(MESSAGE_RUN_NAME)
    for cand in fusion.ordered:
        if cand.doc_id in speaker_universe_ids:
            cand.flags["speaker_match"] = True
        if cand.doc_id in alias_ids:
            cand.flags["alias_match"] = True
    trace["retrievers_used"] = retrievers_used
    trace.update(fusion.trace(limit=pool))
    trace["candidate_limits"] = {"lexical": lex_lim, "dense": den_lim, "pool": pool, "time_scale": time_scale}
    counts_for_confidence = bool(
        soft_event_configured and rc["soft_event"].get("counts_for_confidence", False)
    )
    if soft_event_configured and not counts_for_confidence and any(
        source_family(name) == "soft_event" for name, _ids, _weight in outer_runs
    ):
        # The final fusion keeps soft-event candidates for ranking.  Auto
        # confidence, however, must be calculated from the exact same leaf /
        # summary / person / query runs as the disabled baseline; merely
        # excluding the family from agreement still lets event runs change
        # candidate_count, top order, and margin.
        confidence_runs = [
            (name, ids, weight)
            for name, ids, weight in outer_runs
            if source_family(name) != "soft_event"
        ]
        confidence_raw = {
            name: values
            for name, values in raw_scores.items()
            if source_family(name) != "soft_event"
        }
        if len(confidence_runs) == 1 and confidence_runs[0][0] == "leaf":
            confidence_fusion = leaf_fusion
        else:
            confidence_fusion = fuse(
                confidence_runs,
                children=children,
                raw_scores=confidence_raw,
            )
        if msg_ids:
            confidence_fusion = fuse(
                [("fused", confidence_fusion.ids(), 1.0), (MESSAGE_RUN_NAME, msg_ids, msg_weight)],
                children={"fused": confidence_fusion}, raw_scores={**confidence_raw, **msg_raw},
            )
        # The confidence view is rebuilt after speaker/alias flags were added
        # to the final fusion, so mirror those non-event flags explicitly.
        for cand in confidence_fusion.ordered:
            if cand.doc_id in speaker_universe_ids:
                cand.flags["speaker_match"] = True
            if cand.doc_id in alias_ids:
                cand.flags["alias_match"] = True
    else:
        confidence_fusion = fusion
    meta = {
        "targets_requested": bool(targets),
        "targets_resolved": targets_resolved,
        "soft_event_counts_for_confidence": counts_for_confidence,
        "confidence_fusion": confidence_fusion,
    }
    return fusion, allowed, trace, meta


def retrieve(
    index: IndexBundle,
    client: PlayClient,
    query: str,
    *,
    top_k: int,
    mode: str = "leaf_parent",
    q_vec: np.ndarray | None = None,
    ranking: str | None = None,
    remaining_budget: float = 0.2,
    trace: dict[str, Any] | None = None,
    source_room_id: str | None = None,
    block_ids: list[str] | None = None,
    intent: dict[str, Any] | None = None,
    time_reference: RetrievalTimeReference | None = None,
) -> list[dict[str, Any]]:
    """Scope → R0 v2 candidates → (auto: assess → finish / recovery / rerank) → floor → span merge → parent expand.

    `auto` no longer picks a mode before searching. Concrete modes (`hybrid`/`heuristic`/`rerank`)
    still run the requested ranking on the same candidate pool (ablation)."""

    cfg = index.cfg
    intent = resolve_intent(query, intent, index.cfg)
    try:
        wanted_mode = normalize_ranking(
            ranking if ranking is not None else cfg.get("ranking"), default="heuristic"
        )
    except RankingError as exc:
        raise PlaygroundError(str(exc)) from exc
    auto = wanted_mode == "auto"
    t_embed = time.perf_counter()
    embed_info: dict[str, Any] = {"source": "given", "hedged": False}
    if q_vec is None:
        # q1(semantic_query)을 q0와 동시에 요청해 둔다. 후보 생성 단계에서 기다림 없이 쓴다.
        if mode == "leaf_parent":
            q1_text = _q1_text(query, intent, retrieval_config(cfg)["multi_query"])
            if q1_text:
                prefetch_query_vector(index, client, q1_text)
        q_vec, embed_info = query_vector(index, client, query)
    else:
        q_vec = np.asarray(q_vec, dtype=np.float32)
        q_vec /= np.linalg.norm(q_vec) + 1e-9
        q_vec = fit_query_vector(index, q_vec)
    embed_ms = _elapsed_ms(t_embed)
    if trace is not None:
        trace["embed_source"] = embed_info["source"]
        trace["embed_hedged"] = bool(embed_info.get("hedged"))
    weights = [float(w) for w in cfg["rrf_weights"]]
    char_limit = int(cfg["document_char_limit"])
    rc = retrieval_config(cfg)
    pool = pool_size(cfg, top_k=top_k)
    lex_lim, den_lim = search_limits(cfg)
    scope = _scope_allowed_ids(index, mode=mode, source_room_id=source_room_id, block_ids=block_ids)

    if mode == "baseline":
        if not index.baseline:
            raise PlaygroundError("baseline 인덱스가 없다. config baseline_enabled 후 prepare")
        lexical_query, _ = _lexical_query_expanded(index, query, intent)
        ranked = _hybrid_rank(
            fts=index.baseline_fts, vectors=index.baseline_vectors, ids=index.baseline_ids, query=query, q_vec=q_vec,
            weights=weights, lock=index.lock, allowed_ids=scope, lexical_limit=lex_lim, dense_limit=den_lim,
            lexical_query=lexical_query,
        )
        out = []
        for doc_id in ranked[:top_k]:
            doc = index.baseline_by_id[doc_id]
            out.append(
                {
                    "doc_id": doc_id,
                    **_hit_provenance(index, doc),
                    "excerpt": doc["body"][:char_limit],
                    "observation_ids": list(doc.get("observation_ids") or []),
                    "mode": "baseline",
                }
            )
        return out

    use_summary = mode in {"leaf_parent", "leaf_parent_summary"}
    t_hybrid = time.perf_counter()
    fusion, allowed, rank_trace_seed, gen_meta = _generate_candidates(
        index, client, query, q_vec=q_vec, intent=intent, scope=scope, use_summary=use_summary,
        pool=pool, lex_lim=lex_lim, den_lim=den_lim, time_reference=time_reference,
    )

    # ---- auto v2: observe the pool, then decide finish / recovery / rerank ----
    auto_reason = ""
    recovery_performed = False
    rank_mode = wanted_mode
    if auto:
        thresholds = dict(rc["auto"])
        rerank_ok = bool(rc["rerank"].get("enabled", True)) and _rerank_available()
        excluded_families = (
            set()
            if gen_meta.get("soft_event_counts_for_confidence", False)
            else None
        )
        confidence_fusion = (
            fusion
            if gen_meta.get("soft_event_counts_for_confidence", False)
            else gen_meta.get("confidence_fusion") or fusion
        )
        assessment = assess(
            confidence_fusion,
            thresholds=thresholds,
            targets_requested=gen_meta["targets_requested"],
            targets_resolved=gen_meta["targets_resolved"],
            excluded_families=excluded_families,
        )
        rank_trace_seed["auto_confidence"] = assessment.to_trace()
        decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget)
        if decision["recover"] and bool(rc["recovery"].get("enabled", True)):
            # Recovery (§28): R2 cannot invent candidates, so widen generation once before reranking.
            rec = rc["recovery"]
            scale = float(rec.get("limit_scale") or 1.5)
            fusion2, allowed2, trace2, meta2 = _generate_candidates(
                index, client, query, q_vec=q_vec, intent=intent, scope=scope, use_summary=use_summary,
                pool=pool_size(cfg, top_k=top_k, recovery=True),
                lex_lim=int(lex_lim * scale), den_lim=int(den_lim * scale),
                time_scale=float(rec.get("time_window_scale") or 2.0),
                time_reference=time_reference,
                limit_scale=scale,
            )
            recovery_performed = True
            rank_trace_seed["recovery"] = {
                "before": assessment.to_trace(),
                "candidate_union_n_before": rank_trace_seed.get("candidate_union_n"),
                "candidate_union_n_after": trace2.get("candidate_union_n"),
                "limits": trace2.get("candidate_limits"),
                "time_pool_n": trace2.get("recent_pool_n") or trace2.get("time_pool_n"),
            }
            fusion, allowed, gen_meta = fusion2, allowed2, meta2
            rank_trace_seed.update(trace2)
            pool = pool_size(cfg, top_k=top_k, recovery=True)
            excluded_families = (
                set()
                if gen_meta.get("soft_event_counts_for_confidence", False)
                else None
            )
            confidence_fusion = (
                fusion
                if gen_meta.get("soft_event_counts_for_confidence", False)
                else gen_meta.get("confidence_fusion") or fusion
            )
            assessment = assess(
                confidence_fusion,
                thresholds=thresholds,
                targets_requested=gen_meta["targets_requested"],
                targets_resolved=gen_meta["targets_resolved"],
                excluded_families=excluded_families,
            )
            rank_trace_seed["auto_confidence"] = assessment.to_trace()
            rank_trace_seed["recovery"]["after"] = assessment.to_trace()
            decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget,
                                      recovery_done=True)
        elif decision["recover"]:
            decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget,
                                      recovery_done=True)
            decision["reason"] = "rerank_low_confidence" if decision["mode"] == "rerank" else "no_rerank"
        rank_mode = str(decision["mode"])
        auto_reason = str(decision["reason"])
    cp = rc["candidate_pool"]
    fusion_ids = fusion.ids()
    if bool(rc["soft_event"].get("enabled", False)) and not bool(
        rc["soft_event"].get("reserve_in_pool", False)
    ):
        pool_order, guaranteed, guaranteed_by_source = guarantee_pool(
            fusion,
            pool=pool,
            per_source_min=int(cp.get("per_source_min") or 0),
            reserve_max_frac=float(cp.get("reserve_max_frac") or 0.3),
            excluded_sources={"soft_event"},
        )
    else:
        pool_order, guaranteed, guaranteed_by_source = guarantee_pool(
            fusion,
            pool=pool,
            per_source_min=int(cp.get("per_source_min") or 0),
            reserve_max_frac=float(cp.get("reserve_max_frac") or 0.3),
        )
    candidate_pool_ids = pool_order[:pool]
    rank_trace_seed["fusion_ids"] = fusion_ids[:pool]
    rank_trace_seed["candidate_pool_ids"] = candidate_pool_ids
    rank_trace_seed["candidate_pool_n"] = len(candidate_pool_ids)
    rank_trace_seed["pool_guaranteed_ids"] = list(guaranteed)
    rank_trace_seed["pool_guaranteed_n"] = len(guaranteed)
    rank_trace_seed["pool_guaranteed_by_source"] = {k: list(v) for k, v in guaranteed_by_source.items()}
    hybrid_ms = _elapsed_ms(t_hybrid)

    rank_mode = normalize_ranking(rank_mode, default="heuristic")
    # R0 ranking follows fusion order. R2 membership is the guaranteed pool (may include
    # low-weight retriever hits that the fused cut would have dropped).
    ranked_for_final = candidate_pool_ids if rank_mode == "rerank" else fusion_ids
    rank_trace: dict[str, Any] = {
        **rank_trace_seed,
        "hybrid_top10": fusion_ids[:10],
        "hybrid_ids": candidate_pool_ids,  # eval pool_recall = R2-eligible pool, not fusion cut
        "hybrid_n": len(fusion_ids),
        "pool": pool,
        "top_k": top_k,
        "embed_ms": embed_ms,
        "hybrid_ms": hybrid_ms,
        "query_chars": len(query or ""),
        "pool_hit_n": len(candidate_pool_ids),
        "recovery_performed": recovery_performed,
    }
    if auto:
        rank_trace["ranking_auto_reason"] = auto_reason
        rank_trace["auto_decision"] = rank_mode
    t_rank = time.perf_counter()
    ranked = _rank_leaf_ids(
        index,
        ranked_for_final,
        query=query,
        pool=pool,
        top_k=max(top_k * 4, pool) if rank_mode != "rerank" else top_k,
        ranking=rank_mode,
        remaining_budget=remaining_budget,
        trace=rank_trace,
        allowed_ids=allowed,
        intent=intent,
        auto=auto,
    )
    rank_trace["rerank_performed"] = rank_trace.get("applied") == "rerank"
    if rank_mode == "rerank" and rank_trace.get("abstained"):
        pass
    else:
        ranked, abs_meta = _apply_hybrid_floor(
            index, ranked, q_vec=q_vec, rank_mode=rank_mode
        )
        rank_trace.update(abs_meta)
        if not ranked and abs_meta.get("abstained"):
            rank_trace["abstained"] = True
    ranked, merge_map = finalize_ranked_ids(
        index,
        ranked,
        top_k=top_k,
        intent=intent,
        mode=str(cfg.get("dedup_mode") or "merge"),
    )
    rank_trace["dedup_mode"] = str(cfg.get("dedup_mode") or "merge")
    rank_trace["merged_leaf_ids"] = {k: v for k, v in merge_map.items() if v}
    if rank_trace.get("rerank_latency_ms") is not None:
        rank_trace["rerank_ms"] = round(float(rank_trace["rerank_latency_ms"]), 1)
    else:
        rank_trace["rerank_ms"] = _elapsed_ms(t_rank)
    if trace is not None:
        trace.update(rank_trace)
        trace["ranking"] = rank_mode
        trace["ranking_requested"] = wanted_mode
        trace["intent"] = intent
        if not ranked:
            trace["abstain_reason"] = "rerank_floor" if rank_trace.get("abstained") else "empty_in_allowed_scope"

    t_expand = time.perf_counter()
    out = []
    id_to_idx = {doc_id: i for i, doc_id in enumerate(index.leaf_ids)}
    cand_by_id = fusion.by_id
    time_scope = resolve_time_scope(
        (intent or {}).get("time") if isinstance((intent or {}).get("time"), dict) else None,
        reference=time_reference,
        recent=bool(intent and intent.get("recent")),
    )
    time_allows = None
    if time_scope.hard and (time_scope.lo is not None or time_scope.hi is not None):
        lo_ts = time_scope.lo.timestamp() if time_scope.lo is not None else None
        hi_ts = time_scope.hi.timestamp() if time_scope.hi is not None else None
        time_allows = make_time_filter(lo_ts, hi_ts, ts_of=lambda row: _parse_ts(row.get("parsed_time")))
    allowed_rooms = None
    allowed_obs = None
    if source_room_id:
        allowed_rooms = {source_room_id}
    elif block_ids is not None:
        allowed_obs = set()
        for lid in scope or []:
            leaf = index.leaf_by_id.get(lid)
            if leaf:
                allowed_obs.update(str(x) for x in (leaf.get("observation_ids") or []))
    segments: list[dict[str, Any]] = []
    extras_by_id: dict[str, list[str]] = {}
    for doc_id in ranked:
        extras = merge_map.get(doc_id) or []
        extras_by_id[doc_id] = extras
        segments.append(merged_span(index, doc_id, extras) if extras else index.leaf_by_id[doc_id])
    packed = pack_evidence_set(
        index.messages,
        segments,
        before=int(cfg["parent_expand_before"]),
        after=int(cfg["parent_expand_after"]),
        with_prefix=bool(cfg.get("context_prefix", True)),
        doc_char_limit=char_limit,
        total_char_limit=int(cfg.get("evidence_total_char_limit") or char_limit * max(1, top_k)),
        allowed_observation_ids=allowed_obs,
        allowed_rooms=allowed_rooms,
        time_allows=time_allows,
        dedupe_visible=True,
    )
    packed_by_id = {str(d["doc_id"]): d for d in packed["docs"]}
    for doc_id in ranked:
        extras = extras_by_id.get(doc_id) or []
        source_leaf = index.leaf_by_id[doc_id]
        expanded = packed_by_id.get(doc_id) or {}
        summary_text = ""
        if use_summary and doc_id in index.summary_by_id:
            summary_text = str(index.summary_by_id[doc_id].get("summary") or "")
        date_label = _leaf_source_date(index, source_leaf)
        date_label = _context_only_date(index, expanded, source_leaf) or date_label
        out.append(
            {
                "doc_id": doc_id,
                **_hit_provenance(index, source_leaf),
                "excerpt": expanded.get("excerpt") or "",
                "summary": summary_text,
                "observation_ids": list(expanded.get("observation_ids") or []),
                "visible_observation_ids": list(expanded.get("visible_observation_ids") or []),
                "visible_spans": list(expanded.get("visible_spans") or []),
                "packing_status": expanded.get("status") or "insufficient",
                "leaf_observation_ids": list(source_leaf.get("observation_ids") or []),
                "merged_leaf_ids": extras,
                "mode": "leaf_parent_summary"
                if summary_text
                else ("leaf_only" if mode == "leaf_only" else "leaf_parent"),
                "span": f"{expanded.get('start', source_leaf.get('start'))}-{int(expanded.get('end') or source_leaf.get('end') or 1)-1}",
                "date": date_label,
                "speakers": list(source_leaf.get("speakers") or []),
                "sources": dict((cand_by_id.get(doc_id).sources if doc_id in cand_by_id else {})),
                "intent": intent,
                "dense_cos": float(index.leaf_vectors[id_to_idx[doc_id]] @ q_vec)
                if doc_id in id_to_idx
                else None,
            }
        )
    expand_ms = _elapsed_ms(t_expand)
    if trace is not None:
        trace["expand_ms"] = expand_ms
        trace["visible_spans"] = list(packed.get("visible_spans") or [])
        trace["visible_observation_ids"] = list(packed.get("visible_observation_ids") or [])
        trace["candidate_observation_ids"] = sorted(
            {oid for lid in (rank_trace.get("candidate_pool_ids") or []) for oid in (index.leaf_by_id.get(lid) or {}).get("observation_ids") or []}
        )
        trace["time_reference"] = time_reference.to_trace() if time_reference is not None else None
        trace["retrieve_ms"] = round(
            float(trace.get("embed_ms") or 0)
            + float(trace.get("hybrid_ms") or 0)
            + float(trace.get("rerank_ms") or 0)
            + expand_ms,
            1,
        )
    return out
