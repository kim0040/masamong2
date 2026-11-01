"""소규모 RAG 엔진 — leaf 매칭 후 parent 원문 확장 실험."""

from __future__ import annotations

import json
import math
import os
import re
import sys
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

import numpy as np

def _repo_root() -> Path:
    here = Path(__file__).resolve().parent
    for cand in [here, *here.parents]:
        if (cand / "tools" / "evaluation").is_dir() and (cand / "docs").is_dir():
            return cand
    return here.parents[1]


ROOT = _repo_root()
EVAL = ROOT / "tools" / "evaluation"
os.environ.setdefault("RAW_CORPUS_ROOT", str(ROOT / ".private" / "raw-corpus"))
os.environ.setdefault("EVAL_SECRETS", str(ROOT / ".private" / "eval" / "secrets.env"))
sys.path.insert(0, str(EVAL))

from corpus import iter_observations  # noqa: E402
from lexical import LexicalIndex, VARIANTS  # noqa: E402

from client import (  # noqa: E402
    DEFAULT_EMBED_QUERY_INSTRUCTION,
    ClientError,
    PlayClient,
    StreamError,
    load_secrets,
)
from retrieval_post import (  # noqa: E402
    TimeReference as RetrievalTimeReference,
    narrow_time_ids,
    resolve_time_scope,
    DENSE_LIMIT as _DEFAULT_DENSE_LIMIT,
    LEXICAL_LIMIT as _DEFAULT_LEXICAL_LIMIT,
    RERANK_MIN_SCORE,
    finalize_ranked_ids,
    merged_span,
    narrow_recent_ids,
    recency_anchor_ts,
    scaled_person,
    search_limits,
    resolve_rerank_min_score,
)
from segment import (  # noqa: E402
    collect_messages,
    expand_segment,
    slice_segments_per_room,
)
from rag_upgrade.playground_debug import (  # noqa: E402
    RankingError,
    append_debug_error,
    append_debug_turn,
    new_debug_id,
    normalize_ranking,
    recent_debug_turns,
)
from rag_upgrade.rerank_client import RerankClient, RerankError  # noqa: E402
from models import ModelConfigError, resolve_models, validate_models  # noqa: E402
from bot_plan import (  # noqa: E402
    intent_from_plan,
    memory_query,
    needs_local_rag,
    parse_plan,
)
from bot_runtime import (  # noqa: E402
    channel_id_from_room,
    compose_system_prompt,
    ensure_runtime,
    get_channel,
    public_channel_list,
    respond_allows,
    tool_policy,
)
from planner_context import (  # noqa: E402
    PLANNER_MAX_TOKENS,
    PlannerHistory,
    TimeReference as PlannerTimeReference,
    plan_turn,
)
from evidence_packing import make_time_filter, pack_evidence_set  # noqa: E402
from bot_tools import execute_named_tool, is_success  # noqa: E402
from memory_blocks import (  # noqa: E402
    catalog_from_leaves,
    leaf_ids_for_blocks,
    platform_of_room,
    segmentation_version_from_meta,
    space_from_index,
    write_catalog,
    _label_for_room,
)
from candidates import FusionResult, fuse, guarantee_pool, source_family  # noqa: E402
from candidates_assess import assess  # noqa: E402
from person_retrieval import alias_run, build_identity_table, resolve_persons, speaker_fusion  # noqa: E402
from retrieval_config import pool_size, retrieval_config  # noqa: E402
from message_vectors import (  # noqa: E402
    MESSAGE_LEAF_NAME,
    MESSAGE_META_NAME,
    MESSAGE_RRF_WEIGHT,
    MESSAGE_RUN_NAME,
    MESSAGE_SCALE_NAME,
    MESSAGE_VECS_NAME,
    fit_message_vectors,
    embed_full_width,
    load_message_vectors,
    message_dense_run,
    message_dims,
    message_fingerprint,
    select_message_rows,
    write_message_vectors,
)
from query_embed import QueryEmbedder, query_embed_config  # noqa: E402
from turn_diagnosis import build_diagnosis  # noqa: E402
from soft_events import (  # noqa: E402
    SoftEventRecipe,
    build_soft_events,
    project_event_ranks,
)
from sandbox_paths import load_local_config, local_config_path, resolve_sandbox_data_dir  # noqa: E402
from tool_parity import (  # noqa: E402
    canonicalize_tool,
    decide_ranking,
    fallback_ranking,
    parse_bang_command,
    ranking_policy,
)
from conversation import (  # noqa: E402
    DEFAULT_CHAR_LIMIT,
    DEFAULT_KEEP,
    SUMMARIZE_PROMPT,
    append_turn,
    compact,
    get_conversation,
    history_for_llm,
    needs_compact,
    put_conversation,
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
DATA = HERE / "data"
MESSAGES_PATH = DATA / "messages.json"
LEAVES_PATH = DATA / "leaves.json"
BASELINE_PATH = DATA / "baseline_windows.json"
LEAF_VECS_PATH = DATA / "leaf_vecs.npy"
BASELINE_VECS_PATH = DATA / "baseline_vecs.npy"
SUMMARIES_PATH = DATA / "summaries.json"
SUMMARY_VECS_PATH = DATA / "summary_vecs.npy"
META_PATH = DATA / "meta.json"
LEDGER_PATH = DATA / "session_ledger.json"
CKPT_DIR = DATA / "ckpt"
CKPT_STATE_PATH = CKPT_DIR / "state.json"
PROGRESS_PATH = DATA / "prepare_progress.json"
WINDOWS_PATH = LEAVES_PATH
VECS_PATH = LEAF_VECS_PATH


def _bind_data_dir(data: Path) -> None:
    """RAG-ified artifacts live under test-sandbox/data, not the product DB."""
    global DATA, MESSAGES_PATH, LEAVES_PATH, BASELINE_PATH, LEAF_VECS_PATH
    global BASELINE_VECS_PATH, SUMMARIES_PATH, SUMMARY_VECS_PATH, META_PATH
    global LEDGER_PATH, CKPT_DIR, CKPT_STATE_PATH, WINDOWS_PATH, VECS_PATH, PROGRESS_PATH
    DATA = Path(data)
    MESSAGES_PATH = DATA / "messages.json"
    LEAVES_PATH = DATA / "leaves.json"
    BASELINE_PATH = DATA / "baseline_windows.json"
    LEAF_VECS_PATH = DATA / "leaf_vecs.npy"
    BASELINE_VECS_PATH = DATA / "baseline_vecs.npy"
    SUMMARIES_PATH = DATA / "summaries.json"
    SUMMARY_VECS_PATH = DATA / "summary_vecs.npy"
    META_PATH = DATA / "meta.json"
    LEDGER_PATH = DATA / "session_ledger.json"
    CKPT_DIR = DATA / "ckpt"
    CKPT_STATE_PATH = CKPT_DIR / "state.json"
    WINDOWS_PATH = LEAVES_PATH
    VECS_PATH = LEAF_VECS_PATH
    PROGRESS_PATH = DATA / "prepare_progress.json"


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


# 테스트가 모듈 상수를 잠시 덮어쓰므로 retrieval_post 기본값을 여기서 재바인딩한다.
LEXICAL_LIMIT = _DEFAULT_LEXICAL_LIMIT
DENSE_LIMIT = _DEFAULT_DENSE_LIMIT


def load_config() -> dict[str, Any]:
    return json.loads(local_config_path().read_text(encoding="utf-8"))


def _maybe_rebind_data_from_env(cfg: dict[str, Any]) -> None:
    """Rebind only when an explicit override is set so tests can keep a temp DATA."""

    if str(os.environ.get("MASAMONG_SANDBOX_DATA") or "").strip() or str(
        os.environ.get("MASAMONG_SANDBOX_CONFIG") or ""
    ).strip():
        _bind_data_dir(resolve_sandbox_data_dir(cfg))


def weighted_rrf(
    rank_lists: list[list[str]],
    weights: list[float],
    *,
    k: int = 60,
) -> list[str]:
    scores: dict[str, float] = {}
    for ranks, weight in zip(rank_lists, weights):
        for pos, doc_id in enumerate(ranks):
            scores[doc_id] = scores.get(doc_id, 0.0) + weight / (k + pos + 1)
    return [doc_id for doc_id, _ in sorted(scores.items(), key=lambda item: -item[1])]


_LEDGER_LOCK = threading.Lock()


def load_ledger() -> dict[str, float]:
    if not LEDGER_PATH.exists():
        return {"spent_usd": 0.0}
    raw = json.loads(LEDGER_PATH.read_text(encoding="utf-8"))
    return {"spent_usd": float(raw.get("spent_usd", 0.0))}


def save_ledger(spent_usd: float) -> None:
    DATA.mkdir(parents=True, exist_ok=True)
    with _LEDGER_LOCK:
        existing: dict[str, Any] = {}
        if LEDGER_PATH.exists():
            try:
                row = json.loads(LEDGER_PATH.read_text(encoding="utf-8"))
                if isinstance(row, dict):
                    existing = row
            except (OSError, json.JSONDecodeError, TypeError):
                existing = {}
        existing["spent_usd"] = round(float(spent_usd), 6)
        _atomic_write_json(LEDGER_PATH, existing)


def apply_client_usage(
    client: PlayClient,
    *,
    cursor: int = 0,
    extra_usd: float = 0.0,
) -> dict[str, Any]:
    """Persist settled client usage exactly once per attempt_id. Unknown cost is kept, never zeroed."""

    with _LEDGER_LOCK:
        existing: dict[str, Any] = {"spent_usd": 0.0, "unknown_cost_usd": 0.0, "attempt_ids": []}
        if LEDGER_PATH.exists():
            try:
                row = json.loads(LEDGER_PATH.read_text(encoding="utf-8"))
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
        DATA.mkdir(parents=True, exist_ok=True)
        _atomic_write_json(LEDGER_PATH, existing)
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
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(path)


def _load_vecs(path: Path) -> np.ndarray:
    return np.asarray(np.load(path), dtype=np.float32)


_prepare_summary_on = True
_prepare_cost_usd: float | None = None


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
        cost_usd=_prepare_cost_usd,
        summary_enabled=_prepare_summary_on,
        started_at=phase_started(phase),
    )
    wrote = write_progress(PROGRESS_PATH, payload, force=force or status != "running")
    if wrote:
        print_cli(payload)


def _note_prepare_cost(client: PlayClient) -> None:
    global _prepare_cost_usd
    try:
        _prepare_cost_usd = float(client.usage.cost_usd)
    except (TypeError, ValueError, AttributeError):
        return


def prepare_status_payload() -> dict[str, Any]:
    row = read_progress(PROGRESS_PATH)
    if row:
        return row
    ready = META_PATH.is_file() and LEAVES_PATH.is_file() and LEAF_VECS_PATH.is_file()
    return idle_payload(ready=ready)


def _load_ckpt_state(wanted: dict[str, Any]) -> dict[str, Any] | None:
    if not CKPT_STATE_PATH.exists():
        return None
    try:
        state = json.loads(CKPT_STATE_PATH.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    if state.get("fingerprint") != wanted:
        return None
    return state


def _save_ckpt_state(state: dict[str, Any]) -> None:
    CKPT_DIR.mkdir(parents=True, exist_ok=True)
    _atomic_write_json(CKPT_STATE_PATH, state)


def _clear_checkpoint() -> None:
    if not CKPT_DIR.exists():
        return
    for path in CKPT_DIR.iterdir():
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
        _atomic_write_json(SUMMARIES_PATH, rows)
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

    CKPT_DIR.mkdir(parents=True, exist_ok=True)
    partial_path = CKPT_DIR / f"{name}_vecs.partial.npy"
    meta_path = CKPT_DIR / f"{name}_embed.json"
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
            leaf_vecs = np.load(DATA / "leaf_vecs.npy", mmap_mode="r", allow_pickle=False)
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

    fp_path = CKPT_DIR / "message_fingerprint.json"
    try:
        same = json.loads(fp_path.read_text(encoding="utf-8")) == fingerprint
    except (OSError, ValueError):
        same = False
    CKPT_DIR.mkdir(parents=True, exist_ok=True)
    if not same:
        for name in _MESSAGE_CKPT_FILES:
            (CKPT_DIR / name).unlink(missing_ok=True)
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
        (DATA / MESSAGE_VECS_NAME).exists()
        and (DATA / MESSAGE_LEAF_NAME).exists()
        and load_message_vectors(DATA, fingerprint=fingerprint, leaf_rows=leaf_rows) is not None
    ):
        return {**report, "reused": True}
    _align_message_checkpoint(fingerprint)
    full_path = CKPT_DIR / "message_vecs_full.npy"
    _report_prepare(phase="embeddings", done=0, total=1, detail="messages", force=True)
    full = _embed_texts(client, [text for _, text in observations], name="message", final_path=full_path, resume=True)
    truncatable = bool(cfg["embed_truncatable"]) if "embed_truncatable" in cfg else embed_full_width(client.embed_model) > 0
    if not truncatable and full.shape[1] < fingerprint["dims"]:
        raise PlaygroundError("메시지 임베딩 폭이 leaf 폭보다 작습니다")
    row_of = {oid: i for i, (oid, _) in enumerate(observations)}
    vecs = fit_message_vectors(full, fingerprint["dims"])[[row_of[oid] for oid, _ in pairs]]
    write_message_vectors(DATA, vecs, leaf_rows, meta={"fingerprint": fingerprint, "count": len(pairs)})
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
    META_PATH.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
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
        report = prepare_message_vectors(cfg, json.loads(MESSAGES_PATH.read_text(encoding="utf-8")), leaves, client)
    except BaseException:
        _settle_prepare(client, meta, base_cost=prior, clear=False, usage_key="message_prepare_usage")
        raise
    meta["message_vectors"] = report["message_vectors"]
    _settle_prepare(client, meta, base_cost=prior, clear=True, usage_key="message_prepare_usage")


def prepare_index(*, force: bool = False) -> dict[str, Any]:
    global _prepare_summary_on, _prepare_cost_usd
    cfg = load_config()
    _bind_data_dir(resolve_sandbox_data_dir(cfg))
    DATA.mkdir(parents=True, exist_ok=True)
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
    _prepare_summary_on = summary_on
    _prepare_cost_usd = None
    ready = (
        META_PATH.exists()
        and MESSAGES_PATH.exists()
        and LEAVES_PATH.exists()
        and LEAF_VECS_PATH.exists()
        and (not baseline_on or (BASELINE_PATH.exists() and BASELINE_VECS_PATH.exists()))
        and (not summary_on or (SUMMARIES_PATH.exists() and SUMMARY_VECS_PATH.exists()))
    )
    layers = wanted_layers(cfg)
    stored_fp = None
    if META_PATH.exists():
        try:
            stored_fp = json.loads(META_PATH.read_text(encoding="utf-8")).get("index_fingerprint")
        except (OSError, json.JSONDecodeError):
            stored_fp = None
    ckpt_fp = None
    if CKPT_STATE_PATH.exists():
        try:
            ckpt_fp = json.loads(CKPT_STATE_PATH.read_text(encoding="utf-8")).get("fingerprint")
        except (OSError, json.JSONDecodeError):
            ckpt_fp = None
    on_disk_fp = stored_fp if stored_fp is not None else ckpt_fp
    existing_manifest = load_manifest(DATA / MANIFEST_NAME)
    reuse_plan = decide_reuse(existing_manifest, layers, files_ready=ready)
    if ready and not force:
        meta = json.loads(META_PATH.read_text(encoding="utf-8"))
        leaves_on_disk: list[Any] = []
        try:
            leaves_on_disk = json.loads(LEAVES_PATH.read_text(encoding="utf-8"))
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
                DATA / MANIFEST_NAME,
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
            LEAF_VECS_PATH,
            BASELINE_VECS_PATH,
            SUMMARY_VECS_PATH,
            SUMMARIES_PATH,
            META_PATH,
            DATA / MESSAGE_VECS_NAME,
            DATA / MESSAGE_LEAF_NAME,
            DATA / MESSAGE_SCALE_NAME,
            DATA / MESSAGE_META_NAME,
        ):
            path.unlink(missing_ok=True)
        resumed = False
    else:
        ckpt = _load_ckpt_state(wanted)
        resumed = bool(ckpt) or (
            SUMMARIES_PATH.exists() and not (META_PATH.exists() and ready)
        )
        if ckpt:
            print(f"체크포인트 재개 phase={ckpt.get('phase')} summaries={ckpt.get('summary_count', 0)}")
        else:
            # fingerprint가 바뀌었으면 옛 ckpt 폐기
            if CKPT_STATE_PATH.exists():
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
        and MESSAGES_PATH.exists()
        and LEAVES_PATH.exists()
        and (not baseline_on or BASELINE_PATH.exists())
        and reuse_plan.segments
        and on_disk_fp == wanted
    )
    if can_reuse_segments:
        messages = json.loads(MESSAGES_PATH.read_text(encoding="utf-8"))
        leaves = json.loads(LEAVES_PATH.read_text(encoding="utf-8"))
        if baseline_on and BASELINE_PATH.exists():
            baseline = json.loads(BASELINE_PATH.read_text(encoding="utf-8"))
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
        MESSAGES_PATH.write_text(json.dumps(messages, ensure_ascii=False), encoding="utf-8")
        LEAVES_PATH.write_text(json.dumps(leaves, ensure_ascii=False), encoding="utf-8")
        if baseline_on:
            BASELINE_PATH.write_text(json.dumps(baseline, ensure_ascii=False), encoding="utf-8")
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
        if not force and SUMMARIES_PATH.exists():
            try:
                for row in json.loads(SUMMARIES_PATH.read_text(encoding="utf-8")):
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
        ("leaf", leaf_texts, LEAF_VECS_PATH),
    ]
    if baseline_texts:
        embed_specs.append(("baseline", baseline_texts, BASELINE_VECS_PATH))
    if summaries:
        embed_specs.append(("summary", [s["summary"] for s in summaries], SUMMARY_VECS_PATH))

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
        _atomic_write_json(SUMMARIES_PATH, summaries)
        np.save(SUMMARY_VECS_PATH, summary_vecs)
    elif SUMMARIES_PATH.exists():
        SUMMARIES_PATH.unlink()
        if SUMMARY_VECS_PATH.exists():
            SUMMARY_VECS_PATH.unlink()

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
        DATA / MANIFEST_NAME,
        layers,
        extra={"leaf_count": len(leaves), "message_count": len(messages), "status": "ready"},
    )
    META_PATH.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
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


