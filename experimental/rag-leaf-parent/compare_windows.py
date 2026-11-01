#!/usr/bin/env python3
"""Bounded, retrieval-only comparison of fixed and adaptive context windows.

This is an intentionally small experiment runner.  The input is an explicit
JSON list of original messages and every generated artifact is written below
the caller supplied ``--out`` directory.  It does not discover a corpus, read
the playground index, or reuse any of the older window/vector/summary caches.

The default mode is a deterministic local self-test.  ``--live`` is required
before a provider is contacted; ``--offline`` only reads call artifacts that
already exist below ``--out``.  Retrieval metrics in this file measure visible
source-observation coverage.  They are not answer-accuracy metrics.
"""

from __future__ import annotations

import argparse
import hashlib
import inspect
import json
import math
import os
import re
import sys
import tempfile
from collections import Counter
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
EVAL = REPO / "tools" / "evaluation"
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
if str(EVAL) not in sys.path:
    sys.path.insert(0, str(EVAL))

from lexical import LexicalIndex  # noqa: E402
from openrouter import (  # noqa: E402
    BudgetError,
    OpenRouter,
    ProviderResponseError,
)


CODE_REVISION = "compare-windows-v1"
RECIPE_REVISION = "window-compare-rrf-bigram-dense4-v1"
GOLD_PROMPT_REVISION = "synthetic-gold-local-indices-v1"
SUMMARY_PROMPT_REVISION = "adaptive-summary-120-v1"
ANSWER_PROMPT_REVISION = "visible-context-answer-structure-v1"
MAX_QUESTIONS = 24
MAX_CANDIDATES = 40
EMBED_BATCH_SIZE = 16
SUMMARY_MAX_TOKENS = 120
DEFAULT_CONTEXT_CHARS = 1600
GOLD_GROUP_SIZE = 11

PROMPT_REVISIONS = {
    "gold": GOLD_PROMPT_REVISION,
    "summary": SUMMARY_PROMPT_REVISION,
    "answer": ANSWER_PROMPT_REVISION,
}


VARIANTS: tuple[dict[str, Any], ...] = (
    {"name": "fixed20", "mode": "fixed20", "expand_messages": 0, "summary": False},
    {
        "name": "fixed8+parent",
        "mode": "fixed8",
        "expand_messages": 4,
        "summary": False,
    },
    {
        "name": "adaptive+parent",
        "mode": "adaptive",
        "expand_messages": 4,
        "summary": False,
    },
    {
        "name": "adaptive+summary+parent",
        "mode": "adaptive",
        "expand_messages": 4,
        "summary": True,
    },
)


class ExperimentError(RuntimeError):
    """A safe experiment configuration or artifact error."""


class MissingArtifactError(ExperimentError):
    """An offline run did not find a required artifact."""


def _progress(stage: str, detail: str = "") -> None:
    """Emit only safe counters/stage names so long runs remain observable."""

    suffix = f" {detail}" if detail else ""
    print(f"[compare_windows] {stage}{suffix}", file=sys.stderr, flush=True)


def build_chunks(
    messages: list[dict[str, Any]],
    mode: str,
    *,
    idle_seconds: float = 300,
    target_chars: int = 600,
    max_chars: int = 1200,
    max_messages: int = 40,
) -> list[dict[str, Any]]:
    """Call the independent pure context helper lazily.

    Keeping this wrapper lazy makes the CLI importable for network-free tests
    even while the helper is being developed in a sibling working tree.
    """

    try:
        from adaptive_context import build_chunks as helper
    except ImportError as exc:  # pragma: no cover - only during bad installs
        raise ExperimentError("adaptive_context helper is unavailable") from exc
    return helper(
        messages,
        mode,
        idle_seconds=idle_seconds,
        target_chars=target_chars,
        max_chars=max_chars,
        max_messages=max_messages,
    )


def pack_context(
    messages: list[dict[str, Any]],
    chunks: list[dict[str, Any]],
    ranked_ids: list[str],
    *,
    budget_chars: int = 4000,
    expand_messages: int = 4,
) -> dict[str, Any]:
    """Call the independent pure packing helper lazily."""

    try:
        from adaptive_context import pack_context as helper
    except ImportError as exc:  # pragma: no cover - only during bad installs
        raise ExperimentError("adaptive_context helper is unavailable") from exc
    return helper(
        messages,
        chunks,
        ranked_ids,
        budget_chars=budget_chars,
        expand_messages=expand_messages,
    )


def _write_json(path: Path, value: Any) -> None:
    """Write a private JSON artifact atomically with restrictive permissions."""

    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, ensure_ascii=False, indent=2, sort_keys=True)
            handle.write("\n")
        os.chmod(name, 0o600)
        os.replace(name, path)
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
    finally:
        try:
            os.unlink(name)
        except FileNotFoundError:
            pass


def _read_json(path: Path) -> Any:
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError as exc:
        raise MissingArtifactError(f"required artifact is missing: {path.name}") from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise ExperimentError(f"artifact could not be read: {path.name}") from exc


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path) -> str:
    return _sha256_bytes(path.read_bytes())


def _code_hashes() -> dict[str, str]:
    result = {"compare_windows.py": _sha256_file(Path(__file__))}
    helper = HERE / "adaptive_context.py"
    if helper.exists():
        result["adaptive_context.py"] = _sha256_file(helper)
    return result


def _finite_number(value: Any, *, name: str, positive: bool = False) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise ExperimentError(f"{name} must be numeric") from exc
    if not math.isfinite(number) or (number <= 0 if positive else number < 0):
        qualifier = "positive finite" if positive else "finite non-negative"
        raise ExperimentError(f"{name} must be {qualifier}")
    return number


def _normalize_messages(value: Any) -> list[dict[str, Any]]:
    """Validate the explicit input list and add only local stable fields."""

    if not isinstance(value, list):
        raise ExperimentError("--input must contain a JSON list of messages")
    messages: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    for index, raw in enumerate(value):
        if not isinstance(raw, Mapping):
            raise ExperimentError(f"message {index} is not an object")
        row = dict(raw)
        content = row.get("content")
        if content is None:
            content = row.get("text", "")
        if not isinstance(content, str):
            content = str(content)
        row["content"] = content

        observation_id = row.get("observation_id")
        if observation_id is None or str(observation_id) == "":
            raise ExperimentError(f"message {index} observation_id is required")
        observation_id = str(observation_id)
        if observation_id in seen_ids:
            raise ExperimentError(f"message {index} has duplicate observation_id")
        seen_ids.add(observation_id)
        row["observation_id"] = observation_id

        room = row.get("source_room_id")
        if room is None or str(room) == "":
            room = row.get("room") or row.get("source_channel_id")
        if room is None or str(room) == "":
            raise ExperimentError(f"message {index} source_room_id is required")
        row["source_room_id"] = str(room)
        if row.get("actor_label") is None or str(row.get("actor_label")) == "":
            row["actor_label"] = "?"
        else:
            row["actor_label"] = str(row["actor_label"])
        # The helper uses sample_block.  Accept equivalent explicit names while
        # retaining the caller's source fields unchanged.
        if row.get("sample_block") is None:
            for key in ("block", "source_block", "block_id"):
                if row.get(key) is not None:
                    row["sample_block"] = str(row[key])
                    break
        row["index"] = index
        messages.append(row)
    if not messages:
        raise ExperimentError("input contains no messages")
    return messages


