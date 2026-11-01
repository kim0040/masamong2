"""Retrieval post-processing shared by engine ranking paths (no network)."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone, tzinfo
from typing import Any, Iterable, Mapping

# Hybrid rank-list length defaults (overridable via config / _hybrid_rank args).
LEXICAL_LIMIT = 200
DENSE_LIMIT = 150
# Person heuristic raw score saturates around speaker 2.4 + 1.15*3 + paste.
PERSON_SCORE_SCALE = 6.0
RERANK_MIN_SCORE = 0.5


def search_limits(cfg: Mapping[str, Any] | None) -> tuple[int, int]:
    cfg = cfg or {}
    lex = int(cfg.get("lexical_limit") or LEXICAL_LIMIT)
    dense = int(cfg.get("dense_limit") or DENSE_LIMIT)
    return max(1, lex), max(1, dense)


def resolve_rerank_min_score(cfg: Mapping[str, Any] | None, model: str) -> float:
    raw = (cfg or {}).get("rerank_min_score", RERANK_MIN_SCORE)
    if raw is None:
        return float(RERANK_MIN_SCORE)
    if isinstance(raw, Mapping):
        if model and model in raw:
            return float(raw[model])
        if "_default" in raw:
            return float(raw["_default"])
        return float(RERANK_MIN_SCORE)
    return float(raw)


def scaled_person(raw: float) -> float:
    return float(raw) / PERSON_SCORE_SCALE


def min_gap_for(cfg: Mapping[str, Any], intent: Mapping[str, Any] | None) -> int:
    leaf_size = int((cfg or {}).get("leaf_size", 8) or 8)
    if intent and intent.get("recent"):
        return leaf_size * 3
    return max(4, leaf_size // 2)


def finalize_ranked_ids(
    index: Any,
    ranked: list[str],
    *,
    top_k: int,
    intent: Mapping[str, Any] | None = None,
    mode: str | None = None,
) -> tuple[list[str], dict[str, list[str]]]:
    """Adjacent-leaf skip or merge. Returns (picked ids, surviving→merged extras)."""

    cfg = getattr(index, "cfg", {}) or {}
    mode = str(mode if mode is not None else cfg.get("dedup_mode") or "merge")
    if mode in {"off", "none", ""}:
        return list(ranked[: max(0, top_k)]), {}
    min_gap = min_gap_for(cfg, intent)
    picked: list[str] = []
    picked_starts: list[int] = []
    merge_of: dict[str, list[str]] = {}
    skipped: list[tuple[str, int]] = []
    by_id = index.leaf_by_id
    for doc_id in ranked:
        leaf = by_id.get(doc_id) or {}
        start = int(leaf["start"]) if leaf.get("start") is not None else -10_000
        room = str(leaf.get("source_room_id") or "")
        mate_i = None
        for i, prev_id in enumerate(picked):
            prev = by_id.get(prev_id) or {}
            if str(prev.get("source_room_id") or "") != room:
                continue
            if abs(start - picked_starts[i]) < min_gap:
                mate_i = i
                break
        if mate_i is None:
            picked.append(doc_id)
            picked_starts.append(start)
            merge_of[doc_id] = []
            if mode != "merge" and len(picked) >= top_k:
                break
            continue
        skipped.append((doc_id, mate_i))
        if mode == "merge":
            merge_of[picked[mate_i]].append(doc_id)
        if mode != "merge" and len(picked) >= top_k:
            break
    if mode != "merge" and len(picked) < top_k:
        for doc_id, _ in skipped:
            if doc_id not in picked:
                picked.append(doc_id)
            if len(picked) >= top_k:
                break
        for doc_id in ranked:
            if doc_id not in picked:
                picked.append(doc_id)
            if len(picked) >= top_k:
                break
    if mode == "merge":
        picked = picked[: max(0, top_k)]
        merge_of = {k: merge_of.get(k, []) for k in picked}
    return picked[: max(0, top_k)], merge_of


def merged_span(index: Any, doc_id: str, extra_ids: list[str]) -> dict[str, Any]:
    leaf = dict(index.leaf_by_id[doc_id])
    ids = [doc_id, *extra_ids]
    starts = [int((index.leaf_by_id.get(i) or {}).get("start") or leaf["start"]) for i in ids]
    ends = [int((index.leaf_by_id.get(i) or {}).get("end") or leaf["end"]) for i in ids]
    leaf["start"] = min(starts)
    leaf["end"] = max(ends)
    return leaf


def recency_anchor_ts(index: Any) -> float:
    cfg = getattr(index, "cfg", {}) or {}
    if str(cfg.get("recency_anchor") or "corpus_max") == "now":
        return datetime.now(tz=timezone.utc).timestamp()
    return float(getattr(index, "corpus_tmax", 0.0) or 0.0)


def _narrow_recent_one(
    index: Any,
    universe: set[str],
    intent: Mapping[str, Any],
    *,
    scale: float = 1.0,
) -> tuple[set[str], dict[str, Any]]:
    days = float((index.cfg or {}).get("recent_window_days", 30) or 30) * max(1.0, float(scale))
    k = int(int((index.cfg or {}).get("recent_window_leaves", 200) or 200) * max(1.0, float(scale)))
    room_ts = [float(index.leaf_end_ts.get(doc_id) or 0.0) for doc_id in universe]
    anchor = max(room_ts) if room_ts else recency_anchor_ts(index)
    cutoff = anchor - max(days, 0.0) * 86400.0
    by_ts = [doc_id for doc_id in index.leaves_by_recency if doc_id in universe]
    fresh = [
        doc_id
        for doc_id in by_ts
        if float(index.leaf_end_ts.get(doc_id) or 0.0) >= cutoff
    ]
    window = set(fresh) if fresh else set(by_ts[: max(1, k)])
    if len(window) < min(k, len(by_ts)):
        window |= set(by_ts[:k])
    meta = {
        "recent_window_applied": True,
        "recent_window_days": days,
        "recent_window_leaves": k,
        "recent_pool_n": len(window),
        "recent_anchor_ts": anchor,
    }
    return window, meta


def _by_room(index: Any, universe: set[str]) -> dict[str, set[str]]:
    by_room: dict[str, set[str]] = {}
    leaf_by_id = getattr(index, "leaf_by_id", {}) or {}
    for doc_id in universe:
        room = str((leaf_by_id.get(doc_id) or {}).get("source_room_id") or "_")
        by_room.setdefault(room, set()).add(doc_id)
    return by_room


def narrow_recent_ids(
    index: Any,
    allowed: set[str] | None,
    intent: Mapping[str, Any] | None,
    *,
    scale: float = 1.0,
) -> tuple[set[str] | None, dict[str, Any]]:
    """Recent window is per source_room (block). Times from different rooms are not compared."""

    meta: dict[str, Any] = {"recent_window_applied": False}
    if not intent or not intent.get("recent"):
        return allowed, meta
    universe = allowed if allowed is not None else set(index.leaf_ids)
    by_room = _by_room(index, universe)
    if len(by_room) <= 1:
        window, meta = _narrow_recent_one(index, universe, intent, scale=scale)
        return window, meta
    window: set[str] = set()
    for group in by_room.values():
        part, _ = _narrow_recent_one(index, group, intent, scale=scale)
        window |= part
    meta = {
        "recent_window_applied": True,
        "recent_window_blocks": len(by_room),
        "recent_pool_n": len(window),
    }
    return window, meta


# --- v2 structured time constraint (planner_output.time) ---------------------------------
#
# Retrieval post-processing contract:
# - The caller injects the reference time (`TimeReference`): `live` = the request's wall clock,
#   `replay` = an explicit playback anchor. This module never derives "now" from the newest leaf.
# - `hard` kinds (today/yesterday/last_week/this_month/before/after/between) are scope. Recovery
#   `scale` never widens them; only the soft `recent` window may grow.
# - Naive (kakao, timezone unknown) and aware (discord, explicit offset) times are never compared
#   directly. A naive record can only be placed in an aware frame through an explicit
#   `TimeReference.naive_tz`; without it the record is `unresolved`, never silently included.
# - Missing reference / missing frame policy → `unresolved` with an empty window and metadata.
#   "No records in the period" (`time_records_in_range_n == 0`, applied) and "could not resolve"
#   (`time_unresolved`) are distinct.
# - Candidate inclusion uses the leaf's time *overlap* (any message in range); the final evidence
#   filter is per observation (`filter_observations`). A leaf that crosses midnight can still be a
#   candidate while its out-of-range messages are excluded from the evidence.

RELATIVE_TIME_KINDS = ("today", "yesterday", "last_week", "this_month")
ABSOLUTE_TIME_KINDS = ("before", "after", "between")
HARD_TIME_KINDS = RELATIVE_TIME_KINDS + ABSOLUTE_TIME_KINDS
SOFT_TIME_KINDS = ("recent",)

TIME_REFERENCE_MODES = ("live", "replay")


def _parse_iso_dt(value: Any) -> tuple[datetime, bool] | None:
    """ISO-8601 → (datetime, date_only). Naive stays naive; 'Z' → UTC. None on failure."""

    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        if len(text) == 10:
            return datetime.strptime(text, "%Y-%m-%d"), True
        return datetime.fromisoformat(text), False
    except ValueError:
        return None


def _parse_iso_ts(value: Any) -> float | None:
    """Legacy epoch helper kept for the soft path. Not used for hard bounds (frame-blind)."""

    parsed = _parse_iso_dt(value)
    if parsed is None:
        return None
    return parsed[0].timestamp()


def _frame_of(dt: datetime) -> str:
    return "aware" if dt.tzinfo is not None and dt.utcoffset() is not None else "naive"


@dataclass(frozen=True)
class TimeReference:
    """Caller-injected reference time.

    mode: "live" (request wall clock; `now` must be aware) or "replay" (explicit playback anchor
          passed by an evaluation/replay caller; aware or naive).
    tz:   zone for day boundaries (today/yesterday/…) and for naive planner dates. Defaults to
          `now.tzinfo`. Ignored when `now` is naive (day boundaries are then in the naive frame).
    naive_tz: declared zone for *records* whose time is naive (kakao). None means such records
          cannot be placed in an aware frame and become `unresolved`.
    source: trace label ("request", "eval:newest_record", …). Never a user/channel id."""

    mode: str
    now: datetime
    tz: tzinfo | None = None
    naive_tz: tzinfo | None = None
    source: str = ""

    def __post_init__(self) -> None:
        if self.mode not in TIME_REFERENCE_MODES:
            raise ValueError(f"time reference mode must be one of {TIME_REFERENCE_MODES}")
        if not isinstance(self.now, datetime):
            raise ValueError("time reference now must be a datetime")
        if self.mode == "live" and _frame_of(self.now) != "aware":
            raise ValueError("live time reference requires an aware datetime (explicit offset)")
        if _frame_of(self.now) == "aware" and self.tz is None:
            object.__setattr__(self, "tz", self.now.tzinfo)

    @property
    def frame(self) -> str:
        return _frame_of(self.now)

    def local_now(self) -> datetime:
        if self.frame == "aware" and self.tz is not None:
            return self.now.astimezone(self.tz)
        return self.now

    def to_trace(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "source": self.source,
            "frame": self.frame,
            "now": self.now.isoformat(),
            "tz": str(self.tz) if self.tz is not None else None,
            "naive_tz": str(self.naive_tz) if self.naive_tz is not None else None,
        }


TIME_SCOPE_STATUSES = ("none", "soft", "hard", "invalid", "unresolved")
OBSERVATION_TIME_STATUSES = ("in", "out", "unresolved", "missing", "unscoped")


@dataclass
class TimeScope:
    """Resolved time constraint. `lo` inclusive, `hi` exclusive, both in `frame`.

    status: none (no constraint) | soft (recent window, policy-widenable) | hard (fixed bounds) |
            invalid (planner bound unparseable) | unresolved (reference/frame policy missing)."""

    status: str
    kind: str
    lo: datetime | None = None
    hi: datetime | None = None
    frame: str = "naive"
    naive_tz: tzinfo | None = None
    reason: str = ""
    reversed: bool = False
    reference: dict[str, Any] | None = None

    @property
    def hard(self) -> bool:
        return self.status == "hard"

    @property
    def applied(self) -> bool:
        return self.status in {"hard", "soft"}

    def _to_frame(self, dt: datetime) -> datetime | None:
        f = _frame_of(dt)
        if f == self.frame:
            return dt
        if self.naive_tz is None:
            return None
        if f == "naive":  # naive record into aware frame
            return dt.replace(tzinfo=self.naive_tz)
        return dt.astimezone(self.naive_tz).replace(tzinfo=None)  # aware record into naive frame

    def contains(self, dt: datetime) -> bool | None:
        """True/False inside the hard bounds; None when the record cannot be placed in the frame."""

        local = self._to_frame(dt)
        if local is None:
            return None
        if self.lo is not None and local < self.lo:
            return False
        if self.hi is not None and local >= self.hi:
            return False
        return True

    def observation_status(self, parsed_time: Any) -> str:
        if not self.hard:
            return "unscoped"
        parsed = _parse_iso_dt(parsed_time)
        if parsed is None:
            return "missing"
        inside = self.contains(parsed[0])
        if inside is None:
            return "unresolved"
        return "in" if inside else "out"

    def to_trace(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "kind": self.kind,
            "lo": self.lo.isoformat() if self.lo is not None else None,
            "hi": self.hi.isoformat() if self.hi is not None else None,
            "hi_exclusive": True,
            "frame": self.frame,
            "naive_tz": str(self.naive_tz) if self.naive_tz is not None else None,
            "reason": self.reason,
            "reversed": self.reversed,
            "reference": self.reference,
        }


def _midnight(dt: datetime) -> datetime:
    return dt.replace(hour=0, minute=0, second=0, microsecond=0)


def _bound_in_frame(parsed: tuple[datetime, bool], reference: TimeReference | None) -> datetime | None:
    """Planner date/datetime → reference frame. Naive planner dates are read in `reference.tz`
    (the request's zone). An aware bound against a naive reference needs `naive_tz`.
    With no reference, the bound stays in its own frame (absolute kinds)."""

    dt, _date_only = parsed
    if reference is None:
        return dt
    ref_frame = reference.frame
    if _frame_of(dt) == ref_frame:
        return dt
    if ref_frame == "aware":
        assert reference is not None
        return dt.replace(tzinfo=reference.tz)
    if reference is not None and reference.naive_tz is not None:
        return dt.astimezone(reference.naive_tz).replace(tzinfo=None)
    return None


def resolve_time_scope(
    time: Mapping[str, Any] | None,
    *,
    reference: TimeReference | None = None,
    recent: bool = False,
) -> TimeScope:
    """planner `time` {kind, from, to} (+ v1 `recent`) → TimeScope. Pure; no wall clock."""

    ref_trace = reference.to_trace() if reference is not None else None
    frame = reference.frame if reference is not None else "naive"
    naive_tz = reference.naive_tz if reference is not None else None
    kind = str((time or {}).get("kind") or "").strip() if isinstance(time, Mapping) else ""
    if not kind:
        kind = "recent" if recent else "any"
    if kind == "any":
        return TimeScope("none", kind, reference=ref_trace)
    if kind in SOFT_TIME_KINDS:
        return TimeScope("soft", kind, reference=ref_trace)
    if kind not in HARD_TIME_KINDS:
        return TimeScope("invalid", kind, reason="unknown_kind", reference=ref_trace)

    if kind in RELATIVE_TIME_KINDS:
        if reference is None:
            return TimeScope("unresolved", kind, reason="reference_time_missing", reference=None)
        now = reference.local_now()
        day0 = _midnight(now)
        day1 = day0 + timedelta(days=1)
        if kind == "today":
            lo, hi = day0, day1
        elif kind == "yesterday":
            lo, hi = day0 - timedelta(days=1), day0
        elif kind == "last_week":
            lo, hi = day0 - timedelta(days=7), day1
        else:  # this_month
            lo, hi = day0.replace(day=1), day1
        return TimeScope("hard", kind, lo, hi, frame=frame, naive_tz=naive_tz, reference=ref_trace)

    frm_raw = (time or {}).get("from") if isinstance(time, Mapping) else None
    to_raw = (time or {}).get("to") if isinstance(time, Mapping) else None
    frm_p = _parse_iso_dt(frm_raw)
    to_p = _parse_iso_dt(to_raw)
    lo: datetime | None = None
    hi: datetime | None = None
    reversed_ = False
    if kind == "before":
        bound = to_p or frm_p
        if bound is None:
            return TimeScope("invalid", kind, reason="bound_unparseable", reference=ref_trace)
        hi = _bound_in_frame(bound, reference)  # date-only D → strictly before day D
        if hi is None:
            return TimeScope("unresolved", kind, reason="bound_frame_mismatch", reference=ref_trace)
    elif kind == "after":
        bound = frm_p or to_p
        if bound is None:
            return TimeScope("invalid", kind, reason="bound_unparseable", reference=ref_trace)
        lo = _bound_in_frame(bound, reference)  # date-only D → from day D 00:00 inclusive
        if lo is None:
            return TimeScope("unresolved", kind, reason="bound_frame_mismatch", reference=ref_trace)
    else:  # between
        if frm_p is None or to_p is None:
            return TimeScope("invalid", kind, reason="bound_unparseable", reference=ref_trace)
        if _frame_of(frm_p[0]) != _frame_of(to_p[0]):
            return TimeScope("invalid", kind, reason="bound_frames_differ", reference=ref_trace)
        if frm_p[0] > to_p[0]:
            frm_p, to_p = to_p, frm_p
            reversed_ = True
        lo = _bound_in_frame(frm_p, reference)
        hi_dt, hi_date_only = to_p
        hi = _bound_in_frame((hi_dt + timedelta(days=1) if hi_date_only else hi_dt, hi_date_only), reference)
        if lo is None or hi is None:
            return TimeScope("unresolved", kind, reason="bound_frame_mismatch", reference=ref_trace)
    if reference is None:
        probe = lo or hi
        if probe is not None:
            frame = _frame_of(probe)
    return TimeScope("hard", kind, lo, hi, frame=frame, naive_tz=naive_tz, reversed=reversed_, reference=ref_trace)


def time_bounds(
    kind: str,
    *,
    anchor_ts: float,
    frm: Any = None,
    to: Any = None,
    scale: float = 1.0,
) -> tuple[float | None, float | None] | None:
    """Replay helper used by tests: hard window as epoch seconds.

    ``scale`` is accepted for call-site compatibility and **ignored** for hard kinds
    Recovery must not widen yesterday/between/last_week.
    """

    del scale
    now = datetime.fromtimestamp(float(anchor_ts))
    ref = TimeReference(mode="replay", now=now, source="anchor_ts")
    scope = resolve_time_scope({"kind": kind, "from": frm, "to": to}, reference=ref)
    if scope.status == "invalid":
        return None
    if scope.status != "hard":
        return None
    lo = scope.lo.timestamp() if scope.lo is not None else None
    hi = scope.hi.timestamp() if scope.hi is not None else None
    return lo, hi


# --- observation / leaf time membership --------------------------------------------------------


def _message_index_by_observation(index: Any) -> dict[str, int]:
    cached = getattr(index, "_t04_msg_index_by_obs", None)
    if cached is not None:
        return cached
    table: dict[str, int] = {}
    for i, row in enumerate(getattr(index, "messages", None) or []):
        oid = str(row.get("observation_id") or "")
        if oid and oid not in table:
            table[oid] = i
    try:
        index._t04_msg_index_by_obs = table
    except AttributeError:
        pass
    return table


@dataclass
class ObservationTimeFilter:
    """Per-observation hard-time verdicts. `hard=False` means no hard scope: everything allowed."""

    hard: bool
    allowed: list[str] = field(default_factory=list)
    excluded: list[str] = field(default_factory=list)
    unresolved: list[str] = field(default_factory=list)
    missing: list[str] = field(default_factory=list)

    def to_trace(self) -> dict[str, Any]:
        return {
            "hard": self.hard,
            "allowed_n": len(self.allowed),
            "excluded_n": len(self.excluded),
            "unresolved_n": len(self.unresolved),
            "missing_n": len(self.missing),
        }


def filter_observations(index: Any, observation_ids: Iterable[str], scope: TimeScope) -> ObservationTimeFilter:
    """Final evidence filter (03/08): which observation ids may appear as evidence under `scope`.
    Ids not present in `index.messages` count as `missing` (never allowed under a hard scope)."""

    ids = [str(x) for x in observation_ids]
    if not scope.hard:
        return ObservationTimeFilter(hard=False, allowed=ids)
    out = ObservationTimeFilter(hard=True)
    by_obs = _message_index_by_observation(index)
    messages = getattr(index, "messages", None) or []
    for oid in ids:
        i = by_obs.get(oid)
        if i is None or i >= len(messages):
            out.missing.append(oid)
            continue
        status = scope.observation_status(messages[i].get("parsed_time"))
        if status == "in":
            out.allowed.append(oid)
        elif status == "out":
            out.excluded.append(oid)
        elif status == "unresolved":
            out.unresolved.append(oid)
        else:
            out.missing.append(oid)
    return out


def leaf_time_overlap(index: Any, doc_id: str, scope: TimeScope) -> dict[str, Any]:
    """Candidate-level check: does any message of the leaf fall inside the hard scope?
    Uses every message in [start, end), not just the leaf's end time."""

    leaf = (getattr(index, "leaf_by_id", None) or {}).get(doc_id) or {}
    messages = getattr(index, "messages", None) or []
    start = int(leaf.get("start") or 0)
    end = int(leaf.get("end") or 0)
    counts = {"in": 0, "out": 0, "unresolved": 0, "missing": 0}
    if not scope.hard:
        return {"overlaps": True, "hard": False, **counts}
    if 0 <= start < end <= len(messages):
        for row in messages[start:end]:
            counts[scope.observation_status(row.get("parsed_time"))] += 1
    else:
        counts["missing"] += max(1, end - start)
    return {"overlaps": counts["in"] > 0, "hard": True, **counts}


def narrow_time_scope(
    index: Any,
    allowed: set[str] | None,
    scope: TimeScope,
    *,
    scale: float = 1.0,
    intent: Mapping[str, Any] | None = None,
) -> tuple[set[str] | None, dict[str, Any]]:
    """Apply a resolved TimeScope to the candidate universe. Hard scopes ignore `scale`."""

    base_meta: dict[str, Any] = {
        "recent_window_applied": False,
        "time_window_applied": False,
        "time_kind": scope.kind,
        "time_window_hard": scope.hard,
        "time_scope": scope.to_trace(),
    }
    if scope.status == "none":
        return allowed, base_meta
    if scope.status == "soft":
        soft_intent = dict(intent or {})
        soft_intent["recent"] = True
        window, meta = narrow_recent_ids(index, allowed, soft_intent, scale=scale)
        meta.update(base_meta)
        meta.update({"time_window_applied": True, "time_window_scale": scale, "time_window_scale_applied": scale > 1.0,
                     "recent_anchor_source": "room_newest_leaf"})
        return window, meta
    if scope.status == "invalid":
        # Planner bound unusable. Scope stays as the caller gave it (not widened past `allowed`)
        # and the caller must read `time_window_invalid` — it is not a time-filtered result.
        base_meta.update({"time_window_invalid": True, "time_window_invalid_reason": scope.reason})
        return allowed, base_meta
    if scope.status == "unresolved":
        # Reference/frame policy missing: nothing can be a candidate. Not "no records in period".
        base_meta.update({"time_unresolved": True, "time_unresolved_reason": scope.reason, "time_pool_n": 0})
        return set(), base_meta

    universe = allowed if allowed is not None else set(index.leaf_ids)
    window: set[str] = set()
    records_in = 0
    leaves_unresolved = 0
    leaves_missing = 0
    leaves_out = 0
    for doc_id in universe:
        ov = leaf_time_overlap(index, doc_id, scope)
        if ov["overlaps"]:
            window.add(doc_id)
            records_in += ov["in"]
        elif ov["unresolved"] > 0:
            leaves_unresolved += 1
        elif ov["out"] > 0:
            leaves_out += 1
        else:
            leaves_missing += 1
    base_meta.update({
        "time_window_applied": True,
        "time_pool_n": len(window),
        "time_records_in_range_n": records_in,
        "time_leaves_out_n": leaves_out,
        "time_leaves_unresolved_n": leaves_unresolved,
        "time_leaves_missing_n": leaves_missing,
        "time_window_scale": scale,
        "time_window_scale_applied": False,
        "time_from": scope.lo.isoformat() if scope.lo is not None else None,
        "time_to": scope.hi.isoformat() if scope.hi is not None else None,
    })
    if not window and leaves_unresolved and not records_in:
        base_meta["time_unresolved_partial"] = True  # in-scope leaves exist but none could be placed
    return window, base_meta


def narrow_time_ids(
    index: Any,
    allowed: set[str] | None,
    intent: Mapping[str, Any] | None,
    *,
    scale: float = 1.0,
    reference: TimeReference | None = None,
) -> tuple[set[str] | None, dict[str, Any]]:
    """Scope the universe by the planner's `time` constraint (v2) or `recent` (v1).

    Scope comes *before* ranking: nothing outside the window is a candidate. Relative kinds need a
    caller-injected `reference` (live request time or explicit replay anchor); without one they
    are `unresolved` (empty window + `time_unresolved`), never anchored on the newest leaf."""

    time = (intent or {}).get("time") if intent else None
    recent = bool(intent and intent.get("recent"))
    scope = resolve_time_scope(time if isinstance(time, Mapping) else None, reference=reference, recent=recent)
    if scope.status == "none" and not recent:
        return allowed, {"recent_window_applied": False, "time_window_applied": False,
                         **({"time_kind": scope.kind} if isinstance(time, Mapping) else {})}
    if scope.status == "soft":
        window, meta = narrow_recent_ids(index, allowed, intent, scale=scale)
        meta.update({"time_window_applied": True, "time_kind": "recent", "time_window_hard": False,
                     "time_window_scale": scale, "time_window_scale_applied": scale > 1.0,
                     "recent_anchor_source": "room_newest_leaf"})
        return window, meta
    return narrow_time_scope(index, allowed, scope, scale=scale, intent=intent)
