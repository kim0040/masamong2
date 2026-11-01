"""Incremental leaf builder — reference behaviour for live ingest (Discord Gateway, appended Kakao
exports, any multi-party chat adapter). Not wired into the sandbox index; it is the contract the
the product can implement and the replay harness checks.

Why this exists: the sandbox builds leaves once, in batch, over a static corpus. A live room needs
leaves that (a) never move once closed, (b) close in quiet rooms without waiting for 8 messages,
(c) absorb edits/deletes by touching exactly one leaf, (d) give "just said" messages a searchable
home before any embedding exists, and (e) are identical whether built live or rebuilt from the log.

Rules
- Segmentation follows the *event log order* per room (arrival order), not a later sort. The same log
  always yields the same leaves (batch == incremental).
- A leaf closes when it holds `size` message slots, or when the next message arrives after an idle gap
  of `idle_gap_s`, or when `tick(now)` sees the room quiet for `idle_gap_s`. A tick close and a
  gap-at-next-message close produce the same boundary.
- Leaf id = hash(version, room, first native id). Closed leaves are immutable in membership.
- Edit → same slot, new revision → the one leaf containing it is marked dirty (re-embed job).
- Delete → tombstone in place (slot kept, boundaries unchanged); text is gone from every doc at once.
- Re-import of an already-seen native id with identical content is a no-op; different content is an
  edit. Kakao has no native ids: `kakao_native_keys` derives stable keys from
  (room, minute, label, content hash, occurrence) so an overlapping re-export does not duplicate.
- Open (not yet closed) leaves are "hot tail" docs: lexical-searchable immediately, never embedded.
"""

from __future__ import annotations

import hashlib
import math
from collections import Counter
from dataclasses import dataclass, field
from typing import Any, Iterable, Iterator, Mapping, Sequence


@dataclass(frozen=True)
class ChatEvent:
    kind: str  # create | edit | delete
    room: str
    native_id: str
    ts: float  # epoch seconds, timezone-resolved by the adapter
    label: str = ""
    content: str = ""

    def __post_init__(self) -> None:
        if self.kind not in ("create", "edit", "delete"):
            raise ValueError(f"unknown event kind: {self.kind}")
        if not self.room or not self.native_id:
            raise ValueError("room and native_id are required")
        if not math.isfinite(self.ts):
            raise ValueError("ts must be a finite epoch (adapter resolves timezone)")


@dataclass
class Slot:
    native_id: str
    ts: float
    label: str
    content: str | None  # None = tombstoned
    content_sha: str
    revision: int = 0


@dataclass
class Leaf:
    leaf_id: str
    room: str
    slots: list[str] = field(default_factory=list)  # native ids, in arrival order
    closed: bool = False
    close_reason: str = ""
    revision: int = 0
    embedded_revision: int = -1

    @property
    def dirty(self) -> bool:
        return self.closed and self.embedded_revision < self.revision


def _sha(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]