_PARTICLE_CHARS = set("이가은는을를도만의와과야아여요랑")


def _strip_name_particles(word: str) -> str:
    out = word
    for _ in range(3):
        if out and out[-1] in _PARTICLE_CHARS:
            out = out[:-1]
        else:
            break
    return out


def _token_in_text(
    text: str,
    tok: str,
    *,
    labels: set[str] | None = None,
) -> bool:
    """토큰 매칭. 조사(이/가)와 성+이름(김철수⊃철수), 별칭 라벨 포함(닉네임⊃이름)을 허용."""

    if not tok or not text or tok not in text:
        return False
    labels = labels or set()
    start = 0
    n = len(tok)
    while True:
        i = text.find(tok, start)
        if i < 0:
            return False
        before = text[i - 1] if i > 0 else ""
        after = text[i + n] if i + n < len(text) else ""
        hangul_before = bool(before) and "가" <= before <= "힣"
        hangul_after = bool(after) and "가" <= after <= "힣"
        if not hangul_before and not hangul_after:
            return True
        left = i
        right = i + n
        while left > 0 and "가" <= text[left - 1] <= "힣":
            left -= 1
        while right < len(text) and "가" <= text[right] <= "힣":
            right += 1
        word = text[left:right]
        core = _strip_name_particles(word)
        if core == tok or core in labels:
            return True
        # 더 긴 별칭 라벨(긴 닉네임)이 단어에 포함.
        if any(
            lab and lab != tok and len(lab) > len(tok) and lab in core for lab in labels
        ):
            return True
        # 성+이름: 김철수 ⊃ 철수 (성 1~2글자)
        if core.endswith(tok):
            prefix = core[: len(core) - len(tok)]
            if 1 <= len(prefix) <= 2 and all("가" <= c <= "힣" for c in prefix):
                return True
        if hangul_before:
            start = i + 1
            continue
        # 뒤에만 조사
        if hangul_after and all(c in _PARTICLE_CHARS for c in word[n:]):
            return True
        start = i + 1


# 소개형 질문에서 "정체성 단서"로 볼 용어. 기본값은 첫 실험 코퍼스에 맞춘 것이라
# 다른 채널에서는 config `identity_terms` 로 바꿔야 한다 (코드에 인물 특성을 박지 않는다).
DEFAULT_IDENTITY_TERMS: tuple[str, ...] = ()  # 비어 있으면 소개형 정체성 가산 없음
_RECENT_RE = re.compile(
    r"(최근|요즘|요새|요즈음|근래|방금|오늘|어제|이번\s*주|이번주|이번\s*달)"
)
_INTRO_RE = re.compile(r"(소개|누구야|누구\s*야|어떤\s*사람|뭐\s*하는\s*사람|프로필)")
# 오탐 차단: 지지난주(지지), 보수공사·보수를 받다(보수), 성향 테스트(성향)는 정치가 아니다.
_POLITICS_RE = re.compile(
    r"(정치|정치\s*성향|지지(?!난|율)(?=\s*(하|해|층|자|정당|후보|당|율)|$)"
    r"|보수(?!\s*(공사|작업|비|료|를|점검|유지|하|되|공))"
    r"|진보(?!\s*(하|되|된|시|를))"
    r"|좌파|우파|대선|대통령|국힘|민주당|정당)"
)


def _identity_re(cfg: dict[str, Any]) -> re.Pattern[str] | None:
    terms = cfg.get("identity_terms")
    if not isinstance(terms, list) or not terms:
        terms = list(DEFAULT_IDENTITY_TERMS)
    terms = [str(t) for t in terms if str(t).strip()]
    if not terms:
        return None
    return re.compile("|".join(re.escape(t) for t in terms), re.I)



def _source_date(value: Any) -> str:
    """원문 시각의 날짜를 그 원문 기준으로 ("2026-07-25"). 시간대가 없는 카카오 시각을 이 컴퓨터의 시간대로
    읽어 UTC로 바꾸면 새벽 대화가 하루 앞당겨지므로 변환하지 않는다. Go corpus.ParseTime + Format과 같다."""
    if not value or not isinstance(value, str):
        return ""
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text).strftime("%Y-%m-%d")
    except ValueError:
        return ""


def _leaf_source_date(index: Any, leaf: dict[str, Any]) -> str:
    """leaf 마지막 원문(없으면 첫 원문)의 날짜. Go search.leafDate와 같다."""
    messages = index.messages
    for i in (int(leaf.get("end") or 0) - 1, int(leaf.get("start") or 0)):
        if 0 <= i < len(messages):
            d = _source_date(messages[i].get("parsed_time"))
            if d:
                return d
    return ""


def _context_only_date(index: Any, packed: dict[str, Any], leaf: dict[str, Any]) -> str:
    """근거의 matched 원문이 모두 앞 근거에 이미 보여서 앞뒤 문맥만 남았으면, 날짜 라벨을 실제로 보이는
    원문의 날짜("2026-08-11" 또는 "2026-08-11~2026-09-06")로 한다. 그 밖의 근거는 leaf 날짜 그대로("").
    Go internal/memory/search/retrieve.go contextOnlyDate와 같은 규칙."""
    visible = {str(x) for x in (packed.get("visible_observation_ids") or [])}
    if not visible:
        return ""
    if any(str(o) in visible for o in (leaf.get("observation_ids") or [])):
        return ""
    first = last = ""
    messages = index.messages
    start = int(packed.get("start") or 0)
    end = min(int(packed.get("end") or 0), len(messages))
    for i in range(start, end):
        row = messages[i]
        if str(row.get("observation_id")) not in visible:
            continue
        d = _source_date(row.get("parsed_time"))
        if not d:
            continue
        if not first:
            first = d
        last = d
    if not first or first == last:
        return first
    return f"{first}~{last}"


def _parse_ts(value: Any) -> float | None:
    if not value or not isinstance(value, str):
        return None
    text = value.strip()
    if not text:
        return None
    try:
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        return datetime.fromisoformat(text).timestamp()
    except ValueError:
        return None


def _query_intent(query: str) -> dict[str, Any]:
    """Regex fallback only. 플래너가 돌지 않는 경로(평가 스크립트·직접 retrieve 호출)에서 쓴다.
    런타임은 플래너 LLM 출력(`intent_from_plan`)을 우선한다 — 키워드는 방마다 다르다."""

    recent = bool(_RECENT_RE.search(query))
    intro = bool(_INTRO_RE.search(query))
    politics = bool(_POLITICS_RE.search(query))
    return {
        "recent": recent,
        "intro": intro,
        "politics": politics,
        "prefer_missing_over_guess": politics or ("어때" in query and "정치" in query),
        "kind": "person_intro" if intro else "other",
        "targets": None,  # None → 별칭 표를 질의에 regex 매칭해서 찾는다
        "source": "regex",
    }


def _neutral_intent() -> dict[str, Any]:
    """`intent_fallback: none` — no corpus keywords. Without planner output the query is treated as a
    plain search (no recency/intro/sensitive flags, no alias-table matching of the raw query)."""

    return {
        "recent": False,
        "intro": False,
        "politics": False,
        "prefer_missing_over_guess": False,
        "kind": "other",
        "targets": [],
        "source": "none",
    }


def resolve_intent(
    query: str, intent: dict[str, Any] | None, cfg: Mapping[str, Any] | None = None
) -> dict[str, Any]:
    """Planner intent wins. Without planner output the default (`intent_fallback: none`) is a neutral
    intent; the legacy Korean keyword regex is opt-in (`intent_fallback: regex`)."""

    # Default is `none` since the 2026-09-23 generalization audit: on holdout gold (Discord + two Kakao
    # rooms) the keyword fallback changed nothing measurable, and its word lists are corpus-specific.
    mode = str((cfg or {}).get("intent_fallback") or "none")
    fallback = _query_intent(query) if mode == "regex" else _neutral_intent()
    if not intent:
        return fallback
    out = dict(fallback)
    out.update(intent)
    return out


# 별칭 키가 흔한 낱말과 겹칠 때 인물 부스트가 잘못 켜지는 걸 막는다 (regex fallback 전용).
# 코퍼스마다 다르므로 코드 기본값은 비우고 config `person_alias_ignore`로만 준다.
DEFAULT_ALIAS_IGNORE: frozenset[str] = frozenset()
# 플래너가 person_targets에 넣을 수 있는 대명사·지시어. 사람 이름이 아니므로 버린다.
_PRONOUNS = frozenset({"걔", "그", "그녀", "그분", "그사람", "그 사람", "얘", "이 사람", "저 사람", "너", "나", "저", "우리", "애", "쟤", "누구", "본인", "자기", "봇", "마사몽"})


def _clean_planner_targets(raw: list[Any]) -> list[str]:
    """플래너 person_targets 정리: 조사 제거(철수가→철수), 대명사 제외, 길이 2~40, 중복 제거."""

    out: list[str] = []
    for item in raw:
        name = str(item or "").strip()
        if not name or name in _PRONOUNS:
            continue
        stripped = _strip_name_particles(name) if all("가" <= c <= "힣" for c in name) else name
        if len(stripped) >= 2:
            name = stripped
        # 한 글자는 이름이 아니라 조사·지시어일 확률이 높다 (예: "이").
        if 2 <= len(name) <= 40 and name not in _PRONOUNS and name not in out:
            out.append(name)
    return out


def _person_identity_mode(cfg: Mapping[str, Any]) -> str:
    """`retrieval.person.identity`: `observed` (default — labels one native account used, derived from
    the index; Kakao labels stay unlinked), `config` (legacy hand-written `person_aliases`) or `both`.
    Holdout R2 results were identical with and without the hand table (2026-09-23 audit)."""

    person = ((cfg or {}).get("retrieval") or {}).get("person") or {}
    mode = str(person.get("identity") or "observed")
    return mode if mode in {"config", "observed", "both"} else "observed"


def _identity_for(index: Any) -> Any:
    """Account-backed identity table, cached on the index (derived from rows, never hand-written)."""

    cached = getattr(index, "_identity_table", None)
    if cached is None:
        cached = build_identity_table(index)
        try:
            index._identity_table = cached
        except AttributeError:
            pass
    return cached


def _observed_alias_table(index: Any) -> dict[str, set[str]]:
    """label → all labels the same *native account* used. Label-only keys (Kakao) add nothing."""

    table = _identity_for(index)
    out: dict[str, set[str]] = {}
    for key, labels in table.labels_by_key.items():
        if not getattr(key, "linked", False) or len(labels) < 2:
            continue
        for lab in labels:
            out.setdefault(lab, set()).update(labels)
    return out


def _alias_table_for(index: Any) -> dict[str, set[str]]:
    mode = _person_identity_mode(index.cfg)
    if mode == "config":
        return _alias_table(index.cfg)
    observed = _observed_alias_table(index)
    if mode == "observed":
        return observed
    merged = {k: set(v) for k, v in _alias_table(index.cfg).items()}
    for k, v in observed.items():
        merged.setdefault(k, set()).update(v)
    return merged


def _alias_table(cfg: dict[str, Any]) -> dict[str, set[str]]:
    """검색 힌트용 별칭. 제품 인물 병합이 아니라 sandbox retrieval boost 전용."""

    raw = cfg.get("person_aliases") or {}
    out: dict[str, set[str]] = {}
    if not isinstance(raw, dict):
        return out
    for key, values in raw.items():
        k = str(key).strip()
        if not k:
            continue
        labels = {k}
        if isinstance(values, list):
            labels.update(str(v).strip() for v in values if str(v).strip())
        out[k] = labels
    return out


def _query_targets(
    query: str,
    aliases: dict[str, set[str]],
    ignore: set[str] | frozenset[str] = DEFAULT_ALIAS_IGNORE,
) -> list[str]:
    """질의 안의 인물 별칭 키. 조사·성+이름 경계를 지키고(철수영 ⊅ 철수), ASCII는 대소문자 무시."""

    hits: list[str] = []
    q_fold = query.casefold()
    for key in sorted(aliases.keys(), key=len, reverse=True):
        if not key or key.casefold() in {x.casefold() for x in ignore}:
            continue
        if key.isascii():
            if re.search(r"(?<![a-z0-9])" + re.escape(key.casefold()) + r"(?![a-z0-9])", q_fold):
                hits.append(key)
            continue
        if _token_in_text(query, key, labels=aliases.get(key) or {key}):
            hits.append(key)
    # 더 긴 별칭에 흡수되는 짧은 키 제거 (예: 김철수 ∩ 철수)
    return [
        key
        for key in hits
        if not any(key != other and key in other for other in hits)
    ]


def _target_labels(targets: list[str], aliases: dict[str, set[str]]) -> set[str]:
    labels: set[str] = set()
    for key in targets:
        labels.update(aliases.get(key) or {key})
        labels.add(key)
    return {x for x in labels if x}


def _intent_targets(index: IndexBundle, query: str, intent: dict[str, Any]) -> list[str]:
    aliases = _alias_table_for(index)
    if intent.get("targets") is not None:
        return _clean_planner_targets(list(intent["targets"]))
    ignore = set(DEFAULT_ALIAS_IGNORE) | {
        str(x).strip() for x in (index.cfg.get("person_alias_ignore") or []) if str(x).strip()
    }
    return _query_targets(query, aliases, ignore)


def _lexical_query_expanded(
    index: IndexBundle, query: str, intent: dict[str, Any]
) -> tuple[str, list[str]]:
    if not bool(index.cfg.get("alias_expand_lexical", True)):
        return query, []
    aliases = _alias_table_for(index)
    targets = _intent_targets(index, query, intent)
    labels = _target_labels(targets, aliases)
    extra = [lab for lab in sorted(labels, key=len, reverse=True) if lab and lab not in query]
    if not extra:
        return query, []
    return query + " " + " ".join(extra), extra


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

    @property
    def docs(self) -> list[dict[str, Any]]:
        return self.leaves


@dataclass
class _SoftEventRuntime:
    """Transient searchable view over stable leaves; never an evidence store."""

    recipe: SoftEventRecipe
    result: Any
    lexical: ThreadSafeLexicalIndex
    event_ids: list[str]
    event_vectors: np.ndarray
    event_vector_ids: list[str]
    search_texts: dict[str, str] = field(default_factory=dict)


def _rerank_available() -> bool:
    """R2를 부를 수 있는가 — OpenRouter 키가 있어야 한다. 예산은 decide_ranking이 본다."""

    try:
        secrets = load_secrets(Path(os.environ["EVAL_SECRETS"]))
    except (OSError, KeyError, ValueError):
        return False
    return bool(str(secrets.get("OPENROUTER_API_KEY") or "").strip())


def _configured_embed_model() -> str:
    """config.json models.embed > secrets EVAL_EMBED_MODEL > 기본값 (models.resolve_models)."""

    return resolve_models(load_config(), _secrets())["embed"]


def load_index() -> IndexBundle:
    cfg = load_config()
    _maybe_rebind_data_from_env(cfg)
    needed = [
        META_PATH,
        MESSAGES_PATH,
        LEAVES_PATH,
        LEAF_VECS_PATH,
    ]
    if not all(p.exists() for p in needed):
        raise PlaygroundError("leaf/parent 인덱스가 없다. prepare.py --force 를 실행하라")
    meta = json.loads(META_PATH.read_text(encoding="utf-8"))
    # 인덱스를 만든 임베딩 모델과 질의 임베딩 모델이 다르면 차원이 같아도 유사도가 무의미하다.
    indexed_model = str(((meta.get("recipe") or {}).get("models") or {}).get("embed") or "")
    wanted_model = _configured_embed_model()
    if indexed_model and indexed_model != wanted_model:
        raise PlaygroundError(
            f"인덱스 임베딩 모델({indexed_model})과 현재 설정({wanted_model})이 다르다. "
            "EVAL_EMBED_MODEL을 맞추거나 prepare.py --force 를 실행하라"
        )
    messages = json.loads(MESSAGES_PATH.read_text(encoding="utf-8"))
    leaves = json.loads(LEAVES_PATH.read_text(encoding="utf-8"))
    leaf_vectors = _load_vecs(LEAF_VECS_PATH)
    if len(leaves) != len(leaf_vectors):
        raise PlaygroundError("세그먼트와 벡터 개수가 맞지 않는다. prepare.py --force")

    baseline: list[dict[str, Any]] = []
    baseline_vectors = np.zeros((0, int(leaf_vectors.shape[1])), dtype=np.float32)
    if BASELINE_PATH.exists() and BASELINE_VECS_PATH.exists():
        baseline = json.loads(BASELINE_PATH.read_text(encoding="utf-8"))
        baseline_vectors = _load_vecs(BASELINE_VECS_PATH)
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
        if not SUMMARIES_PATH.exists() or not SUMMARY_VECS_PATH.exists():
            raise PlaygroundError("요약 인덱스가 없다. prepare.py --force 를 실행하라")
        summaries = json.loads(SUMMARIES_PATH.read_text(encoding="utf-8"))
        summary_vectors = _load_vecs(SUMMARY_VECS_PATH)
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
        loaded = load_message_vectors(DATA, fingerprint=fingerprint, leaf_rows=[li for _, li in pairs])
        if loaded is not None:
            bundle.message_vectors, bundle.message_leaf = loaded
    apply_vector_dims(bundle, int(cfg.get("vector_dims") or 0))
    populate_block_catalog(bundle, persist_dir=DATA)
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


def fit_query_vector(index: Any, q_vec: np.ndarray) -> np.ndarray:
    """Match a query vector to the stored dimensionality (after `apply_vector_dims`)."""

    vecs = getattr(index, "leaf_vectors", None)
    dims = int(vecs.shape[1]) if vecs is not None and getattr(vecs, "ndim", 0) == 2 else 0
    if dims and q_vec.shape[0] > dims:
        q_vec = np.ascontiguousarray(q_vec[:dims], dtype=np.float32)
        q_vec /= np.linalg.norm(q_vec) + 1e-9
    return q_vec


def _embed_instruction(cfg: Mapping[str, Any]) -> str:
    return str(cfg.get("embed_query_instruction") or DEFAULT_EMBED_QUERY_INSTRUCTION)


def _late_settle(client: Any) -> None:
    """헤지에서 진 요청이 턴 장부 반영 뒤에 끝나면 그 비용을 장부에 더한다."""

    hook = getattr(client, "late_settle", None)
    if callable(hook):
        try:
            hook()
        except Exception:  # noqa: BLE001 — 장부 보강 실패가 응답을 막지 않는다
            pass


