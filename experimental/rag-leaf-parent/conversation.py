"""Sandbox chat threads. Not the Discord RAG index. No product DB."""

from __future__ import annotations

import json
import threading
import time
import uuid
from pathlib import Path
from typing import Any, Callable

_lock = threading.RLock()
FILENAME = "conversations.json"

# Keep recent turns verbatim; older ones go into summary when long.
DEFAULT_KEEP = 12
DEFAULT_CHAR_LIMIT = 4500
# 압축 fallback(요약 실패) 문구. 누적 금지 — 항상 한 번만 남긴다.
FALLBACK_NOTE = "(앞부분은 길이 때문에 접었다. 최근 대화만 원문이다.)"


def _fold_target(keep: int) -> int:
    """압축 후 남길 메시지 수. `keep`까지 다시 차오를 여유(히스테리시스)를 둔다.

    이전에는 정확히 `keep`개만 남겨서 다음 턴마다 다시 임계를 넘고,
    턴마다 요약 LLM을 부르며 요약의 요약을 반복했다.
    """

    return max(2, (max(2, int(keep)) // 2) // 2 * 2)


def store_path(data_dir: Path) -> Path:
    return Path(data_dir) / FILENAME


def _empty() -> dict[str, Any]:
    return {"schema_version": 1, "channels": {}}


def _load(data_dir: Path) -> dict[str, Any]:
    path = store_path(data_dir)
    if not path.is_file():
        return _empty()
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return _empty()
    if not isinstance(data, dict):
        return _empty()
    data.setdefault("schema_version", 1)
    data.setdefault("channels", {})
    return data


def _save(data_dir: Path, payload: dict[str, Any]) -> None:
    path = store_path(data_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(path)


def _now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _new_id() -> str:
    return "conv_" + uuid.uuid4().hex[:12]


def _channel_bucket(root: dict[str, Any], channel_id: str) -> dict[str, Any]:
    channels = root.setdefault("channels", {})
    bucket = channels.get(channel_id)
    if not isinstance(bucket, dict):
        bucket = {"active_id": "", "conversations": []}
        channels[channel_id] = bucket
    bucket.setdefault("active_id", "")
    convs = bucket.get("conversations")
    if not isinstance(convs, list):
        convs = []
        bucket["conversations"] = convs
    return bucket


def public_list(data_dir: Path, channel_id: str) -> dict[str, Any]:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        items = []
        for conv in bucket["conversations"]:
            if not isinstance(conv, dict):
                continue
            items.append(
                {
                    "id": conv.get("id"),
                    "title": conv.get("title") or "새 대화",
                    "updated_at": conv.get("updated_at") or "",
                    "n": len(conv.get("messages") or []),
                    "has_summary": bool(str(conv.get("summary") or "").strip()),
                }
            )
        items.reverse()
        return {"channel_id": channel_id, "active_id": bucket.get("active_id") or "", "conversations": items}


def get_conversation(
    data_dir: Path,
    channel_id: str,
    conv_id: str | None = None,
    *,
    create: bool = True,
) -> dict[str, Any]:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        cid = (conv_id or bucket.get("active_id") or "").strip()
        for conv in bucket["conversations"]:
            if isinstance(conv, dict) and conv.get("id") == cid:
                return dict(conv)
        if bucket["conversations"]:
            conv = bucket["conversations"][-1]
            if isinstance(conv, dict):
                return dict(conv)
        if not create:
            return {"id": "", "title": "새 대화", "created_at": "", "updated_at": "", "messages": [], "summary": ""}
        created = new_conversation(data_dir, channel_id)
        return created


def new_conversation(data_dir: Path, channel_id: str) -> dict[str, Any]:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        conv = {
            "id": _new_id(),
            "title": "새 대화",
            "created_at": _now(),
            "updated_at": _now(),
            "messages": [],
            "summary": "",
            "summarized_n": 0,
        }
        bucket["conversations"].append(conv)
        bucket["active_id"] = conv["id"]
        _save(data_dir, root)
        return dict(conv)


def put_conversation(data_dir: Path, channel_id: str, conv: dict[str, Any]) -> None:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        cid = str(conv.get("id") or "")
        found = False
        for i, row in enumerate(bucket["conversations"]):
            if isinstance(row, dict) and row.get("id") == cid:
                bucket["conversations"][i] = conv
                found = True
                break
        if not found:
            bucket["conversations"].append(conv)
        bucket["active_id"] = cid
        _save(data_dir, root)


def select_conversation(data_dir: Path, channel_id: str, conv_id: str) -> dict[str, Any] | None:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        for conv in bucket["conversations"]:
            if isinstance(conv, dict) and conv.get("id") == conv_id:
                bucket["active_id"] = conv_id
                _save(data_dir, root)
                return dict(conv)
        return None


def history_for_llm(conv: dict[str, Any], *, keep: int) -> tuple[str, list[dict[str, str]]]:
    messages = [m for m in (conv.get("messages") or []) if isinstance(m, dict)]
    keep = max(2, int(keep))
    recent = messages[-keep:]
    summary = str(conv.get("summary") or "").strip()
    out: list[dict[str, str]] = []
    for m in recent:
        role = m.get("role")
        content = str(m.get("content") or "").strip()
        if role in {"user", "assistant"} and content:
            out.append({"role": str(role), "content": content})
    return summary, out


def needs_compact(conv: dict[str, Any], *, keep: int, char_limit: int) -> bool:
    messages = [m for m in (conv.get("messages") or []) if isinstance(m, dict)]
    if len(messages) <= keep:
        chars = sum(len(str(m.get("content") or "")) for m in messages)
        return chars > char_limit
    return True


def compact(
    conv: dict[str, Any],
    *,
    keep: int,
    char_limit: int,
    summarizer: Callable[[str], str] | None = None,
) -> dict[str, Any]:
    messages = [m for m in (conv.get("messages") or []) if isinstance(m, dict)]
    keep = max(2, int(keep))
    if not needs_compact(conv, keep=keep, char_limit=char_limit):
        return conv
    target = _fold_target(keep)
    if len(messages) > target:
        # 턴 수 초과든 글자 수 초과든 같은 지점(keep/2)까지만 접는다 — 절벽처럼 2개로 무너지지 않게.
        old = messages[:-target]
        keep_msgs = messages[-target:]
        drop_old = True
    else:
        old = messages
        keep_msgs = messages
        drop_old = False
    if not old or not drop_old:
        return conv
    blob_parts = []
    prev = str(conv.get("summary") or "").strip()
    prev_clean = prev.replace(FALLBACK_NOTE, "").strip()
    if prev_clean:
        blob_parts.append("이전 요약:\n" + prev_clean)
    for m in old:
        role = "사용자" if m.get("role") == "user" else "마사몽"
        blob_parts.append(f"{role}: {str(m.get('content') or '')[:800]}")
    blob = "\n".join(blob_parts)[:12000]
    summary = ""
    if summarizer is not None:
        try:
            summary = (summarizer(blob) or "").strip()
        except Exception:
            summary = ""
    if not summary:
        summary = (prev_clean + "\n" if prev_clean else "") + FALLBACK_NOTE
    conv["summary"] = summary[:2500]
    if drop_old:
        conv["summarized_n"] = int(conv.get("summarized_n") or 0) + len(old)
        conv["messages"] = keep_msgs
    conv["updated_at"] = _now()
    return conv


def append_turn(
    data_dir: Path,
    channel_id: str,
    conv_id: str,
    *,
    user: str,
    assistant: str,
    speaker: str = "",
    keep: int = DEFAULT_KEEP,
    char_limit: int = DEFAULT_CHAR_LIMIT,
    summarizer: Callable[[str], str] | None = None,
    debug_id: str | None = None,
) -> dict[str, Any]:
    with _lock:
        root = _load(data_dir)
        bucket = _channel_bucket(root, channel_id)
        conv = None
        for row in bucket["conversations"]:
            if isinstance(row, dict) and row.get("id") == conv_id:
                conv = row
                break
        if conv is None:
            conv = {
                "id": conv_id or _new_id(),
                "title": "새 대화",
                "created_at": _now(),
                "updated_at": _now(),
                "messages": [],
                "summary": "",
                "summarized_n": 0,
            }
            bucket["conversations"].append(conv)
        msgs = conv.setdefault("messages", [])
        ts = _now()
        msgs.append({"role": "user", "content": user, "speaker": speaker, "ts": ts})
        reply: dict[str, Any] = {"role": "assistant", "content": assistant, "ts": ts}
        if debug_id:
            # 새로고침 뒤에도 화면이 이 답의 진단·판정 버튼을 다시 붙일 수 있게 한다.
            reply["debug_id"] = str(debug_id)
        msgs.append(reply)
        if conv.get("title") in {"", "새 대화"}:
            conv["title"] = user.strip().replace("\n", " ")[:36] or "새 대화"
        conv["updated_at"] = ts
        bucket["active_id"] = conv["id"]
        snapshot = json.loads(json.dumps(conv))
        _save(data_dir, root)

    if needs_compact(snapshot, keep=keep, char_limit=char_limit):
        snapshot = compact(snapshot, keep=keep, char_limit=char_limit, summarizer=summarizer)
        with _lock:
            root = _load(data_dir)
            bucket = _channel_bucket(root, channel_id)
            for i, row in enumerate(bucket["conversations"]):
                if isinstance(row, dict) and row.get("id") == snapshot.get("id"):
                    bucket["conversations"][i] = snapshot
                    break
            _save(data_dir, root)
    return dict(snapshot)


SUMMARIZE_PROMPT = """다음 채널 테스트 대화를 한국어 8문장 이내로 요약하라.
이름·결정·약속·아직 안 끝난 질문만 남겨라. 없는 사실을 만들지 마라. 요약만 출력하라.

{body}"""
