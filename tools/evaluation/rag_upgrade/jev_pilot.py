"""Jev evaluation: synthetic Korean fixtures → hold / go_shadow.

Tests inject urlopen. Live runs read only TYPESAFE_* / JEV_MODEL from secrets.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
import time
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Mapping, Sequence

HERE = Path(__file__).resolve().parent
if str(HERE.parent) not in sys.path:
    sys.path.insert(0, str(HERE.parent))

from rag_upgrade.jev_client import (  # noqa: E402
    DEFAULT_MAX_REQUESTS,
    HARD_REQUEST_CAP,
    JevBudgetError,
    JevClient,
    JevConfigError,
    read_jev_secrets,
)

JOIN_REASONS = ("unresolved_question", "fact_conflict", "implicit_calc", "linked_memory", "none")
MEMORY_REQUIREMENTS = ("none", "optional", "required")
QUESTION_KINDS = ("person_intro", "person_fact", "event", "casual", "other")
TIME_KINDS = ("any", "recent", "today", "yesterday", "last_week", "this_month", "before", "after", "between")

JUDGE_HOLD = {
    "private_or_sensitive_recall_min": 0.85,
    "join_reason_accuracy_min": 0.70,
    "join_reason_none_recall_min": 0.80,
    "open_question_auroc_min": 0.80,
}
PLANNER_HOLD = {
    "memory_requirement_accuracy_min": 0.80,
    "required_to_none_max": 0.10,
    "sensitive_recall_min": 0.85,
}


def judge_questions(*, lang: str, n_hits: int) -> dict[str, dict[str, Any]]:
    ko = lang.startswith("ko")
    q: dict[str, dict[str, Any]] = {
        "open_question": {
            "type": "noul",
            "instructions": "마지막 사람 메시지가 아직 답을 못 받은 정보 질문인가" if ko else "Is the last human message an unanswered information question?",
        },
        "private_or_sensitive": {
            "type": "noul",
            "instructions": "이 대화가 사적·감정적·다툼·민감한 농담이어서 봇이 침묵해야 하는가" if ko else "Is this private, emotional, conflict, or sensitive so the bot should stay silent?",
        },
        "interrupts_flow": {
            "type": "noul",
            "instructions": "봇이 지금 한마디 하면 대화 흐름을 끊는가" if ko else "Would the bot speaking now interrupt the flow?",
        },
        "join_reason": {
            "type": "choice",
            "instructions": "참여 이유. none이면 끼어들지 않는다." if ko else "Why join? Use none to stay silent.",
            "criteria": {
                "unresolved_question": "답이 없는 정보 질문" if ko else "Unanswered information question",
                "fact_conflict": "명백한 사실 오류" if ko else "Clear factual conflict",
                "implicit_calc": "계산·n빵 등" if ko else "Arithmetic / split-the-bill",
                "linked_memory": "허용된 기억이 직접 답함" if ko else "Allowed memory hit answers it",
                "none": "침묵" if ko else "Stay silent",
            },
        },
        "participation_value": {
            "type": "score",
            "instructions": "참여 가치" if ko else "Join value",
            "criteria": ["침묵", "반응만", "짧은 정보", "명확히 도움"] if ko else ["silence", "react only", "short fact", "clearly helpful"],
        },
    }
    for i in range(n_hits):
        q[f"memory_hit_relevant_{i}"] = {
            "type": "noul",
            "instructions": f"allowed_memory_hits[{i}]가 현재 화제와 직접 관련 있는가" if ko else f"Is memory hit {i} directly on-topic?",
        }
    return q


def planner_questions(*, lang: str) -> dict[str, dict[str, Any]]:
    ko = lang.startswith("ko")
    return {
        "memory_requirement": {
            "type": "choice",
            "instructions": "채널 과거 기억이 필요한가" if ko else "Does this need channel memory?",
            "criteria": {
                "none": "잡담·도구만으로 답함" if ko else "Casual / tools only",
                "optional": "있으면 좋지만 필수는 아님" if ko else "Nice to have",
                "required": "기억 없이 답할 수 없음" if ko else "Cannot answer without memory",
            },
        },
        "question_kind": {
            "type": "choice",
            "instructions": "질문 종류" if ko else "Question kind",
            "criteria": {k: k for k in QUESTION_KINDS},
        },
        "sensitive": {
            "type": "noul",
            "instructions": "정치·종교·건강·연애·돈처럼 근거 없이 추측하면 안 되는가" if ko else "Is this a sensitive topic?",
        },
        "time_kind": {
            "type": "choice",
            "instructions": "시간 제약 종류" if ko else "Time constraint kind",
            "criteria": {k: k for k in TIME_KINDS},
        },
    }


def _noul(answers: Mapping[str, Any], name: str) -> float | None:
    row = answers.get(name) or {}
    val = row.get("noul")
    return float(val) if isinstance(val, (int, float)) and not isinstance(val, bool) else None


def _choice(answers: Mapping[str, Any], name: str) -> str | None:
    row = answers.get(name) or {}
    val = row.get("choice")
    return str(val) if isinstance(val, str) else None


def _score(answers: Mapping[str, Any], name: str) -> float | None:
    row = answers.get(name) or {}
    val = row.get("score")
    return float(val) if isinstance(val, (int, float)) and not isinstance(val, bool) else None


def auroc(labels: Sequence[bool], scores: Sequence[float]) -> float | None:
    pairs = [(float(s), 1 if y else 0) for s, y in zip(scores, labels) if s is not None]
    pos = [s for s, y in pairs if y == 1]
    neg = [s for s, y in pairs if y == 0]
    if not pos or not neg:
        return None
    correct = sum(1.0 for p in pos for n in neg if p > n) + 0.5 * sum(1.0 for p in pos for n in neg if p == n)
    return correct / (len(pos) * len(neg))


def binary_prf(labels: Sequence[bool], preds: Sequence[bool]) -> dict[str, float]:
    tp = sum(1 for y, p in zip(labels, preds) if y and p)
    fp = sum(1 for y, p in zip(labels, preds) if (not y) and p)
    fn = sum(1 for y, p in zip(labels, preds) if y and (not p))
    tn = sum(1 for y, p in zip(labels, preds) if (not y) and (not p))
    acc = (tp + tn) / max(1, len(labels))
    prec = tp / max(1, tp + fp)
    rec = tp / max(1, tp + fn)
    return {"accuracy": acc, "precision": prec, "recall": rec, "tp": tp, "fp": fp, "fn": fn, "tn": tn}


def reliability_table(labels: Sequence[bool], scores: Sequence[float], bins: int = 10) -> list[dict[str, Any]]:
    rows = []
    for b in range(bins):
        lo, hi = b / bins, (b + 1) / bins
        bucket = [(s, y) for s, y in zip(scores, labels) if s is not None and lo <= s < hi or (b == bins - 1 and s == 1.0)]
        if not bucket:
            rows.append({"bin": b, "lo": lo, "hi": hi, "n": 0, "mean_score": None, "mean_label": None})
            continue
        mean_s = sum(s for s, _ in bucket) / len(bucket)
        mean_y = sum(y for _, y in bucket) / len(bucket)
        rows.append({"bin": b, "lo": lo, "hi": hi, "n": len(bucket), "mean_score": mean_s, "mean_label": mean_y})
    return rows


def ece(table: Sequence[Mapping[str, Any]], n: int) -> float | None:
    if n <= 0:
        return None
    total = 0.0
    for row in table:
        if not row["n"] or row["mean_score"] is None:
            continue
        total += row["n"] * abs(float(row["mean_score"]) - float(row["mean_label"]))
    return total / n


def load_fixture(path: str | Path) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def _judge_state(case: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "recent_messages": case.get("recent_messages") or [],
        "allowed_memory_hits": case.get("memory_hits") or [],
    }


def _planner_state(case: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "message": case.get("message"),
        "previous_user_message": case.get("previous_user_message"),
    }


def run_pilot(
    fixture: Mapping[str, Any],
    client: JevClient | None,
    *,
    lang: str = "ko",
    dry_run: bool = False,
) -> dict[str, Any]:
    judge_cases = list(fixture.get("judge_cases") or [])
    planner_cases = list(fixture.get("planner_cases") or [])
    judge_rows: list[dict[str, Any]] = []
    planner_rows: list[dict[str, Any]] = []
    budget_exhausted = False
    errors = 0

    def decide(state, questions):
        nonlocal budget_exhausted, errors
        if dry_run or client is None:
            return None
        try:
            return client.decide(state, questions)
        except JevBudgetError:
            budget_exhausted = True
            return None
        except Exception:
            errors += 1
            return None

    for case in judge_cases:
        if budget_exhausted:
            break
        hits = list(case.get("memory_hits") or [])
        resp = decide(_judge_state(case), judge_questions(lang=lang, n_hits=len(hits)))
        labels = case.get("labels") or {}
        row: dict[str, Any] = {"id": case.get("id"), "labels": labels}
        if resp is not None:
            row["pred"] = {
                "open_question": _noul(resp.answers, "open_question"),
                "private_or_sensitive": _noul(resp.answers, "private_or_sensitive"),
                "interrupts_flow": _noul(resp.answers, "interrupts_flow"),
                "join_reason": _choice(resp.answers, "join_reason"),
                "participation_value": _score(resp.answers, "participation_value"),
                "join_confidence": (resp.answers.get("join_reason") or {}).get("confidence"),
            }
        judge_rows.append(row)

    for case in planner_cases:
        if budget_exhausted:
            break
        resp = decide(_planner_state(case), planner_questions(lang=lang))
        labels = case.get("labels") or {}
        row = {"id": case.get("id"), "labels": labels}
        if resp is not None:
            row["pred"] = {
                "memory_requirement": _choice(resp.answers, "memory_requirement"),
                "question_kind": _choice(resp.answers, "question_kind"),
                "sensitive": _noul(resp.answers, "sensitive"),
                "time_kind": _choice(resp.answers, "time_kind"),
            }
        planner_rows.append(row)

    metrics = {
        "judge": _judge_metrics(judge_rows),
        "planner": _planner_metrics(planner_rows),
        "usage": None if client is None else client.usage.as_dict(),
        "budget_exhausted": budget_exhausted,
        "errors": errors,
        "lang": lang,
        "dry_run": dry_run,
        "n_judge": len(judge_cases),
        "n_planner": len(planner_cases),
    }
    metrics["verdict"] = verdict(metrics)
    return metrics


def _judge_metrics(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    scored = [r for r in rows if r.get("pred")]
    if not scored:
        return {"n": len(rows), "scored": 0}
    y_open = [bool(r["labels"].get("open_question")) for r in scored]
    s_open = [r["pred"]["open_question"] for r in scored]
    y_priv = [bool(r["labels"].get("private_or_sensitive")) for r in scored]
    s_priv = [r["pred"]["private_or_sensitive"] for r in scored]
    p_priv = [None if s is None else s >= 0.5 for s in s_priv]
    join_y = [r["labels"].get("join_reason") for r in scored]
    join_p = [r["pred"]["join_reason"] for r in scored]
    acc = sum(a == b for a, b in zip(join_y, join_p) if b is not None) / max(1, sum(1 for b in join_p if b is not None))
    none_y = [j == "none" for j in join_y]
    none_p = [p == "none" for p in join_p]
    conf = [r["pred"].get("join_confidence") for r in scored]
    numbered = [(c if isinstance(c, (int, float)) else 0.0, int(y == p)) for c, y, p in zip(conf, join_y, join_p) if p is not None]
    numbered.sort()
    half = max(1, len(numbered) // 2)
    low = numbered[:half]
    high = numbered[half:] or numbered
    low_acc = sum(v for _, v in low) / max(1, len(low))
    high_acc = sum(v for _, v in high) / max(1, len(high))
    rel = reliability_table(y_priv, [s or 0.0 for s in s_priv])
    return {
        "n": len(rows),
        "scored": len(scored),
        "open_question": {"auroc": auroc(y_open, [s or 0.0 for s in s_open]), "prf": binary_prf(y_open, [bool(s is not None and s >= 0.5) for s in s_open])},
        "private_or_sensitive": {**binary_prf(y_priv, [bool(p) for p in p_priv]), "auroc": auroc(y_priv, [s or 0.0 for s in s_priv]), "reliability": rel, "ece": ece(rel, len(scored))},
        "join_reason": {
            "accuracy": acc,
            "none_recall": binary_prf(none_y, none_p)["recall"],
            "confusion": dict(Counter(f"{y}->{p}" for y, p in zip(join_y, join_p))),
            "high_minus_low_acc": high_acc - low_acc,
        },
    }


def _planner_metrics(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    scored = [r for r in rows if r.get("pred")]
    if not scored:
        return {"n": len(rows), "scored": 0}
    mem_y = [r["labels"].get("memory_requirement") for r in scored]
    mem_p = [r["pred"]["memory_requirement"] for r in scored]
    acc = sum(a == b for a, b in zip(mem_y, mem_p) if b is not None) / max(1, sum(1 for b in mem_p if b is not None))
    req = sum(1 for y in mem_y if y == "required")
    req_none = sum(1 for y, p in zip(mem_y, mem_p) if y == "required" and p == "none")
    y_s = [bool(r["labels"].get("sensitive")) for r in scored]
    p_s = [bool((r["pred"]["sensitive"] or 0) >= 0.5) for r in scored]
    return {
        "n": len(rows),
        "scored": len(scored),
        "memory_requirement_accuracy": acc,
        "required_to_none_rate": (req_none / req) if req else 0.0,
        "sensitive": binary_prf(y_s, p_s),
    }


def verdict(metrics: Mapping[str, Any]) -> dict[str, Any]:
    judge = metrics.get("judge") or {}
    planner = metrics.get("planner") or {}
    judge_hold = []
    planner_hold = []
    if judge.get("scored"):
        priv = (judge.get("private_or_sensitive") or {}).get("recall")
        if priv is None or priv < JUDGE_HOLD["private_or_sensitive_recall_min"]:
            judge_hold.append("private_or_sensitive_recall")
        jr = judge.get("join_reason") or {}
        if float(jr.get("accuracy") or 0) < JUDGE_HOLD["join_reason_accuracy_min"]:
            judge_hold.append("join_reason_accuracy")
        if float(jr.get("none_recall") or 0) < JUDGE_HOLD["join_reason_none_recall_min"]:
            judge_hold.append("join_reason_none_recall")
        auroc_open = ((judge.get("open_question") or {}).get("auroc"))
        if auroc_open is None or auroc_open < JUDGE_HOLD["open_question_auroc_min"]:
            judge_hold.append("open_question_auroc")
        if float(jr.get("high_minus_low_acc") or 0) < 0:
            judge_hold.append("calibration_inverted")
    else:
        judge_hold.append("not_scored")
    if planner.get("scored"):
        if float(planner.get("memory_requirement_accuracy") or 0) < PLANNER_HOLD["memory_requirement_accuracy_min"]:
            planner_hold.append("memory_requirement_accuracy")
        if float(planner.get("required_to_none_rate") or 0) > PLANNER_HOLD["required_to_none_max"]:
            planner_hold.append("required_to_none")
        rec = ((planner.get("sensitive") or {}).get("recall"))
        if rec is None or rec < PLANNER_HOLD["sensitive_recall_min"]:
            planner_hold.append("sensitive_recall")
    else:
        planner_hold.append("not_scored")
    judge_v = "hold" if judge_hold else "go_shadow"
    planner_v = "hold" if planner_hold else "go_shadow"
    overall = "go_shadow" if (judge_v == "go_shadow" or planner_v == "go_shadow") and not (judge_v == "hold" and planner_v == "hold") else "hold"
    if judge_v == "go_shadow" and planner_v == "go_shadow":
        overall = "go_shadow"
    elif judge_v == "hold" and planner_v == "hold":
        overall = "hold"
    else:
        overall = "partial_go_shadow"
    return {"overall": overall, "judge": judge_v, "planner": planner_v, "judge_reasons": judge_hold, "planner_reasons": planner_hold}


def _redact(obj: Any) -> Any:
    text = json.dumps(obj, ensure_ascii=False)
    for needle in ("apikey_", "Bearer ", "TYPESAFE_API_KEY"):
        if needle.lower() in text.lower():
            raise RuntimeError("refusing to write secrets into pilot output")
    return obj


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--fixture", default=str(HERE / "fixtures" / "jev_pilot_ko.json"))
    p.add_argument("--secrets")
    p.add_argument("--max-requests", type=int, default=DEFAULT_MAX_REQUESTS)
    p.add_argument("--out")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--lang", default="ko", choices=("ko", "en"))
    args = p.parse_args(argv)
    if args.max_requests > HARD_REQUEST_CAP:
        raise SystemExit("max-requests exceeds hard cap")
    fixture = load_fixture(args.fixture)
    client = None
    if not args.dry_run:
        if not args.secrets:
            raise SystemExit("secrets path required unless --dry-run")
        secrets = read_jev_secrets(args.secrets)
        client = JevClient(secrets, max_requests=args.max_requests)
    metrics = run_pilot(fixture, client, lang=args.lang, dry_run=args.dry_run)
    payload = _redact({"metrics": metrics, "fixture_version": fixture.get("version"), "label_status": fixture.get("label_status")})
    printed = json.dumps(payload, ensure_ascii=False, indent=2)
    if "apikey_" in printed.lower():
        raise SystemExit("refusing to print secrets")
    print(printed)
    if args.out:
        dest = Path(args.out)
        dest.mkdir(parents=True, exist_ok=True)
        (dest / "metrics.json").write_text(printed, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
