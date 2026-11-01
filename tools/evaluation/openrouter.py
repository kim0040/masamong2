"""Small OpenRouter client used by the evaluation pilots.

The limiter is deliberately conservative: every physical request attempt must
have an explicit maximum cost reservation, and a response without a provider
reported cost stops subsequent paid requests until that attempt is reconciled.
The limiter is per ``OpenRouter`` instance.  It is a pilot safety control, not a
provider-key spending cap; OpenRouter/provider billing can still differ from a
client-side estimate.

Keys are read from the local secrets file only.  Request payloads, source text,
and credentials are never included in errors or usage summaries.
"""
from __future__ import annotations

import json
import math
import os
import random
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from numbers import Real
from pathlib import Path
from threading import RLock
from typing import Any, Callable, Mapping

SECRETS = Path(os.environ.get("EVAL_SECRETS", ".private/eval/secrets.env"))
RETRYABLE = {408, 409, 429, 500, 502, 503, 504, 520, 524}
MAX_ATTEMPTS = 6


class OpenRouterError(RuntimeError):
    """Base class for safe, user-facing client errors."""


class BudgetError(OpenRouterError):
    """A client-side budget/configuration signal.

    Callers must not silently turn this into another paid retry.  In
    particular, ``_post`` re-raises this class before its general transport
    error handler.
    """


class BudgetConfigurationError(BudgetError):
    """The limiter does not have a safe, explicit cost configuration."""


class BudgetExceededError(BudgetError):
    """The next request would reach or exceed the configured budget."""


class BudgetReconciliationError(BudgetError):
    """A previous attempt has no verified cost and must be reconciled."""


class BudgetOverrunError(BudgetError):
    """A provider-reported charge exceeded the configured request maximum."""


# A descriptive alias for callers that only need to catch a budget signal.
BudgetSignal = BudgetError


class ProviderRequestError(OpenRouterError):
    """A provider/transport request failed after safe retry handling."""


class ProviderResponseError(OpenRouterError):
    """A provider response did not satisfy the expected response shape."""


def load_secrets(path: Path = SECRETS) -> dict[str, str]:
    if not path.exists():
        raise FileNotFoundError(
            f"{path} 없음. 로컬 평가용 키 파일을 만들어라 (.private/ 아래, 0600)."
        )
    out: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            out[key.strip()] = value.strip()
    return out


@dataclass
class Usage:
    """Provider usage plus client-side attempt/accounting state.

    ``cost_usd`` is only the sum of provider-reported, verified costs.  It is
    never silently increased by a missing field.  ``reserved_cost_usd`` is the
    maximum reserved for attempts whose billing outcome is not yet reconciled;
    it includes the reservations represented by ``unknown_cost_attempts``.
    """

    attempts: int = 0
    calls: int = 0
    retries: int = 0
    failures: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0
    reserved_cost_usd: float = 0.0
    unknown_cost_attempts: int = 0
    errors: dict[str, int] = field(default_factory=dict)

    def summary(self) -> str:
        return (
            f"attempts={self.attempts} calls={self.calls} retries={self.retries} "
            f"failures={self.failures} tokens={self.prompt_tokens}+{self.completion_tokens} "
            f"cost=${self.cost_usd:.4f} reserved=${self.reserved_cost_usd:.4f} "
            f"unknown_cost_attempts={self.unknown_cost_attempts}"
        )


def _positive_finite(value: Any, *, name: str) -> float:
    """Parse a strictly positive finite number without exposing its source."""

    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise BudgetConfigurationError(f"{name} must be a finite positive number") from exc
    if not math.isfinite(number) or number <= 0:
        raise BudgetConfigurationError(f"{name} must be a finite positive number")
    return number


