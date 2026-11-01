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
    DATA,
    SERVER_RUN_ID,
    IndexBundle,
    PlaygroundError,
    chat,
    chat_stream,
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
INDEX: IndexBundle | None = None


def _indexed_room_list() -> list[str]:
    if INDEX is None:
        return [str(r) for r in (load_config().get("rooms") or [])]
    return [str(r) for r in (INDEX.cfg.get("rooms") or [])]


def _runtime() -> dict:
    return ensure_runtime(
        DATA,
        cfg=load_config(),
        indexed_rooms=_indexed_room_list(),
        blocks=list(INDEX.blocks or []) if INDEX is not None else None,
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
        path = urlparse(self.path).path
        if path == "/api/status":
            self._send_json(200, status_payload(INDEX))
            return
        if path == "/api/prepare/progress":
            self._send_json(200, prepare_status_payload())
            return
        if path == "/api/lab/inventory":
            qs = parse_qs(urlparse(self.path).query)
            if (qs.get("refresh") or [""])[0] in {"1", "true"}:
                room_catalog(force=True)
            self._send_json(200, inventory_payload())
            return
        if path == "/api/lab/export":
            self._send_json(200, export_bundle())
            return
        if path == "/api/lab/config":
            cfg = load_config()
            self._send_json(200, {k: cfg.get(k) for k in SAFE_CONFIG_KEYS if k in cfg})
            return
        if path == "/api/debug/turns":
            qs = parse_qs(urlparse(self.path).query)
            try:
                limit = int((qs.get("limit") or ["100"])[0])
            except ValueError:
                limit = 100
            self._send_json(
                200,
                {
                    "turns": recent_debug_turns(DATA, limit=min(2000, max(1, limit))),
                    "path": str(DATA / "debug_turns.jsonl"),
                    "dumps": str(DATA / "debug_dumps"),
                },
            )
            return
        if path == "/api/debug/run":
            self._send_json(200, summarize_run([DATA / "debug_turns.jsonl"], SERVER_RUN_ID))
            return
        if path == "/api/review/sets":
            self._send_json(200, {"sets": list_sheets(INDEX)})
            return
        if path == "/api/review/items":
            qs = parse_qs(urlparse(self.path).query)
            try:
                data = sheet_items(INDEX, str((qs.get("set") or [""])[0]))
            except (ValueError, OSError, json.JSONDecodeError) as exc:
                self._send_json(400, {"error": str(exc)})
                return
            for item in data["items"]:
                item["channel_id"] = channel_id_from_room(str(item.get("room") or "")) or None
            self._send_json(200, data)
            return
        if path == "/api/debug/turn":
            qs = parse_qs(urlparse(self.path).query)
            debug_id = str((qs.get("id") or [""])[0]).strip()
            row = load_debug_record(DATA, debug_id)
            if row is None:
                self._send_json(404, {"error": "debug_id 없음", "debug_id": debug_id})
                return
            if (qs.get("compact") or [""])[0] in {"1", "true"}:
                row = compact_record(row)
            self._send_json(200, {"turn": row})
            return
        if path == "/api/blocks":
            blocks = []
            if INDEX is not None:
                for b in INDEX.blocks or []:
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
            self._send_json(200, {"blocks": blocks})
            return
        if path == "/api/bot":
            runtime = _runtime()
            ch = get_channel(runtime, str(runtime.get("active_channel_id") or ""))
            self._send_json(
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
            return
        if path == "/api/conversations":
            qs = parse_qs(urlparse(self.path).query)
            channel_id = str((qs.get("channel_id") or [""])[0]).strip()
            if not channel_id:
                runtime = _runtime()
                channel_id = str(runtime.get("active_channel_id") or "")
            listing = public_list(DATA, channel_id)
            conv_id = str((qs.get("id") or [listing.get("active_id") or ""])[0]).strip()
            conv = (
                get_conversation(DATA, channel_id, conv_id or None, create=False)
                if channel_id
                else {"id": "", "title": "새 대화", "messages": [], "summary": ""}
            )
            listing = public_list(DATA, channel_id)
            self._send_json(
                200,
                {
                    **listing,
                    "conversation": {
                        "id": conv.get("id"),
                        "title": conv.get("title"),
                        "summary": conv.get("summary") or "",
                        "messages": conv.get("messages") or [],
                    },
                },
            )
            return
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

        if path == "/api/chat":
            if INDEX is None:
                self._send_json(503, {"error": "인덱스가 준비되지 않았다"})
                return
            message = payload.get("message", "")
            history = payload.get("history") or []
            if not isinstance(history, list):
                self._send_json(400, {"error": "history 형식이 잘못됐다"})
                return
            try:
                result = chat(
                    INDEX,
                    message=str(message),
                    history=history,
                    ranking=payload.get("ranking"),
                    speaker=payload.get("speaker"),
                    conversation_id=str(payload.get("conversation_id") or "") or None,
                    persist=True,
                )
            except PlaygroundError as exc:
                self._send_json(400, {"error": str(exc)})
                return
            except Exception as exc:  # noqa: BLE001
                traceback.print_exc()
                try:
                    append_debug_error(
                        DATA,
                        query=str(message)[:2000],
                        ranking=payload.get("ranking"),
                        error=exc,
                        extra={"path": "http_chat"},
                    )
                except OSError:
                    pass
                self._send_json(500, {"error": "응답 생성에 실패했다. 서버 로그를 확인하라"})
                return
            self._send_json(200, result)
            return

        if path == "/api/chat/stream":
            if INDEX is None:
                self._send_json(503, {"error": "인덱스가 준비되지 않았다"})
                return
            message = payload.get("message", "")
            history = payload.get("history") or []
            if not isinstance(history, list):
                self._send_json(400, {"error": "history 형식이 잘못됐다"})
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.send_header("X-Accel-Buffering", "no")
            self.end_headers()

            def _sse(payload_obj: dict) -> None:
                data = f"data: {json.dumps(payload_obj, ensure_ascii=False)}\n\n".encode(
                    "utf-8"
                )
                # wfile 버퍼를 건너뛰고 바로 소켓으로 보낸다 (작성중 상태 즉시 표시).
                self.connection.sendall(data)

            try:
                for event in chat_stream(
                    INDEX,
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
                        DATA,
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
            return

        if path == "/api/debug/flag":
            debug_id = str(payload.get("debug_id") or "").strip()
            if not debug_id:
                self._send_json(400, {"error": "debug_id가 없다"})
                return
            note = str(payload.get("note") or "")
            ok = payload.get("ok")
            if ok is not None:
                ok = bool(ok)
            tags = payload.get("tags") if isinstance(payload.get("tags"), list) else []
            append_debug_flag(
                DATA,
                debug_id=debug_id,
                ok=ok,
                note=note,
                tags=[str(t) for t in tags],
                extra={"conversation_id": str(payload.get("conversation_id") or "") or None},
            )
            self._send_json(200, {"ok": True, "debug_id": debug_id})
            return

        if path == "/api/debug/client":
            append_client_event(DATA, event=payload if isinstance(payload, dict) else {})
            self._send_json(200, {"ok": True})
            return

        if path == "/api/review/verdict":
            try:
                result = save_verdict(
                    str(payload.get("set") or ""),
                    question_id=str(payload.get("question_id") or ""),
                    verdict=str(payload.get("verdict") or ""),
                    note=str(payload.get("note") or ""),
                )
            except (ValueError, OSError, json.JSONDecodeError) as exc:
                self._send_json(400, {"error": str(exc)})
                return
            self._send_json(200, result)
            return

        if path == "/api/lab/estimate":
            self._send_json(200, estimate_payload(payload if isinstance(payload, dict) else {}))
            return
        if path == "/api/lab/config":
            try:
                patched = patch_config(payload if isinstance(payload, dict) else {})
            except PlaygroundError as exc:
                self._send_json(400, {"error": str(exc)})
                return
            self._send_json(
                200,
                {
                    "ok": True,
                    "config": patched,
                    "note": "방·한도를 바꿨으면 인덱스는 CLI prepare로 다시 만든다.",
                },
            )
            return
        if path == "/api/lab/import":
            try:
                result = import_bundle(payload if isinstance(payload, dict) else {})
            except PlaygroundError as exc:
                self._send_json(400, {"error": str(exc)})
                return
            self._send_json(200, result)
            return

        if path == "/api/conversations":
            rooms = list((INDEX.cfg.get("rooms") if INDEX else []) or [])
            runtime = ensure_runtime(DATA, cfg=load_config(), indexed_rooms=[str(r) for r in rooms])
            channel_id = str(payload.get("channel_id") or runtime.get("active_channel_id") or "")
            if not channel_id:
                self._send_json(400, {"error": "channel_id가 없다"})
                return
            action = str(payload.get("action") or "new")
            if action == "new":
                conv = new_conversation(DATA, channel_id)
            elif action == "select":
                conv = select_conversation(DATA, channel_id, str(payload.get("id") or ""))
                if conv is None:
                    self._send_json(404, {"error": "대화를 찾지 못했다"})
                    return
            else:
                self._send_json(400, {"error": "action은 new 또는 select"})
                return
            listing = public_list(DATA, channel_id)
            self._send_json(
                200,
                {
                    **listing,
                    "conversation": {
                        "id": conv.get("id"),
                        "title": conv.get("title"),
                        "summary": conv.get("summary") or "",
                        "messages": conv.get("messages") or [],
                    },
                },
            )
            return

        if path == "/api/bot":
            runtime = _runtime()
            cid = str(payload.get("channel_id") or runtime.get("active_channel_id") or "")
            if payload.get("channel_id"):
                runtime["active_channel_id"] = cid
            if payload.get("persona") is not None or payload.get("rules") is not None or "allowed" in payload or "respond" in payload or "view_blocks" in payload:
                try:
                    update_channel(runtime, cid, payload)
                except ValueError as exc:
                    self._send_json(400, {"error": str(exc)})
                    return
            if "mention_required" in payload and "respond" not in payload:
                # legacy toggle → per-channel respond
                runtime["mention_required"] = bool(payload.get("mention_required"))
                update_channel(runtime, cid, {"respond": "mention" if payload.get("mention_required") else "always"})
            if "agent_system_prompt" in payload:
                runtime["agent_system_prompt"] = str(payload.get("agent_system_prompt") or "")
            save_runtime(DATA, runtime)
            ch = get_channel(runtime, cid)
            self._send_json(
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
            return

        self._send_json(404, {"error": "not found"})


def main() -> int:
    global INDEX
    cfg = load_config()
    try:
        INDEX = load_index()
    except PlaygroundError as exc:
        print(f"인덱스 아직 없음: {exc}", file=sys.stderr)
        print("python3 prepare.py 진행 상황은 화면·/api/prepare/progress 에 표시된다.", file=sys.stderr)
        INDEX = None
    host = str(cfg["host"])
    port = int(cfg["port"])
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"테스트 샌드박스  http://{host}:{port}  (data={DATA})")
    if INDEX is None:
        print("인덱스가 없다. prepare.py 를 켜 두면 진행률이 실시간으로 보인다.")
    else:
        print(
            f"experiment={INDEX.meta.get('experiment')} "
            f"leaves={len(INDEX.leaves)} summaries={len(INDEX.summaries)} "
            f"msgs={len(INDEX.messages)} session_budget=${cfg['session_budget_usd']}"
        )
    print("Ctrl+C 로 종료. NEVER_COMMIT.md 를 읽어라.")
    threading.Thread(target=lambda: room_catalog(), daemon=True).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n종료")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
