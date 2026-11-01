"""OpenRouter /rerank client — model is configurable (config.json models.rerank). Cohere rerank client via OpenRouter-compatible /v1/rerank.

Default model ID is ``RERANK_MODEL`` (``voyageai/rerank-2.5-lite``).
The same OpenRouter ``/v1/rerank`` path accepts Cohere/Voyage/Qwen IDs;
override via constructor or sandbox ``config.json`` ``rerank_model``.
"""

from __future__ import annotations

import json
import math
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable, Mapping, Sequence

from . import RERANK_MODEL

UrlOpen = Callable[..., Any]


class RerankError(RuntimeError):
    """Safe rerank failure (no query/document bodies in message)."""


class RerankBudgetError(RerankError):
    pass


@dataclass(frozen=True)
class _Reservation:
    token: int
    amount: float


class RerankBudgetLedger:
    """Thread-safe cumulative budget ledger shared by rerank clients.

    A caller that creates a client per query can pass the same ledger to every
    client.  Reservation happens before the provider request; every settled
    or failed reservation remains in ``spent_usd`` so an uncertain provider
    outcome is never silently treated as free.

    A reservation that fails only because *other threads* hold in-flight
    reservations waits for them to settle instead of failing, so running
    queries in parallel never changes which requests get budget compared with
    running them one by one (settled spend still decides).
    """

    WAIT_FOR_INFLIGHT_S = 300.0

    def __init__(self, budget_usd: float) -> None:
        budget = _finite_nonnegative(budget_usd, "budget_usd")
        if budget <= 0:
            raise RerankBudgetError("budget and max_request_cost_usd must be > 0")
        self.budget_usd = budget
        self.spent_usd = 0.0
        self.reserved_usd = 0.0
        self.verified_cost_usd = 0.0
        self.estimated_cost_usd = 0.0
        self.unknown_cost_usd = 0.0
        self.unknown_cost_attempts = 0
        self.overrun = False
        self._lock = threading.RLock()
        self._settled = threading.Condition(self._lock)
        self._next_token = 0
        self._active: dict[int, _Reservation] = {}
        self._owner: dict[int, int] = {}

    @property
    def remaining_usd(self) -> float:
        with self._lock:
            return max(0.0, self.budget_usd - self.spent_usd - self.reserved_usd)

    @property
    def budget_state(self) -> str:
        with self._lock:
            if self.overrun:
                return "request_cost_overrun"
            if self.spent_usd >= self.budget_usd:
                return "exhausted"
            return "ready"

    def reserve(self, amount_usd: float) -> _Reservation:
        amount = _finite_nonnegative(amount_usd, "reservation")
        if amount <= 0:
            raise RerankBudgetError("reservation must be > 0")
        me = threading.get_ident()
        with self._lock:
            deadline = None
            while True:
                if self.overrun or self.spent_usd >= self.budget_usd:
                    raise RerankBudgetError("budget exhausted")
                if self.spent_usd + amount > self.budget_usd + 1e-12:
                    raise RerankBudgetError("budget exhausted")
                projected = self.spent_usd + self.reserved_usd + amount
                if projected <= self.budget_usd + 1e-12:
                    break
                # Short only because of other threads' in-flight reservations: wait for them.
                if not any(owner != me for owner in self._owner.values()):
                    raise RerankBudgetError("budget exhausted")
                if deadline is None:
                    deadline = time.monotonic() + self.WAIT_FOR_INFLIGHT_S
                left = deadline - time.monotonic()
                if left <= 0:
                    raise RerankBudgetError("budget exhausted")
                self._settled.wait(timeout=left)
            self._next_token += 1
            reservation = _Reservation(self._next_token, amount)
            self._active[reservation.token] = reservation
            self._owner[reservation.token] = me
            self.reserved_usd += amount
            return reservation

    def settle(
        self,
        reservation: _Reservation,
        cost_usd: float,
        *,
        basis: str = "provider",
        unknown: bool = False,
    ) -> bool:
        """Settle one reservation and return whether it exceeded its cap."""

        cost = _finite_nonnegative(cost_usd, "cost_usd")
        with self._lock:
            active = self._active.pop(reservation.token, None)
            self._owner.pop(reservation.token, None)
            self._settled.notify_all()
            if active is None or active.amount != reservation.amount:
                raise RerankBudgetError("reservation already settled")
            self.reserved_usd = max(0.0, self.reserved_usd - reservation.amount)
            self.spent_usd += cost
            if unknown:
                self.unknown_cost_attempts += 1
                self.unknown_cost_usd += cost
                self.estimated_cost_usd += cost
            elif basis == "provider" or basis == "generation":
                self.verified_cost_usd += cost
            else:
                self.estimated_cost_usd += cost
            overrun = cost > reservation.amount + 1e-12
            if overrun:
                self.overrun = True
            return overrun


