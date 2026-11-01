"""Load shared Action IDs with the product catalog (contracts/sandbox/tool-parity.json)."""

from __future__ import annotations

import json
import re
from functools import lru_cache
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent


def _repo_root() -> Path:
    for cand in [HERE, *HERE.parents]:
        if (cand / "contracts" / "sandbox" / "tool-parity.json").is_file():
            return cand
    return HERE.parents[1]


@lru_cache(maxsize=1)
def parity_spec() -> dict[str, Any]:
    path = _repo_root() / "contracts" / "sandbox" / "tool-parity.json"
    return json.loads(path.read_text(encoding="utf-8"))


def agent_tools() -> list[dict[str, Any]]:
    rows = parity_spec().get("agent_tools")
    return rows if isinstance(rows, list) else []


def default_enabled_ids() -> list[str]:
    return [str(row["id"]) for row in agent_tools() if str(row.get("id") or "")]


def canonicalize_tool(name: str) -> str:
    raw = str(name or "").strip()
    if not raw:
        return ""
    for row in agent_tools():
        action_id = str(row.get("id") or "")
        aliases = [str(a) for a in (row.get("aliases") or [])]
        if raw == action_id or raw in aliases:
            return action_id
    return raw


def bang_alias_map() -> dict[str, str]:
    out: dict[str, str] = {}
    for row in agent_tools():
        action_id = str(row.get("id") or "")
        for alias in row.get("bang") or []:
            key = str(alias).strip().lower()
            if key:
                out[key] = action_id
    return out


_BANG = re.compile(r"^!(\S+)(?:\s+([\s\S]*))?\s*$")


def parse_bang_command(message: str) -> dict[str, Any] | None:
    """Map `!날씨 전주` to the same Action IDs as product Discord commands."""
    text = str(message or "").strip()
    match = _BANG.match(text)
    if not match:
        return None
    verb = match.group(1).strip().lower()
    rest = (match.group(2) or "").strip()
    action_id = bang_alias_map().get(verb)
    if not action_id:
        return None
    params: dict[str, Any] = {}
    if action_id == "weather.query":
        params["region"] = rest or "서울"
        params["location"] = params["region"]
    elif action_id == "finance.quote":
        params["symbol"] = rest.split()[0] if rest else ""
    elif action_id == "finance.fx":
        params["base"] = rest.split()[0] if rest else "USD"
    elif action_id == "search.web":
        params["query"] = rest
    elif action_id == "images.generate":
        params["prompt"] = rest
    elif action_id == "polls.create":
        q, sep, answers = rest.partition("|")
        if sep:
            params["question"] = q.strip()
            params["options"] = [x.strip() for x in answers.split(",") if x.strip()]
        else:
            parts = [p for p in re.findall(r'"([^"]+)"|(\S+)', rest) for p in p if p]
            if parts:
                params["question"] = parts[0]
                params["options"] = parts[1:]
        params["duration_hours"] = 24
    return {
        "tool_name": action_id,
        "parameters": params,
        "via": "bang",
    }


# --- ranking auto policy (contracts/sandbox/tool-parity.json → ranking.auto_policy) ---
#
# auto v2 (2026-09-17) is a cascade, not a pre-search selector:
#   R0 v2 candidates → assess (candidates_assess) → high: R0 · medium: R2 · low: recovery once → R2.
# Reranker unavailable / failed → R0. R1(`heuristic`) is ablation-only and never chosen here.

_DEFAULT_POLICY: dict[str, Any] = {
    "on_high": "hybrid",
    "on_medium": "rerank",
    "on_low": "rerank",
    "no_rerank": "hybrid",
    "rerank_failed": "hybrid",
    "min_remaining_budget_usd": 0.02,
    "recovery_max_attempts": 1,
}

# auto가 고를 수 있는 구체 모드. R1은 ablation 전용이라 여기 없다.
AUTO_ALLOWED = frozenset({"hybrid", "rerank"})
AUTO_REASONS = (
    "r0_confident", "rerank_uncertain", "rerank_low_confidence",
    "recovery_rerank", "recovery_no_rerank", "no_rerank", "rerank_failed",
)


def ranking_policy() -> dict[str, Any]:
    raw = (parity_spec().get("ranking") or {}).get("auto_policy")
    out = dict(_DEFAULT_POLICY)
    if isinstance(raw, dict):
        out.update({k: raw[k] for k in _DEFAULT_POLICY if k in raw})
        if isinstance(raw.get("thresholds"), dict):
            out["thresholds"] = dict(raw["thresholds"])
    for key in ("on_high", "on_medium", "on_low", "no_rerank", "rerank_failed"):
        if out[key] not in AUTO_ALLOWED:
            raise ValueError(f"auto_policy.{key}={out[key]!r} — auto는 R0/R2만 고른다 (R1 퇴역)")
    return out


def decide_ranking(
    level: str,
    *,
    rerank_available: bool,
    remaining_budget_usd: float,
    recovery_done: bool = False,
    policy: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """One cascade step → {"mode": hybrid|rerank|None, "reason": str, "recover": bool}.

    `recover=True` means: run the recovery retrieval, then call again with recovery_done=True.
    제품 Go 정책(internal/memory/retrieval.AutoPolicy.Decide)과 같은 표를 쓴다 — 계약 파일이 원본이다.
    """

    pol = policy or ranking_policy()
    can_rerank = rerank_available and float(remaining_budget_usd) >= float(pol.get("min_remaining_budget_usd", 0.02))
    if level == "high":
        return {"mode": str(pol["on_high"]), "reason": "r0_confident", "recover": False}
    if level == "medium":
        if not can_rerank:
            return {"mode": str(pol["no_rerank"]), "reason": "no_rerank", "recover": False}
        return {"mode": str(pol["on_medium"]), "reason": "rerank_uncertain", "recover": False}
    # low
    if not recovery_done and int(pol.get("recovery_max_attempts") or 0) > 0:
        return {"mode": None, "reason": "recovery_low_confidence", "recover": True}
    if not can_rerank:
        return {"mode": str(pol["no_rerank"]), "reason": "recovery_no_rerank" if recovery_done else "no_rerank", "recover": False}
    return {"mode": str(pol["on_low"]), "reason": "recovery_rerank" if recovery_done else "rerank_low_confidence", "recover": False}


def fallback_ranking(intent: dict[str, Any] | None = None, *, policy: dict[str, Any] | None = None) -> tuple[str, str]:
    """R2 호출이 실패했을 때 auto가 내려갈 곳 — 항상 R0 (recent/person은 이미 풀에 반영돼 있다)."""

    del intent  # 신호가 있어도 R1로 가지 않는다
    pol = policy or ranking_policy()
    return str(pol["rerank_failed"]), "rerank_failed"