def prefetch_query_vector(index: Any, client: Any, text: str) -> None:
    """결과를 기다리지 않고 질의 임베딩을 시작한다(플래너와 겹치기). 설정에서 끄면 아무것도 안 한다."""

    embedder = getattr(index, "query_embedder", None)
    if embedder is None or not bool(query_embed_config(index.cfg).get("prefetch")):
        return
    embedder.prefetch(client, text, instruction=_embed_instruction(index.cfg), on_late=lambda: _late_settle(client))


def query_vector(index: Any, client: Any, text: str) -> tuple[np.ndarray, dict[str, Any]]:
    """정규화·차원 맞춤된 질의 벡터와 출처(hit/joined/miss/direct, hedged)."""

    instruction = _embed_instruction(index.cfg)
    embedder = getattr(index, "query_embedder", None)
    if embedder is None:
        vec = np.asarray(client.embed([text], kind="query", instruction=instruction)[0], dtype=np.float32)
        vec /= np.linalg.norm(vec) + 1e-9
        info: dict[str, Any] = {"source": "direct", "hedged": False}
    else:
        vec, info = embedder.get(client, text, instruction=instruction, on_late=lambda: _late_settle(client))
        vec = vec.copy()
    return fit_query_vector(index, vec), info


def _q1_text(query: str, intent: Mapping[str, Any] | None, mq: Mapping[str, Any]) -> str:
    """multi-query q1(dense) 질의: 플래너 semantic_query, 없으면 memory.search.query. 원문과 같으면 빈 문자열."""

    if not intent or not bool(mq.get("enabled", True)) or int(mq.get("max_queries") or 3) <= 1:
        return ""
    sem_q = str(intent.get("semantic_query") or "").strip()
    rewrite_q = str(intent.get("memory_search_query") or "").strip()
    if sem_q and _canon_query(sem_q) != _canon_query(query):
        return sem_q
    if rewrite_q and _canon_query(rewrite_q) != _canon_query(query):
        return rewrite_q
    return ""


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


def _hit_provenance(index: IndexBundle, doc: dict[str, Any]) -> dict[str, Any]:
    room = str(doc.get("source_room_id") or "")
    bid = str((index.block_id_by_room or {}).get(room) or "")
    block = (index.block_by_id or {}).get(bid) or {}
    return {
        "block_id": bid,
        "platform": str(block.get("platform") or platform_of_room(room)),
        "source_room_id": room,
        "label": str(block.get("label") or _label_for_room(room)),
    }


LEXICAL_LIMIT = 80
DENSE_LIMIT = 50


def _dense_hits(
    vectors: np.ndarray, ids: list[str], q_vec: np.ndarray, *, allowed_ids: set[str] | None, limit: int
) -> list[tuple[str, float]]:
    if allowed_ids is not None:
        idx = [i for i, doc_id in enumerate(ids) if doc_id in allowed_ids]
        if not idx:
            return []
        sims = vectors[idx] @ q_vec
        order = np.argsort(-sims, kind="stable")[:limit]
        return [(ids[idx[i]], float(sims[i])) for i in order]
    sims = vectors @ q_vec
    order = np.argsort(-sims, kind="stable")[:limit]
    return [(ids[i], float(sims[i])) for i in order]


def _lexical_hits(
    fts: ThreadSafeLexicalIndex, query: str, *, ids_n: int, allowed_ids: set[str] | None, limit: int, lock: threading.Lock
) -> list[tuple[str, float]]:
    with lock:
        raw = fts.search(query, limit=limit if allowed_ids is None else max(limit, ids_n))
    return [(d, float(sc)) for d, sc in raw if allowed_ids is None or d in allowed_ids][:limit]


def _hybrid_fusion(
    *,
    fts: ThreadSafeLexicalIndex,
    vectors: np.ndarray,
    ids: list[str],
    query: str,
    q_vec: np.ndarray,
    weights: list[float],
    lock: threading.Lock,
    trace: dict[str, Any] | None = None,
    trace_key: str = "leaf",
    allowed_ids: set[str] | None = None,
    lexical_limit: int | None = None,
    dense_limit: int | None = None,
    lexical_query: str | None = None,
) -> FusionResult:
    """One lexical run + one dense run over a doc set, fused with weighted RRF.

    Returns candidates with provenance (`{trace_key}_lexical` / `{trace_key}_dense` ranks and raw
    scores). `_hybrid_rank` is the id-only wrapper kept for callers/tests that predate candidates.
    """

    # 완전 동점은 입력 행 순서로 깬다(Go denseTop과 동일). 기본 quicksort는
    # 같은 코사인의 행 순서를 바꿔 BLAS 오차와 별개의 parity 차이를 만든다.

    # 방 필터가 있으면 FTS LIMIT을 필터 *뒤*에 적용한다. 전역 top-80을 먼저 자르면
    # 다른 방의 매칭이 많을 때 이 방의 어휘 후보가 0이 된다.
    lex_lim = int(lexical_limit if lexical_limit is not None else LEXICAL_LIMIT)
    den_lim = int(dense_limit if dense_limit is not None else DENSE_LIMIT)
    search_q = query if lexical_query is None else lexical_query
    lex_name, den_name = f"{trace_key}_lexical", f"{trace_key}_dense"
    with lock:
        lexical = fts.search(
            search_q, limit=lex_lim if allowed_ids is None else max(lex_lim, len(ids))
        )
    lexical = [
        (doc_id, score)
        for doc_id, score in lexical
        if allowed_ids is None or doc_id in allowed_ids
    ][:lex_lim]
    lexical_hits = [doc_id for doc_id, _ in lexical]
    if allowed_ids is not None:
        idx = [i for i, doc_id in enumerate(ids) if doc_id in allowed_ids]
        if not idx:
            return fuse([(lex_name, [], weights[0]), (den_name, [], weights[1])])
        sims = vectors[idx] @ q_vec
        dense_local = np.argsort(-sims, kind="stable")[:den_lim]
        dense_trace = [(ids[idx[i]], float(sims[i])) for i in dense_local]
    else:
        sims = vectors @ q_vec
        dense_local = np.argsort(-sims, kind="stable")[:den_lim]
        dense_trace = [(ids[i], float(sims[i])) for i in dense_local]
    dense_hits = [doc_id for doc_id, _ in dense_trace]
    fused = fuse(
        [(lex_name, lexical_hits, weights[0]), (den_name, dense_hits, weights[1])],
        raw_scores={lex_name: dict(lexical), den_name: dict(dense_trace)},
    )
    if trace is not None:
        trace[lex_name] = [
            {"doc_id": doc_id, "score": float(score)} for doc_id, score in lexical
        ]
        trace[den_name] = [
            {"doc_id": doc_id, "score": score} for doc_id, score in dense_trace
        ]
        trace[f"{trace_key}_rrf"] = fused.ids()[:lex_lim]
        if lexical_query is not None and lexical_query != query:
            trace["lexical_query"] = lexical_query
    return fused


def _hybrid_rank(**kwargs: Any) -> list[str]:
    """Id-only view of `_hybrid_fusion` (same order as the pre-candidate `weighted_rrf` path)."""

    return _hybrid_fusion(**kwargs).ids()


def _person_signal(
    index: IndexBundle,
    leaf: dict[str, Any],
    *,
    tokens: set[str],
    labels: set[str],
) -> tuple[float, dict[str, Any]]:
    """원문 화자·짧은 멘션을 붙이고, 긴 붙여넣기만의 이름 언급은 낮게 본다."""

    if not tokens:
        return 0.0, {"speaker_hit": False, "short": 0, "paste": 0}

    speakers = {str(s) for s in (leaf.get("speakers") or [])}
    speaker_hit = bool(speakers & labels) or any(
        any(_token_in_text(sp, tok, labels=labels) for tok in tokens) for sp in speakers
    )
    start = int(leaf.get("start") or 0)
    end = int(leaf.get("end") or start)
    short = 0
    paste = 0
    for i in range(start, min(end, len(index.messages))):
        msg = index.messages[i]
        content = str(msg.get("content") or "")
        actor = str(msg.get("actor_label") or "")
        hit = any(_token_in_text(content, tok, labels=labels) for tok in tokens) or any(
            _token_in_text(actor, tok, labels=labels) for tok in tokens
        )
        if not hit:
            if actor in labels:
                speaker_hit = True
            continue
        if actor in labels or any(
            _token_in_text(actor, tok, labels=labels) for tok in tokens
        ):
            speaker_hit = True
        if len(content) >= 380:
            paste += 1
        else:
            short += 1

    score = 0.0
    if speaker_hit:
        score += 2.4
    score += 1.15 * min(short, 3)
    score += 0.12 * min(paste, 2)
    if short == 0 and paste > 0 and not speaker_hit:
        score -= 1.3
    return score, {"speaker_hit": speaker_hit, "short": short, "paste": paste}


def _recency_bonus(
    index: IndexBundle,
    doc_id: str,
    *,
    half_life_days: float,
    strength: float,
) -> float:
    if strength <= 0:
        return 0.0
    ts = index.leaf_end_ts.get(doc_id)
    if ts is None:
        return -0.15 * strength
    tmax = recency_anchor_ts(index)
    age_days = max(0.0, (tmax - ts) / 86400.0)
    # 최근일수록 1에 가깝고, half_life에서 ~0.5
    freshness = math.exp(-math.log(2) * age_days / max(half_life_days, 1.0))
    return strength * (freshness - 0.35)


