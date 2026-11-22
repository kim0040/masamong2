#!/usr/bin/env python3
"""테스트 샌드박스 HTTP. 제품 관리 콘솔이 아니다. 인덱스는 test-sandbox/data/."""

from __future__ import annotations

import json
import sys
import threading
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from engine import (  # noqa: E402
    SERVER_RUN_ID,
    IndexBundle,
    PlaygroundError,
    chat,
    chat_stream,
    context,
    load_config,
    load_index,
    prepare_status_payload,
    status_payload,
)
from conversation import (  # noqa: E402
    get_conversation,
    new_conversation,
    public_list,
    select_conversation,
)
from rag_upgrade.playground_debug import (  # noqa: E402
    append_client_event,
    append_debug_error,
    append_debug_flag,
    load_debug_record,
    recent_debug_turns,
)
from lab import (  # noqa: E402
    SAFE_CONFIG_KEYS,
    estimate_payload,
    export_bundle,
    import_bundle,
    inventory_payload,
    patch_config,
    room_catalog,
)
from review_sheets import list_sheets, save_verdict, sheet_items  # noqa: E402
from turn_diagnosis import compact_record  # noqa: E402
from debug_report import summarize_run  # noqa: E402
from bot_runtime import (  # noqa: E402
    channel_id_from_room,
    ensure_runtime,
    get_channel,
    load_runtime,
    save_runtime,
    update_channel,
)

STATIC = HERE / "static"


def _data() -> Path:
    """The data dir in use now (the engine context rebinds it; do not keep a copy)."""
    return context().paths.data


class AppState:
    """What the request handlers share: the loaded index and the room-catalog warm-up thread.

    The thread belongs to the state: `start_catalog_refresh()` starts it once, `stop()` tells it not to start the scan
    if it has not begun and waits a little for one that has (the scan itself cannot be interrupted), and a failure is
    kept for /api/status instead of dying silently on a daemon thread."""

    def __init__(self) -> None:
        self.index: IndexBundle | None = None
        self._stop = threading.Event()
        self._catalog_thread: threading.Thread | None = None
        self._catalog_state = "idle"
        self._catalog_error = ""

    def start_catalog_refresh(self) -> None:
        if self._catalog_thread is not None:
            return
        self._catalog_state = "running"
        self._catalog_thread = threading.Thread(target=self._refresh_catalog, name="room-catalog", daemon=True)
        self._catalog_thread.start()

    def _refresh_catalog(self) -> None:
        if self._stop.is_set():
            self._catalog_state = "idle"
            return
        try:
            room_catalog()
        except Exception as exc:  # noqa: BLE001 - the thread must report, never die unseen
            traceback.print_exc()
            self._catalog_error = f"{type(exc).__name__}: {exc}"[:300]
            self._catalog_state = "error"
            return
        self._catalog_state = "ready"

    def catalog_status(self) -> dict:
        return {"state": self._catalog_state, "error": self._catalog_error}

    def stop(self, timeout: float = 2.0) -> None:
        self._stop.set()
        if self._catalog_thread is not None:
            self._catalog_thread.join(timeout)


STATE = AppState()


def _indexed_room_list() -> list[str]:
    if STATE.index is None:
        return [str(r) for r in (load_config().get("rooms") or [])]
    return [str(r) for r in (STATE.index.cfg.get("rooms") or [])]


def _runtime() -> dict:
    return ensure_runtime(
        _data(),
        cfg=load_config(),
        indexed_rooms=_indexed_room_list(),
        blocks=list(STATE.index.blocks or []) if STATE.index is not None else None,
    )


