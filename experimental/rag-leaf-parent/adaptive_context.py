"""Small, source-preserving context chunking helpers for the RAG experiment.

This module deliberately has no dependency on the playground engine.  It works
on the original message rows and keeps source-room/sample-block boundaries
explicit.  Adaptive compaction changes only the presentation body; the raw
message indices and observation ids remain complete.
"""

from __future__ import annotations

from datetime import datetime, timezone
import re
from typing import Any, Iterable


_MODES = {"fixed20": 20, "fixed8": 8, "adaptive": None}
_CORRECTION_RE = re.compile(
    r"(?:아니|아님|아니다|아니고|아닌|정정|수정|오타|잘못|취소|\bnot\b|\bcorrection\b)",
    re.IGNORECASE,
)
_ACKS = {
    "네",
    "넵",
    "넹",
    "응",
    "ㅇㅇ",
    "ㅇㅋ",
    "ok",
    "OK",
    "확인",
    "알겠어",
    "알겠습니다",
}
_REACTION_RE = re.compile(r"^[\sㅋㅎㅠㅜ!！.,。~～……♡♥❤😂🤣:;；()（）]+$")


def _room(row: dict[str, Any]) -> str:
    """Return the original room namespace, never a logical merged room."""

    original = row.get("source_room_original")
    if original is None or str(original) == "":
        original = row.get("source_room_id", "")
    return str(original)


def _block(row: dict[str, Any]) -> str:
    value = row.get("sample_block")
    return "" if value is None else str(value)


def _content(row: dict[str, Any]) -> str:
    value = row.get("content")
    return "" if value is None else str(value)


def _label(row: dict[str, Any]) -> str:
    value = row.get("actor_label")
    return "?" if value is None or str(value) == "" else str(value)


def _observation_id(row: dict[str, Any]) -> str:
    value = row.get("observation_id")
    return "" if value is None else str(value)


def _line(index: int, row: dict[str, Any], *, suffix: str = "") -> str:
    return f"[{index}] {_label(row)}: {_content(row)}{suffix}"


def _parsed_time(value: Any) -> datetime | float | None:
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        return float(value)
    text = str(value).strip()
    if not text:
        return None
    try:
        return float(text)
    except ValueError:
        pass
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed


def _time_gap_seconds(before: Any, after: Any) -> float | None:
    """Compare compatible timestamp kinds without inventing a timezone."""

    left = _parsed_time(before)
    right = _parsed_time(after)
    if isinstance(left, datetime) and isinstance(right, datetime):
        left_aware = left.tzinfo is not None and left.utcoffset() is not None
        right_aware = right.tzinfo is not None and right.utcoffset() is not None
        if left_aware != right_aware:
            return None
        if left_aware:
            left = left.astimezone(timezone.utc)
            right = right.astimezone(timezone.utc)
        return (right - left).total_seconds()
    if isinstance(left, (int, float)) and isinstance(right, (int, float)):
        return float(right - left)
    return None


def _same_actor(left: dict[str, Any], right: dict[str, Any]) -> bool:
    """Use an account id when present; labels are only a display hint."""

    la = left.get("actor_account")
    ra = right.get("actor_account")
    l_has = la is not None and str(la) != ""
    r_has = ra is not None and str(ra) != ""
    if l_has or r_has:
        return l_has and r_has and str(la) == str(ra)
    ll = left.get("actor_label")
    rl = right.get("actor_label")
    return ll is not None and rl is not None and str(ll) == str(rl)


def _is_ack(content: str) -> bool:
    return content.strip() in _ACKS


def _is_reaction(content: str) -> bool:
    stripped = content.strip()
    return bool(stripped) and bool(_REACTION_RE.fullmatch(stripped))


def _is_correction(content: str) -> bool:
    return bool(_CORRECTION_RE.search(content))


def _is_ambiguous_punctuation(content: str) -> bool:
    return content.strip() in {"?", "？"}


