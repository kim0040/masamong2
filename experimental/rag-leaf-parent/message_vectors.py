"""Message-level vectors (retrieval.message_dense) — mirror of internal/memory/search/message_dense.go
and messagefilter.go. Off by default; nothing here runs unless `retrieval.message_dense.enabled` is set
and `message_vecs.npy` + `message_leaf.npy` exist next to the other index artifacts.

A leaf of eight lines averages its lines into one vector, which can bury a one-line fact. Scoring the
messages themselves finds the leaf that holds it; the run joins the finished fusion as one more weighted
list. testdata/message_dense/fixture.json (internal/memory/search) pins both implementations.
"""

from __future__ import annotations

import hashlib
import json
import unicodedata
from functools import lru_cache
from pathlib import Path
from typing import Any, Iterable, Sequence

import numpy as np

# messages with fewer meaningful characters than this get no vector of their own (search.MinMessageRunes)
MIN_MESSAGE_RUNES = 6
# MRL message vectors default to at most this many dimensions (engine.defaultMaxMessageDims).
MAX_MESSAGE_DIMS = 512
# The measurement-only override accepts widths through this value (engine.maxMeasuredMessageDims).
MAX_MEASURED_MESSAGE_DIMS = 1024

# Load only when catalogue-based classification is needed. A detached sandbox can still import this
# module and use an explicit embed_truncatable flag without the product's hosts.json.
_EMBED_WIDTHS_PATH = Path(__file__).resolve().parents[2] / "internal/providers/openai/hosts.json"


@lru_cache(maxsize=1)
def _embed_widths() -> dict[str, int]:
    try:
        widths = json.loads(_EMBED_WIDTHS_PATH.read_text(encoding="utf-8"))["embed_widths"]
        if not isinstance(widths, dict):
            raise ValueError("embed_widths must be an object")
        return widths
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise RuntimeError(
            f"MRL 모델 판정용 카탈로그를 읽을 수 없음: {_EMBED_WIDTHS_PATH} ({type(exc).__name__}: {exc}). "
            "설정에 embed_truncatable을 명시하거나 hosts.json을 함께 배치하세요."
        ) from exc


def reference_name(model: str) -> str:
    """openai.ReferenceName, including Go's unicode.IsSpace set for TrimSpace."""
    whitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
    # Go uses one-rune lowercase mappings, whereas Python expands İ and applies contextual sigma.
    lowered = "".join(ch.lower()[0] for ch in model.strip(whitespace))
    return lowered.rsplit("/", 1)[-1].split(":", 1)[0]


def embed_full_width(model: str) -> int:
    """openai.EmbedFullWidth: a positive width identifies a known MRL model, otherwise zero."""
    return max(0, int(_embed_widths().get(reference_name(model), 0)))

MESSAGE_RUN_NAME = "message_dense"  # candidates.MESSAGE_RUN
MESSAGE_VECS_NAME = "message_vecs.npy"
MESSAGE_LEAF_NAME = "message_leaf.npy"
MESSAGE_SCALE_NAME = "message_scale.npy"
MESSAGE_META_NAME = "message_vecs_meta.json"
MESSAGE_FINGERPRINT_VERSION = 2
# weight of the message run in the final fusion when the config leaves it unset (search.DefaultMessageDenseWeight)
MESSAGE_RRF_WEIGHT = 0.1


def _meaningful_rune(ch: str) -> bool:
    """search.meaningfulRune: Unicode letters (L*) and numbers (N*) count, except the Hangul compatibility
    jamo U+3131–U+318E (laughter and keyboard noise). White space, control and format characters,
    punctuation, symbols and emoji, combining marks, private-use and unassigned code points never count.

    Python's unicodedata and Go's unicode tables ship different Unicode versions: a code point assigned
    as a letter or number in one version and still unassigned in the other can count in only one of the
    two (rare; it only changes whether such a message gets a vector of its own)."""

    cp = ord(ch)
    if 0x3131 <= cp <= 0x318E:
        return False
    return unicodedata.category(ch)[0] in ("L", "N")


def meaningful_runes(text: str) -> int:
    return sum(1 for ch in text if _meaningful_rune(ch))


def message_eligible(content: str) -> bool:
    return meaningful_runes(content.strip()) >= MIN_MESSAGE_RUNES


def message_text(label: str, content: str) -> str:
    """store.MessageText: the speaker label and the trimmed content."""

    return f"{label}: {content.strip()}"


def message_dims(embed_dims: int = 0, *, embed_truncatable: bool = False, override: int = 0) -> int:
    """Return message width; non-MRL uses actual leaf width, zero while that width is unresolved."""

    measurement_dims = int(override or 0)
    if 0 < measurement_dims <= MAX_MEASURED_MESSAGE_DIMS:
        return measurement_dims
    if not embed_truncatable:
        return max(0, int(embed_dims or 0))
    d = int(embed_dims or 0)
    return d if 0 < d <= MAX_MESSAGE_DIMS else MAX_MESSAGE_DIMS


