"""Sandbox-only experiments. NOT part of the Go port contract: the R1 heuristic reranker and the soft-event candidates.

The Go port leaves both out on purpose (internal/memory/search/config.go). Nothing here is compared by the parity
tests; `retrieve()` reaches this module only through the `ranking="heuristic"` mode and the `soft_events` recipe.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from typing import Any, Mapping
import numpy as np

from engine_index import (
    IndexBundle,
    PlaygroundError,
    ThreadSafeLexicalIndex,
    _parse_ts,
)
from engine_query import (
    DEFAULT_ALIAS_IGNORE,
    LEXICAL_LIMIT,
    _alias_table,
    _clean_planner_targets,
    _dense_hits,
    _identity_re,
    _query_targets,
    _target_labels,
    _token_in_text,
    resolve_intent,
)

from retrieval_post import (  # noqa: E402
    recency_anchor_ts,
    scaled_person,
)
from retrieval_config import retrieval_config  # noqa: E402
from soft_events import (  # noqa: E402
    SoftEventRecipe,
    build_soft_events,
    project_event_ranks,
)

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