class IncrementalLeafBuilder:
    def __init__(self, *, size: int = 8, idle_gap_s: float | None = None, version: str | None = None) -> None:
        if size < 1:
            raise ValueError("size must be >= 1")
        self.size = int(size)
        self.idle_gap_s = float(idle_gap_s) if idle_gap_s else None
        self.version = version or f"leaf{self.size}-gap{int(self.idle_gap_s or 0)}"
        self.slots: dict[tuple[str, str], Slot] = {}
        self.leaves: dict[str, Leaf] = {}
        self.leaf_order: dict[str, list[str]] = {}
        self.open_leaf: dict[str, str] = {}
        self.leaf_of: dict[tuple[str, str], str] = {}
        self.last_ts: dict[str, float] = {}
        self.stats: Counter[str] = Counter()

    # ------------------------------------------------------------------ ingest

    def apply(self, ev: ChatEvent) -> None:
        key = (ev.room, ev.native_id)
        if ev.kind == "create":
            existing = self.slots.get(key)
            if existing is not None:
                if existing.content is not None and existing.content_sha == _sha(ev.content):
                    self.stats["duplicate_create_ignored"] += 1
                    return
                self.stats["create_as_edit"] += 1
                self._edit(key, ev.content)
                return
            self._create(ev)
        elif ev.kind == "edit":
            if key not in self.slots:
                self.stats["edit_unknown_ignored"] += 1
                return
            self._edit(key, ev.content)
        else:
            if key not in self.slots:
                self.stats["delete_unknown_ignored"] += 1
                return
            slot = self.slots[key]
            if slot.content is None:
                return
            slot.content = None
            slot.content_sha = ""
            slot.revision += 1
            self._touch(self.leaf_of[key])
            self.stats["deleted"] += 1

    def apply_all(self, events: Iterable[ChatEvent]) -> "IncrementalLeafBuilder":
        for ev in events:
            self.apply(ev)
        return self

    def _create(self, ev: ChatEvent) -> None:
        room = ev.room
        prev = self.last_ts.get(room)
        if self.idle_gap_s is not None and prev is not None and ev.ts - prev >= self.idle_gap_s:
            self._close(room, "idle_gap")
        leaf_id = self.open_leaf.get(room)
        if leaf_id is None:
            leaf_id = _sha(f"{self.version}|{room}|{ev.native_id}")
            self.leaves[leaf_id] = Leaf(leaf_id=leaf_id, room=room)
            self.leaf_order.setdefault(room, []).append(leaf_id)
            self.open_leaf[room] = leaf_id
        self.slots[(room, ev.native_id)] = Slot(ev.native_id, ev.ts, ev.label, ev.content, _sha(ev.content))
        self.leaf_of[(room, ev.native_id)] = leaf_id
        self.leaves[leaf_id].slots.append(ev.native_id)
        self.last_ts[room] = max(prev, ev.ts) if prev is not None else ev.ts
        self.stats["created"] += 1
        if len(self.leaves[leaf_id].slots) >= self.size:
            self._close(room, "size")

    def _edit(self, key: tuple[str, str], content: str) -> None:
        slot = self.slots[key]
        if slot.content is None:
            self.stats["edit_after_delete_ignored"] += 1
            return
        if slot.content_sha == _sha(content):
            return
        slot.content = content
        slot.content_sha = _sha(content)
        slot.revision += 1
        self._touch(self.leaf_of[key])
        self.stats["edited"] += 1

    def _touch(self, leaf_id: str) -> None:
        self.leaves[leaf_id].revision += 1

    def _close(self, room: str, reason: str) -> None:
        leaf_id = self.open_leaf.pop(room, None)
        if leaf_id is None:
            return
        leaf = self.leaves[leaf_id]
        leaf.closed = True
        leaf.close_reason = reason
        self.stats[f"closed_{reason}"] += 1

    def tick(self, now: float) -> list[str]:
        """Close leaves of rooms quiet for idle_gap_s. Returns closed leaf ids."""

        if self.idle_gap_s is None:
            return []
        closed = []
        for room, leaf_id in list(self.open_leaf.items()):
            if now - self.last_ts.get(room, now) >= self.idle_gap_s:
                self._close(room, "idle_gap")
                closed.append(leaf_id)
        return closed

    # ------------------------------------------------------------------ embedding jobs

    def pending_embed_jobs(self) -> list[str]:
        return [lid for lid, leaf in self.leaves.items() if leaf.dirty]

    def mark_embedded(self, leaf_ids: Iterable[str]) -> None:
        for lid in leaf_ids:
            leaf = self.leaves[lid]
            leaf.embedded_revision = leaf.revision

    # ------------------------------------------------------------------ views

    def leaf_text(self, leaf_id: str, *, as_of: float | None = None) -> str:
        leaf = self.leaves[leaf_id]
        lines = []
        for nid in leaf.slots:
            slot = self.slots[(leaf.room, nid)]
            if slot.content is None or (as_of is not None and slot.ts > as_of):
                continue
            lines.append(f"{slot.label}: {slot.content}")
        return "\n".join(lines)

    def searchable_docs(self, *, as_of: float | None = None) -> Iterator[dict[str, Any]]:
        """Closed+embedded leaves are dense+lexical; open leaves are lexical-only hot tail.
        `as_of` hides every message newer than the replay clock (future isolation)."""

        for room, order in self.leaf_order.items():
            for lid in order:
                leaf = self.leaves[lid]
                members = [self.slots[(room, n)] for n in leaf.slots]
                visible = [s for s in members if as_of is None or s.ts <= as_of]
                if not visible:
                    continue
                mode = "dense+lexical" if (leaf.closed and leaf.embedded_revision == leaf.revision) else "lexical_hot"
                yield {
                    "leaf_id": lid,
                    "room": room,
                    "mode": mode,
                    "native_ids": [s.native_id for s in visible if s.content is not None],
                    "max_ts": max(s.ts for s in visible),
                    "text": self.leaf_text(lid, as_of=as_of),
                }

    def boundaries(self) -> dict[str, list[tuple[str, ...]]]:
        return {room: [tuple(self.leaves[l].slots) for l in order] for room, order in self.leaf_order.items()}


def kakao_native_keys(rows: Sequence[Mapping[str, Any]], *, room_key: str = "source_room_id") -> list[str]:
    """Stable ids for sources without native message ids (Kakao CSV and similar exports).

    key = room | minute | label | content-sha | occurrence-within-that-tuple. Re-exporting an
    overlapping range yields the same keys for the same messages, so appends do not duplicate.
    Two genuinely repeated identical lines in the same minute stay distinct via the occurrence index.
    """

    seen: Counter[tuple[str, str, str, str]] = Counter()
    out = []
    for r in rows:
        base = (str(r.get(room_key) or ""), str(r.get("parsed_time") or "")[:16], str(r.get("actor_label") or ""),
                _sha(str(r.get("content") or "")))
        n = seen[base]
        seen[base] += 1
        out.append("k:" + _sha("|".join(base) + f"|{n}"))
    return out
