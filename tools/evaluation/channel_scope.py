"""Explicit, provenance preserving channel scopes for offline evaluation.

This module is deliberately small and in-memory.  It is an evaluation seam for
the future retrieval implementation, not a production permission service.  A
source room is identified by the tuple ``(workspace_id, platform,
source_room_id)``.  Collections and display names are metadata only; they do
not grant access, merge identities, or define window boundaries.

The retrieval path follows the important ordering rule used by the eventual
product:

1. materialise an immutable view snapshot;
2. apply source/destination/purpose grants to observations *before* indexing;
3. run lexical ranking on the remaining observations; and
4. re-read the policy before returning evidence.

If the policy revision changes between steps 1 and 4, no old evidence is
returned.  All fixture data is synthetic and this module performs no network,
database, or filesystem writes.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence

try:  # Package import when used as ``tools.evaluation.channel_scope``.
    from .lexical import LexicalIndex
except ImportError:  # Script/test import from the evaluation directory.
    from lexical import LexicalIndex


SCHEMA_VERSION = "channel-scope/v1"
DEFAULT_PURPOSE = "memory_retrieval"


class ChannelScopeError(ValueError):
    """Base error for malformed scope definitions or unknown identities."""


class FixtureValidationError(ChannelScopeError):
    """Raised when a synthetic channel-scope fixture is invalid."""


class DuplicateChannelError(FixtureValidationError):
    """A source-room identity was declared more than once."""


class DuplicatePolicyError(FixtureValidationError):
    """A link or grant was declared more than once."""


class UnknownChannelError(ChannelScopeError):
    """A link, grant, view, observation, or request names an unknown channel."""


class UnknownViewError(ChannelScopeError):
    """A request names a view that is not present in its workspace."""


def _required_text(value: Any, field_name: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise FixtureValidationError(f"{field_name} must be a non-empty string")
    return value.strip()


def _optional_text(value: Any, field_name: str) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str):
        raise FixtureValidationError(f"{field_name} must be a string or null")
    value = value.strip()
    return value or None


def _mapping(value: Any, field_name: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise FixtureValidationError(f"{field_name} must be an object")
    return value


def _canonical(value: Any) -> Any:
    """Turn supported values into JSON-safe, deterministically ordered data."""

    if isinstance(value, ChannelKey):
        return value.as_dict()
    if isinstance(value, ChannelSpec):
        return value.as_dict()
    if isinstance(value, ChannelLink):
        return value.as_dict()
    if isinstance(value, AccessGrant):
        return value.as_dict()
    if isinstance(value, RetrievalView):
        return value.as_dict()
    if isinstance(value, Observation):
        return value.as_dict()
    if isinstance(value, Mapping):
        return {str(k): _canonical(value[k]) for k in sorted(value, key=str)}
    if isinstance(value, (list, tuple)):
        return [_canonical(item) for item in value]
    if isinstance(value, (set, frozenset)):
        return sorted((_canonical(item) for item in value), key=lambda item: _json(item))
    return value


def _json(value: Any) -> str:
    return json.dumps(
        _canonical(value),
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def deterministic_fingerprint(value: Any) -> str:
    """Return a stable SHA-256 fingerprint for a scope or fixture payload."""

    return hashlib.sha256(_json(value).encode("utf-8")).hexdigest()


@dataclass(frozen=True, slots=True, order=True)
class ChannelKey:
    """Immutable source-room identity.

    ``platform`` is case-insensitive at the identity boundary.  The workspace
    and source-room strings remain otherwise opaque; a display name is never
    part of this key.
    """

    workspace_id: str
    platform: str
    source_room_id: str

    def __post_init__(self) -> None:
        object.__setattr__(self, "workspace_id", _required_text(self.workspace_id, "workspace_id"))
        object.__setattr__(self, "platform", _required_text(self.platform, "platform").casefold())
        object.__setattr__(self, "source_room_id", _required_text(self.source_room_id, "source_room_id"))

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any], *, field_name: str = "channel") -> "ChannelKey":
        value = _mapping(value, field_name)
        return cls(
            workspace_id=value.get("workspace_id"),
            platform=value.get("platform"),
            source_room_id=value.get("source_room_id", value.get("source_room")),
        )

    def as_dict(self) -> dict[str, str]:
        return {
            "workspace_id": self.workspace_id,
            "platform": self.platform,
            "source_room_id": self.source_room_id,
        }

    @property
    def identity(self) -> tuple[str, str, str]:
        return (self.workspace_id, self.platform, self.source_room_id)


@dataclass(frozen=True, slots=True)
class ChannelSpec:
    """A registered source-room channel and non-authoritative metadata."""

    workspace_id: str
    platform: str
    source_room_id: str
    name: str | None = None
    collection_id: str | None = None

    def __post_init__(self) -> None:
        key = ChannelKey(self.workspace_id, self.platform, self.source_room_id)
        object.__setattr__(self, "workspace_id", key.workspace_id)
        object.__setattr__(self, "platform", key.platform)
        object.__setattr__(self, "source_room_id", key.source_room_id)
        object.__setattr__(self, "name", _optional_text(self.name, "name"))
        object.__setattr__(self, "collection_id", _optional_text(self.collection_id, "collection_id"))

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any]) -> "ChannelSpec":
        value = _mapping(value, "channel")
        key_data = value.get("key", value)
        key = ChannelKey.from_mapping(key_data)
        return cls(
            workspace_id=key.workspace_id,
            platform=key.platform,
            source_room_id=key.source_room_id,
            name=value.get("name"),
            collection_id=value.get("collection_id"),
        )

    @property
    def key(self) -> ChannelKey:
        return ChannelKey(self.workspace_id, self.platform, self.source_room_id)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = self.key.as_dict()
        if self.name is not None:
            result["name"] = self.name
        if self.collection_id is not None:
            result["collection_id"] = self.collection_id
        return result


@dataclass(frozen=True, slots=True)
class Observation:
    """A searchable observation with immutable source provenance."""

    observation_id: str
    workspace_id: str
    platform: str
    source_room_id: str
    content: str
    ordinal: int | None = None
    collection_id: str | None = None

    def __post_init__(self) -> None:
        object.__setattr__(self, "observation_id", _required_text(self.observation_id, "observation_id"))
        key = ChannelKey(self.workspace_id, self.platform, self.source_room_id)
        object.__setattr__(self, "workspace_id", key.workspace_id)
        object.__setattr__(self, "platform", key.platform)
        object.__setattr__(self, "source_room_id", key.source_room_id)
        if not isinstance(self.content, str):
            raise FixtureValidationError("content must be a string")
        if self.ordinal is not None and (not isinstance(self.ordinal, int) or self.ordinal < 0):
            raise FixtureValidationError("ordinal must be a non-negative integer or null")
        object.__setattr__(self, "collection_id", _optional_text(self.collection_id, "collection_id"))

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any]) -> "Observation":
        value = _mapping(value, "observation")
        channel_data = value.get("channel", value)
        key = ChannelKey.from_mapping(channel_data, field_name="observation.channel")
        return cls(
            observation_id=value.get("observation_id"),
            workspace_id=key.workspace_id,
            platform=key.platform,
            source_room_id=key.source_room_id,
            content=value.get("content", ""),
            ordinal=value.get("ordinal"),
            collection_id=value.get("collection_id"),
        )

    @property
    def channel(self) -> ChannelKey:
        return ChannelKey(self.workspace_id, self.platform, self.source_room_id)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "observation_id": self.observation_id,
            **self.channel.as_dict(),
            "content": self.content,
        }
        if self.ordinal is not None:
            result["ordinal"] = self.ordinal
        if self.collection_id is not None:
            result["collection_id"] = self.collection_id
        return result


@dataclass(frozen=True, slots=True)
class ChannelLink:
    """A directed, non-transitive relationship between two source rooms."""

    source: ChannelKey
    target: ChannelKey

    def __post_init__(self) -> None:
        if self.source.workspace_id != self.target.workspace_id:
            raise FixtureValidationError("a link cannot cross workspaces")
        if self.source == self.target:
            raise FixtureValidationError("a channel cannot link to itself")

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any]) -> "ChannelLink":
        value = _mapping(value, "link")
        return cls(
            source=ChannelKey.from_mapping(value.get("source"), field_name="link.source"),
            target=ChannelKey.from_mapping(value.get("target", value.get("destination")), field_name="link.target"),
        )

    @property
    def workspace_id(self) -> str:
        return self.source.workspace_id

    def as_dict(self) -> dict[str, Any]:
        return {"source": self.source.as_dict(), "target": self.target.as_dict()}


@dataclass(frozen=True, slots=True)
class AccessGrant:
    """Explicit source-to-destination access for one purpose."""

    source: ChannelKey
    destination: ChannelKey
    purpose: str

    def __post_init__(self) -> None:
        if self.source.workspace_id != self.destination.workspace_id:
            raise FixtureValidationError("a grant cannot cross workspaces")
        object.__setattr__(self, "purpose", _required_text(self.purpose, "purpose"))

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any]) -> "AccessGrant":
        value = _mapping(value, "grant")
        return cls(
            source=ChannelKey.from_mapping(value.get("source"), field_name="grant.source"),
            destination=ChannelKey.from_mapping(
                value.get("destination"), field_name="grant.destination"
            ),
            purpose=value.get("purpose", DEFAULT_PURPOSE),
        )

    @property
    def workspace_id(self) -> str:
        return self.source.workspace_id

    def as_dict(self) -> dict[str, Any]:
        return {
            "source": self.source.as_dict(),
            "destination": self.destination.as_dict(),
            "purpose": self.purpose,
        }


@dataclass(frozen=True, slots=True)
class RetrievalView:
    """A revisioned set of explicit roots with optional one-hop link expansion."""

    view_id: str
    workspace_id: str
    selected_channels: tuple[ChannelKey, ...]
    include_outbound_links: bool = False
    revision: int = 1

    def __post_init__(self) -> None:
        object.__setattr__(self, "view_id", _required_text(self.view_id, "view_id"))
        object.__setattr__(self, "workspace_id", _required_text(self.workspace_id, "workspace_id"))
        # ``frozen=True`` protects the attribute, but callers can still pass a
        # mutable list unless the value is normalised at the boundary.
        object.__setattr__(self, "selected_channels", tuple(self.selected_channels))
        if not self.selected_channels:
            raise FixtureValidationError("a retrieval view must select at least one channel")
        if len(set(self.selected_channels)) != len(self.selected_channels):
            raise FixtureValidationError("a retrieval view cannot repeat a channel")
        if any(channel.workspace_id != self.workspace_id for channel in self.selected_channels):
            raise FixtureValidationError("view channels must belong to the view workspace")
        if not isinstance(self.include_outbound_links, bool):
            raise FixtureValidationError("include_outbound_links must be boolean")
        if not isinstance(self.revision, int) or self.revision < 1:
            raise FixtureValidationError("view revision must be a positive integer")

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any]) -> "RetrievalView":
        value = _mapping(value, "view")
        channels = value.get("selected_channels", value.get("channels"))
        if not isinstance(channels, Sequence) or isinstance(channels, (str, bytes)):
            raise FixtureValidationError("view.selected_channels must be an array")
        keys = tuple(
            ChannelKey.from_mapping(item, field_name="view.selected_channels[]") for item in channels
        )
        return cls(
            view_id=value.get("view_id"),
            workspace_id=value.get("workspace_id"),
            selected_channels=keys,
            include_outbound_links=value.get("include_outbound_links", False),
            revision=value.get("revision", 1),
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "view_id": self.view_id,
            "workspace_id": self.workspace_id,
            "selected_channels": [channel.as_dict() for channel in self.selected_channels],
            "include_outbound_links": self.include_outbound_links,
            "revision": self.revision,
        }


@dataclass(frozen=True, slots=True)
class ScopeSnapshot:
    """Immutable materialised policy used for one retrieval attempt."""

    view_id: str
    workspace_id: str
    destination: ChannelKey
    origin: ChannelKey
    purpose: str
    view_revision: int
    policy_revision: int
    view_channels: tuple[ChannelKey, ...]
    authorized_channels: tuple[ChannelKey, ...]
    fingerprint: str

    def __post_init__(self) -> None:
        object.__setattr__(self, "view_channels", tuple(self.view_channels))
        object.__setattr__(self, "authorized_channels", tuple(self.authorized_channels))
        if self.destination.workspace_id != self.workspace_id or self.origin.workspace_id != self.workspace_id:
            raise FixtureValidationError("snapshot endpoints must belong to the snapshot workspace")
        if not set(self.authorized_channels).issubset(self.view_channels):
            raise FixtureValidationError("authorized channels must come from the materialised view")

    def allows(self, channel: ChannelKey) -> bool:
        return channel in self.authorized_channels

    def as_dict(self) -> dict[str, Any]:
        return {
            "view_id": self.view_id,
            "workspace_id": self.workspace_id,
            "destination": self.destination.as_dict(),
            "origin": self.origin.as_dict(),
            "purpose": self.purpose,
            "view_revision": self.view_revision,
            "policy_revision": self.policy_revision,
            "view_channels": [channel.as_dict() for channel in self.view_channels],
            "authorized_channels": [channel.as_dict() for channel in self.authorized_channels],
            "fingerprint": self.fingerprint,
        }


@dataclass(frozen=True, slots=True)
class SearchHit:
    """A ranked evidence reference; ``content`` is hidden from repr/logs."""

    rank: int
    score: float
    observation_id: str
    workspace_id: str
    platform: str
    source_room_id: str
    content: str = field(repr=False)

    @property
    def channel(self) -> ChannelKey:
        return ChannelKey(self.workspace_id, self.platform, self.source_room_id)

    @property
    def source_platform(self) -> str:
        return self.platform

    @property
    def source_room(self) -> str:
        return self.source_room_id


@dataclass(frozen=True, slots=True)
class SearchResult:
    """Search state and evidence references without exposing text in repr."""

    state: str
    hits: tuple[SearchHit, ...]
    snapshot: ScopeSnapshot | None
    fresh_snapshot: ScopeSnapshot | None = None
    cache_hit: bool = False
    indexed_count: int = 0

    @property
    def status(self) -> str:
        return self.state

    @property
    def evidence(self) -> tuple[SearchHit, ...]:
        return self.hits

    @property
    def results(self) -> tuple[SearchHit, ...]:
        return self.hits

    @property
    def invalidated(self) -> bool:
        return self.state == "invalidated"


@dataclass(frozen=True, slots=True)
class FixtureData:
    """Validated synthetic fixture and its deterministic fingerprint."""

    schema_version: str
    channels: tuple[ChannelSpec, ...]
    links: tuple[ChannelLink, ...]
    grants: tuple[AccessGrant, ...]
    views: tuple[RetrievalView, ...]
    observations: tuple[Observation, ...]
    fingerprint: str

    def build_scope(self) -> "ChannelScope":
        scope = ChannelScope()
        # Stable ordering makes the resulting revision sequence reproducible.
        for channel in sorted(self.channels, key=lambda item: item.key.identity):
            scope.register_channel(channel)
        for view in sorted(self.views, key=lambda item: (item.workspace_id, item.view_id)):
            scope.create_view(
                view.view_id,
                view.workspace_id,
                view.selected_channels,
                include_outbound_links=view.include_outbound_links,
            )
        for link in sorted(self.links, key=lambda item: (item.source.identity, item.target.identity)):
            scope.link_channels(link.source, link.target)
        for grant in sorted(
            self.grants,
            key=lambda item: (item.source.identity, item.destination.identity, item.purpose),
        ):
            scope.grant_access(grant.source, grant.destination, grant.purpose)
        return scope


def _coerce_channel(value: ChannelKey | ChannelSpec | Mapping[str, Any]) -> ChannelKey:
    if isinstance(value, ChannelKey):
        return value
    if isinstance(value, ChannelSpec):
        return value.key
    if isinstance(value, Mapping):
        return ChannelKey.from_mapping(value)
    raise ChannelScopeError("channel must be a ChannelKey, ChannelSpec, or object")


def _coerce_observation(value: Observation | Mapping[str, Any]) -> Observation:
    if isinstance(value, Observation):
        return value
    if isinstance(value, Mapping):
        return Observation.from_mapping(value)
    raise ChannelScopeError("observation must be an Observation or object")


class ChannelScope:
    """In-memory registry and scoped lexical retrieval evaluator."""

    def __init__(self) -> None:
        self._channels: dict[ChannelKey, ChannelSpec] = {}
        self._links: set[ChannelLink] = set()
        self._grants: set[AccessGrant] = set()
        self._views: dict[tuple[str, str], RetrievalView] = {}
        self._workspace_revisions: dict[str, int] = {}
        self._cache: dict[tuple[str, str, str, int, str], tuple[SearchHit, ...]] = {}

    @classmethod
    def from_fixture(cls, fixture: FixtureData) -> "ChannelScope":
        return fixture.build_scope()

    @property
    def channels(self) -> tuple[ChannelSpec, ...]:
        return tuple(self._channels[key] for key in sorted(self._channels))

    @property
    def links(self) -> tuple[ChannelLink, ...]:
        return tuple(sorted(self._links, key=lambda item: (item.source.identity, item.target.identity)))

    @property
    def grants(self) -> tuple[AccessGrant, ...]:
        return tuple(
            sorted(self._grants, key=lambda item: (item.source.identity, item.destination.identity, item.purpose))
        )

    def _bump(self, workspace_id: str) -> int:
        self._workspace_revisions[workspace_id] = self._workspace_revisions.get(workspace_id, 0) + 1
        # A mutation must never leave a mutable-policy cache reachable.
        self._cache.clear()
        return self._workspace_revisions[workspace_id]

    def _require_registered(self, channel: ChannelKey) -> ChannelKey:
        if channel not in self._channels:
            raise UnknownChannelError("unknown channel")
        return channel

    def register_channel(self, spec: ChannelSpec) -> ChannelSpec:
        if not isinstance(spec, ChannelSpec):
            raise ChannelScopeError("register_channel expects ChannelSpec")
        existing = self._channels.get(spec.key)
        if existing is not None:
            # Source-room identities are immutable.  Even an identical duplicate
            # is rejected so a fixture cannot accidentally hide a duplicate row.
            raise DuplicateChannelError("duplicate channel identity")
        self._channels[spec.key] = spec
        self._bump(spec.workspace_id)
        return spec

    add_channel = register_channel

    def create_view(
        self,
        view_id: str,
        workspace_id: str,
        selected_channels: Iterable[ChannelKey | ChannelSpec | Mapping[str, Any]],
        *,
        include_outbound_links: bool = False,
    ) -> RetrievalView:
        workspace_id = _required_text(workspace_id, "workspace_id")
        view_id = _required_text(view_id, "view_id")
        channels = tuple(_coerce_channel(item) for item in selected_channels)
        if not channels:
            raise FixtureValidationError("a retrieval view must select at least one channel")
        for channel in channels:
            self._require_registered(channel)
            if channel.workspace_id != workspace_id:
                raise FixtureValidationError("view channels must belong to the view workspace")
        key = (workspace_id, view_id)
        if key in self._views:
            raise DuplicatePolicyError("duplicate view")
        revision = self._bump(workspace_id)
        view = RetrievalView(
            view_id=view_id,
            workspace_id=workspace_id,
            selected_channels=channels,
            include_outbound_links=include_outbound_links,
            revision=revision,
        )
        self._views[key] = view
        return view

    add_view = create_view

    def update_view(
        self,
        view_id: str,
        workspace_id: str,
        selected_channels: Iterable[ChannelKey | ChannelSpec | Mapping[str, Any]],
        *,
        include_outbound_links: bool = False,
    ) -> RetrievalView:
        """Replace a view definition while keeping its identity and revisioned scope."""

        key = (_required_text(workspace_id, "workspace_id"), _required_text(view_id, "view_id"))
        if key not in self._views:
            raise UnknownViewError("unknown retrieval view")

        # Validate the replacement completely before changing the existing
        # view.  In particular, an unknown channel or malformed option must
        # leave the last-known-good policy available.
        channels = tuple(_coerce_channel(item) for item in selected_channels)
        for channel in channels:
            self._require_registered(channel)
        current = self._views[key]
        candidate = RetrievalView(
            view_id=key[1],
            workspace_id=key[0],
            selected_channels=channels,
            include_outbound_links=include_outbound_links,
            revision=current.revision,
        )
        revision = self._bump(key[0])
        updated = RetrievalView(
            view_id=candidate.view_id,
            workspace_id=candidate.workspace_id,
            selected_channels=candidate.selected_channels,
            include_outbound_links=candidate.include_outbound_links,
            revision=revision,
        )
        self._views[key] = updated
        return updated

    def _view(self, view_id: str, workspace_id: str) -> RetrievalView:
        try:
            return self._views[(workspace_id, view_id)]
        except KeyError as exc:
            raise UnknownViewError("unknown retrieval view") from exc

    def link_channels(
        self,
        source: ChannelKey | ChannelSpec | Mapping[str, Any],
        target: ChannelKey | ChannelSpec | Mapping[str, Any],
    ) -> ChannelLink:
        source_key = self._require_registered(_coerce_channel(source))
        target_key = self._require_registered(_coerce_channel(target))
        link = ChannelLink(source_key, target_key)
        if link in self._links:
            raise DuplicatePolicyError("duplicate channel link")
        self._links.add(link)
        self._bump(link.workspace_id)
        return link

    add_link = link_channels

    def unlink_channels(
        self,
        source: ChannelKey | ChannelSpec | Mapping[str, Any],
        target: ChannelKey | ChannelSpec | Mapping[str, Any],
    ) -> bool:
        source_key = _coerce_channel(source)
        target_key = _coerce_channel(target)
        link = ChannelLink(source_key, target_key)
        if link not in self._links:
            return False
        self._links.remove(link)
        self._bump(link.workspace_id)
        return True

    remove_link = unlink_channels

    def grant_access(
        self,
        source: ChannelKey | ChannelSpec | Mapping[str, Any],
        destination: ChannelKey | ChannelSpec | Mapping[str, Any],
        purpose: str = DEFAULT_PURPOSE,
    ) -> AccessGrant:
        source_key = self._require_registered(_coerce_channel(source))
        destination_key = self._require_registered(_coerce_channel(destination))
        grant = AccessGrant(source_key, destination_key, purpose)
        if grant in self._grants:
            raise DuplicatePolicyError("duplicate access grant")
        self._grants.add(grant)
        self._bump(grant.workspace_id)
        return grant

    add_grant = grant_access

    def revoke_access(
        self,
        source: ChannelKey | ChannelSpec | Mapping[str, Any],
        destination: ChannelKey | ChannelSpec | Mapping[str, Any],
        purpose: str = DEFAULT_PURPOSE,
    ) -> bool:
        grant = AccessGrant(_coerce_channel(source), _coerce_channel(destination), purpose)
        if grant not in self._grants:
            return False
        self._grants.remove(grant)
        self._bump(grant.workspace_id)
        return True

    remove_grant = revoke_access

    def _materialise_view(self, view: RetrievalView) -> tuple[ChannelKey, ...]:
        roots = tuple(view.selected_channels)
        if not view.include_outbound_links:
            return roots
        root_set = set(roots)
        # Deliberately iterate roots only: no reverse edge and no transitive
        # closure through a target's outgoing links.
        linked = {link.target for link in self._links if link.source in root_set}
        return tuple(sorted(root_set | linked))

    def _has_grant(self, source: ChannelKey, destination: ChannelKey, purpose: str) -> bool:
        return AccessGrant(source, destination, purpose) in self._grants

    def snapshot(
        self,
        view_id: str,
        destination: ChannelKey | ChannelSpec | Mapping[str, Any],
        purpose: str = DEFAULT_PURPOSE,
        *,
        origin: ChannelKey | ChannelSpec | Mapping[str, Any] | None = None,
    ) -> ScopeSnapshot:
        destination_key = self._require_registered(_coerce_channel(destination))
        purpose = _required_text(purpose, "purpose")
        origin_key = destination_key if origin is None else self._require_registered(_coerce_channel(origin))
        if origin_key.workspace_id != destination_key.workspace_id:
            raise FixtureValidationError("origin and destination must share a workspace")
        view = self._view(view_id, destination_key.workspace_id)
        view_channels = self._materialise_view(view)
        authorized = tuple(
            channel
            for channel in view_channels
            if self._has_grant(channel, destination_key, purpose)
            and (
                origin_key == destination_key
                or self._has_grant(channel, origin_key, purpose)
            )
        )
        revision = self._workspace_revisions.get(view.workspace_id, 0)
        payload = {
            "schema_version": SCHEMA_VERSION,
            "view": view.as_dict(),
            "view_channels": [channel.as_dict() for channel in view_channels],
            "authorized_channels": [channel.as_dict() for channel in authorized],
            "destination": destination_key.as_dict(),
            "origin": origin_key.as_dict(),
            "purpose": purpose,
            "policy_revision": revision,
            # Include the active policy state as well as its monotonic revision;
            # this keeps fingerprints deterministic for independently rebuilt scopes.
            "links": [link.as_dict() for link in self.links if link.workspace_id == view.workspace_id],
            "grants": [grant.as_dict() for grant in self.grants if grant.workspace_id == view.workspace_id],
        }
        return ScopeSnapshot(
            view_id=view.view_id,
            workspace_id=view.workspace_id,
            destination=destination_key,
            origin=origin_key,
            purpose=purpose,
            view_revision=view.revision,
            policy_revision=revision,
            view_channels=view_channels,
            authorized_channels=authorized,
            fingerprint=deterministic_fingerprint(payload),
        )

    scope_snapshot = snapshot

    def _refresh_snapshot(self, snapshot: ScopeSnapshot) -> ScopeSnapshot | None:
        try:
            return self.snapshot(
                snapshot.view_id,
                snapshot.destination,
                snapshot.purpose,
                origin=snapshot.origin,
            )
        except ChannelScopeError:
            return None

    @staticmethod
    def _records_fingerprint(records: Sequence[Observation]) -> str:
        # Hash content without logging or returning it.  This prevents a cache
        # built for an older observation generation from being reused.
        payload = [
            {
                "observation_id": record.observation_id,
                "channel": record.channel.as_dict(),
                "ordinal": record.ordinal,
                "content_sha256": hashlib.sha256(record.content.encode("utf-8")).hexdigest(),
            }
            for record in records
        ]
        return deterministic_fingerprint(payload)

    def scoped_search(
        self,
        records: Iterable[Observation | Mapping[str, Any]],
        query: str,
        *,
        view_id: str | None = None,
        destination: ChannelKey | ChannelSpec | Mapping[str, Any] | None = None,
        purpose: str = DEFAULT_PURPOSE,
        origin: ChannelKey | ChannelSpec | Mapping[str, Any] | None = None,
        limit: int = 10,
        variant: str = "bigram",
        snapshot: ScopeSnapshot | None = None,
        policy_supplier: Callable[[], ScopeSnapshot | None] | None = None,
    ) -> SearchResult:
        """Search only records allowed by an immutable policy snapshot.

        ``policy_supplier`` is called after ranking, immediately before evidence
        is returned.  A supplier may revoke a grant as part of that call to
        exercise the mid-search invalidation path in tests.
        """

        if not isinstance(query, str):
            raise ChannelScopeError("query must be a string")
        if not isinstance(limit, int) or limit < 1:
            raise ChannelScopeError("limit must be a positive integer")

        initial = snapshot
        if initial is None:
            if view_id is None or destination is None:
                # A missing destination is an explicit default-deny request.
                return SearchResult("denied_by_policy", (), None, indexed_count=0)
            initial = self.snapshot(view_id, destination, purpose, origin=origin)
        elif view_id is not None and view_id != initial.view_id:
            raise ChannelScopeError("view_id does not match supplied snapshot")

        # A caller-supplied snapshot is only a hint.  Rebuild the complete
        # snapshot from this registry before filtering or indexing; otherwise a
        # forged ``authorized_channels`` list could widen the lexical input.
        trusted_initial = self._refresh_snapshot(initial)
        if trusted_initial is None or trusted_initial != initial:
            return SearchResult(
                "invalidated",
                (),
                initial,
                fresh_snapshot=trusted_initial,
                indexed_count=0,
            )
        initial = trusted_initial

        all_records = tuple(_coerce_observation(record) for record in records)
        authorized = set(initial.authorized_channels)
        filtered = tuple(
            record
            for record in all_records
            if record.workspace_id == initial.workspace_id and record.channel in authorized
        )
        records_fp = self._records_fingerprint(filtered)
        cache_key = (initial.fingerprint, query, variant, limit, records_fp)
        cached = self._cache.get(cache_key)
        if cached is None:
            hits: tuple[SearchHit, ...]
            if not filtered or not query.strip() or not initial.authorized_channels:
                hits = ()
            else:
                index = LexicalIndex(variant)
                internal_ids: list[str] = []
                lookup: dict[str, Observation] = {}
                rows: list[tuple[str, str]] = []
                for index_position, record in enumerate(filtered):
                    internal_id = f"observation-{index_position:08d}"
                    internal_ids.append(internal_id)
                    lookup[internal_id] = record
                    rows.append((internal_id, record.content))
                # This is the security-critical boundary: only ``filtered`` is
                # ever inserted into the lexical index.
                index.add_many(rows)
                ranked = index.search(query, limit=max(limit, len(rows)))
                ordered = sorted(ranked, key=lambda item: (item[1], item[0]))[:limit]
                hits = tuple(
                    SearchHit(
                        rank=rank,
                        score=float(score),
                        observation_id=lookup[internal_id].observation_id,
                        workspace_id=lookup[internal_id].workspace_id,
                        platform=lookup[internal_id].platform,
                        source_room_id=lookup[internal_id].source_room_id,
                        content=lookup[internal_id].content,
                    )
                    for rank, (internal_id, score) in enumerate(ordered, start=1)
                )
            self._cache[cache_key] = hits
            cache_hit = False
        else:
            hits = cached
            cache_hit = True

        # The callback is only a race-test hook.  Its return value is never
        # trusted as policy; the registry is always re-read independently.
        if policy_supplier is not None:
            policy_supplier()
        fresh = self._refresh_snapshot(initial)

        if fresh is None or fresh.fingerprint != initial.fingerprint:
            # Do not return even a cached hit built under the old revision.
            return SearchResult(
                "invalidated",
                (),
                initial,
                fresh_snapshot=fresh,
                cache_hit=False,
                indexed_count=len(filtered),
            )
        if any(not fresh.allows(hit.channel) for hit in hits):
            return SearchResult(
                "invalidated",
                (),
                initial,
                fresh_snapshot=fresh,
                cache_hit=False,
                indexed_count=len(filtered),
            )

        state = "candidates" if hits else (
            "denied_by_policy" if not initial.authorized_channels else "empty_in_allowed_scope"
        )
        return SearchResult(
            state,
            hits,
            initial,
            fresh_snapshot=fresh,
            cache_hit=cache_hit,
            indexed_count=len(filtered),
        )

    search = scoped_search


def search_scoped(
    scope: ChannelScope,
    records: Iterable[Observation | Mapping[str, Any]],
    query: str,
    **kwargs: Any,
) -> SearchResult:
    """Small functional adapter for evaluation scripts and integration tests."""

    return scope.scoped_search(records, query, **kwargs)


def _parse_fixture(data: Mapping[str, Any]) -> FixtureData:
    data = _mapping(data, "fixture")
    version = data.get("schema_version", data.get("version"))
    if version != SCHEMA_VERSION:
        raise FixtureValidationError("unsupported fixture schema version")

    raw_channels = data.get("channels", [])
    raw_links = data.get("links", [])
    raw_grants = data.get("grants", data.get("access_grants", []))
    raw_views = data.get("views", [])
    raw_observations = data.get("observations", [])
    for field_name, value in (
        ("channels", raw_channels),
        ("links", raw_links),
        ("grants", raw_grants),
        ("views", raw_views),
        ("observations", raw_observations),
    ):
        if not isinstance(value, list):
            raise FixtureValidationError(f"{field_name} must be an array")

    channels = tuple(ChannelSpec.from_mapping(item) for item in raw_channels)
    by_key: dict[ChannelKey, ChannelSpec] = {}
    for channel in channels:
        if channel.key in by_key:
            raise DuplicateChannelError("duplicate channel identity")
        by_key[channel.key] = channel

    links = tuple(ChannelLink.from_mapping(item) for item in raw_links)
    seen_links: set[ChannelLink] = set()
    for link in links:
        if link.source not in by_key or link.target not in by_key:
            raise UnknownChannelError("link references an unregistered channel")
        if link in seen_links:
            raise DuplicatePolicyError("duplicate link")
        seen_links.add(link)

    grants = tuple(AccessGrant.from_mapping(item) for item in raw_grants)
    seen_grants: set[AccessGrant] = set()
    for grant in grants:
        if grant.source not in by_key or grant.destination not in by_key:
            raise UnknownChannelError("grant references an unregistered channel")
        if grant in seen_grants:
            raise DuplicatePolicyError("duplicate grant")
        seen_grants.add(grant)

    views = tuple(RetrievalView.from_mapping(item) for item in raw_views)
    seen_views: set[tuple[str, str]] = set()
    for view in views:
        view_key = (view.workspace_id, view.view_id)
        if view_key in seen_views:
            raise DuplicatePolicyError("duplicate view")
        seen_views.add(view_key)
        for channel in view.selected_channels:
            if channel not in by_key:
                raise UnknownChannelError("view references an unregistered channel")

    observations = tuple(Observation.from_mapping(item) for item in raw_observations)
    seen_observations: set[tuple[str, str, str, str]] = set()
    for observation in observations:
        if observation.channel not in by_key:
            raise UnknownChannelError("observation references an unregistered channel")
        observation_key = (*observation.channel.identity, observation.observation_id)
        if observation_key in seen_observations:
            raise FixtureValidationError("duplicate observation provenance")
        seen_observations.add(observation_key)

    payload = {
        "schema_version": SCHEMA_VERSION,
        "channels": sorted((_canonical(item) for item in channels), key=_json),
        "links": sorted((_canonical(item) for item in links), key=_json),
        "grants": sorted((_canonical(item) for item in grants), key=_json),
        "views": sorted((_canonical(item) for item in views), key=_json),
        "observations": sorted((_canonical(item) for item in observations), key=_json),
    }
    return FixtureData(
        schema_version=SCHEMA_VERSION,
        channels=channels,
        links=links,
        grants=grants,
        views=views,
        observations=observations,
        fingerprint=deterministic_fingerprint(payload),
    )


def load_fixture_data(data: Mapping[str, Any]) -> FixtureData:
    """Validate and load an already decoded synthetic fixture object."""

    return _parse_fixture(data)


def load_fixture(path: str | Path) -> FixtureData:
    """Read one synthetic fixture without writing any derived artifact."""

    fixture_path = Path(path)
    try:
        with fixture_path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise FixtureValidationError(f"could not read fixture {fixture_path}: {exc}") from exc
    return load_fixture_data(data)


def load_json_fixture(path: str | Path) -> FixtureData:
    """Compatibility alias with an explicit name for evaluation scripts."""

    return load_fixture(path)


__all__ = [
    "SCHEMA_VERSION",
    "DEFAULT_PURPOSE",
    "AccessGrant",
    "ChannelKey",
    "ChannelLink",
    "ChannelScope",
    "ChannelScopeError",
    "ChannelSpec",
    "DuplicateChannelError",
    "DuplicatePolicyError",
    "FixtureData",
    "FixtureValidationError",
    "Observation",
    "RetrievalView",
    "ScopeSnapshot",
    "SearchHit",
    "SearchResult",
    "UnknownChannelError",
    "UnknownViewError",
    "deterministic_fingerprint",
    "load_fixture",
    "load_fixture_data",
    "load_json_fixture",
    "search_scoped",
]
