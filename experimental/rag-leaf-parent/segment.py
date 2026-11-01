"""메시지 스트림 → leaf / baseline window 분할. 원문은 수정하지 않는다."""

from __future__ import annotations

import heapq
from typing import Any, Iterable, Mapping


class _MaxTimeItem:
    """max-heap용: 선택된 집합에서 시각이 가장 늦은 항목이 루트."""

    __slots__ = ("time", "orig", "oid", "row")

    def __init__(self, time: str, orig: str, oid: str, row: dict[str, Any]) -> None:
        self.time = time
        self.orig = orig
        self.oid = oid
        self.row = row

    def __lt__(self, other: _MaxTimeItem) -> bool:
        return (self.time, self.orig, self.oid) > (other.time, other.orig, other.oid)


def collect_messages(
    observations: Iterable[Any],
    *,
    limit: int,
    room_merge: Mapping[str, str] | None = None,
) -> list[dict[str, Any]]:
    """의미 후보 관찰을 모은다.

    ``room_merge``가 있으면 원래 source_room_id는 ``source_room_original``에 남기고
    논리 방 ID로 합친 뒤, 시각 순으로 한 스트림으로 정렬한다.
    Discord(``discord:guild:channel``)는 서버·채널이 다르므로 서로 다른 방으로
    합치지 않는다 (항등 매핑만 허용).
    limit>0 이면 전량 적재 없이 가장 이른 limit개만 유지한다.
    """

    merge = dict(room_merge or {})
    for src, dst in merge.items():
        if str(src).startswith("discord:") or str(dst).startswith("discord:"):
            if str(src) != str(dst):
                raise ValueError(
                    "Discord 채널은 guild_id:channel_id 단위로 구분하며 서로 병합할 수 없다: "
                    f"{src} → {dst}"
                )

    def _row(obs: Any, *, logical: str, original: str) -> dict[str, Any]:
        return {
            "observation_id": str(obs.observation_id),
            "source_room_id": logical,
            "source_room_original": original,
            "actor_label": obs.actor_label or "?",
            "actor_account": obs.actor_account,
            "content": (getattr(obs, "content", None) or "").strip(),
            "parsed_time": obs.parsed_time,
        }

    if not merge:
        out: list[dict[str, Any]] = []
        for obs in observations:
            if not getattr(obs, "semantic_eligible", False):
                continue
            content = (getattr(obs, "content", None) or "").strip()
            if not content:
                continue
            room = str(obs.source_room_id)
            out.append(_row(obs, logical=room, original=room))
            if limit > 0 and len(out) >= limit:
                break
        return order_messages_by_room(out)

    if limit <= 0:
        out = []
        for obs in observations:
            if not getattr(obs, "semantic_eligible", False):
                continue
            content = (getattr(obs, "content", None) or "").strip()
            if not content:
                continue
            original = str(obs.source_room_id)
            out.append(
                _row(obs, logical=merge.get(original, original), original=original)
            )
        out.sort(
            key=lambda m: (
                str(m.get("parsed_time") or ""),
                str(m.get("source_room_original") or ""),
                str(m.get("observation_id") or ""),
            )
        )
        return order_messages_by_room(out)

    heap: list[_MaxTimeItem] = []
    for obs in observations:
        if not getattr(obs, "semantic_eligible", False):
            continue
        content = (getattr(obs, "content", None) or "").strip()
        if not content:
            continue
        original = str(obs.source_room_id)
        logical = merge.get(original, original)
        row = _row(obs, logical=logical, original=original)
        time = str(obs.parsed_time or "")
        oid = str(obs.observation_id)
        item = _MaxTimeItem(time, original, oid, row)
        if len(heap) < limit:
            heapq.heappush(heap, item)
        elif (time, original, oid) < (heap[0].time, heap[0].orig, heap[0].oid):
            heapq.heapreplace(heap, item)

    out = [item.row for item in heap]
    out.sort(
        key=lambda m: (
            str(m.get("parsed_time") or ""),
            str(m.get("source_room_original") or ""),
            str(m.get("observation_id") or ""),
        )
    )
    return order_messages_by_room(out)


