#!/usr/bin/env python3
"""Answer-level check on holdout gold (2026-09-23 generalization audit, phase 4).

Retrieval recall says whether the right leaf ranked; users see answers. For a small stratified
sample this runs the runtime path end to end — planner intent (cached) → `retrieve` (auto ranking)
→ `_build_chat_messages` (the sandbox answer prompt) → response model — and asks a judge model:

  verdict              correct | partial | wrong | abstained   (vs. the reference answer)
  evidence_sufficient  does the retrieved evidence text actually contain the answer?

For expected-abstention questions the good outcome is `abstained`; anything else is a hallucination.
Crossing verdict × sufficiency separates retrieval failures (evidence insufficient) from generation
failures (evidence sufficient but wrong / abstained). Outputs stay in .masamong-scratch.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import sys
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
sys.path.insert(0, str(CODE))
sys.path.insert(0, str(EVAL))

JUDGE_PROMPT = """너는 채팅 기억 봇의 답변을 채점한다. 사실 여부만 본다.

질문: {question}
{truth}
<봇이 받은 근거원문 (일부)>
{evidence}
</봇이 받은 근거원문>
봇 답변: {answer}

판정:
- verdict: 기준과 맞으면 correct, 일부만 맞으면 partial, 틀리거나 근거 없는 내용을 지어냈으면 wrong,
  모른다/기록에 없다고 답했으면 abstained.