def _json_bytes(payload: dict) -> bytes:
    return json.dumps(payload, ensure_ascii=False).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    server_version = "RagPlayground/0.1"

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003
        # 질문/본문·폴링은 액세스 로그에 남기지 않는다.
        path = urlparse(self.path).path
        if path in {"/api/status", "/api/prepare/progress"}:
            return
        if path.startswith("/static/") or path in {"/", "/index.html", "/favicon.ico", "/app.js", "/app.css"}:
            return
        sys.stderr.write(f"{self.address_string()} {self.command} {path}\n")

    def _send(self, code: int, body: bytes, content_type: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, code: int, payload: dict) -> None:
        self._send(code, _json_bytes(payload), "application/json; charset=utf-8")

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        route = GET_ROUTES.get(parsed.path)
        if route is not None:
            route(self, parse_qs(parsed.query))
            return
        self._send_static(parsed.path)

    def _send_static(self, path: str) -> None:
        if path == "/favicon.ico":
            self._send(204, b"", "image/x-icon")
            return
        if path in {"/", "/index.html"}:
            target = STATIC / "index.html"
        else:
            rel = path.lstrip("/")
            target = (STATIC / rel).resolve()
            if STATIC.resolve() not in target.parents and target != STATIC.resolve():
                self._send_json(404, {"error": "not found"})
                return
        if not target.is_file():
            self._send_json(404, {"error": "not found"})
            return
        data = target.read_bytes()
        ctype = "text/plain; charset=utf-8"
        if target.suffix == ".html":
            ctype = "text/html; charset=utf-8"
        elif target.suffix == ".css":
            ctype = "text/css; charset=utf-8"
        elif target.suffix == ".js":
            ctype = "text/javascript; charset=utf-8"
        self._send(200, data, ctype)

    def do_POST(self) -> None:  # noqa: N802
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length") or "0")
        if length > (2_000_000 if path == "/api/lab/import" else 100_000):
            self._send_json(413, {"error": "요청이 너무 크다"})
            return
        raw = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._send_json(400, {"error": "JSON을 읽을 수 없다"})
            return
        route = POST_ROUTES.get(path)
        if route is None:
            self._send_json(404, {"error": "not found"})
            return
        route(self, payload)


# ---- GET endpoints: (handler, query string as parse_qs gives it) -> None

def _get_status(h: Handler, qs: dict) -> None:
    h._send_json(200, {**status_payload(STATE.index), "room_catalog": STATE.catalog_status()})


def _get_prepare_progress(h: Handler, qs: dict) -> None:
    h._send_json(200, prepare_status_payload())


def _get_lab_inventory(h: Handler, qs: dict) -> None:
    if (qs.get("refresh") or [""])[0] in {"1", "true"}:
        room_catalog(force=True)
    h._send_json(200, inventory_payload())


def _get_lab_export(h: Handler, qs: dict) -> None:
    h._send_json(200, export_bundle())


def _get_lab_config(h: Handler, qs: dict) -> None:
    cfg = load_config()
    h._send_json(200, {k: cfg.get(k) for k in SAFE_CONFIG_KEYS if k in cfg})


def _get_debug_turns(h: Handler, qs: dict) -> None:
    try:
        limit = int((qs.get("limit") or ["100"])[0])
    except ValueError:
        limit = 100
    data = _data()
    h._send_json(
        200,
        {
            "turns": recent_debug_turns(data, limit=min(2000, max(1, limit))),
            "path": str(data / "debug_turns.jsonl"),
            "dumps": str(data / "debug_dumps"),
        },
    )


def _get_debug_run(h: Handler, qs: dict) -> None:
    h._send_json(200, summarize_run([_data() / "debug_turns.jsonl"], SERVER_RUN_ID))


def _get_review_sets(h: Handler, qs: dict) -> None:
    h._send_json(200, {"sets": list_sheets(STATE.index)})


def _get_review_items(h: Handler, qs: dict) -> None:
    try:
        data = sheet_items(STATE.index, str((qs.get("set") or [""])[0]))
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        h._send_json(400, {"error": str(exc)})
        return
    for item in data["items"]:
        item["channel_id"] = channel_id_from_room(str(item.get("room") or "")) or None
    h._send_json(200, data)


def _get_debug_turn(h: Handler, qs: dict) -> None:
    debug_id = str((qs.get("id") or [""])[0]).strip()
    row = load_debug_record(_data(), debug_id)
    if row is None:
        h._send_json(404, {"error": "debug_id 없음", "debug_id": debug_id})
        return
    if (qs.get("compact") or [""])[0] in {"1", "true"}:
        row = compact_record(row)
    h._send_json(200, {"turn": row})


