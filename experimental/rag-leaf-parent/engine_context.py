"""What the sandbox engine used to keep in module globals: where the artifacts live and the prepare state.

`SandboxPaths` is a frozen set of artifact paths for one data dir; `EngineContext` holds the paths in use, the
state of a running prepare and the lock that guards the spend ledger. engine_index owns the one live context and
rebinding the data dir is one assignment of a new `SandboxPaths`, so a function that reads `ctx.paths` once sees
a consistent set. This module imports nothing from the sandbox and touches no environment.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class SandboxPaths:
    data: Path
    messages: Path
    leaves: Path
    baseline: Path
    leaf_vecs: Path
    baseline_vecs: Path
    summaries: Path
    summary_vecs: Path
    meta: Path
    ledger: Path
    ckpt_dir: Path
    ckpt_state: Path
    progress: Path

    @classmethod
    def under(cls, data: Path | str) -> "SandboxPaths":
        data = Path(data)
        ckpt_dir = data / "ckpt"
        return cls(
            data=data,
            messages=data / "messages.json",
            leaves=data / "leaves.json",
            baseline=data / "baseline_windows.json",
            leaf_vecs=data / "leaf_vecs.npy",
            baseline_vecs=data / "baseline_vecs.npy",
            summaries=data / "summaries.json",
            summary_vecs=data / "summary_vecs.npy",
            meta=data / "meta.json",
            ledger=data / "session_ledger.json",
            ckpt_dir=ckpt_dir,
            ckpt_state=ckpt_dir / "state.json",
            progress=data / "prepare_progress.json",
        )


@dataclass
class PrepareState:
    """Set by prepare_index and read by the progress reporter."""

    summary_on: bool = True
    cost_usd: float | None = None


@dataclass
class EngineContext:
    paths: SandboxPaths
    prepare: PrepareState = field(default_factory=PrepareState)
    ledger_lock: threading.Lock = field(default_factory=threading.Lock)
