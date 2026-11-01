"""Deterministic, runtime-derived soft events over stable leaf records.

This module deliberately sits *above* the stable leaf index.  It does not read
the corpus, call a model, or manufacture a timestamp when one is missing.  A
soft event is only a grouping and ranking aid: its ``leaf_ids`` are the raw
evidence references and an optional summary is search text, never evidence.
"""

from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime
from types import MappingProxyType
from typing import Any


def _parse_timestamp(value: Any) -> float | None:
    """Parse an explicitly supplied timestamp; never infer one from position."""

    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        parsed = float(value)
        return parsed if math.isfinite(parsed) else None
    raw = str(value).strip()
    if not raw:
        return None
    try:
        if raw.endswith("Z"):
            raw = raw[:-1] + "+00:00"
        parsed = datetime.fromisoformat(raw).timestamp()
    except (TypeError, ValueError, OverflowError):
        return None
    return parsed if math.isfinite(parsed) else None


def _timestamp_pair(row: Mapping[str, Any]) -> tuple[float | None, float | None]:
    """Return explicit (start, end) timestamps, preserving unknown as ``None``."""

    start: float | None = None
    end: float | None = None
    for key in ("start_time", "first_time", "start_ts"):
        if key in row:
            start = _parse_timestamp(row.get(key))
            break
    for key in ("end_time", "last_time", "end_ts"):
        if key in row:
            end = _parse_timestamp(row.get(key))
            break
    if start is None and end is None:
        for key in ("parsed_time", "timestamp", "time"):
            if key in row:
                value = _parse_timestamp(row.get(key))
                return value, value
    return start, end


def _copy_row(row: Mapping[str, Any]) -> dict[str, Any]:
    # A shallow copy is intentional: event identity never consults the body,
    # and nested source values are not modified by this module.
    return dict(row)


def _leaf_id(row: Mapping[str, Any]) -> str:
    for key in ("leaf_id", "doc_id", "id"):
        value = row.get(key)
        if value is not None and str(value).strip():
            return str(value)
    raise ValueError("each leaf must have a non-empty leaf_id/doc_id/id")


def _room_id(row: Mapping[str, Any]) -> str:
    value = row.get("source_room_id", row.get("room_id"))
    if value is None or not str(value).strip():
        raise ValueError("each leaf must have a non-empty source_room_id")
    return str(value)


def _body_chars(row: Mapping[str, Any]) -> int:
    # Stable leaves use ``body``.  The fallbacks make this helper useful for a
    # small runtime adapter without ever putting the body in an event ID.
    for key in ("body", "text", "content"):
        if key in row:
            value = row.get(key)
            return len(value) if isinstance(value, str) else len(str(value or ""))
    return 0


def _finite_vector(value: Sequence[float]) -> tuple[float, ...]:
    try:
        out = tuple(float(item) for item in value)
    except (TypeError, ValueError):
        raise ValueError("leaf vectors must be finite numeric sequences") from None
    if not out or not all(math.isfinite(item) for item in out):
        raise ValueError("leaf vectors must be non-empty and finite")
    return out


