"""Sandbox engine, index half: config, paths, ledger, prepare_index and load_index (re-exported by engine.py)."""

from __future__ import annotations

import json
import os
import tempfile
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np


import sandbox_env
from engine_context import EngineContext, SandboxPaths
from timeparse import parse_epoch

sandbox_env.ensure()
ROOT = sandbox_env.ROOT
EVAL = sandbox_env.EVAL

from corpus import iter_observations  # noqa: E402
from lexical import (  # noqa: E402
    LexicalIndex,
    VARIANTS,
)
from client import (  # noqa: E402
    ClientError,
    PlayClient,
    load_secrets,
)
from segment import (  # noqa: E402
    collect_messages,
    slice_segments_per_room,
)
from models import (  # noqa: E402
    ModelConfigError,
    resolve_models,
    validate_models,
)
from memory_blocks import (  # noqa: E402
    catalog_from_leaves,
    segmentation_version_from_meta,
    space_from_index,
    write_catalog,
)
from retrieval_config import retrieval_config  # noqa: E402
from message_vectors import (  # noqa: E402
    MESSAGE_LEAF_NAME,
    MESSAGE_META_NAME,
    MESSAGE_SCALE_NAME,
    MESSAGE_VECS_NAME,
    fit_message_vectors,
    embed_full_width,
    load_message_vectors,
    message_dims,
    message_fingerprint,
    select_message_rows,
    write_message_vectors,
)
from query_embed import QueryEmbedder  # noqa: E402
from sandbox_paths import (  # noqa: E402
    load_local_config,
    local_config_path,
    resolve_sandbox_data_dir,
)
from prepare_progress import (  # noqa: E402
    idle_payload,
    mark_phase_start,
    phase_started,
    print_cli,
    read_progress,
    reset_embed_counts,
    set_embed_count,
    snapshot,
    write_progress,
)

HERE = Path(__file__).resolve().parent

_CTX = EngineContext(SandboxPaths.under(HERE / "data"))


def context() -> EngineContext:
    """The paths in use and the prepare state; read it when you need it, do not keep the paths."""
    return _CTX


def _bind_data_dir(data: Path) -> None:
    """RAG-ified artifacts live under test-sandbox/data, not the product DB."""
    _CTX.paths = SandboxPaths.under(data)


_bind_data_dir(resolve_sandbox_data_dir(load_local_config()))


SUMMARY_PROMPT = """다음 카카오톡 대화 조각을 한국어로 1~2문장만 요약하라.
고유명사·사건·약속·결정은 남기고 군더더기는 빼라. 없는 내용을 추측하지 마라.
요약문만 출력하라.

{body}"""


class ThreadSafeLexicalIndex(LexicalIndex):
    def __init__(self, variant: str) -> None:
        if variant not in VARIANTS:
            raise ValueError(f"알 수 없는 tokenizer 변형: {variant}")
        import sqlite3

        self.variant = variant
        self.spec = VARIANTS[variant]
        self.db = sqlite3.connect(":memory:", check_same_thread=False)
        self.db.execute(
            f"CREATE VIRTUAL TABLE docs USING fts5(doc_id UNINDEXED, body, "
            f"tokenize='{self.spec['tokenize']}')"
        )


class PlaygroundError(RuntimeError):
    pass


def load_config() -> dict[str, Any]:
    return json.loads(local_config_path().read_text(encoding="utf-8"))


def _maybe_rebind_data_from_env(cfg: dict[str, Any]) -> None:
    """Rebind only when an explicit override is set so tests can keep a temp data dir."""

    if str(os.environ.get("MASAMONG_SANDBOX_DATA") or "").strip() or str(
        os.environ.get("MASAMONG_SANDBOX_CONFIG") or ""
    ).strip():
        _bind_data_dir(resolve_sandbox_data_dir(cfg))


def load_ledger() -> dict[str, float]:
    with _CTX.ledger_lock:
        if not _CTX.paths.ledger.exists():
            return {"spent_usd": 0.0}
        raw = json.loads(_CTX.paths.ledger.read_text(encoding="utf-8"))
    return {"spent_usd": float(raw.get("spent_usd", 0.0))}


def save_ledger(spent_usd: float) -> None:
    _CTX.paths.data.mkdir(parents=True, exist_ok=True)
    with _CTX.ledger_lock:
        existing: dict[str, Any] = {}
        if _CTX.paths.ledger.exists():
            try:
                row = json.loads(_CTX.paths.ledger.read_text(encoding="utf-8"))
                if isinstance(row, dict):
                    existing = row
            except (OSError, json.JSONDecodeError, TypeError):
                existing = {}
        existing["spent_usd"] = round(float(spent_usd), 6)
        _atomic_write_json(_CTX.paths.ledger, existing)


def apply_client_usage(
    client: PlayClient,
    *,
    cursor: int = 0,
    extra_usd: float = 0.0,
) -> dict[str, Any]:
    """Persist settled client usage exactly once per attempt_id. Unknown cost is kept, never zeroed."""

    with _CTX.ledger_lock:
        existing: dict[str, Any] = {"spent_usd": 0.0, "unknown_cost_usd": 0.0, "attempt_ids": []}
        if _CTX.paths.ledger.exists():
            try:
                row = json.loads(_CTX.paths.ledger.read_text(encoding="utf-8"))
                if isinstance(row, dict):
                    existing.update(row)
            except (OSError, json.JSONDecodeError, TypeError):
                pass
        seen = {str(x) for x in (existing.get("attempt_ids") or []) if str(x)}
        delta = client.usage.delta_since(cursor)
        added = 0.0
        unknown = float(existing.get("unknown_cost_usd") or 0.0)
        ids = [str(x) for x in (existing.get("attempt_ids") or []) if str(x)]
        for ev, aid in zip(delta.get("events") or [], delta.get("attempt_ids") or []):
            if not aid or aid in seen:
                continue
            cost = float(ev.get("cost") or 0.0)
            added += cost
            if ev.get("cost_basis") == "reservation":
                unknown += cost
            seen.add(aid)
            ids.append(aid)
        extra = max(0.0, float(extra_usd or 0.0))
        total = float(existing.get("spent_usd") or 0.0) + added + extra
        existing["spent_usd"] = round(total, 6)
        existing["unknown_cost_usd"] = round(unknown, 6)
        existing["attempt_ids"] = ids[-500:]
        _CTX.paths.data.mkdir(parents=True, exist_ok=True)
        _atomic_write_json(_CTX.paths.ledger, existing)
        return {
            "spent_usd": round(added + extra, 6),
            "session_spent_usd": existing["spent_usd"],
            "cursor": int(delta.get("cursor") or cursor),
            "unknown_cost_usd": existing["unknown_cost_usd"],
        }