def _normalize_rows(vecs: np.ndarray) -> np.ndarray:
    return vecs / (np.linalg.norm(vecs, axis=1, keepdims=True) + 1e-9)


def fit_message_vectors(vecs: np.ndarray, dims: int) -> np.ndarray:
    """When dims is positive, cut to that MRL width and normalize again; 0 keeps full vectors."""

    vecs = np.asarray(vecs, dtype=np.float32)
    if dims > 0 and vecs.ndim == 2 and vecs.shape[1] > dims:
        vecs = vecs[:, :dims]
    return np.ascontiguousarray(_normalize_rows(vecs), dtype=np.float32)


def fit_message_query(q_vec: np.ndarray, dims: int) -> np.ndarray | None:
    """The question is embedded once in the leaf space; the message matrix may be narrower, so cut it to
    that and normalise again. None when the question is narrower than the messages (Go: no run)."""

    q = np.asarray(q_vec, dtype=np.float32)
    if q.shape[0] < dims:
        return None
    q = np.ascontiguousarray(q[:dims], dtype=np.float32)
    return q / (np.linalg.norm(q) + 1e-9)


def message_dense_run(
    vectors: np.ndarray,
    leaf_of_row: Sequence[int] | np.ndarray,
    leaf_ids: Sequence[str],
    q_vec: np.ndarray,
    *,
    allowed_ids: set[str] | None,
    limit: int,
) -> tuple[list[str], dict[str, float], int]:
    """Score every message row of the allowed leaves, rank the *leaves* by their best message, keep the
    first `limit`. Returns (leaf ids, {leaf id: best cosine}, rows compared). Ties go to the message that
    comes first in row order (search.Index.messageDense)."""

    if vectors is None or len(vectors) == 0 or limit <= 0:
        return [], {}, 0
    mq = fit_message_query(q_vec, int(vectors.shape[1]))
    if mq is None:
        return [], {}, 0
    leaf_of = np.asarray(leaf_of_row, dtype=np.int64)
    rows = np.arange(len(vectors))
    if allowed_ids is not None:
        ok = np.fromiter((leaf_ids[i] in allowed_ids for i in range(len(leaf_ids))), dtype=bool, count=len(leaf_ids))
        rows = rows[ok[leaf_of]]
        if len(rows) == 0:
            return [], {}, 0
    sims = vectors[rows] @ mq
    best: dict[int, int] = {}  # leaf → position in sims of its best message (first on ties)
    order: list[int] = []
    for k in range(len(sims)):
        lf = int(leaf_of[rows[k]])
        if lf not in best:
            best[lf] = k
            order.append(lf)
        elif float(sims[k]) > float(sims[best[lf]]):
            best[lf] = k
    order.sort(key=lambda lf: (-float(sims[best[lf]]), best[lf]))
    order = order[:limit]
    ids = [str(leaf_ids[lf]) for lf in order]
    return ids, {str(leaf_ids[lf]): float(sims[best[lf]]) for lf in order}, int(len(sims))


def project_message_rows(
    leaf_observations: Sequence[Sequence[str]], has: Any
) -> list[tuple[str, int]]:
    """search.ProjectMessages: the canonical (observation id, leaf row) pairs. `leaf_observations` lists,
    for each leaf in leaf order, the observation ids it holds in message order; `has(id)` says whether the
    message gets a vector. A message in several leaves (leaf_overlap) is projected onto every one of them,
    and the pair order — the leaf order, then the message order inside the leaf — is the row order of the
    vectors and the tie-break of the run. It never depends on observation ids or times."""

    return [(oid, li) for li, obs in enumerate(leaf_observations) for oid in obs if has(oid)]


def select_message_rows(
    messages: Sequence[dict[str, Any]], leaves: Sequence[dict[str, Any]]
) -> tuple[list[tuple[str, str]], list[tuple[str, int]]]:
    """The messages that get a vector. Returns (observations, pairs): observations are the distinct eligible
    messages as (observation id, text) in order of first appearance in the canonical pair order — each is
    embedded once — and pairs are the canonical (observation id, leaf row) projection, one per leaf that
    holds the message (project_message_rows)."""

    text_of: dict[str, str] = {}
    for m in messages:
        content = str(m.get("content") or "")
        if message_eligible(content):
            text_of[str(m.get("observation_id"))] = message_text(str(m.get("actor_label") or "?"), content)
    pairs = project_message_rows(
        [[str(o) for o in (leaf.get("observation_ids") or [])] for leaf in leaves], text_of.__contains__
    )
    seen: set[str] = set()
    observations: list[tuple[str, str]] = []
    for oid, _li in pairs:
        if oid not in seen:
            seen.add(oid)
            observations.append((oid, text_of[oid]))
    return observations, pairs


