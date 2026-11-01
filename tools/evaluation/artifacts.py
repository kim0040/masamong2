"""Validated, atomic artifacts for the offline evaluation harness.

The first pilot reused ``.npy`` files by row count alone.  That is unsafe:
same-count windows, reordered documents, a different embedding model, and a
changed prompt can all silently produce a wrong evaluation.  Every cache in
this module has a sidecar with the complete input fingerprint and a checksum.
An artifact without its sidecar is deliberately unusable.

This module contains no provider or secret loading.  It is safe to import
from offline analysis commands and from synthetic unit tests.
"""
from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any, Mapping, Sequence

import numpy as np


ARTIFACT_SCHEMA = "masamong-evaluation-artifact/v2"
MANIFEST_SCHEMA = "masamong-evaluation-manifest/v2"
DEFAULT_OUTPUT_DIR = Path(".private/eval/bounded-v2")


class ArtifactValidationError(ValueError):
    """Raised when an artifact is missing, stale, corrupt, or untrusted."""


def output_dir(value: str | os.PathLike[str] | None = None) -> Path:
    """Return the configured output directory without creating it.

    Historical pilot artifacts live directly under ``.private/eval``.  The
    default for this bounded harness is a new child directory, so a CLI run
    cannot overwrite those historical files.  ``EVAL_OUTPUT_DIR`` remains an
    explicit escape hatch for callers that already have an isolated target.
    """

    if value is not None:
        return Path(value)
    configured = os.environ.get("EVAL_OUTPUT_DIR")
    return Path(configured) if configured else DEFAULT_OUTPUT_DIR


def _json_default(value: Any) -> Any:
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, (np.integer,)):
        return int(value)
    if isinstance(value, (np.floating,)):
        return float(value)
    if isinstance(value, np.ndarray):
        return value.tolist()
    if hasattr(value, "as_dict"):
        return value.as_dict()
    if hasattr(value, "__dict__"):
        return value.__dict__
    raise TypeError(f"not JSON serializable: {type(value).__name__}")


def canonical_bytes(value: Any) -> bytes:
    """Encode a JSON-compatible value deterministically."""

    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        default=_json_default,
        allow_nan=False,
    ).encode("utf-8")


def fingerprint(value: Any) -> str:
    """Return a SHA-256 fingerprint of a deterministic JSON value."""

    return hashlib.sha256(canonical_bytes(value)).hexdigest()


def documents_fingerprint(documents: Sequence[Mapping[str, Any]]) -> str:
    """Fingerprint ordered window documents, including their provenance."""

    # Keep list order: retrieval rows are positional, and reordering equal
    # sized documents must invalidate dense and gold caches.
    return fingerprint({
        "schema": "evaluation-documents/v2",
        "documents": list(documents),
    })


def cache_input_fingerprint(
    content: Any,
    *,
    provider: Any,
    model: Any,
    recipe: Any,
    kind: str,
) -> str:
    """Fingerprint all inputs that can change a cache result.

    ``provider``, ``model`` and ``recipe`` are intentionally opaque structured
    values, rather than a provider name/model string shortcut.  New fields in
    any of them therefore invalidate old artifacts automatically.
    """

    return fingerprint({
        "schema": "evaluation-cache-input/v2",
        "kind": kind,
        "content": content,
        "provider": provider,
        "model": model,
        "recipe": recipe,
    })


def _sidecar_path(path: Path) -> Path:
    return Path(str(path) + ".meta.json")


def sidecar_path(path: str | os.PathLike[str]) -> Path:
    """Public sidecar path helper used by callers and tests."""

    return _sidecar_path(Path(path))


def _ensure_parent(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)