def _nonnegative_finite(value: Any, *, name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise BudgetReconciliationError(f"{name} must be a finite non-negative number") from exc
    if not math.isfinite(number) or number < 0:
        raise BudgetReconciliationError(f"{name} must be a finite non-negative number")
    return number


class OpenRouter:
    """Minimal OpenRouter client with a conservative per-instance pilot budget.

    Args:
        secrets: Settings mapping.  Supplying a mapping is useful for synthetic
            tests and avoids reading the local secrets file.  It must contain
            ``OPENROUTER_API_KEY`` and should contain
            ``EVAL_MAX_REQUEST_COST_USD`` when no explicit constructor value is
            supplied.
        budget_usd: Exclusive client-side budget for this instance.  A request
            is rejected when known cost or a new reservation reaches this
            limit.  This does not cap provider-key billing.
        max_request_cost_usd: Conservative maximum cost for each physical
            attempt.  Missing configuration blocks paid calls.  The estimate
            must be based on a checked price/token/request-size assumption;
            this class does not verify provider billing or prices.
        urlopen: Optional urllib-compatible transport for synthetic tests.
        sleep: Optional backoff function for synthetic tests.
    """

    def __init__(
        self,
        secrets: dict[str, str] | None = None,
        *,
        budget_usd: float = 2.0,
        max_request_cost_usd: float | None = None,
        urlopen: Callable[..., Any] | None = None,
        sleep: Callable[[float], Any] | None = None,
    ) -> None:
        cfg = secrets if secrets is not None else load_secrets()
        if "OPENROUTER_API_KEY" not in cfg:
            raise BudgetConfigurationError("OPENROUTER_API_KEY 설정이 없다")
        self._key = cfg["OPENROUTER_API_KEY"]
        self.base = cfg.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")
        self.llm_model = cfg.get("EVAL_LLM_MODEL", "z-ai/glm-5.3-flash")
        self.embed_model = cfg.get("EVAL_EMBED_MODEL", "qwen/qwen3-embedding-8b")
        try:
            self.budget_usd = float(budget_usd)
        except (TypeError, ValueError) as exc:
            raise BudgetConfigurationError("budget_usd must be a finite positive number") from exc
        if not math.isfinite(self.budget_usd) or self.budget_usd <= 0:
            raise BudgetConfigurationError("budget_usd must be a finite positive number")

        raw_max = cfg.get("EVAL_MAX_REQUEST_COST_USD")
        if max_request_cost_usd is None and raw_max not in (None, ""):
            max_request_cost_usd = raw_max
        self.max_request_cost_usd: float | None
        if max_request_cost_usd is None:
            # Deliberately defer this error until a paid call is requested so
            # callers can construct/inspect a keyless or policy-disabled client.
            self.max_request_cost_usd = None
        else:
            self.max_request_cost_usd = _positive_finite(
                max_request_cost_usd, name="max_request_cost_usd"
            )

        self.usage = Usage()
        self._urlopen = urlopen if urlopen is not None else urllib.request.urlopen
        self._sleep = sleep if sleep is not None else time.sleep
        self._lock = RLock()
        self._unknown_reserved_usd = 0.0
        self._blocked_reason: str | None = None

    @property
    def budget_state(self) -> str:
        """Return a safe status string without exposing request data."""

        with self._lock:
            if self._blocked_reason:
                return self._blocked_reason
            if self.usage.unknown_cost_attempts:
                return "reconciliation_required"
            if self.usage.cost_usd >= self.budget_usd:
                return "exhausted"
            return "ready"

    @property
    def budget_remaining_usd(self) -> float:
        """Return the conservative unreserved remainder for this instance."""

        with self._lock:
            return max(
                0.0,
                self.budget_usd - self.usage.cost_usd - self.usage.reserved_cost_usd,
            )

    def reconcile_unknown_cost(self, actual_cost_usd: float) -> None:
        """Reconcile all currently unknown attempts with an external amount.

        ``actual_cost_usd`` is the verified aggregate provider charge for all
        unknown attempts, not a new estimate.  Until this method succeeds, no
        new paid operation is allowed.  A value that reaches the budget or
        exceeds the request maximum leaves the instance blocked.
        """

        amount = _nonnegative_finite(actual_cost_usd, name="actual_cost_usd")
        with self._lock:
            if not self.usage.unknown_cost_attempts:
                raise BudgetReconciliationError("reconcile할 미확인 비용이 없다")
            unknown_attempts = self.usage.unknown_cost_attempts
            self.usage.reserved_cost_usd = max(
                0.0, self.usage.reserved_cost_usd - self._unknown_reserved_usd
            )
            self._unknown_reserved_usd = 0.0
            self.usage.unknown_cost_attempts = 0
            self.usage.cost_usd += amount
            if self.max_request_cost_usd is not None and amount > self.max_request_cost_usd * unknown_attempts:
                self._blocked_reason = "request_cost_overrun"
                raise BudgetOverrunError(
                    "reconciled provider cost exceeded configured request maximum"
                )
            if self.usage.cost_usd >= self.budget_usd:
                self._blocked_reason = "exhausted"
                raise BudgetExceededError("reconciled cost reaches the configured budget")

    def _reserve_attempt(self, *, allow_pending_unknown: bool) -> float:
        with self._lock:
            if self._blocked_reason:
                if self._blocked_reason == "exhausted":
                    raise BudgetExceededError("budget is exhausted")
                if self._blocked_reason == "request_cost_overrun":
                    raise BudgetOverrunError("request cost exceeded configured maximum")
                raise BudgetReconciliationError("budget reconciliation is required")
            if self.usage.unknown_cost_attempts and not allow_pending_unknown:
                raise BudgetReconciliationError(
                    "previous request cost is unknown; reconcile it before another request"
                )
            if self.max_request_cost_usd is None:
                raise BudgetConfigurationError(
                    "max_request_cost_usd is required before a paid request"
                )
            # The budget is exclusive.  A reservation that reaches the limit
            # is rejected, leaving a safety margin for provider charge drift.
            projected = (
                self.usage.cost_usd
                + self.usage.reserved_cost_usd
                + self.max_request_cost_usd
            )
            if self.usage.cost_usd >= self.budget_usd or projected >= self.budget_usd:
                self._blocked_reason = "exhausted" if self.usage.cost_usd >= self.budget_usd else None
                raise BudgetExceededError(
                    "request reservation reaches the configured budget"
                )
            self.usage.reserved_cost_usd += self.max_request_cost_usd
            self.usage.attempts += 1
            return self.max_request_cost_usd

    def _mark_unknown_attempt(self, reservation: float) -> None:
        with self._lock:
            self._unknown_reserved_usd += reservation
            self.usage.unknown_cost_attempts += 1

    def _record_response_usage(self, body: Mapping[str, Any], reservation: float) -> None:
        """Record a successful response or raise a fail-closed budget signal."""

        raw_usage = body.get("usage")
        if not isinstance(raw_usage, Mapping) or "cost" not in raw_usage:
            self._mark_unknown_attempt(reservation)
            raise BudgetReconciliationError(
                "provider response omitted a verifiable request cost"
            )
        try:
            actual = float(raw_usage["cost"])
        except (TypeError, ValueError) as exc:
            self._mark_unknown_attempt(reservation)
            raise BudgetReconciliationError(
                "provider response contained an invalid request cost"
            ) from exc
        if not math.isfinite(actual) or actual < 0:
            self._mark_unknown_attempt(reservation)
            raise BudgetReconciliationError(
                "provider response contained an invalid request cost"
            )

        with self._lock:
            self.usage.reserved_cost_usd = max(
                0.0, self.usage.reserved_cost_usd - reservation
            )
            self.usage.calls += 1
            self.usage.cost_usd += actual
            for name in ("prompt_tokens", "completion_tokens"):
                value = raw_usage.get(name, 0)
                if isinstance(value, bool):
                    value = 0
                try:
                    value = int(value or 0)
                except (TypeError, ValueError):
                    value = 0
                if value >= 0:
                    setattr(self.usage, name, getattr(self.usage, name) + value)

            if actual > reservation:
                self._blocked_reason = "request_cost_overrun"
                raise BudgetOverrunError(
                    "provider-reported cost exceeded configured request maximum"
                )
            if self.usage.cost_usd >= self.budget_usd:
                self._blocked_reason = "exhausted"
                raise BudgetExceededError("provider-reported cost reaches the configured budget")
            if self.usage.unknown_cost_attempts:
                raise BudgetReconciliationError(
                    "a previous attempt still has an unknown provider cost"
                )

    def _post(
        self,
        path: str,
        payload: dict,
        *,
        attempts: int = MAX_ATTEMPTS,
        timeout: int = 180,
    ) -> dict:
        """POST with a bounded number of physically budgeted attempts."""

        if (
            isinstance(attempts, bool)
            or not isinstance(attempts, int)
            or not 1 <= attempts <= MAX_ATTEMPTS
        ):
            raise ValueError(f"attempts must be an integer from 1 to {MAX_ATTEMPTS}")
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or timeout <= 0:
            raise ValueError("timeout must be positive")

        # Serialize before reserving: malformed local input did not make a
        # provider attempt and must not strand a budget reservation.
        try:
            request_data = json.dumps(payload).encode()
        except (TypeError, ValueError) as exc:
            raise ProviderRequestError("request payload could not be serialized") from exc

        last = "transport error"
        pending_from_this_operation = False
        for attempt in range(attempts):
            reservation = self._reserve_attempt(
                allow_pending_unknown=pending_from_this_operation
            )
            req = urllib.request.Request(
                self.base + path,
                method="POST",
                data=request_data,
                headers={
                    "Authorization": f"Bearer {self._key}",
                    "Content-Type": "application/json",
                },
            )
            try:
                with self._urlopen(req, timeout=timeout) as resp:
                    body = json.loads(resp.read())
            except BudgetError:
                raise
            except urllib.error.HTTPError as exc:
                code = int(exc.code) if isinstance(exc.code, int) else 0
                last = f"HTTP {code}" if code else "HTTP error"
                self.usage.errors[last] = self.usage.errors.get(last, 0) + 1
                self._mark_unknown_attempt(reservation)
                pending_from_this_operation = True
                retryable = code in RETRYABLE
                if not retryable or attempt == attempts - 1:
                    self.usage.failures += 1
                    raise ProviderRequestError(f"{last}; no further safe retry")
            except Exception as exc:  # network/timeout; type only is safe
                last = type(exc).__name__
                self.usage.errors[last] = self.usage.errors.get(last, 0) + 1
                self._mark_unknown_attempt(reservation)
                pending_from_this_operation = True
                if attempt == attempts - 1:
                    self.usage.failures += 1
                    raise ProviderRequestError(
                        f"{attempts} bounded attempts failed ({last})"
                    )
            else:
                if not isinstance(body, dict):
                    self._mark_unknown_attempt(reservation)
                    self.usage.failures += 1
                    raise ProviderResponseError("provider response was not an object")
                # Some gateways return HTTP 200 with an error object.  Treat it
                # as a retryable provider failure without exposing its body.
                if "error" in body and "choices" not in body and "data" not in body:
                    last = "HTTP 503"
                    self.usage.errors[last] = self.usage.errors.get(last, 0) + 1
                    self._mark_unknown_attempt(reservation)
                    pending_from_this_operation = True
                    if attempt == attempts - 1:
                        self.usage.failures += 1
                        raise ProviderRequestError(
                            f"{attempts} bounded attempts failed ({last})"
                        )
                else:
                    self._record_response_usage(body, reservation)
                    return body

            self.usage.retries += 1
            self._sleep(min(2**attempt, 30) * (0.6 + random.random() * 0.8))

        # The loop returns or raises on every path; keep a safe defensive
        # failure for static analyzers and future changes.
        self.usage.failures += 1
        raise ProviderRequestError(f"{attempts} bounded attempts failed ({last})")

    def _validate_embedding_rows(
        self, body: Mapping[str, Any], expected_count: int
    ) -> list[list[float]]:
        data = body.get("data")
        if not isinstance(data, list):
            raise ProviderResponseError("embedding response data is not a list")
        if len(data) != expected_count:
            raise ProviderResponseError(
                f"embedding response count mismatch: expected {expected_count}, got {len(data)}"
            )

        indexed: dict[int, list[float]] = {}
        dimension: int | None = None
        for row in data:
            if not isinstance(row, Mapping):
                raise ProviderResponseError("embedding response row is not an object")
            index = row.get("index")
            if isinstance(index, bool) or not isinstance(index, int):
                raise ProviderResponseError("embedding response index is invalid")
            if index in indexed:
                raise ProviderResponseError("embedding response contains duplicate indexes")
            vector = row.get("embedding")
            if not isinstance(vector, (list, tuple)) or not vector:
                raise ProviderResponseError("embedding vector is empty or invalid")
            if dimension is None:
                dimension = len(vector)
            if len(vector) != dimension:
                raise ProviderResponseError("embedding dimensions are inconsistent")
            converted: list[float] = []
            squared_norm = 0.0
            for value in vector:
                if isinstance(value, bool) or not isinstance(value, Real):
                    raise ProviderResponseError("embedding contains a non-numeric value")
                number = float(value)
                if not math.isfinite(number):
                    raise ProviderResponseError("embedding contains a non-finite value")
                converted.append(number)
                squared_norm += number * number
            if not math.isfinite(squared_norm):
                raise ProviderResponseError("embedding norm is not finite")
            indexed[index] = converted

        expected_indexes = set(range(expected_count))
        if set(indexed) != expected_indexes:
            raise ProviderResponseError("embedding response indexes are not contiguous")
        if any(not any(vector) for vector in indexed.values()):
            raise ProviderResponseError("embedding contains a zero vector")
        return [indexed[index] for index in range(expected_count)]

    def embed(self, texts: list[str]) -> list[list[float]]:
        if not isinstance(texts, list):
            raise TypeError("texts must be a list")
        if not texts:
            return []
        body = self._post("/embeddings", {"model": self.embed_model, "input": texts})
        return self._validate_embedding_rows(body, len(texts))

    def chat(
        self,
        messages: list[dict],
        *,
        max_tokens: int = 512,
        temperature: float = 0.0,
        reasoning: str | None = "low",
    ) -> str:
        """Request chat completion with optional provider reasoning guidance.

        ``reasoning`` is passed using OpenRouter's provider-specific request
        shape.  The selected model/provider determines whether it is accepted,
        how many tokens it uses, its cost, and its output quality; this client
        makes no universal claim about disabling reasoning or preserving
        quality.
        """

        payload: dict = {
            "model": self.llm_model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
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
        return message["content"]
