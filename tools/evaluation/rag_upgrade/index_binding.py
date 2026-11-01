"""Index fingerprint and gold↔index binding (no stale-eval hardcoding)."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any, Mapping, Sequence


class BindingError(ValueError):
    """Gold is not bound to the current index, or the binding is invalid."""


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path, *, max_bytes: int | None = None) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        if max_bytes is None:
            for chunk in iter(lambda: f.read(1024 * 1024), b""):
                h.update(chunk)
        else:
            remaining = max_bytes
            while remaining > 0:
                chunk = f.read(min(1024 * 1024, remaining))
                if not chunk:
                    break
                h.update(chunk)
                remaining -= len(chunk)
    return h.hexdigest()


def stable_leaf_id_from_playground_leaf(
    leaf: Mapping[str, Any],
    *,
    segmentation_version: str,
) -> str:
    """Stable id that survives observation-hash churn when span/room/seg stay fixed."""

    room = str(leaf.get("source_room_id") or "").strip()
    if "start" not in leaf or "end" not in leaf:
        raise BindingError("leaf missing source_room_id/start/end")
    start = int(leaf["start"])
    end = int(leaf["end"])
    if not room or start < 0 or end <= start:
        raise BindingError("leaf missing source_room_id/start/end")
    material = "\0".join(
        [room, segmentation_version, f"start={start}", f"end={end}"]
    )
    return hashlib.sha256(material.encode("utf-8")).hexdigest()[:32]


def segmentation_version_from_cfg(cfg: Mapping[str, Any]) -> str:
    base = (
        f"leaf{cfg.get('leaf_size')}-o{cfg.get('leaf_overlap')}-"
        f"lim{cfg.get('leaf_limit')}-prefix{int(bool(cfg.get('context_prefix', True)))}"
    )
    split = str(cfg.get("leaf_split") or "fixed")
    if split != "fixed":
        gap = int(float(cfg.get("gap_minutes") or 30))
        base += f"-{split}{gap}"
    return base


def compute_corpus_binding(
    messages: Sequence[Mapping[str, Any]],
    cfg: Mapping[str, Any],
) -> dict[str, Any]:
    """Observation-id stream + room recipe. Independent of leaf split / RRF weights."""

    oids = [str(m.get("observation_id") or "") for m in messages]
    payload = {
        "observation_ids": oids,
        "rooms": cfg.get("rooms"),
        "message_limit": cfg.get("message_limit"),
        "collection": cfg.get("collection"),
        "room_merge": cfg.get("room_merge") or {},
    }
    blob = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return {
        "schema_version": 1,
        "message_count": len(messages),
        "corpus_fingerprint": _sha256_bytes(blob.encode("utf-8")),
    }


def compute_index_binding(
    *,
    leaves: Sequence[Mapping[str, Any]],
    messages: Sequence[Mapping[str, Any]],
    leaf_vecs_path: Path,
    cfg: Mapping[str, Any],
    meta: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """Fingerprint an index so gold cannot silently score against a rebuilt corpus."""

    seg = segmentation_version_from_cfg(cfg)
    leaf_rows = []
    for leaf in leaves:
        oids = [str(x) for x in (leaf.get("observation_ids") or [])]
        leaf_rows.append(
            {
                "doc_id": str(leaf.get("doc_id")),
                "source_room_id": str(leaf.get("source_room_id")),
                "start": int(leaf.get("start") or 0),
                "end": int(leaf.get("end") or 0),
                "observation_ids": oids,
                "stable_leaf_id": stable_leaf_id_from_playground_leaf(
                    leaf, segmentation_version=seg
                ),
            }
        )
    leaves_blob = json.dumps(leaf_rows, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    leaves_sha = _sha256_bytes(leaves_blob.encode("utf-8"))
    # Full vector file hash is expensive; bind shape + prefix + size + mtime-independent bytes sample.
    st = leaf_vecs_path.stat()
    vec_bind = {
        "path": leaf_vecs_path.name,
        "size_bytes": st.st_size,
        "sha256_head_1m": _sha256_file(leaf_vecs_path, max_bytes=1024 * 1024),
        "sha256_tail_64k": _sha256_bytes(_read_tail(leaf_vecs_path, 65536)),
    }
    recipe = {
        "leaf_size": cfg.get("leaf_size"),
        "leaf_overlap": cfg.get("leaf_overlap"),
        "leaf_limit": cfg.get("leaf_limit"),
        "context_prefix": cfg.get("context_prefix"),
        "lexical": cfg.get("lexical"),
        "rrf_weights": cfg.get("rrf_weights"),
        "rooms": cfg.get("rooms"),
        "message_limit": cfg.get("message_limit"),
        "summary_enabled": cfg.get("summary_enabled"),
    }
    if meta and isinstance(meta.get("index_fingerprint"), Mapping):
        recipe["meta_index_fingerprint"] = meta["index_fingerprint"]
    binding = {
        "schema_version": 1,
        "segmentation_version": seg,
        "leaf_count": len(leaves),
        "message_count": len(messages),
        "leaves_sha256": leaves_sha,
        "vectors": vec_bind,
        "recipe": recipe,
    }
    binding["index_fingerprint"] = _sha256_bytes(
        json.dumps(binding, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode(
            "utf-8"
        )
    )
    binding["corpus_binding"] = compute_corpus_binding(messages, cfg)
    return binding


def _read_tail(path: Path, n: int) -> bytes:
    with path.open("rb") as f:
        f.seek(0, 2)
        size = f.tell()
        f.seek(max(0, size - n))
        return f.read(n)


def assert_gold_bound_to_index(
    gold: Mapping[str, Any],
    binding: Mapping[str, Any],
    *,
    allow_corpus_only: bool = True,
) -> None:
    gb = gold.get("index_binding")
    if not isinstance(gb, Mapping):
        raise BindingError(
            "gold missing index_binding; regenerate with regen_bound_gold.py "
            "(refusing to score unbound/stale gold)"
        )
    want_fp = str(gb.get("index_fingerprint") or "")
    have_fp = str(binding.get("index_fingerprint") or "")
    gold_corpus = gold.get("corpus_binding")
    if not isinstance(gold_corpus, Mapping):
        gold_corpus = gb.get("corpus_binding") if isinstance(gb.get("corpus_binding"), Mapping) else None
    have_corpus = binding.get("corpus_binding") if isinstance(binding.get("corpus_binding"), Mapping) else None
    if want_fp and want_fp == have_fp:
        for key in ("leaf_count", "message_count", "leaves_sha256"):
            if gb.get(key) is not None and gb.get(key) != binding.get(key):
                raise BindingError(f"gold binding field mismatch: {key}")
        return
    if (
        allow_corpus_only
        and isinstance(gold_corpus, Mapping)
        and isinstance(have_corpus, Mapping)
        and gold_corpus.get("corpus_fingerprint")
        and gold_corpus.get("corpus_fingerprint") == have_corpus.get("corpus_fingerprint")
    ):
        return
    raise BindingError(
        "gold index_fingerprint mismatch: gold is bound to a different index. "
        "Regenerate gold against the current index (regen_bound_gold.py). "
        f"gold={want_fp[:12] or 'missing'}… current={have_fp[:12]}…"
    )


def validate_question_wants_against_leaves(
    questions: Sequence[Mapping[str, Any]],
    leaves: Sequence[Mapping[str, Any]],
) -> None:
    """Every want observation id must exist in the current leaf inventory."""

    universe: set[str] = set()
    by_stable: dict[str, set[str]] = {}
    by_doc: dict[str, set[str]] = {}
    for leaf in leaves:
        oids = {str(x) for x in (leaf.get("observation_ids") or [])}
        universe |= oids
        doc_id = str(leaf.get("doc_id") or "")
        if doc_id:
            by_doc[doc_id] = oids
        sid = str(leaf.get("stable_leaf_id") or "")
        if sid:
            by_stable[sid] = oids

    for q in questions:
        qid = str(q.get("question_id") or q.get("query") or "?")
        want = {str(x) for x in (q.get("want_observation_ids") or q.get("expected_evidence_ids") or [])}
        if not want:
            if q.get("expected_abstention"):
                continue
            raise BindingError(f"{qid}: empty want_observation_ids")
        missing = want - universe
        if missing:
            raise BindingError(
                f"{qid}: {len(missing)} want ids absent from current index "
                "(gold must be regenerated; do not remap with ad-hoc heuristics)"
            )
