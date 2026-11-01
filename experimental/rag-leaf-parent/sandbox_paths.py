"""Isolate RAG-ified sandbox files under test-sandbox/data (never the product DB)."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Mapping

HERE = Path(__file__).resolve().parent


def repo_root() -> Path:
    for cand in [HERE, *HERE.parents]:
        if (cand / "docs" / "TEST_SANDBOX.md").is_file() or (
            cand / "contracts" / "sandbox" / "tool-parity.json"
        ).is_file():
            return cand
    return HERE.parents[1]


def resolve_sandbox_data_dir(cfg: Mapping[str, Any] | None = None) -> Path:
    env = str(os.environ.get("MASAMONG_SANDBOX_DATA") or "").strip()
    if env:
        return Path(env).expanduser().resolve()

    if cfg:
        raw = str(cfg.get("data_dir") or "").strip()
        if raw:
            path = Path(raw).expanduser()
            if not path.is_absolute():
                path = (HERE / path).resolve()
            return path

    preferred = repo_root() / "test-sandbox" / "data"
    if _looks_like_index(preferred):
        return preferred
    local_legacy = HERE / "data"
    if _looks_like_index(local_legacy):
        return local_legacy
    return preferred


def _looks_like_index(path: Path) -> bool:
    return (
        (path / "meta.json").is_file()
        or (path / "leaves.json").is_file()
        or (path / "blocks" / "index.json").is_file()
    )


def local_config_path() -> Path:
    """engine.load_config와 같은 규칙: MASAMONG_SANDBOX_CONFIG가 있으면 그 파일, 없으면 config.json."""

    raw = str(os.environ.get("MASAMONG_SANDBOX_CONFIG") or "").strip()
    if not raw:
        return HERE / "config.json"
    path = Path(raw).expanduser()
    return path if path.is_absolute() else (HERE / path).resolve()


def load_local_config() -> dict[str, Any] | None:
    path = local_config_path()
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return None
    return data if isinstance(data, dict) else None
