"""Block IDs and channel views — same contract as contracts/memory/blocks.json."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import threading
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterable, Mapping

CONTRACT_REL = Path("contracts") / "memory" / "blocks.json"


class BlockError(ValueError):
    """Binding or identity rejected by the memory-block contract."""


def _payload(parts: Iterable[str], join: str) -> bytes:
    return join.join(str(p) for p in parts).encode("utf-8")


def embedding_space(
    provider: str,
    model: str,
    dimension: int | str,
    normalization: str = "l2",
) -> str:
    return "|".join(
        [str(provider or ""), str(model or ""), str(int(dimension or 0)), str(normalization or "l2")]
    )


def block_id(
    collection: str,
    source_room: str,
    segmentation_version: str,
    space: str,
) -> str:
    digest = hashlib.sha256(
        _payload([collection, source_room, segmentation_version, space], "\n")
    ).hexdigest()
    return digest[:16]


def scope_fingerprint(block_ids: Iterable[str], revision: int) -> str:
    ids = sorted({str(x) for x in block_ids if str(x).strip()})
    payload = "\n".join(ids + [str(int(revision))])
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def platform_of_room(source_room: str) -> str:
    raw = str(source_room or "")
    if raw.startswith("kakao:"):
        return "kakao"
    if raw.startswith("discord:"):
        return "discord"
    if raw.startswith("sandbox:"):
        return "discord"
    return "discord"


def time_semantics_of(platform: str) -> str:
    return "naive" if platform == "kakao" else "offset"


def make_view(
    channel_key: str,
    block_ids: Iterable[str],
    *,
    revision: int,
    updated_at: str = "",
    updated_by: str = "",
) -> dict[str, Any]:
    ids = sorted({str(x) for x in block_ids if str(x).strip()})
    return {
        "channel_key": str(channel_key),
        "blocks": ids,
        "revision": int(revision),
        "scope_fingerprint": scope_fingerprint(ids, int(revision)),
        "updated_at": updated_at,
        "updated_by": updated_by,
    }


def assert_same_embedding_space(blocks: Iterable[Mapping[str, Any]]) -> str:
    spaces = []
    for block in blocks:
        space = str(block.get("embedding_space") or "")
        if space:
            spaces.append(space)
    uniq = sorted(set(spaces))
    if len(uniq) > 1:
        raise BlockError("view members must share embedding_space")
    return uniq[0] if uniq else ""


def attach_block(
    view: Mapping[str, Any],
    block: Mapping[str, Any],
    bound_blocks: Iterable[Mapping[str, Any]],
) -> dict[str, Any]:
    catalog = {str(b.get("block_id")): b for b in bound_blocks}
    catalog[str(block.get("block_id"))] = dict(block)
    assert_same_embedding_space(catalog.values())
    ids = set(view.get("blocks") or [])
    ids.add(str(block["block_id"]))
    return make_view(
        str(view.get("channel_key") or ""),
        ids,
        revision=int(view.get("revision") or 0) + 1,
        updated_at=str(view.get("updated_at") or ""),
        updated_by=str(view.get("updated_by") or ""),
    )


def detach_block(view: Mapping[str, Any], block_id_value: str) -> dict[str, Any]:
    ids = [b for b in (view.get("blocks") or []) if str(b) != str(block_id_value)]
    return make_view(
        str(view.get("channel_key") or ""),
        ids,
        revision=int(view.get("revision") or 0) + 1,
        updated_at=str(view.get("updated_at") or ""),
        updated_by=str(view.get("updated_by") or ""),
    )


def assert_fingerprint(view: Mapping[str, Any]) -> None:
    expected = scope_fingerprint(view.get("blocks") or [], int(view.get("revision") or 0))
    got = str(view.get("scope_fingerprint") or "")
    if got != expected:
        raise BlockError("stale_scope")


def segmentation_version_from_meta(meta: Mapping[str, Any] | None, cfg: Mapping[str, Any] | None = None) -> str:
    recipe = (meta or {}).get("recipe") if isinstance(meta, Mapping) else {}
    if isinstance(recipe, Mapping):
        raw = str(recipe.get("segmentation_version") or "").strip()
        if raw:
            return raw
    cfg = cfg or {}
    leaf = int(cfg.get("leaf_size") or recipe.get("leaf_size") or 8) if isinstance(recipe, Mapping) else int(cfg.get("leaf_size") or 8)
    split = str(cfg.get("leaf_split") or (recipe.get("leaf_split") if isinstance(recipe, Mapping) else "") or "fixed")
    return f"leaf{leaf}-o0-lim0-prefix1-{split}"


def space_from_index(meta: Mapping[str, Any] | None, cfg: Mapping[str, Any] | None, dim: int) -> str:
    models = {}
    recipe = (meta or {}).get("recipe") if isinstance(meta, Mapping) else {}
    if isinstance(recipe, Mapping) and isinstance(recipe.get("models"), Mapping):
        models = recipe["models"]
    cfg_models = (cfg or {}).get("models") if isinstance((cfg or {}).get("models"), Mapping) else {}
    embed = str(models.get("embed") or cfg_models.get("embed") or "")
    provider = str(models.get("embed_provider") or cfg_models.get("embed_provider") or "openrouter")
    return embedding_space(provider, embed, dim, "l2")


def catalog_from_leaves(
    leaves: Iterable[Mapping[str, Any]],
    *,
    collection: str,
    segmentation_version: str,
    space: str,
    messages: Iterable[Mapping[str, Any]] | None = None,
    hints: Mapping[str, Any] | None = None,
) -> list[dict[str, Any]]:
    rooms: dict[str, dict[str, Any]] = {}
    for leaf in leaves:
        room = str(leaf.get("source_room_id") or "")
        if not room:
            continue
        rec = rooms.setdefault(
            room,
            {
                "source_room": room,
                "leaf_ids": [],
                "leaf_count": 0,
            },
        )
        rec["leaf_ids"].append(str(leaf.get("doc_id")))
        rec["leaf_count"] += 1
    msg_count: dict[str, int] = {}
    if messages is not None:
        for msg in messages:
            room = str(msg.get("source_room_id") or "")
            if room:
                msg_count[room] = msg_count.get(room, 0) + 1
    blocks: list[dict[str, Any]] = []
    for room, rec in sorted(rooms.items()):
        platform = platform_of_room(room)
        bid = block_id(collection, room, segmentation_version, space)
        blocks.append(
            {
                "block_id": bid,
                "platform": platform,
                "collection": collection,
                "source_room": room,
                "label": _label_for_room(room),
                "segmentation_version": segmentation_version,
                "embedding_space": space,
                "message_count": int(msg_count.get(room, 0)),
                "leaf_count": rec["leaf_count"],
                "leaf_ids": rec["leaf_ids"],
                "status": "ready",
                "hints": dict(hints or {}),
                "time_semantics": time_semantics_of(platform),
            }
        )
    return blocks


# 호출자가 원문 catalog에서 추가 방을 전달할 수 있다. 저장소에는 실제 방 이름을 넣지 않는다.
CORPUS_CHANNEL_ROOMS: tuple[str, ...] = ()


def display_room_label(room: str) -> str:
    raw = str(room or "")
    parts = raw.split(":")
    if len(parts) >= 3 and parts[0] == "discord":
        tail = parts[-1][-6:] if parts[-1] else "?"
        return f"#{tail}"
    if parts and parts[0] == "kakao":
        return parts[-1] or raw
    return raw[-12:] if len(raw) > 12 else raw


def _label_for_room(room: str) -> str:
    return display_room_label(room)


def index_blocks_path(data_dir: Path) -> Path:
    return Path(data_dir) / "blocks" / "index.json"


_catalog_locks_guard = threading.Lock()
_catalog_locks: dict[str, threading.Lock] = {}


@contextmanager
def _catalog_replace_lock(path: Path):
    """Serialize catalog replacement between threads and processes on Windows and Unix."""
    key = os.path.normcase(str(path.resolve()))
    with _catalog_locks_guard:
        thread_lock = _catalog_locks.setdefault(key, threading.Lock())
    lock_path = path.with_name(path.name + ".lock")
    with thread_lock:
        with lock_path.open("a+b") as handle:
            if os.fstat(handle.fileno()).st_size == 0:
                handle.write(b"\0")
                handle.flush()
            handle.seek(0)
            if os.name == "nt":
                import msvcrt

                deadline = time.monotonic() + 10
                while True:
                    try:
                        msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                        break
                    except OSError:
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(0.01)
                try:
                    yield
                finally:
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def write_catalog(data_dir: Path, blocks: list[Mapping[str, Any]]) -> Path:
    path = index_blocks_path(data_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    public = []
    for block in blocks:
        row = {k: v for k, v in dict(block).items() if k != "leaf_ids"}
        row["leaf_ids"] = list(block.get("leaf_ids") or [])
        public.append(row)
    payload = {"schema_version": 1, "blocks": public}
    # Evaluation workers can load the same index concurrently. A fixed
    # ``index.json.tmp`` lets one process rename another process's temporary
    # file, causing intermittent FileNotFoundError. Use a unique sibling temp
    # and atomic replace instead.
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=False, indent=2) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        with _catalog_replace_lock(path):
            deadline = time.monotonic() + 5
            delay = 0.01
            while True:
                try:
                    os.replace(tmp_name, path)
                    break
                except PermissionError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(delay)
                    delay = min(delay * 2, 0.25)
    except BaseException:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise
    return path


def load_catalog(data_dir: Path) -> list[dict[str, Any]]:
    path = index_blocks_path(data_dir)
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return []
    rows = data.get("blocks") if isinstance(data, dict) else None
    if not isinstance(rows, list):
        return []
    return [row for row in rows if isinstance(row, dict)]


def leaf_ids_for_blocks(blocks: Iterable[Mapping[str, Any]], block_ids: Iterable[str]) -> set[str]:
    wanted = {str(x) for x in block_ids}
    out: set[str] = set()
    for block in blocks:
        if str(block.get("block_id")) not in wanted:
            continue
        for doc_id in block.get("leaf_ids") or []:
            out.add(str(doc_id))
    return out


def repo_contract_path(start: Path) -> Path | None:
    for cand in [start, *start.parents]:
        path = cand / CONTRACT_REL
        if path.is_file():
            return path
    return None