def _rerank_leaves(
    index: IndexBundle,
    ranked: list[str],
    *,
    query: str,
    pool: int,
    top_k: int,
    trace: dict[str, Any] | None = None,
    allowed_ids: set[str] | None = None,
    intent: dict[str, Any] | None = None,
) -> list[str]:
    """R1 · 최근·인물 가산. `allowed_ids`(방 필터)는 주입 후보에도 강제한다.

    `intent`는 플래너 LLM 출력(time_scope/question_kind/person_targets). 없으면 regex fallback."""

    intent = resolve_intent(query, intent, index.cfg)
    aliases = _alias_table(index.cfg)
    if intent.get("targets") is not None:
        # 플래너가 사람 이름을 골라 줬다. 별칭 표는 그 이름을 라벨로 넓히는 데만 쓴다.
        targets = _clean_planner_targets(list(intent["targets"]))
    else:
        ignore = set(DEFAULT_ALIAS_IGNORE) | {
            str(x).strip() for x in (index.cfg.get("person_alias_ignore") or []) if str(x).strip()
        }
        targets = _query_targets(query, aliases, ignore)
    labels = _target_labels(targets, aliases)
    identity_re = _identity_re(index.cfg)
    tokens = set(targets) | {x for x in labels if len(x) <= 12}
    tokens = {t for t in tokens if 2 <= len(t) <= 12}
    if trace is not None:
        trace["intent"] = intent

    half_life = float(index.cfg.get("recency_half_life_days", 40))
    # 점수 스케일: base(hybrid 순위)는 [0, 1]. 가산항은 그 스케일에 상대적으로 정한다.
    # 일반 질의에서 recency는 동점 정리 수준(≈±0.03)이어야 한다 — 이전 0.35는 hybrid
    # 순위 전체 범위(≈0.05)의 7배라 R1이 "최신순 정렬"로 붕괴했다 (07 실험 로그 R1 악화).
    if intent["recent"]:
        recent_strength = 1.0
    elif intent["intro"]:
        recent_strength = 0.0
    else:
        recent_strength = 0.05
    person_strength = 1.0 if targets else 0.0
    intro_strength = 0.55 if intent["intro"] else 0.0
    base_mult = 1.5 if intent["intro"] else 1.0
    effective_pool = pool
    if intent["recent"]:
        effective_pool = max(pool, 100)
    if intent["intro"]:
        effective_pool = max(pool, 80)

    # 최근 질문: 하이브리드 top에 안 잡힌 최신·인물 leaf를 후보에 섞는다.
    candidate_ids = list(ranked)
    if intent["recent"]:
        extras: list[str] = []
        for doc_id in index.leaves_by_recency:
            if allowed_ids is not None and doc_id not in allowed_ids:
                continue
            leaf = index.leaf_by_id.get(doc_id)
            if leaf is None:
                continue
            if targets:
                person, _meta = _person_signal(
                    index, leaf, tokens=tokens, labels=labels
                )
                if person < 0.8:
                    continue
            extras.append(doc_id)
            if len(extras) >= effective_pool:
                break
        candidate_ids = list(dict.fromkeys(extras + ranked))
    elif intent["intro"] and targets:
        # 소개: config identity_terms(소속·전공 등 정체성 단서)가 있는 인물 leaf를 후보에 섞는다.
        extras_scored: list[tuple[float, str]] = []
        for doc_id, sdoc in index.summary_by_id.items():
            if allowed_ids is not None and doc_id not in allowed_ids:
                continue
            summary = str(sdoc.get("summary") or "")
            leaf = index.leaf_by_id.get(doc_id)
            if leaf is None:
                continue
            blob = f"{leaf.get('body') or ''}\n{summary}"
            if identity_re is None or not identity_re.search(blob):
                continue
            person, _meta = _person_signal(index, leaf, tokens=tokens, labels=labels)
            if person < 0.8:
                continue
            strong = 1.5 if identity_re and identity_re.search(blob) else 0.0
            extras_scored.append((person + strong, doc_id))
        extras_scored.sort(key=lambda item: -item[0])
        extras = [doc_id for _, doc_id in extras_scored[:effective_pool]]
        candidate_ids = list(dict.fromkeys(extras + ranked))

    rrf_rank = {doc_id: i for i, doc_id in enumerate(ranked)}
    if intent["recent"] or intent["intro"]:
        injected = [doc_id for doc_id in candidate_ids if doc_id not in rrf_rank][
            :effective_pool
        ]
        consider = list(dict.fromkeys(injected + ranked[:effective_pool]))
    else:
        consider = ranked[: max(effective_pool, top_k)]

    scored: list[tuple[float, int, str]] = []
    n_consider = max(1, len(consider))
    for cand_i, doc_id in enumerate(consider):
        leaf = index.leaf_by_id.get(doc_id)
        if leaf is None:
            continue
        if allowed_ids is not None and doc_id not in allowed_ids:
            continue
        rank = rrf_rank.get(doc_id, LEXICAL_LIMIT + cand_i)
        # hybrid 1위 → 1.0, 풀 마지막 → 0에 가깝게. 주입된 후보(hybrid에 없음)는 0.
        base = max(0.0, 1.0 - rank / n_consider)
        person, meta = _person_signal(index, leaf, tokens=tokens, labels=labels)
        if intent["intro"] and targets and person < 0.8:
            person -= 0.8
        if (intent["politics"] or intent["prefer_missing_over_guess"]) and targets:
            if meta["short"] == 0 and not meta["speaker_hit"]:
                person -= 2.0
        recency = _recency_bonus(
            index, doc_id, half_life_days=half_life, strength=recent_strength
        )
        paste_penalty = 0.0
        if intent["politics"] or intent["prefer_missing_over_guess"]:
            body = str(leaf.get("body") or "")
            if len(body) > 1200:
                paste_penalty -= 0.6
        identity_bonus = 0.0
        if intent["intro"]:
            summary = ""
            if doc_id in index.summary_by_id:
                summary = str(index.summary_by_id[doc_id].get("summary") or "")
            blob = f"{leaf.get('body') or ''}\n{summary}"
            if identity_re is None:
                identity_bonus = 0.0
            elif identity_re.search(blob):
                identity_bonus = 2.4 / 6.0
            elif targets:
                identity_bonus = -1.5 / 6.0
        person_n = scaled_person(person)
        score = (
            base * base_mult
            + person_strength * person_n
            + recency
            + intro_strength * max(person_n, 0.0)
            + paste_penalty
            + identity_bonus
        )
        scored.append((score, rank, doc_id))

    scored.sort(key=lambda item: (-item[0], item[1]))
    leaf_size = int(index.cfg.get("leaf_size", 8))
    min_gap = leaf_size * 3 if intent["recent"] else max(4, leaf_size // 2)
    picked: list[str] = []
    picked_starts: list[int] = []
    adjacent_skipped: list[str] = []
    for score, rank, doc_id in scored:
        leaf = index.leaf_by_id.get(doc_id) or {}
        start = int(leaf["start"]) if leaf.get("start") is not None else -10_000
        if any(abs(start - s) < min_gap for s in picked_starts):
            adjacent_skipped.append(doc_id)
            continue
        picked.append(doc_id)
        picked_starts.append(start)
        if len(picked) >= top_k:
            break
    if len(picked) < top_k:
        for _, _, doc_id in scored:
            if doc_id not in picked:
                picked.append(doc_id)
            if len(picked) >= top_k:
                break
    if trace is not None:
        trace["heuristic"] = {
            "effective_pool": effective_pool,
            "consider_n": len(consider),
            "candidate_n": len(candidate_ids),
            "targets": sorted(targets)[:24],
            "min_gap": min_gap,
            "scored": [
                {"doc_id": doc_id, "score": round(float(score), 6), "rrf_rank": rank}
                for score, rank, doc_id in scored[:80]
            ],
            "picked": picked,
            "adjacent_skipped": adjacent_skipped[:40],
        }
    return picked


RERANK_DOC_FORMATS = ("body", "summary_body", "speaker_body", "time_body", "full")


def _rerank_document(index: IndexBundle, doc_id: str, doc_format: str, *, limit: int = 2000) -> str:
    """Text handed to the reranker for one leaf (§24 representation experiment).

    body          raw leaf text only
    summary_body  summary + body (2026-09-17 default; the ablation harness used body only)
    speaker_body  [Speaker] labels + body
    time_body     [Time] end date + body
    full          speaker + time + summary + body"""

    leaf = index.leaf_by_id.get(doc_id) or {}
    body = str(leaf.get("body") or "")
    summary = str((index.summary_by_id.get(doc_id) or {}).get("summary") or "")
    speakers = ", ".join(str(x) for x in (leaf.get("speakers") or [])[:8])
    end_ts = index.leaf_end_ts.get(doc_id)
    when = datetime.fromtimestamp(end_ts).strftime("%Y-%m-%d %H:%M") if end_ts else ""
    parts: list[str] = []
    if doc_format in {"speaker_body", "full"} and speakers:
        parts.append(f"[Speaker]\n{speakers}")
    if doc_format in {"time_body", "full"} and when:
        parts.append(f"[Time]\n{when}")
    if doc_format in {"summary_body", "full"} and summary:
        parts.append(summary if doc_format == "summary_body" else f"[Summary]\n{summary}")
    if doc_format == "full":
        parts.append(f"[Content]\n{body}")
    else:
        parts.append(body)
    blob = "\n".join(p for p in parts if p).strip()
    return (blob or body)[:limit]


def _pre_rerank_dedup(index: IndexBundle, pool_ids: list[str]) -> tuple[list[str], list[str]]:
    """Keep the best-ranked leaf of each adjacent run (same room, |start diff| < leaf_size)."""

    leaf_size = int(index.cfg.get("leaf_size", 8) or 8)
    kept: list[str] = []
    kept_pos: list[tuple[str, int]] = []
    dropped: list[str] = []
    for doc_id in pool_ids:
        leaf = index.leaf_by_id.get(doc_id) or {}
        room = str(leaf.get("source_room_id") or "")
        start = int(leaf["start"]) if leaf.get("start") is not None else -10_000
        if any(r == room and abs(start - s0) < leaf_size for r, s0 in kept_pos):
            dropped.append(doc_id)
            continue
        kept.append(doc_id)
        kept_pos.append((room, start))
    return kept, dropped


def _rank_leaf_ids(
    index: IndexBundle,
    ranked: list[str],
    *,
    query: str,
    pool: int,
    top_k: int,
    ranking: str,
    remaining_budget: float,
    trace: dict[str, Any] | None,
    allowed_ids: set[str] | None = None,
    intent: dict[str, Any] | None = None,
    auto: bool = False,
) -> list[str]:
    """Apply one ranking variant. Unknown modes are rejected before this call.

    R2 실패 시 R0(hybrid) 풀 순서를 그대로 쓴다. `auto=True`면 reason만 계약 이름(`rerank_failed`)으로
    남긴다. R1(`heuristic`)은 명시 요청 때만 돌며 auto의 fallback이 아니다 (2026-09-17 퇴역)."""

    if ranking == "hybrid":
        if trace is not None:
            trace["applied"] = "hybrid"
        return ranked[:top_k]
    if ranking == "rerank":
        rr_cfg = retrieval_config(index.cfg)["rerank"]
        pool_ids = ranked[:pool]
        if bool(rr_cfg.get("pre_dedup", False)):
            # §26 cheap adjacency dedup before the reranker: neighbouring leaves of one conversation
            # stretch would otherwise occupy several reranker slots. Final span merge still runs.
            pool_ids, pre_dropped = _pre_rerank_dedup(index, pool_ids)
            if trace is not None:
                trace["rerank_pre_dedup_dropped"] = len(pre_dropped)
        doc_format = str(rr_cfg.get("doc_format") or "summary_body")
        docs: list[str] = [_rerank_document(index, doc_id, doc_format) for doc_id in pool_ids]
        try:
            secrets = load_secrets()
            rerank_budget = max(0.02, min(0.08, max(remaining_budget, 0.02) * 0.4))
            requested_model = configured_models(index.cfg)["rerank"]
            rr_client = RerankClient(
                secrets,
                model=requested_model,
                budget_usd=rerank_budget,
                max_request_cost_usd=min(0.05, rerank_budget),
                base_url=secrets.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
                ledger=index.rerank_budget_ledger,
            )
            result = rr_client.rerank(
                query,
                docs,
                top_n=min(len(docs), max(top_k, min(pool, len(docs)))),
            )
            ordered = [pool_ids[i] for i in result.indices if 0 <= i < len(pool_ids)]
            extra = [doc_id for doc_id in pool_ids if doc_id not in ordered]
            # relevance floor: 잡담·오프도메인 질의에 top_k를 억지로 채우지 않는다.
            min_score = resolve_rerank_min_score(index.cfg, result.model or requested_model)
            kept: list[str] = []
            seen: set[str] = set()
            for idx, score in zip(result.indices, result.scores):
                if not (0 <= idx < len(pool_ids)):
                    continue
                if float(score) < min_score:
                    continue
                doc_id = pool_ids[idx]
                if doc_id in seen:
                    continue
                seen.add(doc_id)
                kept.append(doc_id)
            abstained = bool(result.indices) and not kept
            if trace is not None:
                trace["applied"] = "rerank"
                trace["rerank_doc_format"] = doc_format
                trace["rerank_min_score"] = min_score
                trace["rerank_kept_n"] = len(kept)
                trace["abstained"] = abstained
                trace["rerank_model"] = result.model
                trace["rerank_model_requested"] = requested_model
                trace["rerank_cost_usd"] = result.cost_usd
                trace["rerank_latency_ms"] = result.latency_ms
                trace["pool_n"] = len(pool_ids)
                trace["pool_ids"] = pool_ids
                trace["rerank_indices"] = list(result.indices)
                trace["rerank_scores"] = [round(float(s), 6) for s in result.scores]
                trace["rerank_order"] = ordered
                trace["doc_chars"] = [len(d) for d in docs]
                trace["rerank_docs"] = [
                    {"doc_id": pool_ids[i], "chars": len(docs[i]), "preview": docs[i][:400]}
                    for i in range(len(pool_ids))
                ]
                if result.billed:
                    trace["rerank_billed"] = result.billed
                if result.generation_id:
                    try:
                        gen = rr_client.fetch_generation(result.generation_id, retries=2)
                    except Exception:  # noqa: BLE001
                        gen = None
                    if gen:
                        trace["rerank_generation"] = gen
            return kept[:top_k]
        except (RerankError, OSError, TimeoutError, RuntimeError) as exc:
            fb_mode, fb_reason = ("hybrid", "rerank_failed")
            if auto:
                fb_mode, fb_reason = fallback_ranking(intent)
            if trace is not None:
                trace["applied"] = f"{fb_mode}_fallback"
                trace["rerank_error"] = type(exc).__name__
                # 오류 본문에 문서·질의가 섞일 수 있으므로 종류만 남긴다 (privacy).
                trace["rerank_model_requested"] = configured_models(index.cfg)["rerank"]
                trace["pool_n"] = len(pool_ids)
                trace["pool_ids"] = pool_ids
                trace["ranking_auto_reason"] = fb_reason
            # R0 order, not the guarantee-reordered pool prefix.
            fusion_ids = list((trace or {}).get("fusion_ids") or pool_ids)
            return fusion_ids[:top_k]
    if trace is not None:
        trace["applied"] = "heuristic"
    return _rerank_leaves(
        index,
        ranked,
        query=query,
        pool=pool,
        top_k=top_k,
        trace=trace,
        allowed_ids=allowed_ids,
        intent=intent,
    )


def _apply_hybrid_floor(
    index: IndexBundle,
    ranked: list[str],
    *,
    q_vec: np.ndarray,
    rank_mode: str,
) -> tuple[list[str], dict[str, Any]]:
    if rank_mode not in {"hybrid", "heuristic"}:
        return ranked, {}
    try:
        min_cos = float(index.cfg.get("hybrid_min_cos") or 0.0)
    except (TypeError, ValueError):
        min_cos = 0.0
    if min_cos <= 0:
        return ranked, {}
    id_to_idx = {doc_id: i for i, doc_id in enumerate(index.leaf_ids)}
    kept: list[str] = []
    for doc_id in ranked:
        i = id_to_idx.get(doc_id)
        if i is None:
            continue
        if float(index.leaf_vectors[i] @ q_vec) >= min_cos:
            kept.append(doc_id)
    abstained = bool(ranked) and not kept
    return kept, {"hybrid_min_cos": min_cos, "abstained": abstained, "hybrid_kept_n": len(kept)}


def _scope_allowed_ids(
    index: IndexBundle,
    *,
    mode: str,
    source_room_id: str | None,
    block_ids: list[str] | None,
) -> set[str] | None:
    """Channel / block scope. Applied before any retrieval — other rooms are never candidates."""

    allowed: set[str] | None = None
    if block_ids is not None:
        allowed = leaf_ids_for_blocks(index.blocks, block_ids)
        if source_room_id:
            room_ids = {
                str(d["doc_id"])
                for d in (index.baseline if mode == "baseline" else index.leaves)
                if d.get("source_room_id") == source_room_id
            }
            allowed &= room_ids
    elif source_room_id:
        docs = index.baseline if mode == "baseline" else index.leaves
        allowed = {str(d["doc_id"]) for d in docs if d.get("source_room_id") == source_room_id}
    return allowed


def _canon_query(text: str) -> str:
    """Collapse whitespace so duplicate planner rewrites of the same user query are dropped."""

    return " ".join((text or "").split())


def _soft_event_recipe(cfg: Mapping[str, Any]) -> tuple[dict[str, Any], SoftEventRecipe] | None:
    """Return the opt-in runtime recipe without changing the stable leaf recipe."""

    raw = retrieval_config(cfg).get("soft_event") or {}
    if not bool(raw.get("enabled", False)):
        return None
    pooling = str(raw.get("pooling") or "leaf_mean")
    if pooling != "leaf_mean":
        raise PlaygroundError("soft_event.pooling must be 'leaf_mean'")
    partial_scope = str(raw.get("partial_scope") or "skip")
    # A partially scoped event would mix authorized and unauthorized leaves.
    # Until another policy is explicitly designed, fail closed rather than
    # silently returning a mixed event.
    if partial_scope != "skip":
        raise PlaygroundError("soft_event.partial_scope must be 'skip'")
    recipe = SoftEventRecipe.from_value(
        {
            "version": raw.get("version", "soft-event-v1"),
            "gap_minutes": raw.get("gap_minutes", 30),
            "min_leaves": raw.get("min_leaves", 1),
            "max_leaves": raw.get("max_leaves", 8),
            "max_chars": raw.get("max_chars", 4000),
        }
    )
    return dict(raw), recipe


def _soft_event_leaf_times(index: IndexBundle) -> dict[str, dict[str, float | None]]:
    """Build explicit leaf intervals from source messages; never infer missing times."""

    out: dict[str, dict[str, float | None]] = {}
    messages = index.messages
    for leaf in index.leaves:
        doc_id = str(leaf["doc_id"])
        try:
            start = int(leaf.get("start"))
            end = int(leaf.get("end")) - 1
        except (TypeError, ValueError):
            start, end = -1, -1
        start_value = None
        end_value = None
        if 0 <= start < len(messages):
            start_value = _parse_ts(messages[start].get("parsed_time"))
        if 0 <= end < len(messages):
            end_value = _parse_ts(messages[end].get("parsed_time"))
        out[doc_id] = {"start_time": start_value, "end_time": end_value}
    return out


def _soft_event_search_text(index: IndexBundle, event: Any, recipe: SoftEventRecipe) -> str:
    """Create event-only search text; it is never passed to evidence packing."""

    bodies = [
        str((index.leaf_by_id.get(leaf_id) or {}).get("body") or "").strip()
        for leaf_id in event.leaf_ids
    ]
    body = "\n".join(part for part in bodies if part)
    body = body[: recipe.max_chars]
    summary = str(getattr(event, "summary_text", "") or "").strip()
    if summary and len(body) < recipe.max_chars:
        room = recipe.max_chars - len(body)
        suffix = summary[: max(0, room - (1 if body else 0))]
        if suffix:
            body = f"{body}\n{suffix}" if body else suffix
    return body


def _soft_event_runtime(index: IndexBundle, *, use_summary: bool) -> _SoftEventRuntime:
    """Build/cache a runtime-derived event view for the current retrieval recipe."""

    recipe_config = _soft_event_recipe(index.cfg)
    if recipe_config is None:
        raise PlaygroundError("soft event runtime requested while disabled")
    raw, recipe = recipe_config
    summary_enabled = bool(use_summary and retrieval_config(index.cfg)["summaries"].get("enabled", True))
    cache_key = json.dumps(
        {
            "recipe": recipe.as_dict(),
            "summary_enabled": summary_enabled,
            "summary_ids": list(index.summary_ids) if summary_enabled else [],
            "pooling": str(raw.get("pooling") or "leaf_mean"),
        },
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )
    cached = index.soft_event_runtime_cache.get(cache_key)
    if isinstance(cached, _SoftEventRuntime):
        return cached

    summaries = None
    if summary_enabled:
        summaries = {
            str(doc_id): str(row.get("summary") or "")
            for doc_id, row in index.summary_by_id.items()
            if isinstance(row, Mapping) and str(row.get("summary") or "").strip()
        }
    result = build_soft_events(
        index.leaves,
        recipe=recipe,
        leaf_vectors=index.leaf_vectors,
        leaf_times=_soft_event_leaf_times(index),
        summaries=summaries,
    )
    search_texts = {
        event.event_id: _soft_event_search_text(index, event, recipe)
        for event in result.events
    }
    lexical = ThreadSafeLexicalIndex(str(index.cfg["lexical"]))
    lexical.add_many([(event_id, text) for event_id, text in search_texts.items()])
    vector_rows: list[tuple[str, tuple[float, ...]]] = [
        (event.event_id, event.vector)
        for event in result.events
        if event.vector is not None
    ]
    if vector_rows:
        vector_ids = [event_id for event_id, _ in vector_rows]
        vectors = np.asarray([vector for _, vector in vector_rows], dtype=np.float32)
    else:
        vector_ids = []
        vectors = np.zeros((0, int(index.leaf_vectors.shape[1])), dtype=np.float32)
    runtime = _SoftEventRuntime(
        recipe=recipe,
        result=result,
        lexical=lexical,
        event_ids=[event.event_id for event in result.events],
        event_vectors=vectors,
        event_vector_ids=vector_ids,
        search_texts=search_texts,
    )
    # Keep one current recipe only.  This makes a runtime config change a
    # deterministic rebuild and avoids retaining old in-memory FTS databases.
    index.soft_event_runtime_cache.clear()
    index.soft_event_runtime_cache[cache_key] = runtime
    return runtime


def _soft_event_candidates(
    index: IndexBundle,
    *,
    query: str,
    q_vec: np.ndarray,
    allowed: set[str] | None,
    use_summary: bool,
    trace: dict[str, Any],
) -> tuple[list[tuple[str, list[str], float]], dict[str, dict[str, float]]]:
    """Search events and project each hit to dense-ranked stable leaf IDs."""

    raw, _recipe = _soft_event_recipe(index.cfg) or ({}, None)
    runtime = _soft_event_runtime(index, use_summary=use_summary)
    event_by_id = runtime.result.event_by_id
    eligible: list[str] = []
    partial_skipped = 0
    for event_id in runtime.event_ids:
        members = set(event_by_id[event_id].leaf_ids)
        if allowed is None:
            eligible.append(event_id)
        elif members <= allowed:
            eligible.append(event_id)
        elif members & allowed:
            partial_skipped += 1

    event_limit = max(0, int(raw.get("event_limit", 40) or 0))
    max_leaf_ids = max(0, int(raw.get("max_leaf_ids_per_event", 8) or 0))
    eligible_set = set(eligible)
    lexical_hits: list[tuple[str, float]] = []
    if event_limit and eligible:
        with index.lock:
            lexical_hits = runtime.lexical.search(
                query,
                limit=max(event_limit, len(runtime.event_ids)),
            )
        lexical_hits = [
            (event_id, float(score))
            for event_id, score in lexical_hits
            if event_id in eligible_set
        ][:event_limit]
    dense_hits: list[tuple[str, float]] = []
    if event_limit and runtime.event_vector_ids and eligible:
        dense_hits = _dense_hits(
            runtime.event_vectors,
            runtime.event_vector_ids,
            q_vec,
            allowed_ids=eligible_set,
            limit=event_limit,
        )

    id_to_row = {leaf_id: row for row, leaf_id in enumerate(index.leaf_ids)}

    def project(hits: list[tuple[str, float]]) -> tuple[list[str], dict[str, float], dict[str, list[str]]]:
        projected: list[str] = []
        child_scores: dict[str, float] = {}
        projected_map: dict[str, list[str]] = {}
        for event_id, _event_score in hits:
            event = event_by_id.get(event_id)
            if event is None:
                continue
            # Use the soft_events public projection API for membership, then
            # impose the query-child dense order required by this adapter.
            member_ids = project_event_ranks(runtime.result, [event_id])
            children = [
                (leaf_id, float(index.leaf_vectors[id_to_row[leaf_id]] @ q_vec))
                for leaf_id in member_ids
                if allowed is None or leaf_id in allowed
            ]
            child_position = {leaf_id: pos for pos, leaf_id in enumerate(member_ids)}
            children.sort(key=lambda item: (-item[1], child_position[item[0]]))
            child_ids = [leaf_id for leaf_id, _score in children[:max_leaf_ids]]
            projected_map[event_id] = child_ids
            for leaf_id, score in children[:max_leaf_ids]:
                if leaf_id not in projected:
                    projected.append(leaf_id)
                    child_scores[leaf_id] = score
        return projected, child_scores, projected_map

    lexical_projected, lexical_raw, lexical_map = project(lexical_hits)
    dense_projected, dense_raw, dense_map = project(dense_hits)
    trace["soft_event"] = {
        "recipe": runtime.recipe.as_dict(),
        "count": {
            "events": len(runtime.result.events),
            "eligible": len(eligible),
        },
        "hits": {
            "lexical": [event_id for event_id, _score in lexical_hits],
            "dense": [event_id for event_id, _score in dense_hits],
        },
        "projected_map": {
            "lexical": lexical_map,
            "dense": dense_map,
        },
        "partial_scope_skipped": partial_skipped,
    }
    weight = float(raw.get("rrf_weight", 0.35) or 0.0)
    runs: list[tuple[str, list[str], float]] = []
    raw_scores: dict[str, dict[str, float]] = {}
    if lexical_projected and weight > 0:
        runs.append(("soft_event_lexical", lexical_projected, weight))
        raw_scores["soft_event_lexical"] = lexical_raw
    if dense_projected and weight > 0:
        runs.append(("soft_event_dense", dense_projected, weight))
        raw_scores["soft_event_dense"] = dense_raw
    return runs, raw_scores


def _generate_candidates(
    index: IndexBundle,
    client: PlayClient,
    query: str,
    *,
    q_vec: np.ndarray,
    intent: dict[str, Any],
    scope: set[str] | None,
    use_summary: bool,
    pool: int,
    lex_lim: int,
    den_lim: int,
    time_scale: float = 1.0,
    lexical_query: str | None = None,
    time_reference: RetrievalTimeReference | None = None,
    limit_scale: float = 1.0,
) -> tuple[FusionResult, set[str] | None, dict[str, Any], dict[str, Any]]:
    """R0 v2 candidate generation: scope → independent retrievers → fusion with provenance.

    Returns (fusion, allowed_ids_after_time_scope, trace, meta) where meta carries what auto
    needs (targets requested/resolved). Every retriever honours `scope`; person/alias/multi-
    query failures are isolated so lexical+dense always remain."""

    cfg = index.cfg
    rc = retrieval_config(cfg)
    weights = [float(w) for w in cfg["rrf_weights"]]
    trace: dict[str, Any] = {}
    # 시점 제약은 scope다: 창 밖 leaf는 후보가 되지 않는다 (가산 아님). v1 recent / v2 time 모두.
    allowed, time_meta = narrow_time_ids(
        index, scope, intent, scale=time_scale, reference=time_reference
    )
    trace.update(time_meta)
    if lexical_query is None:
        lexical_query, alias_extra = _lexical_query_expanded(index, query, intent)
        if alias_extra:
            trace["lexical_query_expanded"] = True
            trace["alias_labels_added"] = alias_extra[:24]

    leaf_fusion = _hybrid_fusion(
        fts=index.leaf_fts, vectors=index.leaf_vectors, ids=index.leaf_ids, query=query, q_vec=q_vec,
        weights=weights, lock=index.lock, trace=trace, trace_key="leaf", allowed_ids=allowed,
        lexical_limit=lex_lim, dense_limit=den_lim, lexical_query=lexical_query,
    )
    outer_runs: list[tuple[str, list[str], float]] = [("leaf", leaf_fusion.ids(), 1.0)]
    children: dict[str, FusionResult] = {"leaf": leaf_fusion}
    raw_scores: dict[str, dict[str, float]] = {}
    retrievers_used = ["leaf_lexical", "leaf_dense"]
    if (
        use_summary
        and bool(rc["summaries"].get("enabled", True))
        and index.summary_fts is not None
        and index.summary_vectors is not None
        and index.summary_ids
    ):
        summary_fusion = _hybrid_fusion(
            fts=index.summary_fts, vectors=index.summary_vectors, ids=index.summary_ids, query=query, q_vec=q_vec,
            weights=weights, lock=index.lock, trace=trace, trace_key="summary", allowed_ids=allowed,
            lexical_limit=lex_lim, dense_limit=den_lim, lexical_query=lexical_query,
        )
        summary_w = float(cfg.get("summary_rrf_weight", 1.0))
        # 중첩 RRF: (leaf lexical⊕dense) ⊕ (summary lexical⊕dense). 순서는 예전 weighted_rrf와 같고
        # provenance만 retriever 단위로 남는다.
        outer_runs.append(("summary", summary_fusion.ids(), summary_w))
        children["summary"] = summary_fusion
        retrievers_used += ["summary_lexical", "summary_dense"]
        trace["summary_rrf_weight"] = summary_w

    # Soft events are a derived candidate source only.  They search an
    # in-memory event view, then project back to stable leaf IDs before fusion;
    # final ranking, expansion, and evidence packing remain leaf/raw based.
    soft_event_configured = _soft_event_recipe(cfg) is not None
    if soft_event_configured:
        event_runs, event_raw = _soft_event_candidates(
            index,
            query=query,
            q_vec=q_vec,
            allowed=allowed,
            use_summary=use_summary,
            trace=trace,
        )
        if event_runs:
            outer_runs.extend(event_runs)
            raw_scores.update(event_raw)
            retrievers_used.extend(name for name, _ids, _weight in event_runs)

    # person / alias는 가산이 아니라 후보원이다 (R1 person bonus 대체). 실패해도 R0는 남는다.
    speaker_universe_ids: set[str] = set()
    alias_ids: set[str] = set()
    targets = _intent_targets(index, query, intent)
    targets_resolved = False
    if targets and (bool(rc["person"].get("enabled", True)) or bool(rc["alias"].get("enabled", True))):
        try:
            # Resolve only among speakers the caller may see (defect: other rooms' namesakes used to
            # count toward ambiguity). `resolve_in_scope: false` restores the legacy global lookup.
            person_scope = allowed if bool(rc["person"].get("resolve_in_scope", True)) else None
            person_identity = _identity_for(index) if _person_identity_mode(cfg) != "config" else None
            person_res = resolve_persons(
                index, targets, _alias_table_for(index), token_in_text=_token_in_text,
                max_speakers_per_target=int(rc["person"].get("max_speakers_per_target") or 3),
                allowed=person_scope, identity=person_identity,
            )
            trace["persons"] = person_res.to_trace()
            targets_resolved = person_res.resolved
            if bool(rc["person"].get("enabled", True)) and person_res.resolved:
                sp_fusion, speaker_universe_ids = speaker_fusion(
                    index, person_res, query=query, q_vec=q_vec, allowed=allowed,
                    limit=int(rc["person"].get("limit") or 60), weights=weights, lock=index.lock,
                    identity=person_identity,
                )
                if sp_fusion.ordered:
                    outer_runs.append(("speaker", sp_fusion.ids(), float(rc["person"].get("rrf_weight") or 1.0)))
                    children["speaker"] = sp_fusion
                    retrievers_used += ["speaker_lexical", "speaker_dense"]
                trace["speaker_pool_n"] = len(speaker_universe_ids)
            if bool(rc["alias"].get("enabled", True)) and person_res.labels:
                al = alias_run(index, person_res, allowed=allowed,
                               limit=int(rc["alias"].get("limit") or 60), lock=index.lock)
                if al:
                    outer_runs.append(("alias_lexical", [d for d, _ in al], float(rc["alias"].get("rrf_weight") or 0.5)))
                    raw_scores["alias_lexical"] = dict(al)
                    alias_ids = {d for d, _ in al}
                    retrievers_used.append("alias_lexical")
        except Exception as exc:  # noqa: BLE001 — 실패 격리: lexical+dense R0는 남는다
            trace["person_retriever_error"] = type(exc).__name__

    # multi-query (§13): q0는 항상 원문. 플래너 semantic_query 또는 (없을 때) memory.search.query를
    # q1 dense로, key_terms를 q2 lexical로. 원문을 rewrite로 바꾸지 않는다. LLM 추가 호출 없음.
    mq = rc["multi_query"]
    queries_used: dict[str, str] = {"q0": query}
    if bool(mq.get("enabled", True)) and intent and int(mq.get("max_queries") or 3) > 1:
        try:
            sem_q = _q1_text(query, intent, mq)
            if sem_q and sem_q != query and len(queries_used) < int(mq.get("max_queries") or 3):
                t_q1 = time.perf_counter()
                q1_vec, q1_info = query_vector(index, client, sem_q)
                trace["q1_embed_ms"] = _elapsed_ms(t_q1)
                trace["q1_embed_source"] = q1_info["source"]
                q1_runs: list[tuple[str, list[str], float]] = []
                q1_raw: dict[str, dict[str, float]] = {}
                leaf_d = _dense_hits(index.leaf_vectors, index.leaf_ids, q1_vec, allowed_ids=allowed, limit=den_lim)
                q1_runs.append(("q1_dense", [d for d, _ in leaf_d], 1.0))
                q1_raw["q1_dense"] = dict(leaf_d)
                if "summary" in children and index.summary_vectors is not None:
                    sum_d = _dense_hits(index.summary_vectors, index.summary_ids, q1_vec, allowed_ids=allowed, limit=den_lim)
                    q1_runs.append(("q1_summary_dense", [d for d, _ in sum_d], float(cfg.get("summary_rrf_weight", 1.0))))
                    q1_raw["q1_summary_dense"] = dict(sum_d)
                q1_fusion = fuse(q1_runs, raw_scores=q1_raw)
                if q1_fusion.ordered:
                    outer_runs.append(("q1", q1_fusion.ids(), float(mq.get("semantic_rrf_weight") or 1.0)))
                    children["q1"] = q1_fusion
                    retrievers_used += [name for name, _, _ in q1_runs]
                queries_used["q1"] = sem_q
            terms = [str(t).strip() for t in (intent.get("key_terms") or []) if str(t).strip()]
            if terms and len(queries_used) < int(mq.get("max_queries") or 3):
                q2 = " ".join(terms[:12])
                if q2 != query:
                    lex2 = _lexical_hits(index.leaf_fts, q2, ids_n=len(index.leaf_ids), allowed_ids=allowed, limit=lex_lim, lock=index.lock)
                    if lex2:
                        outer_runs.append(("q2_lexical", [d for d, _ in lex2], float(mq.get("key_terms_rrf_weight") or 0.5)))
                        raw_scores["q2_lexical"] = dict(lex2)
                        retrievers_used.append("q2_lexical")
                    queries_used["q2"] = q2
        except Exception as exc:  # noqa: BLE001 — 실패 격리
            trace["multi_query_error"] = type(exc).__name__
    trace["queries_used"] = queries_used

    if len(outer_runs) == 1:
        fusion = leaf_fusion
    else:
        fusion = fuse(outer_runs, children=children, raw_scores=raw_scores)
        trace["merged_rrf"] = fusion.ids()[:80]
    # Single messages find the leaf that holds a one-line fact an averaged leaf vector buries. The run
    # joins the finished fusion as one more weighted list, so every existing candidate keeps the place
    # it came with. Disabled (default) or without artifacts: none of this runs and nothing is traced.
    msg_dense = rc["message_dense"]
    message_on = bool(msg_dense.get("enabled", False)) and index.message_vectors is not None
    msg_raw: dict[str, dict[str, float]] = {}
    msg_ids: list[str] = []
    if message_on:
        t_msg = time.perf_counter()
        msg_ids, msg_scores, msg_scored = message_dense_run(
            index.message_vectors, index.message_leaf, index.leaf_ids, q_vec,
            allowed_ids=allowed, limit=int(int(msg_dense.get("limit") or 200) * limit_scale),
        )
        trace["message_dense"] = {
            "messages_scored": msg_scored, "leaves": len(msg_ids),
            "ms": _elapsed_ms(t_msg), "dims": int(index.message_vectors.shape[1]),
        }
        if msg_ids:
            msg_raw = {MESSAGE_RUN_NAME: msg_scores}
            msg_weight = float(msg_dense.get("rrf_weight") or MESSAGE_RRF_WEIGHT)
            fusion = fuse(
                [("fused", fusion.ids(), 1.0), (MESSAGE_RUN_NAME, msg_ids, msg_weight)],
                children={"fused": fusion}, raw_scores={**raw_scores, **msg_raw},
            )
            retrievers_used.append(MESSAGE_RUN_NAME)
    for cand in fusion.ordered:
        if cand.doc_id in speaker_universe_ids:
            cand.flags["speaker_match"] = True
        if cand.doc_id in alias_ids:
            cand.flags["alias_match"] = True
    trace["retrievers_used"] = retrievers_used
    trace.update(fusion.trace(limit=pool))
    trace["candidate_limits"] = {"lexical": lex_lim, "dense": den_lim, "pool": pool, "time_scale": time_scale}
    counts_for_confidence = bool(
        soft_event_configured and rc["soft_event"].get("counts_for_confidence", False)
    )
    if soft_event_configured and not counts_for_confidence and any(
        source_family(name) == "soft_event" for name, _ids, _weight in outer_runs
    ):
        # The final fusion keeps soft-event candidates for ranking.  Auto
        # confidence, however, must be calculated from the exact same leaf /
        # summary / person / query runs as the disabled baseline; merely
        # excluding the family from agreement still lets event runs change
        # candidate_count, top order, and margin.
        confidence_runs = [
            (name, ids, weight)
            for name, ids, weight in outer_runs
            if source_family(name) != "soft_event"
        ]
        confidence_raw = {
            name: values
            for name, values in raw_scores.items()
            if source_family(name) != "soft_event"
        }
        if len(confidence_runs) == 1 and confidence_runs[0][0] == "leaf":
            confidence_fusion = leaf_fusion
        else:
            confidence_fusion = fuse(
                confidence_runs,
                children=children,
                raw_scores=confidence_raw,
            )
        if msg_ids:
            confidence_fusion = fuse(
                [("fused", confidence_fusion.ids(), 1.0), (MESSAGE_RUN_NAME, msg_ids, msg_weight)],
                children={"fused": confidence_fusion}, raw_scores={**confidence_raw, **msg_raw},
            )
        # The confidence view is rebuilt after speaker/alias flags were added
        # to the final fusion, so mirror those non-event flags explicitly.
        for cand in confidence_fusion.ordered:
            if cand.doc_id in speaker_universe_ids:
                cand.flags["speaker_match"] = True
            if cand.doc_id in alias_ids:
                cand.flags["alias_match"] = True
    else:
        confidence_fusion = fusion
    meta = {
        "targets_requested": bool(targets),
        "targets_resolved": targets_resolved,
        "soft_event_counts_for_confidence": counts_for_confidence,
        "confidence_fusion": confidence_fusion,
    }
    return fusion, allowed, trace, meta


def retrieve(
    index: IndexBundle,
    client: PlayClient,
    query: str,
    *,
    top_k: int,
    mode: str = "leaf_parent",
    q_vec: np.ndarray | None = None,
    ranking: str | None = None,
    remaining_budget: float = 0.2,
    trace: dict[str, Any] | None = None,
    source_room_id: str | None = None,
    block_ids: list[str] | None = None,
    intent: dict[str, Any] | None = None,
    time_reference: RetrievalTimeReference | None = None,
) -> list[dict[str, Any]]:
    """Scope → R0 v2 candidates → (auto: assess → finish / recovery / rerank) → floor → span merge → parent expand.

    `auto` no longer picks a mode before searching. Concrete modes (`hybrid`/`heuristic`/`rerank`)
    still run the requested ranking on the same candidate pool (ablation)."""

    cfg = index.cfg
    intent = resolve_intent(query, intent, index.cfg)
    try:
        wanted_mode = normalize_ranking(
            ranking if ranking is not None else cfg.get("ranking"), default="heuristic"
        )
    except RankingError as exc:
        raise PlaygroundError(str(exc)) from exc
    auto = wanted_mode == "auto"
    t_embed = time.perf_counter()
    embed_info: dict[str, Any] = {"source": "given", "hedged": False}
    if q_vec is None:
        # q1(semantic_query)을 q0와 동시에 요청해 둔다. 후보 생성 단계에서 기다림 없이 쓴다.
        if mode == "leaf_parent":
            q1_text = _q1_text(query, intent, retrieval_config(cfg)["multi_query"])
            if q1_text:
                prefetch_query_vector(index, client, q1_text)
        q_vec, embed_info = query_vector(index, client, query)
    else:
        q_vec = np.asarray(q_vec, dtype=np.float32)
        q_vec /= np.linalg.norm(q_vec) + 1e-9
        q_vec = fit_query_vector(index, q_vec)
    embed_ms = _elapsed_ms(t_embed)
    if trace is not None:
        trace["embed_source"] = embed_info["source"]
        trace["embed_hedged"] = bool(embed_info.get("hedged"))
    weights = [float(w) for w in cfg["rrf_weights"]]
    char_limit = int(cfg["document_char_limit"])
    rc = retrieval_config(cfg)
    pool = pool_size(cfg, top_k=top_k)
    lex_lim, den_lim = search_limits(cfg)
    scope = _scope_allowed_ids(index, mode=mode, source_room_id=source_room_id, block_ids=block_ids)

    if mode == "baseline":
        if not index.baseline:
            raise PlaygroundError("baseline 인덱스가 없다. config baseline_enabled 후 prepare")
        lexical_query, _ = _lexical_query_expanded(index, query, intent)
        ranked = _hybrid_rank(
            fts=index.baseline_fts, vectors=index.baseline_vectors, ids=index.baseline_ids, query=query, q_vec=q_vec,
            weights=weights, lock=index.lock, allowed_ids=scope, lexical_limit=lex_lim, dense_limit=den_lim,
            lexical_query=lexical_query,
        )
        out = []
        for doc_id in ranked[:top_k]:
            doc = index.baseline_by_id[doc_id]
            out.append(
                {
                    "doc_id": doc_id,
                    **_hit_provenance(index, doc),
                    "excerpt": doc["body"][:char_limit],
                    "observation_ids": list(doc.get("observation_ids") or []),
                    "mode": "baseline",
                }
            )
        return out

    use_summary = mode in {"leaf_parent", "leaf_parent_summary"}
    t_hybrid = time.perf_counter()
    fusion, allowed, rank_trace_seed, gen_meta = _generate_candidates(
        index, client, query, q_vec=q_vec, intent=intent, scope=scope, use_summary=use_summary,
        pool=pool, lex_lim=lex_lim, den_lim=den_lim, time_reference=time_reference,
    )

    # ---- auto v2: observe the pool, then decide finish / recovery / rerank ----
    auto_reason = ""
    recovery_performed = False
    rank_mode = wanted_mode
    if auto:
        thresholds = dict(rc["auto"])
        rerank_ok = bool(rc["rerank"].get("enabled", True)) and _rerank_available()
        excluded_families = (
            set()
            if gen_meta.get("soft_event_counts_for_confidence", False)
            else None
        )
        confidence_fusion = (
            fusion
            if gen_meta.get("soft_event_counts_for_confidence", False)
            else gen_meta.get("confidence_fusion") or fusion
        )
        assessment = assess(
            confidence_fusion,
            thresholds=thresholds,
            targets_requested=gen_meta["targets_requested"],
            targets_resolved=gen_meta["targets_resolved"],
            excluded_families=excluded_families,
        )
        rank_trace_seed["auto_confidence"] = assessment.to_trace()
        decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget)
        if decision["recover"] and bool(rc["recovery"].get("enabled", True)):
            # Recovery (§28): R2 cannot invent candidates, so widen generation once before reranking.
            rec = rc["recovery"]
            scale = float(rec.get("limit_scale") or 1.5)
            fusion2, allowed2, trace2, meta2 = _generate_candidates(
                index, client, query, q_vec=q_vec, intent=intent, scope=scope, use_summary=use_summary,
                pool=pool_size(cfg, top_k=top_k, recovery=True),
                lex_lim=int(lex_lim * scale), den_lim=int(den_lim * scale),
                time_scale=float(rec.get("time_window_scale") or 2.0),
                time_reference=time_reference,
                limit_scale=scale,
            )
            recovery_performed = True
            rank_trace_seed["recovery"] = {
                "before": assessment.to_trace(),
                "candidate_union_n_before": rank_trace_seed.get("candidate_union_n"),
                "candidate_union_n_after": trace2.get("candidate_union_n"),
                "limits": trace2.get("candidate_limits"),
                "time_pool_n": trace2.get("recent_pool_n") or trace2.get("time_pool_n"),
            }
            fusion, allowed, gen_meta = fusion2, allowed2, meta2
            rank_trace_seed.update(trace2)
            pool = pool_size(cfg, top_k=top_k, recovery=True)
            excluded_families = (
                set()
                if gen_meta.get("soft_event_counts_for_confidence", False)
                else None
            )
            confidence_fusion = (
                fusion
                if gen_meta.get("soft_event_counts_for_confidence", False)
                else gen_meta.get("confidence_fusion") or fusion
            )
            assessment = assess(
                confidence_fusion,
                thresholds=thresholds,
                targets_requested=gen_meta["targets_requested"],
                targets_resolved=gen_meta["targets_resolved"],
                excluded_families=excluded_families,
            )
            rank_trace_seed["auto_confidence"] = assessment.to_trace()
            rank_trace_seed["recovery"]["after"] = assessment.to_trace()
            decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget,
                                      recovery_done=True)
        elif decision["recover"]:
            decision = decide_ranking(assessment.level, rerank_available=rerank_ok, remaining_budget_usd=remaining_budget,
                                      recovery_done=True)
            decision["reason"] = "rerank_low_confidence" if decision["mode"] == "rerank" else "no_rerank"
        rank_mode = str(decision["mode"])
        auto_reason = str(decision["reason"])
    cp = rc["candidate_pool"]
    fusion_ids = fusion.ids()
    if bool(rc["soft_event"].get("enabled", False)) and not bool(
        rc["soft_event"].get("reserve_in_pool", False)
    ):
        pool_order, guaranteed, guaranteed_by_source = guarantee_pool(
            fusion,
            pool=pool,
            per_source_min=int(cp.get("per_source_min") or 0),
            reserve_max_frac=float(cp.get("reserve_max_frac") or 0.3),
            excluded_sources={"soft_event"},
        )
    else:
        pool_order, guaranteed, guaranteed_by_source = guarantee_pool(
            fusion,
            pool=pool,
            per_source_min=int(cp.get("per_source_min") or 0),
            reserve_max_frac=float(cp.get("reserve_max_frac") or 0.3),
        )
    candidate_pool_ids = pool_order[:pool]
    rank_trace_seed["fusion_ids"] = fusion_ids[:pool]
    rank_trace_seed["candidate_pool_ids"] = candidate_pool_ids
    rank_trace_seed["candidate_pool_n"] = len(candidate_pool_ids)
    rank_trace_seed["pool_guaranteed_ids"] = list(guaranteed)
    rank_trace_seed["pool_guaranteed_n"] = len(guaranteed)
    rank_trace_seed["pool_guaranteed_by_source"] = {k: list(v) for k, v in guaranteed_by_source.items()}
    hybrid_ms = _elapsed_ms(t_hybrid)

    rank_mode = normalize_ranking(rank_mode, default="heuristic")
    # R0 ranking follows fusion order. R2 membership is the guaranteed pool (may include
    # low-weight retriever hits that the fused cut would have dropped).
    ranked_for_final = candidate_pool_ids if rank_mode == "rerank" else fusion_ids
    rank_trace: dict[str, Any] = {
        **rank_trace_seed,
        "hybrid_top10": fusion_ids[:10],
        "hybrid_ids": candidate_pool_ids,  # eval pool_recall = R2-eligible pool, not fusion cut
        "hybrid_n": len(fusion_ids),
        "pool": pool,
        "top_k": top_k,
        "embed_ms": embed_ms,
        "hybrid_ms": hybrid_ms,
        "query_chars": len(query or ""),
        "pool_hit_n": len(candidate_pool_ids),
        "recovery_performed": recovery_performed,
    }
    if auto:
        rank_trace["ranking_auto_reason"] = auto_reason
        rank_trace["auto_decision"] = rank_mode
    t_rank = time.perf_counter()
    ranked = _rank_leaf_ids(
        index,
        ranked_for_final,
        query=query,
        pool=pool,
        top_k=max(top_k * 4, pool) if rank_mode != "rerank" else top_k,
        ranking=rank_mode,
        remaining_budget=remaining_budget,
        trace=rank_trace,
        allowed_ids=allowed,
        intent=intent,
        auto=auto,
    )
    rank_trace["rerank_performed"] = rank_trace.get("applied") == "rerank"
    if rank_mode == "rerank" and rank_trace.get("abstained"):
        pass
    else:
        ranked, abs_meta = _apply_hybrid_floor(
            index, ranked, q_vec=q_vec, rank_mode=rank_mode
        )
        rank_trace.update(abs_meta)
        if not ranked and abs_meta.get("abstained"):
            rank_trace["abstained"] = True
    ranked, merge_map = finalize_ranked_ids(
        index,
        ranked,
        top_k=top_k,
        intent=intent,
        mode=str(cfg.get("dedup_mode") or "merge"),
    )
    rank_trace["dedup_mode"] = str(cfg.get("dedup_mode") or "merge")
    rank_trace["merged_leaf_ids"] = {k: v for k, v in merge_map.items() if v}
    if rank_trace.get("rerank_latency_ms") is not None:
        rank_trace["rerank_ms"] = round(float(rank_trace["rerank_latency_ms"]), 1)
    else:
        rank_trace["rerank_ms"] = _elapsed_ms(t_rank)
    if trace is not None:
        trace.update(rank_trace)
        trace["ranking"] = rank_mode
        trace["ranking_requested"] = wanted_mode
        trace["intent"] = intent
        if not ranked:
            trace["abstain_reason"] = "rerank_floor" if rank_trace.get("abstained") else "empty_in_allowed_scope"

    t_expand = time.perf_counter()
    out = []
    id_to_idx = {doc_id: i for i, doc_id in enumerate(index.leaf_ids)}
    cand_by_id = fusion.by_id
    time_scope = resolve_time_scope(
        (intent or {}).get("time") if isinstance((intent or {}).get("time"), dict) else None,
        reference=time_reference,
        recent=bool(intent and intent.get("recent")),
    )
    time_allows = None
    if time_scope.hard and (time_scope.lo is not None or time_scope.hi is not None):
        lo_ts = time_scope.lo.timestamp() if time_scope.lo is not None else None
        hi_ts = time_scope.hi.timestamp() if time_scope.hi is not None else None
        time_allows = make_time_filter(lo_ts, hi_ts, ts_of=lambda row: _parse_ts(row.get("parsed_time")))
    allowed_rooms = None
    allowed_obs = None
    if source_room_id:
        allowed_rooms = {source_room_id}
    elif block_ids is not None:
        allowed_obs = set()
        for lid in scope or []:
            leaf = index.leaf_by_id.get(lid)
            if leaf:
                allowed_obs.update(str(x) for x in (leaf.get("observation_ids") or []))
    segments: list[dict[str, Any]] = []
    extras_by_id: dict[str, list[str]] = {}
    for doc_id in ranked:
        extras = merge_map.get(doc_id) or []
        extras_by_id[doc_id] = extras
        segments.append(merged_span(index, doc_id, extras) if extras else index.leaf_by_id[doc_id])
    packed = pack_evidence_set(
        index.messages,
        segments,
        before=int(cfg["parent_expand_before"]),
        after=int(cfg["parent_expand_after"]),
        with_prefix=bool(cfg.get("context_prefix", True)),
        doc_char_limit=char_limit,
        total_char_limit=int(cfg.get("evidence_total_char_limit") or char_limit * max(1, top_k)),
        allowed_observation_ids=allowed_obs,
        allowed_rooms=allowed_rooms,
        time_allows=time_allows,
        dedupe_visible=True,
    )
    packed_by_id = {str(d["doc_id"]): d for d in packed["docs"]}
    for doc_id in ranked:
        extras = extras_by_id.get(doc_id) or []
        source_leaf = index.leaf_by_id[doc_id]
        expanded = packed_by_id.get(doc_id) or {}
        summary_text = ""
        if use_summary and doc_id in index.summary_by_id:
            summary_text = str(index.summary_by_id[doc_id].get("summary") or "")
        date_label = _leaf_source_date(index, source_leaf)
        date_label = _context_only_date(index, expanded, source_leaf) or date_label
        out.append(
            {
                "doc_id": doc_id,
                **_hit_provenance(index, source_leaf),
                "excerpt": expanded.get("excerpt") or "",
                "summary": summary_text,
                "observation_ids": list(expanded.get("observation_ids") or []),
                "visible_observation_ids": list(expanded.get("visible_observation_ids") or []),
                "visible_spans": list(expanded.get("visible_spans") or []),
                "packing_status": expanded.get("status") or "insufficient",
                "leaf_observation_ids": list(source_leaf.get("observation_ids") or []),
                "merged_leaf_ids": extras,
                "mode": "leaf_parent_summary"
                if summary_text
                else ("leaf_only" if mode == "leaf_only" else "leaf_parent"),
                "span": f"{expanded.get('start', source_leaf.get('start'))}-{int(expanded.get('end') or source_leaf.get('end') or 1)-1}",
                "date": date_label,
                "speakers": list(source_leaf.get("speakers") or []),
                "sources": dict((cand_by_id.get(doc_id).sources if doc_id in cand_by_id else {})),
                "intent": intent,
                "dense_cos": float(index.leaf_vectors[id_to_idx[doc_id]] @ q_vec)
                if doc_id in id_to_idx
                else None,
            }
        )
    expand_ms = _elapsed_ms(t_expand)
    if trace is not None:
        trace["expand_ms"] = expand_ms
        trace["visible_spans"] = list(packed.get("visible_spans") or [])
        trace["visible_observation_ids"] = list(packed.get("visible_observation_ids") or [])
        trace["candidate_observation_ids"] = sorted(
            {oid for lid in (rank_trace.get("candidate_pool_ids") or []) for oid in (index.leaf_by_id.get(lid) or {}).get("observation_ids") or []}
        )
        trace["time_reference"] = time_reference.to_trace() if time_reference is not None else None
        trace["retrieve_ms"] = round(
            float(trace.get("embed_ms") or 0)
            + float(trace.get("hybrid_ms") or 0)
            + float(trace.get("rerank_ms") or 0)
            + expand_ms,
            1,
        )
    return out