def _iter_runs(messages: list[dict[str, Any]]) -> Iterable[tuple[int, int, str, str]]:
    """Yield contiguous original-room/sample-block runs.

    Treating a later occurrence of the same key as a new run avoids bridging
    over interleaved source streams while retaining the input order.
    """

    if not messages:
        return
    start = 0
    current = (_room(messages[0]), _block(messages[0]))
    for index in range(1, len(messages)):
        key = (_room(messages[index]), _block(messages[index]))
        if key != current:
            yield start, index, current[0], current[1]
            start = index
            current = key
    yield start, len(messages), current[0], current[1]


def _presentation_groups(
    messages: list[dict[str, Any]], indices: list[int]
) -> list[tuple[list[int], str]]:
    """Return ``(source_indices, suffix)`` groups for adaptive body output."""

    groups: list[tuple[list[int], str]] = []
    pos = 0
    while pos < len(indices):
        first_index = indices[pos]
        first = messages[first_index]
        content = _content(first)

        # Short acknowledgements and corrections stay as individual lines.
        # This keeps a brief confirmation or a negation/correction visible.
        if _is_ack(content) or _is_correction(content) or _is_ambiguous_punctuation(content):
            groups.append(([first_index], ""))
            pos += 1
            continue

        same = [first_index]
        next_pos = pos + 1
        while next_pos < len(indices):
            candidate_index = indices[next_pos]
            candidate = messages[candidate_index]
            if (
                candidate_index != same[-1] + 1
                or _content(candidate) != content
                or not _same_actor(first, candidate)
            ):
                break
            if (
                _is_ack(_content(candidate))
                or _is_correction(_content(candidate))
                or _is_ambiguous_punctuation(_content(candidate))
            ):
                break
            same.append(candidate_index)
            next_pos += 1
        if len(same) > 1:
            groups.append((same, f" (repeat x{len(same)})"))
            pos = next_pos
            continue

        # A burst of pure laughter/punctuation may contain slightly different
        # reaction strings or labels.  It is presentation-only compaction;
        # provenance remains in the group's full index list.
        if _is_reaction(content):
            burst = [first_index]
            next_pos = pos + 1
            while next_pos < len(indices):
                candidate_index = indices[next_pos]
                candidate_content = _content(messages[candidate_index])
                if (
                    candidate_index != burst[-1] + 1
                    or not _is_reaction(candidate_content)
                    or _is_ambiguous_punctuation(candidate_content)
                    or not _same_actor(first, messages[candidate_index])
                ):
                    break
                burst.append(candidate_index)
                next_pos += 1
            if len(burst) > 1:
                groups.append((burst, f" (reaction burst x{len(burst)})"))
                pos = next_pos
                continue

        groups.append(([first_index], ""))
        pos += 1
    return groups


def _body(
    messages: list[dict[str, Any]],
    indices: list[int],
    *,
    compact: bool,
    oversized_indices: set[int] | None = None,
) -> str:
    oversized_indices = oversized_indices or set()
    if not compact:
        lines = []
        for index in indices:
            suffix = " [oversized]" if index in oversized_indices else ""
            lines.append(_line(index, messages[index], suffix=suffix))
        return "\n".join(lines)

    lines: list[str] = []
    for group, suffix in _presentation_groups(messages, indices):
        index = group[0]
        oversized = " [oversized]" if index in oversized_indices else ""
        lines.append(_line(index, messages[index], suffix=f"{suffix}{oversized}"))
    return "\n".join(lines)


def _useful_chars(messages: list[dict[str, Any]], indices: list[int]) -> int:
    """Approximate adaptive budget using visible content and repeat markers."""

    total = 0
    for group, suffix in _presentation_groups(messages, indices):
        total += len(_content(messages[group[0]])) + len(suffix)
    return total