@dataclass(frozen=True)
class SoftEventRecipe:
    """The complete deterministic grouping recipe used in an event ID."""

    version: str = "soft-event-v1"
    gap_seconds: float | None = 30.0 * 60.0
    max_leaves: int = 8
    max_chars: int = 4000
    min_leaves: int = 1

    def __post_init__(self) -> None:
        if not str(self.version).strip():
            raise ValueError("recipe version must be non-empty")
        if self.gap_seconds is not None:
            gap = float(self.gap_seconds)
            if not math.isfinite(gap) or gap < 0:
                raise ValueError("gap_seconds must be finite and non-negative or None")
            object.__setattr__(self, "gap_seconds", gap)
        max_leaves = int(self.max_leaves)
        min_leaves = int(self.min_leaves)
        max_chars = int(self.max_chars)
        if max_leaves <= 0 or max_chars <= 0 or min_leaves <= 0:
            raise ValueError("max_leaves, max_chars, and min_leaves must be positive")
        if min_leaves > max_leaves:
            raise ValueError("min_leaves must not exceed max_leaves")
        object.__setattr__(self, "version", str(self.version))
        object.__setattr__(self, "max_leaves", max_leaves)
        object.__setattr__(self, "max_chars", max_chars)
        object.__setattr__(self, "min_leaves", min_leaves)

    @classmethod
    def from_value(cls, value: SoftEventRecipe | Mapping[str, Any] | str | None) -> SoftEventRecipe:
        if value is None:
            return cls()
        if isinstance(value, cls):
            return value
        if isinstance(value, str):
            return cls(version=value)
        if not isinstance(value, Mapping):
            raise TypeError("recipe must be SoftEventRecipe, mapping, string, or None")
        gap = value.get("gap_seconds")
        if gap is None and "gap_minutes" in value:
            raw_minutes = value.get("gap_minutes")
            gap = None if raw_minutes is None else float(raw_minutes) * 60.0
        if gap is None and "gap_seconds" not in value and "gap_minutes" not in value:
            gap = cls.gap_seconds
        return cls(
            version=str(value.get("version", value.get("recipe", cls.version))),
            gap_seconds=gap,
            max_leaves=int(value.get("max_leaves", cls.max_leaves)),
            max_chars=int(value.get("max_chars", cls.max_chars)),
            min_leaves=int(value.get("min_leaves", cls.min_leaves)),
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "gap_seconds": self.gap_seconds,
            "max_leaves": self.max_leaves,
            "max_chars": self.max_chars,
            "min_leaves": self.min_leaves,
        }


def make_event_id(
    recipe: SoftEventRecipe | Mapping[str, Any] | str | None,
    room_id: str,
    leaf_ids: Iterable[str],
) -> str:
    """Make an ID from recipe, room, and ordered leaf IDs only.

    Body, summaries, vectors, timestamps, and object identity are intentionally
    absent.  The ordered membership is part of the identity because changing
    the event partition must invalidate the derived event.
    """

    normalized_recipe = SoftEventRecipe.from_value(recipe)
    if not str(room_id).strip():
        raise ValueError("event IDs require a non-empty room ID")
    members = tuple(str(leaf_id) for leaf_id in leaf_ids)
    if not members or any(not member for member in members):
        raise ValueError("event IDs require at least one non-empty leaf ID")
    payload = {
        "recipe": normalized_recipe.as_dict(),
        "room": str(room_id),
        "leaf_ids": list(members),
    }
    encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    digest = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    return f"soft-event:{digest}"


deterministic_event_id = make_event_id


