"""Human review pack builder. Synthetic-safe: does not call paid models.

Writes only to an explicit output directory. Public stdout is counts, never
question text or observation bodies.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Mapping, Sequence

_EVAL_DIR = Path(__file__).resolve().parents[1]
if str(_EVAL_DIR) not in sys.path:
    sys.path.insert(0, str(_EVAL_DIR))

from evidence_metrics import GoldSet, SplitLeakageError, load_gold  # noqa: E402

from rag_upgrade.quality_gate import PromotionRefusedError  # noqa: E402

BUCKETS = (
    "person",
    "event",
    "followup",
    "time",
    "abstention",
    "tool",
    "casual",
)

REVIEW_BLANKS = {
    "answer_support": None,
    "person_attribution": None,
    "abstention_correct": None,
    "notes": "",
    "reviewer": None,
    "review_status": "unreviewed",
}


def _bucket(question) -> str:
    if question.abstain_expected:
        return "abstention"
    prompt = (question.prompt or "").lower()
    if any(tok in prompt for tok in ("언제", "어제", "오늘", "그때")):
        return "time"
    if any(tok in prompt for tok in ("누구", "사람", "별명")):
        return "person"
    if len(question.required_evidence_groups) > 1:
        return "event"
    if not question.required_evidence_groups:
        return "casual"
    return "followup"


def _group_key(question) -> str:
    return str(getattr(question, "question_split_group", None) or getattr(question, "episode_group", None) or question.question_id)


def prepare_pack(
    gold: GoldSet | Mapping[str, Any] | str | Path,
    *,
    sample_size: int = 20,
    out_dir: str | Path | None = None,
    seed: int = 7,
) -> dict[str, Any]:
    gold_set = gold if isinstance(gold, GoldSet) else load_gold(gold)
    if gold_set.gold_kind == "human_reviewed":
        raise PromotionRefusedError("prepare_review_pack does not promote or rewrite reviewed gold")

    by_bucket: dict[str, list] = defaultdict(list)
    by_room: Counter[str] = Counter()
    groups: dict[str, set[str]] = defaultdict(set)
    for q in gold_set.questions:
        by_bucket[_bucket(q)].append(q)
        rooms = tuple(getattr(q, "allowed_source_channel_ids", ()) or ())
        if rooms:
            by_room[str(rooms[0])] += 1
        else:
            by_room["(unspecified)"] += 1
        groups[_group_key(q)].add(q.split)

    leaked = [g for g, splits in groups.items() if len(splits) > 1]
    if leaked:
        raise SplitLeakageError("split group leak: " + ",".join(sorted(leaked)[:8]))

    # round-robin across buckets then rooms so small rooms survive
    ordered: list = []
    seen: set[str] = set()
    max_len = max((len(v) for v in by_bucket.values()), default=0)
    for i in range(max_len):
        for name in BUCKETS:
            rows = by_bucket[name]
            if i < len(rows):
                q = rows[i]
                if q.question_id not in seen:
                    ordered.append(q)
                    seen.add(q.question_id)
    for q in gold_set.questions:
        if q.question_id not in seen:
            ordered.append(q)
            seen.add(q.question_id)

    n = min(int(sample_size), len(ordered))
    sample = ordered[:n]
    items = []
    for q in sample:
        items.append(
            {
                "question_id": q.question_id,
                "split": q.split,
                "bucket": _bucket(q),
                "group": _group_key(q),
                "rooms": list(getattr(q, "allowed_source_channel_ids", ()) or ()),
                "abstain_expected": bool(q.abstain_expected),
                "review": dict(REVIEW_BLANKS),
                "observation_refs": [
                    {"observation_id": oid, "body": None}
                    for group in q.required_evidence_groups
                    for oid in group.alternatives
                ],
            }
        )

    summary = {
        "dataset_id": gold_set.dataset_id,
        "gold_kind": gold_set.gold_kind,
        "review_status": gold_set.review_status,
        "question_count": len(gold_set.questions),
        "sample_size": n,
        "bucket_counts": {k: len(v) for k, v in by_bucket.items()},
        "room_counts": dict(by_room),
        "split_counts": dict(Counter(q.split for q in gold_set.questions)),
        "note": "Pack is unreviewed. Empty review fields must not be treated as approval.",
    }
    pack = {"summary": summary, "items": items}
    if out_dir is not None:
        dest = Path(out_dir)
        dest.mkdir(parents=True, exist_ok=True)
        (dest / "pack_summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")
        private_items = []
        obs_text = {oid: obs.text for oid, obs in gold_set.observations.items()}
        for item in items:
            row = dict(item)
            row["observation_refs"] = [
                {"observation_id": ref["observation_id"], "body": obs_text.get(ref["observation_id"])}
                for ref in item["observation_refs"]
            ]
            q = next(x for x in gold_set.questions if x.question_id == item["question_id"])
            row["query"] = q.prompt
            private_items.append(row)
        (dest / "review_pack.json").write_text(json.dumps(private_items, ensure_ascii=False, indent=2), encoding="utf-8")
    return pack


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Prepare an unreviewed human-review pack")
    p.add_argument("--gold", required=True)
    p.add_argument("--out", required=True, help="isolated directory under .masamong-scratch")
    p.add_argument("--sample-size", type=int, default=20)
    args = p.parse_args(argv)
    pack = prepare_pack(args.gold, sample_size=args.sample_size, out_dir=args.out)
    print(json.dumps(pack["summary"], ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
