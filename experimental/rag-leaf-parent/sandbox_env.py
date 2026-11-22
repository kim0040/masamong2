"""The import path and environment defaults the sandbox needs, set in one place.

The sandbox imports the evaluation helpers (`corpus`, `lexical`, `rag_upgrade.*`) from tools/evaluation by bare
name, and they read two environment variables. `ensure()` puts that folder first on `sys.path` and sets the
two defaults (never over a value that is already set). engine_index calls it before its first such import;
nothing else in the engine mutates `sys.path` or `os.environ`.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path


def repo_root() -> Path:
    here = Path(__file__).resolve().parent
    for cand in [here, *here.parents]:
        if (cand / "tools" / "evaluation").is_dir() and (cand / "docs").is_dir():
            return cand
    return here.parents[1]


ROOT = repo_root()
EVAL = ROOT / "tools" / "evaluation"


def ensure() -> None:
    os.environ.setdefault("RAW_CORPUS_ROOT", str(ROOT / ".private" / "raw-corpus"))
    os.environ.setdefault("EVAL_SECRETS", str(ROOT / ".private" / "eval" / "secrets.env"))
    sys.path.insert(0, str(EVAL))
