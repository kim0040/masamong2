"""Person Memory View dirty coalescing (§14–15) — no per-message LLM."""

from __future__ import annotations

from dataclasses import dataclass, field


class PersonMemoryError(ValueError):
    pass


@dataclass
class PersonFacet:
    facet_id: str
    revision: int = 0
    dirty: bool = False
    last_materialized: str | None = None
    source_claim_ids: list[str] = field(default_factory=list)


@dataclass
class PersonMemoryView:
    person_id: str
    facets: dict[str, PersonFacet] = field(default_factory=dict)

    def mark_dirty(self, facet_id: str) -> None:
        facet = self.facets.setdefault(facet_id, PersonFacet(facet_id=facet_id))
        facet.dirty = True

    def coalesce_dirty_ids(self) -> list[str]:
        return sorted(fid for fid, f in self.facets.items() if f.dirty)

    def materialize(self, facet_id: str, *, claim_ids: list[str], at: str) -> None:
        """Batch materialization — call once after many dirty marks."""

        if facet_id not in self.facets or not self.facets[facet_id].dirty:
            raise PersonMemoryError("materialize requires dirty facet")
        if not claim_ids:
            raise PersonMemoryError("materialize requires source_claim_ids")
        facet = self.facets[facet_id]
        facet.revision += 1
        facet.dirty = False
        facet.last_materialized = at
        facet.source_claim_ids = list(claim_ids)
