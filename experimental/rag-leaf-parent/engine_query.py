"""Sandbox engine, question side: intent, person targets and aliases, and the dense/lexical hit primitives (re-exported by engine.py)."""

from __future__ import annotations

import re
import threading
from typing import Any, Mapping
import numpy as np

from engine_index import (
    IndexBundle,
    ThreadSafeLexicalIndex,
)

from person_retrieval import build_identity_table  # noqa: E402

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