@dataclass(frozen=True)
class SoftEventIndex:
    """Immutable input view over the already-built stable leaf index."""

    leaf_ids: tuple[str, ...]
    leaf_by_id: Mapping[str, Mapping[str, Any]]
    leaf_vectors: Mapping[str, Sequence[float]] = field(default_factory=dict)
    leaf_times: Mapping[str, Any] = field(default_factory=dict)
    leaf_summaries: Mapping[str, str] = field(default_factory=dict)

    def __post_init__(self) -> None:
        ids = tuple(str(leaf_id) for leaf_id in self.leaf_ids)
        if len(ids) != len(set(ids)):
            raise ValueError("leaf_ids must not contain duplicates")
        if any(not leaf_id for leaf_id in ids):
            raise ValueError("leaf_ids must be non-empty")
        raw_rows = dict(self.leaf_by_id)
        rows: dict[str, Mapping[str, Any]] = {}
        for leaf_id in ids:
            if leaf_id not in raw_rows:
                raise ValueError(f"leaf_by_id is missing leaf {leaf_id!r}")
            row = raw_rows[leaf_id]
            if not isinstance(row, Mapping):
                raise TypeError("leaf_by_id values must be mappings")
            actual_id = _leaf_id(row)
            if actual_id != leaf_id:
                raise ValueError("leaf_by_id key and leaf identity differ")
            # Copy the row so later caller mutation cannot alter this index.
            rows[leaf_id] = MappingProxyType(_copy_row(row))

        vectors = _normalize_vectors(ids, self.leaf_vectors)
        times = {
            str(key): (dict(value) if isinstance(value, Mapping) else value)
            for key, value in dict(self.leaf_times or {}).items()
        }
        summaries = {
            str(key): str(value)
            for key, value in dict(self.leaf_summaries or {}).items()
            if value is not None and str(value).strip()
        }
        object.__setattr__(self, "leaf_ids", ids)
        object.__setattr__(self, "leaf_by_id", MappingProxyType(rows))
        object.__setattr__(self, "leaf_vectors", MappingProxyType(vectors))
        object.__setattr__(self, "leaf_times", MappingProxyType(times))
        object.__setattr__(self, "leaf_summaries", MappingProxyType(summaries))

    @classmethod
    def from_leaves(
        cls,
        leaves: Sequence[Mapping[str, Any]],
        *,
        leaf_vectors: Mapping[str, Sequence[float]] | Sequence[Sequence[float]] | None = None,
        leaf_times: Mapping[str, Any] | None = None,
        leaf_summaries: Mapping[str, str] | None = None,
    ) -> SoftEventIndex:
        rows = [_copy_row(row) for row in leaves]
        ids = tuple(_leaf_id(row) for row in rows)
        return cls(
            leaf_ids=ids,
            leaf_by_id=dict(zip(ids, rows)),
            leaf_vectors={} if leaf_vectors is None else leaf_vectors,
            leaf_times=leaf_times or {},
            leaf_summaries=leaf_summaries or {},
        )

    @property
    def leaves(self) -> Mapping[str, Mapping[str, Any]]:
        """Compatibility/readability alias for ``leaf_by_id``."""

        return self.leaf_by_id

    @property
    def vectors(self) -> Mapping[str, Sequence[float]]:
        return self.leaf_vectors

    @property
    def summaries(self) -> Mapping[str, str]:
        return self.leaf_summaries


def _normalize_vectors(
    leaf_ids: Sequence[str],
    vectors: Mapping[str, Sequence[float]] | Sequence[Sequence[float]] | None,
) -> dict[str, tuple[float, ...]]:
    if vectors is None:
        return {}
    if isinstance(vectors, Mapping):
        if not vectors:
            return {}
        raw = {str(key): value for key, value in vectors.items()}
        missing = [leaf_id for leaf_id in leaf_ids if leaf_id not in raw]
        if missing:
            raise ValueError("leaf_vectors is missing a vector for a leaf")
        return {leaf_id: _finite_vector(raw[leaf_id]) for leaf_id in leaf_ids}
    try:
        if len(vectors) != len(leaf_ids):
            raise ValueError("sequence leaf_vectors must align with leaf_ids")
        return {
            leaf_id: _finite_vector(vectors[position])
            for position, leaf_id in enumerate(leaf_ids)
        }
    except TypeError:
        raise TypeError("leaf_vectors must be a mapping or aligned sequence") from None