ANSWER_SYSTEM = """너는 로컬 테스트용 채널 봇이다.
페르소나처럼 짧게 한국어로 답한다. 근거 문장을 길게 복붙하지 마라.
기억이 있으면 친구처럼 자연스럽게 섞고, 없거나 흐릿하면 대충 받아치거나 농담으로 넘겨도 된다.
분석 라벨·면책 문구를 붙이지 마라.

규칙:
- 화자를 구분하라. 봇이 조사해 온 글 속 이름은 그 사람 본인 발화가 아니다.
- 질문 유형 힌트가 recent면 근거 날짜의 최근 것을 우선하라.
- 질문 유형이 intro면 소속·별명·반복 특성 위주.
실험 recipe: leaf 검색(+요약) → parent 원문 확장. 제품 확정 전이다."""


TOOL_BLOCK_MAX = 3500


def _json_len(value: Any) -> int:
    return len(json.dumps(value, ensure_ascii=False))


def _fit_tail(value: Any, budget: int) -> Any:
    """``value`` cut from its end until its JSON takes at most ``budget`` characters (Go chat.fitTail).

    A list drops its last items (one is kept and cut instead), an object cuts its largest value first,
    a text loses its end ("…").
    """

    if _json_len(value) <= budget:
        return value
    if isinstance(value, str):
        keep = len(value) - (_json_len(value) - budget) - 1
        return value[:keep] + "…" if keep > 0 else "…"
    if isinstance(value, list):
        items = list(value)
        while len(items) > 1 and _json_len(items) > budget:
            items.pop()
        if _json_len(items) > budget:
            items[0] = _fit_tail(items[0], budget - 2)
        elif len(items) < len(value):
            rest = budget - _json_len(items) - 2  # ", " before the next item
            if rest > 40:
                items.append(_fit_tail(value[len(items)], rest))
                if _json_len(items) > budget:
                    items.pop()
        return items
    if isinstance(value, dict):
        out = dict(value)
        while _json_len(out) > budget:
            big, size = "", 0
            for k, v in out.items():
                n = _json_len(v)
                if n > size or (n == size and k < big):
                    big, size = k, n
            if size <= 3:
                break
            out[big] = _fit_tail(out[big], max(1, size - (_json_len(out) - budget)))
            if _json_len(out[big]) >= size:
                del out[big]
        return out
    return value