def _make_chunk(
    messages: list[dict[str, Any]],
    *,
    mode: str,
    sequence: int,
    room: str,
    block: str,
    indices: list[int],
    compact: bool,
    max_chars: int | None = None,
) -> dict[str, Any]:
    oversized_indices = {
        index for index in indices if max_chars is not None and len(_content(messages[index])) > max_chars
    }
    reps = (
        [group[0][0] for group in _presentation_groups(messages, indices)]
        if compact
        else list(indices)
    )
    return {
        "doc_id": f"{mode}:{sequence}",
        "room": room,
        "block": block,
        "start": indices[0],
        "end": indices[-1] + 1,
        "body": _body(
            messages,
            indices,
            compact=compact,
            oversized_indices=oversized_indices,
        ),
        "message_indices": list(indices),
        "representative_indices": reps,
        "all_observation_ids": [_observation_id(messages[index]) for index in indices],
        "oversized_indices": sorted(oversized_indices),
    }


def _adaptive_runs(
    messages: list[dict[str, Any]],
    start: int,
    end: int,
    *,
    idle_seconds: float,
    target_chars: int,
    max_chars: int,
    max_messages: int,
    max_raw_messages: int,
) -> list[list[int]]:
    chunks: list[list[int]] = []
    current: list[int] = []
    current_useful = 0
    for index in range(start, end):
        row = messages[index]
        gap = None
        if current:
            previous = messages[current[-1]]
            gap = _time_gap_seconds(
                previous.get("parsed_time"), row.get("parsed_time")
            )

        if current and (gap is not None and gap > idle_seconds):
            chunks.append(current)
            current = []
            current_useful = 0

        proposed = current + [index]
        proposed_groups = _presentation_groups(messages, proposed)
        if current and (
            len(proposed) > max_raw_messages
            or len(proposed_groups) > max_messages
        ):
            chunks.append(current)
            current = []
            current_useful = 0
            proposed = [index]
            proposed_groups = _presentation_groups(messages, proposed)
        proposed_useful = _useful_chars(messages, proposed)
        if current and (
            proposed_useful > max_chars
            or (current_useful >= target_chars and proposed_useful > current_useful)
        ):
            chunks.append(current)
            current = []
            current_useful = 0
            proposed = [index]
            proposed_useful = _useful_chars(messages, proposed)

        current = proposed
        current_useful = proposed_useful
        # An oversized message is intentionally kept intact in its own chunk.
        if len(_content(row)) > max_chars:
            if current:
                chunks.append(current)
            current = []
            current_useful = 0

    if current:
        chunks.append(current)
    return chunks


def build_chunks(
    messages: list[dict[str, Any]],
    mode: str,
    *,
    idle_seconds: float = 300,
    target_chars: int = 600,
    max_chars: int = 1200,
    max_messages: int = 40,
    max_raw_messages: int = 400,
) -> list[dict[str, Any]]:
    """Build fixed or adaptive chunks without changing the input rows.

    ``start`` is inclusive and ``end`` is exclusive.  A missing
    ``sample_block`` is represented by the empty string.  Every input row is
    present in exactly one chunk's ``message_indices`` and
    ``all_observation_ids``; adaptive compaction only reduces body lines and
    ``representative_indices``.  In adaptive mode ``max_messages`` counts
    displayed presentation groups, while ``max_raw_messages`` is an
    independent raw-row safety cap.
    """

    if mode not in _MODES:
        raise ValueError(f"unsupported mode: {mode}")
    if (
        target_chars <= 0
        or max_chars <= 0
        or max_messages <= 0
        or max_raw_messages <= 0
    ):
        raise ValueError(
            "target_chars, max_chars, max_messages, and max_raw_messages must be positive"
        )
    if idle_seconds < 0:
        raise ValueError("idle_seconds must be non-negative")
    if not isinstance(messages, list):
        raise TypeError("messages must be a list")

    chunks: list[dict[str, Any]] = []
    sequence = 0
    fixed_size = _MODES[mode]
    for run_start, run_end, room, block in _iter_runs(messages):
        if mode in {"fixed20", "fixed8"}:
            assert fixed_size is not None
            groups = [
                list(range(index, min(index + fixed_size, run_end)))
                for index in range(run_start, run_end, fixed_size)
            ]
        else:
            groups = _adaptive_runs(
                messages,
                run_start,
                run_end,
                idle_seconds=float(idle_seconds),
                target_chars=min(target_chars, max_chars),
                max_chars=max_chars,
                max_messages=max_messages,
                max_raw_messages=max_raw_messages,
            )
        for indices in groups:
            if not indices:
                continue
            chunks.append(
                _make_chunk(
                    messages,
                    mode=mode,
                    sequence=sequence,
                    room=room,
                    block=block,
                    indices=indices,
                    compact=mode == "adaptive",
                    max_chars=max_chars if mode == "adaptive" else None,
                )
            )
            sequence += 1
    return chunks


