"""Deep search round limits and strategy change (§18)."""

from __future__ import annotations

from typing import Any, Mapping, Sequence


class DeepSearchError(ValueError):
    pass


MAX_ROUNDS = 2


def next_query_strategy(
    round_index: int,
    *,
    base_query: str,
    aliases: Sequence[str] = (),
    predicates: Sequence[str] = (),
) -> dict[str, Any]:
    if round_index < 1 or round_index > MAX_ROUNDS:
        raise DeepSearchError("round_index must be 1..2")
    if round_index == 1:
        return {
            "round": 1,
            "query": base_query,
            "strategy": "normal",
            "alias_expansion": False,
        }
    parts = [base_query, *aliases, *predicates]
    return {
        "round": 2,
        "query": " ".join(p for p in parts if p),
        "strategy": "alias_predicate_expand",
        "alias_expansion": True,
        # explicitly not "just increase top_k"
        "top_k_only_increase": False,
    }


def should_abstain(round_index: int, hit: bool) -> bool:
    if hit:
        return False
    return round_index >= MAX_ROUNDS
