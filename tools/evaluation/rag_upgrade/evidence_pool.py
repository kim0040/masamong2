"""Common Evidence Pool normalization (§21)."""

from __future__ import annotations

from typing import Any, Mapping


class EvidenceError(ValueError):
    pass


ALLOWED_KIND = frozenset(
    {
        "market_quote",
        "fx_rate",
        "memory_claim",
        "memory_excerpt",
        "weather",
        "web",
        "other",
    }
)
ALLOWED_STATUS = frozenset({"ok", "partial", "error", "missing"})


def normalize_evidence(raw: Mapping[str, Any]) -> dict[str, Any]:
    eid = str(raw.get("id") or "").strip()
    kind = str(raw.get("kind") or "").strip()
    status = str(raw.get("status") or "").strip()
    if not eid or kind not in ALLOWED_KIND or status not in ALLOWED_STATUS:
        raise EvidenceError("id/kind/status invalid")
    out: dict[str, Any] = {
        "id": eid,
        "kind": kind,
        "status": status,
        "source": raw.get("source"),
        "observed_at": raw.get("observed_at"),
        "data": raw.get("data") if isinstance(raw.get("data"), Mapping) else {},
    }
    if kind in {"memory_claim", "memory_excerpt"}:
        out["subject_person_id"] = raw.get("subject_person_id")
        out["evidence_ids"] = list(raw.get("evidence_ids") or [])
        if status == "ok" and kind == "memory_claim" and not out["evidence_ids"]:
            raise EvidenceError("memory_claim ok requires evidence_ids")
    return out


def merge_pool(items: list[Mapping[str, Any]]) -> list[dict[str, Any]]:
    seen: set[str] = set()
    out: list[dict[str, Any]] = []
    for item in items:
        ev = normalize_evidence(item)
        if ev["id"] in seen:
            raise EvidenceError(f"duplicate evidence id {ev['id']}")
        seen.add(ev["id"])
        out.append(ev)
    return out


def synthesizer_input_ok(pool: list[Mapping[str, Any]]) -> bool:
    """Synthesizer may only see normalized evidence; reject raw prompts."""

    for item in pool:
        if "prompt" in item or "raw_message_body" in item:
            return False
        normalize_evidence(item)
    return True
