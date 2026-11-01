#!/usr/bin/env python3
"""Draft temporal gold from the current index. Output is unreviewed synthetic.

Gold leaves must actually sit inside the asked time range. Distractors are older
leaves from the same speaker in the same room. This is a review sheet seed, not a
production dataset — gold_kind stays synthetic.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
sys.path.insert(0, str(CODE))

from engine import load_index  # noqa: E402


KINDS = (
    ("today", 1, "오늘 {speaker}가 뭐라고 했어?"),
    ("yesterday", 2, "어제 {speaker} 얘기 뭐였지?"),
    ("last_week", 7, "지난주 {speaker}가 뭐 한다고 했어?"),
    ("recent", 14, "최근 {speaker}가 뭐라고 했어?"),
)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", type=Path, default=REPO / ".masamong-scratch" / "retrieval-v2" / "temporal-gold.draft.json")
    ap.add_argument("--per-kind", type=int, default=4)
    args = ap.parse_args()
    os.chdir(CODE)
    index = load_index()
    tmax = float(index.corpus_tmax or 0)
    if tmax <= 0:
        print(json.dumps({"error": "no_corpus_tmax"}))
        return 2
    by_room_speaker: dict[tuple[str, str], list[str]] = defaultdict(list)
    for doc_id, ts in (index.leaf_end_ts or {}).items():
        leaf = index.leaf_by_id.get(doc_id) or {}
        room = str(leaf.get("source_room_id") or "")
        for sp in leaf.get("speakers") or []:
            by_room_speaker[(room, str(sp))].append(doc_id)

    questions: list[dict] = []
    qn = 0
    for kind, days, template in KINDS:
        lo = tmax - timedelta(days=days).total_seconds()
        n_kind = 0
        for (room, speaker), ids in sorted(by_room_speaker.items()):
            recent = [d for d in ids if float(index.leaf_end_ts[d]) >= lo]
            old = [d for d in ids if float(index.leaf_end_ts[d]) < lo]
            if not recent or not old:
                continue
            gold = max(recent, key=lambda d: index.leaf_end_ts[d])
            distractor = max(old, key=lambda d: index.leaf_end_ts[d])
            leaf = index.leaf_by_id[gold]
            obs = list(leaf.get("observation_ids") or [])[:1]
            if not obs:
                continue
            qn += 1
            n_kind += 1
            questions.append(
                {
                    "question_id": f"temporal-{kind}-{qn:03d}",
                    "query": template.format(speaker=speaker),
                    "question_type": "temporal",
                    "time_kind": kind,
                    "want_observation_ids": obs,
                    "source_leaf_doc_id": gold,
                    "distractor_leaf_doc_id": distractor,
                    "source_room_id": room,
                    "expected_abstention": False,
                    "review_status": "unreviewed",
                    "gold_kind": "synthetic",
                    "note": "gold leaf is inside the asked window; distractor is the same speaker outside it",
                }
            )
            if n_kind >= args.per_kind:
                break
    payload = {
        "gold_kind": "synthetic",
        "not_human_reviewed": True,
        "n": len(questions),
        "questions": questions,
        "built_at": datetime.now(timezone.utc).isoformat(),
    }
    args.out = Path(args.out).resolve()
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"out": str(args.out), "n": len(questions), "human_reviewed": False}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
