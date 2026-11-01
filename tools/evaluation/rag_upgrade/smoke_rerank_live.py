#!/usr/bin/env python3
"""Opt-in live Cohere rerank smoke (synthetic docs only).

Usage:
  python3 tools/evaluation/rag_upgrade/smoke_rerank_live.py

Never reads Discord tokens for auth. Never loads private chat corpora.
"""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
EVAL = HERE.parent
REPO = HERE.parents[2]  # rag_upgrade -> evaluation -> tools -> repo root
sys.path.insert(0, str(EVAL))

from openrouter import load_secrets  # noqa: E402
from rag_upgrade import RERANK_MODEL  # noqa: E402
from rag_upgrade.rerank_client import RerankClient  # noqa: E402


def main() -> int:
    secrets = load_secrets(REPO / ".private/eval/secrets.env")
    if secrets.get("DISCORD_TOKEN") or secrets.get("DISCORD_BOT_TOKEN"):
        # File may contain them later; client still refuses equal keys.
        pass
    client = RerankClient(
        secrets,
        model=RERANK_MODEL,
        budget_usd=0.10,
        max_request_cost_usd=0.05,
    )
    docs = ["오늘 점심 메뉴", "을동은 C고 졸업", "날씨 맑음"]
    result = client.rerank("을동 고등학교", docs, top_n=3)
    print("ok", result.model, "top", result.indices[0], "cost", result.cost_usd)
    print("school_first", result.indices[0] == 1)
    return 0 if result.indices[0] == 1 else 1


if __name__ == "__main__":
    raise SystemExit(main())
