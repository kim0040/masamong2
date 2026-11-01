"""Rule-based candidate confidence for auto v2 (no model, no network).

Auto no longer picks R0/R2 *before* searching. It generates the R0 v2 candidate pool, looks at
it, and decides: confident → use R0 order; uncertain → semantic rerank (R2); poor → one
recovery pass (wider pool / window / limits) then R2. The signals below are deliberately
simple and observable; thresholds live in `retrieval.auto` config and were set from the
2026-09-17 gold v3 sweep (07 experiment log), not by hand-tuning to a single query.

Signals (over the top-N fused candidates):
  candidate_count   how many candidates the union produced at all
  agreement         share of top-N surfaced by ≥2 retriever families (lexical/dense/speaker/…)
  lexical_dense_overlap   |top-N leaf_lexical ∩ top-N leaf_dense| / N
  top_margin        (score₁ − score₂) / score₁ of the fused RRF scores
  target_coverage   share of top-N tied to the requested person (speaker/alias flags); None when
                    the question names nobody or nobody resolved
  source_diversity  distinct retriever families in the top-N

The same level rule is mirrored in Go (`internal/memory/retrieval.Assess`) and both are checked
against the cases in contracts/sandbox/tool-parity.json `ranking.auto_policy.cases`.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

from candidates import FusionResult

LEVEL_HIGH = "high"
LEVEL_MEDIUM = "medium"
LEVEL_LOW = "low"

DEFAULT_THRESHOLDS: dict[str, float] = {
    "assess_top_n": 10,
    "high_agreement": 0.6,
    "high_margin": 0.15,
    "low_agreement": 0.2,
    "min_candidates": 3,
    "min_target_coverage": 0.3,
}

# Derived soft-event views may have lexical and dense runs, but they are not
# independent evidence families. Keep this default here so every assessment
# caller gets the same conservative behavior.
DEFAULT_EXCLUDED_FAMILIES = frozenset({"soft_event"})


@dataclass
class Assessment:
    level: str
    signals: dict[str, Any]
    reasons: list[str] = field(default_factory=list)

    def to_trace(self) -> dict[str, Any]:
        return {"level": self.level, "reasons": list(self.reasons), **{k: v for k, v in self.signals.items()}}


def signals_for(
    fusion: FusionResult,
    *,
    top_n: int,
    targets_requested: bool,
    targets_resolved: bool,
    excluded_families: set[str] | frozenset[str] | None = None,
) -> dict[str, Any]:
    top = fusion.ordered[: max(1, int(top_n))]
    n = len(top)
    count = len(fusion.ordered)
    excluded = set(DEFAULT_EXCLUDED_FAMILIES if excluded_families is None else excluded_families)
    effective_families = [c.families - excluded for c in top]
    agreement = (sum(1 for families in effective_families if len(families) >= 2) / n) if n else 0.0
    lex = set((fusion.runs.get("leaf_lexical") or [])[: int(top_n)])
    den = set((fusion.runs.get("leaf_dense") or [])[: int(top_n)])
    overlap = (len(lex & den) / max(1, int(top_n))) if (lex or den) else 0.0
    if n >= 2 and top[0].score > 0:
        margin = (top[0].score - top[1].score) / top[0].score
    else:
        margin = 1.0 if n == 1 else 0.0
    coverage: float | None
    if targets_requested and targets_resolved:
        coverage = (sum(1 for c in top if c.flags.get("speaker_match") or c.flags.get("alias_match")) / n) if n else 0.0
    else:
        coverage = None
    families: set[str] = set()
    for candidate_families in effective_families:
        families |= candidate_families
    return {
        "candidate_count": count,
        "agreement": round(agreement, 4),
        "lexical_dense_overlap": round(overlap, 4),
        "top_margin": round(margin, 4),
        "target_coverage": None if coverage is None else round(coverage, 4),
        "targets_requested": bool(targets_requested),
        "targets_resolved": bool(targets_resolved),
        "source_diversity": len(families),
    }


def level_from_signals(sig: Mapping[str, Any], th: Mapping[str, Any] | None = None) -> tuple[str, list[str]]:
    """Pure decision rule shared (by contract cases) with the Go mirror."""

    t = {**DEFAULT_THRESHOLDS, **(th or {})}
    reasons: list[str] = []
    count = int(sig.get("candidate_count") or 0)
    agreement = float(sig.get("agreement") or 0.0)
    margin = float(sig.get("top_margin") or 0.0)
    coverage = sig.get("target_coverage")
    requested = bool(sig.get("targets_requested"))
    resolved = bool(sig.get("targets_resolved"))
    if count < int(t["min_candidates"]):
        reasons.append("few_candidates")
        return LEVEL_LOW, reasons
    if requested and resolved and coverage is not None and float(coverage) < float(t["min_target_coverage"]):
        reasons.append("target_missing")
        return LEVEL_LOW, reasons
    if agreement < float(t["low_agreement"]):
        reasons.append("low_agreement")
        return LEVEL_LOW, reasons
    if requested and not resolved:
        reasons.append("target_unresolved")  # a person was named but no speaker matched → never "high"
        return LEVEL_MEDIUM, reasons
    if agreement >= float(t["high_agreement"]) and margin >= float(t["high_margin"]):
        reasons.append("agreement_and_margin")
        return LEVEL_HIGH, reasons
    reasons.append("uncertain")
    return LEVEL_MEDIUM, reasons


def assess(
    fusion: FusionResult,
    *,
    thresholds: Mapping[str, Any] | None = None,
    targets_requested: bool = False,
    targets_resolved: bool = False,
    excluded_families: set[str] | frozenset[str] | None = None,
) -> Assessment:
    t = {**DEFAULT_THRESHOLDS, **(thresholds or {})}
    sig = signals_for(
        fusion,
        top_n=int(t["assess_top_n"]),
        targets_requested=targets_requested,
        targets_resolved=targets_resolved,
        excluded_families=excluded_families,
    )
    level, reasons = level_from_signals(sig, t)
    return Assessment(level=level, signals=sig, reasons=reasons)