def _fill_shares(sizes: list[int], room: int) -> list[int]:
    """Results smaller than an even share keep everything; the rest split what remains evenly."""

    shares = [0] * len(sizes)
    done = [False] * len(sizes)
    left = room
    open_n = sum(1 for n in sizes if n > 0)
    while open_n > 0:
        even = left // open_n
        settled = False
        for i, n in enumerate(sizes):
            if n > 0 and not done[i] and n <= even:
                shares[i], done[i] = n, True
                left -= n
                open_n -= 1
                settled = True
        if not settled:
            for i, n in enumerate(sizes):
                if n > 0 and not done[i]:
                    shares[i] = even
            break
    return shares


def _tool_block(usable: list[dict[str, Any]], other: list[dict[str, Any]], limit: int = TOOL_BLOCK_MAX) -> str:
    """Tool results for the answer prompt, as Go's chat.toolJSON does.

    When they do not fit in ``limit`` characters, every result keeps its tool, ok, status and error,
    and the data that does not fit is cut from its end (a list loses its last items, a text its last
    words), so the block stays valid JSON, the first-ranked hits stay whole and one long result does
    not hide the outcome of the tools after it.
    """

    raw = json.dumps({"usable": usable, "not_success": other}, ensure_ascii=False)
    if len(raw) <= limit:
        return raw
    results = list(usable) + list(other)
    envs: list[dict[str, Any]] = []
    sizes: list[int] = []
    for r in results:
        m: dict[str, Any] = {"tool": r.get("tool"), "ok": bool(r.get("ok"))}
        if r.get("status"):
            m["status"] = r["status"]
        if r.get("error"):
            m["error"] = str(r["error"])[:200]
        envs.append(m)
        sizes.append(_json_len(r["data"]) if r.get("data") else 0)

    def block(shares: list[int]) -> str:
        for i, r in enumerate(results):
            m = envs[i]
            m.pop("data", None)
            m.pop("truncated", None)
            if not r.get("data"):
                continue
            if sizes[i] <= shares[i]:
                m["data"] = r["data"]
            elif shares[i] > 0:
                m["data"], m["truncated"] = _fit_tail(r["data"], shares[i]), True
            else:
                m["truncated"] = True
        return json.dumps({"usable": envs[: len(usable)], "not_success": envs[len(usable) :]}, ensure_ascii=False)

    empty = [0] * len(results)
    room = limit - len(block(empty)) - 16 * len(results)
    while room > 0:
        out = block(_fill_shares(sizes, room))
        if len(out) <= limit:
            return out
        room -= len(out) - limit
    return block(empty)[:limit]


def _build_chat_messages(
    index: IndexBundle,
    *,
    message: str,
    history: list[dict[str, str]],
    hits: list[dict[str, Any]],
    system_prompt: str | None = None,
    tool_results: list[dict[str, Any]] | None = None,
    speaker: str | None = None,
    session_summary: str = "",
    rag_state: str = "",
    intent: dict[str, Any] | None = None,
) -> list[dict[str, str]]:
    cfg = index.cfg
    intent = resolve_intent(message, intent, index.cfg)
    intent_bits = [k for k in ("recent", "intro", "prefer_missing_over_guess") if intent.get(k)]
    if intent.get("kind") and intent["kind"] not in {"other", "person_intro"}:
        intent_bits.append(str(intent["kind"]))
    intent_line = ",".join(intent_bits) if intent_bits else "general"
    rag_note = {
        "not_attempted": "(이 질문엔 채널 기억을 찾지 않았다)",
        "candidates": "",
        "empty_in_allowed_scope": "(딱히 떠오르는 기억이 없다 — 누가 뭘 했는지 짐작하지 말고(묻는 사람도 후보가 아니다), 기억 안 난다고 가볍게 말한 뒤 아는 사람 있는지 방에 물어봐라)",
        "insufficient_authorized_candidates": "(관련된 기억이 흐릿하다 — 아는 척 대충 넘기거나 농담으로 받아쳐도 된다)",
        "memory_search_disabled": "(채널 기억 검색이 꺼져 있다)",
    }.get(rag_state, "(딱히 떠오르는 기억이 없다 — 누가 뭘 했는지 짐작하지 말고(묻는 사람도 후보가 아니다), 기억 안 난다고 가볍게 말한 뒤 아는 사람 있는지 방에 물어봐라)")
    evidence_blocks = []
    for i, hit in enumerate(hits, 1):
        speakers = ", ".join(hit.get("speakers") or []) or "?"
        date = hit.get("date") or "?"
        status = hit.get("packing_status") or ""
        evidence_blocks.append(
            f"[{i}] date={date} speakers={speakers} room={hit.get('source_room_id')} "
            f"leaf={hit['doc_id']} span={hit.get('span')} visible={status}\n"
            f"{hit.get('excerpt') or ''}"
        )
    evidence = "\n\n".join(evidence_blocks) if evidence_blocks else rag_note
    if evidence_blocks:
        evidence += "\n\n" + MEMORY_TAIL
    derived_summaries = []
    for i, hit in enumerate(hits, 1):
        if hit.get("summary"):
            derived_summaries.append(f"[{i}] {hit['summary']}")
    summary_block = "\n".join(derived_summaries) if derived_summaries else "(생성 요약 없음)"
    usable_tools = [r for r in (tool_results or []) if is_success(r)]
    other_tools = [r for r in (tool_results or []) if not is_success(r)]
    tool_block = _tool_block(usable_tools, other_tools) if tool_results else "(없음)"
    who = (speaker or "사용자").strip() or "사용자"
    messages: list[dict[str, str]] = [
        {"role": "system", "content": (system_prompt or ANSWER_SYSTEM)}
    ]
    for turn in history[-int(cfg["max_history_turns"]) :]:
        role = turn.get("role")
        content = (turn.get("content") or "").strip()
        if role in {"user", "assistant"} and content:
            messages.append({"role": role, "content": content[:1500]})
    messages.append(
        {
            "role": "user",
            "content": (
                f"Discord 채널 메시지. 화자 표시명: {who}\n"
                f"질문유형힌트: {intent_line}\n"
                f"<세션요약 파생정보 사실근거아님>\n{session_summary or '(이번 창 요약 없음)'}\n</세션요약>\n"
                f"<도구결과>\n{tool_block}\n</도구결과>\n"
                f"<생성요약 원문아님>\n{summary_block}\n</생성요약>\n"
                f"<근거원문>\n{evidence}\n</근거원문>\n\n"
                f"{who}: {message}"
            ),
        }
    )
    return messages