def _message_block_key(row: Mapping[str, Any], index: int) -> tuple[str, str]:
    room = str(row.get("source_room_original") or row.get("source_room_id") or row.get("room") or "input")
    block: Any = row.get("sample_block")
    if block is None:
        for key in ("block", "source_block", "block_id"):
            if row.get(key) is not None:
                block = row[key]
                break
    # The fallback is an experiment sampling block, independent of the four
    # retrieval recipes and therefore independent of their chunk boundaries.
    if block is None:
        block = index // 300
    return room, str(block)


def _gold_groups(messages: list[dict[str, Any]], *, group_size: int = 11) -> list[list[int]]:
    """Make contiguous, room/block-bounded groups before any chunking."""

    if group_size <= 0:
        raise ValueError("group_size must be positive")
    runs: list[list[int]] = []
    current: list[int] = []
    current_key: tuple[str, str] | None = None
    for index, row in enumerate(messages):
        key = _message_block_key(row, index)
        if current and key != current_key:
            runs.append(current)
            current = []
        current.append(index)
        current_key = key
    if current:
        runs.append(current)

    groups: list[list[int]] = []
    for run in runs:
        for start in range(0, len(run) - group_size + 1, group_size):
            groups.append(run[start : start + group_size])
    return groups


def _evenly_spaced(items: Sequence[list[int]], limit: int) -> list[list[int]]:
    if limit <= 0 or not items:
        return []
    if len(items) <= limit:
        return [list(item) for item in items]
    # Quantile selection keeps early/middle/late and different blocks in play.
    positions = [round(i * (len(items) - 1) / (limit - 1)) for i in range(limit)] if limit > 1 else [len(items) // 2]
    selected: list[list[int]] = []
    seen: set[int] = set()
    for position in positions:
        if position not in seen:
            selected.append(list(items[position]))
            seen.add(position)
    if len(selected) < limit:
        for position, item in enumerate(items):
            if position not in seen:
                selected.append(list(item))
                seen.add(position)
            if len(selected) >= limit:
                break
    return selected


def _render_group(messages: list[dict[str, Any]], indices: Sequence[int]) -> str:
    lines: list[str] = []
    for index in indices:
        row = messages[index]
        actor = str(row.get("actor_label") or "?")
        lines.append(f"[{index}] {actor}: {row.get('content') or ''}")
    return "\n".join(lines)


def _parse_json_object(raw: str) -> dict[str, Any]:
    text = (raw or "").strip()
    if "```" in text:
        pieces = text.split("```")
        text = pieces[1] if len(pieces) > 1 else text
        text = re.sub(r"^\s*json\s*", "", text, flags=re.IGNORECASE).strip()
    decoder = json.JSONDecoder()
    for position, char in enumerate(text):
        if char != "{":
            continue
        try:
            value, _ = decoder.raw_decode(text[position:])
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            return value
    raise ValueError("JSON object missing")


def _validate_gold(value: Any, allowed_indices: set[int]) -> tuple[dict[str, Any] | None, str | None]:
    if not isinstance(value, Mapping):
        return None, "object_required"
    question = value.get("question")
    answer = value.get("answer")
    evidence = value.get("evidence_indices")
    if not isinstance(question, str) or not question.strip():
        return None, "question_missing"
    if not isinstance(answer, str) or not answer.strip():
        return None, "answer_missing"
    if isinstance(evidence, (str, bytes, bytearray)) or not isinstance(evidence, list) or not evidence:
        return None, "evidence_indices_missing"
    indices: list[int] = []
    for item in evidence:
        if isinstance(item, bool) or not isinstance(item, int):
            return None, "evidence_index_invalid"
        if item not in allowed_indices:
            return None, "evidence_index_outside_group"
        if item not in indices:
            indices.append(item)
    if not indices:
        return None, "evidence_indices_empty"
    return {
        "question": question.strip(),
        "evidence_indices": indices,
        "answer": answer.strip(),
        "label": "synthetic_unreviewed/dev",
    }, None


def _deterministic_gold(messages: list[dict[str, Any]], groups: Sequence[list[int]]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    accepted: list[dict[str, Any]] = []
    rejected: list[dict[str, Any]] = []
    for group in groups:
        evidence = next((index for index in group if (messages[index].get("content") or "").strip()), group[0])
        snippet = " ".join(str(messages[evidence].get("content") or "").split())[:120]
        if not snippet:
            snippet = "이 대화 조각"
        item = {
            "question": f"대화에서 다음 내용이 나온 맥락은 무엇인가: {snippet}",
            "evidence_indices": [evidence],
            "answer": str(messages[evidence].get("content") or "(빈 발화)"),
            "label": "synthetic_unreviewed/dev",
            "group_indices": list(group),
        }
        accepted.append(item)
    return accepted, rejected


def _provider_policy(config: Mapping[str, str]) -> dict[str, Any]:
    name = str(config.get("EVAL_LLM_PROVIDER") or "DeepInfra").strip()
    if not name:
        raise ExperimentError("EVAL_LLM_PROVIDER is empty")
    return {"order": [name], "allow_fallbacks": True}


class PinnedOpenRouter(OpenRouter):
    """OpenRouter client adapter that keeps the configured route explicit."""

    def __init__(self, *args: Any, provider_policy: Mapping[str, Any], **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self.provider_policy = dict(provider_policy)

    def chat(
        self,
        messages: list[dict[str, Any]],
        *,
        max_tokens: int = 512,
        temperature: float = 0.0,
        reasoning: str | None = "low",
        provider: Mapping[str, Any] | None = None,
    ) -> str:
        payload: dict[str, Any] = {
            "model": self.llm_model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "provider": dict(provider or self.provider_policy),
        }
        if reasoning:
            payload["reasoning"] = {"effort": reasoning}
        body = self._post("/chat/completions", payload)
        choices = body.get("choices")
        if not isinstance(choices, list) or not choices:
            raise ProviderResponseError("chat response choices are missing")
        first = choices[0]
        if not isinstance(first, Mapping):
            raise ProviderResponseError("chat response choice is invalid")
        message = first.get("message")
        if not isinstance(message, Mapping) or not isinstance(message.get("content"), str):
            raise ProviderResponseError("chat response content is missing")
        return str(message["content"])


def _call_chat(
    client: Any,
    messages: list[dict[str, Any]],
    *,
    max_tokens: int,
    temperature: float,
    reasoning: str | None,
    provider: Mapping[str, Any],
) -> str:
    """Pass a provider only when the client's chat signature supports it."""

    signature = inspect.signature(client.chat)
    parameters = signature.parameters.values()
    accepts_provider = "provider" in signature.parameters or any(
        parameter.kind == inspect.Parameter.VAR_KEYWORD for parameter in parameters
    )
    if not accepts_provider:
        raise ExperimentError("chat client cannot pin the requested provider")
    return client.chat(
        messages,
        max_tokens=max_tokens,
        temperature=temperature,
        reasoning=reasoning,
        provider=dict(provider),
    )


USAGE_FIELDS = (
    "attempts",
    "calls",
    "retries",
    "failures",
    "prompt_tokens",
    "completion_tokens",
    "cost_usd",
    "reserved_cost_usd",
    "unknown_cost_attempts",
)


def _zero_usage() -> dict[str, Any]:
    return {field: 0.0 if field.endswith("usd") else 0 for field in USAGE_FIELDS} | {"errors": {}}


def _usage_dict(value: Any) -> dict[str, Any]:
    result = _zero_usage()
    if value is None:
        return result
    source = value if isinstance(value, Mapping) else vars(value)
    for field in USAGE_FIELDS:
        raw = source.get(field, result[field])
        if field.endswith("usd"):
            result[field] = _finite_number(raw, name=field)
        else:
            if isinstance(raw, bool):
                raise ExperimentError(f"usage field {field} is invalid")
            try:
                integer = int(raw)
            except (TypeError, ValueError) as exc:
                raise ExperimentError(f"usage field {field} is invalid") from exc
            if integer < 0:
                raise ExperimentError(f"usage field {field} is invalid")
            result[field] = integer
    errors = source.get("errors", {})
    if isinstance(errors, Mapping):
        result["errors"] = {str(k): int(v) for k, v in errors.items() if int(v) > 0}
    return result


def _sum_usage(left: Mapping[str, Any], right: Mapping[str, Any]) -> dict[str, Any]:
    result = _zero_usage()
    for field in USAGE_FIELDS:
        result[field] = float(left.get(field, 0)) + float(right.get(field, 0)) if field.endswith("usd") else int(left.get(field, 0)) + int(right.get(field, 0))
    errors: Counter[str] = Counter()
    errors.update(left.get("errors", {}) or {})
    errors.update(right.get("errors", {}) or {})
    result["errors"] = dict(errors)
    return result


class UsageLedger:
    """Persist aggregate provider usage and fail closed on unknown charges."""

    def __init__(self, out: Path, budget_usd: float) -> None:
        self.path = out / "usage.json"
        self.budget_usd = budget_usd
        self.prior = _zero_usage()
        self.pending_request = False
        if self.path.exists():
            raw = _read_json(self.path)
            if isinstance(raw, Mapping):
                if "pending_request" in raw and raw.get("pending_request") is not False:
                    raise ExperimentError("prior usage has a pending request; reconcile before resume")
                if "usage" in raw:
                    raw = raw["usage"]
            self.prior = _usage_dict(raw)
        if self.prior["unknown_cost_attempts"] or self.prior["reserved_cost_usd"] > 1e-12:
            raise ExperimentError("prior usage has unknown or reserved cost; reconcile before resume")
        if self.prior["cost_usd"] > budget_usd + 1e-12:
            raise ExperimentError("prior known cost exceeds requested budget")
        self.client: Any | None = None

    @property
    def remaining_usd(self) -> float:
        return max(0.0, self.budget_usd - float(self.prior["cost_usd"]))

    def attach(self, client: Any) -> None:
        self.client = client
        self.checkpoint()

    def current(self) -> dict[str, Any]:
        return _usage_dict(getattr(self.client, "usage", None)) if self.client is not None else _zero_usage()

    def aggregate(self) -> dict[str, Any]:
        return _sum_usage(self.prior, self.current())

    def _persist(self, aggregate: Mapping[str, Any], *, pending_request: bool) -> dict[str, Any]:
        _write_json(
            self.path,
            {
                "schema_version": 1,
                "pending_request": bool(pending_request),
                "usage": dict(aggregate),
            },
        )
        return dict(aggregate)

    def begin_request(self) -> None:
        """Durably mark a physical request before entering provider code."""

        if self.pending_request:
            raise ExperimentError("another provider request is already pending")
        self.pending_request = True
        try:
            self._persist(self.aggregate(), pending_request=True)
        except Exception:
            self.pending_request = False
            raise

    def clear_request(self) -> None:
        """Clear the marker only after the latest usage was durably checkpointed."""

        if not self.pending_request:
            return
        self._persist(self.aggregate(), pending_request=False)
        self.pending_request = False

    def checkpoint(self) -> dict[str, Any]:
        aggregate = self.aggregate()
        return self._persist(aggregate, pending_request=self.pending_request)


class CachedCalls:
    """Content-addressed, per-provider-call cache scoped strictly to ``out``."""

    def __init__(
        self,
        out: Path,
        *,
        runtime: str,
        client: Any | None,
        ledger: UsageLedger,
        provider_policy: Mapping[str, Any] | None,
    ) -> None:
        self.cache_dir = out / "cache"
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        try:
            os.chmod(self.cache_dir, 0o700)
        except OSError:
            pass
        self.runtime = runtime
        self.client = client
        self.ledger = ledger
        self.provider_policy = dict(provider_policy or {})

    def _key(self, operation: str, model: str, payload: Mapping[str, Any]) -> str:
        envelope = {
            "cache_schema": 1,
            "code_revision": CODE_REVISION,
            "recipe_revision": RECIPE_REVISION,
            "operation": operation,
            "model": model,
            "payload": payload,
        }
        encoded = json.dumps(envelope, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
        return _sha256_bytes(encoded)

    def _cached(
        self,
        operation: str,
        model: str,
        payload: Mapping[str, Any],
        invoke: Callable[[], Any],
    ) -> tuple[Any, bool]:
        key = self._key(operation, model, payload)
        path = self.cache_dir / f"{key}.json"
        if path.exists():
            try:
                cached = _read_json(path)
                if (
                    isinstance(cached, Mapping)
                    and cached.get("cache_schema") == 1
                    and cached.get("key") == key
                    and cached.get("operation") == operation
                    and cached.get("model") == model
                    and "result" in cached
                ):
                    return cached["result"], True
            except ExperimentError:
                if self.runtime == "offline":
                    raise
        if self.runtime == "offline":
            raise MissingArtifactError(f"cached {operation} call is missing")
        if self.runtime != "live":
            raise ExperimentError("provider call requested outside live mode")
        self.ledger.begin_request()
        try:
            result = invoke()
        except Exception:
            # OpenRouter records an unknown-cost attempt before raising.  The
            # checkpoint makes a resumed process fail closed instead of
            # silently spending a second budget.
            self.ledger.checkpoint()
            self.ledger.clear_request()
            raise
        self.ledger.checkpoint()
        self.ledger.clear_request()
        _write_json(
            path,
            {
                "cache_schema": 1,
                "key": key,
                "operation": operation,
                "model": model,
                "result": result,
            },
        )
        return result, False

    def chat(
        self,
        *,
        model: str,
        messages: list[dict[str, Any]],
        max_tokens: int,
        temperature: float,
        reasoning: str | None,
    ) -> tuple[str, bool]:
        payload: dict[str, Any] = {
            "model": model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "provider": self.provider_policy,
        }
        if reasoning:
            payload["reasoning"] = {"effort": reasoning}

        def invoke() -> str:
            if self.client is None:
                raise ExperimentError("chat client is unavailable")
            return _call_chat(
                self.client,
                messages,
                max_tokens=max_tokens,
                temperature=temperature,
                reasoning=reasoning,
                provider=self.provider_policy,
            )

        result, cached = self._cached("chat", model, payload, invoke)
        if not isinstance(result, str):
            raise ExperimentError("cached chat result is invalid")
        return result, cached

    def embed(self, *, model: str, texts: list[str]) -> tuple[list[list[float]], bool]:
        payload = {"model": model, "input": texts, "provider": self.provider_policy}

        def invoke() -> list[list[float]]:
            if self.client is None:
                raise ExperimentError("embedding client is unavailable")
            result = self.client.embed(texts)
            if not isinstance(result, list):
                raise ExperimentError("embedding result is invalid")
            return result

        result, cached = self._cached("embedding", model, payload, invoke)
        if not isinstance(result, list) or len(result) != len(texts):
            raise ExperimentError("cached embedding result count is invalid")
        vectors: list[list[float]] = []
        for vector in result:
            if not isinstance(vector, list) or not vector:
                raise ExperimentError("cached embedding vector is invalid")
            converted = [float(value) for value in vector]
            if not all(math.isfinite(value) for value in converted):
                raise ExperimentError("cached embedding vector is non-finite")
            if not any(converted):
                raise ExperimentError("cached embedding vector is zero")
            vectors.append(converted)
        return vectors, cached


def _local_embed(text: str, dimension: int = 64) -> list[float]:
    """Stable hash embedding for the no-provider self-test only."""

    tokens = re.findall(r"[0-9A-Za-z_]+|[가-힣]+", text.casefold())
    if not tokens:
        tokens = [text or "<empty>"]
    vector = [0.0] * dimension
    for token in tokens:
        digest = hashlib.sha256(token.encode("utf-8")).digest()
        slot = int.from_bytes(digest[:4], "big") % dimension
        sign = 1.0 if digest[4] & 1 else -1.0
        vector[slot] += sign
        # A second slot makes short Korean strings less collision-prone while
        # remaining deterministic and dependency-free.
        slot2 = int.from_bytes(digest[5:9], "big") % dimension
        vector[slot2] += sign * 0.5
    norm = math.sqrt(sum(value * value for value in vector)) or 1.0
    return [value / norm for value in vector]


class Embeddings:
    def __init__(self, *, runtime: str, calls: CachedCalls, model: str | None) -> None:
        self.runtime = runtime
        self.calls = calls
        self.model = model

    def many(self, texts: Sequence[str]) -> list[list[float]]:
        if not texts:
            return []
        if self.runtime == "synthetic":
            return [_local_embed(text) for text in texts]
        if not self.model:
            raise ExperimentError("embedding model is unavailable")
        unique = list(dict.fromkeys(str(text) for text in texts))
        vectors_by_text: dict[str, list[float]] = {}
        for start in range(0, len(unique), EMBED_BATCH_SIZE):
            batch = unique[start : start + EMBED_BATCH_SIZE]
            vectors, _ = self.calls.embed(model=self.model, texts=batch)
            for text, vector in zip(batch, vectors):
                vectors_by_text[text] = vector
        return [vectors_by_text[str(text)] for text in texts]


def _limit_words(text: str, limit: int) -> str:
    words = str(text or "").split()
    if len(words) <= limit:
        return str(text or "").strip()
    return " ".join(words[:limit]).strip()


def _local_summary(body: str) -> str:
    # This is explicitly a self-test summary, never a production memory.
    return _limit_words(body, SUMMARY_MAX_TOKENS)


def _chunk_fingerprint(chunks: Sequence[Mapping[str, Any]]) -> str:
    value = [
        {
            "doc_id": str(chunk.get("doc_id")),
            "body": str(chunk.get("body") or ""),
            "message_indices": list(chunk.get("message_indices") or []),
        }
        for chunk in chunks
    ]
    return _sha256_bytes(json.dumps(value, ensure_ascii=False, sort_keys=True).encode("utf-8"))


def _load_summary_artifact(path: Path, *, input_sha: str, fingerprint: str) -> dict[str, str]:
    if not path.exists():
        return {}
    value = _read_json(path)
    if (
        not isinstance(value, Mapping)
        or value.get("input_sha256") != input_sha
        or value.get("chunk_fingerprint") != fingerprint
        or value.get("prompt_revision") != SUMMARY_PROMPT_REVISION
    ):
        return {}
    raw = value.get("summaries")
    if not isinstance(raw, Mapping):
        return {}
    return {str(key): str(text) for key, text in raw.items() if isinstance(text, str) and text.strip()}


def _make_gold_prompt(body: str) -> str:
    return f"""다음은 원문 대화의 한 조각이다. 조각 밖의 사실을 사용하지 말라.\n\n<대화>\n{body}\n</대화>\n\n질문 하나와 그 답을 JSON 하나로만 출력하라. 형식은 다음과 같다.\n{{\"question\": \"한국어 질문\", \"evidence_indices\": [0], \"answer\": \"짧은 답\"}}\n\n- evidence_indices는 위 대화에서 답을 직접 뒷받침하는 [local index]만 고른다.\n- 반드시 하나 이상 고르고, 위에 실제로 표시된 index만 사용한다.\n- question과 answer는 대화에 있는 내용만 사용한다.\n"""


def _generate_gold(
    messages: list[dict[str, Any]],
    groups: Sequence[list[int]],
    *,
    requested: int,
    runtime: str,
    calls: CachedCalls,
    llm_model: str | None,
    out: Path,
    input_sha: str,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    artifact_path = out / "gold.json"
    if runtime == "offline" and artifact_path.exists():
        cached = _read_json(artifact_path)
        if isinstance(cached, Mapping) and cached.get("input_sha256") == input_sha:
            accepted = cached.get("accepted")
            rejected = cached.get("rejected")
            if isinstance(accepted, list) and isinstance(rejected, list):
                return accepted[:requested], rejected

    selected = list(groups)[:requested]
    if runtime == "synthetic":
        accepted, rejected = _deterministic_gold(messages, selected)
        _write_json(
            artifact_path,
            {
                "schema_version": 1,
                "input_sha256": input_sha,
                "prompt_revision": GOLD_PROMPT_REVISION,
                "label": "synthetic_unreviewed/dev",
                "groups": [
                    {"indices": group, "body": _render_group(messages, group)}
                    for group in selected
                ],
                "accepted": accepted,
                "rejected": rejected,
            },
        )
        return accepted, rejected

    if not llm_model:
        raise ExperimentError("LLM model is unavailable")
    accepted: list[dict[str, Any]] = []
    rejected: list[dict[str, Any]] = []
    for group in selected:
        body = _render_group(messages, group)
        prompt = _make_gold_prompt(body)
        try:
            raw, _ = calls.chat(
                model=llm_model,
                messages=[{"role": "user", "content": prompt}],
                max_tokens=300,
                temperature=0.2,
                reasoning="low",
            )
            obj = _parse_json_object(raw)
            item, reason = _validate_gold(obj, set(group))
            if item is None:
                rejected.append({"indices": list(group), "reason": reason or "invalid"})
            else:
                item["group_indices"] = list(group)
                accepted.append(item)
        except BudgetError:
            raise
        except Exception as exc:  # noqa: BLE001 - only type is persisted
            if calls.client is not None and getattr(calls.client.usage, "unknown_cost_attempts", 0):
                raise
            rejected.append({"indices": list(group), "reason": type(exc).__name__})

    _write_json(
        artifact_path,
        {
            "schema_version": 1,
            "input_sha256": input_sha,
            "prompt_revision": GOLD_PROMPT_REVISION,
            "label": "synthetic_unreviewed/dev",
            "groups": [
                {"indices": group, "body": _render_group(messages, group)}
                for group in selected
            ],
            "accepted": accepted,
            "rejected": rejected,
        },
    )
    return accepted, rejected


def _build_summaries(
    messages: list[dict[str, Any]],
    chunks: list[dict[str, Any]],
    *,
    runtime: str,
    calls: CachedCalls,
    llm_model: str | None,
    out: Path,
    input_sha: str,
) -> tuple[dict[str, str], list[str]]:
    fingerprint = _chunk_fingerprint(chunks)
    path = out / "summaries.json"
    summaries = _load_summary_artifact(path, input_sha=input_sha, fingerprint=fingerprint)
    failed: list[str] = []
    if runtime == "synthetic":
        for chunk in chunks:
            doc_id = str(chunk.get("doc_id"))
            summaries.setdefault(doc_id, _local_summary(str(chunk.get("body") or "")))
        _write_json(
            path,
            {
                "schema_version": 1,
                "input_sha256": input_sha,
                "chunk_fingerprint": fingerprint,
                "prompt_revision": SUMMARY_PROMPT_REVISION,
                "summaries": summaries,
            },
        )
        return summaries, failed

    if not llm_model:
        raise ExperimentError("LLM model is unavailable")
    completed = 0
    for chunk in chunks:
        doc_id = str(chunk.get("doc_id"))
        if doc_id in summaries:
            continue
        body = str(chunk.get("body") or "")
        prompt = (
            "한국어 대화 조각을 최대 120 토큰의 짧은 검색용 요약으로 줄여라. "
            "고유명사·사건·결정은 남기고 추측하지 마라. 요약만 출력하라.\n\n"
            f"<대화>\n{body}\n</대화>"
        )
        try:
            raw, _ = calls.chat(
                model=llm_model,
                messages=[{"role": "user", "content": prompt}],
                max_tokens=SUMMARY_MAX_TOKENS,
                temperature=0.1,
                reasoning="low",
            )
            summary = _limit_words(raw, SUMMARY_MAX_TOKENS)
            if summary:
                summaries[doc_id] = summary
            else:
                failed.append(doc_id)
        except BudgetError:
            raise
        except Exception as exc:  # noqa: BLE001 - only type is persisted
            if calls.client is not None and getattr(calls.client.usage, "unknown_cost_attempts", 0):
                raise
            failed.append(f"summary_error:{type(exc).__name__}")
        completed += 1
        if completed % 10 == 0 or completed == len(chunks):
            _progress("summaries", f"{completed}/{len(chunks)}")
        # A partial summary file plus the per-call cache is enough to resume.
        _write_json(
            path,
            {
                "schema_version": 1,
                "input_sha256": input_sha,
                "chunk_fingerprint": fingerprint,
                "prompt_revision": SUMMARY_PROMPT_REVISION,
                "summaries": summaries,
            },
        )
    return summaries, failed


def _dot(left: Sequence[float], right: Sequence[float]) -> float:
    if len(left) != len(right):
        raise ExperimentError("embedding dimensions differ")
    left_norm = math.sqrt(sum(value * value for value in left)) or 1.0
    right_norm = math.sqrt(sum(value * value for value in right)) or 1.0
    return sum(a * b for a, b in zip(left, right)) / (left_norm * right_norm)


def _rrf_rank(
    lexical_ids: Sequence[str],
    dense_ids: Sequence[str],
    *,
    lexical_weight: float = 1.0,
    dense_weight: float = 4.0,
    extra_ranks: Sequence[tuple[Sequence[str], float]] = (),
    limit: int = MAX_CANDIDATES,
) -> list[str]:
    """Weighted reciprocal-rank fusion with a bounded candidate union."""

    scores: dict[str, float] = {}
    order: dict[str, int] = {}
    streams = [
        (lexical_ids, lexical_weight),
        (dense_ids, dense_weight),
        *list(extra_ranks),
    ]
    for ids, weight in streams:
        for rank, doc_id in enumerate(ids, 1):
            doc_id = str(doc_id)
            order.setdefault(doc_id, len(order))
            scores[doc_id] = scores.get(doc_id, 0.0) + float(weight) / (60.0 + rank)
    ranked = sorted(scores, key=lambda doc_id: (-scores[doc_id], order[doc_id], doc_id))
    return ranked[: max(0, int(limit))]


class Retriever:
    def __init__(
        self,
        chunks: list[dict[str, Any]],
        vectors: list[list[float]],
        *,
        summaries: Mapping[str, str] | None = None,
        summary_vectors: Mapping[str, list[float]] | None = None,
    ) -> None:
        if len(chunks) != len(vectors):
            raise ExperimentError("chunk/vector count mismatch")
        self.chunks = chunks
        self.vectors = vectors
        self.by_id = {str(chunk.get("doc_id")): chunk for chunk in chunks}
        self.summaries = dict(summaries or {})
        self.summary_vectors = dict(summary_vectors or {})
        self.lexical = LexicalIndex("bigram")
        self.lexical.add_many(
            [(str(chunk.get("doc_id")), str(chunk.get("body") or "")) for chunk in chunks]
        )

    def rank(self, question: str, query_vector: list[float], *, use_summary: bool) -> list[str]:
        lexical_ids = [doc_id for doc_id, _ in self.lexical.search(question, limit=MAX_CANDIDATES)]
        dense_pairs = [
            (_dot(query_vector, vector), str(chunk.get("doc_id")), position)
            for position, (chunk, vector) in enumerate(zip(self.chunks, self.vectors))
        ]
        dense_pairs.sort(key=lambda item: (-item[0], item[2], item[1]))
        dense_ids = [doc_id for _, doc_id, _ in dense_pairs[:MAX_CANDIDATES]]
        extras: list[tuple[Sequence[str], float]] = []
        if use_summary and self.summary_vectors:
            summary_pairs = [
                (_dot(query_vector, vector), doc_id, position)
                for position, (doc_id, vector) in enumerate(self.summary_vectors.items())
                if doc_id in self.by_id
            ]
            summary_pairs.sort(key=lambda item: (-item[0], item[2], item[1]))
            extras.append(([doc_id for _, doc_id, _ in summary_pairs[:MAX_CANDIDATES]], 1.0))
        return _rrf_rank(lexical_ids, dense_ids, extra_ranks=extras, limit=MAX_CANDIDATES)


def _visible_indices(messages: list[dict[str, Any]], visible_ids: Iterable[Any]) -> list[int]:
    by_id = {str(row.get("observation_id")): index for index, row in enumerate(messages)}
    return [by_id[str(value)] for value in visible_ids if str(value) in by_id]


def _metric(values: list[bool]) -> dict[str, Any]:
    return {"count": sum(1 for value in values if value), "n": len(values), "rate": (sum(values) / len(values) if values else None)}


def _answer_prompt(question: str, context_text: str, visible_indices: Sequence[int]) -> str:
    return f"""아래 원문 근거만 사용해 질문에 답하고 JSON 하나만 출력하라.\n\n<질문>\n{question}\n</질문>\n<근거>\n{context_text}\n</근거>\n\n형식: {{\"answer\": \"짧은 답\", \"evidence_indices\": [0], \"abstain\": false}}\n근거에 답이 없으면 abstain=true로 하고 evidence_indices는 []로 하라. 답을 뒷받침하는 위 local index만 고른다. 허용된 local indices: {list(visible_indices)}\n"""


def _evaluate_answers(
    messages: list[dict[str, Any]],
    gold: Sequence[dict[str, Any]],
    contexts: dict[str, list[dict[str, Any]]],
    *,
    runtime: str,
    calls: CachedCalls,
    llm_model: str | None,
    limit: int,
) -> dict[str, Any]:
    limit = min(max(0, int(limit)), 8)
    if limit == 0:
        return {"status": "not_requested", "n": 0}
    if runtime != "live" or not llm_model:
        return {"status": "not_run_provider_disabled", "n": 0}
    structural: list[bool] = []
    visible_citations: list[bool] = []
    abstains = 0
    for index, item in enumerate(gold[:limit]):
        record = contexts[index]
        visible = set(record.get("visible_indices") or [])
        prompt = _answer_prompt(str(item["question"]), str(record.get("text") or ""), sorted(visible))
        try:
            raw, _ = calls.chat(
                model=llm_model,
                messages=[{"role": "user", "content": prompt}],
                max_tokens=300,
                temperature=0.0,
                reasoning="low",
            )
            obj = _parse_json_object(raw)
            answer = obj.get("answer")
            evidence = obj.get("evidence_indices")
            abstain = obj.get("abstain")
            valid = isinstance(answer, str) and isinstance(abstain, bool) and isinstance(evidence, list)
            if valid:
                valid = all(isinstance(value, int) and not isinstance(value, bool) for value in evidence)
            citation_ok = bool(valid and set(evidence).issubset(visible))
            if valid and abstain:
                abstains += 1
            structural.append(bool(valid))
            visible_citations.append(citation_ok)
            record["answer"] = {
                "structural_valid": bool(valid),
                "evidence_indices_visible": citation_ok,
                "abstain": bool(abstain) if isinstance(abstain, bool) else None,
            }
        except BudgetError:
            raise
        except Exception as exc:  # noqa: BLE001 - only type is persisted
            if calls.client is not None and getattr(calls.client.usage, "unknown_cost_attempts", 0):
                raise
            structural.append(False)
            visible_citations.append(False)
            record["answer"] = {"structural_valid": False, "error": type(exc).__name__}
    return {
        "status": "structural_checks_only",
        "n": len(structural),
        "structural_valid": _metric(structural),
        "evidence_indices_visible": _metric(visible_citations),
        "abstain_count": abstains,
        "limitation": "These citation checks do not judge semantic answer correctness.",
    }


def _make_client(
    *,
    args: argparse.Namespace,
    ledger: UsageLedger,
) -> tuple[Any | None, dict[str, Any], dict[str, str]]:
    if args.live is False:
        return None, {"mode": "offline" if args.offline else "synthetic_local"}, {}
    if not args.secrets:
        raise ExperimentError("--secrets PATH is required with --live")
    from openrouter import load_secrets

    config = load_secrets(Path(args.secrets))
    if ledger.remaining_usd <= 0:
        raise ExperimentError("remaining experiment budget is exhausted")
    policy = _provider_policy(config)
    client = PinnedOpenRouter(
        config,
        budget_usd=ledger.remaining_usd,
        max_request_cost_usd=0.05,
        provider_policy=policy,
    )
    ledger.attach(client)
    models = {"llm": str(client.llm_model), "embedding": str(client.embed_model)}
    return client, {"mode": "live", "policy": policy}, models


def _segmentation_params(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "target_chars": args.target_chars,
        "max_chars": args.max_chars,
        "max_messages": args.max_messages,
        "idle_seconds": args.idle_seconds,
    }


def _routing_identity(value: Any) -> Any:
    if not isinstance(value, Mapping):
        return None
    return {str(key): item for key, item in value.items() if str(key) != "mode"}


def _validate_existing_meta(
    existing: Mapping[str, Any],
    *,
    input_sha: str,
    recipe_revision: str,
    runtime: str,
    models: Mapping[str, Any],
    routing: Mapping[str, Any],
    code_hashes: Mapping[str, str],
    prompt_revisions: Mapping[str, str],
    segmentation_params: Mapping[str, Any],
    context_chars: int,
) -> None:
    required = (
        "input_sha256",
        "recipe_revision",
        "runtime",
        "models",
        "provider_routing",
        "code_hashes",
        "prompt_revisions",
        "segmentation_params",
        "context_chars",
    )
    if any(key not in existing for key in required):
        raise ExperimentError("existing run metadata is incomplete; use a new --out")
    if existing.get("input_sha256") != input_sha or existing.get("recipe_revision") != recipe_revision:
        raise ExperimentError("existing output belongs to another input or recipe; use a new --out")

    prior_runtime = str(existing.get("runtime"))
    if runtime == "live" and prior_runtime != "live":
        raise ExperimentError("live run cannot reuse synthetic or offline output")
    if runtime == "synthetic" and prior_runtime != "synthetic":
        raise ExperimentError("synthetic run cannot reuse live output")
    if runtime == "offline" and prior_runtime not in {"live", "synthetic", "offline"}:
        raise ExperimentError("offline run has an unknown source runtime")

    if dict(existing.get("models") or {}) != dict(models):
        raise ExperimentError("existing output uses different models; use a new --out")
    if _routing_identity(existing.get("provider_routing")) != _routing_identity(routing):
        raise ExperimentError("existing output uses different provider routing; use a new --out")
    if dict(existing.get("code_hashes") or {}) != dict(code_hashes):
        raise ExperimentError("existing output uses different code; use a new --out")
    if dict(existing.get("prompt_revisions") or {}) != dict(prompt_revisions):
        raise ExperimentError("existing output uses different prompts; use a new --out")
    if dict(existing.get("segmentation_params") or {}) != dict(segmentation_params):
        raise ExperimentError("existing output uses different segmentation; use a new --out")
    # An offline pass may only repack the same chunks with a new context budget.
    if runtime != "offline" and existing.get("context_chars") != context_chars:
        raise ExperimentError("only offline runs may change context_chars")


def _arg_positive(value: str) -> int:
    try:
        result = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be an integer") from exc
    if result <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return result


def _arg_budget(value: str) -> float:
    try:
        result = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be numeric") from exc
    if not math.isfinite(result) or result <= 0:
        raise argparse.ArgumentTypeError("must be a positive finite number")
    return result


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description="bounded fixed/adaptive RAG window comparison")
    result.add_argument("--input", required=True, type=Path, help="JSON list of original messages")
    result.add_argument("--out", required=True, type=Path, help="isolated scratch artifact directory")
    result.add_argument("--secrets", type=Path, help="explicit secrets file; read only with --live")
    result.add_argument("--budget-usd", type=_arg_budget, default=1.0)
    result.add_argument("--questions", type=_arg_positive, default=MAX_QUESTIONS)
    result.add_argument("--context-chars", type=_arg_positive, default=DEFAULT_CONTEXT_CHARS)
    result.add_argument("--target-chars", type=_arg_positive, default=300)
    result.add_argument("--max-chars", type=_arg_positive, default=1200)
    result.add_argument("--max-messages", type=_arg_positive, default=40)
    result.add_argument("--idle-seconds", type=lambda value: _finite_number(value, name="idle_seconds"), default=300.0)
    result.add_argument("--answer-questions", "--answers", dest="answer_questions", type=int, default=0)
    result.add_argument("--live", action="store_true", help="allow explicitly configured provider calls")
    result.add_argument("--offline", action="store_true", help="reuse only call/artifact files below --out")
    return result


def run(args: argparse.Namespace) -> dict[str, Any]:
    if args.live and args.offline:
        raise ExperimentError("--live and --offline are mutually exclusive")
    if args.questions <= 0 or args.questions > MAX_QUESTIONS:
        raise ExperimentError(f"--questions must be between 1 and {MAX_QUESTIONS}")
    if args.answer_questions < 0 or args.answer_questions > 8:
        raise ExperimentError("--answer-questions must be between 0 and 8")
    if args.max_chars < args.target_chars:
        raise ExperimentError("--max-chars must be at least --target-chars")

    input_path = Path(args.input)
    input_bytes = input_path.read_bytes()
    input_sha = _sha256_bytes(input_bytes)
    try:
        raw_messages = json.loads(input_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ExperimentError("--input is not valid UTF-8 JSON") from exc
    messages = _normalize_messages(raw_messages)

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(out, 0o700)
    except OSError:
        pass
    metadata_path = out / "run_meta.json"
    existing_meta = _read_json(metadata_path) if metadata_path.exists() else None
    runtime = "live" if args.live else ("offline" if args.offline else "synthetic")
    _progress("input", f"{len(messages)} messages")
    ledger = UsageLedger(out, args.budget_usd)
    client, routing, models = _make_client(args=args, ledger=ledger)
    if args.offline and isinstance(existing_meta, Mapping):
        saved_models = existing_meta.get("models")
        if isinstance(saved_models, Mapping):
            models = {str(key): str(value) for key, value in saved_models.items()}
        saved_routing = existing_meta.get("provider_routing")
        if isinstance(saved_routing, Mapping):
            routing = {"mode": "offline"}
            if isinstance(saved_routing.get("policy"), Mapping):
                routing["policy"] = dict(saved_routing["policy"])
    if args.live and ledger.client is None:
        ledger.attach(client)
    calls = CachedCalls(
        out,
        runtime=runtime,
        client=client,
        ledger=ledger,
        provider_policy=routing.get("policy") if isinstance(routing, Mapping) else None,
    )

    code_hashes = _code_hashes()
    segmentation_params = _segmentation_params(args)
    if isinstance(existing_meta, Mapping):
        _validate_existing_meta(
            existing_meta,
            input_sha=input_sha,
            recipe_revision=RECIPE_REVISION,
            runtime=runtime,
            models=models,
            routing=routing,
            code_hashes=code_hashes,
            prompt_revisions=PROMPT_REVISIONS,
            segmentation_params=segmentation_params,
            context_chars=args.context_chars,
        )

    _write_json(
        metadata_path,
        {
            "schema_version": 1,
            "input_sha256": input_sha,
            "recipe_revision": RECIPE_REVISION,
            "code_revision": CODE_REVISION,
            "runtime": runtime,
            "models": models,
            "provider_routing": routing,
            "code_hashes": code_hashes,
            "prompt_revisions": PROMPT_REVISIONS,
            "segmentation_params": segmentation_params,
            "context_chars": args.context_chars,
        },
    )

    groups = _evenly_spaced(_gold_groups(messages, group_size=GOLD_GROUP_SIZE), args.questions)
    gold, rejected = _generate_gold(
        messages,
        groups,
        requested=args.questions,
        runtime=runtime,
        calls=calls,
        llm_model=models.get("llm"),
        out=out,
        input_sha=input_sha,
    )
    _progress("gold", f"accepted={len(gold)} rejected={len(rejected)}")
    # Cached/offline artifacts are still checked against the explicit local
    # index contract before they can influence any retrieval metric.
    valid_gold: list[dict[str, Any]] = []
    for item in gold:
        group_indices = set(item.get("group_indices") or [])
        checked, reason = _validate_gold(item, group_indices)
        if checked is None:
            rejected.append({"reason": reason or "cached_gold_invalid"})
            continue
        checked["group_indices"] = list(item.get("group_indices") or [])
        valid_gold.append(checked)
    gold = valid_gold

    chunk_sets: dict[str, list[dict[str, Any]]] = {}
    for spec in VARIANTS:
        chunk_sets[spec["name"]] = build_chunks(
            messages,
            spec["mode"],
            idle_seconds=args.idle_seconds,
            target_chars=args.target_chars,
            max_chars=args.max_chars,
            max_messages=args.max_messages,
        )
    _progress("chunks", f"variants={len(chunk_sets)}")

    all_chunk_texts: list[str] = []
    for chunks in chunk_sets.values():
        all_chunk_texts.extend(str(chunk.get("body") or "") for chunk in chunks)
    embeddings = Embeddings(runtime=runtime, calls=calls, model=models.get("embedding"))
    chunk_vectors_all = embeddings.many(all_chunk_texts)
    _progress("embeddings", f"chunks={len(all_chunk_texts)}")
    vector_cursor = 0
    chunk_vectors: dict[str, list[list[float]]] = {}
    for name, chunks in chunk_sets.items():
        chunk_vectors[name] = chunk_vectors_all[vector_cursor : vector_cursor + len(chunks)]
        vector_cursor += len(chunks)

    query_vectors = embeddings.many([str(item["question"]) for item in gold])
    summary_by_variant: dict[str, dict[str, str]] = {}
    summary_vectors_by_variant: dict[str, dict[str, list[float]]] = {}
    summary_failures: list[str] = []
    for spec in VARIANTS:
        if not spec["summary"]:
            continue
        name = spec["name"]
        summaries, failed = _build_summaries(
            messages,
            chunk_sets[name],
            runtime=runtime,
            calls=calls,
            llm_model=models.get("llm"),
            out=out,
            input_sha=input_sha,
        )
        summary_by_variant[name] = summaries
        summary_failures.extend(failed)
        texts = [summaries[doc_id] for doc_id in (str(chunk.get("doc_id")) for chunk in chunk_sets[name]) if doc_id in summaries]
        ids = [str(chunk.get("doc_id")) for chunk in chunk_sets[name] if str(chunk.get("doc_id")) in summaries]
        vectors = embeddings.many(texts)
        summary_vectors_by_variant[name] = dict(zip(ids, vectors))

    contexts_artifact: dict[str, Any] = {
        "schema_version": 1,
        "input_sha256": input_sha,
        "params": {
            "context_chars": args.context_chars,
            "target_chars": args.target_chars,
            "max_chars": args.max_chars,
            "max_messages": args.max_messages,
            "idle_seconds": args.idle_seconds,
            "gold_group_size": GOLD_GROUP_SIZE,
        },
        "variants": {},
    }
    report_variants: dict[str, Any] = {}
    answer_contexts: dict[str, list[dict[str, Any]]] = {}
    for spec in VARIANTS:
        name = spec["name"]
        chunks = chunk_sets[name]
        retriever = Retriever(
            chunks,
            chunk_vectors[name],
            summaries=summary_by_variant.get(name),
            summary_vectors=summary_vectors_by_variant.get(name),
        )
        rows: list[dict[str, Any]] = []
        all_hits: list[bool] = []
        any_hits: list[bool] = []
        raw_candidate_hit1: list[bool] = []
        used_chars: list[int] = []
        for q_index, (item, query_vector) in enumerate(zip(gold, query_vectors)):
            ranked = retriever.rank(str(item["question"]), query_vector, use_summary=bool(spec["summary"]))[:MAX_CANDIDATES]
            packed = pack_context(
                messages,
                chunks,
                ranked,
                budget_chars=args.context_chars,
                expand_messages=int(spec["expand_messages"]),
            )
            if not isinstance(packed, Mapping):
                raise ExperimentError("pack_context returned an invalid object")
            visible_ids = [str(value) for value in (packed.get("visible_observation_ids") or [])]
            visible = set(visible_ids)
            required_ids = {str(messages[index]["observation_id"]) for index in item["evidence_indices"]}
            all_ok = required_ids.issubset(visible)
            any_ok = bool(required_ids & visible)
            first_doc = ranked[:1]
            first_ids = set()
            if first_doc:
                first_chunk = retriever.by_id.get(first_doc[0])
                if first_chunk:
                    first_ids = {str(value) for value in (first_chunk.get("all_observation_ids") or [])}
            first_ok = bool(first_ids & required_ids)
            visible_local = _visible_indices(messages, visible_ids)
            used = int(packed.get("used_chars") or len(str(packed.get("text") or "")))
            all_hits.append(all_ok)
            any_hits.append(any_ok)
            raw_candidate_hit1.append(first_ok)
            used_chars.append(used)
            row = {
                "question_index": q_index,
                "ranked_ids": ranked,
                "required_indices": list(item["evidence_indices"]),
                "visible_observation_ids": visible_ids,
                "visible_indices": visible_local,
                "text": str(packed.get("text") or ""),
                "used_chars": used,
                "selected_docs": list(packed.get("selected_docs") or []),
                "evidence_all_visible": all_ok,
                "evidence_any_visible": any_ok,
                "raw_candidate_hit_at_1": first_ok,
            }
            rows.append(row)
        answer_contexts[name] = rows
        contexts_artifact["variants"][name] = {"questions": rows}
        report_variants[name] = {
            "n": len(rows),
            "evidence_all_visible": _metric(all_hits),
            "evidence_any_visible": _metric(any_hits),
            "raw_candidate_hit_at_1": _metric(raw_candidate_hit1),
            "actual_used_chars": {
                "min": min(used_chars) if used_chars else None,
                "max": max(used_chars) if used_chars else None,
                "mean": (sum(used_chars) / len(used_chars) if used_chars else None),
            },
            "candidate_cap": MAX_CANDIDATES,
            "summary_used_for_evidence": False,
        }

    # Answer evaluation is deliberately opt-in and bounded at eight questions.
    for spec in VARIANTS:
        name = spec["name"]
        report_variants[name]["answer_evaluation"] = _evaluate_answers(
            messages,
            gold,
            answer_contexts[name],
            runtime=runtime,
            calls=calls,
            llm_model=models.get("llm"),
            limit=args.answer_questions,
        )

    _write_json(out / "contexts.json", contexts_artifact)
    aggregate_usage = ledger.checkpoint()
    if aggregate_usage["unknown_cost_attempts"] or aggregate_usage["reserved_cost_usd"] > 1e-12:
        raise ExperimentError("provider usage has unknown or reserved cost")
    limitations = [
        "gold is synthetic_unreviewed/dev and is not human qualification",
        "evidence metrics only test required observation visibility in packed raw context",
        "retrieval evidence does not establish semantic answer correctness or attribution",
        "summary is a ranking signal only and is never counted as evidence",
        "fixed/adaptive comparisons use the same messages and accepted questions",
    ]
    if summary_failures:
        limitations.append(f"summary generation unavailable for {len(summary_failures)} chunk attempts")
    report = {
        "schema_version": 1,
        "status": "ok" if gold else "no_accepted_gold",
        "evaluation_label": "synthetic_unreviewed/dev",
        "input_sha256": input_sha,
        "code_hashes": _code_hashes(),
        "models": models,
        "provider_routing": routing,
        "params": {
            "questions_requested": args.questions,
            "gold_groups_selected": len(groups),
            "context_chars": args.context_chars,
            "target_chars": args.target_chars,
            "max_chars": args.max_chars,
            "max_messages": args.max_messages,
            "idle_seconds": args.idle_seconds,
            "rrf": {"lexical": "bigram", "weights": [1.0, 4.0], "summary_weight": 1.0},
            "candidate_cap": MAX_CANDIDATES,
            "summary_max_tokens": SUMMARY_MAX_TOKENS,
            "answer_questions": min(args.answer_questions, 8),
            "recipe_revision": RECIPE_REVISION,
        },
        "gold": {
            "groups_available": len(_gold_groups(messages, group_size=GOLD_GROUP_SIZE)),
            "accepted": len(gold),
            "rejected": len(rejected),
            "label": "synthetic_unreviewed/dev",
        },
        "variants": report_variants,
        "usage": aggregate_usage,
        "cost_usd": aggregate_usage["cost_usd"],
        "attempts": aggregate_usage["attempts"],
        "limitations": limitations,
    }
    _write_json(out / "report.json", report)
    return report


def main(argv: Sequence[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        report = run(args)
    except Exception as exc:  # noqa: BLE001 - do not expose source or payloads
        print(json.dumps({"status": "error", "error": type(exc).__name__}, ensure_ascii=False))
        return 1
    summary = {
        "status": report.get("status"),
        "runtime": report.get("provider_routing", {}).get("mode"),
        "gold_accepted": report.get("gold", {}).get("accepted", 0),
        "gold_rejected": report.get("gold", {}).get("rejected", 0),
        "variants": {name: data.get("n", 0) for name, data in (report.get("variants") or {}).items()},
        "cost_usd": report.get("cost_usd", 0.0),
        "attempts": report.get("attempts", 0),
    }
    print(json.dumps(summary, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
