"""Playground turn traces for local debugging (not product logs).

Writes gitignored JSONL next to the sandbox index. Query/excerpt stay on disk
so a local agent can inspect failures; they must not be committed or printed
to public docs.
"""

from __future__ import annotations

import hashlib
import json
import threading
import time
import uuid
from pathlib import Path
from typing import Any, Mapping, Sequence

RANKING_MODES = ("hybrid", "heuristic", "rerank", "auto")  # R0, R1, R2(의미 재순위, 모델 교체 가능), auto
CONCRETE_RANKING_MODES = ("hybrid", "heuristic", "rerank")
RANKING_ALIASES = {
    "auto": "auto",
    "자동": "auto",
    "hybrid": "hybrid",
    "r0": "hybrid",
    "heuristic": "heuristic",
    "r1": "heuristic",
    "rerank": "rerank",
    "r2": "rerank",
    "semantic": "rerank",
    "cohere": "rerank",  # 옛 이름 (벤더명). 계속 인식한다
}

_LOCK = threading.Lock()


class RankingError(ValueError):
    pass


def normalize_ranking(value: Any, *, default: str = "heuristic") -> str:
    raw = str(value or default).strip().lower()
    mode = RANKING_ALIASES.get(raw)
    if mode not in RANKING_MODES:
        raise RankingError(f"unknown ranking {raw!r}; use hybrid|heuristic|rerank|auto")
    return mode


def debug_log_path(data_dir: Path) -> Path:
    return Path(data_dir) / "debug_turns.jsonl"


def new_debug_id() -> str:
    return uuid.uuid4().hex[:12]


def query_fingerprint(query: str) -> str:
    return hashlib.sha256((query or "").encode("utf-8")).hexdigest()[:16]


TRACE_VERSION = 2
DUMP_DIRNAME = "debug_dumps"


def debug_dump_dir(data_dir: Path) -> Path:
    return Path(data_dir) / DUMP_DIRNAME


def debug_dump_path(data_dir: Path, debug_id: str) -> Path:
    return debug_dump_dir(data_dir) / f"{debug_id}.json"


def compact_hits(hits: Sequence[Mapping[str, Any]], *, excerpt_chars: int = 8000) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for i, hit in enumerate(hits, start=1):
        excerpt = str(hit.get("excerpt") or "")
        obs = list(hit.get("observation_ids") or [])
        leaf_obs = list(hit.get("leaf_observation_ids") or [])
        row: dict[str, Any] = {
            "rank": i,
            "doc_id": hit.get("doc_id"),
            "date": hit.get("date"),
            "speakers": list(hit.get("speakers") or []),
            "source_room_id": hit.get("source_room_id"),
            "span": hit.get("span"),
            "mode": hit.get("mode"),
            "summary": str(hit.get("summary") or ""),
            "excerpt": excerpt[:excerpt_chars],
            "excerpt_chars": len(excerpt),
            "observation_ids": obs,
            "leaf_observation_ids": leaf_obs,
            "observation_id_count": len(obs),
        }
        if hit.get("intent") is not None:
            row["intent"] = hit.get("intent")
        out.append(row)
    return out


