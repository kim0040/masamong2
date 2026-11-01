#!/usr/bin/env python3
"""Streaming replay of real chat timelines through `incremental_leaves` (no API calls).

Checks the live-ingest contract on real timestamps instead of a static batch:
  equivalence   live build (wall-clock ticks between events) == batch build of the same log
  edits/deletes seeded synthetic edits/deletes: deleted ids never visible after the delete; every
                change dirties exactly one closed leaf (or none while the leaf is still open)
  future        at random replay checkpoints no visible message is newer than the clock
  reexport      Kakao: import export A, then an overlapping export B (day-aligned) → no duplicates,
                same leaves as one full import
  spans         leaf time spans per close policy (fixed count vs count + idle gap)
  freshness     message → leaf-closed lag (when dense search can see it); share needing the hot tail

Inputs are sandbox index dirs (messages.json). Only counts/durations are printed.
"""

from __future__ import annotations

import argparse
import json
import random
import statistics
import sys
import time
from datetime import datetime
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO / "experimental" / "rag-leaf-parent"))

from incremental_leaves import ChatEvent, IncrementalLeafBuilder, kakao_native_keys  # noqa: E402


def _ts(text: Any, tz: Any = None) -> float | None:
    """Epoch seconds. Naive times need an explicitly declared source timezone (`tz`); otherwise the row
    is rejected — never silently read as host-local time."""

    if not text:
        return None
    t = str(text)
    if t.endswith("Z"):
        t = t[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(t)
    except ValueError:
        return None
    if dt.tzinfo is None:
        if tz is None:
            return None  # adapters must resolve timezone; unresolved rows are rejected, not guessed
        dt = dt.replace(tzinfo=tz)
    return dt.timestamp()


def load_events(data_dir: Path, *, kakao_keys: bool, tz: Any = None) -> tuple[list[ChatEvent], dict[str, int]]:
    rows = json.loads((data_dir / "messages.json").read_text(encoding="utf-8"))
    rows.sort(key=lambda r: int(r.get("index") or 0))
    keys = kakao_native_keys(rows) if kakao_keys else [str(r["observation_id"]) for r in rows]
    events: list[ChatEvent] = []
    skipped = {"no_tz_or_time": 0}
    for r, k in zip(rows, keys):
        ts = _ts(r.get("parsed_time"), tz)
        if ts is None:
            skipped["no_tz_or_time"] += 1
            continue
        events.append(ChatEvent("create", str(r["source_room_id"]), k, ts, str(r.get("actor_label") or ""),
                                str(r.get("content") or "")))
    # the global arrival log interleaves rooms by time; within a room the source order is kept
    events.sort(key=lambda e: e.ts)
    return events, skipped


def seed_mutations(events: list[ChatEvent], rng: random.Random, *, edit_rate: float, delete_rate: float) -> list[ChatEvent]:
    out = list(events)
    for ev in events:
        u = rng.random()
        if u < edit_rate:
            out.append(ChatEvent("edit", ev.room, ev.native_id, ev.ts + rng.uniform(60, 3 * 86400), ev.label,
                                 ev.content + " [edited]"))
        elif u < edit_rate + delete_rate:
            out.append(ChatEvent("delete", ev.room, ev.native_id, ev.ts + rng.uniform(60, 3 * 86400)))
    out.sort(key=lambda e: (e.ts, 0 if e.kind == "create" else 1))
    return out


def pct(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    values = sorted(values)
    return values[min(len(values) - 1, int(q * len(values)))]


def span_stats(builder: IncrementalLeafBuilder) -> dict[str, Any]:
    spans, sizes = [], []
    for leaf in builder.leaves.values():
        ts = [builder.slots[(leaf.room, n)].ts for n in leaf.slots]
        spans.append(max(ts) - min(ts))
        sizes.append(len(ts))
    n = len(spans) or 1
    return {"leaves": len(spans), "median_msgs": statistics.median(sizes) if sizes else 0,
            "span_gt_1h": round(sum(s > 3600 for s in spans) / n, 4),
            "span_gt_24h": round(sum(s > 86400 for s in spans) / n, 4),
            "span_gt_7d": round(sum(s > 7 * 86400 for s in spans) / n, 4),
            "n_span_gt_24h": sum(s > 86400 for s in spans)}


def freshness(events: list[ChatEvent], *, size: int, idle_gap_s: float | None) -> dict[str, Any]:
    """Lag from a message to the moment its leaf closes (dense-searchable if embedding is instant).
    Close time = arrival of the size-th message, or last message + idle gap (tick), whichever first."""

    b = IncrementalLeafBuilder(size=size, idle_gap_s=idle_gap_s)
    b.apply_all(e for e in events if e.kind == "create")
    lags = []
    for leaf in b.leaves.values():
        ts = [b.slots[(leaf.room, n)].ts for n in leaf.slots]
        if leaf.close_reason == "size":
            close = max(ts)
        elif idle_gap_s is not None:
            close = max(ts) + idle_gap_s
        else:
            continue  # still open at end of log: unbounded
        lags.extend(close - t for t in ts)
    open_tail = sum(len(l.slots) for l in b.leaves.values() if not l.closed)
    return {"p50_min": round(pct(lags, 0.5) / 60, 1), "p90_min": round(pct(lags, 0.9) / 60, 1),
            "p99_min": round(pct(lags, 0.99) / 60, 1),
            "share_wait_gt_5min": round(sum(l > 300 for l in lags) / max(1, len(lags)), 4),
            "share_wait_gt_1h": round(sum(l > 3600 for l in lags) / max(1, len(lags)), 4),
            "unclosed_at_end_msgs": open_tail}


def replay_checks(events: list[ChatEvent], *, size: int, idle_gap_s: float | None, rng: random.Random,
                  tick_every_s: float, checkpoints: int) -> dict[str, Any]:
    live = IncrementalLeafBuilder(size=size, idle_gap_s=idle_gap_s)
    deleted: set[tuple[str, str]] = set()
    violations = {"deleted_visible": 0, "future_visible": 0, "multi_leaf_dirty": 0}
    dirty_per_change: list[int] = []
    check_at = set(rng.sample(range(len(events)), min(checkpoints, len(events))))
    next_tick = events[0].ts if events else 0.0
    t0 = time.perf_counter()
    for i, ev in enumerate(events):
        while next_tick <= ev.ts:
            live.tick(next_tick)
            next_tick += tick_every_s
        if ev.kind in ("edit", "delete"):
            live.mark_embedded(live.pending_embed_jobs())
            before = set(live.pending_embed_jobs())
            live.apply(ev)
            after = set(live.pending_embed_jobs()) - before
            dirty_per_change.append(len(after))
            if len(after) > 1:
                violations["multi_leaf_dirty"] += 1
            if ev.kind == "delete":
                deleted.add((ev.room, ev.native_id))
        else:
            live.apply(ev)
        if i in check_at:
            for doc in live.searchable_docs(as_of=ev.ts):
                if doc["max_ts"] > ev.ts:
                    violations["future_visible"] += 1
                if any((doc["room"], n) in deleted for n in doc["native_ids"]):
                    violations["deleted_visible"] += 1
    elapsed = time.perf_counter() - t0
    batch = IncrementalLeafBuilder(size=size, idle_gap_s=idle_gap_s).apply_all(events)
    return {"events": len(events), "events_per_sec": round(len(events) / max(elapsed, 1e-9)),
            "batch_equals_live": batch.boundaries() == live.boundaries(),
            "changes": len(dirty_per_change), "changes_dirtying_0": sum(1 for x in dirty_per_change if x == 0),
            "changes_dirtying_1": sum(1 for x in dirty_per_change if x == 1), "violations": violations,
            "stats": dict(live.stats)}


def reexport_check(data_dir: Path, *, size: int, idle_gap_s: float | None, tz: Any = None) -> dict[str, Any]:
    rows = json.loads((data_dir / "messages.json").read_text(encoding="utf-8"))
    rows.sort(key=lambda r: int(r.get("index") or 0))
    days = sorted({str(r.get("parsed_time") or "")[:10] for r in rows})
    cut_a, cut_b = days[int(len(days) * 0.7)], days[int(len(days) * 0.5)]
    export_a = [r for r in rows if str(r.get("parsed_time") or "")[:10] < cut_a]
    export_b = [r for r in rows if str(r.get("parsed_time") or "")[:10] >= cut_b]

    def evs(sub: list[dict[str, Any]]) -> list[ChatEvent]:
        # each export derives its own keys; stable keys make the overlap collapse
        return [ChatEvent("create", str(r["source_room_id"]), k, _ts(r["parsed_time"], tz) or 0.0,
                          str(r.get("actor_label") or ""), str(r.get("content") or ""))
                for r, k in zip(sub, kakao_native_keys(sub))]

    appended = IncrementalLeafBuilder(size=size, idle_gap_s=idle_gap_s).apply_all(evs(export_a) + evs(export_b))
    full = IncrementalLeafBuilder(size=size, idle_gap_s=idle_gap_s).apply_all(evs(rows))
    overlap = len(export_a) + len(export_b) - len(rows)
    return {"export_a": len(export_a), "export_b": len(export_b), "overlap_rows": overlap,
            "duplicates_ignored": appended.stats.get("duplicate_create_ignored", 0),
            "slots_after_append": len(appended.slots), "slots_full": len(full.slots),
            "boundaries_equal_full_import": appended.boundaries() == full.boundaries()}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", type=Path, action="append", required=True)
    ap.add_argument("--kakao", action="append", default=[], help="data-dir names that need derived keys")
    ap.add_argument("--source-tz", action="append", default=[],
                    help="name=IANA zone for sources whose raw timestamps carry no offset (declared, not guessed)")
    ap.add_argument("--size", type=int, default=8)
    ap.add_argument("--gaps-min", default="0,30,120,360")
    ap.add_argument("--replay-gap-min", type=float, default=120)
    ap.add_argument("--edit-rate", type=float, default=0.01)
    ap.add_argument("--delete-rate", type=float, default=0.005)
    ap.add_argument("--tick-min", type=float, default=5)
    ap.add_argument("--checkpoints", type=int, default=60)
    ap.add_argument("--seed", type=int, default=20260923)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    from zoneinfo import ZoneInfo

    declared_tz = {k: ZoneInfo(v) for k, v in (x.split("=", 1) for x in args.source_tz)}
    rng = random.Random(args.seed)
    report: dict[str, Any] = {}
    for data_dir in args.data_dir:
        name = data_dir.name
        is_kakao = name in args.kakao
        tz = declared_tz.get(name)
        events, skipped = load_events(data_dir, kakao_keys=is_kakao, tz=tz)
        entry: dict[str, Any] = {"messages": len(events), "skipped": skipped, "declared_tz": str(tz or ""),
                                 "policies": {}}
        for g in [float(x) for x in args.gaps_min.split(",")]:
            gap_s = g * 60 if g > 0 else None
            b = IncrementalLeafBuilder(size=args.size, idle_gap_s=gap_s).apply_all(events)
            entry["policies"][f"size{args.size}_gap{int(g)}m"] = {
                "spans": span_stats(b), "freshness": freshness(events, size=args.size, idle_gap_s=gap_s)}
        mutated = seed_mutations(events, rng, edit_rate=args.edit_rate, delete_rate=args.delete_rate)
        entry["replay"] = replay_checks(mutated, size=args.size, idle_gap_s=args.replay_gap_min * 60, rng=rng,
                                        tick_every_s=args.tick_min * 60, checkpoints=args.checkpoints)
        if is_kakao:
            entry["reexport"] = reexport_check(data_dir, size=args.size, idle_gap_s=args.replay_gap_min * 60, tz=tz)
        report[name] = entry
        print(json.dumps({name: {"replay": entry["replay"]["violations"],
                                 "batch_equals_live": entry["replay"]["batch_equals_live"]}}), flush=True)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(report, ensure_ascii=False, indent=1), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
