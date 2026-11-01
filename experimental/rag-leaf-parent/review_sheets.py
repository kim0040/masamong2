"""holdout 검수 시트를 샌드박스 화면에서 읽고 판정을 저장한다.

시트는 `build_holdout_gold.py`가 gold 옆에 쓰는 `<이름>.review.json`(문항별 verdict·note)이다.
원문 근거는 지금 켜 둔 인덱스의 leaf·message에서만 찾는다. 인덱스에 없는 문항은 `bound=false`로 표시한다.
경로는 `.masamong-scratch/` 안의 `*.review.json`만 허용한다. 판정은 verdict·note 필드만 바꾼다.
"""

from __future__ import annotations

import json
import os
import tempfile
import threading
from pathlib import Path
from typing import Any, Mapping

from sandbox_paths import repo_root

VERDICTS = ("", "ok", "fix", "drop")
_LOCK = threading.Lock()


def scratch_root() -> Path:
    return (repo_root() / ".masamong-scratch").resolve()


def _resolve_sheet(rel: str) -> Path:
    root = scratch_root()
    path = (root / str(rel or "")).resolve()
    if root not in path.parents or not path.name.endswith(".review.json") or not path.is_file():
        raise ValueError("검수 시트 경로가 올바르지 않다")
    return path


def _gold_for(sheet: Path) -> Path:
    return sheet.with_name(sheet.name[: -len(".review.json")] + ".json")


def _load_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def _atomic_write(path: Path, payload: Any) -> None:
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=".review-", suffix=".json")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=1)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _questions(gold: Any) -> list[dict[str, Any]]:
    if isinstance(gold, dict):
        return [q for q in gold.get("questions") or [] if isinstance(q, dict)]
    return []


def _anchor_leaf(question: Mapping[str, Any], leaf_by_id: Mapping[str, Any]) -> Mapping[str, Any] | None:
    """leaf id는 인덱스 안 위치(`leaf:start-end`)라 다른 인덱스에도 같은 id가 있을 수 있다. 방까지 같아야 인정한다."""

    leaf = leaf_by_id.get(str(question.get("source_leaf_doc_id") or ""))
    if leaf is None:
        return None
    room = str(question.get("room") or "")
    if room and str(leaf.get("source_room_id") or "") != room:
        return None
    return leaf


def _bound_share(questions: list[dict[str, Any]], leaf_by_id: Mapping[str, Any]) -> float:
    anchored = [q for q in questions if not q.get("expected_abstention") and q.get("source_leaf_doc_id")]
    if not anchored:
        return 0.0
    return sum(1 for q in anchored if _anchor_leaf(q, leaf_by_id) is not None) / len(anchored)


def list_sheets(index: Any | None) -> list[dict[str, Any]]:
    root = scratch_root()
    if not root.is_dir():
        return []
    leaf_by_id = getattr(index, "leaf_by_id", {}) or {}
    out: list[dict[str, Any]] = []
    for sheet in sorted(root.rglob("*.review.json")):
        try:
            rows = _load_json(sheet)
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(rows, list):
            continue
        gold_path = _gold_for(sheet)
        questions: list[dict[str, Any]] = []
        if gold_path.is_file():
            try:
                questions = _questions(_load_json(gold_path))
            except (OSError, json.JSONDecodeError):
                questions = []
        out.append(
            {
                "set": str(sheet.relative_to(root)),
                "n": len(rows),
                "reviewed": sum(1 for r in rows if isinstance(r, dict) and r.get("verdict")),
                "has_gold": bool(questions),
                "bound_share": round(_bound_share(questions, leaf_by_id), 3),
            }
        )
    out.sort(key=lambda r: (-r["bound_share"], r["set"]))
    return out


def _evidence(index: Any, question: Mapping[str, Any], *, context: int) -> dict[str, Any]:
    leaf_by_id = getattr(index, "leaf_by_id", {}) or {}
    messages = getattr(index, "messages", []) or []
    leaf = _anchor_leaf(question, leaf_by_id)
    if leaf is None:
        return {"bound": False, "lines": []}
    want = {str(x) for x in (question.get("want_observation_ids") or question.get("expected_evidence_ids") or [])}
    start = max(0, int(leaf.get("start") or 0) - context)
    end = min(len(messages), int(leaf.get("end") or 0) + context)
    room = str(leaf.get("source_room_id") or "")
    lines = []
    for m in messages[start:end]:
        if str(m.get("source_room_id") or "") != room:
            continue
        lines.append(
            {
                "time": m.get("parsed_time"),
                "speaker": m.get("actor_label"),
                "text": str(m.get("content") or "")[:1000],
                "wanted": str(m.get("observation_id") or "") in want,
            }
        )
    return {"bound": True, "room": room, "lines": lines}


def sheet_items(index: Any | None, rel: str, *, context: int = 4) -> dict[str, Any]:
    sheet = _resolve_sheet(rel)
    rows = _load_json(sheet)
    gold_path = _gold_for(sheet)
    by_id = {str(q.get("question_id")): q for q in _questions(_load_json(gold_path))} if gold_path.is_file() else {}
    items = []
    for row in rows if isinstance(rows, list) else []:
        if not isinstance(row, dict):
            continue
        q = by_id.get(str(row.get("question_id")), {})
        item = {
            "question_id": row.get("question_id"),
            "query": row.get("query") or q.get("query"),
            "type": row.get("type") or q.get("question_type"),
            "reference_answer": row.get("reference_answer") or q.get("reference_answer"),
            "expected_abstention": bool(row.get("expected_abstention", q.get("expected_abstention"))),
            "verdict": row.get("verdict") or "",
            "note": row.get("note") or "",
            "room": q.get("room"),
            "anchor_time": q.get("anchor_time"),
        }
        if index is not None and q and not item["expected_abstention"]:
            item["evidence"] = _evidence(index, q, context=context)
        items.append(item)
    return {"set": rel, "items": items, "verdicts": list(VERDICTS)}


def save_verdict(rel: str, *, question_id: str, verdict: str, note: str) -> dict[str, Any]:
    if verdict not in VERDICTS:
        raise ValueError(f"verdict는 {VERDICTS} 중 하나")
    sheet = _resolve_sheet(rel)
    with _LOCK:
        rows = _load_json(sheet)
        if not isinstance(rows, list):
            raise ValueError("검수 시트 형식이 올바르지 않다")
        for row in rows:
            if isinstance(row, dict) and str(row.get("question_id")) == str(question_id):
                row["verdict"] = verdict
                row["note"] = str(note or "")[:2000]
                break
        else:
            raise ValueError("문항을 찾지 못했다")
        _atomic_write(sheet, rows)
    return {"ok": True, "reviewed": sum(1 for r in rows if isinstance(r, dict) and r.get("verdict")), "n": len(rows)}