def message_fingerprint(
    *,
    embed_model: str,
    dims: int,
    observations: Sequence[tuple[str, str]],
    pairs: Sequence[tuple[str, int]],
    leaf_ids: Sequence[str],
) -> dict[str, Any]:
    """What the message artifacts (and the checkpoint of an interrupted run) were made from: the embedding
    model and effective dimensionality, every embedded observation with the hash of its text in canonical
    order, and the whole projection (observation → leaf id). A changed leaf boundary, text, label, model or
    dimensionality changes it, so nothing stale is reused or loaded. The recipe itself is not part of it: a
    recipe change that leaves the embedded texts and the observation → leaf projection exactly as they were
    (for example another split setting that cuts the same leaves) keeps the vectors, which is safe because
    the vectors depend on nothing else."""

    texts = hashlib.sha256()
    for oid, text in observations:
        texts.update(oid.encode("utf-8") + b"\t" + hashlib.sha256(text.encode("utf-8")).digest() + b"\n")
    mapping = hashlib.sha256()
    for oid, li in pairs:
        mapping.update(f"{oid}\t{leaf_ids[li]}\n".encode("utf-8"))
    return {
        "version": MESSAGE_FINGERPRINT_VERSION,
        "embed_model": str(embed_model),
        "dims": int(dims),
        "observations": len(observations),
        "texts_sha": texts.hexdigest(),
        "pairs": len(pairs),
        "mapping_sha": mapping.hexdigest(),
    }


def read_message_fingerprint(data_dir: Path) -> dict[str, Any] | None:
    try:
        meta = json.loads((Path(data_dir) / MESSAGE_META_NAME).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, TypeError):
        return None
    fp = meta.get("fingerprint") if isinstance(meta, dict) else None
    return fp if isinstance(fp, dict) else None


def load_message_vectors(
    data_dir: Path, *, fingerprint: dict[str, Any], leaf_rows: Sequence[int]
) -> tuple[np.ndarray, np.ndarray] | None:
    """(vectors float32 (n, d), leaf row per vector) or None. The artifacts are used only when their
    recorded fingerprint equals the one worked out from the current index (`fingerprint`) and their leaf
    rows are exactly the current projection (`leaf_rows`, in canonical order): anything else — missing,
    unreadable, another model or dimensionality, changed texts or leaf boundaries — is ignored, never
    fatal, because the run is an optional extra."""

    vp, lp = Path(data_dir) / MESSAGE_VECS_NAME, Path(data_dir) / MESSAGE_LEAF_NAME
    if not (vp.exists() and lp.exists()):
        return None
    if read_message_fingerprint(data_dir) != fingerprint:
        return None
    try:
        raw = np.load(vp)
        if raw.dtype == np.int8:  # int8 rows + one float32 scale per row, as the product stores them
            scale = np.asarray(np.load(Path(data_dir) / MESSAGE_SCALE_NAME), dtype=np.float32)
            if raw.ndim != 2 or scale.shape != (len(raw),):
                return None
            vecs = raw.astype(np.float32) * scale[:, None]
        else:
            # float16 written by an earlier version of this module: scored the way the product scores the same
            # rows (float16 -> float32 -> int8 + scale on load), not as the unquantised floats
            q, scale = quantize_rows(np.asarray(raw, dtype=np.float32))
            vecs = q.astype(np.float32) * scale[:, None]
        leaf = np.asarray(np.load(lp), dtype=np.int64)
    except (OSError, ValueError):
        return None
    if vecs.ndim != 2 or len(vecs) != len(leaf) or len(vecs) == 0:
        return None
    if len(leaf) != len(leaf_rows) or not np.array_equal(leaf, np.asarray(leaf_rows, dtype=np.int64)):
        return None
    return vecs, leaf


def quantize_rows(vecs: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """int8 rows and one float32 scale per row, the quantisation the Go index applies (scale = largest
    absolute component / 127, components rounded half away from zero and clipped to +-127), so the stored
    artifact and the product's in-memory matrix hold the same numbers: about dims+4 bytes per vector instead
    of 2*dims."""

    v = np.asarray(vecs, dtype=np.float32)
    if v.ndim != 2:
        raise ValueError("message vectors must be a 2-D array")
    scale = np.abs(v).max(axis=1, initial=0.0).astype(np.float32) / np.float32(127)
    inv = np.zeros_like(scale)
    np.divide(np.float32(1), scale, out=inv, where=scale > 0)
    r = v * inv[:, None]
    r = np.where(r >= 0, r + np.float32(0.5), r - np.float32(0.5))
    return np.clip(np.trunc(r), -127, 127).astype(np.int8), scale


def write_message_vectors(
    data_dir: Path, vecs: np.ndarray, leaf_rows: Iterable[int], *, meta: dict[str, Any]
) -> None:
    """Writes the vectors and leaf rows first and the metadata (with the fingerprint) last, so a run that
    stops half way leaves artifacts that no fingerprint vouches for."""

    data_dir = Path(data_dir)
    (data_dir / MESSAGE_META_NAME).unlink(missing_ok=True)
    q, scale = quantize_rows(vecs)
    np.save(data_dir / MESSAGE_VECS_NAME, q)
    np.save(data_dir / MESSAGE_SCALE_NAME, scale)
    np.save(data_dir / MESSAGE_LEAF_NAME, np.asarray(list(leaf_rows), dtype=np.int32))
    (data_dir / MESSAGE_META_NAME).write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