def _get_blocks(h: Handler, qs: dict) -> None:
    blocks = []
    if STATE.index is not None:
        for b in STATE.index.blocks or []:
            blocks.append(
                {
                    "block_id": b.get("block_id"),
                    "platform": b.get("platform"),
                    "collection": b.get("collection"),
                    "source_room": b.get("source_room"),
                    "label": b.get("label"),
                    "leaf_count": b.get("leaf_count"),
                    "message_count": b.get("message_count"),
                    "status": b.get("status"),
                    "embedding_space": b.get("embedding_space"),
                    "time_semantics": b.get("time_semantics"),
                }
            )
    h._send_json(200, {"blocks": blocks})


def _get_bot(h: Handler, qs: dict) -> None:
    runtime = _runtime()
    ch = get_channel(runtime, str(runtime.get("active_channel_id") or ""))
    h._send_json(
        200,
        {
            "active_channel_id": runtime.get("active_channel_id"),
            "mention_required": bool(runtime.get("mention_required")),
            "respond": ch.get("respond") or "off",
            "tools_enabled": runtime.get("tools_enabled"),
            "channels": [
                {
                    "channel_id": cid,
                    "respond": meta.get("respond") or "off",
                    "allowed": bool(meta.get("allowed")),
                    "enabled": bool(meta.get("enabled")),
                    "indexed": bool(meta.get("indexed")),
                    "label": meta.get("label") or f"#{str(cid)[-6:]}",
                }
                for cid, meta in (runtime.get("channels") or {}).items()
                if isinstance(meta, dict)
            ],
            "persona": ch.get("persona") or "",
            "rules": ch.get("rules") or "",
        },
    )


def _conversation_payload(listing: dict, conv: dict) -> dict:
    return {
        **listing,
        "conversation": {
            "id": conv.get("id"),
            "title": conv.get("title"),
            "summary": conv.get("summary") or "",
            "messages": conv.get("messages") or [],
        },
    }


def _get_conversations(h: Handler, qs: dict) -> None:
    data = _data()
    channel_id = str((qs.get("channel_id") or [""])[0]).strip()
    if not channel_id:
        runtime = _runtime()
        channel_id = str(runtime.get("active_channel_id") or "")
    listing = public_list(data, channel_id)
    conv_id = str((qs.get("id") or [listing.get("active_id") or ""])[0]).strip()
    conv = (
        get_conversation(data, channel_id, conv_id or None, create=False)
        if channel_id
        else {"id": "", "title": "새 대화", "messages": [], "summary": ""}
    )
    listing = public_list(data, channel_id)
    h._send_json(200, _conversation_payload(listing, conv))


GET_ROUTES = {
    "/api/status": _get_status,
    "/api/prepare/progress": _get_prepare_progress,
    "/api/lab/inventory": _get_lab_inventory,
    "/api/lab/export": _get_lab_export,
    "/api/lab/config": _get_lab_config,
    "/api/debug/turns": _get_debug_turns,
    "/api/debug/run": _get_debug_run,
    "/api/review/sets": _get_review_sets,
    "/api/review/items": _get_review_items,
    "/api/debug/turn": _get_debug_turn,
    "/api/blocks": _get_blocks,
    "/api/bot": _get_bot,
    "/api/conversations": _get_conversations,
}


# ---- POST endpoints: (handler, decoded JSON body) -> None

def _history_of(h: Handler, payload: dict):
    """The chat history from the body, or None after answering 400."""
    history = payload.get("history") or []
    if not isinstance(history, list):
        h._send_json(400, {"error": "history 형식이 잘못됐다"})
        return None
    return history


def _post_chat(h: Handler, payload: dict) -> None:
    if STATE.index is None:
        h._send_json(503, {"error": "인덱스가 준비되지 않았다"})
        return
    message = payload.get("message", "")
    history = _history_of(h, payload)
    if history is None:
        return
    try:
        result = chat(
            STATE.index,
            message=str(message),
            history=history,
            ranking=payload.get("ranking"),
            speaker=payload.get("speaker"),
            conversation_id=str(payload.get("conversation_id") or "") or None,
            persist=True,
        )
    except PlaygroundError as exc:
        h._send_json(400, {"error": str(exc)})
        return
    except Exception as exc:  # noqa: BLE001
        traceback.print_exc()
        try:
            append_debug_error(
                _data(),
                query=str(message)[:2000],
                ranking=payload.get("ranking"),
                error=exc,
                extra={"path": "http_chat"},
            )
        except OSError:
            pass
        h._send_json(500, {"error": "응답 생성에 실패했다. 서버 로그를 확인하라"})
        return
    h._send_json(200, result)