def _coerce_index(
    index: SoftEventIndex | Mapping[str, Any] | Sequence[Mapping[str, Any]],
    *,
    leaf_vectors: Mapping[str, Sequence[float]] | Sequence[Sequence[float]] | None,
    leaf_times: Mapping[str, Any] | None,
    summaries: Mapping[str, str] | None,
) -> SoftEventIndex:
    if isinstance(index, SoftEventIndex):
        if leaf_vectors is None and leaf_times is None and summaries is None:
            return index
        return SoftEventIndex(
            leaf_ids=index.leaf_ids,
            leaf_by_id=index.leaf_by_id,
            leaf_vectors=index.leaf_vectors if leaf_vectors is None else leaf_vectors,
            leaf_times=index.leaf_times if leaf_times is None else leaf_times,
            leaf_summaries=index.leaf_summaries if summaries is None else summaries,
        )

    # The runtime engine exposes these fields on IndexBundle.  Duck-typing
    # keeps this pure module independent of engine.py and avoids an import
    # cycle while still allowing a caller to pass the existing stable index.
    if hasattr(index, "leaf_ids") and hasattr(index, "leaf_by_id"):
        object_ids = tuple(str(leaf_id) for leaf_id in getattr(index, "leaf_ids"))
        object_rows = getattr(index, "leaf_by_id")
        source_vectors = getattr(index, "leaf_vectors", None) if leaf_vectors is None else leaf_vectors
        source_times = getattr(index, "leaf_times", None)
        if source_times is None and getattr(index, "leaf_end_ts", None) is not None:
            source_times = {
                str(key): {"end_ts": value}
                for key, value in dict(getattr(index, "leaf_end_ts")).items()
            }
        source_summaries = getattr(index, "leaf_summaries", None)
        if source_summaries is None:
            summary_rows = getattr(index, "summary_by_id", None)
            if isinstance(summary_rows, Mapping):
                source_summaries = {
                    str(key): str(row.get("summary") or row.get("summary_text") or "")
                    for key, row in summary_rows.items()
                    if isinstance(row, Mapping)
                    and str(row.get("summary") or row.get("summary_text") or "").strip()
                }
        if summaries is not None:
            source_summaries = summaries
        return SoftEventIndex(
            leaf_ids=object_ids,
            leaf_by_id=object_rows,
            leaf_vectors={} if source_vectors is None else source_vectors,
            leaf_times=source_times or {},
            leaf_summaries=source_summaries or {},
        )

    if isinstance(index, Mapping):
        rows = index.get("leaf_by_id")
        if rows is None:
            rows = index.get("leaves")
        ids = index.get("leaf_ids")
        if rows is None:
            raise ValueError("index mapping must provide leaf_by_id or leaves")
        if isinstance(rows, Mapping):
            row_map = {str(key): _copy_row(row) for key, row in rows.items()}
            if ids is None:
                ids = tuple(row_map)
            ordered_ids = tuple(str(leaf_id) for leaf_id in ids)
            records = [row_map[leaf_id] for leaf_id in ordered_ids]
        else:
            records = list(rows)
            if ids is None:
                ids = tuple(_leaf_id(row) for row in records)
            ordered_ids = tuple(str(leaf_id) for leaf_id in ids)
        if len(ordered_ids) != len(records):
            raise ValueError("leaf_ids and leaves must have the same length")
        source_vectors = index.get("leaf_vectors") if leaf_vectors is None else leaf_vectors
        source_times = index.get("leaf_times")
        if source_times is None and index.get("leaf_end_ts") is not None:
            # ``IndexBundle.leaf_end_ts`` is explicitly an end timestamp.  Do
            # not turn it into a fabricated start timestamp for gap logic.
            source_times = {
                str(key): {"end_ts": value}
                for key, value in dict(index.get("leaf_end_ts") or {}).items()
            }
        if leaf_times is not None:
            source_times = leaf_times
        source_summaries = index.get("leaf_summaries")
        if source_summaries is None:
            source_summaries = index.get("summaries")
        if summaries is not None:
            source_summaries = summaries
        return SoftEventIndex(
            leaf_ids=ordered_ids,
            leaf_by_id=dict(zip(ordered_ids, records)),
            leaf_vectors={} if source_vectors is None else source_vectors,
            leaf_times=source_times or {},
            leaf_summaries=source_summaries or {},
        )

    records = list(index)
    return SoftEventIndex.from_leaves(
        records,
        leaf_vectors=leaf_vectors,
        leaf_times=leaf_times,
        leaf_summaries=summaries,
    )


def _leaf_times(index: SoftEventIndex, leaf_id: str) -> tuple[float | None, float | None]:
    if leaf_id in index.leaf_times:
        raw = index.leaf_times[leaf_id]
        if isinstance(raw, Mapping):
            return _timestamp_pair(raw)
        parsed = _parse_timestamp(raw)
        return parsed, parsed
    return _timestamp_pair(index.leaf_by_id[leaf_id])


def _event_vector(
    index: SoftEventIndex,
    leaf_ids: Sequence[str],
) -> tuple[float, ...] | None:
    if not index.leaf_vectors:
        return None
    vectors = [index.leaf_vectors[leaf_id] for leaf_id in leaf_ids]
    dimension = len(vectors[0])
    if any(len(vector) != dimension for vector in vectors):
        raise ValueError("all leaf vectors in an event must have the same dimension")
    mean = [sum(vector[position] for vector in vectors) / len(vectors) for position in range(dimension)]
    norm = math.sqrt(sum(value * value for value in mean))
    # A zero mean has no unit direction.  Returning no vector is explicit and
    # safer for callers than pretending that an unnormalised zero is usable.
    if norm == 0.0:
        return None
    return tuple(value / norm for value in mean)


