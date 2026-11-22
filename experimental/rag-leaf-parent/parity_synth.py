"""Synthetic sandbox for the parity gate (`make parity`): a small made-up corpus, its artifacts and a table
of planner intents, with no stored chat, no API calls and no private data.

    python3 experimental/rag-leaf-parent/parity_synth.py <out_dir>

writes into <out_dir>:
    data/{meta,messages,leaves,summaries}.json, data/{leaf_vecs,summary_vecs}.npy   (what prepare.py makes)
    config.json                                                                    (the sandbox recipe)
    intents.json                                                                   (planner intent cases)

`parity_dump.py` reads it (MASAMONG_SANDBOX_DATA / MASAMONG_SANDBOX_CONFIG / PARITY_INTENTS) and the Go parity
tests read the same data directory. Everything is derived from fixed seeds and fixed words, so the output is the
same on every machine. The people, rooms and words are invented; never put real names or text in here.
"""
import json
import random
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from segment import slice_segments_per_room  # noqa: E402

DIMS = 32
LEAF_SIZE = 8
BURSTS_PER_ROOM = 24
KST = timezone(timedelta(hours=9))
START = datetime(2026, 1, 1, tzinfo=KST)

# room id, hour of the daily burst, whether the source gives a zone (Discord does, the Kakao export does not)
ROOMS = [
    ("discord:synthguild:room-a", 10, True),
    ("discord:synthguild:room-b", 21, True),
    ("kakao:synthroom-k", 15, False),
]
PEOPLE = ["누리", "가온", "미르", "새봄", "마루 하나", "마루 둘", "마루 셋", "마루 넷"]  # the last four share a first name: one name, too many people
TOPICS = [
    ["시장", "과일", "가격", "할인", "봉지", "저울", "상자", "계산"],
    ["기차", "역", "표", "환승", "지연", "승강장", "좌석", "노선"],
    ["영화", "극장", "예매", "자막", "상영", "관객", "줄거리", "배우"],
    ["축구", "경기", "골대", "심판", "응원", "연장전", "수비", "공격"],
    ["요리", "냄비", "양념", "불맛", "국물", "간장", "반죽", "숟가락"],
    ["여행", "숙소", "지도", "배낭", "일정", "항구", "기념품", "사진"],
]
FRAMES = ["{a}{p} {b} 얘기 좀 하자", "어제 {a} {b} 봤어", "{a}는 {b}보다 {c}가 낫지 않아", "{b} {c} 때문에 {a} 늦었어",
          "{c}{p} 어디서 샀어 {a} 말이야", "다음에 {a} {b} 같이 하자"]
PARTICLES = ["이", "가", "은", "는"]


def _unit(vec):
    return (vec / np.linalg.norm(vec)).astype(np.float32)


def _iso(dt, aware):
    return dt.isoformat() if aware else dt.replace(tzinfo=None).isoformat()


