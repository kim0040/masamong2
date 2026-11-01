"""Observability event envelope + allowlist logging (§30–32, §35)."""

from __future__ import annotations

from typing import Any, Mapping

ALLOWED_EVENT_FIELDS = frozenset(
    {
        "provider",
        "model",
        "status",
        "duration_ms",
        "attempt",
        "token_count",
        "cost",
        "error_code",
        "candidate_count",
        "rerank_count",
        "evidence_count",
        "retrieval_round",
    }
)

FORBIDDEN_FIELD_NAMES = frozenset(
    {
        "authorization",
        "api_key",
        "prompt",
        "raw_message_body",
        "discord_token",
        "request_raw_body",
    }
)

FEATURE_STATES = frozenset(
    {"healthy", "degraded", "backlogged", "stale", "blocked", "failed"}
)


class ObservabilityError(ValueError):
    pass


def make_event(
    *,
    event_type: str,
    component: str,
    status: str,
    severity: str = "info",
    trace_id: str,
    request_id: str,
    duration_ms: float | None = None,
    fields: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    if not event_type or not component or not trace_id or not request_id:
        raise ObservabilityError("required envelope fields missing")
    safe_fields: dict[str, Any] = {}
    for key, value in (fields or {}).items():
        lk = key.lower()
        if lk in FORBIDDEN_FIELD_NAMES or any(f in lk for f in ("token", "secret", "password")):
            continue
        if key not in ALLOWED_EVENT_FIELDS:
            # drop unknown by default (allowlist)
            continue
        safe_fields[key] = value
    return {
        "schema_version": 1,
        "event_type": event_type,
        "severity": severity,
        "component": component,
        "trace_id": trace_id,
        "request_id": request_id,
        "job_id": None,
        "status": status,
        "duration_ms": duration_ms,
        "fields": safe_fields,
    }


def feature_health(parts: Mapping[str, str]) -> dict[str, str]:
    out: dict[str, str] = {}
    for name, state in parts.items():
        if state not in FEATURE_STATES:
            raise ObservabilityError(f"bad state {state}")
        out[name] = state
    if "ready" in out and len(out) == 1:
        raise ObservabilityError("single ready bit is forbidden")
    return out
