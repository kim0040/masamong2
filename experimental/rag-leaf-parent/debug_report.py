#!/usr/bin/env python3
"""샌드박스 대화 로그를 읽기 쉬운 보고서로 묶는다 (사람·에이전트 공용).

`test-sandbox/data*/debug_turns.jsonl`의 턴·오류·사용자 판정(flag)·브라우저 오류를 debug_id로 합쳐
턴마다 질문 → 답 → 판정 → 진단(검색 경로·의도·시간 창·인물·점수 하한·경고) → 근거 → 단계별 시간을 보여 준다.
옛 기록에 `diagnosis`가 없으면 `rank_trace`에서 다시 만든다. 결과는 표준 출력으로만 낸다(파일 저장은 --out).

    python3 debug_report.py                      # 가장 최근에 켠 서버(run)의 턴 전부
    python3 debug_report.py --run all --last 20  # 모든 run의 최근 20턴
    python3 debug_report.py --only bad           # 사용자가 틀림으로 표시한 턴만
    python3 debug_report.py --since 2h --format json
    python3 debug_report.py --id <debug_id> --full

대화 본문이 들어 있으므로 결과를 커밋하거나 공개 문서에 붙이지 않는다.
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
import time
from collections import Counter
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from sandbox_paths import repo_root  # noqa: E402
from turn_diagnosis import diagnosis_from_record  # noqa: E402

STAGES = (
    ("plan_ms", "계획"),
    ("tools_ms", "도구"),
    ("embed_ms", "임베딩"),
    ("hybrid_ms", "검색"),
    ("rerank_ms", "재순위"),
    ("expand_ms", "원문"),
    ("generate_ms", "작성"),
)


def default_logs() -> list[Path]:
    root = repo_root() / "test-sandbox"
    return sorted(p for p in root.glob("data*/debug_turns.jsonl") if p.is_file())


def read_rows(paths: Iterable[Path]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for path in paths:
        dataset_dir = path.parent.name
        with path.open(encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(row, dict):
                    row.setdefault("_data_dir", dataset_dir)
                    rows.append(row)
    rows.sort(key=lambda r: float(r.get("ts") or 0))
    return rows


def parse_since(text: str) -> float | None:
    text = (text or "").strip()
    if not text:
        return None
    m = re.fullmatch(r"(\d+(?:\.\d+)?)\s*([smhd])", text)
    if m:
        mult = {"s": 1, "m": 60, "h": 3600, "d": 86400}[m.group(2)]
        return time.time() - float(m.group(1)) * mult
    return datetime.fromisoformat(text).timestamp()


def run_id(row: dict[str, Any]) -> str:
    run = row.get("run") if isinstance(row.get("run"), dict) else {}
    return str(run.get("server_run_id") or "")


def diagnosis_of(row: dict[str, Any]) -> dict[str, Any]:
    return diagnosis_from_record(row)


def select(rows: list[dict[str, Any]], args: argparse.Namespace) -> tuple[list[dict[str, Any]], dict[str, list[dict[str, Any]]], str]:
    flags: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        if row.get("kind") == "flag":
            flags.setdefault(str(row.get("debug_id") or ""), []).append(row)
    events = [r for r in rows if r.get("kind") in ("turn", "error", "client_error")]
    since = parse_since(args.since)
    if since is not None:
        events = [r for r in events if float(r.get("ts") or 0) >= since]
    chosen_run = ""
    if args.id:
        events = [r for r in events if str(r.get("debug_id") or "") == args.id]
    elif args.run == "latest":
        runs = [run_id(r) for r in events if run_id(r)]
        chosen_run = runs[-1] if runs else ""
        if chosen_run:
            first_ts = min(float(r.get("ts") or 0) for r in events if run_id(r) == chosen_run)
            # client_error rows carry no run id: keep those that happened after this run started.
            events = [
                r for r in events
                if run_id(r) == chosen_run or (r.get("kind") == "client_error" and float(r.get("ts") or 0) >= first_ts)
            ]
    elif args.run != "all":
        chosen_run = args.run
        events = [r for r in events if run_id(r) == args.run]
    if args.dataset:
        events = [r for r in events if args.dataset in (str(r.get("_data_dir")), str((r.get("run") or {}).get("dataset")))]

    def verdict(r: dict[str, Any]) -> bool | None:
        rows_ = flags.get(str(r.get("debug_id") or ""))
        return rows_[-1].get("ok") if rows_ else None

    if args.only == "bad":
        events = [r for r in events if verdict(r) is False]
    elif args.only == "flagged":
        events = [r for r in events if str(r.get("debug_id") or "") in flags]
    elif args.only == "warn":
        events = [r for r in events if r.get("kind") == "turn" and diagnosis_of(r).get("warnings")]
    elif args.only == "error":
        events = [r for r in events if r.get("kind") != "turn"]
    if args.last > 0:
        events = events[-args.last :]
    return events, flags, chosen_run


def summarize_run(paths: Iterable[Path], run: str) -> dict[str, Any]:
    """한 서버 실행(run)의 턴·판정·경고·오류 수. 화면의 '테스트 기록'이 새로고침 뒤에도 같은 숫자를 보이게 한다."""

    rows = read_rows(paths)
    turns = [r for r in rows if r.get("kind") in ("turn", "error") and run_id(r) == run]
    ids = {str(r.get("debug_id") or "") for r in turns}
    last_flag: dict[str, dict[str, Any]] = {}
    for r in rows:
        if r.get("kind") == "flag" and str(r.get("debug_id") or "") in ids:
            last_flag[str(r.get("debug_id"))] = r
    ok = sum(1 for f in last_flag.values() if f.get("ok") is True)
    bad = sum(1 for f in last_flag.values() if f.get("ok") is False)
    ok_turns = [r for r in turns if r.get("kind") == "turn"]
    return {
        "server_run_id": run,
        "turns": len(ok_turns),
        "errors": len(turns) - len(ok_turns),
        "ok": ok,
        "bad": bad,
        "unjudged": max(0, len(ok_turns) - ok - bad),
        "warned": sum(1 for r in ok_turns if diagnosis_of(r).get("warnings")),
        "spent_usd": round(sum(float(r.get("spent_usd") or 0) for r in ok_turns), 6),
    }


def _clock(ts: Any) -> str:
    try:
        return datetime.fromtimestamp(float(ts)).strftime("%m-%d %H:%M:%S")
    except (TypeError, ValueError, OSError):
        return "?"


def _clip(text: Any, n: int) -> str:
    s = " ".join(str(text or "").split())
    return s if len(s) <= n else s[: n - 1] + "…"


def _sec(ms: Any) -> str:
    try:
        return f"{float(ms) / 1000:.1f}"
    except (TypeError, ValueError):
        return "?"


def timing_line(timing: dict[str, Any], diag: dict[str, Any]) -> str:
    parts = [f"{label} {_sec(timing[key])}" for key, label in STAGES if isinstance(timing.get(key), (int, float)) and timing[key] >= 5]
    total = timing.get("total_ms")
    emb = diag.get("embed") or {}
    tail = []
    if emb.get("source"):
        tail.append(f"임베딩 {emb['source']}{' 헤지' if emb.get('hedged') else ''}")
    if emb.get("q1_source"):
        tail.append(f"q1 {emb['q1_source']}")
    return " · ".join(parts) + (f" = {_sec(total)}초" if total else "") + (f" ({', '.join(tail)})" if tail else "")


def verdict_label(flag_rows: list[dict[str, Any]] | None) -> str:
    if not flag_rows:
        return "미판정"
    last = flag_rows[-1]
    mark = {True: "맞음", False: "틀림", None: "메모"}[last.get("ok") if last.get("ok") in (True, False) else None]
    tags = ",".join(last.get("tags") or [])
    return f"{mark}{' [' + tags + ']' if tags else ''}"


def render_turn_md(i: int, row: dict[str, Any], flag_rows: list[dict[str, Any]] | None, full: bool) -> list[str]:
    run = row.get("run") if isinstance(row.get("run"), dict) else {}
    head = f"### {i}. [{verdict_label(flag_rows)}] {_clock(row.get('ts'))} · {run.get('dataset') or row.get('_data_dir')} · debug `{row.get('debug_id')}`"
    if row.get("kind") == "client_error":
        return [f"### {i}. [브라우저 오류] {_clock(row.get('ts'))} · {row.get('where') or ''}",
                f"- {row.get('type') or ''}: {_clip(row.get('message'), 400)}", ""]
    out = [head]
    if run.get("conversation_id"):
        out[-1] += f" · 대화 `{str(run['conversation_id'])[:8]}`"
    out.append(f"- **Q:** {_clip(row.get('query'), 2000 if full else 300)}")
    if row.get("kind") == "error":
        out.append(f"- **오류:** {row.get('error_type')}: {_clip(row.get('error'), 600)}")
        if row.get("partial_chars"):
            out.append(f"- 중단 전 생성 {row.get('partial_chars')}자")
    else:
        out.append(f"- **A:** {_clip(row.get('answer'), 4000 if full else 600)}")
    for f in flag_rows or []:
        if f.get("note") or f.get("tags"):
            out.append(f"- **판정 메모:** {verdict_label([f])} — {_clip(f.get('note'), 800)}")
    diag = diagnosis_of(row)
    rk = diag.get("ranking") or {}
    out.append(
        f"- 검색: {'실행' if diag.get('rag_used') else '안 함'} · {rk.get('requested')}→{rk.get('applied')}"
        f" ({rk.get('auto_reason') or '-'}, 확신 {rk.get('confidence') or '-'}{', 복구' if rk.get('recovery') else ''})"
        f" · rag_state={diag.get('rag_state')} · memory={diag.get('memory_requirement')}"
    )
    it = diag.get("intent") or {}
    q = diag.get("queries") or {}
    out.append(
        f"- 의도: source={it.get('source')} kind={it.get('kind')} targets={it.get('targets') or []}"
        f" q1={_clip(q.get('q1'), 80) or '-'} terms={it.get('key_terms') or []}"
    )
    tm = diag.get("time") or {}
    if tm.get("planner_kind") not in (None, "", "none", "any") or tm.get("applied"):
        out.append(
            f"- 시간: 플래너 {tm.get('planner_kind')} {tm.get('planner_from') or ''}~{tm.get('planner_to') or ''}"
            f" → 창 {'적용' if tm.get('applied') else '없음'}{' (hard)' if tm.get('hard') else ''}"
            f" {tm.get('from') or ''}~{tm.get('to') or ''} 후보 {tm.get('pool_n')}"
        )
    ps = diag.get("persons")
    if ps:
        out.append(
            f"- 인물: {ps.get('targets')} → 화자 {ps.get('matched_speakers')} · 모호 {ps.get('ambiguous')} · 미해결 {ps.get('unresolved')}"
            f" · 계정 연결 {ps.get('account_linked_n')}/표시명만 {ps.get('label_only_n')}"
        )
    rr = diag.get("rerank") or {}
    if rr.get("performed"):
        tops = " ".join(f"{t['score']:.2f}{'✓' if t.get('above_floor') else '✗'}" for t in rr.get("top") or [])
        out.append(f"- 재순위: 하한 {rr.get('floor')} · 남김 {rr.get('kept_n')} · 상위 {tops}")
    cand = diag.get("candidates") or {}
    if cand.get("union_n"):
        out.append(f"- 후보: 합집합 {cand.get('union_n')} · 풀 {cand.get('pool_n')} · {', '.join(cand.get('retrievers') or [])}")
    for w in diag.get("warnings") or []:
        detail = {k: v for k, v in w.items() if k not in ("code", "label")}
        out.append(f"- ⚠ `{w.get('code')}` {w.get('label')}{' ' + json.dumps(detail, ensure_ascii=False) if detail else ''}")
    hits = row.get("hits") or []
    score_by = {h.get("doc_id"): h.get("score") for h in diag.get("hits") or []}
    for h in hits:
        score = score_by.get(h.get("doc_id"))
        out.append(
            f"  - 근거 #{h.get('rank')} {h.get('date') or ''} · {h.get('source_room_id') or ''}"
            f" · {', '.join((h.get('speakers') or [])[:6])}{f' · {score:.2f}' if isinstance(score, (int, float)) else ''}"
            f" — {_clip(h.get('excerpt'), 1200 if full else 200)}"
        )
    for t in diag.get("tools") or []:
        out.append(f"- 도구: {t.get('tool')} ok={t.get('ok')} {t.get('status') or ''}")
    line = timing_line(row.get("timing") or {}, diag)
    if line:
        out.append(f"- 시간(초): {line}")
    if row.get("spent_usd") is not None:
        out.append(f"- 비용: ${float(row.get('spent_usd') or 0):.4f}")
    out.append("")
    return out


def summary_md(events: list[dict[str, Any]], flags: dict[str, list[dict[str, Any]]], chosen_run: str) -> list[str]:
    turns = [r for r in events if r.get("kind") == "turn"]
    errors = [r for r in events if r.get("kind") == "error"]
    client = [r for r in events if r.get("kind") == "client_error"]
    verdicts = Counter()
    tags = Counter()
    for r in turns + errors:
        rows_ = flags.get(str(r.get("debug_id") or ""))
        if not rows_:
            verdicts["미판정"] += 1
            continue
        last = rows_[-1]
        verdicts[{True: "맞음", False: "틀림"}.get(last.get("ok"), "메모")] += 1
        tags.update(last.get("tags") or [])
    warn = Counter()
    for r in turns:
        warn.update(w.get("code") for w in diagnosis_of(r).get("warnings") or [])
    datasets = Counter(str((r.get("run") or {}).get("dataset") or r.get("_data_dir")) for r in turns + errors)
    lines = [f"# 샌드박스 테스트 로그{' — ' + chosen_run if chosen_run else ''}", ""]
    lines.append(f"- 턴 {len(turns)} · 서버 오류 {len(errors)} · 브라우저 오류 {len(client)} · 데이터셋 {dict(datasets)}")
    lines.append(f"- 판정: {dict(verdicts)}" + (f" · 태그 {dict(tags)}" if tags else ""))
    if warn:
        lines.append(f"- 경고: {dict(warn.most_common())}")
    totals = [float((r.get("timing") or {}).get("total_ms") or 0) / 1000 for r in turns if (r.get("timing") or {}).get("total_ms")]
    if totals:
        q = sorted(totals)
        lines.append(f"- 턴 시간(초): 중앙 {statistics.median(q):.1f} · p90 {q[max(0, int(len(q) * 0.9) - 1)]:.1f} · 최대 {q[-1]:.1f}")
        stage_bits = []
        for key, label in STAGES:
            vals = [float((r.get("timing") or {}).get(key) or 0) for r in turns if (r.get("timing") or {}).get(key)]
            if vals:
                stage_bits.append(f"{label} {statistics.median(vals) / 1000:.1f}")
        lines.append(f"- 단계 중앙값(초): {' · '.join(stage_bits)}")
    spent = sum(float(r.get("spent_usd") or 0) for r in turns)
    lines.append(f"- 비용 합계: ${spent:.4f}")
    lines.append("")
    return lines


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", type=Path, action="append", help="debug_turns.jsonl (여러 번 가능). 기본: test-sandbox/data*/")
    ap.add_argument("--run", default="latest", help="latest(기본) | all | <server_run_id>")
    ap.add_argument("--since", default="", help="예: 30m, 2h, 1d 또는 ISO 시각")
    ap.add_argument("--dataset", default="", help="data 폴더 이름 또는 config experiment")
    ap.add_argument("--only", choices=("all", "bad", "flagged", "warn", "error"), default="all")
    ap.add_argument("--last", type=int, default=0)
    ap.add_argument("--id", default="", help="debug_id 하나")
    ap.add_argument("--full", action="store_true", help="답·근거를 자르지 않는다")
    ap.add_argument("--format", choices=("md", "json"), default="md")
    ap.add_argument("--out", type=Path)
    args = ap.parse_args(argv)
    if args.id:
        args.run = "all"
    paths = args.log or default_logs()
    rows = read_rows(paths)
    events, flags, chosen_run = select(rows, args)
    if args.format == "json":
        payload = {
            "run": chosen_run,
            "logs": [str(p) for p in paths],
            "turns": [
                {
                    "kind": r.get("kind"),
                    "ts": r.get("ts"),
                    "debug_id": r.get("debug_id"),
                    "run": r.get("run"),
                    "data_dir": r.get("_data_dir"),
                    "query": r.get("query"),
                    "answer": r.get("answer"),
                    "error": r.get("error"),
                    "flags": flags.get(str(r.get("debug_id") or "")) or [],
                    "diagnosis": diagnosis_of(r) if r.get("kind") == "turn" else None,
                    "hits": r.get("hits") if args.full else [
                        {k: h.get(k) for k in ("rank", "doc_id", "date", "source_room_id", "speakers")} for h in (r.get("hits") or [])
                    ],
                    "timing": r.get("timing"),
                    "spent_usd": r.get("spent_usd"),
                    **({"message": r.get("message"), "where": r.get("where")} if r.get("kind") == "client_error" else {}),
                }
                for r in events
            ],
        }
        text = json.dumps(payload, ensure_ascii=False, indent=1)
    else:
        lines = summary_md(events, flags, chosen_run)
        for i, r in enumerate(events, 1):
            lines.extend(render_turn_md(i, r, flags.get(str(r.get("debug_id") or "")), args.full))
        if not events:
            lines.append("선택한 조건에 맞는 턴이 없다. --run all 또는 --since 를 바꿔 보라.")
        text = "\n".join(lines)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(text + "\n", encoding="utf-8")
    else:
        print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