# Go: language pack chat.memory_tail (ko)
MEMORY_TAIL = "(위 기억 조각은 재료다. 대사를 줄줄이 옮기거나 \"누가 '~' 이랬음\" 목록으로 늘어놓지 말고, 묶어서 네 말로 결론부터, 목록보다 말하듯이 전해라. 따옴표 인용은 꼭 필요할 때 한 번만.)"


def _indexed_rooms(index: IndexBundle) -> list[str]:
    rooms = [str(r) for r in (index.cfg.get("rooms") or []) if r]
    return rooms


# 서버 프로세스 하나 = 테스트 실행 하나. 로그에서 "이번에 켠 서버의 턴"만 고를 때 쓴다.
SERVER_RUN_ID = f"run-{time.strftime('%Y%m%d-%H%M%S')}-{os.getpid()}"


def _log_context(
    index: IndexBundle,
    bundle: Mapping[str, Any] | None,
    rank_trace: Mapping[str, Any],
    hits: list[dict[str, Any]],
    *,
    conversation_id: str | None,
    timing: Mapping[str, Any] | None = None,
    answer: str | None = None,
) -> dict[str, Any]:
    """로그·화면 공용: 실행 맥락(run)과 턴 진단(diagnosis). 값은 설정·trace에서만 읽는다."""

    b = bundle or {}
    ctx: dict[str, Any] = {
        "run": {
            "server_run_id": SERVER_RUN_ID,
            "dataset": index.cfg.get("experiment"),
            "collection": index.cfg.get("collection"),
            "data_dir": DATA.name,
            "conversation_id": conversation_id or None,
            "channel_id": b.get("channel_id"),
        }
    }
    if b:
        ctx["diagnosis"] = build_diagnosis(
            rank_trace=rank_trace,
            intent=b.get("intent"),
            hits=hits,
            rag_used=bool(b.get("rag_used")),
            rag_state=b.get("rag_state"),
            memory_requirement=b.get("memory_requirement"),
            tools=b.get("tool_results"),
            timing=timing if timing is not None else b.get("timing"),
            answer=answer,
            cfg=index.cfg,
        )
    return ctx


def _config_snap(cfg: dict[str, Any]) -> dict[str, Any]:
    return {
        "ranking": cfg.get("ranking"),
        "models": configured_models(cfg),
        "top_k": cfg.get("top_k"),
        "retrieve_pool": cfg.get("retrieve_pool"),
        "leaf_size": cfg.get("leaf_size"),
        "summary_enabled": bool(cfg.get("summary_enabled", False)),
        "summary_rrf_weight": cfg.get("summary_rrf_weight"),
        "parent_expand_before": cfg.get("parent_expand_before"),
        "parent_expand_after": cfg.get("parent_expand_after"),
        "recency_half_life_days": cfg.get("recency_half_life_days"),
        "lexical": cfg.get("lexical"),
        "rrf_weights": cfg.get("rrf_weights"),
        "document_char_limit": cfg.get("document_char_limit"),
        "max_history_turns": cfg.get("max_history_turns"),
        "lexical_limit": cfg.get("lexical_limit"),
        "dense_limit": cfg.get("dense_limit"),
        "embed_query_instruction": cfg.get("embed_query_instruction"),
        "alias_expand_lexical": cfg.get("alias_expand_lexical"),
        "dedup_mode": cfg.get("dedup_mode"),
        "rerank_min_score": cfg.get("rerank_min_score"),
        "hybrid_min_cos": cfg.get("hybrid_min_cos"),
        "recent_window_days": cfg.get("recent_window_days"),
        "recent_window_leaves": cfg.get("recent_window_leaves"),
        "recency_anchor": cfg.get("recency_anchor"),
        "leaf_split": cfg.get("leaf_split"),
        "baseline_enabled": cfg.get("baseline_enabled"),
    }


def _prompt_stats(messages: list[dict[str, str]]) -> dict[str, Any]:
    parts = [
        {"role": str(m.get("role") or ""), "chars": len(str(m.get("content") or ""))}
        for m in messages
    ]
    return {
        "n": len(messages),
        "chars": sum(p["chars"] for p in parts),
        "parts": parts,
    }


def _history_stats(history: list[dict[str, str]], *, keep: int) -> dict[str, Any]:
    kept = history[-keep:] if keep else []
    return {
        "n": len(history),
        "kept": len(kept),
        "chars": sum(len(str(t.get("content") or "")) for t in kept),
    }


def _request_time_reference(cfg: Mapping[str, Any] | None) -> RetrievalTimeReference:
    raw = (cfg or {}).get("time_reference") if isinstance(cfg, dict) else None
    if isinstance(raw, dict) and raw.get("now"):
        text = str(raw["now"]).strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        now = datetime.fromisoformat(text)
        mode = str(raw.get("mode") or "replay")
        if mode not in {"live", "replay"}:
            mode = "replay"
        if mode == "live" and (now.tzinfo is None or now.utcoffset() is None):
            now = now.replace(tzinfo=timezone.utc)
        return RetrievalTimeReference(mode=mode, now=now, source=str(raw.get("source") or "cfg"))
    return RetrievalTimeReference(mode="live", now=datetime.now(timezone.utc), source="request")


def _planner_time_reference(ref: RetrievalTimeReference) -> PlannerTimeReference:
    tz_name = str(ref.tz) if ref.tz is not None else "UTC"
    return PlannerTimeReference(
        now=ref.now.isoformat(),
        timezone=tz_name,
        basis="live" if ref.mode == "live" else "replay",
    )


def _prepare_bot_turn(
    index: IndexBundle,
    client: PlayClient,
    *,
    message: str,
    ranking: str,
    remaining: float,
    history: list[dict[str, str]] | None = None,
    session_summary: str = "",
    conversation_id: str | None = None,
    time_reference: RetrievalTimeReference | None = None,
) -> dict[str, Any]:
    runtime = ensure_runtime(
        DATA, cfg=index.cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
    )
    cid = str(runtime.get("active_channel_id") or channel_id_from_room(_indexed_rooms(index)[0]))
    channel = get_channel(runtime, cid)
    if not channel.get("indexed"):
        raise PlaygroundError("인덱스가 없는 채널이다. 테스트 데이터셋이 있는 채널만 켠다")
    yield_status = "planning"
    t_plan = time.perf_counter()
    plan_raw = ""
    bang = parse_bang_command(message)
    ok, why = respond_allows(
        str(channel.get("respond") or ""),
        is_command=bang is not None,
        mentioned=("마사몽" in message or "@" in message),
    )
    if not ok:
        raise PlaygroundError(why)
    from bot_runtime import gate_tool, memory_search_gate, resolve_tool_defaults
    policy = tool_policy(runtime)
    enabled_tools = list(policy["enabled"])
    clock = time_reference or _request_time_reference(index.cfg)
    memory_requirement: str | None
    intent: dict[str, Any] | None = None
    conv_id = str(conversation_id or "ephemeral")
    if bang:
        plan = [bang]
        memory_requirement = "none"
        plan_ms = _elapsed_ms(t_plan)
    else:
        # q0는 항상 원문이라 계획 결과를 기다릴 필요가 없다: 플래너와 겹쳐 임베딩한다.
        prefetch_query_vector(index, client, message)

        def _planner_chat(messages: list[dict[str, str]], max_tokens: int) -> str:
            return client.chat(
                messages,
                max_tokens=max_tokens,
                temperature=0.0,
                reasoning="low",
                model=configured_models(index.cfg)["planner"],
            )

        planned = plan_turn(
            message,
            channel_id=cid,
            conversation_id=conv_id,
            chat=_planner_chat,
            history=PlannerHistory(channel_id=cid, conversation_id=conv_id, turns=list(history or [])),
            session_summary=session_summary or "",
            time_reference=_planner_time_reference(clock),
            tools_enabled=enabled_tools,
            max_tokens=PLANNER_MAX_TOKENS,
        )
        plan_ms = _elapsed_ms(t_plan)
        plan_raw = planned.get("raw") or ""
        parsed = planned.get("parsed") or parse_plan(plan_raw)
        assessment = planned.get("assessment") or {}
        memory_requirement = assessment.get("memory_requirement") if assessment.get("memory_requirement") is not None else parsed.get("memory_requirement")
        intent = planned.get("intent") or intent_from_plan(parsed)
        plan = [
            {"tool_name": s["tool_name"], "parameters": s.get("parameters") or {}, "state": s.get("state")}
            for s in (assessment.get("steps") or [])
        ] or list(parsed.get("tool_plan") or [])
        if assessment.get("memory_state") == "unparsed":
            memory_requirement = memory_requirement or "optional"
        if assessment.get("time") and intent is not None:
            intent = {**intent, "time": assessment["time"]}
    tool_defaults = resolve_tool_defaults(runtime, channel)
    tool_results: list[dict[str, Any]] = []
    t_tools = time.perf_counter()
    for step in plan:
        name = canonicalize_tool(str(step.get("tool_name") or ""))
        if name in {"memory.search", "general_chat"}:
            continue
        if step.get("state") == "needs_input":
            tool_results.append(
                {
                    "tool": name,
                    "ok": False,
                    "status": "needs_input",
                    "error": "needs_input",
                    "missing": list(step.get("missing") or []),
                }
            )
            continue
        denied = gate_tool(runtime, name)
        if denied:
            tool_results.append(denied)
            continue
        tool_results.append(
            execute_named_tool(
                name,
                step.get("parameters"),
                data_dir=DATA,
                index_meta=index.meta,
                tool_defaults=tool_defaults,
            )
        )
    tools_ms = _elapsed_ms(t_tools)

    rank_trace: dict[str, Any] = {}
    hits: list[dict[str, Any]] = []
    mem_gate = memory_search_gate(runtime, memory_requirement)
    rag_wanted = mem_gate["action"] == "search" and needs_local_rag(
        [s for s in plan if s.get("state") in (None, "runnable")], memory_requirement
    )
    rag_state = "not_attempted"
    if mem_gate["action"] == "refuse":
        rag_state = "memory_search_disabled"
        rag_wanted = False
    search_query = ""
    if rag_wanted:
        view = channel.get("view") if isinstance(channel.get("view"), dict) else {}
        view_blocks = [str(x) for x in (view.get("blocks") or []) if str(x).strip()]
        room = ""
        for candidate in _indexed_rooms(index):
            if channel_id_from_room(candidate) == cid:
                room = candidate
                break
        rewrite = memory_query([s for s in plan if isinstance(s, dict)])
        if rewrite and intent is not None:
            intent = {**intent, "memory_search_query": rewrite}
        search_query = message
        retrieve_kw: dict[str, Any] = {
            "top_k": int(index.cfg["top_k"]),
            "mode": "leaf_parent",
            "ranking": ranking,
            "remaining_budget": remaining,
            "trace": rank_trace,
            "intent": intent,
            "time_reference": clock,
        }
        if view_blocks:
            retrieve_kw["block_ids"] = view_blocks
        elif room:
            retrieve_kw["source_room_id"] = room
        else:
            retrieve_kw["block_ids"] = []
        hits = retrieve(index, client, search_query, **retrieve_kw)
        if (
            not hits
            and not rank_trace.get("abstained")
            and isinstance(intent, dict)
            and (
                intent.get("recent")
                or (isinstance(intent.get("time"), dict) and (intent["time"].get("kind") or "any") != "any")
            )
        ):
            # the planner's time window found nothing: it is often a guess (a year-less date read as this
            # year, "recently" for a months-old talk), so search once more without it (Go: chat.Respond)
            first = float(rank_trace.get("rerank_cost_usd") or 0.0)
            wide_trace: dict[str, Any] = {}
            wide_kw = {**retrieve_kw, "intent": {**intent, "time": None, "recent": False}, "trace": wide_trace,
                       "remaining_budget": max(0.0, float(remaining) - first)}
            hits = retrieve(index, client, search_query, **wide_kw)
            intent = wide_kw["intent"]  # the answer is not told "recent" about memory found outside the window
            rank_trace.pop("abstain_reason", None)
            rank_trace.update(wide_trace)
            rank_trace["rerank_cost_usd"] = first + float(wide_trace.get("rerank_cost_usd") or 0.0)
            rank_trace["time_relaxed"] = True
        if "source_room_id" in retrieve_kw:
            # last check before the prompt (Go: chat.inScope): another room's memory never reaches it
            kept = [h for h in hits if h.get("source_room_id") == room]
            if len(kept) != len(hits):
                rank_trace["scope_dropped"] = len(hits) - len(kept)
                hits = kept
        rank_trace["search_query"] = search_query
        rank_trace["planner_retrieval_query"] = rewrite
        rank_trace["search_query_rewritten"] = bool(rewrite) and rewrite != message
        rank_trace["view_revision"] = view.get("revision")
        rank_trace["scope_fingerprint"] = view.get("scope_fingerprint")
        rank_trace["view_blocks"] = view_blocks
        if hits:
            rag_state = "candidates"
        elif rank_trace.get("abstained"):
            rag_state = "insufficient_authorized_candidates"
        else:
            rag_state = "empty_in_allowed_scope"
    rank_trace["rag_state"] = rag_state
    rank_trace["memory_requirement"] = memory_requirement
    rank_trace["time_reference"] = clock.to_trace()
    intent = resolve_intent(message, intent, index.cfg)
    timing = _compact_timing(
        {"plan_ms": plan_ms, "tools_ms": tools_ms},
        rank_trace,
    )
    return {
        "hits": hits,
        "system": compose_system_prompt(runtime, channel),
        "tool_results": tool_results,
        "plan": plan,
        "plan_raw": (plan_raw or "")[:4000],
        "memory_requirement": memory_requirement,
        "rag_state": rag_state,
        "search_query": search_query,
        "intent": intent,
        "channel_id": cid,
        "rank_trace": rank_trace,
        "runtime": runtime,
        "channel": channel,
        "yield_status": yield_status,
        "timing": timing,
        "rag_used": rag_wanted,
    }


def _session_keep(cfg: dict[str, Any]) -> int:
    return max(2, int(cfg.get("conversation_keep_turns") or cfg.get("max_history_turns") or DEFAULT_KEEP))


def _session_char_limit(cfg: dict[str, Any]) -> int:
    return max(800, int(cfg.get("conversation_summarize_chars") or DEFAULT_CHAR_LIMIT))


def _load_session(
    index: IndexBundle,
    client: PlayClient,
    *,
    conversation_id: str | None,
) -> tuple[str, str, list[dict[str, str]], str]:
    runtime = ensure_runtime(
        DATA, cfg=index.cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
    )
    channel_id = str(runtime.get("active_channel_id") or channel_id_from_room(_indexed_rooms(index)[0]))
    conv = get_conversation(DATA, channel_id, conversation_id)
    keep = _session_keep(index.cfg)
    limit = _session_char_limit(index.cfg)
    if needs_compact(conv, keep=keep, char_limit=limit):

        def _sum(body: str) -> str:
            return client.chat(
                [{"role": "user", "content": SUMMARIZE_PROMPT.format(body=body)}],
                max_tokens=280,
                temperature=0.1,
                reasoning="low",
                model=configured_models(index.cfg)["summary"],
            )

        conv = compact(conv, keep=keep, char_limit=limit, summarizer=_sum)
        put_conversation(DATA, channel_id, conv)
    summary, history = history_for_llm(conv, keep=keep)
    return channel_id, str(conv.get("id") or ""), history, summary


def _persist_session(
    index: IndexBundle,
    *,
    channel_id: str,
    conversation_id: str,
    user: str,
    assistant: str,
    speaker: str,
    client: PlayClient,
    debug_id: str | None = None,
) -> dict[str, Any]:
    keep = _session_keep(index.cfg)
    limit = _session_char_limit(index.cfg)

    def _sum(body: str) -> str:
        return client.chat(
            [{"role": "user", "content": SUMMARIZE_PROMPT.format(body=body)}],
            max_tokens=280,
            temperature=0.1,
            reasoning="low",
            model=configured_models(index.cfg)["summary"],
        )

    return append_turn(
        DATA,
        channel_id,
        conversation_id,
        user=user,
        assistant=assistant,
        speaker=speaker,
        keep=keep,
        char_limit=limit,
        summarizer=_sum,
        debug_id=debug_id,
    )


