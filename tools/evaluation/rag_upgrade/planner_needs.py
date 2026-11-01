"""Planner needs schema (§2, §19) — validation only, no LLM."""

from __future__ import annotations

from typing import Any, Mapping, Sequence

from . import MEMORY_REQUIREMENTS

ALLOWED_NEED_TYPES = frozenset(
    {
        "market_quote",
        "fx_rate",
        "person_memory",
        "memory_retrieve",
        "weather",
        "web",
        "other",
    }
)


class PlannerSchemaError(ValueError):
    pass


def validate_planner_output(raw: Mapping[str, Any]) -> dict[str, Any]:
    goal = str(raw.get("goal") or "").strip()
    if not goal:
        raise PlannerSchemaError("goal required")
    mem = str(raw.get("memory_requirement") or "").strip()
    if mem not in MEMORY_REQUIREMENTS:
        raise PlannerSchemaError("bad memory_requirement")
    needs = raw.get("needs")
    if not isinstance(needs, list) or not needs:
        raise PlannerSchemaError("needs must be non-empty list")
    ids: set[str] = set()
    normalized: list[dict[str, Any]] = []
    for item in needs:
        if not isinstance(item, Mapping):
            raise PlannerSchemaError("need must be object")
        nid = str(item.get("id") or "").strip()
        ntype = str(item.get("type") or item.get("capability") or "").strip()
        # allow capability style finance.quote → treat as other unless mapped
        if ntype.startswith("finance.quote") or ntype == "market_quote":
            ntype = "market_quote"
        elif ntype.startswith("finance.fx") or ntype == "fx_rate":
            ntype = "fx_rate"
        elif ntype.startswith("memory") or ntype == "person_memory":
            ntype = "person_memory" if "person" in ntype or ntype == "person_memory" else "memory_retrieve"
        if not nid or ntype not in ALLOWED_NEED_TYPES:
            raise PlannerSchemaError(f"bad need {nid}/{ntype}")
        if nid in ids:
            raise PlannerSchemaError(f"duplicate need id {nid}")
        ids.add(nid)
        required = bool(item.get("required", True))
        if mem == "none" and ntype in {"person_memory", "memory_retrieve"} and required:
            raise PlannerSchemaError("memory need required while memory_requirement=none")
        if mem == "required" and ntype in {"person_memory", "memory_retrieve"}:
            # at least one memory need should be required — checked after loop
            pass
        normalized.append(
            {
                "id": nid,
                "type": ntype,
                "required": required,
                "entity": item.get("entity"),
                "topic": item.get("topic"),
                "capability": item.get("capability"),
            }
        )
    if mem == "required":
        if not any(
            n["type"] in {"person_memory", "memory_retrieve"} and n["required"]
            for n in normalized
        ):
            raise PlannerSchemaError("memory_requirement=required needs required memory need")
    return {"goal": goal, "memory_requirement": mem, "needs": normalized}


def score_planner_vs_gold(
    plan: Mapping[str, Any],
    *,
    expected_memory_requirement: str,
    expected_tools: Sequence[str],
) -> dict[str, Any]:
    mem_ok = plan["memory_requirement"] == expected_memory_requirement
    plan_caps = {
        str(n.get("capability") or n["type"]) for n in plan["needs"] if n.get("required")
    }
    expected = set(expected_tools)
    # soft match: expected tool substring in capability/type
    hit = 0
    for exp in expected:
        if any(exp == c or exp in c or c in exp for c in plan_caps):
            hit += 1
    recall = 1.0 if not expected else hit / len(expected)
    extra = [c for c in plan_caps if not any(exp in c or c in exp for exp in expected)]
    return {
        "memory_requirement_ok": mem_ok,
        "required_tool_recall": recall,
        "extra_required_tools": extra,
    }
