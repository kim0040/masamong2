"""Stable leaf identifiers and same-room parent expansion (§7–9)."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any, Mapping, Sequence


class LeafContractError(ValueError):
    """Leaf / parent expansion contract violation."""


@dataclass(frozen=True)
class LeafSpan:
    source_room_id: str
    segmentation_version: str
    first_message_revision_id: str
    last_message_revision_id: str
    content_hash: str
    message_revision_ids: tuple[str, ...]

    @property
    def leaf_id(self) -> str:
        return compute_leaf_id(
            self.source_room_id,
            self.segmentation_version,
            self.first_message_revision_id,
            self.last_message_revision_id,
        )


def compute_leaf_id(
    source_room_id: str,
    segmentation_version: str,
    first_message_revision_id: str,
    last_message_revision_id: str,
) -> str:
    for part in (
        source_room_id,
        segmentation_version,
        first_message_revision_id,
        last_message_revision_id,
    ):
        if not str(part).strip():
            raise LeafContractError("leaf id parts must be non-empty")
    material = "\0".join(
        [
            source_room_id,
            segmentation_version,
            first_message_revision_id,
            last_message_revision_id,
        ]
    )
    return hashlib.sha256(material.encode("utf-8")).hexdigest()[:32]


def content_hash(texts: Sequence[str]) -> str:
    h = hashlib.sha256()
    for t in texts:
        h.update(t.encode("utf-8"))
        h.update(b"\0")
    return h.hexdigest()


def build_leaf_from_messages(
    messages: Sequence[Mapping[str, Any]],
    *,
    segmentation_version: str,
) -> LeafSpan:
    if not messages:
        raise LeafContractError("empty leaf")
    rooms = {str(m.get("source_room_id") or "") for m in messages}
    if len(rooms) != 1 or "" in rooms:
        raise LeafContractError("leaf must be a single non-empty source_room")
    room = next(iter(rooms))
    rev_ids: list[str] = []
    texts: list[str] = []
    for m in messages:
        rid = str(m.get("message_revision_id") or "").strip()
        if not rid:
            raise LeafContractError("message_revision_id required")
        rev_ids.append(rid)
        texts.append(str(m.get("content") or ""))
    return LeafSpan(
        source_room_id=room,
        segmentation_version=segmentation_version,
        first_message_revision_id=rev_ids[0],
        last_message_revision_id=rev_ids[-1],
        content_hash=content_hash(texts),
        message_revision_ids=tuple(rev_ids),
    )


def assert_room_boundary(leaves: Sequence[LeafSpan]) -> None:
    for leaf in leaves:
        rooms = {leaf.source_room_id}
        if len(rooms) != 1:
            raise LeafContractError("room boundary broken")


def parent_expand(
    room_messages: Sequence[Mapping[str, Any]],
    *,
    leaf_revision_ids: Sequence[str],
    before: int,
    after: int,
) -> list[str]:
    """Expand within one ordered room stream by revision ids.

    ``room_messages`` must already be a single source_room, ordered.
    """

    if before < 0 or after < 0:
        raise LeafContractError("before/after must be >= 0")
    if not room_messages:
        return []
    rooms = {str(m.get("source_room_id") or "") for m in room_messages}
    if len(rooms) != 1:
        raise LeafContractError("parent expand requires single source_room stream")
    index = {
        str(m["message_revision_id"]): i for i, m in enumerate(room_messages)
    }
    positions = []
    for rid in leaf_revision_ids:
        if rid not in index:
            raise LeafContractError(f"leaf revision not in room stream: {rid}")
        positions.append(index[rid])
    lo = max(0, min(positions) - before)
    hi = min(len(room_messages) - 1, max(positions) + after)
    return [
        str(room_messages[i]["message_revision_id"]) for i in range(lo, hi + 1)
    ]
