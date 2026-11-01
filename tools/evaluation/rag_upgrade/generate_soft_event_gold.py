#!/usr/bin/env python3
"""Generate a small, review-required multi-evidence gold set over soft events.

This tool deliberately treats a soft event as a *candidate grouping* over the
stable leaf index.  The generated question is grounded in observation IDs from
different leaves, while the model only sees opaque labels (``m000`` etc.) and
the minimum selected message text needed to write the question.

The output is synthetic and unreviewed.  It is intended for an offline pilot,
not for a quality qualification gate.  Network calls are made only when the
CLI is explicitly run with a configured OpenRouter client; importing this
module and its tests never calls a provider.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import re
import sys
import tempfile
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence


REPO = Path(__file__).resolve().parents[3]
CODE = REPO / "experimental" / "rag-leaf-parent"
EVAL = REPO / "tools" / "evaluation"
if str(CODE) not in sys.path:
    sys.path.insert(0, str(CODE))
if str(EVAL) not in sys.path:
    sys.path.insert(0, str(EVAL))

from client import BudgetError, ClientError, PlayClient, load_secrets  # noqa: E402
from engine import DATA, LEAF_VECS_PATH, load_config, load_index  # noqa: E402
from rag_upgrade.index_binding import compute_index_binding  # noqa: E402
from soft_events import SoftEvent, SoftEventRecipe, build_soft_events  # noqa: E402


SCHEMA_VERSION = 2
GOLD_KIND = "synthetic_soft_event_unreviewed"
REVIEW_STATUS = "unreviewed"
DEFAULT_QUESTIONS = 48
DEFAULT_BUDGET_USD = 0.22
DEFAULT_MAX_REQUEST_COST_USD = 0.02
DEFAULT_MAX_PER_ROOM = 20
DEFAULT_LOCK_FRACTION = 0.25
DEFAULT_MIN_QUESTION_CHARS = 8
DEFAULT_MAX_BIGRAM_OVERLAP = 0.72
DEFAULT_MAX_PROMPT_MESSAGES = 16
DEFAULT_MESSAGE_CHAR_LIMIT = 280
QUESTION_TYPES = frozenset({"event", "causal", "decision", "temporal_update"})
UNANSWERABLE_ANSWER_RE = re.compile(
    r"(?:알\s*수\s*없|확인할\s*수\s*없|답할\s*수\s*없|정보가\s*없|모르겠|모른다|모름)"
)


class SoftEventGoldError(ValueError):
    """The candidate or generated object cannot enter the gold set."""


class OutputPathError(ValueError):
    """The output path is outside the private scratch area."""


@dataclass(frozen=True)
class PromptMessage:
    """One opaque prompt label and its stable source location."""

    label: str
    leaf_label: str
    observation_id: str
    leaf_doc_id: str
    text: str
    message_index: int


@dataclass(frozen=True)
class EventCandidate:
    """A selected soft event plus a bounded, opaque prompt context."""

    event: SoftEvent
    split: str
    candidate_hash: str
    prompt_messages: tuple[PromptMessage, ...]
    evidence_leaf_ids: tuple[str, ...]


def _sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def code_hash(path: Path | None = None) -> str:
    """Hash the generator source for the output manifest."""

    source = Path(path or __file__)
    return hashlib.sha256(source.read_bytes()).hexdigest()


def _stable_number(seed: int, value: str) -> int:
    return int(_sha256_text(f"{int(seed)}\0{value}")[:16], 16)


def assign_split(event_id: str, *, seed: int, lock_fraction: float = DEFAULT_LOCK_FRACTION) -> str:
    """Assign dev/lock before candidate selection using only stable event data."""

    fraction = float(lock_fraction)
    if not math.isfinite(fraction) or not 0.0 <= fraction < 1.0:
        raise ValueError("lock_fraction must be finite and in [0, 1)")
    # Domain-separate split assignment from candidate ordering. Reusing the
    # same hash for both makes the earliest selected candidates overwhelmingly
    # fall into the low-hash (lock) bucket.
    bucket = _stable_number(seed, f"split\0{event_id}") / float(16**16)
    return "lock" if bucket < fraction else "dev"


def _selection_hash(event_id: str, *, seed: int) -> str:
    return _sha256_text(f"{int(seed)}\0select\0{event_id}")


def _parse_time(value: Any) -> Any:
    """Return an explicit ISO timestamp or numeric timestamp, otherwise None."""

    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        number = float(value)
        return number if math.isfinite(number) else None
    text = str(value).strip()
    if not text:
        return None
    try:
        normalized = text[:-1] + "+00:00" if text.endswith("Z") else text
        parsed = datetime.fromisoformat(normalized)
        return parsed.timestamp()
    except (TypeError, ValueError, OverflowError):
        return None


def _message_time(row: Mapping[str, Any]) -> Any:
    """Read an explicitly supplied message time; never infer from ordinal."""

    for key in ("parsed_time", "timestamp", "time", "created_at", "datetime"):
        if key in row:
            value = _parse_time(row.get(key))
            if value is not None:
                return value
    return None


def build_leaf_times(index: Any) -> dict[str, dict[str, Any]]:
    """Build soft-event times from each leaf's start/end message timestamps.

    Both endpoints are supplied explicitly.  Missing timestamps remain unknown
    rather than being manufactured from message position or neighbouring rows.
    """

    messages = list(getattr(index, "messages", ()) or ())
    rows = list(getattr(index, "leaves", ()) or ())
    result: dict[str, dict[str, Any]] = {}
    for leaf in rows:
        doc_id = str(leaf.get("doc_id") or leaf.get("leaf_id") or "").strip()
        if not doc_id:
            continue
        start = int(leaf.get("start") or 0)
        end = int(leaf.get("end") or start)
        start_row = messages[start] if 0 <= start < len(messages) else None
        end_row = messages[end - 1] if 0 <= end - 1 < len(messages) and end > start else None
        result[doc_id] = {
            "start_time": _message_time(start_row) if isinstance(start_row, Mapping) else None,
            "end_time": _message_time(end_row) if isinstance(end_row, Mapping) else None,
        }
    return result


def _message_text(row: Mapping[str, Any]) -> str:
    for key in ("content", "body", "text", "message"):
        value = row.get(key)
        if value is not None:
            return str(value).strip()
    return ""


def _message_observation_id(row: Mapping[str, Any]) -> str:
    for key in ("observation_id", "id", "message_id"):
        value = row.get(key)
        if value is not None and str(value).strip():
            return str(value).strip()
    return ""


def _leaf_room(leaf: Mapping[str, Any]) -> str:
    value = leaf.get("source_room_id", leaf.get("room_id"))
    return str(value or "").strip()


def _leaf_doc_id(leaf: Mapping[str, Any]) -> str:
    return str(leaf.get("doc_id", leaf.get("leaf_id", "")) or "").strip()


def _leaf_message_rows(index: Any, leaf: Mapping[str, Any]) -> list[tuple[int, Mapping[str, Any]]]:
    messages = list(getattr(index, "messages", ()) or ())
    start = int(leaf.get("start") or 0)
    end = int(leaf.get("end") or start)
    out: list[tuple[int, Mapping[str, Any]]] = []
    for position in range(max(0, start), min(end, len(messages))):
        row = messages[position]
        if not isinstance(row, Mapping):
            continue
        if _message_observation_id(row) and _message_text(row):
            out.append((position, row))
    return out


def _candidate_leaf_rows(index: Any, event: SoftEvent) -> list[Mapping[str, Any]]:
    by_id = getattr(index, "leaf_by_id", None)
    if isinstance(by_id, Mapping):
        return [by_id[leaf_id] for leaf_id in event.leaf_ids if leaf_id in by_id]
    rows = {_leaf_doc_id(row): row for row in (getattr(index, "leaves", ()) or ())}
    return [rows[leaf_id] for leaf_id in event.leaf_ids if leaf_id in rows]


def make_prompt_context(
    index: Any,
    event: SoftEvent,
    *,
    max_messages: int = DEFAULT_MAX_PROMPT_MESSAGES,
    message_char_limit: int = DEFAULT_MESSAGE_CHAR_LIMIT,
) -> tuple[PromptMessage, ...]:
    """Select bounded source text and replace every source ID with mNNN labels.

    Selection is deterministic: take non-empty messages in stable leaf order,
    while reserving at least one message from the first two leaves.  Actual
    observation/leaf IDs never enter the prompt.
    """

    if int(max_messages) < 2:
        raise ValueError("max_messages must be at least 2")
    if int(message_char_limit) < 1:
        raise ValueError("message_char_limit must be positive")
    leaves = _candidate_leaf_rows(index, event)
    rows_by_leaf: list[tuple[Mapping[str, Any], list[tuple[int, Mapping[str, Any]]]]] = []
    for leaf in leaves:
        rows = _leaf_message_rows(index, leaf)
        if rows:
            rows_by_leaf.append((leaf, rows))
    if len(rows_by_leaf) < 2:
        return ()

    selected: list[tuple[Mapping[str, Any], int, Mapping[str, Any]]] = []
    # Reserve one observation from each of two distinct leaves.
    for leaf, rows in rows_by_leaf[:2]:
        position, row = rows[0]
        selected.append((leaf, position, row))
    selected_keys = {(id(leaf), position) for leaf, position, _ in selected}
    # Fill with the remaining local context in stable order.
    for leaf, rows in rows_by_leaf:
        for position, row in rows:
            if len(selected) >= int(max_messages):
                break
            key = (id(leaf), position)
            if key in selected_keys:
                continue
            selected.append((leaf, position, row))
            selected_keys.add(key)
        if len(selected) >= int(max_messages):
            break

    out: list[PromptMessage] = []
    leaf_labels: dict[str, str] = {}
    for number, (leaf, position, row) in enumerate(selected):
        text = _message_text(row)[: int(message_char_limit)].strip()
        if not text:
            continue
        leaf_doc_id = _leaf_doc_id(leaf)
        if leaf_doc_id not in leaf_labels:
            leaf_labels[leaf_doc_id] = f"g{len(leaf_labels):02d}"
        out.append(
            PromptMessage(
                label=f"m{number:03d}",
                leaf_label=leaf_labels[leaf_doc_id],
                observation_id=_message_observation_id(row),
                leaf_doc_id=leaf_doc_id,
                text=text,
                message_index=position,
            )
        )
    return tuple(out)


def select_candidates(
    events: Iterable[SoftEvent],
    index: Any,
    *,
    questions: int = DEFAULT_QUESTIONS,
    seed: int = 20260922,
    max_per_room: int = DEFAULT_MAX_PER_ROOM,
    lock_fraction: float = DEFAULT_LOCK_FRACTION,
    max_prompt_messages: int = DEFAULT_MAX_PROMPT_MESSAGES,
    message_char_limit: int = DEFAULT_MESSAGE_CHAR_LIMIT,
) -> tuple[EventCandidate, ...]:
    """Select room-balanced, deterministic, answerable multi-leaf candidates.

    Split assignment happens while all events are still in the candidate pool,
    before room round-robin selection.  This prevents selection order from
    changing dev/lock membership.
    """

    n_questions = int(questions)
    per_room = int(max_per_room)
    if n_questions < 0:
        raise ValueError("questions must be non-negative")
    if per_room < 1:
        raise ValueError("max_per_room must be positive")
    prepared: list[EventCandidate] = []
    for event in events:
        room = str(event.source_room_id or "").strip()
        if not room or len(event.leaf_ids) < 2:
            continue
        prompt_messages = make_prompt_context(
            index,
            event,
            max_messages=max_prompt_messages,
            message_char_limit=message_char_limit,
        )
        leaf_ids = tuple(dict.fromkeys(item.leaf_doc_id for item in prompt_messages))
        observation_ids = tuple(dict.fromkeys(item.observation_id for item in prompt_messages))
        if len(leaf_ids) < 2 or len(observation_ids) < 2:
            continue
        candidate_hash = _selection_hash(event.event_id, seed=int(seed))
        prepared.append(
            EventCandidate(
                event=event,
                split=assign_split(event.event_id, seed=int(seed), lock_fraction=lock_fraction),
                candidate_hash=candidate_hash,
                prompt_messages=prompt_messages,
                evidence_leaf_ids=leaf_ids,
            )
        )

    by_room: dict[str, list[EventCandidate]] = defaultdict(list)
    for candidate in prepared:
        by_room[str(candidate.event.source_room_id)].append(candidate)
    for room in by_room:
        by_room[room].sort(key=lambda item: (item.candidate_hash, item.event.event_id))

    # Round-robin rooms to prevent a prolific room from consuming the set.
    selected: list[EventCandidate] = []
    room_counts: dict[str, int] = defaultdict(int)
    rooms = sorted(by_room)
    while len(selected) < n_questions:
        progressed = False
        for room in rooms:
            if len(selected) >= n_questions or room_counts[room] >= per_room:
                continue
            pool = by_room[room]
            if not pool:
                continue
            candidate = pool.pop(0)
            selected.append(candidate)
            room_counts[room] += 1
            progressed = True
        if not progressed:
            break
    return tuple(selected)


def _normalise_for_bigrams(value: str) -> list[str]:
    return re.findall(r"[0-9A-Za-z가-힣]+", str(value or "").lower())


def _bigrams(value: str) -> set[str]:
    result: set[str] = set()
    for token in _normalise_for_bigrams(value):
        if len(token) == 1:
            result.add(token)
        else:
            result.update(token[i : i + 2] for i in range(len(token) - 1))
    return result


def query_evidence_bigram_overlap(question: str, evidence_text: str) -> float:
    """Return the fraction of query character-bigrams copied from evidence."""

    query = _bigrams(question)
    evidence = _bigrams(evidence_text)
    if not query:
        return 1.0
    return len(query & evidence) / float(len(query))


def _parse_json_object(raw: Any) -> Mapping[str, Any]:
    if not isinstance(raw, str):
        raise SoftEventGoldError("invalid_json")
    text = raw.strip()
    if "```" in text:
        parts = text.split("```")
        text = next((part for part in parts if "{" in part and "}" in part), text)
        text = re.sub(r"^\s*(?:json)?\s*", "", text, flags=re.IGNORECASE)
    start, end = text.find("{"), text.rfind("}")
    if start < 0 or end <= start:
        raise SoftEventGoldError("invalid_json")
    try:
        parsed = json.loads(text[start : end + 1])
    except (TypeError, json.JSONDecodeError):
        raise SoftEventGoldError("invalid_json") from None
    if not isinstance(parsed, Mapping):
        raise SoftEventGoldError("invalid_json")
    return parsed


def make_generation_prompt(candidate: EventCandidate) -> str:
    """Build a prompt without exposing source IDs or private metadata."""

    event = candidate.event
    lines = [
        "다음은 하나의 한국어 단체 채팅 사건을 구성하는 일부 원문이다.",
        "표시된 mNNN 라벨만 답의 근거로 사용할 수 있다.",
        "서로 다른 두 익명 그룹(gNN)의 근거가 모두 필요한 질문을 한 개 생성하라.",
        "원문에 없는 사실은 추측하지 말고, 질문과 답은 한국어로 작성하라.",
        "JSON 객체 하나만 출력하라:",
        '{"question":"...", "expected_answer":"...", "evidence_labels":["m000","m001"], "question_type":"event|causal|decision|temporal_update", "answerable":true}',
        "evidence_labels에는 반드시 서로 다른 gNN 그룹에서 하나 이상씩 고른 mNNN 라벨을 넣어라.",
        "질문과 답에는 내부 표식인 mNNN 또는 gNN 라벨을 절대 쓰지 마라.",
        "질문은 원문을 그대로 복사하지 말고 사건의 관계·원인·결정·시간 변화를 묻는 자연스러운 한 문장으로 작성하라.",
        f"사건 시간 범위: {event.start_time!r} ~ {event.end_time!r}",
        "<chat>",
    ]
    for item in candidate.prompt_messages:
        # Only opaque labels and bounded text are included.  Do not add IDs,
        # leaf positions, source room names, or object reprs here.
        lines.append(f"[{item.leaf_label}] {item.label}: {item.text}")
    lines.extend(["</chat>", ""])
    return "\n".join(lines)


def _labels_to_items(candidate: EventCandidate) -> dict[str, PromptMessage]:
    return {item.label: item for item in candidate.prompt_messages}


def validate_generated_question(
    raw: Mapping[str, Any],
    candidate: EventCandidate,
    *,
    min_question_chars: int = DEFAULT_MIN_QUESTION_CHARS,
    max_bigram_overlap: float = DEFAULT_MAX_BIGRAM_OVERLAP,
) -> dict[str, Any]:
    """Validate and normalize one model response; errors contain no source text."""

    if not isinstance(raw, Mapping):
        raise SoftEventGoldError("invalid_object")
    question = str(raw.get("question") or raw.get("query") or "").strip()
    answer = str(raw.get("expected_answer") or "").strip()
    kind = str(raw.get("question_type") or "").strip()
    answerable = raw.get("answerable")
    if not isinstance(answerable, bool) or not answerable:
        raise SoftEventGoldError("unanswerable")
    if len(question) < int(min_question_chars):
        raise SoftEventGoldError("question_too_short")
    if not answer:
        raise SoftEventGoldError("missing_answer")
    if UNANSWERABLE_ANSWER_RE.search(answer):
        raise SoftEventGoldError("unanswerable_answer")
    if kind not in QUESTION_TYPES:
        raise SoftEventGoldError("bad_question_type")
    labels_raw = raw.get("evidence_labels")
    if isinstance(labels_raw, (str, bytes)) or not isinstance(labels_raw, Sequence):
        raise SoftEventGoldError("bad_labels")
    labels = [str(label).strip() for label in labels_raw]
    if len(labels) < 2 or len(labels) != len(set(labels)):
        raise SoftEventGoldError("need_two_labels")
    label_map = _labels_to_items(candidate)
    if any(label not in label_map for label in labels):
        raise SoftEventGoldError("label_outside_prompt")
    selected = [label_map[label] for label in labels]
    selected_leaf_ids = {item.leaf_doc_id for item in selected}
    selected_obs_ids = {item.observation_id for item in selected}
    if len(selected_leaf_ids) < 2:
        raise SoftEventGoldError("same_leaf_only")
    if len(selected_obs_ids) < 2 or any(not item.observation_id for item in selected):
        raise SoftEventGoldError("need_two_observations")
    if re.search(r"\b(?:m\d{3}|g\d{2})\b", question + " " + answer):
        raise SoftEventGoldError("opaque_label_leaked")
    evidence_text = "\n".join(item.text for item in selected)
    overlap = query_evidence_bigram_overlap(question, evidence_text)
    if overlap > float(max_bigram_overlap):
        raise SoftEventGoldError("query_evidence_overlap")
    return {
        "question": question,
        "query": question,
        "expected_answer": answer,
        "evidence_labels": labels,
        "question_type": kind,
        "answerable": True,
        "overlap": round(overlap, 6),
        "_selected": tuple(selected),
    }


def _safe_usage(client: Any) -> dict[str, Any]:
    usage = getattr(client, "usage", None)
    if usage is None:
        return {"cost_usd": 0.0, "calls": 0, "attempts": 0, "errors": {}}
    if hasattr(usage, "as_dict"):
        data = usage.as_dict()
    elif isinstance(usage, Mapping):
        data = dict(usage)
    else:
        data = {
            "cost_usd": float(getattr(usage, "cost_usd", 0.0) or 0.0),
            "calls": int(getattr(usage, "calls", 0) or 0),
            "attempts": int(getattr(usage, "attempts", 0) or 0),
            "errors": dict(getattr(usage, "errors", {}) or {}),
        }
    # Usage may contain low-level event details, but never copy prompt text.
    allowed = {
        "cost_usd",
        "calls",
        "attempts",
        "errors",
        "verified_cost_usd",
        "estimated_cost_usd",
        "unknown_cost_usd",
        "unknown_cost_attempts",
        "reserved_cost_usd",
        "failed_attempts",
        "cost_settled",
        "cursor",
    }
    return {key: data[key] for key in allowed if key in data}


def _cost_value(client: Any) -> float:
    usage = getattr(client, "usage", None)
    try:
        return float(getattr(usage, "cost_usd", 0.0) if usage is not None else 0.0)
    except (TypeError, ValueError):
        return 0.0


def _merge_usage(previous: Mapping[str, Any], current: Mapping[str, Any]) -> dict[str, Any]:
    """Merge resume ledgers without copying per-request/provider details."""

    merged: dict[str, Any] = {}
    for key in (
        "cost_usd",
        "verified_cost_usd",
        "estimated_cost_usd",
        "unknown_cost_usd",
        "reserved_cost_usd",
    ):
        merged[key] = round(float(previous.get(key, 0.0) or 0.0) + float(current.get(key, 0.0) or 0.0), 6)
    for key in ("calls", "attempts", "unknown_cost_attempts", "failed_attempts"):
        merged[key] = int(previous.get(key, 0) or 0) + int(current.get(key, 0) or 0)
    errors: dict[str, int] = {}
    for source in (previous.get("errors"), current.get("errors")):
        if isinstance(source, Mapping):
            for key, value in source.items():
                errors[str(key)] = errors.get(str(key), 0) + int(value or 0)
    merged["errors"] = dict(sorted(errors.items()))
    merged["cost_settled"] = merged["reserved_cost_usd"] <= 0.0
    return merged


def _model_manifest(cfg: Mapping[str, Any], client: Any) -> dict[str, Any]:
    models = cfg.get("models") if isinstance(cfg.get("models"), Mapping) else {}
    return {
        "model": str(getattr(client, "llm_model", models.get("response", "")) or ""),
        "embed_model": str(getattr(client, "embed_model", models.get("embed", "")) or ""),
        "response_model": str(models.get("response") or ""),
    }


def _build_client(cfg: Mapping[str, Any], *, budget_usd: float, max_request_cost_usd: float) -> PlayClient:
    """Construct the real client with both independent budget caps explicit."""

    secret_path = Path(os.environ.get("EVAL_SECRETS", REPO / ".private" / "eval" / "secrets.env"))
    models = cfg.get("models") if isinstance(cfg.get("models"), Mapping) else {}
    return PlayClient(
        budget_usd=float(budget_usd),
        max_request_cost_usd=float(max_request_cost_usd),
        secrets=load_secrets(secret_path),
        max_workers=int(cfg.get("api_workers", 1) or 1),
        llm_model=str(models.get("response") or "") or None,
        embed_model=str(models.get("embed") or "") or None,
    )


def _output_root() -> Path:
    return (REPO / ".masamong-scratch").resolve()


def validate_output_path(path: Path) -> Path:
    resolved = Path(path).expanduser().resolve()
    root = _output_root()
    try:
        resolved.relative_to(root)
    except ValueError:
        raise OutputPathError("outside_private_scratch") from None
    if resolved == root:
        raise OutputPathError("output_must_be_file")
    return resolved


def _manifest_core(manifest: Mapping[str, Any]) -> dict[str, Any]:
    # These fields describe this particular run's progress and necessarily
    # change after a partial resume.  The immutable recipe/config contract is
    # everything else in the manifest.
    transient = {"usage", "event_count", "candidate_count", "generated_count"}
    return {str(key): value for key, value in manifest.items() if key not in transient}


def _resume_manifest_matches(existing: Mapping[str, Any], current: Mapping[str, Any]) -> bool:
    stored = existing.get("generator")
    return isinstance(stored, Mapping) and _manifest_core(stored) == _manifest_core(current)


def _read_existing(path: Path) -> Mapping[str, Any]:
    try:
        parsed = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise SoftEventGoldError("resume_file_invalid") from None
    if not isinstance(parsed, Mapping):
        raise SoftEventGoldError("resume_file_invalid")
    return parsed


def _atomic_write_json(path: Path, payload: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def _event_recipe(args: argparse.Namespace) -> SoftEventRecipe:
    return SoftEventRecipe(
        version="soft-event-gold-v1",
        gap_seconds=float(args.gap_minutes) * 60.0,
        max_leaves=int(args.max_event_leaves),
        max_chars=int(args.max_event_chars),
        min_leaves=1,
    )


def _source_rows_for_event(index: Any, event: SoftEvent) -> dict[str, Mapping[str, Any]]:
    by_id = getattr(index, "leaf_by_id", None)
    if isinstance(by_id, Mapping):
        return {leaf_id: by_id[leaf_id] for leaf_id in event.leaf_ids if leaf_id in by_id}
    return {
        _leaf_doc_id(row): row
        for row in (getattr(index, "leaves", ()) or ())
        if _leaf_doc_id(row) in event.leaf_ids
    }


def _normalised_binding(index: Any, cfg: Mapping[str, Any]) -> dict[str, Any]:
    meta_path = Path(DATA) / "meta.json"
    try:
        meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.exists() else {}
    except (OSError, UnicodeError, json.JSONDecodeError):
        meta = {}
    return compute_index_binding(
        leaves=list(getattr(index, "leaves", ()) or ()),
        messages=list(getattr(index, "messages", ()) or ()),
        leaf_vecs_path=Path(LEAF_VECS_PATH),
        cfg=cfg,
        meta=meta,
    )


def _question_id(candidate: EventCandidate, validated: Mapping[str, Any]) -> str:
    selected = ",".join(str(label) for label in validated["evidence_labels"])
    material = "\0".join(
        [candidate.event.event_id, candidate.split, selected, str(validated["question"])]
    )
    return f"soft-event-q:{_sha256_text(material)[:24]}"


def _normalise_question(
    validated: Mapping[str, Any], candidate: EventCandidate, *, question_id: str
) -> dict[str, Any]:
    selected: Sequence[PromptMessage] = validated["_selected"]
    observation_ids = list(dict.fromkeys(item.observation_id for item in selected))
    groups = [
        {"group_id": f"evidence-{index:02d}", "alternatives": [item.observation_id]}
        for index, item in enumerate(selected)
    ]
    return {
        "question_id": question_id,
        "question": validated["question"],
        "query": validated["query"],
        "expected_answer": validated["expected_answer"],
        "evidence_labels": list(validated["evidence_labels"]),
        "answerable": True,
        "question_type": validated["question_type"],
        "split": candidate.split,
        "review_status": REVIEW_STATUS,
        "expected_memory_requirement": "required",
        "expected_abstention": False,
        "expected_evidence_ids": observation_ids,
        "want_observation_ids": observation_ids,
        "required_evidence_groups": groups,
        "source_event_id": candidate.event.event_id,
        "source_leaf_doc_id": candidate.event.leaf_ids[0],
        "source_leaf_doc_ids": list(candidate.evidence_leaf_ids),
    }


def generate_questions(
    candidates: Sequence[EventCandidate],
    client: Any,
    *,
    target: int,
    min_question_chars: int = DEFAULT_MIN_QUESTION_CHARS,
    max_bigram_overlap: float = DEFAULT_MAX_BIGRAM_OVERLAP,
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    """Generate in bounded parallel batches with one authoritative budget ledger.

    ``PlayClient`` reserves spend under a lock before every request.  A batch
    therefore cannot exceed the shared cap, while ``executor.map`` preserves
    candidate order so accepted output remains reproducible.
    """

    questions: list[dict[str, Any]] = []
    reasons: dict[str, int] = defaultdict(int)
    seen_questions: set[str] = set()

    def generate_one(candidate: EventCandidate) -> tuple[EventCandidate, dict[str, Any] | None, str]:
        try:
            raw_response = client.chat(
                [{"role": "user", "content": make_generation_prompt(candidate)}],
                max_tokens=400,
                temperature=0.15,
                reasoning=None,
            )
            parsed = _parse_json_object(raw_response)
            validated = validate_generated_question(
                parsed,
                candidate,
                min_question_chars=min_question_chars,
                max_bigram_overlap=max_bigram_overlap,
            )
            return candidate, validated, ""
        except BudgetError:
            return candidate, None, "budget_exhausted"
        except (ClientError, TimeoutError, OSError):
            # Do not expose provider text or request content in diagnostics.
            if _cost_value(client) >= float(getattr(client, "budget_usd", float("inf"))):
                return candidate, None, "budget_exhausted"
            return candidate, None, "provider_error"
        except SoftEventGoldError as exc:
            return candidate, None, str(exc)
        except (TypeError, ValueError, KeyError):
            return candidate, None, "invalid_response"

    workers = min(8, max(1, int(getattr(client, "max_workers", 1) or 1)))
    cursor = 0
    while cursor < len(candidates) and len(questions) < int(target):
        remaining = int(target) - len(questions)
        batch = list(candidates[cursor : cursor + min(workers, max(1, remaining))])
        cursor += len(batch)
        with ThreadPoolExecutor(max_workers=min(workers, len(batch))) as pool:
            results = list(pool.map(generate_one, batch))
        exhausted = False
        for candidate, validated, reason in results:
            if reason:
                reasons[reason] += 1
                exhausted = exhausted or reason == "budget_exhausted"
                continue
            assert validated is not None
            fingerprint = str(validated["question"]).casefold()
            if fingerprint in seen_questions:
                reasons["duplicate_question"] += 1
                continue
            seen_questions.add(fingerprint)
            qid = _question_id(candidate, validated)
            questions.append(_normalise_question(validated, candidate, question_id=qid))
            if len(questions) >= int(target):
                break
        if exhausted:
            break
    return questions, dict(reasons)


def _counts(questions: Sequence[Mapping[str, Any]]) -> dict[str, int]:
    out: dict[str, int] = defaultdict(int)
    for question in questions:
        out[str(question.get("question_type") or "unknown")] += 1
        out[f"split:{str(question.get('split') or 'unknown')}"] += 1
    return dict(sorted(out.items()))


def build_payload(
    *,
    questions: Sequence[Mapping[str, Any]],
    binding: Mapping[str, Any],
    manifest: Mapping[str, Any],
) -> dict[str, Any]:
    index_fp = str(binding.get("index_fingerprint") or "")
    return {
        "schema_version": SCHEMA_VERSION,
        "dataset_id": f"soft-event-{index_fp[:16] or 'unbound'}",
        "gold_kind": GOLD_KIND,
        "review_status": REVIEW_STATUS,
        "not_human_reviewed": True,
        "index_binding": dict(binding),
        "corpus_binding": dict(binding.get("corpus_binding") or {}),
        "generator": dict(manifest),
        "questions": [dict(question) for question in questions],
    }


def _summary_output(
    *,
    questions: Sequence[Mapping[str, Any]],
    reasons: Mapping[str, int],
    client: Any,
    out: Path,
) -> dict[str, Any]:
    # Deliberately no path, question, ID, prompt, or exception detail in this
    # object.  It is safe for CI logs and shell history capture.
    return {
        "stage": "soft_event_gold_done",
        "counts": _counts(questions),
        "accepted": len(questions),
        "reasons": dict(sorted((str(k), int(v)) for k, v in reasons.items())),
        "cost_usd": round(_cost_value(client), 6),
    }


def run(args: argparse.Namespace) -> tuple[dict[str, Any], dict[str, Any]]:
    """Run generation and return (private payload, log-safe summary)."""

    out = validate_output_path(Path(args.out))
    if int(args.questions) < 0:
        raise SoftEventGoldError("questions_must_be_non_negative")
    if float(args.budget_usd) <= 0 or float(args.max_request_cost_usd) <= 0:
        raise SoftEventGoldError("budget_must_be_positive")
    cfg = load_config()
    index = load_index()
    binding = _normalised_binding(index, cfg)
    recipe = _event_recipe(args)
    configured_response_model = str((cfg.get("models") or {}).get("response") or "")
    generation_model = str(args.model or configured_response_model).strip()
    manifest_base: dict[str, Any] = {
        "model": generation_model,
        "seed": int(args.seed),
        "recipe": recipe.as_dict(),
        "code_hash": code_hash(),
        "budget_usd": float(args.budget_usd),
        "max_request_cost_usd": float(args.max_request_cost_usd),
        "lock_fraction": float(args.lock_fraction),
        "max_per_room": int(args.max_per_room),
        "max_prompt_messages": int(args.max_prompt_messages),
        "message_char_limit": int(args.message_char_limit),
        "question_target": int(args.questions),
    }
    existing: Mapping[str, Any] | None = None
    if out.exists():
        if not bool(args.resume):
            raise SoftEventGoldError("output_exists_use_resume")
        existing = _read_existing(out)
        if existing.get("index_binding", {}).get("index_fingerprint") != binding.get("index_fingerprint"):
            raise SoftEventGoldError("resume_binding_mismatch")
        if not _resume_manifest_matches(existing, manifest_base):
            raise SoftEventGoldError("resume_manifest_mismatch")

    previous_usage: Mapping[str, Any] = {}
    if existing and isinstance(existing.get("generator"), Mapping):
        stored_usage = existing["generator"].get("usage")
        if isinstance(stored_usage, Mapping):
            previous_usage = stored_usage
    previous_cost = float(previous_usage.get("cost_usd", 0.0) or 0.0)
    if not math.isfinite(previous_cost) or previous_cost < 0.0 or previous_cost > float(args.budget_usd) + 1e-9:
        raise SoftEventGoldError("resume_usage_invalid")

    leaf_times = build_leaf_times(index)
    # Build events strictly on top of the current stable leaf index.  No source
    # message is rewritten and the event result is kept in memory only.
    leaf_vectors = getattr(index, "leaf_vectors", None)
    stable_leaves = list(getattr(index, "leaves", ()) or ())
    event_result = build_soft_events(
        stable_leaves,
        recipe=recipe,
        leaf_vectors=leaf_vectors,
        leaf_times=leaf_times,
    )
    candidates = select_candidates(
        event_result.events,
        index,
        # Rejections are expected for synthetic data (copying, malformed JSON,
        # answerability, etc.).  Keep a deterministic reserve of candidates;
        # the room cap still bounds the total pool.
        questions=(
            max(int(args.questions) * 3, int(args.questions) + 24)
            if int(args.questions) > 0
            else 0
        ),
        seed=int(args.seed),
        max_per_room=int(args.max_per_room),
        lock_fraction=float(args.lock_fraction),
        max_prompt_messages=int(args.max_prompt_messages),
        message_char_limit=int(args.message_char_limit),
    )

    prior_questions: list[dict[str, Any]] = []
    if existing:
        raw_questions = existing.get("questions")
        if isinstance(raw_questions, list):
            prior_questions = [dict(row) for row in raw_questions if isinstance(row, Mapping)]
    used_event_ids = {str(row.get("source_event_id") or "") for row in prior_questions}
    pending = [candidate for candidate in candidates if candidate.event.event_id not in used_event_ids]
    cfg_for_client = dict(cfg)
    cfg_for_client["models"] = dict(cfg.get("models") or {})
    if generation_model:
        cfg_for_client["models"]["response"] = generation_model
    remaining_budget = max(1e-9, float(args.budget_usd) - previous_cost)
    client = _build_client(
        cfg_for_client,
        budget_usd=remaining_budget,
        max_request_cost_usd=float(args.max_request_cost_usd),
    )
    generated, reasons = generate_questions(
        pending,
        client,
        target=max(0, int(args.questions) - len(prior_questions)),
        min_question_chars=int(args.min_question_chars),
        max_bigram_overlap=float(args.max_bigram_overlap),
    )
    questions = (prior_questions + generated)[: int(args.questions)]
    if len(candidates) < int(args.questions):
        reasons["candidate_shortfall"] = int(args.questions) - len(candidates)
    if len(questions) < int(args.questions):
        reasons["accepted_shortfall"] = int(args.questions) - len(questions)
    manifest = dict(manifest_base)
    manifest["usage"] = _merge_usage(previous_usage, _safe_usage(client))
    manifest["event_count"] = len(event_result.events)
    manifest["candidate_count"] = len(candidates)
    manifest["generated_count"] = len(generated)
    manifest["accepted_count"] = len(questions)
    manifest["accepted_shortfall"] = max(0, int(args.questions) - len(questions))
    payload = build_payload(questions=questions, binding=binding, manifest=manifest)
    _atomic_write_json(out, payload)
    summary_client = client
    if previous_cost:
        # The summary is log-safe and needs the cumulative session cost even
        # though the live client only held the remaining resume budget.
        class _CumulativeUsage:
            cost_usd = float(manifest["usage"].get("cost_usd", 0.0))

        class _CumulativeClient:
            usage = _CumulativeUsage()

        summary_client = _CumulativeClient()
    return payload, _summary_output(questions=questions, reasons=reasons, client=summary_client, out=out)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=_output_root() / "soft-event-gold" / "gold.json")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--questions", type=int, default=DEFAULT_QUESTIONS)
    parser.add_argument("--seed", type=int, default=20260922)
    parser.add_argument("--max-per-room", type=int, default=DEFAULT_MAX_PER_ROOM)
    parser.add_argument("--lock-fraction", "--lock-ratio", dest="lock_fraction", type=float, default=DEFAULT_LOCK_FRACTION)
    parser.add_argument("--budget-usd", type=float, default=DEFAULT_BUDGET_USD)
    parser.add_argument("--max-request-cost-usd", type=float, default=DEFAULT_MAX_REQUEST_COST_USD)
    parser.add_argument("--model", default="", help="OpenRouter generation model override")
    parser.add_argument("--gap-minutes", type=float, default=30.0)
    parser.add_argument("--max-event-leaves", type=int, default=8)
    parser.add_argument("--max-event-chars", type=int, default=4000)
    parser.add_argument("--max-prompt-messages", type=int, default=DEFAULT_MAX_PROMPT_MESSAGES)
    parser.add_argument("--message-char-limit", type=int, default=DEFAULT_MESSAGE_CHAR_LIMIT)
    parser.add_argument("--min-question-chars", type=int, default=DEFAULT_MIN_QUESTION_CHARS)
    parser.add_argument("--max-bigram-overlap", type=float, default=DEFAULT_MAX_BIGRAM_OVERLAP)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        _, summary = run(args)
    except Exception:  # noqa: BLE001 - CLI must never expose private/provider details
        # Error details may contain provider/path data, so keep stdout to a
        # stable reason code only.  No source text or generated question leaks.
        reason = "generation_failed"
        print(json.dumps({"stage": "soft_event_gold_failed", "counts": {}, "reasons": {reason: 1}, "cost_usd": 0.0}, ensure_ascii=False))
        return 2
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())


__all__ = [
    "EventCandidate",
    "GOLD_KIND",
    "PromptMessage",
    "QUESTION_TYPES",
    "SoftEventGoldError",
    "assign_split",
    "build_leaf_times",
    "build_payload",
    "code_hash",
    "generate_questions",
    "make_generation_prompt",
    "make_prompt_context",
    "query_evidence_bigram_overlap",
    "select_candidates",
    "validate_generated_question",
    "validate_output_path",
]
