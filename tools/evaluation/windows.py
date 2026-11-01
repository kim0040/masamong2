"""Deterministic, source-room bounded evaluation windows.

This module is deliberately independent from the raw-corpus reader.  Callers
hand it observations (usually after applying ``semantic_eligible``); the
builder keeps one buffer per ``platform``/``collection``/``source_room`` key.
That matters for the merged Kakao collection: collection membership does not
make messages from two rooms adjacent, and an interleaved input stream must
not make them share a window.
"""
from __future__ import annotations

import hashlib
import json
from collections import OrderedDict
from dataclasses import asdict, dataclass
from typing import Any, Iterable, Mapping


class WindowConfigError(ValueError):
    """Raised when a window recipe could create ambiguous or invalid output."""


@dataclass(frozen=True, slots=True)
class WindowConfig:
    """The complete fixed-size window recipe used by the evaluation harness.

    ``boundary`` is explicit even though the only supported value currently
    is ``source_room``.  A future cross-room recipe must therefore be a
    deliberate configuration change instead of an accidental consequence of
    collection-level iteration.
    """

    size: int = 20
    overlap: int = 0
    boundary: str = "source_room"
    recipe_version: str = "window-v2"

    def validate(self) -> "WindowConfig":
        if isinstance(self.size, bool) or not isinstance(self.size, int) or self.size <= 0:
            raise WindowConfigError("window size must be a positive integer")
        if isinstance(self.overlap, bool) or not isinstance(self.overlap, int):
            raise WindowConfigError("window overlap must be an integer")
        if self.overlap < 0 or self.overlap >= self.size:
            raise WindowConfigError("window overlap must satisfy 0 <= overlap < size")
        if self.boundary != "source_room":
            raise WindowConfigError("only source_room window boundaries are supported")
        if not self.recipe_version:
            raise WindowConfigError("window recipe_version must be non-empty")
        return self

    def as_dict(self) -> dict[str, Any]:
        self.validate()
        return asdict(self)

    @classmethod
    def from_mapping(cls, value: Mapping[str, Any] | None) -> "WindowConfig":
        if value is None:
            return cls().validate()
        allowed = {"size", "overlap", "boundary", "recipe_version"}
        unknown = set(value) - allowed
        if unknown:
            raise WindowConfigError(f"unknown window config field(s): {', '.join(sorted(unknown))}")
        return cls(
            size=value.get("size", 20),
            overlap=value.get("overlap", 0),
            boundary=value.get("boundary", "source_room"),
            recipe_version=value.get("recipe_version", "window-v2"),
        ).validate()


def _json_default(value: Any) -> Any:
    if hasattr(value, "as_dict"):
        return value.as_dict()
    if hasattr(value, "__dict__"):
        return value.__dict__
    raise TypeError(f"not JSON serializable: {type(value).__name__}")


def _fingerprint(value: Any) -> str:
    encoded = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        default=_json_default,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def _get(observation: Any, name: str, default: Any = None) -> Any:
    if isinstance(observation, Mapping):
        return observation.get(name, default)
    return getattr(observation, name, default)


def _required_text(observation: Any, name: str) -> str:
    value = _get(observation, name, None)
    if value is None or not str(value).strip():
        raise ValueError(f"window observation requires non-empty {name}")
    return str(value)


def _room_key(observation: Any) -> tuple[str, str, str]:
    """Return a provenance-safe room key.

    ``source_room_id`` alone is not enough when two platforms use the same
    room label.  Empty fields are represented explicitly rather than being
    silently filled from another source.
    """

    return (
        _required_text(observation, "platform"),
        _required_text(observation, "collection_id"),
        _required_text(observation, "source_room_id"),
    )


def _source_row(observation: Any, fallback_ordinal: int) -> dict[str, Any]:
    observation_id = _get(observation, "observation_id", None)
    if observation_id is None or not str(observation_id).strip():
        raise ValueError("window observation requires non-empty observation_id")
    source_ordinal = _get(observation, "source_ordinal", None)
    if source_ordinal is None:
        source_ordinal = _get(observation, "record_ordinal", None)
    return {
        "observation_id": str(observation_id),
        "source_ordinal": source_ordinal,
        "source_fingerprint": _get(observation, "source_fingerprint", None),
        "source_file": _get(observation, "source_file", None),
        "source_kind": _get(observation, "source_kind", None),
    }


