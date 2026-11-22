"""Sandbox lab: corpus catalog, ingest estimate, config import/export."""

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

from rag_upgrade.ingest_cost import estimate_ingest

from corpus import collection_names, iter_observations  # noqa: E402
from engine import DATA, HERE, load_config, PlaygroundError
from memory_blocks import display_room_label

# 실험실에서 바꿀 수 있는 키는 예시 설정에서 만든다. 새 검색·설정 키를 예시에
# 넣으면 자동으로 실험실에서 바꿀 수 있다. 프로세스 바인딩과 파일 경로 키만 뺀다.
_LAB_FORBIDDEN = frozenset({"data_dir", "eval_questions", "experiment", "host", "port"})


def _example_config_keys() -> tuple[str, ...]:
    example_path = Path(__file__).resolve().parent / "config.example.json"
    example = json.loads(example_path.read_text(encoding="utf-8"))
    return tuple(key for key in example if key not in _LAB_FORBIDDEN)


SAFE_CONFIG_KEYS = _example_config_keys()

def config_path() -> Path:
    return HERE / "config.json"


def save_config(cfg: dict[str, Any]) -> None:
    path = config_path()
    path.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def public_room_label(room_id: str) -> str:
    return display_room_label(room_id)


def _scan_collection(collection: str) -> list[dict[str, Any]]:
    rooms: dict[str, dict[str, Any]] = {}
    for kind in ("dated", "undated"):
        try:
            stream = iter_observations(collection, kind=kind)
            for obs in stream:
                rid = str(obs.source_room_id)
                row = rooms.setdefault(rid, {"id": rid, "messages": 0, "chars": 0})
                row["messages"] += 1
                row["chars"] += len(str(getattr(obs, "content", "") or ""))
        except FileNotFoundError:
            continue
    out = []
    for rid, row in rooms.items():
        n = int(row["messages"])
        out.append(
            {
                "id": rid,
                "label": public_room_label(rid),
                "messages": n,
                "avg_chars": round(row["chars"] / n, 1) if n else 0,
            }
        )
    out.sort(key=lambda item: -int(item["messages"]))
    return out


def room_catalog(*, force: bool = False) -> dict[str, Any]:
    cache = DATA / "room_catalog.json"
    if cache.is_file() and not force:
        try:
            raw = json.loads(cache.read_text(encoding="utf-8"))
            if isinstance(raw, dict) and raw.get("collections"):
                return raw
        except (OSError, json.JSONDecodeError):
            pass
    collections: dict[str, Any] = {}
    for name in collection_names():
        rooms = _scan_collection(name)
        collections[name] = {
            "rooms": rooms,
            "room_count": len(rooms),
            "messages": sum(int(r["messages"]) for r in rooms),
        }
    payload = {
        "ts": time.time(),
        "disclaimer": "원문 준비(.private/raw-corpus) 기준. 제품 임베딩 완료가 아니다.",
        "collections": collections,
    }
    DATA.mkdir(parents=True, exist_ok=True)
    cache.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
    return payload


def inventory_payload() -> dict[str, Any]:
    cfg = load_config()
    catalog = room_catalog()
    indexed = [str(r) for r in (cfg.get("rooms") or [])]
    indexed_set = set(indexed)
    collections = []
    for name, block in (catalog.get("collections") or {}).items():
        rooms = []
        for room in block.get("rooms") or []:
            item = dict(room)
            item["indexed"] = item["id"] in indexed_set
            item["active_collection"] = name == cfg.get("collection")
            rooms.append(item)
        collections.append(
            {
                "id": name,
                "messages": block.get("messages"),
                "room_count": block.get("room_count"),
                "indexed_rooms": sum(1 for r in rooms if r["indexed"] and name == cfg.get("collection")),
                "rooms": rooms,
            }
        )
    return {
        "active_collection": cfg.get("collection"),
        "indexed_rooms": indexed,
        "message_limit": cfg.get("message_limit"),
        "leaf_limit": cfg.get("leaf_limit"),
        "summary_enabled": bool(cfg.get("summary_enabled")),
        "rerank_model": cfg.get("rerank_model"),
        "collections": collections,
        "disclaimer": catalog.get("disclaimer"),
    }