- evidence_sufficient: 위 근거원문만으로 질문에 답할 수 있으면 true.
JSON 한 개만: {{"verdict": "...", "evidence_sufficient": true, "why": "짧게"}}"""


def _truth_block(q: dict[str, Any], anchor_text: str) -> str:
    if q.get("expected_abstention"):
        return "기준: 이 질문의 내용은 이 방 기록에 없다(다른 방의 사실). 올바른 답은 모른다/없다고 하는 것."
    return f"기준 답: {q.get('reference_answer')}\n근거 메시지: {anchor_text[:400]}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--domain", required=True)
    ap.add_argument("--data-dir", type=Path, required=True)
    ap.add_argument("--config", type=Path, required=True)
    ap.add_argument("--gold", type=Path, required=True)
    ap.add_argument("--plans", type=Path, required=True)
    ap.add_argument("--config-override", default="")
    ap.add_argument("--n-answerable", type=int, default=15)
    ap.add_argument("--n-abstain", type=int, default=5)
    ap.add_argument("--ranking", default="auto")
    ap.add_argument("--answer-model", default="deepseek/deepseek-v4.1-flash")
    ap.add_argument("--judge-model", default="deepseek/deepseek-v4.1-flash")
    ap.add_argument("--budget-usd", type=float, default=0.1)
    ap.add_argument("--seed", type=int, default=20260923)
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    os.environ["MASAMONG_SANDBOX_DATA"] = str(args.data_dir.resolve())
    os.environ["MASAMONG_SANDBOX_CONFIG"] = str(args.config.resolve())
    from bot_plan import intent_from_plan  # noqa: E402
    from client import PlayClient  # noqa: E402
    from engine import _build_chat_messages, apply_vector_dims, load_config, load_index, retrieve  # noqa: E402
    from openrouter import load_secrets  # noqa: E402
    from rag_upgrade.build_holdout_gold import chat_no_reasoning, parse_json_object  # noqa: E402

    cfg = load_config()
    if args.config_override:
        from retrieval_config import _merge

        cfg = _merge(cfg, json.loads(args.config_override))
    index = load_index()
    index.cfg = cfg
    apply_vector_dims(index, int(cfg.get("vector_dims") or 0))
    gold = json.loads(args.gold.read_text(encoding="utf-8"))
    plans = json.loads(args.plans.read_text(encoding="utf-8"))
    rng = random.Random(args.seed)
    qs = [q for q in gold["questions"] if not q.get("temporal_wording")]
    ans = [q for q in qs if not q.get("expected_abstention")]
    abst = [q for q in qs if q.get("expected_abstention")]
    rng.shuffle(ans)
    rng.shuffle(abst)
    sample = ans[: args.n_answerable] + abst[: args.n_abstain]

    secrets = load_secrets(REPO / ".private" / "eval" / "secrets.env")
    client = PlayClient(secrets=secrets, budget_usd=args.budget_usd, llm_provider="", max_request_cost_usd=0.003,
                        embed_model=str((cfg.get("models") or {}).get("embed") or "qwen/qwen3-embedding-8b"))
    msg_by_obs = {str(m["observation_id"]): m for m in index.messages}
    leaf_by_obs: dict[str, str] = {}
    for leaf in index.leaves:
        for o in leaf.get("observation_ids") or []:
            leaf_by_obs.setdefault(str(o), str(leaf["doc_id"]))

    prepared: list[dict[str, Any]] = []
    for q in sample:
        plan = (plans.get(q["question_id"]) or {}).get("parsed")
        intent = intent_from_plan(plan) if plan else None
        room = str(q.get("room") or "")
        trace: dict[str, Any] = {}
        try:
            hits = retrieve(index, client, q["query"], top_k=5, mode="leaf_parent", ranking=args.ranking,
                            remaining_budget=0.05, trace=trace, source_room_id=room or None,
                            intent=dict(intent) if intent else None)
        except Exception as exc:  # noqa: BLE001
            hits, trace = [], {"error": type(exc).__name__}
        rag_state = "candidates" if hits else ("insufficient_authorized_candidates" if trace.get("abstained") else "empty_in_allowed_scope")
        messages = _build_chat_messages(index, message=q["query"], history=[], hits=hits, rag_state=rag_state,
                                        intent=dict(intent) if intent else None)
        want = {str(x) for x in q.get("want_observation_ids") or []}
        visible = any(want & {str(x) for x in (h.get("visible_observation_ids") or h.get("observation_ids") or [])} for h in hits)
        anchor = " ".join(str((msg_by_obs.get(o) or {}).get("content") or "") for o in want)
        evidence = "\n\n".join(str(h.get("excerpt") or "") for h in hits)[:3000] or "(근거 없음)"
        prepared.append({"q": q, "messages": messages, "rag_state": rag_state, "anchor": anchor, "evidence": evidence,
                         "want_visible": visible, "applied": trace.get("ranking_applied") or trace.get("applied"),
                         "abstained": bool(trace.get("abstained"))})

    def _answer(p: dict[str, Any]) -> str:
        try:
            return chat_no_reasoning(client, p["messages"], model=args.answer_model, max_tokens=300, temperature=0.2)
        except Exception as exc:  # noqa: BLE001
            return f"__error__ {type(exc).__name__}"

    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        answers = list(pool.map(_answer, prepared))

    def _judge(pa: tuple[dict[str, Any], str]) -> dict[str, Any] | None:
        p, a = pa
        prompt = JUDGE_PROMPT.format(question=p["q"]["query"], truth=_truth_block(p["q"], p["anchor"]),
                                     evidence=p["evidence"], answer=a[:800])
        try:
            raw = client.chat([{"role": "user", "content": prompt}], max_tokens=900, temperature=0.0, reasoning="low",
                              model=args.judge_model)
        except Exception as exc:  # noqa: BLE001
            return {"verdict": None, "judge_error": type(exc).__name__}
        return parse_json_object(raw) or {"verdict": None, "judge_error": "unparsable"}

    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        judged = list(pool.map(_judge, zip(prepared, answers)))

    rows = []
    for p, a, j in zip(prepared, answers, judged):
        rows.append({"question_id": p["q"]["question_id"], "type": p["q"]["question_type"],
                     "expected_abstention": bool(p["q"].get("expected_abstention")), "rag_state": p["rag_state"],
                     "applied": p["applied"], "want_visible": p["want_visible"],
                     "verdict": (j or {}).get("verdict"), "evidence_sufficient": (j or {}).get("evidence_sufficient"),
                     "why": (j or {}).get("why"), "judge_error": (j or {}).get("judge_error"), "answer": a,
                     "evidence": p["evidence"], "anchor": p["anchor"]})
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(rows, ensure_ascii=False, indent=1), encoding="utf-8")

    def _count(sub: list[dict[str, Any]], key: str) -> dict[str, int]:
        out: dict[str, int] = {}
        for r in sub:
            out[str(r[key])] = out.get(str(r[key]), 0) + 1
        return out

    answerable = [r for r in rows if not r["expected_abstention"]]
    unanswerable = [r for r in rows if r["expected_abstention"]]
    summary = {
        "domain": args.domain, "n": len(rows), "cost_usd": round(client.usage.cost_usd, 5),
        "answerable": {"n": len(answerable), "verdict": _count(answerable, "verdict"),
                       "evidence_sufficient": _count(answerable, "evidence_sufficient"),
                       "want_visible": _count(answerable, "want_visible")},
        "answerable_cross": {f"{r['evidence_sufficient']}|{r['verdict']}": 0 for r in answerable},
        "unanswerable": {"n": len(unanswerable), "verdict": _count(unanswerable, "verdict"),
                         "rag_state": _count(unanswerable, "rag_state")},
    }
    for r in answerable:
        summary["answerable_cross"][f"{r['evidence_sufficient']}|{r['verdict']}"] += 1
    args.out.with_suffix(".summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=1), encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