def _make_document(
    observations: list[Any],
    *,
    room_key: tuple[str, str, str],
    config: WindowConfig,
    stream_ordinals: list[int],
) -> dict[str, Any]:
    platform, collection_id, source_room_id = room_key
    source_rows = [
        _source_row(observation, ordinal)
        for observation, ordinal in zip(observations, stream_ordinals)
    ]
    observation_ids = [row["observation_id"] for row in source_rows]
    contents = [str(_get(observation, "content", "") or "") for observation in observations]
    actor_labels = [_get(observation, "actor_label", None) for observation in observations]
    body = "\n".join(
        f"{actor or '?'}: {content}"
        for actor, content in zip(actor_labels, contents)
    )
    source_provenance = {
        "platform": platform,
        "collection_id": collection_id,
        "source_room_id": source_room_id,
        "observations": source_rows,
    }
    # Include IDs and source hashes in the content fingerprint.  Equal text
    # from two rooms or two exports must never alias in a cache.
    content_fingerprint = _fingerprint({
        "schema": "window-content/v2",
        "platform": platform,
        "collection_id": collection_id,
        "source_room_id": source_room_id,
        "observation_ids": observation_ids,
        "contents": contents,
        "actor_labels": actor_labels,
        "actor_accounts": [
            _get(observation, "actor_account", None) for observation in observations
        ],
        "parsed_times": [
            _get(observation, "parsed_time", None) for observation in observations
        ],
        "source_provenance": source_provenance,
    })
    # The provenance and observation IDs are already covered by the digest.
    # Keep raw room/user/message identifiers out of the ID itself so a caller
    # can safely use it in aggregate logs.
    doc_id = f"window:{content_fingerprint}"
    document = {
        "doc_id": doc_id,
        "platform": platform,
        "collection_id": collection_id,
        "source_room_id": source_room_id,
        # ``room`` is retained as a compatibility alias for the old pilot.
        "room": source_room_id,
        "source_provenance": source_provenance,
        "observation_ids": observation_ids,
        "content_fingerprint": content_fingerprint,
        "start": _get(observations[0], "parsed_time", None),
        "end": _get(observations[-1], "parsed_time", None),
        "body": body,
        "contents": contents,
        "texts": contents,
        "speakers": [actor or "?" for actor in actor_labels],
        "speaker_accounts": [
            _get(observation, "actor_account", None) for observation in observations
        ],
        "n": len(observations),
        "window_config": config.as_dict(),
    }
    # The window fingerprint includes the recipe as well as its observations;
    # this is the value used by cache inputs and run manifests.
    document["window_fingerprint"] = _fingerprint({
        "schema": "window/v2",
        "document": document,
    })
    return document


def build_windows(
    observations: Iterable[Any],
    config: WindowConfig | Mapping[str, Any] | None = None,
    *,
    limit: int | None = None,
) -> list[dict[str, Any]]:
    """Build bounded windows while preserving every source-room stream.

    Full windows are emitted as soon as a room buffer reaches ``size``.  At
    EOF a tail is emitted only when it contains at least one observation that
    was not already covered by the previous full window.  Thus an
    overlap-only tail is never duplicated, while inputs shorter than ``size``
    and the final partial segment are retained.
    """

    if config is None:
        recipe = WindowConfig()
    elif isinstance(config, WindowConfig):
        recipe = config
    else:
        recipe = WindowConfig.from_mapping(config)
    recipe.validate()
    if limit is not None:
        if isinstance(limit, bool) or not isinstance(limit, int) or limit < 0:
            raise ValueError("window limit must be a non-negative integer or None")
        if limit == 0:
            return []

    # OrderedDict keeps deterministic tail order without collapsing rooms.
    buffers: "OrderedDict[tuple[str, str, str], tuple[list[Any], list[int], bool]]" = OrderedDict()
    seen_observations: dict[str, str] = {}
    documents: list[dict[str, Any]] = []
    for stream_ordinal, observation in enumerate(observations):
        key = _room_key(observation)
        observation_id = _get(observation, "observation_id", None)
        if observation_id is None or not str(observation_id).strip():
            raise ValueError("window observation requires non-empty observation_id")
        observation_id = str(observation_id)
        observation_signature = _fingerprint({
            "platform": key[0],
            "collection_id": key[1],
            "source_room_id": key[2],
            "observation_id": observation_id,
            "content": _get(observation, "content", ""),
            "actor_label": _get(observation, "actor_label", None),
            "actor_account": _get(observation, "actor_account", None),
            "parsed_time": _get(observation, "parsed_time", None),
            "source_ordinal": _get(observation, "source_ordinal", None),
            "source_fingerprint": _get(observation, "source_fingerprint", None),
        })
        prior_signature = seen_observations.get(observation_id)
        if prior_signature is not None and prior_signature != observation_signature:
            raise ValueError(f"conflicting duplicate observation_id: {observation_id}")
        seen_observations[observation_id] = observation_signature
        if key not in buffers:
            buffers[key] = ([], [], False)
        buffer, ordinals, emitted_full = buffers[key]
        buffer.append(observation)
        ordinals.append(stream_ordinal)
        while len(buffer) >= recipe.size:
            documents.append(
                _make_document(
                    buffer[: recipe.size],
                    room_key=key,
                    config=recipe,
                    stream_ordinals=ordinals[: recipe.size],
                )
            )
            emitted_full = True
            if limit is not None and len(documents) >= limit:
                return documents[:limit]
            keep = recipe.overlap
            if keep:
                del buffer[: recipe.size - keep]
                del ordinals[: recipe.size - keep]
            else:
                buffer.clear()
                ordinals.clear()
            buffers[key] = (buffer, ordinals, emitted_full)

    # A tail of exactly ``overlap`` observations is already fully covered by
    # the last emitted window.  A shorter stream has no previous full window,
    # so it is emitted regardless of overlap.
    for key, (buffer, ordinals, emitted_full) in buffers.items():
        if not buffer:
            continue
        if len(buffer) > recipe.overlap or not emitted_full:
            documents.append(
                _make_document(
                    list(buffer),
                    room_key=key,
                    config=recipe,
                    stream_ordinals=list(ordinals),
                )
            )
            if limit is not None and len(documents) >= limit:
                return documents[:limit]
    return documents[:limit] if limit is not None else documents


__all__ = ["WindowConfig", "WindowConfigError", "build_windows"]