def remaining_session_budget(cfg: dict[str, Any]) -> float:
    return max(0.0, float(cfg["session_budget_usd"]) - load_ledger()["spent_usd"])


def _elapsed_ms(started: float) -> float:
    return round((time.perf_counter() - started) * 1000.0, 1)


def _compact_timing(*parts: dict[str, Any]) -> dict[str, float]:
    out: dict[str, float] = {}
    for part in parts:
        for key, raw in part.items():
            if not str(key).endswith("_ms") or str(key) == "retrieve_ms":
                continue
            try:
                value = float(raw)
            except (TypeError, ValueError):
                continue
            if value >= 0:
                out[str(key)] = round(value, 1)
    if out:
        known = ("plan_ms", "tools_ms", "embed_ms", "hybrid_ms", "rerank_ms", "expand_ms", "generate_ms")
        out["total_ms"] = round(sum(out.get(k, 0.0) for k in known), 1)
    return out


def _secrets() -> dict[str, str]:
    try:
        return load_secrets(Path(os.environ["EVAL_SECRETS"]))
    except (OSError, KeyError, ValueError):
        return {}


def configured_models(cfg: dict[str, Any]) -> dict[str, str]:
    """config.json `models` > secrets EVAL_*_MODEL > 기본값. 형식·역할 검사에 실패하면 PlaygroundError."""

    models = resolve_models(cfg, _secrets())
    try:
        validate_models(models)
    except ModelConfigError as exc:
        raise PlaygroundError(str(exc)) from exc
    return models


def make_client(cfg: dict[str, Any], *, budget_usd: float) -> PlayClient:
    models = configured_models(cfg)
    return PlayClient(
        budget_usd=budget_usd,
        secrets=load_secrets(Path(os.environ["EVAL_SECRETS"])),
        max_workers=int(cfg.get("api_workers", 8)),
        llm_model=models["response"],
        embed_model=models["embed"],
    )


def _room_observations(cfg: dict[str, Any]):
    rooms = set(cfg["rooms"])
    return (
        obs
        for obs in iter_observations(cfg["collection"], kind="dated", rooms=rooms)
        if obs.semantic_eligible
    )


def index_fingerprint(cfg: dict[str, Any]) -> dict[str, Any]:
    return {
        "experiment": cfg.get("experiment"),
        "collection": cfg["collection"],
        "rooms": cfg["rooms"],
        "room_merge": cfg.get("room_merge") or {},
        "message_limit": cfg.get("message_limit"),
        "leaf_size": cfg.get("leaf_size"),
        "leaf_overlap": cfg.get("leaf_overlap"),
        "leaf_limit": cfg.get("leaf_limit"),
        "leaf_split": cfg.get("leaf_split") or "fixed",
        "gap_minutes": cfg.get("gap_minutes"),
        "baseline_enabled": bool(cfg.get("baseline_enabled", False)),
        "baseline_window_size": cfg.get("baseline_window_size"),
        "baseline_window_limit": cfg.get("baseline_window_limit"),
        "context_prefix": cfg.get("context_prefix"),
        "summary_enabled": bool(cfg.get("summary_enabled", False)),
        "lexical": cfg["lexical"],
        "document_char_limit": cfg["document_char_limit"],
    }


def _atomic_write_json(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=False, indent=2))
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _load_vecs(path: Path) -> np.ndarray:
    return np.asarray(np.load(path), dtype=np.float32)


def _report_prepare(
    *,
    phase: str,
    done: int,
    total: int,
    status: str = "running",
    detail: str = "",
    force: bool = False,
) -> None:
    payload = snapshot(
        phase=phase,
        done=done,
        total=total,
        status=status,
        detail=detail,
        cost_usd=_CTX.prepare.cost_usd,
        summary_enabled=_CTX.prepare.summary_on,
        started_at=phase_started(phase),
    )
    wrote = write_progress(_CTX.paths.progress, payload, force=force or status != "running")
    if wrote:
        print_cli(payload)


def _note_prepare_cost(client: PlayClient) -> None:
    try:
        _CTX.prepare.cost_usd = float(client.usage.cost_usd)
    except (TypeError, ValueError, AttributeError):
        return


def prepare_status_payload() -> dict[str, Any]:
    row = read_progress(_CTX.paths.progress)
    if row:
        return row
    ready = _CTX.paths.meta.is_file() and _CTX.paths.leaves.is_file() and _CTX.paths.leaf_vecs.is_file()
    return idle_payload(ready=ready)