def estimate_payload(body: dict[str, Any] | None = None) -> dict[str, Any]:
    cfg = load_config()
    catalog = room_catalog()
    req = dict(body or {})
    names = req.get("collections")
    if not names:
        names = [cfg.get("collection") or "discord"]
    if names == ["all"]:
        names = list((catalog.get("collections") or {}).keys())
    rooms: list[dict[str, Any]] = []
    for name in names:
        block = (catalog.get("collections") or {}).get(str(name)) or {}
        for room in block.get("rooms") or []:
            if req.get("indexed_only") and str(room["id"]) not in set(cfg.get("rooms") or []):
                continue
            rooms.append(room)
    leaf_limit = req.get("leaf_limit", cfg.get("leaf_limit"))
    if req.get("uncapped"):
        leaf_limit = None
    return estimate_ingest(
        rooms=rooms,
        leaf_size=int(req.get("leaf_size") or cfg.get("leaf_size") or 8),
        leaf_overlap=int(req.get("leaf_overlap") or cfg.get("leaf_overlap") or 0),
        leaf_limit=None if leaf_limit in (None, "", 0) else int(leaf_limit),
        baseline_size=int(req.get("baseline_window_size") or cfg.get("baseline_window_size") or 20),
        baseline_limit=int(req.get("baseline_window_limit") or cfg.get("baseline_window_limit") or 400),
        summary_enabled=bool(
            cfg.get("summary_enabled") if req.get("summary_enabled") is None else req.get("summary_enabled")
        ),
        document_char_limit=int(cfg.get("document_char_limit") or 4000),
        per_room_segments=True,
    )


def patch_config(updates: dict[str, Any]) -> dict[str, Any]:
    cfg = load_config()
    for key, value in updates.items():
        if key not in SAFE_CONFIG_KEYS:
            raise PlaygroundError(f"바꿀 수 없는 설정: {key}")
        if key == "rooms":
            if not isinstance(value, list) or not all(isinstance(x, str) and x.strip() for x in value):
                raise PlaygroundError("rooms는 문자열 목록이어야 한다")
            value = [x.strip() for x in value]
            collection = str(updates.get("collection") or cfg.get("collection") or "")
            if collection == "discord" or any(x.startswith("discord:") for x in value):
                merge = dict(updates.get("room_merge") if "room_merge" in updates else cfg.get("room_merge") or {})
                for src, dst in merge.items():
                    if str(src).startswith("discord:") and str(src) != str(dst):
                        raise PlaygroundError("Discord 채널은 서로 병합할 수 없다")
        if key == "models":
            from models import ModelConfigError, resolve_models, validate_models

            if not isinstance(value, dict):
                raise PlaygroundError("models는 {역할: 모델ID} 객체여야 한다")
            trial = dict(cfg)
            trial["models"] = {str(k): str(v or "") for k, v in value.items()}
            try:
                validate_models(resolve_models(trial))
            except ModelConfigError as exc:
                raise PlaygroundError(str(exc)) from exc
            value = trial["models"]
        if key == "room_merge" and isinstance(value, dict):
            for src, dst in value.items():
                if str(src).startswith("discord:") and str(src) != str(dst):
                    raise PlaygroundError("Discord 채널은 서로 병합할 수 없다")
        cfg[key] = value
    save_config(cfg)
    return {k: cfg.get(k) for k in SAFE_CONFIG_KEYS if k in cfg}


def export_bundle() -> dict[str, Any]:
    cfg = load_config()
    runtime = {}
    rt_path = DATA / "bot_runtime.json"
    if rt_path.is_file():
        runtime = json.loads(rt_path.read_text(encoding="utf-8"))
    return {
        "kind": "masamong-sandbox-lab/v1",
        "ts": time.time(),
        "config": {k: cfg.get(k) for k in SAFE_CONFIG_KEYS if k in cfg},
        "bot_runtime": runtime,
    }


def import_bundle(payload: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise PlaygroundError("가져오기 JSON이 객체가 아니다")
    cfg_part = payload.get("config") if isinstance(payload.get("config"), dict) else {}
    patched = patch_config({k: v for k, v in cfg_part.items() if k in SAFE_CONFIG_KEYS})
    runtime = payload.get("bot_runtime")
    if isinstance(runtime, dict) and runtime:
        DATA.mkdir(parents=True, exist_ok=True)
        (DATA / "bot_runtime.json").write_text(
            json.dumps(runtime, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
    return {"ok": True, "config": patched}