@dataclass(frozen=True)
class SoftEvent:
    event_id: str
    source_room_id: str
    leaf_ids: tuple[str, ...]
    recipe_revision: str = ""
    vector: tuple[float, ...] | None = None
    summary_text: str = ""
    char_count: int = 0
    start_time: float | None = None
    end_time: float | None = None
    boundary_reason: str = "end"
    boundary_reasons: tuple[str, ...] = ()
    is_raw_evidence: bool = False

    def __post_init__(self) -> None:
        allowed = {"room", "gap", "max_leaves", "max_chars", "end"}
        reason = str(self.boundary_reason)
        reasons = tuple(str(value) for value in self.boundary_reasons) or (reason,)
        if reason not in allowed or any(value not in allowed for value in reasons):
            raise ValueError("boundary_reason must be room, gap, max_leaves, max_chars, or end")
        object.__setattr__(self, "recipe_revision", str(self.recipe_revision))
        object.__setattr__(self, "leaf_ids", tuple(str(leaf_id) for leaf_id in self.leaf_ids))
        object.__setattr__(self, "boundary_reason", reason)
        object.__setattr__(self, "boundary_reasons", reasons)

    @property
    def room_id(self) -> str:
        return self.source_room_id

    @property
    def search_text(self) -> str:
        """Derived summary text only; callers must retrieve raw leaves for evidence."""

        return self.summary_text

    @property
    def summary(self) -> str:
        return self.summary_text

    @property
    def raw_evidence_leaf_ids(self) -> tuple[str, ...]:
        return self.leaf_ids

    def as_dict(self) -> dict[str, Any]:
        return {
            "event_id": self.event_id,
            "source_room_id": self.source_room_id,
            "leaf_ids": list(self.leaf_ids),
            "recipe_revision": self.recipe_revision,
            "vector": list(self.vector) if self.vector is not None else None,
            "summary_text": self.summary_text,
            "char_count": self.char_count,
            "start_time": self.start_time,
            "end_time": self.end_time,
            "boundary_reason": self.boundary_reason,
            "boundary_reasons": list(self.boundary_reasons),
            "is_raw_evidence": False,
        }


@dataclass(frozen=True)
class SoftEventResult:
    recipe: SoftEventRecipe
    events: tuple[SoftEvent, ...]
    event_by_id: Mapping[str, SoftEvent]
    leaf_to_event_id: Mapping[str, str]
    leaf_ids: tuple[str, ...]

    def __post_init__(self) -> None:
        events = tuple(self.events)
        by_id = {event.event_id: event for event in events}
        if len(by_id) != len(events):
            raise ValueError("event IDs must be unique")
        leaf_to_event: dict[str, str] = {}
        ordered_leaf_ids: list[str] = []
        for event in events:
            for leaf_id in event.leaf_ids:
                if leaf_id in leaf_to_event:
                    raise ValueError("a leaf must belong to exactly one soft event")
                leaf_to_event[leaf_id] = event.event_id
                ordered_leaf_ids.append(leaf_id)
        object.__setattr__(self, "events", events)
        object.__setattr__(self, "event_by_id", MappingProxyType(by_id))
        object.__setattr__(self, "leaf_to_event_id", MappingProxyType(leaf_to_event))
        object.__setattr__(self, "leaf_ids", tuple(ordered_leaf_ids))

    @property
    def event_ids(self) -> tuple[str, ...]:
        return tuple(event.event_id for event in self.events)

    @property
    def recipe_revision(self) -> str:
        return self.recipe.version

    @property
    def event_vectors(self) -> Mapping[str, tuple[float, ...] | None]:
        return MappingProxyType({event.event_id: event.vector for event in self.events})

    def as_dict(self) -> dict[str, Any]:
        return {
            "recipe": self.recipe.as_dict(),
            "events": [event.as_dict() for event in self.events],
        }