def append_jsonl(path: Path, row: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    line = json.dumps(dict(row), ensure_ascii=False) + "\n"
    with _LOCK:
        with path.open("a", encoding="utf-8") as handle:
            handle.write(line)


def _write_dump(data_dir: Path, row: Mapping[str, Any]) -> None:
    debug_id = str(row.get("debug_id") or "")
    if not debug_id:
        return
    path = debug_dump_path(data_dir, debug_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    with _LOCK:
        path.write_text(json.dumps(dict(row), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def _mirror_scratch(row: Mapping[str, Any], data_dir: Path) -> None:
    repo_guess = Path(data_dir).resolve().parent.parent
    scratch_root = repo_guess / ".masamong-scratch" / "rag-playground-debug"
    try:
        append_jsonl(scratch_root / "debug_turns.jsonl", row)
        debug_id = str(row.get("debug_id") or "")
        if debug_id:
            dump = scratch_root / DUMP_DIRNAME / f"{debug_id}.json"
            dump.parent.mkdir(parents=True, exist_ok=True)
            dump.write_text(json.dumps(dict(row), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    except OSError:
        pass


def append_debug_turn(
    data_dir: Path,
    *,
    query: str,
    ranking: str,
    hits: Sequence[Mapping[str, Any]],
    extra: Mapping[str, Any] | None = None,
    debug_id: str | None = None,
) -> str:
    """debug_id를 미리 받으면(대화 저장과 같은 id를 쓰려고) 그대로 쓴다."""

    debug_id = str(debug_id or "") or new_debug_id()
    row: dict[str, Any] = {
        "kind": "turn",
        "trace_version": TRACE_VERSION,
        "debug_id": debug_id,
        "ts": time.time(),
        "query": query,
        "query_chars": len(query or ""),
        "query_fingerprint": query_fingerprint(query),
        "ranking": ranking,
        "hits": compact_hits(hits),
        "hit_count": len(hits),
    }
    if extra:
        row.update(dict(extra))
    append_jsonl(debug_log_path(data_dir), row)
    try:
        _write_dump(data_dir, row)
    except OSError:
        pass
    _mirror_scratch(row, data_dir)
    return debug_id


def append_debug_error(
    data_dir: Path,
    *,
    query: str,
    ranking: str | None,
    error: BaseException | str,
    extra: Mapping[str, Any] | None = None,
) -> str:
    debug_id = new_debug_id()
    if isinstance(error, BaseException):
        err_name = type(error).__name__
        err_text = str(error)
    else:
        err_name = "Error"
        err_text = str(error)
    row: dict[str, Any] = {
        "kind": "error",
        "trace_version": TRACE_VERSION,
        "debug_id": debug_id,
        "ts": time.time(),
        "query": query,
        "query_chars": len(query or ""),
        "query_fingerprint": query_fingerprint(query),
        "ranking": ranking,
        "error_type": err_name,
        "error": err_text[:2000],
        "ok": False,
    }
    if extra:
        row.update(dict(extra))
    append_jsonl(debug_log_path(data_dir), row)
    try:
        _write_dump(data_dir, row)
    except OSError:
        pass
    _mirror_scratch(row, data_dir)
    return debug_id


def append_debug_flag(
    data_dir: Path,
    *,
    debug_id: str,
    ok: bool | None,
    note: str,
    tags: Sequence[str] | None = None,
    extra: Mapping[str, Any] | None = None,
) -> None:
    """사용자 판정. ok=True 맞음 / False 틀림 / None 메모만. tags는 화면이 정한 짧은 코드(예: wrong_person)."""

    clean_tags = [str(t).strip()[:40] for t in (tags or []) if str(t).strip()][:12]
    row = {
        "kind": "flag",
        "trace_version": TRACE_VERSION,
        "debug_id": str(debug_id),
        "ts": time.time(),
        "ok": ok,
        "tags": clean_tags,
        "note": (note or "")[:4000],
        "note_chars": len(note or ""),
    }
    if extra:
        row.update({k: v for k, v in dict(extra).items() if k not in row})
    append_jsonl(debug_log_path(data_dir), row)
    dump = debug_dump_path(data_dir, str(debug_id))
    if dump.is_file():
        try:
            payload = json.loads(dump.read_text(encoding="utf-8"))
            flags = list(payload.get("flags") or [])
            flags.append(row)
            payload["flags"] = flags
            _write_dump(data_dir, payload)
        except (OSError, json.JSONDecodeError):
            pass
    _mirror_scratch(row, data_dir)


def append_client_event(data_dir: Path, *, event: Mapping[str, Any]) -> None:
    """브라우저에서 난 오류(스트림 끊김·fetch 실패·JS 예외). 서버 로그에 안 남는 실패를 잡는다."""

    allowed = ("type", "message", "where", "debug_id", "conversation_id", "status", "detail", "url")
    row = {"kind": "client_error", "trace_version": TRACE_VERSION, "ts": time.time()}
    for key in allowed:
        if key in event and event[key] is not None:
            row[key] = str(event[key])[:2000]
    append_jsonl(debug_log_path(data_dir), row)
    _mirror_scratch(row, data_dir)


def recent_debug_turns(data_dir: Path, *, limit: int = 30) -> list[dict[str, Any]]:
    path = debug_log_path(data_dir)
    if not path.is_file():
        return []
    rows: list[dict[str, Any]] = []
    with path.open(encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    return rows[-max(1, limit) :]


def load_debug_record(data_dir: Path, debug_id: str) -> dict[str, Any] | None:
    wanted = str(debug_id or "").strip()
    if not wanted:
        return None
    dump = debug_dump_path(data_dir, wanted)
    if dump.is_file():
        try:
            data = json.loads(dump.read_text(encoding="utf-8"))
            if isinstance(data, dict):
                return data
        except (OSError, json.JSONDecodeError):
            pass
    for row in reversed(recent_debug_turns(data_dir, limit=5000)):
        if str(row.get("debug_id") or "") == wanted:
            return row
    return None
