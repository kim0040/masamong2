"""턴 진단: 검색 trace를 사람·에이전트가 바로 읽을 수 있는 작은 요약으로 줄인다.

화면(답변 아래 진단 카드)과 로그(`debug_turns.jsonl`의 `diagnosis`, `debug_report.py`)가 같은 함수를 쓴다.
코퍼스·채널별 값은 넣지 않는다. 기준(점수 하한 등)은 trace와 config에서만 읽는다.

`warnings`는 규칙 기반 표시일 뿐 정답 판정이 아니다. 코드 값은 안정적인 이름이고 `label`은 화면용 설명이다.
"""

from __future__ import annotations

from typing import Any, Mapping, Sequence

DIAGNOSIS_VERSION = 1
DEFAULTS: dict[str, Any] = {
    # 남은 근거의 최고 재순위 점수가 하한 + 이 값보다 낮으면 "하한 근처"로 표시한다
    # (2026-09-23 답변 점검: 환각은 관련 없는 근거가 하한을 겨우 넘을 때 생겼다).
    "floor_margin": 0.05,
    "top_scores": 8,
    "hit_speakers": 6,
}


def diagnosis_config(cfg: Mapping[str, Any] | None) -> dict[str, Any]:
    out = dict(DEFAULTS)
    raw = (cfg or {}).get("diagnosis")
    if isinstance(raw, Mapping):
        out.update({k: raw[k] for k in DEFAULTS if k in raw})
    return out


def _num(value: Any) -> float | None:
    try:
        n = float(value)
    except (TypeError, ValueError):
        return None
    return n if n == n else None  # NaN 제외


def _warn(code: str, label: str, **detail: Any) -> dict[str, Any]:
    row: dict[str, Any] = {"code": code, "label": label}
    row.update({k: v for k, v in detail.items() if v is not None})
    return row


def rerank_scores_by_doc(rank_trace: Mapping[str, Any]) -> dict[str, float]:
    order = list(rank_trace.get("rerank_order") or [])
    scores = list(rank_trace.get("rerank_scores") or [])
    if not order or len(order) != len(scores):
        return {}
    out: dict[str, float] = {}
    for doc_id, score in zip(order, scores):
        n = _num(score)
        if n is not None:
            out[str(doc_id)] = n
    return out


