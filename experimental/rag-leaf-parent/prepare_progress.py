"""Index prepare progress for CLI and the sandbox UI. No message bodies."""

from __future__ import annotations

import json
import threading
import time
from pathlib import Path
from typing import Any

_lock = threading.Lock()
_last_write = 0.0
_embed: dict[str, tuple[int, int]] = {}
_t0: dict[str, float] = {}
_phase_base: dict[str, int] = {}

PHASE_WEIGHT = {
    "segments": 0.08,
    "summaries": 0.72,
    "embeddings": 0.20,
}


def format_eta(seconds: float | None) -> str:
    if seconds is None or seconds < 0 or seconds > 86400 * 7:
        return ""
    s = int(seconds)
    if s < 15:
        return "곧 끝"
    if s < 90:
        return f"약 {s}초"
    if s < 3600:
        return f"약 {s // 60}분 {s % 60:02d}초"
    return f"약 {s // 3600}시간 {(s % 3600) // 60}분"


def _pct(done: int, total: int) -> float:
    if total <= 0:
        return 0.0
    return min(100.0, max(0.0, 100.0 * done / total))


def _overall(phase: str, phase_pct: float, *, summary: bool) -> float:
    weights = dict(PHASE_WEIGHT)
    if not summary:
        weights["summaries"] = 0.0
        weights["embeddings"] = 0.92
    order = ("segments", "summaries", "embeddings")
    acc = 0.0
    for name in order:
        w = float(weights.get(name) or 0)
        if name == phase:
            acc += w * (phase_pct / 100.0)
            break
        acc += w
    return min(100.0, round(acc * 1000) / 10)


def snapshot(
    *,
    phase: str,
    done: int,
    total: int,
    status: str = "running",
    detail: str = "",
    cost_usd: float | None = None,
    summary_enabled: bool = True,
    started_at: float | None = None,
) -> dict[str, Any]:
    now = time.time()
    t0 = started_at if started_at else now
    elapsed = max(0.0, now - t0)
    remaining = max(0, total - done)
    base = int(_phase_base.get(phase, 0))
    progressed = max(0, done - base)
    rate = (progressed / elapsed) if elapsed > 0.5 and progressed > 0 else 0.0
    eta = (remaining / rate) if rate > 0 else None
    phase_pct = _pct(done, total)
    overall = 100.0 if status in {"done", "ready"} else _overall(phase, phase_pct, summary=summary_enabled)
    if status in {"done", "ready"}:
        remaining = 0
        eta = 0.0
        phase_pct = 100.0
    label_phase = {
        "segments": "문맥 조각 나누는 중",
        "summaries": "leaf 요약 중",
        "embeddings": "임베딩 중",
        "done": "인덱스 준비됨",
    }.get(phase, phase)
    eta_txt = format_eta(eta)
    parts = [f"{label_phase} {done:,}/{total:,} ({phase_pct:.1f}%)"]
    if remaining and status == "running":
        parts.append(f"남음 {remaining:,}")
    if eta_txt and status == "running":
        parts.append(eta_txt)
    if detail:
        parts.append(detail)
    return {
        "status": status,
        "phase": phase,
        "detail": detail,
        "done": int(done),
        "total": int(total),
        "remaining": int(remaining),
        "phase_pct": round(phase_pct, 1),
        "overall_pct": overall if status == "running" else 100.0,
        "rate_per_s": round(rate, 2),
        "eta_s": None if eta is None else int(eta),
        "eta_label": eta_txt,
        "elapsed_s": int(elapsed),
        "label": " · ".join(parts),
        "cost_usd": cost_usd,
        "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
    }


def write_progress(path: Path, payload: dict[str, Any], *, force: bool = False) -> bool:
    global _last_write
    now = time.time()
    with _lock:
        if not force and now - _last_write < 0.4 and payload.get("status") == "running":
            return False
        _last_write = now
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(json.dumps(payload, ensure_ascii=False) + "\n", encoding="utf-8")
        tmp.replace(path)
        return True


def idle_payload(*, ready: bool) -> dict[str, Any]:
    if ready:
        return snapshot(phase="done", done=1, total=1, status="ready")
    return {
        "status": "idle",
        "phase": "idle",
        "detail": "",
        "done": 0,
        "total": 0,
        "remaining": 0,
        "phase_pct": 0.0,
        "overall_pct": 0.0,
        "rate_per_s": 0.0,
        "eta_s": None,
        "eta_label": "",
        "elapsed_s": 0,
        "label": "인덱스 없음. python3 prepare.py 를 실행하세요.",
        "cost_usd": None,
        "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }


def read_progress(path: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def print_cli(payload: dict[str, Any]) -> None:
    line = str(payload.get("label") or "")
    extra = []
    if payload.get("overall_pct") is not None and payload.get("phase") != "done":
        extra.append(f"전체 {payload['overall_pct']:.1f}%")
    if payload.get("cost_usd") is not None:
        extra.append(f"${float(payload['cost_usd']):.4f}")
    if extra:
        line = f"{line} · {' · '.join(extra)}"
    print(line, flush=True)


def mark_phase_start(phase: str, already: int = 0) -> None:
    _t0[phase] = time.time()
    _phase_base[phase] = max(0, int(already))


def phase_started(phase: str) -> float:
    if phase not in _t0:
        _t0[phase] = time.time()
    return float(_t0[phase])


def reset_embed_counts() -> None:
    with _lock:
        _embed.clear()


def set_embed_count(name: str, done: int, total: int) -> tuple[int, int]:
    with _lock:
        _embed[name] = (int(done), int(total))
        d = sum(a for a, _ in _embed.values())
        t = sum(b for _, b in _embed.values())
        return d, t
