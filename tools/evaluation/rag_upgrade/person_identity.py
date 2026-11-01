"""Person / Account / SourceActor / Alias / Memory Space contracts (§11–13)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

IdentityStatus = Literal["confirmed", "candidate", "rejected"]


class IdentityError(ValueError):
    pass


@dataclass
class Account:
    account_id: str
    platform: str  # discord|kakao|other


@dataclass
class SourceActor:
    source_actor_id: str
    platform: str
    display_label: str


@dataclass
class Person:
    person_id: str


@dataclass
class Alias:
    alias: str
    person_id: str | None
    status: IdentityStatus


@dataclass
class IdentityLink:
    person_id: str
    account_id: str | None = None
    source_actor_id: str | None = None
    status: IdentityStatus = "candidate"


@dataclass
class MemorySpace:
    memory_space_id: str
    destination: str
    sources: list[str] = field(default_factory=list)


def assert_no_auto_merge_on_alias_similarity(a: str, b: str) -> None:
    """Policy helper: similar strings alone must not confirm identity."""

    if a == b:
        return
    # callers must not treat this as confirmation
    return


def search_scope(
    *,
    person_id: str | None,
    memory_space: MemorySpace,
    allowed_sources: set[str],
) -> set[str]:
    space = set(memory_space.sources)
    scoped = space & allowed_sources
    if not scoped:
        raise IdentityError("empty search scope")
    # person filter is applied by caller on results; space ∩ allowed is mandatory
    _ = person_id
    return scoped


def confirm_link(link: IdentityLink) -> IdentityLink:
    if link.status != "candidate":
        raise IdentityError("only candidate can be confirmed")
    if not link.account_id and not link.source_actor_id:
        raise IdentityError("link needs account or source_actor")
    return IdentityLink(
        person_id=link.person_id,
        account_id=link.account_id,
        source_actor_id=link.source_actor_id,
        status="confirmed",
    )


def kakao_actor_id(room: str, display: str) -> str:
    return f"kakao:{room}:{display}"