def build(out_dir):
    out = Path(out_dir)
    data = out / "data"
    data.mkdir(parents=True, exist_ok=True)
    rng = random.Random(20260101)
    np_rng = np.random.default_rng(20260101)
    centroids = [_unit(np_rng.standard_normal(DIMS)) for _ in TOPICS]

    messages, burst_topics = [], {}
    for room, hour, aware in ROOMS:
        for burst in range(BURSTS_PER_ROOM):
            topic = (burst + len(room)) % len(TOPICS)
            burst_topics[(room, burst)] = topic
            at = START + timedelta(days=burst, hours=hour)
            for k in range(LEAF_SIZE):
                words = TOPICS[topic]
                frame = rng.choice(FRAMES)
                text = frame.format(a=rng.choice(words), b=rng.choice(words), c=rng.choice(words), p=rng.choice(PARTICLES))
                n = len(messages)
                who_i = (burst + k + rng.randrange(2)) % len(PEOPLE)
                messages.append({
                    "observation_id": f"synth:{room}:{n}",
                    "source_room_id": room,
                    "source_room_original": room,
                    "actor_label": PEOPLE[who_i],
                    "actor_account": f"synth-acct-{who_i}" if aware else None,
                    "content": text,
                    "parsed_time": _iso(at + timedelta(minutes=3 * k), aware),
                })
    leaves = slice_segments_per_room(messages, size=LEAF_SIZE, overlap=0, limit=500, kind="leaf", with_prefix=True,
                                     split="fixed", gap_minutes=30)
    summaries, leaf_vecs, summary_vecs = [], [], []
    for leaf in leaves:
        first = messages[int(leaf["start"])]
        topic = burst_topics[(first["source_room_id"], (int(leaf["start"]) % (BURSTS_PER_ROOM * LEAF_SIZE)) // LEAF_SIZE)]
        leaf_vecs.append(_unit(centroids[topic] + 0.45 * np_rng.standard_normal(DIMS)))
        summary_vecs.append(_unit(leaf_vecs[-1] + 0.25 * np_rng.standard_normal(DIMS)))
        summaries.append({"doc_id": leaf["doc_id"], "summary": f"{TOPICS[topic][0]} {TOPICS[topic][1]} 이야기 요약"})

    (data / "messages.json").write_text(json.dumps(messages, ensure_ascii=False), encoding="utf-8")
    (data / "leaves.json").write_text(json.dumps(leaves, ensure_ascii=False), encoding="utf-8")
    (data / "summaries.json").write_text(json.dumps(summaries, ensure_ascii=False), encoding="utf-8")
    np.save(data / "leaf_vecs.npy", np.stack(leaf_vecs))
    np.save(data / "summary_vecs.npy", np.stack(summary_vecs))
    embed_model = "qwen/qwen3-embedding-8b"
    (data / "meta.json").write_text(json.dumps({"recipe": {"models": {"embed": embed_model}}}), encoding="utf-8")

    cfg = json.loads((HERE / "config.example.json").read_text(encoding="utf-8"))
    cfg.update({
        "experiment": "parity-synthetic", "collection": "synthetic", "rooms": [r for r, _, _ in ROOMS], "room_merge": {},
        "message_limit": len(messages), "leaf_size": LEAF_SIZE, "leaf_overlap": 0, "leaf_limit": 500,
        "summary_enabled": True, "data_dir": "", "recent_window_days": 7, "recent_window_leaves": 12,
        "person_aliases": {"누리": ["누니"]},
    })
    cfg["retrieval"]["person"]["identity"] = "both"  # the hand alias table and the account-linked labels
    cfg["models"] = dict(cfg.get("models") or {}, embed=embed_model)
    (out / "config.json").write_text(json.dumps(cfg, ensure_ascii=False, indent=2), encoding="utf-8")
    (out / "intents.json").write_text(json.dumps(intent_cases(leaves, messages), ensure_ascii=False, indent=2), encoding="utf-8")
    return len(messages), len(leaves)


def intent_cases(leaves, messages):
    """The planner-intent table: each case names a leaf (its text and stored vector are the query), the planner
    output that `bot_plan.intent_from_plan` turns into the retrieval intent, and the reference clock."""
    by_room = {}
    for leaf in leaves:
        by_room.setdefault(leaf["source_room_id"], []).append(leaf["doc_id"])
    room_a, room_b, room_k = (by_room[r][0:BURSTS_PER_ROOM] for r, _, _ in ROOMS)
    now = "2026-01-24T12:00:00+09:00"
    ref = {"mode": "replay", "now": now, "tz": "+09:00", "naive_tz": "+09:00", "source": "parity"}
    ref_no_naive = dict(ref, naive_tz=None)
    ref_naive_now = {"mode": "replay", "now": "2026-01-24T12:00:00", "tz": None, "naive_tz": None, "source": "parity"}

    def case(name, leaf, scope, plan, reference=None):
        return {"name": name, "leaf": leaf, "scope": scope, "plan": plan, "reference": reference}

    def when(kind, frm=None, to=None):
        return {"time_scope": None, "question_kind": "other", "time": {"kind": kind, "from": frm, "to": to}}

    def who(*targets, kind="person_activity", **extra):
        return {"question_kind": kind, "person_targets": list(targets), **extra}

    mid = len(room_a) // 2
    return [
        # recent (soft window)
        case("recent-all", room_a[-1], "all", {"time_scope": "recent", "question_kind": "other"}),
        case("recent-room", room_b[-2], "room", {"time_scope": "recent", "question_kind": "other"}),
        case("recent-kakao-room", room_k[-1], "room", {"time_scope": "recent", "question_kind": "other"}),
        case("recent-kind-in-time", room_a[-3], "all", {"time_scope": None, "question_kind": "other", "time": {"kind": "recent", "from": None, "to": None}}),
        # absolute bounds, with and without a reference clock
        case("between-dates-ref", room_a[mid], "all", when("between", "2026-01-08", "2026-01-12"), ref),
        case("between-dates-noref", room_a[mid], "all", when("between", "2026-01-08", "2026-01-12")),
        case("between-aware-datetimes", room_b[mid], "all", when("between", "2026-01-08T00:00:00+09:00", "2026-01-12T00:00:00+09:00"), ref),
        case("between-reversed", room_a[mid], "all", when("between", "2026-01-12", "2026-01-08"), ref),
        case("between-room-kakao", room_k[mid], "room", when("between", "2026-01-08", "2026-01-12"), ref),
        case("between-room-kakao-no-naive-zone", room_k[mid], "room", when("between", "2026-01-08", "2026-01-12"), ref_no_naive),
        case("between-frames-differ", room_a[mid], "all", when("between", "2026-01-08", "2026-01-12T00:00:00+09:00"), ref),
        case("between-missing-bound", room_a[mid], "all", when("between", "2026-01-08", None), ref),
        case("before-date", room_a[mid], "all", when("before", None, "2026-01-06"), ref),
        case("before-aware", room_b[mid], "all", when("before", None, "2026-01-06T12:00:00+09:00"), ref),
        case("after-date", room_a[mid], "all", when("after", "2026-01-20", None), ref),
        case("after-aware-noref", room_b[mid], "all", when("after", "2026-01-20T00:00:00+09:00", None)),
        case("after-bad-bound", room_a[mid], "all", when("after", "not a date", None), ref),
        # relative days need the reference clock
        case("today-ref", room_a[-1], "all", when("today"), ref),
        case("yesterday-ref", room_a[-2], "all", when("yesterday"), ref),
        case("last-week-ref", room_b[-3], "all", when("last_week"), ref),
        case("this-month-ref", room_a[mid], "all", when("this_month"), ref),
        case("last-week-room-kakao", room_k[-3], "room", when("last_week"), ref),
        case("last-week-room-kakao-no-naive-zone", room_k[-3], "room", when("last_week"), ref_no_naive),
        case("last-week-noref", room_a[-3], "all", when("last_week")),
        case("last-week-naive-now", room_k[-3], "room", when("last_week"), ref_naive_now),
        case("unknown-kind", room_a[mid], "all", when("someday"), ref),
        case("any-kind", room_a[mid], "all", when("any"), ref),
        # people
        case("person-one", room_a[3], "all", who("누리")),
        case("person-two", room_a[4], "all", who("가온", "미르")),
        case("person-alias", room_b[3], "all", who("누니")),
        case("person-room", room_b[5], "room", who("새봄")),
        case("person-ambiguous", room_a[5], "all", who("마루")),
        case("person-unknown", room_a[6], "all", who("없는사람", unresolved=["없는사람"])),
        case("person-and-unknown", room_a[7], "all", who("누리", "없는사람", unresolved=["없는사람"])),
        case("person-intro", room_a[8], "all", who("미르", kind="person_intro")),
        case("person-kakao", room_k[3], "room", who("누리")),
        # people and time together
        case("person-between", room_a[mid], "all", {**who("누리"), **when("between", "2026-01-05", "2026-01-15")}, ref),
        case("person-recent", room_a[-4], "all", who("가온", time_scope="recent")),
        case("person-last-week", room_b[-4], "all", {**who("미르"), **when("last_week")}, ref),
        # other planner fields
        case("sensitive", room_a[9], "all", {"question_kind": "other", "sensitive": True}),
        case("key-terms-only", room_b[9], "all", {"question_kind": "other", "key_terms": ["영화", "극장"]}),
    ]


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: parity_synth.py <out_dir>")
    n_messages, n_leaves = build(sys.argv[1])
    print(f"synthetic sandbox: {n_messages} messages, {n_leaves} leaves -> {sys.argv[1]}")