# Backward/forward-friendly short name for callers that do not need the
# provider-specific class name.
BudgetLedger = RerankBudgetLedger


def _finite_nonnegative(value: Any, name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise RerankBudgetError(f"{name} must be finite and non-negative") from exc
    if not math.isfinite(number) or number < 0:
        raise RerankBudgetError(f"{name} must be finite and non-negative")
    return number


def _optional_cost(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number) or number < 0:
        return None
    return number


@dataclass
class RerankResult:
    indices: list[int]
    scores: list[float]
    model: str
    latency_ms: float
    cost_usd: float
    attempts: int
    generation_id: str | None = None
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    total_tokens: int | None = None
    billed: dict[str, Any] | None = None


class RerankClient:
    """Rerank adapter. Inject ``urlopen`` for tests. Never logs document text."""

    def __init__(
        self,
        secrets: Mapping[str, str],
        *,
        model: str = RERANK_MODEL,
        budget_usd: float,
        max_request_cost_usd: float,
        base_url: str = "https://openrouter.ai/api/v1",
        urlopen: UrlOpen | None = None,
        sleep: Callable[[float], None] | None = None,
        ledger: RerankBudgetLedger | None = None,
        budget_ledger: RerankBudgetLedger | None = None,
    ) -> None:
        key = (secrets.get("OPENROUTER_API_KEY") or "").strip()
        if not key:
            raise RerankBudgetError("OPENROUTER_API_KEY missing")
        # Never accept Discord bot tokens as the OpenRouter credential.
        discord_token = (secrets.get("DISCORD_TOKEN") or secrets.get("DISCORD_BOT_TOKEN") or "").strip()
        if discord_token and key == discord_token:
            raise RerankBudgetError("Discord token must not be used as OpenRouter key")
        self._key = key
        self.model = model
        self.budget_usd = _finite_nonnegative(budget_usd, "budget_usd")
        self.max_request_cost_usd = _finite_nonnegative(
            max_request_cost_usd, "max_request_cost_usd"
        )
        if self.budget_usd <= 0 or self.max_request_cost_usd <= 0:
            raise RerankBudgetError("budget and max_request_cost_usd must be > 0")
        if ledger is not None and budget_ledger is not None and ledger is not budget_ledger:
            raise RerankBudgetError("ledger and budget_ledger must refer to the same ledger")
        self.ledger = ledger or budget_ledger or RerankBudgetLedger(self.budget_usd)
        self.base_url = base_url.rstrip("/")
        self._urlopen = urlopen or urllib.request.urlopen
        self._sleep = sleep or time.sleep
        self._local_lock = threading.RLock()
        self._local_spent_usd = 0.0
        self._local_reserved_usd = 0.0
        self._local_blocked = False

    @property
    def spent_usd(self) -> float:
        """Cumulative settled spend in the shared ledger."""

        return float(self.ledger.spent_usd)

    @property
    def unknown_cost_attempts(self) -> int:
        return int(self.ledger.unknown_cost_attempts)

    @property
    def unknown_cost_usd(self) -> float:
        return float(self.ledger.unknown_cost_usd)

    @property
    def estimated_cost_usd(self) -> float:
        return float(self.ledger.estimated_cost_usd)

    @property
    def reserved_usd(self) -> float:
        return float(self.ledger.reserved_usd)

    @property
    def budget_remaining_usd(self) -> float:
        return float(self.ledger.remaining_usd)

    def _reserve(self) -> _Reservation:
        amount = self.max_request_cost_usd
        with self._local_lock:
            if self._local_blocked:
                raise RerankBudgetError("budget exhausted")
            projected = self._local_spent_usd + self._local_reserved_usd + amount
            if projected > self.budget_usd + 1e-12:
                raise RerankBudgetError("budget exhausted")
            reservation = self.ledger.reserve(amount)
            self._local_reserved_usd += amount
            return reservation

    def _settle(
        self,
        reservation: _Reservation,
        cost_usd: float,
        *,
        basis: str,
        unknown: bool = False,
    ) -> bool:
        overrun = self.ledger.settle(
            reservation,
            cost_usd,
            basis=basis,
            unknown=unknown,
        )
        with self._local_lock:
            self._local_reserved_usd = max(
                0.0, self._local_reserved_usd - reservation.amount
            )
            self._local_spent_usd += float(cost_usd)
            if overrun:
                self._local_blocked = True
        return overrun

    def _settle_unknown(self, reservation: _Reservation) -> None:
        self._settle(
            reservation,
            reservation.amount,
            basis="reservation",
            unknown=True,
        )

    def rerank(
        self,
        query: str,
        documents: Sequence[str],
        *,
        top_n: int | None = None,
    ) -> RerankResult:
        if not query.strip():
            raise RerankError("empty query")
        if not documents:
            raise RerankError("empty documents")
        if any(not isinstance(d, str) for d in documents):
            raise RerankError("documents must be strings")
        n = len(documents)
        top = n if top_n is None else int(top_n)
        if top < 1 or top > n:
            raise RerankError("top_n out of range")

        payload = {
            "model": self.model,
            "query": query,
            "documents": list(documents),
            "top_n": top,
        }
        try:
            body = json.dumps(payload).encode("utf-8")
        except (TypeError, ValueError) as exc:
            # Input validation above should make this unreachable for normal
            # callers.  Keep it before reservation so no provider attempt is
            # charged for a local serialization failure.
            raise RerankError("request payload could not be serialized") from exc
        req = urllib.request.Request(
            f"{self.base_url}/rerank",
            data=body,
            method="POST",
            headers={
                "Authorization": f"Bearer {self._key}",
                "Content-Type": "application/json",
                "HTTP-Referer": "https://github.com/kim0040/masamong2",
                "X-Title": "masamong2-rag-upgrade-eval",
            },
        )
        # Build all local request state before reserving.  A malformed local
        # URL/request therefore cannot strand a paid reservation.
        reservation = self._reserve()
        started = time.perf_counter()
        try:
            with self._urlopen(req, timeout=60) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            self._settle_unknown(reservation)
            raise RerankError(f"http {exc.code}") from None
        except Exception as exc:  # noqa: BLE001 — sanitize
            self._settle_unknown(reservation)
            raise RerankError(f"transport failure: {type(exc).__name__}") from None
        latency_ms = (time.perf_counter() - started) * 1000.0
        try:
            data = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            self._settle_unknown(reservation)
            raise RerankError("invalid json") from exc

        if not isinstance(data, Mapping):
            self._settle_unknown(reservation)
            raise RerankError("invalid response")

        usage = data.get("usage") if isinstance(data.get("usage"), Mapping) else {}
        gen_id = _generation_id(data)
        try:
            results = data.get("results")
            if not isinstance(results, list) or not results:
                raise RerankError("missing results")
            indices: list[int] = []
            scores: list[float] = []
            for row in results:
                if not isinstance(row, Mapping):
                    raise RerankError("bad result row")
                idx = int(row["index"])
                score = float(row.get("relevance_score", row.get("score")))
                if idx < 0 or idx >= n:
                    raise RerankError("index out of range")
                indices.append(idx)
                scores.append(score)
        except (KeyError, TypeError, ValueError, RerankError) as exc:
            # A parsed but unusable provider response may still have incurred
            # a charge.  Use a reported/fetched cost when one is available;
            # otherwise consume the full reservation conservatively.
            cost_f, cost_basis, cost_unknown = self._resolve_cost(
                reservation, usage, gen_id
            )
            if cost_unknown:
                self._settle_unknown(reservation)
                raise RerankError(str(exc)) from None
            if self._settle(reservation, cost_f, basis=cost_basis):
                raise RerankBudgetError("cost overrun") from None
            raise RerankError(str(exc)) from None

        cost_f, cost_basis, cost_unknown = self._resolve_cost(
            reservation, usage, gen_id
        )
        if self._settle(
            reservation,
            cost_f,
            basis=cost_basis,
            unknown=cost_unknown,
        ):
            raise RerankBudgetError("cost overrun")

        prompt_tokens = _opt_int(usage.get("prompt_tokens"))
        completion_tokens = _opt_int(usage.get("completion_tokens"))
        total_tokens = _opt_int(usage.get("total_tokens"))
        billed = {
            "cost": cost_f,
            "cost_basis": cost_basis,
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens": total_tokens,
            "generation_id": gen_id,
        }
        return RerankResult(
            indices=indices,
            scores=scores,
            model=str(data.get("model") or self.model),
            latency_ms=latency_ms,
            cost_usd=cost_f,
            attempts=1,
            generation_id=gen_id,
            prompt_tokens=prompt_tokens,
            completion_tokens=completion_tokens,
            total_tokens=total_tokens,
            billed=billed,
        )

    def _resolve_cost(
        self,
        reservation: _Reservation,
        usage: Mapping[str, Any],
        generation_id: str | None,
    ) -> tuple[float, str, bool]:
        cost = _optional_cost(usage.get("cost"))
        if cost is not None:
            return cost, "provider", False
        if generation_id:
            fetched = self.fetch_generation(generation_id, retries=4)
            generation_cost = _generation_cost(fetched)
            if generation_cost is not None:
                return generation_cost, "generation", False
        # Preserve the ranking but account for the entire request cap.  This
        # is both the cost estimate and the unresolved/unknown spend.
        return reservation.amount, "reservation", True

    def fetch_generation(self, generation_id: str, *, retries: int = 4) -> dict[str, Any] | None:
        """OpenRouter GET /generation?id= — native tokens + total_cost after settle."""

        gid = (generation_id or "").strip()
        if not gid:
            return None
        req = urllib.request.Request(
            f"{self.base_url}/generation?id={urllib.parse.quote(gid)}",
            method="GET",
            headers={"Authorization": f"Bearer {self._key}"},
        )
        last: dict[str, Any] | None = None
        for attempt in range(max(1, retries)):
            try:
                with self._urlopen(req, timeout=30) as resp:
                    raw = resp.read()
                payload = json.loads(raw.decode("utf-8"))
            except Exception:  # noqa: BLE001 — sanitize
                self._sleep(0.35 * (attempt + 1))
                continue
            data = payload.get("data") if isinstance(payload, Mapping) else None
            if not isinstance(data, Mapping):
                data = payload if isinstance(payload, Mapping) else None
            if not isinstance(data, Mapping):
                self._sleep(0.35 * (attempt + 1))
                continue
            last = {
                "id": data.get("id"),
                "model": data.get("model"),
                "total_cost": data.get("total_cost", data.get("usage")),
                "tokens_prompt": data.get("tokens_prompt") or data.get("native_tokens_prompt"),
                "tokens_completion": data.get("tokens_completion")
                or data.get("native_tokens_completion"),
                "native_tokens_prompt": data.get("native_tokens_prompt"),
                "latency": data.get("latency") or data.get("generation_time"),
                "provider_name": data.get("provider_name"),
            }
            if last.get("total_cost") is not None:
                return last
            self._sleep(0.35 * (attempt + 1))
        return last


def _generation_id(data: Mapping[str, Any]) -> str | None:
    value = data.get("id")
    if isinstance(value, str) and value.strip():
        return value
    return None


def _generation_cost(data: Mapping[str, Any] | None) -> float | None:
    if not isinstance(data, Mapping):
        return None
    for key in ("total_cost", "cost"):
        value = data.get(key)
        if isinstance(value, Mapping):
            nested = _generation_cost(value)
            if nested is not None:
                return nested
        else:
            cost = _optional_cost(value)
            if cost is not None:
                return cost
    return None


def _opt_int(value: Any) -> int | None:
    try:
        if value is None:
            return None
        return int(value)
    except (TypeError, ValueError):
        return None


def apply_rerank_order(
    candidate_ids: Sequence[str], result: RerankResult
) -> list[str]:
    ordered: list[str] = []
    for idx in result.indices:
        ordered.append(candidate_ids[idx])
    # append any missing (shouldn't happen if top_n == len)
    for cid in candidate_ids:
        if cid not in ordered:
            ordered.append(cid)
    return ordered


# Backward-compatible name (the client was first written for the Cohere candidate).
CohereRerankClient = RerankClient