def build_diagnosis(
    *,
    rank_trace: Mapping[str, Any] | None,
    intent: Mapping[str, Any] | None,
    hits: Sequence[Mapping[str, Any]] | None,
    rag_used: bool,
    rag_state: str | None,
    memory_requirement: str | None,
    tools: Sequence[Mapping[str, Any]] | None = None,
    timing: Mapping[str, Any] | None = None,
    answer: str | None = None,
    cfg: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    c = diagnosis_config(cfg)
    rt = dict(rank_trace or {})
    it = dict(intent or {})
    hits = list(hits or [])
    score_by = rerank_scores_by_doc(rt)
    floor = _num(rt.get("rerank_min_score"))
    reranked = bool(rt.get("rerank_performed", rt.get("applied") == "rerank")) and bool(score_by)

    top_scores = []
    for doc_id, score in list(score_by.items())[: int(c["top_scores"])]:
        top_scores.append(
            {"doc_id": doc_id, "score": round(score, 4), "above_floor": floor is None or score >= floor}
        )

    persons_raw = rt.get("persons") if isinstance(rt.get("persons"), Mapping) else None
    persons = None
    if persons_raw:
        keys = persons_raw.get("speaker_keys") if isinstance(persons_raw.get("speaker_keys"), Mapping) else {}
        persons = {
            "targets": list(persons_raw.get("targets") or []),
            "matched_speakers": list(persons_raw.get("speakers") or []),
            "ambiguous": list(persons_raw.get("ambiguous") or []),
            "unresolved": list(persons_raw.get("unresolved") or []),
            "account_linked_n": keys.get("account_linked_n"),
            "label_only_n": keys.get("label_only_n"),
            "scope": (persons_raw.get("scope") or {}).get("mode") if isinstance(persons_raw.get("scope"), Mapping) else None,
        }

    time_raw = it.get("time") if isinstance(it.get("time"), Mapping) else {}
    time_scope = {
        "planner_kind": time_raw.get("kind"),
        "planner_from": time_raw.get("from"),
        "planner_to": time_raw.get("to"),
        "kind": rt.get("time_kind"),
        "applied": bool(rt.get("time_window_applied")),
        "hard": rt.get("time_window_hard"),
        "from": rt.get("time_from"),
        "to": rt.get("time_to"),
        "pool_n": rt.get("time_pool_n"),
        "widened": bool(rt.get("time_window_scale_applied")),
        "unresolved": bool(rt.get("time_unresolved")),
        "recent_window_applied": bool(rt.get("recent_window_applied")),
    }
    if time_scope["applied"] and time_scope["kind"] == "recent" and not time_scope["from"]:
        # 막연한 "최근"은 날짜 대신 설정의 창(일수·leaf 수)으로 자른다. 값은 config에서만 읽는다.
        time_scope["recent_window_days"] = (cfg or {}).get("recent_window_days")
        time_scope["recent_window_leaves"] = (cfg or {}).get("recent_window_leaves")
        time_scope["scale"] = rt.get("time_window_scale")

    conf = rt.get("auto_confidence") if isinstance(rt.get("auto_confidence"), Mapping) else {}
    hit_rows = []
    for h in hits:
        doc_id = str(h.get("doc_id") or "")
        hit_rows.append(
            {
                "rank": h.get("rank"),
                "doc_id": doc_id,
                "date": h.get("date"),
                "room": h.get("source_room_id"),
                "speakers": list(h.get("speakers") or [])[: int(c["hit_speakers"])],
                "score": round(score_by[doc_id], 4) if doc_id in score_by else None,
            }
        )

    kept_scores = [r["score"] for r in hit_rows if r["score"] is not None]
    best_kept = max(kept_scores) if kept_scores else None

    warnings: list[dict[str, Any]] = []
    source = str(it.get("source") or "")
    if rag_used and source and source != "planner":
        warnings.append(_warn("planner_fallback", "플래너 출력 없이 기본 의도로 검색", intent_source=source))
    if persons:
        if persons["unresolved"]:
            warnings.append(_warn("person_unresolved", "인물을 허용 범위의 화자와 연결하지 못함", names=persons["unresolved"]))
        if persons["ambiguous"]:
            warnings.append(_warn("person_ambiguous", "같은 이름 후보가 여러 명", names=persons["ambiguous"]))
    if rt.get("rerank_error"):
        warnings.append(_warn("rerank_error", "재순위 실패 → R0 결과 사용", error=rt.get("rerank_error")))
    if reranked and floor is not None and best_kept is not None and best_kept < floor + float(c["floor_margin"]):
        warnings.append(
            _warn("near_floor", "남은 근거 점수가 하한에 가까움 (관련 없는 근거일 수 있음)", best=best_kept, floor=floor)
        )
    if rag_used and not hits:
        code = "abstained_floor" if rt.get("abstained") else "no_evidence"
        label = "모든 후보가 점수 하한 미만 → 근거 없음" if rt.get("abstained") else "허용 범위에서 후보 없음"
        warnings.append(_warn(code, label, answer_chars=len(answer or "") if answer is not None else None))
    if time_scope["applied"] and time_scope["pool_n"] == 0:
        warnings.append(_warn("time_window_empty", "시간 창 안에 후보 없음", kind=time_scope["kind"]))
    if time_scope["widened"]:
        warnings.append(_warn("time_window_widened", "시간 창을 넓혀 다시 찾음"))
    if time_scope["unresolved"]:
        warnings.append(_warn("time_unresolved", "시간 표현을 해석하지 못함", reason=rt.get("time_unresolved_reason")))
    for t in tools or []:
        if isinstance(t, Mapping) and t.get("ok") is False:
            warnings.append(_warn("tool_failed", "도구 실패", tool=t.get("tool"), status=t.get("status") or t.get("error")))

    return {
        "version": DIAGNOSIS_VERSION,
        "rag_used": bool(rag_used),
        "rag_state": rag_state,
        "memory_requirement": memory_requirement,
        "ranking": {
            "requested": rt.get("ranking_requested") or rt.get("ranking"),
            "applied": rt.get("applied"),
            "auto_reason": rt.get("ranking_auto_reason"),
            "confidence": conf.get("level"),
            "confidence_reasons": list(conf.get("reasons") or []),
            "recovery": bool(rt.get("recovery_performed")),
        },
        "intent": {
            "source": source or None,
            "kind": it.get("kind"),
            "targets": list(it.get("targets") or []),
            "semantic_query": it.get("semantic_query"),
            "key_terms": list(it.get("key_terms") or []),
            "unresolved": list(it.get("unresolved") or []),
        },
        "queries": dict(rt.get("queries_used") or {}),
        "time": time_scope,
        "persons": persons,
        "candidates": {
            "union_n": rt.get("candidate_union_n"),
            "pool_n": rt.get("pool_n"),
            "retrievers": list(rt.get("retrievers_used") or []),
            "per_source": dict(rt.get("candidate_sources") or {}),
        },
        "rerank": {
            "performed": reranked,
            "model": rt.get("rerank_model"),
            "floor": floor,
            "kept_n": rt.get("rerank_kept_n"),
            "abstained": bool(rt.get("abstained")),
            "top": top_scores,
        },
        "embed": {
            "source": rt.get("embed_source"),
            "hedged": rt.get("embed_hedged"),
            "q1_source": rt.get("q1_embed_source"),
        },
        "hits": hit_rows,
        "tools": [
            {"tool": t.get("tool"), "ok": t.get("ok"), "status": t.get("status")}
            for t in (tools or [])
            if isinstance(t, Mapping)
        ],
        "timing": dict(timing or {}),
        "warnings": warnings,
    }


def diagnosis_from_record(row: Mapping[str, Any]) -> dict[str, Any]:
    """debug_turns 한 줄의 진단. 옛 기록(diagnosis 없음)은 rank_trace에서 다시 만든다."""

    diag = row.get("diagnosis")
    if isinstance(diag, Mapping):
        return dict(diag)
    cfg = row.get("config")
    return build_diagnosis(
        rank_trace=row.get("rank_trace") or {},
        intent=row.get("intent") or {},
        hits=row.get("hits") or [],
        rag_used=bool(row.get("rag_used")),
        rag_state=row.get("rag_state"),
        memory_requirement=row.get("memory_requirement"),
        tools=row.get("tools") or [],
        timing=row.get("timing") or {},
        answer=row.get("answer"),
        cfg=cfg if isinstance(cfg, Mapping) else None,
    )


_HIT_KEYS = ("rank", "doc_id", "date", "source_room_id", "label", "speakers", "summary", "excerpt")


def compact_record(row: Mapping[str, Any]) -> dict[str, Any]:
    """화면이 저장된 답에 진단·근거·판정을 다시 붙일 때 쓰는 작은 형태 (rank_trace 전체는 빼고)."""

    return {
        "debug_id": row.get("debug_id"),
        "kind": row.get("kind"),
        "diagnosis": diagnosis_from_record(row) if row.get("kind", "turn") == "turn" else None,
        "hits": [{k: h.get(k) for k in _HIT_KEYS if k in h} for h in (row.get("hits") or []) if isinstance(h, Mapping)],
        "timing": dict(row.get("timing") or {}),
        "tools": list(row.get("tools") or []),
        "flags": [
            {k: f.get(k) for k in ("ok", "tags", "note", "ts")} for f in (row.get("flags") or []) if isinstance(f, Mapping)
        ],
    }
