"""matched leaf 원문을 먼저 보존하는 근거 포장.

원문은 수정하지 않는다. 요약을 원문으로 합성하지 않는다. source observation을 지우지 않는다.

계약 (docs/rag/README.md §3 샌드박스 공통 계약 3·4):

- 원문 offset은 index message ``content`` (collect_messages가 strip해 저장한 본문)
  기준의 Unicode 문자 ``[char_start, char_end)`` 다. 형식화된 excerpt 안의 위치는
  ``excerpt_start/excerpt_end`` 로 따로 준다. 항상
  ``excerpt[excerpt_start:excerpt_end] == content[char_start:char_end]`` 다.
- ``observation_ids`` (legacy) = 허용 범위 안의 parent window 관찰 전체 (후보 집합).
  보였다는 뜻이 아니다. 보인 원문은 ``visible_observation_ids`` / ``visible_spans`` 만 믿는다.
- ``matched_observation_ids`` = 허용 범위 안의 matched 구간 ``[segment.start, segment.end)``.
  merged span이면 병합된 구간 전체다. ``leaf_observation_ids`` 는 호출자가 준 segment의
  ``observation_ids`` 그대로다.
- 허용 관찰 집합 ``None`` 은 "제한 없음", 빈 집합은 "허용 없음"이다. 시간 판정은 호출자가
  ``time_allows(row) -> bool`` 로 주입한다. 카카오 naive time에 이 모듈이
  timezone을 부여하지 않는다.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from collections.abc import Callable, Iterable, Mapping, Sequence
from pathlib import Path
from typing import Any

from segment import format_body

HEAD_CUT_MARKER = "…(앞 생략) "
TAIL_CUT_MARKER = " …(뒤 생략)"
ELIDED_BEFORE = "…(앞 {n}개 생략)"
ELIDED_AFTER = "…(뒤 {n}개 생략)"
ELIDED_MIDDLE = "…({n}개 생략)"

STATUS_COMPLETE = "complete"
STATUS_PARTIAL = "partial"
STATUS_INSUFFICIENT = "insufficient"
STATUS_DUPLICATE = "duplicate"
STATUS_OMITTED = "omitted"

ROLE_MATCHED = "matched"
ROLE_CONTEXT = "context"

EXCLUDE_ROOM = "room"
EXCLUDE_ALLOWED = "allowed"
EXCLUDE_TIME = "time"
EXCLUDE_ALREADY_VISIBLE = "already_visible"

TimeFilter = Callable[[Mapping[str, Any]], bool]


# ---------------------------------------------------------------- window / scope


def parent_window(
    messages: Sequence[Mapping[str, Any]],
    segment: Mapping[str, Any],
    *,
    before: int,
    after: int,
) -> tuple[int, int]:
    """matched 구간을 같은 논리 방 안에서 앞뒤 ``before/after`` 개만큼 넓힌 ``[start, end)``."""

    n = len(messages)
    seg_start = int(segment["start"])
    seg_end = int(segment["end"])
    room = str(segment.get("source_room_id") or "")
    lo, hi = 0, n
    if room and 0 <= seg_start < n:
        lo = seg_start
        while lo > 0 and str(messages[lo - 1].get("source_room_id") or "") == room:
            lo -= 1
        hi = seg_end
        while hi < n and str(messages[hi].get("source_room_id") or "") == room:
            hi += 1
    start = max(lo, seg_start - max(0, int(before)))
    end = min(hi, seg_end + max(0, int(after)))
    return start, end


def make_time_filter(
    lo: float | None,
    hi: float | None,
    *,
    ts_of: Callable[[Mapping[str, Any]], float | None],
    unknown_time: str = "exclude",
) -> TimeFilter:
    """hard 시간 조건 ``lo <= ts < hi`` 판정. ``ts_of`` 는 호출자(04/08)가 준다.

    시각을 모르는 관찰은 기본으로 제외한다 (``unknown_time="include"`` 로 바꿀 수 있다).
    """

    if unknown_time not in {"exclude", "include"}:
        raise ValueError("unknown_time must be 'exclude' or 'include'")
    include_unknown = unknown_time == "include"

    def _allows(row: Mapping[str, Any]) -> bool:
        ts = ts_of(row)
        if ts is None:
            return include_unknown
        if lo is not None and ts < lo:
            return False
        if hi is not None and ts >= hi:
            return False
        return True

    return _allows


def _exclusion_reason(
    row: Mapping[str, Any],
    *,
    allowed_observation_ids: set[str] | None,
    allowed_rooms: set[str] | None,
    time_allows: TimeFilter | None,
) -> str | None:
    if allowed_rooms is not None:
        original = str(row.get("source_room_original") or row.get("source_room_id") or "")
        if original not in allowed_rooms:
            return EXCLUDE_ROOM
    if allowed_observation_ids is not None:
        if str(row.get("observation_id")) not in allowed_observation_ids:
            return EXCLUDE_ALLOWED
    if time_allows is not None and not time_allows(row):
        return EXCLUDE_TIME
    return None


# ---------------------------------------------------------------- units / layout


class _Unit:
    __slots__ = ("index", "row", "oid", "role", "excluded", "label", "content", "cut", "sel")

    def __init__(self, index: int, row: Mapping[str, Any], role: str, excluded: str | None) -> None:
        self.index = index
        self.row = row
        self.oid = str(row.get("observation_id"))
        self.role = role
        self.excluded = excluded
        self.label = str(row.get("actor_label") or "?")
        self.content = str(row.get("content") or "")
        self.cut = False  # 렌더링 보류 (already_visible)
        self.sel: tuple[int, int] | None = None  # 보이는 [char_start, char_end)


def _prefix_text(rows: list[Mapping[str, Any]], with_prefix: bool) -> str:
    """format_body와 동일한 prefix. 본문은 offset에 영향이 없으므로 원 row로 계산한다."""

    if not with_prefix or not rows:
        return ""
    full = format_body(list(rows), with_prefix=True)
    body = format_body(list(rows), with_prefix=False)
    if body:
        assert full.endswith(body)
        return full[: len(full) - len(body) - 1]
    return full


def _render(units: list[_Unit], *, with_prefix: bool) -> tuple[str, list[dict[str, Any]], int]:
    """선택 상태로 excerpt를 만들고 visible span을 계산한다. (excerpt, spans, prefix_chars)"""

    visible_rows = [u.row for u in units if u.sel is not None]
    if not visible_rows:
        return "", [], 0
    prefix = _prefix_text(visible_rows, with_prefix)
    parts: list[str] = []
    spans: list[dict[str, Any]] = []
    pos = 0
    if prefix:
        parts.append(prefix)
        pos = len(prefix)
    first_line = not prefix
    omitted_run = 0
    seen_visible = False
    suppress_after_elision = False

    def _emit(text: str) -> None:
        nonlocal pos, first_line
        if not first_line:
            parts.append("\n")
            pos += 1
        parts.append(text)
        pos += len(text)
        first_line = False

    def _flush_omitted(kind: str) -> None:
        nonlocal omitted_run
        if omitted_run <= 0:
            return
        tpl = {"before": ELIDED_BEFORE, "after": ELIDED_AFTER, "middle": ELIDED_MIDDLE}[kind]
        _emit(tpl.format(n=omitted_run))
        omitted_run = 0

    for u in units:
        if u.excluded is not None or u.cut:
            continue
        if u.sel is None:
            omitted_run += 1
            continue
        _flush_omitted("before" if not seen_visible else "middle")
        seen_visible = True
        cs, ce = u.sel
        head = f"{u.label}: "
        if cs > 0:
            head += HEAD_CUT_MARKER
        tail = TAIL_CUT_MARKER if ce < len(u.content) else ""
        line_start = pos + (0 if first_line else 1)
        es = line_start + len(head)
        _emit(head + u.content[cs:ce] + tail)
        spans.append(
            {
                "observation_id": u.oid,
                "role": u.role,
                "char_start": cs,
                "char_end": ce,
                "content_len": len(u.content),
                "complete": cs == 0 and ce == len(u.content),
                "excerpt_start": es,
                "excerpt_end": es + (ce - cs),
            }
        )
        if ce < len(u.content):
            omitted_run = 0
            suppress_after_elision = True
        else:
            suppress_after_elision = False
    if omitted_run and not suppress_after_elision:
        _flush_omitted("after")
    return "".join(parts), spans, len(prefix)


def _select_layout(
    units: list[_Unit],
    *,
    char_limit: int,
    with_prefix: bool,
    per_message_cap: int | None,
    min_partial_chars: int,
) -> None:
    """matched 먼저, 남은 예산으로 앞뒤 문맥을 번갈아 채운다. ``u.sel`` 을 채운다."""

    def _fits() -> bool:
        excerpt, _, _ = _render(units, with_prefix=with_prefix)
        return len(excerpt) <= char_limit

    def _overshoot() -> int:
        excerpt, _, _ = _render(units, with_prefix=with_prefix)
        return len(excerpt) - char_limit

    def _try(u: _Unit, side: str) -> bool:
        """전체 → 부분 순으로 시도. side: head(앞부분 유지) / tail(뒷부분 유지)."""

        n = len(u.content)
        cap = n if per_message_cap is None else min(n, max(0, int(per_message_cap)))
        u.sel = (0, cap) if side == "head" else (n - cap, n)
        over = _overshoot()
        if over <= 0:
            return True
        take = cap - over
        # 마커가 붙으면 그 길이만큼 더 줄어야 한다 (한 번 더 확인)
        for _ in range(3):
            if take < min_partial_chars:
                u.sel = None
                return False
            u.sel = (0, take) if side == "head" else (n - take, n)
            over = _overshoot()
            if over <= 0:
                return True
            take -= over
        u.sel = None
        return False

    active = [u for u in units if u.excluded is None and not u.cut]
    for u in active:
        u.sel = None
    matched = [u for u in active if u.role == ROLE_MATCHED]
    deferred = [u for u in units if u.role == ROLE_MATCHED and u.cut and u.excluded is None]
    if not matched:
        if deferred:
            for u in active:
                if not _try(u, "head"):
                    break
                if u.sel is None or u.sel[1] < len(u.content):
                    break
        return
    # 짧은 matched를 통째로 남기고, 예산을 넘는 긴 matched는 중간부터 생략한다
    for u in matched:
        n = len(u.content)
        cap = n if per_message_cap is None else min(n, max(0, int(per_message_cap)))
        u.sel = (0, cap)
    while not _fits() and sum(1 for u in matched if u.sel is not None) > 1:
        present = [u for u in matched if u.sel is not None]
        mid = (present[0].index + present[-1].index) / 2
        drop = max(present, key=lambda u: (len(u.content), -abs(u.index - mid)))
        drop.sel = None
    if not _fits():
        remaining = [u for u in matched if u.sel is not None] or matched[:1]
        for u in remaining:
            u.sel = None
            _try(u, "head")
        while not _fits():
            held = [u for u in matched if u.sel is not None]
            if not held:
                break
            held[-1].sel = None
    if not _fits():
        for u in reversed(matched):
            u.sel = None
            if _fits():
                break
        return
    if any(u.sel is not None and (u.sel[0] > 0 or u.sel[1] < len(u.content)) for u in matched):
        return
    first_pos = active.index(matched[0])
    last_pos = active.index(matched[-1])
    b = first_pos - 1
    a = last_pos + 1
    open_before = True
    open_after = True
    while (open_before and b >= 0) or (open_after and a < len(active)):
        if open_before and b >= 0:
            u = active[b]
            if _try(u, "tail") and u.sel is not None and u.sel[0] == 0:
                b -= 1
            else:
                open_before = False
        if open_after and a < len(active):
            u = active[a]
            if _try(u, "head") and u.sel is not None and u.sel[1] == len(u.content):
                a += 1
            else:
                open_after = False


# ---------------------------------------------------------------- public API


def pack_segment_evidence(
    messages: Sequence[Mapping[str, Any]],
    segment: Mapping[str, Any],
    *,
    before: int,
    after: int,
    with_prefix: bool,
    char_limit: int,
    allowed_observation_ids: Iterable[str] | None = None,
    allowed_rooms: Iterable[str] | None = None,
    time_allows: TimeFilter | None = None,
    required_observation_ids: Iterable[str] | None = None,
    already_visible: Iterable[str] | None = None,
    per_message_cap: int | None = None,
    min_partial_chars: int = 8,
) -> dict[str, Any]:
    """하나의 matched segment(leaf 또는 merged span)를 parent 문맥과 함께 예산 안에 포장한다.

    반환 키는 기존 ``segment.expand_segment`` 의 superset이다 (모듈 docstring 참조).
    ``status``: complete(허용 matched 전부 보임) / partial(일부 잘림·생략) /
    insufficient(matched 원문이 하나도 보이지 않거나 required가 완전히 보이지 않음).
    """

    allowed_ids = None if allowed_observation_ids is None else {str(x) for x in allowed_observation_ids}
    rooms = None if allowed_rooms is None else {str(x) for x in allowed_rooms}
    required = {str(x) for x in (required_observation_ids or [])}
    claimed = {str(x) for x in (already_visible or [])}
    seg_start = int(segment["start"])
    seg_end = int(segment["end"])
    start, end = parent_window(messages, segment, before=before, after=after)

    units: list[_Unit] = []
    for i in range(start, end):
        row = messages[i]
        role = ROLE_MATCHED if seg_start <= i < seg_end else ROLE_CONTEXT
        reason = _exclusion_reason(
            row, allowed_observation_ids=allowed_ids, allowed_rooms=rooms, time_allows=time_allows
        )
        u = _Unit(i, row, role, reason)
        if reason is None and u.oid in claimed:
            u.cut = True
        units.append(u)

    limit = int(char_limit)
    if limit > 0:
        _select_layout(
            units,
            char_limit=limit,
            with_prefix=with_prefix,
            per_message_cap=per_message_cap,
            min_partial_chars=max(1, int(min_partial_chars)),
        )
    excerpt, spans, prefix_chars = _render(units, with_prefix=with_prefix)
    if limit <= 0:
        excerpt, spans, prefix_chars = "", [], 0
    assert len(excerpt) <= max(0, limit)

    in_scope = [u for u in units if u.excluded is None]
    matched_in_scope = [u for u in in_scope if u.role == ROLE_MATCHED]
    visible = [u for u in units if u.sel is not None]
    visible_ids = [u.oid for u in visible]
    span_by_id = {s["observation_id"]: s for s in spans}
    excluded: dict[str, list[str]] = {
        EXCLUDE_ROOM: [],
        EXCLUDE_ALLOWED: [],
        EXCLUDE_TIME: [],
        EXCLUDE_ALREADY_VISIBLE: [],
    }
    for u in units:
        if u.excluded is not None:
            excluded[u.excluded].append(u.oid)
        elif u.cut:
            excluded[EXCLUDE_ALREADY_VISIBLE].append(u.oid)
    matched_excluded = [u.oid for u in units if u.role == ROLE_MATCHED and u.excluded is not None]
    matched_deferred = [u.oid for u in units if u.role == ROLE_MATCHED and u.excluded is None and u.cut]
    omitted = [u.oid for u in in_scope if not u.cut and u.sel is None]
    matched_visible_complete = [
        u.oid for u in matched_in_scope if u.sel is not None and span_by_id[u.oid]["complete"]
    ]
    matched_visible_any = [u.oid for u in matched_in_scope if u.sel is not None]

    reason: str | None = None
    if limit <= 0:
        reason = "char_limit"
    elif not matched_in_scope:
        if seg_end <= seg_start:
            reason = "matched_empty"
        elif allowed_ids is not None and not allowed_ids:
            reason = "empty_allowed_set"
        else:
            reason = "matched_out_of_scope"
    elif not [u for u in matched_in_scope if not u.cut]:
        if any(u.sel is not None for u in units):
            reason = None
        else:
            reason = "matched_already_visible"
    elif not matched_visible_any:
        matched_prefix = _prefix_text([u.row for u in matched_in_scope if not u.cut], with_prefix)
        reason = "prefix_exhausts_budget" if matched_prefix and len(matched_prefix) + 1 >= limit else "budget"
    else:
        missing_required = [
            oid
            for oid in sorted(required)
            if not (oid in span_by_id and span_by_id[oid]["complete"])
        ]
        if missing_required:
            reason = "required_not_visible"
    packable_n = len([u for u in matched_in_scope if not u.cut])
    if reason is not None:
        status = STATUS_DUPLICATE if reason == "matched_already_visible" else STATUS_INSUFFICIENT
    elif packable_n == 0 and matched_in_scope:
        status = STATUS_COMPLETE
    elif len(matched_visible_complete) == packable_n:
        status = STATUS_COMPLETE
    else:
        status = STATUS_PARTIAL
    context_in_scope = [u for u in in_scope if u.role == ROLE_CONTEXT and not u.cut]
    context_complete = all(u.sel is not None and span_by_id[u.oid]["complete"] for u in context_in_scope)

    return {
        # legacy keys (expand_segment)
        "doc_id": segment["doc_id"],
        "kind": "parent_expand",
        "matched_leaf": segment["doc_id"],
        "start": start,
        "end": end,
        "source_room_id": segment.get("source_room_id"),
        "observation_ids": [u.oid for u in in_scope],
        "speakers": list(segment.get("speakers") or []),
        "excerpt": excerpt,
        "leaf_observation_ids": list(segment.get("observation_ids") or []),
        # Evidence packing contract
        "status": status,
        "insufficient_reason": reason,
        "char_limit": limit,
        "used_chars": len(excerpt),
        "prefix_chars": prefix_chars,
        "matched_span": [seg_start, seg_end],
        "matched_observation_ids": [u.oid for u in matched_in_scope],
        "matched_excluded_observation_ids": matched_excluded,
        "matched_deferred_observation_ids": matched_deferred,
        "matched_counts": {
            "span": seg_end - seg_start,
            "in_scope": len(matched_in_scope),
            "packable": len([u for u in matched_in_scope if not u.cut]),
            "visible_any": len(matched_visible_any),
            "visible_complete": len(matched_visible_complete),
        },
        "visible_observation_ids": visible_ids,
        "visible_spans": spans,
        "omitted_observation_ids": omitted,
        "context_complete": context_complete,
        "excluded": excluded,
        "scope_applied": {
            "observation_ids": allowed_ids is not None,
            "rooms": rooms is not None,
            "time": time_allows is not None,
        },
    }


def verify_visible_spans(
    messages: Sequence[Mapping[str, Any]] | Mapping[str, str],
    packed: Mapping[str, Any],
) -> list[str]:
    """excerpt와 원문 offset이 일치하는지 검사한다. 불일치한 observation_id 목록을 돌려준다.

    ``messages`` 는 row 목록 또는 ``{observation_id: content}`` 매핑이다.
    """

    if isinstance(messages, Mapping):
        content_of = {str(k): str(v) for k, v in messages.items()}
    else:
        content_of = {str(r.get("observation_id")): str(r.get("content") or "") for r in messages}
    excerpt = str(packed.get("excerpt") or "")
    bad: list[str] = []
    for span in packed.get("visible_spans") or []:
        oid = str(span["observation_id"])
        content = content_of.get(oid)
        if content is None:
            bad.append(oid)
            continue
        cs, ce = int(span["char_start"]), int(span["char_end"])
        es, ee = int(span["excerpt_start"]), int(span["excerpt_end"])
        if not (0 <= cs <= ce <= len(content)) or len(content) != int(span["content_len"]):
            bad.append(oid)
            continue
        if excerpt[es:ee] != content[cs:ce]:
            bad.append(oid)
            continue
        if bool(span["complete"]) != (cs == 0 and ce == len(content)):
            bad.append(oid)
    return bad


def pack_evidence_set(
    messages: Sequence[Mapping[str, Any]],
    segments: Sequence[Mapping[str, Any]],
    *,
    before: int,
    after: int,
    with_prefix: bool,
    doc_char_limit: int,
    total_char_limit: int | None = None,
    doc_overhead_chars: int = 0,
    allowed_observation_ids: Iterable[str] | None = None,
    allowed_rooms: Iterable[str] | None = None,
    time_allows: TimeFilter | None = None,
    required_by_doc: Mapping[str, Iterable[str]] | None = None,
    dedupe_visible: bool = True,
    per_message_cap: int | None = None,
    min_partial_chars: int = 8,
) -> dict[str, Any]:
    """순위 순의 여러 segment를 per-document cap과 전체 cap 안에 포장한다.

    같은 원문은 처음 보인 doc에서만 예산을 쓴다 (``dedupe_visible``). 뒤 doc에서 그 원문은
    ``excluded.already_visible`` 로 남고 ``covered_by`` 가 앞 doc을 가리킨다. matched가 전부
    앞에서 보였으면 ``status=duplicate``. 전체 예산이 바닥나면 ``status=omitted``.
    ``doc_overhead_chars`` 는 호출자가 doc마다 붙일 헤더 길이다 (전체 cap에만 과금).
    """

    allowed_ids = None if allowed_observation_ids is None else {str(x) for x in allowed_observation_ids}
    rooms = None if allowed_rooms is None else {str(x) for x in allowed_rooms}
    required_by_doc = required_by_doc or {}
    claimed: dict[str, str] = {}  # observation_id -> doc_id that shows it
    docs: list[dict[str, Any]] = []
    used = 0
    omitted_docs: list[str] = []
    duplicate_docs: list[str] = []
    total = None if total_char_limit is None else int(total_char_limit)
    for seg in segments:
        doc_id = str(seg["doc_id"])
        budget = int(doc_char_limit)
        if total is not None:
            budget = min(budget, total - used - int(doc_overhead_chars))
        if budget <= 0:
            docs.append(
                {
                    "doc_id": doc_id,
                    "matched_leaf": doc_id,
                    "status": STATUS_OMITTED,
                    "insufficient_reason": "total_budget",
                    "excerpt": "",
                    "used_chars": 0,
                    "observation_ids": [],
                    "visible_observation_ids": [],
                    "visible_spans": [],
                    "leaf_observation_ids": list(seg.get("observation_ids") or []),
                }
            )
            omitted_docs.append(doc_id)
            continue
        packed = pack_segment_evidence(
            messages,
            seg,
            before=before,
            after=after,
            with_prefix=with_prefix,
            char_limit=budget,
            allowed_observation_ids=allowed_ids,
            allowed_rooms=rooms,
            time_allows=time_allows,
            required_observation_ids=required_by_doc.get(doc_id),
            already_visible=claimed.keys() if dedupe_visible else None,
            per_message_cap=per_message_cap,
            min_partial_chars=min_partial_chars,
        )
        covered = sorted({claimed[o] for o in packed["excluded"][EXCLUDE_ALREADY_VISIBLE] if o in claimed})
        packed["covered_by"] = covered
        leftover = total is not None and budget < int(doc_char_limit)
        if leftover and packed["status"] == STATUS_INSUFFICIENT and not packed["used_chars"]:
            packed["status"] = STATUS_OMITTED
            packed["insufficient_reason"] = "total_budget"
            packed["excerpt"] = ""
            packed["used_chars"] = 0
            packed["visible_spans"] = []
            packed["visible_observation_ids"] = []
            omitted_docs.append(doc_id)
        elif packed["status"] == STATUS_DUPLICATE:
            packed["excerpt"] = ""
            packed["used_chars"] = 0
            packed["visible_spans"] = []
            packed["visible_observation_ids"] = []
            duplicate_docs.append(doc_id)
        else:
            used += int(packed["used_chars"]) + (int(doc_overhead_chars) if packed["used_chars"] else 0)
            for oid in packed["visible_observation_ids"]:
                claimed.setdefault(oid, doc_id)
        docs.append(packed)
    visible_ids = [oid for d in docs for oid in d.get("visible_observation_ids") or []]
    flat_spans = [
        {**s, "doc_id": d["doc_id"]} for d in docs for s in (d.get("visible_spans") or [])
    ]
    return {
        "docs": docs,
        "doc_char_limit": int(doc_char_limit),
        "total_char_limit": total,
        "used_chars": used,
        "visible_observation_ids": visible_ids,
        "visible_spans": flat_spans,
        "omitted_doc_ids": omitted_docs,
        "duplicate_doc_ids": duplicate_docs,
    }


# ---------------------------------------------------------------- read-only audit


def _legacy_matched_loss(
    messages: Sequence[Mapping[str, Any]],
    segment: Mapping[str, Any],
    *,
    before: int,
    after: int,
    with_prefix: bool,
    char_limit: int,
) -> dict[str, Any]:
    """옛 expand_segment(body[:char_limit]) 기준으로 matched 본문 탈락 여부를 계산한다."""

    start, end = parent_window(messages, segment, before=before, after=after)
    chunk = [messages[i] for i in range(start, end)]
    body = format_body(list(chunk), with_prefix=with_prefix)
    over = len(body) > char_limit
    pos = len(_prefix_text(list(chunk), with_prefix))
    if pos:
        pos += 1
    seg_start, seg_end = int(segment["start"]), int(segment["end"])
    full = partial = lost = 0
    for i, row in enumerate(chunk, start):
        line = f"{row.get('actor_label') or '?'}: {row.get('content') or ''}"
        line_start, line_end = pos, pos + len(line)
        pos = line_end + 1
        if not (seg_start <= i < seg_end):
            continue
        if line_end <= char_limit:
            full += 1
        elif line_start < char_limit:
            partial += 1
        else:
            lost += 1
    return {
        "over_limit": over,
        "matched_full": full,
        "matched_partial": partial,
        "matched_lost": lost,
        "matched_n": seg_end - seg_start,
    }


def audit_expansion(
    messages: Sequence[Mapping[str, Any]],
    leaves: Sequence[Mapping[str, Any]],
    *,
    before: int,
    after: int,
    with_prefix: bool,
    char_limit: int,
) -> dict[str, Any]:
    """모든 leaf가 선택된다고 가정한 구조 집계. 본문·ID는 포함하지 않는다."""

    legacy = {"over_limit": 0, "any_matched_partial_or_lost": 0, "all_matched_lost": 0}
    new = {
        "status": {STATUS_COMPLETE: 0, STATUS_PARTIAL: 0, STATUS_INSUFFICIENT: 0},
        "context_complete": 0,
        "matched_complete_all": 0,
        "span_verify_failures": 0,
        "over_limit": 0,
    }
    content_of = {str(r.get("observation_id")): str(r.get("content") or "") for r in messages}
    for leaf in leaves:
        old = _legacy_matched_loss(
            messages, leaf, before=before, after=after, with_prefix=with_prefix, char_limit=char_limit
        )
        if old["over_limit"]:
            legacy["over_limit"] += 1
        if old["matched_partial"] or old["matched_lost"]:
            legacy["any_matched_partial_or_lost"] += 1
        if old["matched_n"] and old["matched_lost"] == old["matched_n"]:
            legacy["all_matched_lost"] += 1
        packed = pack_segment_evidence(
            messages, leaf, before=before, after=after, with_prefix=with_prefix, char_limit=char_limit
        )
        new["status"][packed["status"]] = new["status"].get(packed["status"], 0) + 1
        if packed["context_complete"]:
            new["context_complete"] += 1
        counts = packed["matched_counts"]
        if counts["visible_complete"] == counts["packable"]:
            new["matched_complete_all"] += 1
        if len(packed["excerpt"]) > char_limit:
            new["over_limit"] += 1
        if verify_visible_spans(content_of, packed):
            new["span_verify_failures"] += 1
    return {
        "params": {
            "before": before,
            "after": after,
            "with_prefix": with_prefix,
            "char_limit": char_limit,
        },
        "message_count": len(messages),
        "leaf_count": len(leaves),
        "legacy_body_truncation": legacy,
        "evidence_packing": new,
    }


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def _main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="read-only parent expansion audit (aggregates only)")
    ap.add_argument("--data-dir", required=True, help="index dir with messages.json / leaves.json")
    ap.add_argument("--out", required=True, help="output JSON path (isolated scratch run dir)")
    ap.add_argument("--before", type=int, default=12)
    ap.add_argument("--after", type=int, default=12)
    ap.add_argument("--char-limit", type=int, default=4000)
    ap.add_argument("--no-prefix", action="store_true")
    args = ap.parse_args(argv)
    data = Path(args.data_dir)
    msg_path, leaf_path = data / "messages.json", data / "leaves.json"
    messages = json.loads(msg_path.read_text(encoding="utf-8"))
    leaves = json.loads(leaf_path.read_text(encoding="utf-8"))
    report = audit_expansion(
        messages,
        leaves,
        before=args.before,
        after=args.after,
        with_prefix=not args.no_prefix,
        char_limit=args.char_limit,
    )
    report["input"] = {
        "messages_sha256": _sha256(msg_path),
        "leaves_sha256": _sha256(leaf_path),
    }
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({k: report[k] for k in ("leaf_count", "legacy_body_truncation", "evidence_packing")}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(_main())
