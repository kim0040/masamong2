"""Index layer fingerprints and reuse policy.

Compare *existing* artifact fingerprints to the wanted recipe *before* writing
the new wanted value. A checkpoint that already stores the new recipe is not
evidence that on-disk leaves/embeddings match it.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

MANIFEST_NAME = "index_manifest.json"


class ArtifactConflict(ValueError):
    """On-disk files do not match the wanted recipe; caller must rebuild or refuse."""


def _sha(payload: Any) -> str:
    blob = json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str)
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def source_fingerprint(
    *,
    collection: str,
    rooms: Sequence[str],
    room_merge: Mapping[str, Any] | None,
    message_limit: Any,
    message_ids: Sequence[str] | None = None,
    body_digest: str | None = None,
) -> dict[str, Any]:
    return {
        "collection": collection,
        "rooms": list(rooms),
        "room_merge": dict(room_merge or {}),
        "message_limit": message_limit,
        "message_ids_sha": _sha(list(message_ids)) if message_ids is not None else None,
        "body_digest": body_digest,
    }


def segmentation_fingerprint(cfg: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "leaf_size": cfg.get("leaf_size"),
        "leaf_overlap": cfg.get("leaf_overlap"),
        "leaf_limit": cfg.get("leaf_limit"),
        "leaf_split": cfg.get("leaf_split") or "fixed",
        "gap_minutes": cfg.get("gap_minutes"),
        "context_prefix": cfg.get("context_prefix"),
        "baseline_enabled": bool(cfg.get("baseline_enabled", False)),
        "baseline_window_size": cfg.get("baseline_window_size"),
        "baseline_window_limit": cfg.get("baseline_window_limit"),
        "lexical": cfg.get("lexical"),
        "document_char_limit": cfg.get("document_char_limit"),
    }


def summary_fingerprint(cfg: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "summary_enabled": bool(cfg.get("summary_enabled", False)),
        "summary_model": (cfg.get("models") or {}).get("summary") if isinstance(cfg.get("models"), Mapping) else cfg.get("summary_model"),
        "summary_max_tokens": cfg.get("summary_max_tokens"),
        "summary_prompt_sha": _sha(str(cfg.get("summary_prompt") or "default")),
    }


def embedding_fingerprint(cfg: Mapping[str, Any]) -> dict[str, Any]:
    models = cfg.get("models") if isinstance(cfg.get("models"), Mapping) else {}
    return {
        "embed_model": models.get("embed") or cfg.get("embed_model"),
        "embed_dimension": cfg.get("embed_dimension"),
        "embed_instruction": cfg.get("embed_instruction") or cfg.get("embed_query_instruction"),
        "document_char_limit": cfg.get("document_char_limit"),
    }


def wanted_layers(cfg: Mapping[str, Any], *, message_ids: Sequence[str] | None = None, body_digest: str | None = None) -> dict[str, Any]:
    return {
        "source": source_fingerprint(
            collection=str(cfg.get("collection") or ""),
            rooms=list(cfg.get("rooms") or []),
            room_merge=cfg.get("room_merge") if isinstance(cfg.get("room_merge"), Mapping) else {},
            message_limit=cfg.get("message_limit"),
            message_ids=message_ids,
            body_digest=body_digest,
        ),
        "segmentation": segmentation_fingerprint(cfg),
        "summary": summary_fingerprint(cfg),
        "embedding": embedding_fingerprint(cfg),
        "experiment": cfg.get("experiment"),
    }


def layers_digest(layers: Mapping[str, Any]) -> str:
    return _sha(layers)


@dataclass(frozen=True)
class ReusePlan:
    segments: bool
    summaries: bool
    embeddings: bool
    reason: str
    existing_digest: str | None
    wanted_digest: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "reuse_segments": self.segments,
            "reuse_summaries": self.summaries,
            "reuse_embeddings": self.embeddings,
            "reason": self.reason,
            "existing_digest": self.existing_digest,
            "wanted_digest": self.wanted_digest,
        }


def load_manifest(path: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    try:
        row = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return row if isinstance(row, dict) else None


def decide_reuse(
    existing: Mapping[str, Any] | None,
    wanted: Mapping[str, Any],
    *,
    files_ready: bool,
) -> ReusePlan:
    """Reuse only when the on-disk manifest (not a freshly written checkpoint) matches."""

    want_d = layers_digest(wanted)
    if not files_ready or not existing:
        return ReusePlan(False, False, False, "missing_manifest_or_files", None, want_d)
    stored = existing.get("layers") if isinstance(existing.get("layers"), Mapping) else existing
    have_d = existing.get("digest") or layers_digest(stored)
    if have_d != want_d:
        same_seg = stored.get("segmentation") == wanted.get("segmentation") and stored.get("source") == wanted.get("source")
        same_sum = same_seg and stored.get("summary") == wanted.get("summary")
        same_emb = same_sum and stored.get("embedding") == wanted.get("embedding")
        if not same_seg:
            return ReusePlan(False, False, False, "segmentation_changed", have_d, want_d)
        if not same_sum:
            return ReusePlan(True, False, False, "summary_changed", have_d, want_d)
        if not same_emb:
            return ReusePlan(True, True, False, "embedding_changed", have_d, want_d)
        return ReusePlan(False, False, False, "fingerprint_mismatch", have_d, want_d)
    return ReusePlan(True, True, True, "match", have_d, want_d)


def segmentation_matches_leaves(leaves: Sequence[Mapping[str, Any]], cfg: Mapping[str, Any]) -> bool:
    """True when on-disk leaf spans fit the wanted leaf_size (+ overlap). Catches F1 poison."""

    size = int(cfg.get("leaf_size") or 0)
    overlap = int(cfg.get("leaf_overlap") or 0)
    if size <= 0 or not leaves:
        return False
    cap = size + max(0, overlap)
    for leaf in leaves:
        n = int(leaf.get("end") or 0) - int(leaf.get("start") or 0)
        if n > cap:
            return False
    return True


def write_manifest(path: Path, layers: Mapping[str, Any], *, extra: Mapping[str, Any] | None = None) -> dict[str, Any]:
    payload = {"schema": 1, "layers": dict(layers), "digest": layers_digest(layers)}
    if extra:
        payload.update(dict(extra))
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(path)
    return payload
