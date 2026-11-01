#!/usr/bin/env python3
"""Holdout gold for the 2026-09-23 generalization check (overfitting audit).

Why a new generator: gold v3 was written *from* the index's own leaves (leaf-boundary bias, 80% one
room, high question↔source overlap) and then used to pick every retrieval knob. This builder is
independent of leaf boundaries and of the tuned sets:

- anchors are single *messages* (not leaves), stratified by room (sqrt share, min per room) and by
  time decile inside each room; messages already used as gold evidence can be excluded
- the LLM sees the anchor inside ±6 raw messages and must paraphrase; questions whose character
  bigrams are mostly copied from the anchor are dropped (`max_containment`), and any leaked
  scaffold (line markers, ids) is rejected
- unanswerable questions are generated from a *different collection* and scoped to a room of this
  index (`expected_abstention: true`) — they test abstention and room isolation together
- output is bound to the index (index_binding + corpus_binding) and marked synthetic/unreviewed;
  a review sheet is written next to it. Nothing here is a human-reviewed gold.

`--anchor span` builds the *flow* variant: the anchor is a run of 3-6 consecutive messages (one room,
>= 2 speakers, short gaps) and the question must need at least two of them. Gold evidence is every
message id of the span (`evidence_match: "any"` -- the runner/score.py count a hit when a retrieved
leaf contains any of them; `span_observation_ids` keeps the full set for an all-ids check).

Outputs stay under .masamong-scratch/. Conversation text is never printed.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import random
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
sys.path.insert(0, str(CODE))
sys.path.insert(0, str(EVAL))

PLACEHOLDER_RE = re.compile(
    r"^(사진|동영상|이모티콘|파일|음성메시지|삭제된 메시지입니다\.?|사진 \d+장|샵검색.*|보이스톡.*|페이스톡.*)$"
)
URL_ONLY_RE = re.compile(r"^\s*(https?://\S+\s*)+$")
MENTION_RE = re.compile(r"<@!?\d+>|<#\d+>|<a?:\w+:\d+>")
SCAFFOLD_RE = re.compile(r"\[\*\]|\bm\d{3}\b|\bg\d{2}\b|obs[-_:]|observation|leaf|L\d{2,}", re.I)
QUESTION_TYPES = ("fact", "person", "when")
SPAN_TYPES = ("decision", "plan_change", "who_agreed", "summary_of_discussion")
SPAN_SCAFFOLD_RE = re.compile(r"\[\*\d*\]|\d+\s*번째\s*줄|\d+\s*번\s*줄")


# ---------------------------------------------------------------- pure helpers (unit tested)


def char_bigrams(text: str) -> set[str]:
    s = re.sub(r"\s+", "", str(text or "").lower())
    return {s[i : i + 2] for i in range(len(s) - 1)}


def containment(question: str, source: str, *, ignore: Iterable[str] = ()) -> float:
    """Share of the question's character bigrams that also occur in the source (names ignored)."""

    q = str(question or "")
    for name in ignore:
        if name and len(name) >= 2:
            q = q.replace(name, " ")
    qb = char_bigrams(q)
    if not qb:
        return 0.0
    return len(qb & char_bigrams(source)) / len(qb)


def longest_common_substring(a: str, b: str) -> int:
    a = re.sub(r"\s+", "", a or "")
    b = re.sub(r"\s+", "", b or "")
    best = 0
    prev = [0] * (len(b) + 1)
    for i in range(1, len(a) + 1):
        cur = [0] * (len(b) + 1)
        for j in range(1, len(b) + 1):
            if a[i - 1] == b[j - 1]:
                cur[j] = prev[j - 1] + 1
                best = max(best, cur[j])
        prev = cur
    return best


def eligible_anchor(content: str, *, min_chars: int = 15) -> bool:
    text = str(content or "").strip()
    if len(text) < min_chars:
        return False
    if PLACEHOLDER_RE.match(text) or URL_ONLY_RE.match(text):
        return False
    stripped = MENTION_RE.sub("", text).strip()
    if len(stripped) < min_chars:
        return False
    # mostly laughter / jamo noise
    jamo = sum(1 for c in stripped if "ㄱ" <= c <= "ㆎ")
    return jamo / max(1, len(stripped)) < 0.4


