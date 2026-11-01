"""Proactive participation invariants (§23) — pure checks."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Sequence


class ProactiveInvariantError(ValueError):
    pass


@dataclass
class ProactiveCandidate:
    candidate_id: str
    conversation_version: str
    topic_version: str
    human_anchor_message_ids: tuple[str, ...]
    source_message_is_bot: bool = False
    created_by_timer: bool = False
    stale: bool = False


@dataclass
class ConversationEpoch:
    conversation_version: str
    last_human_message_id: str | None
    proactive_sent_ids: list[str] = field(default_factory=list)
    human_activity_since_proactive: bool = True


def validate_candidate(c: ProactiveCandidate) -> None:
    if c.source_message_is_bot:
        raise ProactiveInvariantError("bot/system cannot create proactive candidate")
    if c.created_by_timer:
        raise ProactiveInvariantError("timer/cron cannot create new proactive candidate")
    if not c.human_anchor_message_ids:
        raise ProactiveInvariantError("human_anchor_message_ids required")
    if c.stale:
        raise ProactiveInvariantError("stale candidate must be discarded")


def can_send(c: ProactiveCandidate, epoch: ConversationEpoch) -> None:
    validate_candidate(c)
    if c.conversation_version != epoch.conversation_version:
        raise ProactiveInvariantError("conversation_version mismatch")
    if not epoch.last_human_message_id:
        raise ProactiveInvariantError("no recent human message anchor")
    if epoch.proactive_sent_ids and not epoch.human_activity_since_proactive:
        raise ProactiveInvariantError("no second proactive without new human activity")
    if c.candidate_id in epoch.proactive_sent_ids:
        raise ProactiveInvariantError("duplicate proactive in epoch")


def shadow_record(
    c: ProactiveCandidate, *, would_participate: bool, predicted_response: str
) -> dict[str, Any]:
    validate_candidate(c)
    return {
        "mode": "shadow",
        "candidate_id": c.candidate_id,
        "would_participate": would_participate,
        "predicted_response_present": bool(predicted_response),
        # never include raw predicted text in logs by default
    }