def _post_chat_stream(h: Handler, payload: dict) -> None:
    if STATE.index is None:
        h._send_json(503, {"error": "인덱스가 준비되지 않았다"})
        return
    message = payload.get("message", "")
    history = _history_of(h, payload)
    if history is None:
        return
    h.send_response(200)
    h.send_header("Content-Type", "text/event-stream; charset=utf-8")
    h.send_header("Cache-Control", "no-store")
    h.send_header("Connection", "close")
    h.send_header("X-Accel-Buffering", "no")
    h.end_headers()

    def _sse(payload_obj: dict) -> None:
        data = f"data: {json.dumps(payload_obj, ensure_ascii=False)}\n\n".encode("utf-8")
        # wfile 버퍼를 건너뛰고 바로 소켓으로 보낸다 (작성중 상태 즉시 표시).
        h.connection.sendall(data)

    try:
        for event in chat_stream(
            STATE.index,
            message=str(message),
            history=history,
            ranking=payload.get("ranking"),
            speaker=payload.get("speaker"),
            conversation_id=str(payload.get("conversation_id") or "") or None,
            persist=True,
        ):
            _sse(event)
    except PlaygroundError as exc:
        _sse({"event": "error", "error": str(exc)})
    except Exception as exc:  # noqa: BLE001
        traceback.print_exc()
        try:
            append_debug_error(
                _data(),
                query=str(message)[:2000],
                ranking=payload.get("ranking"),
                error=exc,
                extra={"path": "http_stream"},
            )
        except OSError:
            pass
        try:
            _sse({"event": "error", "error": "응답 생성에 실패했다. 서버 로그를 확인하라"})
        except (BrokenPipeError, OSError):
            pass


def _post_debug_flag(h: Handler, payload: dict) -> None:
    debug_id = str(payload.get("debug_id") or "").strip()
    if not debug_id:
        h._send_json(400, {"error": "debug_id가 없다"})
        return
    note = str(payload.get("note") or "")
    ok = payload.get("ok")
    if ok is not None:
        ok = bool(ok)
    tags = payload.get("tags") if isinstance(payload.get("tags"), list) else []
    append_debug_flag(
        _data(),
        debug_id=debug_id,
        ok=ok,
        note=note,
        tags=[str(t) for t in tags],
        extra={"conversation_id": str(payload.get("conversation_id") or "") or None},
    )
    h._send_json(200, {"ok": True, "debug_id": debug_id})


def _post_debug_client(h: Handler, payload: dict) -> None:
    append_client_event(_data(), event=payload if isinstance(payload, dict) else {})
    h._send_json(200, {"ok": True})


def _post_review_verdict(h: Handler, payload: dict) -> None:
    try:
        result = save_verdict(
            str(payload.get("set") or ""),
            question_id=str(payload.get("question_id") or ""),
            verdict=str(payload.get("verdict") or ""),
            note=str(payload.get("note") or ""),
        )
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        h._send_json(400, {"error": str(exc)})
        return
    h._send_json(200, result)


def _post_lab_estimate(h: Handler, payload: dict) -> None:
    h._send_json(200, estimate_payload(payload if isinstance(payload, dict) else {}))


def _post_lab_config(h: Handler, payload: dict) -> None:
    try:
        patched = patch_config(payload if isinstance(payload, dict) else {})
    except PlaygroundError as exc:
        h._send_json(400, {"error": str(exc)})
        return
    h._send_json(
        200,
        {
            "ok": True,
            "config": patched,
            "note": "방·한도를 바꿨으면 인덱스는 CLI prepare로 다시 만든다.",
        },
    )


