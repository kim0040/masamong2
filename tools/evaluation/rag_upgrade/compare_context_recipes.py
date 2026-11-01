#!/usr/bin/env python3
"""Offline structure comparison of three context recipes.

Does not call embed/summary APIs. Does not rank answer quality. Gold remains
unreviewed unless the caller passes a reviewed pack (then lexical-only coverage
can be attached; this CLI still will not invent a meaning score).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from collections import Counter
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

HERE = Path(__file__).resolve()
REPO = HERE.parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
if str(CODE) not in sys.path:
    sys.path.insert(0, str(CODE))

from adaptive_context import build_chunks  # noqa: E402
from evidence_packing import pack_evidence_set  # noqa: E402

GAP_MINUTES_HYPOTHESIS = 30.0
SPEAKER_MAX_CHARS = 800
SPEAKER_MAX_SECONDS = 20 * 60
DEFAULT_BUDGET = 4000
RECIPES = ("fixed8", "gap30_max8", "speaker_run")


def _sha(payload: Any) -> str:
    blob = json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str)
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def _room(row: Mapping[str, Any]) -> str:
    return str(row.get("source_room_original") or row.get("source_room_id") or "")


def _ts(row: Mapping[str, Any]) -> float | None:
    raw = row.get("parsed_time")
    if raw is None or raw == "":
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    text = str(raw).strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text).timestamp()
    except ValueError:
        return None


def _actor(row: Mapping[str, Any]) -> str:
    return str(row.get("actor_label") or "")


def _content(row: Mapping[str, Any]) -> str:
    return str(row.get("content") or "")


def windows_fixed8(messages: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    chunks = build_chunks(list(messages), "fixed8")
    out = []
    for ch in chunks:
        idxs = list(ch.get("message_indices") or [])
        if not idxs:
            continue
        out.append(_window(messages, idxs, recipe="fixed8", room=_room(messages[idxs[0]])))
    return out


def windows_gap30_max8(messages: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    """Hypothesis: 30-minute gap starts a new window; hard cap 8 messages. Not a quality claim."""

    windows: list[dict[str, Any]] = []
    current: list[int] = []
    prev_ts: float | None = None
    prev_room = ""
    gap = GAP_MINUTES_HYPOTHESIS * 60.0
    for i, row in enumerate(messages):
        room = _room(row)
        ts = _ts(row)
        split = False
        if current and room != prev_room:
            split = True
        elif current and prev_ts is not None and ts is not None and (ts - prev_ts) > gap:
            split = True
        elif len(current) >= 8:
            split = True
        if split and current:
            windows.append(_window(messages, current, recipe="gap30_max8", room=prev_room))
            current = []
        current.append(i)
        prev_ts = ts if ts is not None else prev_ts
        prev_room = room
    if current:
        windows.append(_window(messages, current, recipe="gap30_max8", room=prev_room))
    return windows


def windows_speaker_run(messages: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    """Same speaker consecutive run, capped by time and character budget. Hypothesis."""

    windows: list[dict[str, Any]] = []
    current: list[int] = []
    chars = 0
    start_ts: float | None = None
    prev_actor = None
    prev_room = ""
    for i, row in enumerate(messages):
        room = _room(row)
        actor = _actor(row)
        ts = _ts(row)
        body = _content(row)
        split = False
        if current and (room != prev_room or actor != prev_actor):
            split = True
        elif current and start_ts is not None and ts is not None and (ts - start_ts) > SPEAKER_MAX_SECONDS:
            split = True
        elif current and chars + len(body) > SPEAKER_MAX_CHARS:
            split = True
        if split and current:
            windows.append(_window(messages, current, recipe="speaker_run", room=prev_room))
            current, chars, start_ts = [], 0, None
        if not current:
            start_ts = ts
        current.append(i)
        chars += len(body)
        prev_actor = actor
        prev_room = room
    if current:
        windows.append(_window(messages, current, recipe="speaker_run", room=prev_room))
    return windows


def _window(messages: Sequence[Mapping[str, Any]], idxs: Sequence[int], *, recipe: str, room: str) -> dict[str, Any]:
    start, end = int(idxs[0]), int(idxs[-1]) + 1
    oids = [str(messages[i].get("observation_id") or f"row:{i}") for i in idxs]
    bodies = [_content(messages[i]) for i in idxs]
    gaps = []
    for a, b in zip(idxs, idxs[1:]):
        ta, tb = _ts(messages[a]), _ts(messages[b])
        if ta is not None and tb is not None:
            gaps.append(tb - ta)
    return {
        "recipe": recipe,
        "doc_id": f"{recipe}:{room}:{start}-{end}",
        "start": start,
        "end": end,
        "source_room_id": room,
        "source_room_original": room,
        "observation_ids": oids,
        "n_messages": len(idxs),
        "chars": sum(len(b) for b in bodies),
        "max_gap_s": max(gaps) if gaps else 0.0,
        "has_correction": any("정정" in b or "취소" in b or "아니" == b.strip() for b in bodies),
        "has_long": any(len(b) >= 80 for b in bodies),
        "speakers": list(dict.fromkeys(_actor(messages[i]) for i in idxs)),
    }


BUILDERS = {
    "fixed8": windows_fixed8,
    "gap30_max8": windows_gap30_max8,
    "speaker_run": windows_speaker_run,
}


def structure_report(messages: Sequence[Mapping[str, Any]], *, char_budget: int = DEFAULT_BUDGET) -> dict[str, Any]:
    rooms = {_room(m) for m in messages}
    mixed = 0
    by_recipe: dict[str, Any] = {}
    for name, builder in BUILDERS.items():
        wins = builder(messages)
        room_set = {_room(messages[w["start"]]) for w in wins if w["end"] > w["start"]}
        mixed += sum(1 for w in wins if _room(messages[w["start"]]) != w["source_room_id"])
        packed = pack_evidence_set(
            messages,
            wins[:12],
            before=0,
            after=0,
            with_prefix=False,
            doc_char_limit=char_budget,
            total_char_limit=char_budget,
            allowed_rooms=None,
            dedupe_visible=True,
        )
        by_recipe[name] = {
            "n_windows": len(wins),
            "rooms": sorted(room_set),
            "mean_messages": round(sum(w["n_messages"] for w in wins) / max(1, len(wins)), 3),
            "mean_chars": round(sum(w["chars"] for w in wins) / max(1, len(wins)), 1),
            "windows_with_long": sum(1 for w in wins if w["has_long"]),
            "windows_with_correction": sum(1 for w in wins if w["has_correction"]),
            "max_gap_s_p95": _p95([w["max_gap_s"] for w in wins]),
            "packed_visible": len(packed.get("visible_observation_ids") or []),
            "packed_used_chars": packed.get("used_chars"),
            "hypothesis": name != "fixed8",
        }
    return {
        "n_messages": len(messages),
        "n_rooms": len(rooms),
        "room_mix_violations": mixed,
        "char_budget": char_budget,
        "input_sha": _sha([m.get("observation_id") for m in messages]),
        "recipes": by_recipe,
        "meaning_quality": "not_evaluated",
        "layers_run": ["structure", "visible_span_pack"],
    }


def _p95(vals: Iterable[float]) -> float:
    seq = sorted(float(v) for v in vals)
    if not seq:
        return 0.0
    return seq[min(len(seq) - 1, int(round(0.95 * (len(seq) - 1))))]


def sample_messages(rows: Sequence[Mapping[str, Any]], *, seed: str, limit: int) -> list[dict[str, Any]]:
    """Deterministic cap. Stratify by original room, keep ordinal order inside each room."""

    by_room: dict[str, list[Mapping[str, Any]]] = {}
    for row in rows:
        by_room.setdefault(_room(row), []).append(row)
    rooms = sorted(by_room)
    if not rooms or limit <= 0:
        return []
    per = max(1, limit // len(rooms))
    out: list[dict[str, Any]] = []
    salt = int(_sha(seed)[:8], 16)
    for i, room in enumerate(rooms):
        block = list(by_room[room])
        take = per if i < len(rooms) - 1 else max(0, limit - len(out))
        if len(block) <= take:
            chosen = block
        else:
            start = (salt + i * 17) % max(1, len(block) - take + 1)
            chosen = block[start : start + take]
        out.extend(dict(r) for r in chosen)
    return out[:limit]


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Compare context recipes (structure only)")
    p.add_argument("--messages", type=Path, help="JSON list of synthetic or local copied rows")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--seed", default="rag-hardening-20260921/09")
    p.add_argument("--limit", type=int, default=48)
    p.add_argument("--char-budget", type=int, default=DEFAULT_BUDGET)
    p.add_argument("--out", type=Path, default=None)
    args = p.parse_args(argv)
    if args.dry_run and not args.messages:
        rows = _synthetic()
    elif args.messages:
        rows = json.loads(Path(args.messages).read_text(encoding="utf-8"))
        if not isinstance(rows, list):
            raise SystemExit("messages file must be a JSON list")
    else:
        p.error("provide --messages or --dry-run")
    sampled = sample_messages(rows, seed=args.seed, limit=args.limit)
    report = structure_report(sampled, char_budget=args.char_budget)
    report["seed"] = args.seed
    report["dry_run"] = bool(args.dry_run)
    text = json.dumps(report, ensure_ascii=False, indent=2)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(text + "\n", encoding="utf-8")
    else:
        print(text)
    return 0


def _synthetic() -> list[dict[str, Any]]:
    rows = []
    t0 = datetime(2026, 9, 1, 12, 0, 0)
    rooms = ("kakao:a", "kakao:b")
    speakers = ("가", "나", "다")
    for r, room in enumerate(rooms):
        for i in range(16):
            extra = 0 if i != 7 else 45
            body = "ㅇㅇ" if i % 5 else ("정정이야 그거 아니야" if i == 9 else f"이야기 {i} " + ("장문 " * (12 if i == 3 else 1)))
            ts = t0.timestamp() + r * 10_000 + i * 60 + extra * 60
            rows.append(
                {
                    "observation_id": f"{room}:{i}",
                    "source_room_id": room,
                    "source_room_original": room,
                    "actor_label": speakers[i % 3],
                    "content": body,
                    "parsed_time": datetime.fromtimestamp(ts).isoformat(),
                    "sample_block": room,
                }
            )
    return rows


if __name__ == "__main__":
    raise SystemExit(main())
