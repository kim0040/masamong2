"""Rough ingest cost estimator for leaf→parent prepare (not billing).

Rates are OpenRouter-order-of-magnitude defaults. Live invoices can differ.
"""

from __future__ import annotations

from typing import Any, Mapping, Sequence

ESTIMATE_VERSION = 1

# 2026-09 OpenRouter list prices, rounded. Not a quote.
DEFAULT_RATES = {
    "embed_usd_per_m": 0.01,  # qwen/qwen3-embedding-8b class
    "llm_input_usd_per_m": 0.05,  # z-ai/glm-5.3-flash class
    "llm_output_usd_per_m": 0.15,
    "chars_per_token": 2.2,  # Korean chat, rough
    "summary_prompt_overhead_tokens": 120,
    "summary_output_tokens": 80,
}


def segment_count(messages: int, *, size: int, overlap: int = 0, limit: int | None = None) -> int:
    size = max(1, int(size))
    overlap = max(0, min(int(overlap), size - 1))
    n = max(0, int(messages))
    if n <= 0:
        return 0
    if n < size:
        raw = 1
    else:
        step = max(1, size - overlap)
        raw = 1 + (n - size) // step
    if limit is None:
        return raw
    return min(raw, max(0, int(limit)))


def estimate_ingest(
    *,
    rooms: Sequence[Mapping[str, Any]],
    leaf_size: int = 8,
    leaf_overlap: int = 0,
    leaf_limit: int | None = None,
    baseline_size: int = 20,
    baseline_limit: int | None = 400,
    summary_enabled: bool = True,
    document_char_limit: int = 4000,
    rates: Mapping[str, float] | None = None,
    per_room_segments: bool = True,
) -> dict[str, Any]:
    """Estimate prepare (summary+embed) cost. Query-time rerank is excluded."""

    r = {**DEFAULT_RATES, **dict(rates or {})}
    cpt = max(0.5, float(r["chars_per_token"]))
    leaf_n = 0
    base_n = 0
    msg_n = 0
    char_n = 0
    room_rows: list[dict[str, Any]] = []
    for room in rooms:
        messages = int(room.get("messages") or 0)
        avg_chars = float(room.get("avg_chars") or 20.0)
        msg_n += messages
        char_n += avg_chars * messages
        if per_room_segments:
            leaves = segment_count(messages, size=leaf_size, overlap=leaf_overlap, limit=None)
            bases = segment_count(messages, size=baseline_size, overlap=0, limit=None)
        else:
            leaves = 0
            bases = 0
        leaf_n += leaves
        base_n += bases
        room_rows.append(
            {
                "id": room.get("id"),
                "label": room.get("label"),
                "messages": messages,
                "leaves": leaves,
                "baselines": bases,
            }
        )
    if not per_room_segments:
        leaf_n = segment_count(msg_n, size=leaf_size, overlap=leaf_overlap, limit=None)
        base_n = segment_count(msg_n, size=baseline_size, overlap=0, limit=None)
    if leaf_limit is not None:
        leaf_n = min(leaf_n, max(0, int(leaf_limit)))
    if baseline_limit is not None:
        base_n = min(base_n, max(0, int(baseline_limit)))

    avg_chars = (char_n / msg_n) if msg_n else 20.0
    leaf_chars = min(float(document_char_limit), max(24.0, avg_chars * leaf_size))
    leaf_tokens = leaf_chars / cpt
    summary_in = leaf_tokens + float(r["summary_prompt_overhead_tokens"])
    summary_out = float(r["summary_output_tokens"])
    summary_calls = leaf_n if summary_enabled else 0
    embed_docs = leaf_n + base_n + (leaf_n if summary_enabled else 0)

    llm_usd = summary_calls * (
        summary_in * float(r["llm_input_usd_per_m"]) / 1_000_000.0
        + summary_out * float(r["llm_output_usd_per_m"]) / 1_000_000.0
    )
    embed_usd = embed_docs * leaf_tokens * float(r["embed_usd_per_m"]) / 1_000_000.0
    mid = llm_usd + embed_usd
    return {
        "estimate_version": ESTIMATE_VERSION,
        "disclaimer": "예상치. 실과금·토큰화·의미 후보 필터와 다를 수 있다. 질의 리랭크는 제외.",
        "messages": msg_n,
        "rooms": len(rooms),
        "leaves": leaf_n,
        "baselines": base_n,
        "summary_calls": summary_calls,
        "embed_docs": embed_docs,
        "usd": {
            "llm": round(llm_usd, 4),
            "embed": round(embed_usd, 4),
            "mid": round(mid, 4),
            "low": round(mid * 0.5, 4),
            "high": round(mid * 2.5, 4),
        },
        "rates": r,
        "per_room_segments": per_room_segments,
        "room_rows": room_rows,
    }