def pack_context(
    messages: list[dict[str, Any]],
    chunks: list[dict[str, Any]],
    ranked_ids: list[str],
    *,
    budget_chars: int = 4000,
    expand_messages: int = 4,
) -> dict[str, Any]:
    """Greedily pack complete, visible source-message lines.

    Expansion follows the original input order within the selected chunk's
    room and sample block.  A line that cannot fit is skipped as a whole; it is
    never truncated and its observation id is not reported as visible.
    """

    if budget_chars < 0 or expand_messages < 0:
        raise ValueError("budget_chars and expand_messages must be non-negative")
    if not isinstance(messages, list):
        raise TypeError("messages must be a list")
    by_id = {str(chunk.get("doc_id")): chunk for chunk in chunks}
    # (representative input index, complete displayed line, source indices
    # represented by that line, visible observation ids) in source order.
    selected_units: list[tuple[int, str, list[int], list[str]]] = []
    consumed_indices: set[int] = set()
    selected_docs: list[str] = []
    used_chars = 0

    for ranked_id in ranked_ids:
        doc_id = str(ranked_id)
        chunk = by_id.get(doc_id)
        if chunk is None:
            continue
        raw_indices = [
            int(i)
            for i in (chunk.get("message_indices") or [])
            if 0 <= int(i) < len(messages)
        ]
        if not raw_indices:
            continue
        room = str(chunk.get("room", ""))
        block = str(chunk.get("block", ""))
        eligible = [
            index
            for index, row in enumerate(messages)
            if _room(row) == room and _block(row) == block
        ]
        positions = [eligible.index(index) for index in raw_indices if index in eligible]
        if not positions:
            continue
        lo = max(0, min(positions) - expand_messages)
        hi = min(len(eligible), max(positions) + expand_messages + 1)
        candidate_indices = eligible[lo:hi]

        newly_added = False
        # Adaptive chunks keep presentation compaction when expanded; fixed
        # chunks expose every source message as a complete line.
        adaptive = str(chunk.get("doc_id", "")).startswith("adaptive:")
        pending = [index for index in candidate_indices if index not in consumed_indices]
        display_groups = (
            _presentation_groups(messages, pending)
            if adaptive
            else [([index], "") for index in pending]
        )
        oversized_indices = {int(i) for i in (chunk.get("oversized_indices") or [])}
        for group, marker in display_groups:
            if not group:
                continue
            representative = group[0]
            suffix = marker
            if representative in oversized_indices:
                suffix += " [oversized]"
            line = _line(representative, messages[representative], suffix=suffix)
            needed = len(line) + (1 if selected_units else 0)
            if used_chars + needed > budget_chars:
                continue
            selected_units.append(
                (
                    representative,
                    line,
                    list(group),
                    [_observation_id(messages[representative])],
                )
            )
            consumed_indices.update(group)
            used_chars += needed
            newly_added = True
        if newly_added:
            selected_docs.append(doc_id)

    selected_units.sort(key=lambda unit: unit[0])
    text = "\n".join(unit[1] for unit in selected_units)
    visible_ids = [oid for unit in selected_units for oid in unit[3]]
    return {
        "text": text,
        "visible_observation_ids": visible_ids,
        "used_chars": len(text),
        "selected_docs": selected_docs,
    }