def chat(
    index: IndexBundle,
    *,
    message: str,
    history: list[dict[str, str]] | None = None,
    ranking: str | None = None,
    speaker: str | None = None,
    conversation_id: str | None = None,
    persist: bool = False,
) -> dict[str, Any]:
    cfg = index.cfg
    message = (message or "").strip()
    if not message:
        raise PlaygroundError("메시지를 입력해 달라")
    if len(message) > 2000:
        raise PlaygroundError("메시지가 너무 길다 (2000자 이하)")

    remaining = remaining_session_budget(cfg)
    if remaining <= 0.01:
        raise PlaygroundError("세션 예산이 소진됐다")
    try:
        rank_mode = normalize_ranking(
            ranking if ranking is not None else cfg.get("ranking"),
            default="heuristic",
        )
    except RankingError as exc:
        raise PlaygroundError(str(exc)) from exc

    client = make_client(cfg, budget_usd=min(remaining, float(cfg.get("turn_budget_usd", 0.45))))
    history = list(history or [])
    session_summary = ""
    session_id = str(conversation_id or "")
    session_channel = ""
    if persist:
        session_channel, session_id, history, session_summary = _load_session(
            index, client, conversation_id=conversation_id
        )
    bundle: dict[str, Any] | None = None
    hits: list[dict[str, Any]] = []
    rank_trace: dict[str, Any] = {}
    messages: list[dict[str, str]] = []
    generate_ms = 0.0
    answer_text = ""
    failed = False
    try:
        bundle = _prepare_bot_turn(
            index,
            client,
            message=message,
            ranking=rank_mode,
            remaining=remaining,
            history=history,
            session_summary=session_summary,
            conversation_id=session_id or conversation_id,
        )
        hits = bundle["hits"]
        rank_trace = bundle["rank_trace"]
        messages = _build_chat_messages(
            index,
            message=message,
            history=history,
            hits=hits,
            system_prompt=bundle["system"],
            tool_results=bundle["tool_results"],
            speaker=speaker,
            session_summary=session_summary,
            rag_state=str(bundle.get("rag_state") or ""),
            intent=bundle.get("intent"),
        )
        t_gen = time.perf_counter()
        answer = client.chat(messages, max_tokens=500, temperature=0.2, reasoning="low")
        generate_ms = _elapsed_ms(t_gen)
        answer_text = answer.strip() if answer else ""
    except (ClientError, PlaygroundError) as exc:
        failed = True
        append_debug_error(
            DATA,
            query=message,
            ranking=rank_mode,
            error=exc,
            extra={
                "path": "chat",
                **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=None, answer=None),
                "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
                "rank_trace": rank_trace,
                "plan": (bundle or {}).get("plan"),
                "plan_raw": (bundle or {}).get("plan_raw"),
                "tools": (bundle or {}).get("tool_results"),
                "channel_id": (bundle or {}).get("channel_id"),
                "speaker": speaker,
                "usage": client.usage.as_dict(),
                "config": _config_snap(cfg),
            },
        )
        posted = apply_client_usage(
            client, extra_usd=float(rank_trace.get("rerank_cost_usd") or 0.0)
        )
        if isinstance(exc, PlaygroundError):
            raise
        raise PlaygroundError(str(exc)) from exc
    posted = apply_client_usage(client, extra_usd=float(rank_trace.get("rerank_cost_usd") or 0.0))
    # 턴 장부 반영 뒤에 끝난 헤지 요청(query_embed)은 이 훅으로 따로 더한다(attempt_id로 중복 방지).
    client.late_settle = lambda: apply_client_usage(client)
    spent = float(posted["spent_usd"])
    new_total = float(posted["session_spent_usd"])
    timing = _compact_timing((bundle or {}).get("timing") or {}, rank_trace, {"generate_ms": generate_ms})
    debug_id = append_debug_turn(
        DATA,
        query=message,
        ranking=rank_mode,
        hits=hits,
        extra={
            "path": "chat",
            **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer_text),
            "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
            "rank_trace": rank_trace,
            "spent_usd": spent,
            "answer": answer_text,
            "answer_preview": answer_text[:400],
            "answer_chars": len(answer_text),
            "plan": bundle.get("plan"),
            "plan_raw": bundle.get("plan_raw"),
            "tools": bundle.get("tool_results"),
            "channel_id": bundle.get("channel_id"),
            "speaker": speaker,
            "timing": timing,
            "usage": client.usage.as_dict(),
            "models": {
                "response": client.llm_model,
                "planner": configured_models(cfg)["planner"],
                "summary": configured_models(cfg)["summary"],
                "embed": client.embed_model,
                "rerank": rank_trace.get("rerank_model")
                or rank_trace.get("rerank_model_requested")
                or configured_models(cfg)["rerank"],
            },
            "prompt": _prompt_stats(messages),
            "history": _history_stats(history, keep=int(cfg.get("max_history_turns") or 0)),
            "config": _config_snap(cfg),
            "ranking_applied": rank_trace.get("applied"),
            "rag_used": bundle.get("rag_used"),
            "rag_state": bundle.get("rag_state"),
            "memory_requirement": bundle.get("memory_requirement"),
            "search_query": bundle.get("search_query"),
        },
    )
    if persist and session_id:
        saved = _persist_session(
            index,
            channel_id=session_channel or str(bundle.get("channel_id") or ""),
            conversation_id=session_id,
            user=message,
            assistant=answer_text,
            speaker=str(speaker or ""),
            client=client,
            debug_id=debug_id,
        )
        session_id = str(saved.get("id") or session_id)
    return {
        "answer": answer_text,
        "hits": hits,
        "usage": client.usage.summary(),
        "spent_usd": spent,
        "session_spent_usd": new_total,
        "session_remaining_usd": max(0.0, float(cfg["session_budget_usd"]) - new_total),
        "recipe": index.meta.get("recipe"),
        "debug_id": debug_id,
        "ranking": rank_mode,
        "ranking_applied": rank_trace.get("applied"),
        "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
        "plan": bundle.get("plan"),
        "memory_requirement": bundle.get("memory_requirement"),
        "rag_state": bundle.get("rag_state"),
        "search_query": bundle.get("search_query"),
        "tools": bundle.get("tool_results"),
        "channel_id": bundle.get("channel_id"),
        "conversation_id": session_id,
        "session_summary": bool(session_summary),
        "timing": timing,
        "diagnosis": _log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer_text).get("diagnosis"),
        "server_run_id": SERVER_RUN_ID,
        "usage_detail": client.usage.as_dict(),
        "models": {
            "response": client.llm_model,
            "planner": configured_models(cfg)["planner"],
            "summary": configured_models(cfg)["summary"],
            "embed": client.embed_model,
            "rerank": rank_trace.get("rerank_model")
            or rank_trace.get("rerank_model_requested")
            or configured_models(cfg)["rerank"],
        },
    }


def chat_stream(
    index: IndexBundle,
    *,
    message: str,
    history: list[dict[str, str]] | None = None,
    ranking: str | None = None,
    speaker: str | None = None,
    conversation_id: str | None = None,
    persist: bool = False,
):
    """SSE용 이벤트 스트림. status → hits → delta* → done|error."""

    cfg = index.cfg
    message = (message or "").strip()
    if not message:
        raise PlaygroundError("메시지를 입력해 달라")
    if len(message) > 2000:
        raise PlaygroundError("메시지가 너무 길다 (2000자 이하)")

    remaining = remaining_session_budget(cfg)
    if remaining <= 0.01:
        raise PlaygroundError("세션 예산이 소진됐다")
    try:
        rank_mode = normalize_ranking(
            ranking if ranking is not None else cfg.get("ranking"),
            default="heuristic",
        )
    except RankingError as exc:
        raise PlaygroundError(str(exc)) from exc

    yield {"event": "status", "phase": "retrieve", "label": "봇이 끼어들 준비…"}

    client = make_client(cfg, budget_usd=min(remaining, float(cfg.get("turn_budget_usd", 0.45))))
    history = list(history or [])
    session_summary = ""
    session_id = str(conversation_id or "")
    session_channel = ""
    if persist:
        session_channel, session_id, history, session_summary = _load_session(
            index, client, conversation_id=conversation_id
        )
    bundle: dict[str, Any] | None = None
    hits: list[dict[str, Any]] = []
    rank_trace: dict[str, Any] = {}
    messages: list[dict[str, str]] = []
    parts: list[str] = []
    generate_ms = 0.0
    stream_failed: StreamError | ClientError | PlaygroundError | None = None
    try:
        yield {"event": "status", "phase": "plan", "label": "도구 고르는 중…"}
        bundle = _prepare_bot_turn(
            index,
            client,
            message=message,
            ranking=rank_mode,
            remaining=remaining,
            history=history,
            session_summary=session_summary,
            conversation_id=session_id or conversation_id,
        )
        hits = bundle["hits"]
        rank_trace = bundle["rank_trace"]
        yield {
            "event": "debug",
            "debug_id": None,
            "ranking": rank_mode,
            "ranking_applied": rank_trace.get("applied"),
            "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
            "rerank_error": rank_trace.get("rerank_error"),
            "plan": bundle.get("plan"),
            "memory_requirement": bundle.get("memory_requirement"),
            "rag_state": bundle.get("rag_state"),
            "search_query": bundle.get("search_query"),
            "tools": bundle.get("tool_results"),
            "channel_id": bundle.get("channel_id"),
            "timing": bundle.get("timing"),
            "rerank_model": rank_trace.get("rerank_model")
            or rank_trace.get("rerank_model_requested"),
            "pool_n": rank_trace.get("pool_n") or rank_trace.get("pool"),
            "diagnosis": _log_context(index, bundle, rank_trace, hits, conversation_id=session_id).get("diagnosis"),
        }
        yield {"event": "hits", "hits": hits}
        yield {"event": "tools", "tools": bundle.get("tool_results") or []}
        yield {"event": "status", "phase": "writing", "label": "마사몽이 입력 중…"}
        messages = _build_chat_messages(
            index,
            message=message,
            history=history,
            hits=hits,
            system_prompt=bundle["system"],
            tool_results=bundle["tool_results"],
            speaker=speaker,
            session_summary=session_summary,
            rag_state=str(bundle.get("rag_state") or ""),
            intent=bundle.get("intent"),
        )
        t_gen = time.perf_counter()
        for piece in client.chat_stream(
            messages, max_tokens=500, temperature=0.2, reasoning="low"
        ):
            if piece.get("type") == "delta":
                text = str(piece.get("text") or "")
                parts.append(text)
                yield {"event": "delta", "text": text}
            elif piece.get("type") == "done":
                break
            elif piece.get("type") == "error":
                raise StreamError("stream error payload", status="provider_error", partial_chars=sum(len(p) for p in parts))
        generate_ms = _elapsed_ms(t_gen)
    except StreamError as exc:
        stream_failed = exc
        append_debug_error(
            DATA,
            query=message,
            ranking=rank_mode,
            error=exc,
            extra={
                "path": "stream",
                **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=None, answer="".join(parts)),
                "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
                "rank_trace": rank_trace,
                "plan": (bundle or {}).get("plan"),
                "plan_raw": (bundle or {}).get("plan_raw"),
                "tools": (bundle or {}).get("tool_results"),
                "channel_id": (bundle or {}).get("channel_id"),
                "speaker": speaker,
                "usage": client.usage.as_dict(),
                "config": _config_snap(cfg),
                "partial_chars": exc.partial_chars,
            },
        )
    except (ClientError, PlaygroundError) as exc:
        stream_failed = exc
        append_debug_error(
            DATA,
            query=message,
            ranking=rank_mode,
            error=exc,
            extra={
                "path": "stream",
                **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=None, answer="".join(parts)),
                "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
                "rank_trace": rank_trace,
                "plan": (bundle or {}).get("plan"),
                "plan_raw": (bundle or {}).get("plan_raw"),
                "tools": (bundle or {}).get("tool_results"),
                "channel_id": (bundle or {}).get("channel_id"),
                "speaker": speaker,
                "usage": client.usage.as_dict(),
                "config": _config_snap(cfg),
                "partial_chars": sum(len(p) for p in parts),
            },
        )
        posted = apply_client_usage(client, extra_usd=float(rank_trace.get("rerank_cost_usd") or 0.0))
        if isinstance(exc, PlaygroundError):
            raise
        raise PlaygroundError(str(exc)) from exc

    posted = apply_client_usage(client, extra_usd=float(rank_trace.get("rerank_cost_usd") or 0.0))
    # 턴 장부 반영 뒤에 끝난 헤지 요청(query_embed)은 이 훅으로 따로 더한다(attempt_id로 중복 방지).
    client.late_settle = lambda: apply_client_usage(client)
    spent = float(posted["spent_usd"])
    new_total = float(posted["session_spent_usd"])
    if stream_failed is not None:
        status = getattr(stream_failed, "status", "error")
        yield {
            "event": "error",
            "status": status,
            "partial": bool(parts),
            "label": "생성이 중단됐다",
            "spent_usd": spent,
            "session_spent_usd": new_total,
        }
        return

    timing = _compact_timing((bundle or {}).get("timing") or {}, rank_trace, {"generate_ms": generate_ms})
    answer = "".join(parts).strip()
    debug_id = new_debug_id()
    if persist and session_id:
        saved = _persist_session(
            index,
            channel_id=session_channel or str((bundle or {}).get("channel_id") or ""),
            conversation_id=session_id,
            user=message,
            assistant=answer,
            speaker=str(speaker or ""),
            client=client,
            debug_id=debug_id,
        )
        session_id = str(saved.get("id") or session_id)
    append_debug_turn(
        DATA,
        debug_id=debug_id,
        query=message,
        ranking=rank_mode,
        hits=hits,
        extra={
            "path": "stream",
            **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer),
            "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
            "rank_trace": rank_trace,
            "spent_usd": spent,
            "answer": answer,
            "answer_preview": answer[:400],
            "answer_chars": len(answer),
            "plan": (bundle or {}).get("plan"),
            "plan_raw": (bundle or {}).get("plan_raw"),
            "tools": (bundle or {}).get("tool_results"),
            "channel_id": (bundle or {}).get("channel_id"),
            "speaker": speaker,
            "timing": timing,
            "usage": client.usage.as_dict(),
            "models": {
                "response": client.llm_model,
                "planner": configured_models(cfg)["planner"],
                "summary": configured_models(cfg)["summary"],
                "embed": client.embed_model,
                "rerank": rank_trace.get("rerank_model")
                or rank_trace.get("rerank_model_requested")
                or configured_models(cfg)["rerank"],
            },
            "prompt": _prompt_stats(messages),
            "history": _history_stats(history, keep=int(cfg.get("max_history_turns") or 0)),
            "config": _config_snap(cfg),
            "ranking_applied": rank_trace.get("applied"),
            "rag_used": (bundle or {}).get("rag_used"),
            "rag_state": (bundle or {}).get("rag_state"),
            "memory_requirement": (bundle or {}).get("memory_requirement"),
            "search_query": (bundle or {}).get("search_query"),
        },
    )
    yield {
        "event": "done",
        "answer": answer,
        "hits": hits,
        "usage": client.usage.summary(),
        "spent_usd": spent,
        "session_spent_usd": new_total,
        "session_remaining_usd": max(0.0, float(cfg["session_budget_usd"]) - new_total),
        "recipe": index.meta.get("recipe"),
        "debug_id": debug_id,
        "ranking": rank_mode,
        "ranking_applied": rank_trace.get("applied"),
        "intent": (bundle or {}).get("intent") or resolve_intent(message, None, cfg),
        "plan": bundle.get("plan"),
        "memory_requirement": bundle.get("memory_requirement"),
        "rag_state": bundle.get("rag_state"),
        "search_query": bundle.get("search_query"),
        "tools": bundle.get("tool_results"),
        "channel_id": (bundle or {}).get("channel_id"),
        "conversation_id": session_id,
        "timing": timing,
        "diagnosis": _log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer).get("diagnosis"),
        "server_run_id": SERVER_RUN_ID,
        "usage_detail": client.usage.as_dict(),
        "models": {
            "response": client.llm_model,
            "planner": configured_models(cfg)["planner"],
            "summary": configured_models(cfg)["summary"],
            "embed": client.embed_model,
            "rerank": rank_trace.get("rerank_model")
            or rank_trace.get("rerank_model_requested")
            or configured_models(cfg)["rerank"],
        },
    }


def status_payload(index: IndexBundle | None) -> dict[str, Any]:
    cfg = load_config()
    ledger = load_ledger()
    base = {
        "ready": index is not None,
        "experiment": cfg.get("experiment"),
        "config": {
            "collection": cfg["collection"],
            "rooms": cfg["rooms"],
            "room_merge": cfg.get("room_merge") or {},
            "leaf_size": cfg.get("leaf_size"),
            "leaf_limit": cfg.get("leaf_limit"),
            "message_limit": cfg.get("message_limit"),
            "parent_expand_before": cfg.get("parent_expand_before"),
            "parent_expand_after": cfg.get("parent_expand_after"),
            "summary_enabled": bool(cfg.get("summary_enabled", False)),
            "top_k": cfg["top_k"],
            "retrieve_pool": cfg.get("retrieve_pool"),
            "lexical_limit": cfg.get("lexical_limit"),
            "dense_limit": cfg.get("dense_limit"),
            "ranking": cfg.get("ranking", "heuristic"),
            "models": configured_models(cfg),
            "rrf_weights": cfg["rrf_weights"],
            "lexical": cfg["lexical"],
            "session_budget_usd": cfg["session_budget_usd"],
        },
        "session_spent_usd": ledger["spent_usd"],
        "session_remaining_usd": remaining_session_budget(cfg),
        "warning": "Discord 봇 흉내 샌드박스. 실 토큰/발송 없음. 인덱스가 있는 채널만 활성.",
        "debug_turns": str(DATA / "debug_turns.jsonl"),
        "debug_dumps": str(DATA / "debug_dumps"),
        "prepare": prepare_status_payload(),
        "run": {
            "server_run_id": SERVER_RUN_ID,
            "dataset": cfg.get("experiment"),
            "collection": cfg.get("collection"),
            "data_dir": DATA.name,
        },
    }
    embedder = getattr(index, "query_embedder", None)
    if embedder is not None:
        base["query_embed"] = dict(embedder.stats)
    if index is None:
        base["index"] = None
        base["bot"] = {
            "active_channel_id": "",
            "channels": public_channel_list({"channels": {}, "indexed_rooms": []}),
            "respond": "off",
            "respond_modes": ["command", "mention", "always", "off"],
            "tools_enabled": [],
            "persona": "",
            "rules": "",
            "agent_system_prompt": "",
            "view": {},
            "blocks": [],
        }
        return base
    runtime = ensure_runtime(
        DATA, cfg=cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
    )
    active = get_channel(runtime, str(runtime.get("active_channel_id") or ""))
    base["bot"] = {
        "active_channel_id": runtime.get("active_channel_id"),
        "mention_required": bool(runtime.get("mention_required")),  # legacy; per-channel `respond` is authoritative
        "respond": active.get("respond") or "off",
        "respond_modes": ["command", "mention", "always", "off"],
        "tools_enabled": runtime.get("tools_enabled"),
        "channels": public_channel_list(runtime),
        "persona": active.get("persona") or "",
        "rules": active.get("rules") or "",
        "agent_system_prompt": runtime.get("agent_system_prompt") or "",
        "view": active.get("view") if isinstance(active.get("view"), dict) else {},
        "blocks": [
            {
                "block_id": b.get("block_id"),
                "platform": b.get("platform"),
                "label": b.get("label"),
                "source_room": b.get("source_room"),
                "leaf_count": b.get("leaf_count"),
                "status": b.get("status"),
            }
            for b in (index.blocks or [])
        ],
    }
    base["index"] = {
        "message_count": len(index.messages),
        "leaf_count": len(index.leaves),
        "baseline_count": len(index.baseline),
        "summary_count": len(index.summaries),
        "embedding_dim": int(index.leaf_vectors.shape[1]),
        "recipe": index.meta.get("recipe"),
        "prepare_cost_usd": index.meta.get("prepare_cost_usd"),
    }
    return base
