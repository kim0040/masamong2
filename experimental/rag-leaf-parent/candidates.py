"""Candidate objects with retriever provenance, and weighted-RRF fusion over them (no network).

R0 v2 runs several independent candidate sources (leaf lexical/dense, summary lexical/dense,
speaker, alias, extra queries). Each source yields a ranked id list; `fuse` merges them with the
same weighted RRF formula as `engine.weighted_rrf` (regression-tested to be order-identical) but
keeps *where each candidate came from* so auto can assess retriever agreement and evaluation can
measure per-retriever recall / unique contribution.

Nothing here is persisted; candidates are transient per query.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping

RRF_K = 60
MESSAGE_RUN = "message_dense"  # run name of the optional single-message vector source


@dataclass
class Candidate:
    doc_id: str
    score: float = 0.0  # fused RRF score
    rank: int = 0  # 1-based fused rank
    sources: dict[str, int] = field(default_factory=dict)  # source → 1-based rank in that run
    raw: dict[str, float] = field(default_factory=dict)  # source → raw score (bm25 / cos) if known
    flags: dict[str, bool] = field(default_factory=dict)  # speaker_match, alias_match, time_match…

    @property
    def families(self) -> set[str]:
        """Retriever families (lexical / dense / speaker / alias / …) that surfaced this candidate."""

        return {source_family(s) for s in self.sources}

    def to_trace(self) -> dict[str, Any]:
        out: dict[str, Any] = {"doc_id": self.doc_id, "score": round(self.score, 6), "sources": dict(self.sources)}
        if self.flags:
            out["flags"] = {k: v for k, v in self.flags.items() if v}
        return out


def source_family(source: str) -> str:
    """`leaf_lexical` → lexical, `q1_dense` → dense, `speaker_dense` → speaker …"""

    name = source
    # Soft-event runs are derived views over stable leaf candidates. Keep
    # their concrete mode in provenance, but expose one family so confidence
    # cannot treat lexical and dense event views as independent retrievers.
    if name == "soft_event" or name.startswith("soft_event_"):
        return "soft_event"
    if name == "message_dense":
        return "dense"  # single-message vectors: the same retriever family as leaf/summary dense
    for prefix in ("leaf_", "summary_", "q1_", "q2_"):
        if name.startswith(prefix):
            name = name[len(prefix):]
    if name.startswith("speaker"):
        return "speaker"
    if name.startswith("alias"):
        return "alias"
    if name.startswith("time"):
        return "time"
    return name


@dataclass
class FusionResult:
    ordered: list[Candidate]
    weights: dict[str, float]
    runs: dict[str, list[str]]  # source → ranked ids (as fused)

    def ids(self) -> list[str]:
        return [c.doc_id for c in self.ordered]

    @property
    def by_id(self) -> dict[str, Candidate]:
        return {c.doc_id: c for c in self.ordered}

    def source_counts(self) -> dict[str, int]:
        return {name: len(ids) for name, ids in self.runs.items()}

    def trace(self, *, limit: int = 100, runs_limit: int = 200) -> dict[str, Any]:
        return {
            "candidate_union_n": len(self.ordered),
            "candidate_sources": self.source_counts(),
            "candidate_provenance": [c.to_trace() for c in self.ordered[:limit]],
            # full per-retriever id lists (bounded) — evaluation measures per-source recall here
            "candidate_runs": {name: ids[:runs_limit] for name, ids in self.runs.items()},
        }


def fuse(
    runs: Iterable[tuple[str, list[str], float]],
    *,
    k: int = RRF_K,
    children: Mapping[str, FusionResult] | None = None,
    raw_scores: Mapping[str, Mapping[str, float]] | None = None,
) -> FusionResult:
    """Weighted RRF over named ranked lists, preserving provenance.

    `children` lets a run name stand for an already-fused list (nested fusion: leaf hybrid ⊕
    summary hybrid): the child's per-source ranks are copied into the candidate instead of the
    child's name, so provenance stays at the retriever level.

    Order is identical to `engine.weighted_rrf` for the same inputs (sum of weight/(k+pos+1),
    stable sort on first appearance).
    """

    scores: dict[str, float] = {}
    sources: dict[str, dict[str, int]] = {}
    weights: dict[str, float] = {}
    kept_runs: dict[str, list[str]] = {}
    children = children or {}
    raw_scores = raw_scores or {}
    for name, ids, weight in runs:
        weights[name] = float(weight)
        kept_runs[name] = list(ids)
        child = children.get(name)
        for pos, doc_id in enumerate(ids):
            scores[doc_id] = scores.get(doc_id, 0.0) + float(weight) / (k + pos + 1)
            src = sources.setdefault(doc_id, {})
            if child is not None:
                for s, r in (child.by_id.get(doc_id) or Candidate(doc_id)).sources.items():
                    src.setdefault(s, r)
            else:
                src.setdefault(name, pos + 1)
    # Child names are aggregates, not retrievers: expose their runs, drop the aggregate name.
    for name, child in children.items():
        for s, ids in child.runs.items():
            kept_runs.setdefault(s, ids)
        kept_runs.pop(name, None)
    ordered_ids = [doc_id for doc_id, _ in sorted(scores.items(), key=lambda item: -item[1])]
    ordered: list[Candidate] = []
    for i, doc_id in enumerate(ordered_ids):
        raw: dict[str, float] = {}
        for s in sources.get(doc_id, {}):
            table = raw_scores.get(s)
            if table is not None and doc_id in table:
                raw[s] = float(table[doc_id])
        ordered.append(
            Candidate(doc_id=doc_id, score=scores[doc_id], rank=i + 1, sources=dict(sources.get(doc_id, {})), raw=raw)
        )
    return FusionResult(ordered=ordered, weights=weights, runs=kept_runs)


def per_source_hit(fusion: FusionResult, want_ids: set[str]) -> dict[str, bool]:
    """Which retriever runs contain any wanted id — for retriever-specific recall."""

    return {name: any(doc_id in want_ids for doc_id in ids) for name, ids in fusion.runs.items()}


def unique_contribution(fusion: FusionResult, want_ids: set[str]) -> list[str]:
    """Retrievers that were the *only* source of a wanted id (dropping them would lose the hit)."""

    hits = per_source_hit(fusion, want_ids)
    only = [name for name, hit in hits.items() if hit]
    return only if len(only) == 1 else []


def pool_necessary_sources(
    fusion: FusionResult,
    pool_ids: list[str],
    want_ids: set[str],
    *,
    fusion_head: list[str] | None = None,
) -> list[str]:
    """Sources without which a wanted id would be missing from `pool_ids`.

    If the wanted id is already in `fusion_head` (R0 ranking / fused cut), no source is
    necessary for pool membership. Otherwise every source that uniquely holds it in
    `fusion.runs` is listed — typically a low-weight retriever that entered via guarantee.
    """

    pool = set(pool_ids)
    head = set(fusion_head or [])
    if not want_ids or not (want_ids & pool):
        return []
    if want_ids & head:
        return []
    hits = per_source_hit(fusion, want_ids & pool)
    only = [name for name, hit in hits.items() if hit]
    return only if len(only) == 1 else []


def guarantee_pool(
    fusion: FusionResult,
    *,
    pool: int,
    per_source_min: int,
    reserve_max_frac: float = 0.3,
    excluded_sources: Iterable[str] | None = None,
    reserve_sources: Iterable[str] | None = None,
) -> tuple[list[str], list[str], dict[str, list[str]]]:
    """Candidate pool = fused top-(pool − reserved) + top-`per_source_min` of every retriever run
    that the fused cut would have dropped. Separates *order* (fusion weights) from *pool
    membership*: a low-weight source (speaker, alias) still gets its best few candidates in
    front of the reranker without distorting the R0 order at the top.

    Reserve slots are filled round-robin across sources (not alphabetical drain) so one
    retriever cannot consume the whole reserve.

    ``excluded_sources`` removes sources (or source families) from reserve
    consideration. ``reserve_sources`` is the inverse allow-list; when
    supplied, only matching sources (or families) may consume reserve slots.
    Both are optional so historical calls have exactly the same behavior.

    Returns (ids with the pool prefix first then the rest of fusion order, ids pulled in by
    the guarantee, extras grouped by source). Fusion order itself is unchanged: callers must
    use `fusion.ids()` for R0 ranking and the pool prefix for R2 membership.
    """

    ids = fusion.ids()
    empty: dict[str, list[str]] = {}
    if pool <= 0 or per_source_min <= 0 or not fusion.runs:
        return ids, [], empty
    reserve_cap = max(0, int(pool * max(0.0, min(1.0, reserve_max_frac))))
    base_n = max(0, pool - reserve_cap)
    base = ids[:base_n]
    in_pool = set(base)
    extras: list[str] = []
    by_source: dict[str, list[str]] = {}
    excluded = {str(source) for source in (excluded_sources or ())}
    allowed = None if reserve_sources is None else {str(source) for source in reserve_sources}

    def matches(name: str, selectors: set[str]) -> bool:
        return name in selectors or source_family(name) in selectors

    names = [name for name in sorted(fusion.runs) if not matches(name, excluded)]
    if allowed is not None:
        names = [name for name in names if matches(name, allowed)]
    # The message run claims its slots last, from what the other runs left of the reserve, so adding it
    # never displaces a candidate that would have been guaranteed without it (Go: guaranteePool).
    late = [name for name in names if name == MESSAGE_RUN]
    names = [name for name in names if name != MESSAGE_RUN]
    for k in range(per_source_min):
        if len(extras) >= reserve_cap:
            break
        for name in names:
            run = fusion.runs[name]
            if k >= len(run):
                continue
            doc_id = run[k]
            if doc_id in in_pool:
                continue
            extras.append(doc_id)
            in_pool.add(doc_id)
            by_source.setdefault(name, []).append(doc_id)
            if len(extras) >= reserve_cap:
                break
    for name in late:
        run = fusion.runs[name]
        for k in range(per_source_min):
            if len(extras) >= reserve_cap or k >= len(run):
                break
            doc_id = run[k]
            if doc_id in in_pool:
                continue
            extras.append(doc_id)
            in_pool.add(doc_id)
            by_source.setdefault(name, []).append(doc_id)
    fill = [d for d in ids[base_n:] if d not in in_pool]
    head = base + extras
    room = pool - len(head)
    pool_ids = head + fill[: max(0, room)]
    rest = [d for d in ids if d not in set(pool_ids)]
    return pool_ids + rest, extras, by_source