def _post_lab_import(h: Handler, payload: dict) -> None:
    try:
        result = import_bundle(payload if isinstance(payload, dict) else {})
    except PlaygroundError as exc:
        h._send_json(400, {"error": str(exc)})
        return
    h._send_json(200, result)


def _post_conversations(h: Handler, payload: dict) -> None:
    data = _data()
    rooms = list((STATE.index.cfg.get("rooms") if STATE.index else []) or [])
    runtime = ensure_runtime(data, cfg=load_config(), indexed_rooms=[str(r) for r in rooms])
    channel_id = str(payload.get("channel_id") or runtime.get("active_channel_id") or "")
    if not channel_id:
        h._send_json(400, {"error": "channel_id가 없다"})
        return
    action = str(payload.get("action") or "new")
    if action == "new":
        conv = new_conversation(data, channel_id)
    elif action == "select":
        conv = select_conversation(data, channel_id, str(payload.get("id") or ""))
        if conv is None:
            h._send_json(404, {"error": "대화를 찾지 못했다"})
            return
    else:
        h._send_json(400, {"error": "action은 new 또는 select"})
        return
    h._send_json(200, _conversation_payload(public_list(data, channel_id), conv))


def _post_bot(h: Handler, payload: dict) -> None:
    runtime = _runtime()
    cid = str(payload.get("channel_id") or runtime.get("active_channel_id") or "")
    if payload.get("channel_id"):
        runtime["active_channel_id"] = cid
    if payload.get("persona") is not None or payload.get("rules") is not None or "allowed" in payload or "respond" in payload or "view_blocks" in payload:
        try:
            update_channel(runtime, cid, payload)
        except ValueError as exc:
            h._send_json(400, {"error": str(exc)})
            return
    if "mention_required" in payload and "respond" not in payload:
        # legacy toggle → per-channel respond
        runtime["mention_required"] = bool(payload.get("mention_required"))
        update_channel(runtime, cid, {"respond": "mention" if payload.get("mention_required") else "always"})
    if "agent_system_prompt" in payload:
        runtime["agent_system_prompt"] = str(payload.get("agent_system_prompt") or "")
    save_runtime(_data(), runtime)
    ch = get_channel(runtime, cid)
    h._send_json(
        200,
        {
            "ok": True,
            "channel_id": cid,
            "persona": ch.get("persona") or "",
            "rules": ch.get("rules") or "",
            "allowed": bool(ch.get("allowed")),
            "respond": ch.get("respond") or "off",
            "mention_required": bool(runtime.get("mention_required")),
            "view": ch.get("view") if isinstance(ch.get("view"), dict) else {},
        },
    )


POST_ROUTES = {
    "/api/chat": _post_chat,
    "/api/chat/stream": _post_chat_stream,
    "/api/debug/flag": _post_debug_flag,
    "/api/debug/client": _post_debug_client,
    "/api/review/verdict": _post_review_verdict,
    "/api/lab/estimate": _post_lab_estimate,
    "/api/lab/config": _post_lab_config,
    "/api/lab/import": _post_lab_import,
    "/api/conversations": _post_conversations,
    "/api/bot": _post_bot,
}


def main() -> int:
    cfg = load_config()
    try:
        STATE.index = load_index()
    except PlaygroundError as exc:
        print(f"인덱스 아직 없음: {exc}", file=sys.stderr)
        print("python3 prepare.py 진행 상황은 화면·/api/prepare/progress 에 표시된다.", file=sys.stderr)
        STATE.index = None
    host = str(cfg["host"])
    port = int(cfg["port"])
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"테스트 샌드박스  http://{host}:{port}  (data={_data()})")
    if STATE.index is None:
        print("인덱스가 없다. prepare.py 를 켜 두면 진행률이 실시간으로 보인다.")
    else:
        print(
            f"experiment={STATE.index.meta.get('experiment')} "
            f"leaves={len(STATE.index.leaves)} summaries={len(STATE.index.summaries)} "
            f"msgs={len(STATE.index.messages)} session_budget=${cfg['session_budget_usd']}"
        )
    print("Ctrl+C 로 종료. NEVER_COMMIT.md 를 읽어라.")
    STATE.start_catalog_refresh()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n종료")
    finally:
        STATE.stop()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