def _load_ckpt_state(wanted: dict[str, Any]) -> dict[str, Any] | None:
    if not _CTX.paths.ckpt_state.exists():
        return None
    try:
        state = json.loads(_CTX.paths.ckpt_state.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    if state.get("fingerprint") != wanted:
        return None
    return state


def _save_ckpt_state(state: dict[str, Any]) -> None:
    _CTX.paths.ckpt_dir.mkdir(parents=True, exist_ok=True)
    _atomic_write_json(_CTX.paths.ckpt_state, state)


def _clear_checkpoint() -> None:
    if not _CTX.paths.ckpt_dir.exists():
        return
    for path in _CTX.paths.ckpt_dir.iterdir():
        if path.is_file():
            path.unlink(missing_ok=True)


def _normalize_summary_text(text: str, *, leaf_body: str) -> str:
    summary = (text or "").strip().replace("\n", " ")
    if len(summary) > 400:
        summary = summary[:400].rstrip() + "…"
    if not summary:
        summary = leaf_body[:180]
    return summary


def _summarize_leaves(
    client: PlayClient,
    leaves: list[dict[str, Any]],
    *,
    max_tokens: int,
    char_limit: int,
    fingerprint: dict[str, Any],
    existing: dict[str, dict[str, Any]] | None = None,
    flush_every: int = 5,
) -> list[dict[str, Any]]:
    """leaf 요약. existing(doc_id→row)는 건너뛰고, 진행분을 주기적으로 저장한다."""

    from concurrent.futures import ThreadPoolExecutor, as_completed

    by_id: dict[str, dict[str, Any]] = dict(existing or {})
    pending = [leaf for leaf in leaves if leaf["doc_id"] not in by_id]
    print(f"요약: 완료 {len(by_id)} / 남음 {len(pending)} (전체 {len(leaves)})")
    mark_phase_start("summaries", already=len(by_id))
    _note_prepare_cost(client)
    _report_prepare(phase="summaries", done=len(by_id), total=len(leaves), force=True)
    if not pending:
        return [by_id[leaf["doc_id"]] for leaf in leaves]

    lock = threading.Lock()
    done_since_flush = 0

    def _flush() -> None:
        rows = [by_id[leaf["doc_id"]] for leaf in leaves if leaf["doc_id"] in by_id]
        _atomic_write_json(_CTX.paths.summaries, rows)
        state = _load_ckpt_state(fingerprint) or {"fingerprint": fingerprint}
        state["fingerprint"] = fingerprint
        state["phase"] = "summaries"
        state["summary_done"] = sorted(by_id.keys())
        state["summary_count"] = len(by_id)
        _save_ckpt_state(state)

    summary_model = configured_models(load_config())["summary"]

    def _one(leaf: dict[str, Any]) -> tuple[str, dict[str, Any]]:
        try:
            raw = client.chat(
                [
                    {
                        "role": "user",
                        "content": SUMMARY_PROMPT.format(
                            body=leaf["body"][: min(char_limit, 2800)]
                        ),
                    }
                ],
                max_tokens=max_tokens,
                temperature=0.1,
                reasoning="none",  # thinking would eat the short summary budget
                model=summary_model,
            )
        except ClientError as exc:
            if "비었다" not in str(exc):
                raise
            raw = ""
        summary = _normalize_summary_text(raw, leaf_body=leaf["body"])
        row = {
            "doc_id": leaf["doc_id"],
            "kind": "leaf_summary",
            "summary": summary,
            "body": summary,
            "source_room_id": leaf.get("source_room_id"),
            "observation_ids": list(leaf.get("observation_ids") or []),
        }
        return leaf["doc_id"], row

    workers = max(1, min(int(client.max_workers), len(pending)))
    with ThreadPoolExecutor(max_workers=workers) as pool:
        futures = {pool.submit(_one, leaf): leaf["doc_id"] for leaf in pending}
        for fut in as_completed(futures):
            doc_id, row = fut.result()
            with lock:
                by_id[doc_id] = row
                done_since_flush += 1
                _note_prepare_cost(client)
                _report_prepare(
                    phase="summaries",
                    done=len(by_id),
                    total=len(leaves),
                    force=done_since_flush >= flush_every,
                )
                if done_since_flush >= flush_every:
                    _flush()
                    done_since_flush = 0
    _flush()
    missing = [leaf["doc_id"] for leaf in leaves if leaf["doc_id"] not in by_id]
    if missing:
        raise PlaygroundError(f"요약 누락 {len(missing)}개")
    return [by_id[leaf["doc_id"]] for leaf in leaves]


def _embed_texts(
    client: PlayClient,
    texts: list[str],
    *,
    name: str,
    final_path: Path,
    batch_size: int = 32,
    resume: bool = True,
) -> np.ndarray:
    """임베딩. 완료분은 ckpt partial에 두고 재시작 시 이어서 한다."""

    if not texts:
        return np.zeros((0, 0), dtype=np.float32)

    if resume and final_path.exists():
        arr = _load_vecs(final_path)
        if len(arr) == len(texts):
            print(f"임베딩 reuse {name}: {len(texts)}")
            done, total = set_embed_count(name, len(texts), len(texts))
            _note_prepare_cost(client)
            _report_prepare(phase="embeddings", done=done, total=total, detail=name)
            return np.asarray(arr, dtype=np.float32)

    _CTX.paths.ckpt_dir.mkdir(parents=True, exist_ok=True)
    partial_path = _CTX.paths.ckpt_dir / f"{name}_vecs.partial.npy"
    meta_path = _CTX.paths.ckpt_dir / f"{name}_embed.json"
    start = 0
    parts: list[np.ndarray] = []
    if resume and partial_path.exists() and meta_path.exists():
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            done = int(meta.get("done", 0))
            arr = np.load(partial_path)
            if done > 0 and len(arr) == done and done <= len(texts):
                parts.append(np.asarray(arr, dtype=np.float32))
                start = done
                print(f"임베딩 resume {name}: {start}/{len(texts)}")
                done, total = set_embed_count(name, start, len(texts))
                _note_prepare_cost(client)
                _report_prepare(phase="embeddings", done=done, total=total, detail=name)
        except (OSError, json.JSONDecodeError, ValueError):
            start = 0
            parts = []

    batch_size = max(1, int(batch_size))
    workers = max(1, int(client.max_workers))
    cursor = start
    while cursor < len(texts):
        wave = texts[cursor : cursor + batch_size * workers]
        batches = [wave[i : i + batch_size] for i in range(0, len(wave), batch_size)]

        def _one(batch: list[str]) -> list[list[float]]:
            return client.embed(batch)

        from client import map_parallel

        vec_lists = map_parallel(batches, _one, workers=min(workers, len(batches)))
        flat: list[list[float]] = []
        for part in vec_lists:
            flat.extend(part)
        chunk = np.asarray(flat, dtype=np.float32)
        chunk /= np.linalg.norm(chunk, axis=1, keepdims=True) + 1e-9
        parts.append(chunk)
        cursor += len(wave)
        combined = np.concatenate(parts, axis=0) if len(parts) > 1 else parts[0]
        np.save(partial_path, combined)
        _atomic_write_json(
            meta_path,
            {"name": name, "done": int(combined.shape[0]), "total": len(texts)},
        )
        print(f"  임베딩 체크포인트 {name}: {combined.shape[0]}/{len(texts)}")
        done, total = set_embed_count(name, int(combined.shape[0]), len(texts))
        _note_prepare_cost(client)
        _report_prepare(phase="embeddings", done=done, total=total, detail=name, force=True)

    combined = parts[0] if len(parts) == 1 else np.concatenate(parts, axis=0)
    if len(combined) != len(texts):
        raise PlaygroundError(
            f"임베딩 길이 불일치 {name}: {len(combined)} != {len(texts)}"
        )
    np.save(final_path, combined.astype(np.float16, copy=False))
    partial_path.unlink(missing_ok=True)
    meta_path.unlink(missing_ok=True)
    return combined


def _message_fingerprint_for(
    cfg: dict[str, Any], embed_model: str, messages: list[dict[str, Any]], leaves: list[dict[str, Any]]
) -> tuple[dict[str, Any], list[tuple[str, str]], list[tuple[str, int]]]:
    observations, pairs = select_message_rows(messages, leaves)
    truncatable = bool(cfg["embed_truncatable"]) if "embed_truncatable" in cfg else embed_full_width(embed_model) > 0
    dims = int(cfg.get("embed_dimension") or 0)
    if not truncatable:
        # Match the query's stored leaf space; mmap reads only the array header for its width.
        dims = 0
        try:
            leaf_vecs = np.load(_CTX.paths.data / "leaf_vecs.npy", mmap_mode="r", allow_pickle=False)
            if leaf_vecs.ndim == 2 and leaf_vecs.shape[0] > 0:
                dims = int(leaf_vecs.shape[1])
        except (OSError, ValueError):
            pass
    fingerprint = message_fingerprint(
        embed_model=embed_model,
        dims=message_dims(
            dims,
            embed_truncatable=truncatable,
            override=int(cfg.get("message_dims") or 0),
        ),
        observations=observations,
        pairs=pairs,
        leaf_ids=[str(d.get("doc_id")) for d in leaves],
    )
    return fingerprint, observations, pairs


_MESSAGE_CKPT_FILES = ("message_vecs_full.npy", "message_vecs.partial.npy", "message_embed.json")


def _align_message_checkpoint(fingerprint: dict[str, Any]) -> None:
    """The partial and finished embeddings of an interrupted message run are kept only while they carry the
    fingerprint of the run that wrote them (model, dimensionality, texts in canonical order, projection).
    Anything else is dropped, so a changed index never resumes from rows of another one."""

    fp_path = _CTX.paths.ckpt_dir / "message_fingerprint.json"
    try:
        same = json.loads(fp_path.read_text(encoding="utf-8")) == fingerprint
    except (OSError, ValueError):
        same = False
    _CTX.paths.ckpt_dir.mkdir(parents=True, exist_ok=True)
    if not same:
        for name in _MESSAGE_CKPT_FILES:
            (_CTX.paths.ckpt_dir / name).unlink(missing_ok=True)
        _atomic_write_json(fp_path, fingerprint)


def prepare_message_vectors(
    cfg: dict[str, Any], messages: list[dict[str, Any]], leaves: list[dict[str, Any]], client: PlayClient
) -> dict[str, Any]:
    """retrieval.message_dense: embed every eligible message once (MRL defaults to at most 512 dimensions;
    non-MRL models use the stored leaf width, with an optional measurement override through 1024, and
    re-normalised) and write message_vecs.npy + message_leaf.npy: one row per (message, leaf) pair in the
    canonical order, so a message in several leaves is embedded once and projected onto each. Only runs
    when the flag is on. Artifacts and checkpoints carry a fingerprint (model, dimensions, texts,
    projection); a run whose fingerprint equals the stored one is reused, so a second prepare costs
    nothing, and a changed index never reuses either. Returns a small report for the prepare result."""

    fingerprint, observations, pairs = _message_fingerprint_for(cfg, client.embed_model, messages, leaves)
    if not pairs or fingerprint["dims"] <= 0:
        return {"message_vectors": 0, "message_rows": 0, "reused": False}
    leaf_rows = [li for _, li in pairs]
    report = {"message_vectors": len(observations), "message_rows": len(pairs)}
    if (
        (_CTX.paths.data / MESSAGE_VECS_NAME).exists()
        and (_CTX.paths.data / MESSAGE_LEAF_NAME).exists()
        and load_message_vectors(_CTX.paths.data, fingerprint=fingerprint, leaf_rows=leaf_rows) is not None
    ):
        return {**report, "reused": True}
    _align_message_checkpoint(fingerprint)
    full_path = _CTX.paths.ckpt_dir / "message_vecs_full.npy"
    _report_prepare(phase="embeddings", done=0, total=1, detail="messages", force=True)
    full = _embed_texts(client, [text for _, text in observations], name="message", final_path=full_path, resume=True)
    truncatable = bool(cfg["embed_truncatable"]) if "embed_truncatable" in cfg else embed_full_width(client.embed_model) > 0
    if not truncatable and full.shape[1] < fingerprint["dims"]:
        raise PlaygroundError("메시지 임베딩 폭이 leaf 폭보다 작습니다")
    row_of = {oid: i for i, (oid, _) in enumerate(observations)}
    vecs = fit_message_vectors(full, fingerprint["dims"])[[row_of[oid] for oid, _ in pairs]]
    write_message_vectors(_CTX.paths.data, vecs, leaf_rows, meta={"fingerprint": fingerprint, "count": len(pairs)})
    full_path.unlink(missing_ok=True)
    return {**report, "reused": False}


def _settle_prepare(
    client: PlayClient, meta: dict[str, Any], *, base_cost: float, clear: bool, usage_key: str = "prepare_usage"
) -> None:
    """The one place a prepare run is closed: the metadata records what this client spent (on top of
    base_cost, already recorded for an earlier run), the persistent ledger takes it exactly once, the
    checkpoint is cleared when the run finished, and the progress shows the cost. Both the full prepare
    and the message-only pass over an index that was already prepared end here."""

    meta["prepare_cost_usd"] = round(base_cost + float(client.usage.cost_usd), 6)
    meta[usage_key] = client.usage.summary()
    _CTX.paths.meta.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    save_ledger(load_ledger()["spent_usd"] + float(client.usage.cost_usd))
    if clear:
        _clear_checkpoint()
    _note_prepare_cost(client)


def _prepare_messages_only(cfg: dict[str, Any], meta: dict[str, Any], leaves: list[dict[str, Any]]) -> None:
    """The message vectors over a leaf index that is already prepared. The pass has a client of its own, so
    it ends in the same place as a full prepare: the metadata gets the cost on top of what the earlier run
    recorded, the ledger takes it exactly once (also when the pass fails or stops on the budget), and the
    checkpoint is cleared only when the pass finished."""

    client = make_client(cfg, budget_usd=float(cfg["prepare_budget_usd"]))
    prior = float(meta.get("prepare_cost_usd") or 0.0)
    try:
        report = prepare_message_vectors(cfg, json.loads(_CTX.paths.messages.read_text(encoding="utf-8")), leaves, client)
    except BaseException:
        _settle_prepare(client, meta, base_cost=prior, clear=False, usage_key="message_prepare_usage")
        raise
    meta["message_vectors"] = report["message_vectors"]
    _settle_prepare(client, meta, base_cost=prior, clear=True, usage_key="message_prepare_usage")


def prepare_index(*, force: bool = False) -> dict[str, Any]:
    cfg = load_config()
    _bind_data_dir(resolve_sandbox_data_dir(cfg))
    _CTX.paths.data.mkdir(parents=True, exist_ok=True)
    wanted = index_fingerprint(cfg)
    from index_artifacts import (
        wanted_layers,
        write_manifest,
        MANIFEST_NAME,
        load_manifest,
        decide_reuse,
        segmentation_matches_leaves,
    )

    summary_on = bool(cfg.get("summary_enabled", False))
    baseline_on = bool(cfg.get("baseline_enabled", False))
    _CTX.prepare.summary_on = summary_on
    _CTX.prepare.cost_usd = None
    ready = (
        _CTX.paths.meta.exists()
        and _CTX.paths.messages.exists()
        and _CTX.paths.leaves.exists()
        and _CTX.paths.leaf_vecs.exists()
        and (not baseline_on or (_CTX.paths.baseline.exists() and _CTX.paths.baseline_vecs.exists()))
        and (not summary_on or (_CTX.paths.summaries.exists() and _CTX.paths.summary_vecs.exists()))
    )
    layers = wanted_layers(cfg)
    stored_fp = None
    if _CTX.paths.meta.exists():
        try:
            stored_fp = json.loads(_CTX.paths.meta.read_text(encoding="utf-8")).get("index_fingerprint")
        except (OSError, json.JSONDecodeError):
            stored_fp = None
    ckpt_fp = None
    if _CTX.paths.ckpt_state.exists():
        try:
            ckpt_fp = json.loads(_CTX.paths.ckpt_state.read_text(encoding="utf-8")).get("fingerprint")
        except (OSError, json.JSONDecodeError):
            ckpt_fp = None
    on_disk_fp = stored_fp if stored_fp is not None else ckpt_fp
    existing_manifest = load_manifest(_CTX.paths.data / MANIFEST_NAME)
    reuse_plan = decide_reuse(existing_manifest, layers, files_ready=ready)
    if ready and not force:
        meta = json.loads(_CTX.paths.meta.read_text(encoding="utf-8"))
        leaves_on_disk: list[Any] = []
        try:
            leaves_on_disk = json.loads(_CTX.paths.leaves.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, TypeError):
            leaves_on_disk = []
        spans_ok = segmentation_matches_leaves(leaves_on_disk, cfg) if isinstance(leaves_on_disk, list) else False
        if meta.get("index_fingerprint") == wanted and reuse_plan.reason == "match" and spans_ok:
            if bool(retrieval_config(cfg)["message_dense"].get("enabled", False)):
                # artifacts of the leaf index are current; the optional message vectors may not exist yet
                _prepare_messages_only(cfg, meta, leaves_on_disk)
            _report_prepare(
                phase="done",
                done=1,
                total=1,
                status="ready",
                detail="이미 준비됨",
                force=True,
            )
            return {"status": "ready", "reused": True, "resumed": False, "meta": meta}
        if meta.get("index_fingerprint") == wanted and not spans_ok:
            raise PlaygroundError(
                "저장된 leaf 크기가 현재 설정과 다르다. 기존 산출물을 새 recipe로 쓰지 않는다. prepare.py --force"
            )
        if meta.get("index_fingerprint") == wanted and reuse_plan.reason not in {"match", "missing_manifest_or_files"}:
            raise PlaygroundError(
                f"인덱스 재사용 불가 ({reuse_plan.reason}). 자동 재생성하지 않는다. prepare.py --force"
            )
        if (
            meta.get("index_fingerprint") == wanted
            and reuse_plan.reason == "missing_manifest_or_files"
            and spans_ok
        ):
            write_manifest(
                _CTX.paths.data / MANIFEST_NAME,
                layers,
                extra={"status": "bootstrapped", "leaf_count": len(leaves_on_disk)},
            )
            _report_prepare(
                phase="done",
                done=1,
                total=1,
                status="ready",
                detail="이미 준비됨(manifest 부트스트랩)",
                force=True,
            )
            return {"status": "ready", "reused": True, "resumed": False, "meta": meta, "manifest": "bootstrapped"}

    if force:
        print("force: 체크포인트·기존 산출물 재생성")
        _clear_checkpoint()
        for path in (
            _CTX.paths.leaf_vecs,
            _CTX.paths.baseline_vecs,
            _CTX.paths.summary_vecs,
            _CTX.paths.summaries,
            _CTX.paths.meta,
            _CTX.paths.data / MESSAGE_VECS_NAME,
            _CTX.paths.data / MESSAGE_LEAF_NAME,
            _CTX.paths.data / MESSAGE_SCALE_NAME,
            _CTX.paths.data / MESSAGE_META_NAME,
        ):
            path.unlink(missing_ok=True)
        resumed = False
    else:
        ckpt = _load_ckpt_state(wanted)
        resumed = bool(ckpt) or (
            _CTX.paths.summaries.exists() and not (_CTX.paths.meta.exists() and ready)
        )
        if ckpt:
            print(f"체크포인트 재개 phase={ckpt.get('phase')} summaries={ckpt.get('summary_count', 0)}")
        else:
            # fingerprint가 바뀌었으면 옛 ckpt 폐기
            if _CTX.paths.ckpt_state.exists():
                print("fingerprint 변경 → 체크포인트 초기화")
                _clear_checkpoint()
            resumed = False

    state = _load_ckpt_state(wanted) or {
        "fingerprint": wanted,
        "phase": "segments",
        "summary_done": [],
        "summary_count": 0,
        "reuse": reuse_plan.as_dict(),
    }
    # Write the *wanted* fingerprint only after we have decided reuse from on-disk state.
    if state.get("fingerprint") != wanted:
        state["fingerprint"] = wanted
    _save_ckpt_state(state)

    mark_phase_start("segments")
    _report_prepare(phase="segments", done=0, total=1, detail="원문·문맥 조각", force=True)

    # 세그먼트는 디스크에 있는 실제 recipe와 wanted가 같을 때만 재사용한다 (ckpt를 먼저 갱신하지 않음).
    can_reuse_segments = (
        not force
        and _CTX.paths.messages.exists()
        and _CTX.paths.leaves.exists()
        and (not baseline_on or _CTX.paths.baseline.exists())
        and reuse_plan.segments
        and on_disk_fp == wanted
    )
    if can_reuse_segments:
        messages = json.loads(_CTX.paths.messages.read_text(encoding="utf-8"))
        leaves = json.loads(_CTX.paths.leaves.read_text(encoding="utf-8"))
        if baseline_on and _CTX.paths.baseline.exists():
            baseline = json.loads(_CTX.paths.baseline.read_text(encoding="utf-8"))
        else:
            baseline = []
        print(
            f"세그먼트 reuse msgs={len(messages)} leaves={len(leaves)} baseline={len(baseline)}"
        )
    else:
        messages = collect_messages(
            _room_observations(cfg),
            limit=int(cfg["message_limit"]),
            room_merge=cfg.get("room_merge") or {},
        )
        if len(messages) < int(cfg["leaf_size"]):
            raise PlaygroundError("메시지 풀이 leaf_size보다 작다")

        prefix = bool(cfg.get("context_prefix", True))
        split = str(cfg.get("leaf_split") or "fixed")
        gap_minutes = float(cfg.get("gap_minutes") or 30)
        leaves = slice_segments_per_room(
            messages,
            size=int(cfg["leaf_size"]),
            overlap=int(cfg.get("leaf_overlap", 0)),
            limit=int(cfg["leaf_limit"]),
            kind="leaf",
            with_prefix=prefix,
            split=split,
            gap_minutes=gap_minutes,
        )
        baseline = []
        if baseline_on:
            baseline = slice_segments_per_room(
                messages,
                size=int(cfg["baseline_window_size"]),
                overlap=0,
                limit=int(cfg["baseline_window_limit"]),
                kind="baseline",
                with_prefix=prefix,
                split=split,
                gap_minutes=gap_minutes,
            )
        if not leaves or (baseline_on and not baseline):
            raise PlaygroundError("leaf/baseline 세그먼트를 만들지 못했다")
        _CTX.paths.messages.write_text(json.dumps(messages, ensure_ascii=False), encoding="utf-8")
        _CTX.paths.leaves.write_text(json.dumps(leaves, ensure_ascii=False), encoding="utf-8")
        if baseline_on:
            _CTX.paths.baseline.write_text(json.dumps(baseline, ensure_ascii=False), encoding="utf-8")
        state["phase"] = "segments_done"
        _save_ckpt_state(state)

    _report_prepare(
        phase="segments",
        done=1,
        total=1,
        detail=f"메시지 {len(messages):,} · leaf {len(leaves):,}",
        force=True,
    )

    prefix = bool(cfg.get("context_prefix", True))
    client = make_client(cfg, budget_usd=float(cfg["prepare_budget_usd"]))
    char_limit = int(cfg["document_char_limit"])
    leaf_texts = [d["body"][:char_limit] for d in leaves]
    baseline_texts = [d["body"][:char_limit] for d in baseline] if baseline else []

    summaries: list[dict[str, Any]] = []
    if summary_on:
        existing: dict[str, dict[str, Any]] = {}
        if not force and _CTX.paths.summaries.exists():
            try:
                for row in json.loads(_CTX.paths.summaries.read_text(encoding="utf-8")):
                    if isinstance(row, dict) and row.get("doc_id") and row.get("summary"):
                        existing[str(row["doc_id"])] = row
            except (OSError, json.JSONDecodeError):
                existing = {}
        # fingerprint 불일치 분 제거: leaf id 집합과 교집합만
        leaf_ids = {d["doc_id"] for d in leaves}
        existing = {k: v for k, v in existing.items() if k in leaf_ids}
        state["phase"] = "summaries"
        _save_ckpt_state(state)
        print(f"요약 생성… workers={cfg.get('api_workers', 20)}")
        summaries = _summarize_leaves(
            client,
            leaves,
            max_tokens=int(cfg.get("summary_max_tokens", 100)),
            char_limit=char_limit,
            fingerprint=wanted,
            existing=existing,
        )

    state["phase"] = "embeddings"
    _save_ckpt_state(state)
    reset_embed_counts()
    mark_phase_start("embeddings")
    _report_prepare(phase="embeddings", done=0, total=1, detail="시작", force=True)

    from concurrent.futures import ThreadPoolExecutor

    embed_specs: list[tuple[str, list[str], Path]] = [
        ("leaf", leaf_texts, _CTX.paths.leaf_vecs),
    ]
    if baseline_texts:
        embed_specs.append(("baseline", baseline_texts, _CTX.paths.baseline_vecs))
    if summaries:
        embed_specs.append(("summary", [s["summary"] for s in summaries], _CTX.paths.summary_vecs))

    print(f"임베딩 병렬… jobs={[name for name, _, _ in embed_specs]}")
    vectors_by_name: dict[str, np.ndarray] = {}
    resume_embed = not force
    with ThreadPoolExecutor(max_workers=len(embed_specs)) as pool:
        futs = {
            name: pool.submit(
                _embed_texts,
                client,
                texts,
                name=name,
                final_path=path,
                resume=resume_embed,
            )
            for name, texts, path in embed_specs
        }
        for name, fut in futs.items():
            vectors_by_name[name] = fut.result()

    leaf_vecs = vectors_by_name["leaf"]
    baseline_vecs = vectors_by_name.get("baseline")
    if baseline_vecs is None:
        baseline_vecs = np.zeros((0, int(leaf_vecs.shape[1]) if leaf_vecs.size else 0), dtype=np.float32)
    summary_vecs = vectors_by_name.get("summary")

    if summaries and summary_vecs is not None:
        _atomic_write_json(_CTX.paths.summaries, summaries)
        np.save(_CTX.paths.summary_vecs, summary_vecs)
    elif _CTX.paths.summaries.exists():
        _CTX.paths.summaries.unlink()
        if _CTX.paths.summary_vecs.exists():
            _CTX.paths.summary_vecs.unlink()

    meta = {
        "status": "ready",
        "experiment": cfg.get("experiment"),
        "message_count": len(messages),
        "leaf_count": len(leaves),
        "baseline_count": len(baseline),
        "summary_count": len(summaries),
        "embedding_dim": int(leaf_vecs.shape[1]),
        "prepare_usage": client.usage.summary(),
        "prepare_cost_usd": float(client.usage.cost_usd),
        "config": cfg,
        "index_fingerprint": wanted,
        "recipe": {
            "mode": "leaf_match_parent_expand_with_summary"
            if summary_on
            else "leaf_match_parent_expand",
            "status": "experimental_unconfirmed",
            "lexical": cfg["lexical"],
            "rrf_weights": cfg["rrf_weights"],
            "leaf": {
                "size": cfg["leaf_size"],
                "overlap": cfg.get("leaf_overlap", 0),
                "limit": cfg["leaf_limit"],
            },
            "parent_expand": {
                "before": cfg["parent_expand_before"],
                "after": cfg["parent_expand_after"],
            },
            "summary": {
                "enabled": summary_on,
                "count": len(summaries),
                "rrf_weight": cfg.get("summary_rrf_weight", 1.0),
            },
            "baseline": {
                "enabled": baseline_on,
                "size": cfg.get("baseline_window_size"),
                "limit": cfg.get("baseline_window_limit"),
            },
            "leaf_split": cfg.get("leaf_split") or "fixed",
            "gap_minutes": cfg.get("gap_minutes"),
            "context_prefix": prefix,
            "room_merge": cfg.get("room_merge") or {},
            "models": {"embed": client.embed_model, "response": client.llm_model, "summary": configured_models(cfg)["summary"]},
        },
    }
    write_manifest(
        _CTX.paths.data / MANIFEST_NAME,
        layers,
        extra={"leaf_count": len(leaves), "message_count": len(messages), "status": "ready"},
    )
    _CTX.paths.meta.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    if bool(retrieval_config(cfg)["message_dense"].get("enabled", False)):
        try:
            meta["message_vectors"] = prepare_message_vectors(cfg, messages, leaves, client)["message_vectors"]
        except BaseException:
            # the leaf index is complete; what the message step already spent is booked once, and its
            # checkpoint stays so a later pass resumes
            _settle_prepare(client, meta, base_cost=0.0, clear=False)
            raise
    _settle_prepare(client, meta, base_cost=0.0, clear=True)
    _report_prepare(phase="done", done=1, total=1, status="ready", force=True)
    return {"status": "ready", "reused": False, "resumed": resumed, "meta": meta}


def _parse_ts(value: Any) -> float | None:
    return parse_epoch(value)


@dataclass
class IndexBundle:
    cfg: dict[str, Any]
    messages: list[dict[str, Any]]
    leaves: list[dict[str, Any]]
    leaf_vectors: np.ndarray
    leaf_ids: list[str]
    leaf_by_id: dict[str, dict[str, Any]]
    leaf_fts: ThreadSafeLexicalIndex
    baseline: list[dict[str, Any]]
    baseline_vectors: np.ndarray
    baseline_ids: list[str]
    baseline_by_id: dict[str, dict[str, Any]]
    baseline_fts: ThreadSafeLexicalIndex
    summaries: list[dict[str, Any]]
    summary_by_id: dict[str, dict[str, Any]]
    summary_vectors: np.ndarray | None
    summary_ids: list[str]
    summary_fts: ThreadSafeLexicalIndex | None
    meta: dict[str, Any]
    leaf_end_ts: dict[str, float] = field(default_factory=dict)
    leaves_by_recency: list[str] = field(default_factory=list)
    corpus_tmax: float = 0.0
    models: dict[str, str] = field(default_factory=dict)
    lock: threading.Lock = field(default_factory=threading.Lock)
    blocks: list[dict[str, Any]] = field(default_factory=list)
    block_by_id: dict[str, dict[str, Any]] = field(default_factory=dict)
    leaf_ids_by_block: dict[str, set[str]] = field(default_factory=dict)
    block_id_by_room: dict[str, str] = field(default_factory=dict)
    leaf_ids_by_speaker: dict[str, list[str]] = field(default_factory=dict)  # person_retrieval cache
    # Runtime-only cache for derived soft-event views.  It is intentionally not
    # persisted and is keyed by the recipe plus summary inclusion, so changing
    # retrieval config rebuilds the view without touching leaf artifacts.
    soft_event_runtime_cache: dict[str, Any] = field(default_factory=dict)
    # Optional shared rerank budget ledger injected by the runner.  None keeps
    # direct tests and legacy callers on the existing per-client behavior.
    rerank_budget_ledger: Any = None
    # 질의 임베딩 캐시·선행 요청·헤지 (query_embed.py). None이면 매번 직접 호출한다.
    query_embedder: Any = None
    # Optional single-message vectors (retrieval.message_dense): one row per eligible message, and the
    # leaf row each belongs to. Loaded only when the run is enabled and the artifacts exist.
    message_vectors: np.ndarray | None = None
    message_leaf: np.ndarray | None = None
    # Lazily built runtime caches, never serialized. `_identity_for` fills
    # `_identity_table`; retrieval_post fills `_t04_msg_index_by_obs`.
    _identity_table: Any = None
    _t04_msg_index_by_obs: dict[str, int] | None = None

    @property
    def docs(self) -> list[dict[str, Any]]:
        return self.leaves


def _configured_embed_model() -> str:
    """config.json models.embed > secrets EVAL_EMBED_MODEL > 기본값 (models.resolve_models)."""

    return resolve_models(load_config(), _secrets())["embed"]


def load_index() -> IndexBundle:
    cfg = load_config()
    _maybe_rebind_data_from_env(cfg)
    needed = [
        _CTX.paths.meta,
        _CTX.paths.messages,
        _CTX.paths.leaves,
        _CTX.paths.leaf_vecs,
    ]
    if not all(p.exists() for p in needed):
        raise PlaygroundError("leaf/parent 인덱스가 없다. prepare.py --force 를 실행하라")
    meta = json.loads(_CTX.paths.meta.read_text(encoding="utf-8"))
    # 인덱스를 만든 임베딩 모델과 질의 임베딩 모델이 다르면 차원이 같아도 유사도가 무의미하다.
    indexed_model = str(((meta.get("recipe") or {}).get("models") or {}).get("embed") or "")
    wanted_model = _configured_embed_model()
    if indexed_model and indexed_model != wanted_model:
        raise PlaygroundError(
            f"인덱스 임베딩 모델({indexed_model})과 현재 설정({wanted_model})이 다르다. "
            "EVAL_EMBED_MODEL을 맞추거나 prepare.py --force 를 실행하라"
        )
    messages = json.loads(_CTX.paths.messages.read_text(encoding="utf-8"))
    leaves = json.loads(_CTX.paths.leaves.read_text(encoding="utf-8"))
    leaf_vectors = _load_vecs(_CTX.paths.leaf_vecs)
    if len(leaves) != len(leaf_vectors):
        raise PlaygroundError("세그먼트와 벡터 개수가 맞지 않는다. prepare.py --force")

    baseline: list[dict[str, Any]] = []
    baseline_vectors = np.zeros((0, int(leaf_vectors.shape[1])), dtype=np.float32)
    if _CTX.paths.baseline.exists() and _CTX.paths.baseline_vecs.exists():
        baseline = json.loads(_CTX.paths.baseline.read_text(encoding="utf-8"))
        baseline_vectors = _load_vecs(_CTX.paths.baseline_vecs)
        if len(baseline) != len(baseline_vectors):
            raise PlaygroundError("세그먼트와 벡터 개수가 맞지 않는다. prepare.py --force")

    leaf_fts = ThreadSafeLexicalIndex(str(cfg["lexical"]))
    leaf_fts.add_many([(d["doc_id"], d["body"]) for d in leaves])
    baseline_fts = ThreadSafeLexicalIndex(str(cfg["lexical"]))
    if baseline:
        baseline_fts.add_many([(d["doc_id"], d["body"]) for d in baseline])

    summaries: list[dict[str, Any]] = []
    summary_vectors: np.ndarray | None = None
    summary_fts: ThreadSafeLexicalIndex | None = None
    summary_ids: list[str] = []
    summary_by_id: dict[str, dict[str, Any]] = {}
    if bool(cfg.get("summary_enabled", False)):
        if not _CTX.paths.summaries.exists() or not _CTX.paths.summary_vecs.exists():
            raise PlaygroundError("요약 인덱스가 없다. prepare.py --force 를 실행하라")
        summaries = json.loads(_CTX.paths.summaries.read_text(encoding="utf-8"))
        summary_vectors = _load_vecs(_CTX.paths.summary_vecs)
        if len(summaries) != len(summary_vectors):
            raise PlaygroundError("요약과 벡터 개수가 맞지 않는다. prepare.py --force")
        summary_ids = [d["doc_id"] for d in summaries]
        summary_by_id = {d["doc_id"]: d for d in summaries}
        summary_fts = ThreadSafeLexicalIndex(str(cfg["lexical"]))
        summary_fts.add_many([(d["doc_id"], d["summary"]) for d in summaries])

    leaf_end_ts: dict[str, float] = {}
    corpus_tmax = 0.0
    n_msg = len(messages)
    for leaf in leaves:
        doc_id = str(leaf["doc_id"])
        end = int(leaf.get("end") or 0) - 1
        ts = None
        if 0 <= end < n_msg:
            ts = _parse_ts(messages[end].get("parsed_time"))
        if ts is None:
            start = int(leaf.get("start") or 0)
            if 0 <= start < n_msg:
                ts = _parse_ts(messages[start].get("parsed_time"))
        if ts is not None:
            leaf_end_ts[doc_id] = ts
            if ts > corpus_tmax:
                corpus_tmax = ts
    if corpus_tmax <= 0:
        corpus_tmax = datetime.now(tz=timezone.utc).timestamp()
    leaves_by_recency = sorted(
        leaf_end_ts.keys(), key=lambda doc_id: leaf_end_ts[doc_id], reverse=True
    )

    bundle = IndexBundle(
        cfg=cfg,
        messages=messages,
        leaves=leaves,
        leaf_vectors=leaf_vectors,
        leaf_ids=[d["doc_id"] for d in leaves],
        leaf_by_id={d["doc_id"]: d for d in leaves},
        leaf_fts=leaf_fts,
        baseline=baseline,
        baseline_vectors=baseline_vectors,
        baseline_ids=[d["doc_id"] for d in baseline],
        baseline_by_id={d["doc_id"]: d for d in baseline},
        baseline_fts=baseline_fts,
        summaries=summaries,
        summary_by_id=summary_by_id,
        summary_vectors=summary_vectors,
        summary_ids=summary_ids,
        summary_fts=summary_fts,
        meta=meta,
        leaf_end_ts=leaf_end_ts,
        leaves_by_recency=leaves_by_recency,
        corpus_tmax=corpus_tmax,
        models=configured_models(cfg),
        query_embedder=QueryEmbedder(cfg),
    )
    if bool(retrieval_config(cfg)["message_dense"].get("enabled", False)):
        # the artifacts are used only while their fingerprint equals the one worked out from this very
        # index (model, dimensionality, texts and leaf projection); stale ones are ignored
        fingerprint, _obs, pairs = _message_fingerprint_for(cfg, wanted_model, messages, leaves)
        loaded = load_message_vectors(_CTX.paths.data, fingerprint=fingerprint, leaf_rows=[li for _, li in pairs])
        if loaded is not None:
            bundle.message_vectors, bundle.message_leaf = loaded
    apply_vector_dims(bundle, int(cfg.get("vector_dims") or 0))
    populate_block_catalog(bundle, persist_dir=_CTX.paths.data)
    return bundle


def _truncate_rows(vecs: np.ndarray | None, dims: int) -> np.ndarray | None:
    if vecs is None or not len(vecs) or int(vecs.shape[1]) <= dims:
        return vecs
    out = np.ascontiguousarray(vecs[:, :dims], dtype=np.float32)
    out /= np.linalg.norm(out, axis=1, keepdims=True) + 1e-9
    return out


def apply_vector_dims(index: IndexBundle, dims: int) -> None:
    """Matryoshka truncation (Qwen3-Embedding supports MRL): keep the first `dims` components of every
    stored vector and re-normalise. Storage/latency experiment only — never rewrites artifacts."""

    dims = int(dims or 0)
    if dims <= 0:
        return
    index.leaf_vectors = _truncate_rows(index.leaf_vectors, dims)
    index.summary_vectors = _truncate_rows(index.summary_vectors, dims)
    index.baseline_vectors = _truncate_rows(index.baseline_vectors, dims)
    if getattr(index, "message_vectors", None) is not None:
        index.message_vectors = _truncate_rows(index.message_vectors, dims)


def populate_block_catalog(index: IndexBundle, *, persist_dir: Path | None = None) -> None:
    """Derive per-room blocks from the loaded monolith. Does not rewrite vectors."""

    dim = int(index.leaf_vectors.shape[1]) if getattr(index, "leaf_vectors", None) is not None and len(index.leaf_vectors) else 0
    space = space_from_index(index.meta, index.cfg, dim)
    seg = segmentation_version_from_meta(index.meta, index.cfg)
    collection = str((index.cfg or {}).get("collection") or "discord")
    blocks = catalog_from_leaves(
        index.leaves,
        collection=collection,
        segmentation_version=seg,
        space=space,
        messages=index.messages,
        hints={
            "person_aliases": (index.cfg or {}).get("person_aliases") or {},
            "identity_terms": (index.cfg or {}).get("identity_terms") or [],
        },
    )
    index.blocks = blocks
    index.block_by_id = {str(b["block_id"]): b for b in blocks}
    index.leaf_ids_by_block = {str(b["block_id"]): set(b.get("leaf_ids") or []) for b in blocks}
    index.block_id_by_room = {str(b["source_room"]): str(b["block_id"]) for b in blocks}
    if persist_dir is not None:
        write_catalog(persist_dir, blocks)