def _atomic_bytes(path: Path, data: bytes) -> None:
    _ensure_parent(path)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def _atomic_npy(path: Path, array: np.ndarray) -> None:
    _ensure_parent(path)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "wb") as handle:
            np.save(handle, array, allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _array_sha256(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array)
    return hashlib.sha256(contiguous.tobytes(order="C")).hexdigest()


def _write_sidecar(path: Path, metadata: Mapping[str, Any]) -> None:
    _atomic_bytes(
        _sidecar_path(path),
        canonical_bytes(dict(metadata)) + b"\n",
    )


def _read_sidecar(path: Path) -> dict[str, Any]:
    sidecar = _sidecar_path(path)
    if not sidecar.exists():
        raise ArtifactValidationError(f"missing artifact sidecar: {sidecar.name}")
    try:
        value = json.loads(sidecar.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ArtifactValidationError(f"invalid artifact sidecar: {sidecar.name}") from exc
    if not isinstance(value, dict):
        raise ArtifactValidationError(f"artifact sidecar is not an object: {sidecar.name}")
    if value.get("schema") != ARTIFACT_SCHEMA:
        raise ArtifactValidationError(f"unsupported artifact sidecar schema: {sidecar.name}")
    return value


def read_sidecar(path: str | os.PathLike[str]) -> dict[str, Any]:
    return _read_sidecar(Path(path))


def _check_expected(
    metadata: Mapping[str, Any],
    *,
    input_fingerprint: str | None,
    provider: Any,
    model: Any,
    recipe: Any,
    manifest_ref: str | None,
) -> None:
    expected = {
        "input_fingerprint": input_fingerprint,
        "provider": provider,
        "model": model,
        "recipe": recipe,
        "manifest_ref": manifest_ref,
    }
    for field, wanted in expected.items():
        if wanted is not None and metadata.get(field) != wanted:
            raise ArtifactValidationError(f"artifact {field} mismatch")


def save_array_cache(
    path: str | os.PathLike[str],
    array: np.ndarray,
    *,
    input_fingerprint: str,
    provider: Any,
    model: Any,
    recipe: Any,
    kind: str = "embedding",
    manifest_ref: str | None = None,
) -> dict[str, Any]:
    """Atomically save a finite NumPy array and its validation sidecar."""

    target = Path(path)
    value = np.asarray(array)
    if value.dtype.hasobject:
        raise ArtifactValidationError("object dtype arrays are not allowed")
    try:
        finite = bool(np.isfinite(value).all())
    except TypeError as exc:
        raise ArtifactValidationError("array dtype is not numerically finite-checkable") from exc
    if not finite:
        raise ArtifactValidationError("array contains non-finite values")
    value = np.ascontiguousarray(value)
    _atomic_npy(target, value)
    metadata: dict[str, Any] = {
        "schema": ARTIFACT_SCHEMA,
        "artifact_type": "numpy_array",
        "kind": kind,
        "path": target.name,
        "input_fingerprint": input_fingerprint,
        "provider": provider,
        "model": model,
        "recipe": recipe,
        "manifest_ref": manifest_ref,
        "dtype": str(value.dtype),
        "shape": list(value.shape),
        "finite": True,
        "array_sha256": _array_sha256(value),
        "checksum_sha256": _file_sha256(target),
    }
    _write_sidecar(target, metadata)
    return metadata


def load_array_cache(
    path: str | os.PathLike[str],
    *,
    input_fingerprint: str | None = None,
    provider: Any = None,
    model: Any = None,
    recipe: Any = None,
    expected_shape: Sequence[int] | None = None,
    manifest_ref: str | None = None,
) -> np.ndarray:
    """Load only an array whose sidecar and bytes match every expectation."""

    target = Path(path)
    if not target.exists():
        raise ArtifactValidationError(f"missing artifact: {target}")
    metadata = _read_sidecar(target)
    if metadata.get("artifact_type") != "numpy_array":
        raise ArtifactValidationError("artifact is not a NumPy array")
    if _file_sha256(target) != metadata.get("checksum_sha256"):
        raise ArtifactValidationError("array file checksum mismatch")
    _check_expected(
        metadata,
        input_fingerprint=input_fingerprint,
        provider=provider,
        model=model,
        recipe=recipe,
        manifest_ref=manifest_ref,
    )
    try:
        value = np.load(target, allow_pickle=False)
    except Exception as exc:
        raise ArtifactValidationError("invalid or truncated NumPy artifact") from exc
    if not isinstance(value, np.ndarray):
        raise ArtifactValidationError("NumPy artifact did not contain an array")
    if str(value.dtype) != metadata.get("dtype") or list(value.shape) != metadata.get("shape"):
        raise ArtifactValidationError("array dtype/shape metadata mismatch")
    if expected_shape is not None and tuple(value.shape) != tuple(expected_shape):
        raise ArtifactValidationError("array shape mismatch")
    if metadata.get("finite") is not True:
        raise ArtifactValidationError("array finite guard missing")
    try:
        if not bool(np.isfinite(value).all()):
            raise ArtifactValidationError("array contains non-finite values")
    except TypeError as exc:
        raise ArtifactValidationError("array dtype is not numerically finite-checkable") from exc
    if _array_sha256(value) != metadata.get("array_sha256"):
        raise ArtifactValidationError("array content checksum mismatch")
    return value


def save_json_cache(
    path: str | os.PathLike[str],
    value: Any,
    *,
    input_fingerprint: str,
    provider: Any,
    model: Any,
    recipe: Any,
    kind: str = "json",
    manifest_ref: str | None = None,
) -> dict[str, Any]:
    """Atomically save JSON payload and a sidecar with input provenance."""

    target = Path(path)
    payload = canonical_bytes(value) + b"\n"
    _atomic_bytes(target, payload)
    metadata: dict[str, Any] = {
        "schema": ARTIFACT_SCHEMA,
        "artifact_type": "json",
        "kind": kind,
        "path": target.name,
        "input_fingerprint": input_fingerprint,
        "provider": provider,
        "model": model,
        "recipe": recipe,
        "manifest_ref": manifest_ref,
        "payload_fingerprint": fingerprint(value),
        "checksum_sha256": _file_sha256(target),
    }
    _write_sidecar(target, metadata)
    return metadata


def load_json_cache(
    path: str | os.PathLike[str],
    *,
    input_fingerprint: str | None = None,
    provider: Any = None,
    model: Any = None,
    recipe: Any = None,
    manifest_ref: str | None = None,
) -> Any:
    target = Path(path)
    if not target.exists():
        raise ArtifactValidationError(f"missing artifact: {target}")
    metadata = _read_sidecar(target)
    if metadata.get("artifact_type") != "json":
        raise ArtifactValidationError("artifact is not JSON")
    if _file_sha256(target) != metadata.get("checksum_sha256"):
        raise ArtifactValidationError("JSON artifact checksum mismatch")
    _check_expected(
        metadata,
        input_fingerprint=input_fingerprint,
        provider=provider,
        model=model,
        recipe=recipe,
        manifest_ref=manifest_ref,
    )
    try:
        value = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ArtifactValidationError("invalid or truncated JSON artifact") from exc
    if fingerprint(value) != metadata.get("payload_fingerprint"):
        raise ArtifactValidationError("JSON payload checksum mismatch")
    return value


def write_manifest(path: str | os.PathLike[str], manifest: Mapping[str, Any]) -> dict[str, Any]:
    """Write a self-checking run manifest and its sidecar atomically."""

    target = Path(path)
    value = dict(manifest)
    value.setdefault("schema", MANIFEST_SCHEMA)
    if value["schema"] != MANIFEST_SCHEMA:
        raise ArtifactValidationError("unsupported manifest schema")
    value.pop("manifest_fingerprint", None)
    value["manifest_fingerprint"] = fingerprint(value)
    payload = canonical_bytes(value) + b"\n"
    _atomic_bytes(target, payload)
    metadata = {
        "schema": ARTIFACT_SCHEMA,
        "artifact_type": "manifest",
        "kind": "run_manifest",
        "path": target.name,
        "manifest_schema": MANIFEST_SCHEMA,
        "manifest_fingerprint": value["manifest_fingerprint"],
        "checksum_sha256": _file_sha256(target),
    }
    _write_sidecar(target, metadata)
    return value


def load_manifest(path: str | os.PathLike[str]) -> dict[str, Any]:
    """Read and verify a run manifest; unmanifested legacy files are rejected."""

    target = Path(path)
    if not target.exists():
        raise ArtifactValidationError(f"missing manifest: {target}")
    metadata = _read_sidecar(target)
    if metadata.get("artifact_type") != "manifest":
        raise ArtifactValidationError("artifact sidecar is not a manifest")
    if _file_sha256(target) != metadata.get("checksum_sha256"):
        raise ArtifactValidationError("manifest checksum mismatch")
    try:
        value = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ArtifactValidationError("invalid manifest") from exc
    if not isinstance(value, dict) or value.get("schema") != MANIFEST_SCHEMA:
        raise ArtifactValidationError("unsupported manifest schema")
    actual = value.get("manifest_fingerprint")
    without = dict(value)
    without.pop("manifest_fingerprint", None)
    if actual != fingerprint(without) or actual != metadata.get("manifest_fingerprint"):
        raise ArtifactValidationError("manifest fingerprint mismatch")
    return value


def resolve_manifest_artifact(manifest_path: str | os.PathLike[str], reference: str) -> Path:
    """Resolve a manifest-relative artifact while rejecting path traversal."""

    root = Path(manifest_path).resolve().parent
    candidate = (root / reference).resolve()
    try:
        candidate.relative_to(root)
    except ValueError as exc:
        raise ArtifactValidationError("manifest artifact escapes output directory") from exc
    return candidate


__all__ = [
    "ARTIFACT_SCHEMA",
    "MANIFEST_SCHEMA",
    "ArtifactValidationError",
    "cache_input_fingerprint",
    "canonical_bytes",
    "documents_fingerprint",
    "fingerprint",
    "load_array_cache",
    "load_json_cache",
    "load_manifest",
    "output_dir",
    "read_sidecar",
    "resolve_manifest_artifact",
    "save_array_cache",
    "save_json_cache",
    "sidecar_path",
    "write_manifest",
]