def build_soft_events(
    index: SoftEventIndex | Mapping[str, Any] | Sequence[Mapping[str, Any]],
    *,
    recipe: SoftEventRecipe | Mapping[str, Any] | str | None = None,
    leaf_vectors: Mapping[str, Sequence[float]] | Sequence[Sequence[float]] | None = None,
    leaf_times: Mapping[str, Any] | None = None,
    summaries: Mapping[str, str] | None = None,
) -> SoftEventResult:
    """Build non-overlapping soft events in the supplied stable leaf order.

    Room changes always flush the current event.  A known timestamp gap at or
    above ``gap_seconds`` flushes only after ``min_leaves`` have accumulated;
    unknown or invalid timestamps never create a gap.  Length caps are enforced
    after the minimum, except a single overlong leaf is retained so coverage is
    never lost.
    """

    normalized_recipe = SoftEventRecipe.from_value(recipe)
    source = _coerce_index(
        index,
        leaf_vectors=leaf_vectors,
        leaf_times=leaf_times,
        summaries=summaries,
    )
    if not source.leaf_ids:
        return SoftEventResult(normalized_recipe, (), {}, {}, ())

    groups: list[tuple[str, tuple[str, ...], str, tuple[str, ...]]] = []
    current_room: str | None = None
    current_ids: list[str] = []
    current_chars = 0

    def flush(reason: str = "end", reasons: Sequence[str] = ()) -> None:
        nonlocal current_room, current_ids, current_chars
        if current_ids:
            all_reasons = tuple(str(value) for value in reasons) or (reason,)
            groups.append((str(current_room), tuple(current_ids), reason, all_reasons))
        current_room = None
        current_ids = []
        current_chars = 0

    for leaf_id in source.leaf_ids:
        row = source.leaf_by_id[leaf_id]
        room = _room_id(row)
        chars = _body_chars(row)
        if current_ids and room != current_room:
            flush("room")

        if not current_ids:
            current_room = room
            current_ids.append(leaf_id)
            current_chars = chars
            continue

        previous_id = current_ids[-1]
        previous_end = _leaf_times(source, previous_id)[1]
        current_start = _leaf_times(source, leaf_id)[0]
        known_gap = (
            previous_end is not None
            and current_start is not None
            and current_start - previous_end >= float(normalized_recipe.gap_seconds or 0.0)
            if normalized_recipe.gap_seconds is not None
            else False
        )
        at_max_leaves = len(current_ids) >= normalized_recipe.max_leaves
        over_chars = current_chars + chars > normalized_recipe.max_chars
        can_split_for_soft_rule = len(current_ids) >= normalized_recipe.min_leaves
        reasons: list[str] = []
        if at_max_leaves:
            reasons.append("max_leaves")
        if can_split_for_soft_rule and known_gap:
            reasons.append("gap")
        if can_split_for_soft_rule and over_chars:
            reasons.append("max_chars")
        if reasons:
            # Keep a single stable primary reason for simple consumers while
            # preserving simultaneous causes for diagnostics.
            primary = next(
                (value for value in ("max_leaves", "gap", "max_chars") if value in reasons),
                reasons[0],
            )
            flush(primary, reasons)
            current_room = room
            current_ids.append(leaf_id)
            current_chars = chars
        else:
            current_ids.append(leaf_id)
            current_chars += chars

    flush("end")

    events: list[SoftEvent] = []
    for room, member_ids, boundary_reason, boundary_reasons in groups:
        summary_parts = [source.leaf_summaries[leaf_id].strip() for leaf_id in member_ids if leaf_id in source.leaf_summaries]
        starts = [_leaf_times(source, leaf_id)[0] for leaf_id in member_ids]
        ends = [_leaf_times(source, leaf_id)[1] for leaf_id in member_ids]
        known_starts = [value for value in starts if value is not None]
        known_ends = [value for value in ends if value is not None]
        complete_start = len(known_starts) == len(starts)
        complete_end = len(known_ends) == len(ends)
        events.append(
            SoftEvent(
                event_id=make_event_id(normalized_recipe, room, member_ids),
                source_room_id=room,
                leaf_ids=member_ids,
                recipe_revision=normalized_recipe.version,
                vector=_event_vector(source, member_ids),
                summary_text="\n".join(summary_parts),
                char_count=sum(_body_chars(source.leaf_by_id[leaf_id]) for leaf_id in member_ids),
                # A partial interval would imply a boundary for an unknown
                # timestamp, so expose it as unknown instead of estimating it.
                start_time=min(known_starts) if complete_start and known_starts else None,
                end_time=max(known_ends) if complete_end and known_ends else None,
                boundary_reason=boundary_reason,
                boundary_reasons=boundary_reasons,
            )
        )

    covered = tuple(leaf_id for event in events for leaf_id in event.leaf_ids)
    if covered != source.leaf_ids or len(set(covered)) != len(covered):
        raise AssertionError("soft event construction lost or duplicated a stable leaf")
    return SoftEventResult(
        recipe=normalized_recipe,
        events=tuple(events),
        event_by_id={},
        leaf_to_event_id={},
        leaf_ids=covered,
    )