def order_messages_by_room(messages: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """방별로 묶고 시각 순으로 둔다. Discord 채널을 한 타임라인으로 이어 붙이지 않는다."""

    by_room: dict[str, list[dict[str, Any]]] = {}
    for row in messages:
        by_room.setdefault(str(row.get("source_room_id") or ""), []).append(row)
    ordered: list[dict[str, Any]] = []
    for room in sorted(by_room):
        rows = by_room[room]
        rows.sort(
            key=lambda m: (
                str(m.get("parsed_time") or ""),
                str(m.get("source_room_original") or ""),
                str(m.get("observation_id") or ""),
            )
        )
        ordered.extend(rows)
    for i, row in enumerate(ordered):
        row["index"] = i
    return ordered


def _discord_room_label(room: str) -> str:
    """discord:guild:channel → 사람이 읽기 쉬운 라벨 (이름은 raw에 없음)."""

    parts = str(room).split(":")
    if len(parts) >= 3 and parts[0] == "discord":
        guild, channel = parts[1], parts[2]
        if guild == "0":
            return f"discord DM channel={channel}"
        return f"discord guild={guild} channel={channel}"
    return room


def format_body(
    messages: list[dict[str, Any]],
    *,
    with_prefix: bool,
) -> str:
    lines = [f"{m['actor_label']}: {m['content']}" for m in messages]
    body = "\n".join(lines)
    if not with_prefix or not messages:
        return body
    room_raw = messages[0].get("source_room_id") or "?"
    room = _discord_room_label(str(room_raw))
    date = ""
    for m in messages:
        pt = m.get("parsed_time") or ""
        if pt:
            date = str(pt)[:10]
            break
    speakers: list[str] = []
    seen: set[str] = set()
    for m in messages:
        label = str(m.get("actor_label") or "?")
        if label not in seen:
            seen.add(label)
            speakers.append(label)
    speaker_part = ", ".join(speakers[:8])
    if len(speakers) > 8:
        speaker_part += ", …"
    prefix = f"[{room}"
    if date:
        prefix += f" · {date}"
    if speaker_part:
        prefix += f" · {speaker_part}"
    prefix += "]"
    return f"{prefix}\n{body}"


def slice_segments(
    messages: list[dict[str, Any]],
    *,
    size: int,
    overlap: int,
    limit: int,
    kind: str,
    with_prefix: bool,
) -> list[dict[str, Any]]:
    if size <= 0:
        raise ValueError("size must be positive")
    if overlap < 0 or overlap >= size:
        raise ValueError("overlap must satisfy 0 <= overlap < size")
    if limit <= 0:
        return []
    step = size - overlap
    segments: list[dict[str, Any]] = []
    start = 0
    while start < len(messages) and len(segments) < limit:
        end = min(len(messages), start + size)
        chunk = messages[start:end]
        if not chunk:
            break
        # overlap-only tail of a full previous window: skip empty progress
        if segments and end == segments[-1]["end"] and len(chunk) <= overlap:
            break
        body = format_body(chunk, with_prefix=with_prefix)
        observation_ids = [m["observation_id"] for m in chunk]
        speakers = []
        seen: set[str] = set()
        for m in chunk:
            label = str(m["actor_label"])
            if label not in seen:
                seen.add(label)
                speakers.append(label)
        doc_id = f"{kind}:{start}-{end-1}"
        segments.append(
            {
                "doc_id": doc_id,
                "kind": kind,
                "start": start,
                "end": end,
                "body": body,
                "source_room_id": chunk[0]["source_room_id"],
                "observation_ids": observation_ids,
                "speakers": speakers,
                "n": len(chunk),
            }
        )
        if end >= len(messages):
            break
        start += step
    return segments


def _parse_msg_ts(row: Mapping[str, Any]) -> float | None:
    raw = str(row.get("parsed_time") or "").strip()
    if not raw:
        return None
    try:
        if raw.endswith("Z"):
            raw = raw[:-1] + "+00:00"
        from datetime import datetime

        return datetime.fromisoformat(raw).timestamp()
    except ValueError:
        return None


def slice_segments_time_aware(
    messages: list[dict[str, Any]],
    *,
    max_size: int,
    gap_minutes: float,
    min_size: int,
    limit: int,
    kind: str,
    with_prefix: bool,
) -> list[dict[str, Any]]:
    """방 스트림을 시간 간격·최대 길이로 자른다. 원문 인덱스는 유지한다."""

    if max_size <= 0 or limit <= 0:
        return []
    gap_sec = max(0.0, float(gap_minutes)) * 60.0
    min_size = max(1, int(min_size))
    segments: list[dict[str, Any]] = []
    start = 0
    n = len(messages)
    while start < n and len(segments) < limit:
        end = start + 1
        prev_ts = _parse_msg_ts(messages[start])
        while end < n and (end - start) < max_size:
            ts = _parse_msg_ts(messages[end])
            if (
                prev_ts is not None
                and ts is not None
                and ts - prev_ts >= gap_sec
                and (end - start) >= min_size
            ):
                break
            if ts is not None:
                prev_ts = ts
            end += 1
        chunk = messages[start:end]
        body = format_body(chunk, with_prefix=with_prefix)
        speakers: list[str] = []
        seen: set[str] = set()
        for m in chunk:
            label = str(m["actor_label"])
            if label not in seen:
                seen.add(label)
                speakers.append(label)
        segments.append(
            {
                "doc_id": f"{kind}:{start}-{end - 1}",
                "kind": kind,
                "start": start,
                "end": end,
                "body": body,
                "source_room_id": chunk[0]["source_room_id"],
                "observation_ids": [m["observation_id"] for m in chunk],
                "speakers": speakers,
                "n": len(chunk),
            }
        )
        start = end
    return segments


def slice_segments_per_room(
    messages: list[dict[str, Any]],
    *,
    size: int,
    overlap: int,
    limit: int,
    kind: str,
    with_prefix: bool,
    split: str = "fixed",
    gap_minutes: float = 30.0,
    min_size: int = 2,
) -> list[dict[str, Any]]:
    """방 구간 안에서만 leaf/window를 자른다. 서로 다른 Discord 채널을 한 조각에 넣지 않는다."""

    if limit <= 0:
        return []
    out: list[dict[str, Any]] = []
    i = 0
    n = len(messages)
    use_gap = str(split or "fixed") == "time_gap"
    while i < n and len(out) < limit:
        room = str(messages[i].get("source_room_id") or "")
        j = i + 1
        while j < n and str(messages[j].get("source_room_id") or "") == room:
            j += 1
        if use_gap:
            segs = slice_segments_time_aware(
                messages[i:j],
                max_size=size,
                gap_minutes=gap_minutes,
                min_size=min_size,
                limit=limit - len(out),
                kind=kind,
                with_prefix=with_prefix,
            )
        else:
            segs = slice_segments(
                messages[i:j],
                size=size,
                overlap=overlap,
                limit=limit - len(out),
                kind=kind,
                with_prefix=with_prefix,
            )
        for seg in segs:
            seg["start"] = int(seg["start"]) + i
            seg["end"] = int(seg["end"]) + i
            seg["doc_id"] = f"{kind}:{seg['start']}-{seg['end'] - 1}"
            out.append(seg)
        i = j
    return out


def expand_segment(
    messages: list[dict[str, Any]],
    segment: dict[str, Any],
    *,
    before: int,
    after: int,
    with_prefix: bool,
    char_limit: int,
    allowed_observation_ids: Iterable[str] | None = None,
    allowed_rooms: Iterable[str] | None = None,
    time_allows: Any = None,
    required_observation_ids: Iterable[str] | None = None,
) -> dict[str, Any]:
    """matched segment를 같은 방 안에서 parent로 넓혀 예산 안에 포장한다.

    옛 동작(``format_body(window)[:char_limit]``)은 앞 문맥이 길면 matched 본문을 잘라냈다.
    이제 ``evidence_packing.pack_segment_evidence`` 에 위임한다: matched 원문을 먼저 확보하고
    남은 예산으로 앞뒤 문맥을 배분하며, ``visible_spans`` 로 실제 보인 원문 [start, end) 를
    돌려준다. 반환 키는 기존 키의 superset이다. ``observation_ids`` 는 허용 범위 안의 window
    후보 집합이며 보였다는 뜻이 아니다 (보인 것은 ``visible_observation_ids``).
    scope 인자를 주지 않으면(None) 제한 없음이고, 빈 집합은 허용 없음이다.
    """

    from evidence_packing import pack_segment_evidence

    return pack_segment_evidence(
        messages,
        segment,
        before=before,
        after=after,
        with_prefix=with_prefix,
        char_limit=char_limit,
        allowed_observation_ids=allowed_observation_ids,
        allowed_rooms=allowed_rooms,
        time_allows=time_allows,
        required_observation_ids=required_observation_ids,
    )