def room_quota(room_sizes: Mapping[str, int], total: int, *, min_per_room: int, min_room_msgs: int) -> dict[str, int]:
    """sqrt-proportional allocation so one huge room cannot dominate; tiny rooms are skipped."""

    rooms = {r: n for r, n in room_sizes.items() if n >= min_room_msgs}
    if not rooms or total <= 0:
        return {}
    weights = {r: math.sqrt(n) for r, n in rooms.items()}
    wsum = sum(weights.values())
    quota = {r: max(min_per_room, int(round(total * w / wsum))) for r, w in weights.items()}
    # trim the largest rooms if the minimums pushed us over
    while sum(quota.values()) > total:
        r = max(quota, key=lambda k: (quota[k], k))
        if quota[r] <= min_per_room:
            break
        quota[r] -= 1
    return quota


def stratified_anchors(
    rows: Sequence[Mapping[str, Any]],
    k: int,
    rng: random.Random,
    *,
    deciles: int = 10,
) -> list[int]:
    """Pick k positions from `rows` (one room, time-ordered), spread across time deciles."""

    if k <= 0 or not rows:
        return []
    n = len(rows)
    buckets: list[list[int]] = [[] for _ in range(deciles)]
    for pos in range(n):
        buckets[min(deciles - 1, pos * deciles // n)].append(pos)
    order = list(range(deciles))
    rng.shuffle(order)
    picked: list[int] = []
    for bi in (order * (k // deciles + 2)):
        if len(picked) >= k:
            break
        pool = [p for p in buckets[bi] if p not in picked]
        if pool:
            picked.append(rng.choice(pool))
    return picked


def parse_json_object(text: str) -> dict[str, Any] | None:
    raw = str(text or "").strip()
    raw = re.sub(r"^```(?:json)?|```$", "", raw, flags=re.M).strip()
    m = re.search(r"\{.*\}", raw, flags=re.S)
    if not m:
        return None
    try:
        obj = json.loads(m.group(0))
    except json.JSONDecodeError:
        return None
    return obj if isinstance(obj, dict) else None


def review_question(
    obj: Mapping[str, Any] | None,
    *,
    anchor_text: str,
    speaker_labels: Iterable[str],
    max_containment: float,
    max_common_run: int,
) -> tuple[bool, str, dict[str, Any]]:
    """Accept/reject one generated question. Returns (ok, reason, stats)."""

    if not obj:
        return False, "unparsable", {}
    if bool(obj.get("skip")):
        return False, "model_skip", {}
    q = str(obj.get("question") or "").strip()
    a = str(obj.get("answer") or "").strip()
    if not (4 <= len(q) <= 120) or not a:
        return False, "bad_length", {}
    if SCAFFOLD_RE.search(q) or SCAFFOLD_RE.search(a):
        return False, "scaffold_leak", {}
    labels = [str(x) for x in speaker_labels if str(x).strip()]
    c = containment(q, anchor_text, ignore=labels)
    run = longest_common_substring(q, anchor_text)
    stats = {"containment": round(c, 3), "common_run": run}
    if c > max_containment:
        return False, "copied_bigrams", stats
    if run > max_common_run:
        return False, "copied_span", stats
    return True, "ok", stats


def _minutes_between(a: Any, b: Any) -> float | None:
    from datetime import datetime

    try:
        return abs((datetime.fromisoformat(str(b)) - datetime.fromisoformat(str(a))).total_seconds()) / 60.0
    except (TypeError, ValueError):
        return None


def valid_span(
    rows: Sequence[Mapping[str, Any]],
    start: int,
    length: int,
    *,
    max_gap_minutes: float = 30.0,
    exclude: set[str] | frozenset[str] = frozenset(),
    min_substantive: int = 2,
    substantive_chars: int = 10,
) -> bool:
    """A flow anchor: `length` consecutive messages of one room, >= 2 speakers, no long pause inside,
    at least `min_substantive` messages with real content, none of them already used as gold."""

    end = start + length
    if start < 0 or length < 2 or end > len(rows):
        return False
    span = rows[start:end]
    if any(str(r.get("observation_id")) in exclude for r in span):
        return False
    if len({str(r.get("actor_label") or r.get("actor_account") or "") for r in span}) < 2:
        return False
    for a, b in zip(span, span[1:]):
        gap = _minutes_between(a.get("parsed_time"), b.get("parsed_time"))
        if gap is not None and gap > max_gap_minutes:
            return False
    return sum(1 for r in span if eligible_anchor(r.get("content"), min_chars=substantive_chars)) >= min_substantive


def pick_spans(
    rows: Sequence[Mapping[str, Any]],
    k: int,
    rng: random.Random,
    *,
    min_len: int = 3,
    max_len: int = 6,
    max_gap_minutes: float = 30.0,
    exclude: set[str] | frozenset[str] = frozenset(),
) -> list[tuple[int, int]]:
    """k non-overlapping (start, length) spans of one room, spread over time deciles."""

    lengths = [rng.randint(min_len, max_len) for _ in rows]
    starts = [i for i in range(len(rows)) if valid_span(rows, i, lengths[i], max_gap_minutes=max_gap_minutes, exclude=exclude)]
    if not starts:
        return []
    cand = [rows[i] for i in starts]
    picked = stratified_anchors(cand, min(len(cand), k * 3), rng)  # spare picks cover overlaps
    out: list[tuple[int, int]] = []
    taken: set[int] = set()
    for p in picked:
        s, ln = starts[p], lengths[starts[p]]
        if any(j in taken for j in range(s, s + ln)):
            continue
        taken.update(range(s, s + ln))
        out.append((s, ln))
        if len(out) >= k:
            break
    return out


def review_span_question(
    obj: Mapping[str, Any] | None,
    *,
    span_texts: Sequence[str],
    speaker_labels: Iterable[str],
    max_containment: float,
    max_common_run: int,
) -> tuple[bool, str, dict[str, Any]]:
    """Span variant of review_question: copy checks run against the whole span, the question must not
    cite line markers, and the model has to say it used >= 2 distinct lines of the span."""

    ok, why, stats = review_question(obj, anchor_text=" ".join(span_texts), speaker_labels=speaker_labels,
                                     max_containment=max_containment, max_common_run=max_common_run)
    if not ok:
        return ok, why, stats
    assert obj is not None
    if SPAN_SCAFFOLD_RE.search(str(obj.get("question") or "")) or SPAN_SCAFFOLD_RE.search(str(obj.get("answer") or "")):
        return False, "scaffold_leak", stats
    used = obj.get("lines_used")
    try:
        lines = {int(x) for x in used} if isinstance(used, (list, tuple)) else set()
    except (TypeError, ValueError):
        lines = set()
    lines = {x for x in lines if 1 <= x <= len(span_texts)}
    stats = {**stats, "lines_used": len(lines)}
    if len(lines) < 2:
        return False, "single_line", stats
    return True, "ok", stats


# ---------------------------------------------------------------- prompts

GEN_PROMPT = """너는 단체 채팅방 기록으로 기억 검색 평가 문항을 만드는 사람이다.
아래는 채팅방의 한 구간이다. [*] 표시 줄이 정답 근거다. 앞뒤 줄은 이해를 돕는 문맥이다.

{snippet}

몇 주 뒤 이 방의 멤버가 봇에게 물어볼 법한 질문을 하나 만들어라.
- 질문 유형: {qtype_desc}
- 답은 [*] 줄(필요하면 문맥)로 확인할 수 있어야 한다.
- [*] 줄의 표현을 그대로 베끼지 말고 다른 말로 바꿔 물어라. 사람 이름은 써도 된다.
- 실제 사람이 치는 짧은 구어체 한 문장. 줄 번호·기호·[*]를 언급하지 마라.
- [*] 줄이 잡담·감탄·인사라서 기억할 사실이 없으면 skip=true.
JSON 한 개만 출력: {{"question": "...", "answer": "짧은 답", "skip": false}}"""

QTYPE_DESC = {
    "fact": "무슨 일/무엇에 대한 사실 질문 (예: 뭐 샀다고 했지, 어디 간다고 했더라)",
    "person": "특정 인물이 한 말이나 그 사람에 대한 질문 (예: ○○가 ~에 대해 뭐라 했어)",
    "when": "언제·어느 시점의 일인지 묻거나 그 시점의 상황을 묻는 질문",
}


SPAN_PROMPT = """너는 단체 채팅방 기록으로 기억 검색 평가 문항을 만드는 사람이다.
아래는 채팅방의 한 구간이다. [*1], [*2]... 표시 줄은 이어진 대화 한 토막이고, 정답 근거다. 앞뒤 줄은 이해를 돕는 문맥이다.

{snippet}

몇 주 뒤 이 방의 멤버가 봇에게 물어볼 법한 질문을 하나 만들어라.
- 질문 유형: {qtype_desc}
- 답을 하려면 [*] 줄 중 서로 다른 둘 이상을 함께 봐야 해야 한다. 한 줄만 보고 답할 수 있으면 안 된다.
- [*] 줄의 표현을 그대로 베끼지 말고 다른 말로 바꿔 물어라. 사람 이름은 써도 된다.
- 실제 사람이 치는 짧은 구어체 한 문장. 줄 번호·기호·[*]를 언급하지 마라.
- [*] 줄이 잡담·감탄·인사뿐이라 기억할 내용이 없거나 위 유형의 질문이 안 나오면 skip=true.
JSON 한 개만 출력: {{"question": "...", "answer": "짧은 답", "lines_used": [답에 쓴 [*] 줄 번호들], "skip": false}}"""

SPAN_TYPE_DESC = {
    "decision": "여러 사람이 이야기한 끝에 무엇으로 정했는지/결론이 뭐였는지 묻는 질문",
    "plan_change": "계획·일정·장소·방법 등이 처음에서 나중에 어떻게 바뀌었는지 묻는 질문",
    "who_agreed": "누가 무엇에 동의·반대·합류했는지(또는 누가 어떻게 반응했는지) 묻는 질문",
    "summary_of_discussion": "그때 무슨 얘기가 오갔는지, 어떻게 흘러갔는지 요약해 달라는 질문",
}


def span_snippet(rows: Sequence[Mapping[str, Any]], start: int, length: int, *, before: int = 6, after: int = 6,
                 max_chars: int = 220) -> str:
    lines = []
    for i in range(max(0, start - before), min(len(rows), start + length + after)):
        r = rows[i]
        text = str(r.get("content") or "").replace("\n", " ")[:max_chars]
        mark = f"[*{i - start + 1}] " if start <= i < start + length else "     "
        when = str(r.get("parsed_time") or "")[:16].replace("T", " ")
        lines.append(f"{mark}{when} {r.get('actor_label') or '?'}: {text}")
    return "\n".join(lines)


def snippet_for(rows: Sequence[Mapping[str, Any]], pos: int, *, before: int = 6, after: int = 6, max_chars: int = 220) -> str:
    lines = []
    for i in range(max(0, pos - before), min(len(rows), pos + after + 1)):
        r = rows[i]
        text = str(r.get("content") or "").replace("\n", " ")[:max_chars]
        mark = "[*] " if i == pos else "    "
        when = str(r.get("parsed_time") or "")[:16].replace("T", " ")
        lines.append(f"{mark}{when} {r.get('actor_label') or '?'}: {text}")
    return "\n".join(lines)


def chat_no_reasoning(client: Any, messages: list[dict[str, str]], *, model: str, max_tokens: int, temperature: float) -> str:
    """One chat call with hidden reasoning disabled (6x cheaper for this task, same JSON validity).
    Goes through the client's budget reservation/ledger (`_post`)."""

    payload = {"model": model, "messages": messages, "max_tokens": max_tokens, "temperature": temperature,
               "reasoning": {"enabled": False}}
    body = client._post("/chat/completions", payload, fallback=0.002, kind="chat", model=model)
    choices = body.get("choices") or []
    msg = choices[0].get("message") if choices and isinstance(choices[0], dict) else None
    return str((msg or {}).get("content") or "")


# ---------------------------------------------------------------- IO


def _load_index_dir(data_dir: Path) -> tuple[list[dict], list[dict], dict]:
    messages = json.loads((data_dir / "messages.json").read_text(encoding="utf-8"))
    leaves = json.loads((data_dir / "leaves.json").read_text(encoding="utf-8"))
    meta = json.loads((data_dir / "meta.json").read_text(encoding="utf-8"))
    return messages, leaves, meta


def _rooms(messages: Sequence[Mapping[str, Any]]) -> dict[str, list[dict]]:
    by: dict[str, list[dict]] = defaultdict(list)
    for m in messages:
        by[str(m.get("source_room_id") or "")].append(dict(m))
    for rows in by.values():
        rows.sort(key=lambda r: int(r.get("index") or 0))
    return by


def _excluded_obs(paths: Sequence[Path]) -> set[str]:
    out: set[str] = set()
    for p in paths:
        g = json.loads(Path(p).read_text(encoding="utf-8"))
        for q in g.get("questions") or []:
            for key in ("want_observation_ids", "expected_evidence_ids"):
                out.update(str(x) for x in q.get(key) or [])
    return out


def _pick_anchors(
    by_room: Mapping[str, Sequence[Mapping[str, Any]]],
    total: int,
    rng: random.Random,
    *,
    exclude: set[str],
    min_per_room: int,
    min_room_msgs: int,
    oversample: float,
) -> list[tuple[str, int]]:
    quota = room_quota({r: len(v) for r, v in by_room.items()}, total, min_per_room=min_per_room, min_room_msgs=min_room_msgs)
    out: list[tuple[str, int]] = []
    for room in sorted(quota):
        rows = by_room[room]
        ok = [i for i, r in enumerate(rows) if eligible_anchor(r.get("content")) and str(r.get("observation_id")) not in exclude]
        if not ok:
            continue
        sub = [rows[i] for i in ok]
        want = max(1, int(math.ceil(quota[room] * oversample)))
        for p in stratified_anchors(sub, min(want, len(sub)), rng):
            out.append((room, ok[p]))
    return out


def _leaf_for_obs(leaves: Sequence[Mapping[str, Any]]) -> dict[str, str]:
    out: dict[str, str] = {}
    for leaf in leaves:
        for o in leaf.get("observation_ids") or []:
            out.setdefault(str(o), str(leaf["doc_id"]))
    return out


def _span_jobs(by_room: Mapping[str, Sequence[Mapping[str, Any]]], args: Any, rng: random.Random, exclude: set[str]) -> list[dict[str, Any]]:
    quota = room_quota({r: len(v) for r, v in by_room.items()}, args.n_answerable, min_per_room=args.min_per_room,
                       min_room_msgs=args.min_room_msgs)
    jobs: list[dict[str, Any]] = []
    for room in sorted(quota):
        want = max(1, int(math.ceil(quota[room] * args.oversample)))
        picks = pick_spans(by_room[room], want, rng, min_len=args.span_min_len, max_len=args.span_max_len,
                           max_gap_minutes=args.span_max_gap_min, exclude=exclude)
        for s0, ln in picks:
            jobs.append({"kind": "answerable", "room": room, "pos": s0, "length": ln, "rows": by_room[room]})
    rng.shuffle(jobs)
    for i, j in enumerate(jobs):  # round-robin so the type mix stays even after rejects
        j["qtype"] = SPAN_TYPES[i % len(SPAN_TYPES)]
    return jobs


def _prompt_for(job: Mapping[str, Any]) -> str:
    if "length" in job:
        return SPAN_PROMPT.format(snippet=span_snippet(job["rows"], job["pos"], job["length"]),
                                  qtype_desc=SPAN_TYPE_DESC[job["qtype"]])
    return GEN_PROMPT.format(snippet=snippet_for(job["rows"], job["pos"]), qtype_desc=QTYPE_DESC[job["qtype"]])


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--data-dir", type=Path, required=True, help="index dir (messages/leaves/meta)")
    ap.add_argument("--config", type=Path, required=True, help="sandbox config used to build that index")
    ap.add_argument("--foreign-data-dir", type=Path, default=None, help="other collection for unanswerable questions")
    ap.add_argument("--domain", required=True)
    ap.add_argument("--anchor", choices=("message", "span"), default="message",
                    help="message: one-line anchors (default); span: 3-6 consecutive messages needing a flow")
    ap.add_argument("--span-min-len", type=int, default=3)
    ap.add_argument("--span-max-len", type=int, default=6)
    ap.add_argument("--span-max-gap-min", type=float, default=30.0, help="max minutes between consecutive span messages")
    ap.add_argument("--copy-abstain-from", type=Path, default=None,
                    help="span mode: copy the unanswerable questions from this existing gold instead of generating")
    ap.add_argument("--secrets", type=Path, default=REPO / ".private" / "eval" / "secrets.env")
    ap.add_argument("--n-answerable", type=int, default=60)
    ap.add_argument("--n-abstain", type=int, default=15)
    ap.add_argument("--exclude-gold", type=Path, action="append", default=[])
    ap.add_argument("--min-per-room", type=int, default=3)
    ap.add_argument("--min-room-msgs", type=int, default=300)
    ap.add_argument("--oversample", type=float, default=1.6)
    ap.add_argument("--max-containment", type=float, default=0.5)
    ap.add_argument("--max-common-run", type=int, default=7)
    ap.add_argument("--model", default="deepseek/deepseek-v4.1-flash")
    ap.add_argument("--budget-usd", type=float, default=0.1)
    ap.add_argument("--seed", type=int, default=20260923)
    ap.add_argument("--workers", type=int, default=32)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    os.environ["MASAMONG_SANDBOX_DATA"] = str(args.data_dir.resolve())
    os.environ["MASAMONG_SANDBOX_CONFIG"] = str(args.config.resolve())
    from client import PlayClient  # noqa: E402
    from openrouter import load_secrets  # noqa: E402
    from rag_upgrade.index_binding import compute_index_binding  # noqa: E402

    rng = random.Random(args.seed)
    messages, leaves, meta = _load_index_dir(args.data_dir)
    cfg = json.loads(args.config.read_text(encoding="utf-8"))
    by_room = _rooms(messages)
    leaf_by_obs = _leaf_for_obs(leaves)
    exclude = _excluded_obs(args.exclude_gold)

    jobs: list[dict[str, Any]] = []
    span = args.anchor == "span"
    anchors = [] if span else _pick_anchors(by_room, args.n_answerable, rng, exclude=exclude, min_per_room=args.min_per_room,
                                            min_room_msgs=args.min_room_msgs, oversample=args.oversample)
    for i, (room, pos) in enumerate(anchors):
        jobs.append({"kind": "answerable", "room": room, "pos": pos, "rows": by_room[room], "qtype": QUESTION_TYPES[i % 3]})
    if span:
        jobs = _span_jobs(by_room, args, rng, exclude)

    foreign_jobs: list[dict[str, Any]] = []
    if args.foreign_data_dir and args.n_abstain > 0 and not span:
        f_messages, _f_leaves, _ = _load_index_dir(args.foreign_data_dir)
        f_by_room = _rooms(f_messages)
        f_anchors = _pick_anchors(f_by_room, args.n_abstain, rng, exclude=set(), min_per_room=1,
                                  min_room_msgs=args.min_room_msgs, oversample=args.oversample)
        target_rooms = sorted(r for r, v in by_room.items() if len(v) >= args.min_room_msgs)
        for i, (room, pos) in enumerate(f_anchors):
            foreign_jobs.append({"kind": "abstain", "room": room, "pos": pos, "rows": f_by_room[room],
                                 "qtype": ("fact", "person")[i % 2], "target_room": target_rooms[i % len(target_rooms)]})

    all_jobs = jobs + foreign_jobs
    prompts = [[{"role": "user", "content": _prompt_for(j)}] for j in all_jobs]
    secrets = load_secrets(args.secrets)
    client = PlayClient(secrets=secrets, budget_usd=args.budget_usd, max_workers=args.workers, llm_provider="",
                        max_request_cost_usd=0.002)
    from concurrent.futures import ThreadPoolExecutor

    stop = {"budget": False}

    def _one(msgs: list[dict[str, str]]) -> str:
        # per-job isolation: one empty/failed reply must not discard the rest of the batch
        if stop["budget"]:
            return ""
        try:
            return chat_no_reasoning(client, msgs, model=args.model, max_tokens=500 if span else 400, temperature=0.4)
        except Exception as exc:  # noqa: BLE001
            if type(exc).__name__ == "BudgetError":
                stop["budget"] = True
            return ""

    with ThreadPoolExecutor(max_workers=max(1, args.workers)) as pool:
        outs = list(pool.map(_one, prompts))
    errors = sum(1 for o in outs if not o)
    if stop["budget"]:
        print(json.dumps({"stage": "budget_stop", "completed": len(outs) - errors}), flush=True)

    leaf_rooms = {str(l["doc_id"]): str(l.get("source_room_id") or "") for l in leaves}
    room_leaf = {}
    for doc_id, room in leaf_rooms.items():
        room_leaf.setdefault(room, doc_id)
    questions: list[dict[str, Any]] = []
    reasons: dict[str, int] = defaultdict(int)
    containments: list[float] = []
    per_room_answerable: dict[str, int] = defaultdict(int)
    quota = room_quota({r: len(v) for r, v in by_room.items()}, args.n_answerable,
                       min_per_room=args.min_per_room, min_room_msgs=args.min_room_msgs)
    n_abstain = 0
    for job, text in zip(all_jobs, outs):
        anchor = job["rows"][job["pos"]]
        labels = {str(r.get("actor_label") or "") for r in job["rows"][max(0, job["pos"] - 6): job["pos"] + job.get("length", 1) + 6]}
        if span:
            members = job["rows"][job["pos"]: job["pos"] + job["length"]]
            ok, why, stats = review_span_question(parse_json_object(text), span_texts=[str(r.get("content") or "") for r in members],
                                                  speaker_labels=labels, max_containment=args.max_containment,
                                                  max_common_run=args.max_common_run)
        else:
            ok, why, stats = review_question(parse_json_object(text), anchor_text=str(anchor.get("content") or ""),
                                             speaker_labels=labels, max_containment=args.max_containment,
                                             max_common_run=args.max_common_run)
        reasons[why] += 1
        if not ok:
            continue
        obj = parse_json_object(text) or {}
        containments.append(stats["containment"])
        oid = str(anchor.get("observation_id"))
        qid_seed = f"{args.domain}|{job['kind']}|{oid}"
        qid = f"{args.domain}-{job['kind'][:3]}-{hashlib.sha256(qid_seed.encode()).hexdigest()[:10]}"
        if job["kind"] == "answerable":
            if per_room_answerable[job["room"]] >= quota.get(job["room"], 0):
                reasons["over_quota"] += 1
                continue
            if oid not in leaf_by_obs:
                reasons["anchor_not_indexed"] += 1
                continue
            if span:
                span_ids = [str(r.get("observation_id")) for r in members]
                if any(o not in leaf_by_obs for o in span_ids):
                    reasons["anchor_not_indexed"] += 1
                    continue
                per_room_answerable[job["room"]] += 1
                leaf_ids = list(dict.fromkeys(leaf_by_obs[o] for o in span_ids))
                questions.append({
                    "question_id": qid, "query": obj["question"], "question_type": job["qtype"],
                    "expected_abstention": False, "want_observation_ids": span_ids, "expected_evidence_ids": span_ids,
                    "evidence_match": "any", "span_observation_ids": span_ids, "span_len": len(span_ids),
                    "span_speakers": len({str(r.get("actor_label") or "") for r in members}),
                    "span_leaf_doc_ids": leaf_ids,
                    "source_leaf_doc_id": leaf_by_obs[oid], "reference_answer": obj.get("answer"),
                    "anchor_time": anchor.get("parsed_time"), "room": job["room"],
                    "split": "holdout", "review_status": "unreviewed", "gen_stats": stats,
                })
                continue
            per_room_answerable[job["room"]] += 1
            questions.append({
                "question_id": qid, "query": obj["question"], "question_type": job["qtype"],
                "expected_abstention": False, "want_observation_ids": [oid], "expected_evidence_ids": [oid],
                "source_leaf_doc_id": leaf_by_obs[oid], "reference_answer": obj.get("answer"),
                "anchor_time": anchor.get("parsed_time"), "room": job["room"],
                "split": "holdout", "review_status": "unreviewed", "gen_stats": stats,
            })
        else:
            if n_abstain >= args.n_abstain:
                continue
            n_abstain += 1
            questions.append({
                "question_id": qid, "query": obj["question"], "question_type": "unanswerable",
                "expected_abstention": True, "want_observation_ids": [], "expected_evidence_ids": [],
                "source_leaf_doc_id": room_leaf.get(job["target_room"], ""), "room": job["target_room"],
                "foreign_source_room": job["room"], "reference_answer": obj.get("answer"),
                "split": "holdout", "review_status": "unreviewed", "gen_stats": stats,
            })

    if span and args.copy_abstain_from:
        src = json.loads(args.copy_abstain_from.read_text(encoding="utf-8"))
        copied = [dict(q) for q in src.get("questions") or [] if q.get("expected_abstention")][: args.n_abstain]
        questions.extend(copied)
        n_abstain = len(copied)

    # bind to the index exactly as the evaluator will recompute it
    from engine import LEAF_VECS_PATH, load_config  # noqa: E402

    binding = compute_index_binding(leaves=leaves, messages=messages, leaf_vecs_path=LEAF_VECS_PATH,
                                    cfg=load_config(), meta=meta)
    gold = {
        "schema_version": 1,
        "dataset_id": f"holdout-{args.domain}-{'span-' if span else ''}20260923",
        "gold_kind": "synthetic",
        "review_status": "unreviewed",
        "index_binding": binding,
        "corpus_binding": binding.get("corpus_binding"),
        "generator": {
            "script": "tools/evaluation/rag_upgrade/build_holdout_gold.py", "model": args.model, "seed": args.seed,
            "anchor_unit": "span" if span else "message", "max_containment": args.max_containment, "max_common_run": args.max_common_run,
            "excluded_gold": [str(p) for p in args.exclude_gold], "foreign": str(args.foreign_data_dir or ""),
            **({"span_len": [args.span_min_len, args.span_max_len], "span_max_gap_min": args.span_max_gap_min,
                "abstain_copied_from": str(args.copy_abstain_from or ""), "evidence_match": "any"} if span else {}),
        },
        "questions": questions,
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(gold, ensure_ascii=False, indent=1), encoding="utf-8")
    sheet = [{"question_id": q["question_id"], "query": q["query"], "type": q["question_type"],
              "reference_answer": q.get("reference_answer"), "expected_abstention": q["expected_abstention"],
              "verdict": "", "note": ""} for q in questions]
    args.out.with_suffix(".review.json").write_text(json.dumps(sheet, ensure_ascii=False, indent=1), encoding="utf-8")
    summary = {
        "domain": args.domain, "questions": len(questions),
        "answerable": sum(1 for q in questions if not q["expected_abstention"]),
        "abstain": sum(1 for q in questions if q["expected_abstention"]),
        "rooms": len({q["room"] for q in questions if not q["expected_abstention"]}),
        "by_type": {t: sum(1 for q in questions if q["question_type"] == t) for t in (*(SPAN_TYPES if span else QUESTION_TYPES), "unanswerable")},
        "reject_reasons": dict(reasons),
        "containment_mean": round(sum(containments) / len(containments), 3) if containments else None,
        "cost_usd": round(client.usage.cost_usd, 5), "errors": errors,
        "index_fingerprint": str(binding.get("index_fingerprint") or "")[:16],
    }
    if span:
        sq = [q for q in questions if not q["expected_abstention"]]
        n = max(1, len(sq))
        summary["span"] = {
            "mean_len": round(sum(q["span_len"] for q in sq) / n, 2),
            "len_hist": {str(k): sum(1 for q in sq if q["span_len"] == k) for k in range(args.span_min_len, args.span_max_len + 1)},
            "multi_leaf_share": round(sum(1 for q in sq if len(q["span_leaf_doc_ids"]) > 1) / n, 3),
            "mean_lines_used": round(sum(q["gen_stats"].get("lines_used", 0) for q in sq) / n, 2),
            "evidence_match": "any",
        }
    print(json.dumps(summary, ensure_ascii=False))
    args.out.with_suffix(".summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=1), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