def _ranked_event_ids(ranks: Mapping[str, float] | Sequence[Any]) -> list[str]:
    if isinstance(ranks, Mapping):
        scored = [(str(event_id), float(score)) for event_id, score in ranks.items()]
        scored.sort(key=lambda item: (-item[1], item[0]))
        return [event_id for event_id, _ in scored]
    out: list[str] = []
    for item in ranks:
        if isinstance(item, Mapping):
            event_id = item.get("event_id", item.get("id"))
        elif isinstance(item, (tuple, list)) and item:
            event_id = item[0]
        else:
            event_id = item
        if event_id is not None:
            out.append(str(event_id))
    return out


def project_event_ranks(
    result_or_ranks: SoftEventResult | Mapping[str, float] | Sequence[Any],
    ranks_or_result: Mapping[str, float] | Sequence[Any] | SoftEventResult,
    *,
    top_k: int | None = None,
) -> tuple[str, ...]:
    """Project ranked event IDs back to stable leaf IDs.

    Both ``project_event_ranks(result, ranks)`` and
    ``project_event_ranks(ranks, result)`` are accepted.  A mapping of scores
    is sorted by descending score then event ID; an already ranked sequence is
    kept in order.  Unknown/duplicate event IDs are ignored, and leaves are
    emitted once in their stable event order.
    """

    if isinstance(result_or_ranks, SoftEventResult):
        result = result_or_ranks
        ranks = ranks_or_result
    elif isinstance(ranks_or_result, SoftEventResult):
        result = ranks_or_result
        ranks = result_or_ranks
    else:
        raise TypeError("one argument must be a SoftEventResult")
    if top_k is not None and int(top_k) < 0:
        raise ValueError("top_k must be non-negative or None")
    selected: list[str] = []
    seen_events: set[str] = set()
    for event_id in _ranked_event_ids(ranks):
        if event_id in seen_events or event_id not in result.event_by_id:
            continue
        seen_events.add(event_id)
        for leaf_id in result.event_by_id[event_id].leaf_ids:
            if leaf_id not in selected:
                selected.append(leaf_id)
                if top_k is not None and len(selected) >= int(top_k):
                    return tuple(selected)
    return tuple(selected)


def rank_events(
    result: SoftEventResult,
    scores: Mapping[str, float],
    *,
    top_k: int | None = None,
) -> tuple[str, ...]:
    """Deterministically rank event scores before projecting them to leaves."""

    ranked = _ranked_event_ids(scores)
    if top_k is not None:
        if int(top_k) < 0:
            raise ValueError("top_k must be non-negative or None")
        ranked = ranked[: int(top_k)]
    return tuple(event_id for event_id in ranked if event_id in result.event_by_id)


project_event_rank_to_leaf_ids = project_event_ranks
project_ranks = project_event_ranks
build = build_soft_events


__all__ = [
    "SoftEvent",
    "SoftEventIndex",
    "SoftEventRecipe",
    "SoftEventResult",
    "build",
    "build_soft_events",
    "deterministic_event_id",
    "make_event_id",
    "project_event_rank_to_leaf_ids",
    "project_event_ranks",
    "project_ranks",
    "rank_events",
]
