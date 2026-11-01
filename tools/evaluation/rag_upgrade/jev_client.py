"""Minimal TypeSafe Jev (System One) ``/v1/systemone`` evaluation client.

Design rules:

* urllib only, injectable ``urlopen``/``sleep`` so tests never touch the network.
* Every physical HTTP attempt counts against an explicit request cap. The cap is
  enforced *before* the attempt; reaching it raises :class:`JevBudgetError` and
  the caller is expected to stop the run and persist usage.
* 429 follows ``retry-after`` (bounded); 5xx retries with backoff; other 4xx fail
  immediately. JSON that cannot be parsed raises :class:`JevDecodeError`.
* ``usage`` accumulates input/output tokens; a response without ``usage`` is
  counted as *unknown* rather than silently zero.
* Secrets: only ``TYPESAFE_API_KEY``, ``TYPESAFE_BASE_URL``, ``JEV_MODEL`` are read
  from the secrets file. No other key is loaded. Nothing in an error message or
  in ``usage`` contains the key, the URL, the state, or the response body.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Mapping

UrlOpen = Callable[..., Any]

SECRET_KEYS = ("TYPESAFE_API_KEY", "TYPESAFE_BASE_URL", "JEV_MODEL")
FORBIDDEN_MODEL_ALIASES = ("jev-latest", "latest")
HARD_REQUEST_CAP = 2000  # total across one evaluation run, never raise
DEFAULT_MAX_REQUESTS = 500
RETRYABLE_5XX = {500, 502, 503, 504, 520, 522, 524}
MAX_RETRY_AFTER_S = 30.0
MAX_ATTEMPTS = 4


class JevError(RuntimeError):
    """Base class: message is safe (no secrets, no state, no response body)."""


class JevConfigError(JevError):
    pass


class JevBudgetError(JevError):
    """The request cap would be exceeded; no attempt was made."""


class JevHTTPError(JevError):
    def __init__(self, message: str, *, status: int | None, attempts: int) -> None:
        super().__init__(message)
        self.status = status
        self.attempts = attempts


class JevRateLimitError(JevHTTPError):
    pass


class JevDecodeError(JevError):
    """Response was not the expected JSON shape."""


def read_jev_secrets(path: str | Path) -> dict[str, str]:
    """Read only the three Jev keys from a ``KEY=value`` env file.

    Other lines are ignored and never returned. Raises :class:`JevConfigError` if
    the file is missing or any of the three keys is empty. Rejects ``jev-latest``.
    """
    p = Path(path)
    if not p.is_file():
        raise JevConfigError("secrets file not found (expected TYPESAFE_API_KEY, TYPESAFE_BASE_URL, JEV_MODEL)")
    out: dict[str, str] = {}
    with p.open("r", encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            if line.startswith("export "):
                line = line[len("export "):].strip()
            key, _, value = line.partition("=")
            key = key.strip()
            if key not in SECRET_KEYS:
                continue
            value = value.strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
                value = value[1:-1]
            out[key] = value
    missing = [k for k in SECRET_KEYS if not out.get(k)]
    if missing:
        raise JevConfigError("missing secrets: " + ", ".join(missing))
    validate_model_id(out["JEV_MODEL"])
    out["TYPESAFE_BASE_URL"] = out["TYPESAFE_BASE_URL"].rstrip("/")
    return out


def validate_model_id(model: str) -> None:
    m = (model or "").strip().lower()
    if not m:
        raise JevConfigError("JEV_MODEL is empty")
    if m in FORBIDDEN_MODEL_ALIASES or m.endswith("-latest"):
        raise JevConfigError("JEV_MODEL must be a pinned version, not a 'latest' alias")


@dataclass
class JevResponse:
    answers: dict[str, dict[str, Any]]
    model: str | None
    input_tokens: int | None
    output_tokens: int | None
    latency_ms: float
    attempts: int
    rate_limited: int

    @property
    def usage_known(self) -> bool:
        return self.input_tokens is not None


@dataclass
class JevUsage:
    """Accumulated over the client lifetime. Survives failures (cost already spent)."""

    requests: int = 0  # logical decide() calls that produced at least one attempt
    attempts: int = 0  # physical HTTP attempts (this is what the cap counts)
    successes: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    usage_unknown: int = 0  # successful responses without a usage block
    http_429: int = 0
    http_5xx: int = 0
    http_other: int = 0
    transport_errors: int = 0
    decode_errors: int = 0
    latencies_ms: list[float] = field(default_factory=list)

    def as_dict(self) -> dict[str, Any]:
        lat = sorted(self.latencies_ms)
        return {
            "requests": self.requests,
            "attempts": self.attempts,
            "successes": self.successes,
            "input_tokens": self.input_tokens,
            "output_tokens": self.output_tokens,
            "usage_unknown": self.usage_unknown,
            "http_429": self.http_429,
            "http_5xx": self.http_5xx,
            "http_other": self.http_other,
            "transport_errors": self.transport_errors,
            "decode_errors": self.decode_errors,
            "latency_p50_ms": percentile(lat, 0.50),
            "latency_p95_ms": percentile(lat, 0.95),
            "latency_n": len(lat),
        }


def percentile(sorted_values: list[float], q: float) -> float | None:
    if not sorted_values:
        return None
    if len(sorted_values) == 1:
        return round(sorted_values[0], 1)
    pos = q * (len(sorted_values) - 1)
    lo = int(pos)
    hi = min(lo + 1, len(sorted_values) - 1)
    frac = pos - lo
    return round(sorted_values[lo] * (1 - frac) + sorted_values[hi] * frac, 1)


class JevClient:
    """One instance = one budgeted run. Inject ``urlopen``/``sleep`` in tests."""

    def __init__(
        self,
        secrets: Mapping[str, str],
        *,
        max_requests: int = DEFAULT_MAX_REQUESTS,
        urlopen: UrlOpen | None = None,
        sleep: Callable[[float], None] | None = None,
        clock: Callable[[], float] | None = None,
        timeout_s: float = 30.0,
        max_attempts: int = MAX_ATTEMPTS,
    ) -> None:
        key = (secrets.get("TYPESAFE_API_KEY") or "").strip()
        base = (secrets.get("TYPESAFE_BASE_URL") or "").strip().rstrip("/")
        model = (secrets.get("JEV_MODEL") or "").strip()
        if not key or not base or not model:
            raise JevConfigError("TYPESAFE_API_KEY / TYPESAFE_BASE_URL / JEV_MODEL are required")
        validate_model_id(model)
        if not isinstance(max_requests, int) or max_requests <= 0:
            raise JevConfigError("max_requests must be a positive integer")
        if max_requests > HARD_REQUEST_CAP:
            raise JevConfigError(f"max_requests exceeds the pilot hard cap ({HARD_REQUEST_CAP})")
        self._key = key
        self._url = base + "/v1/systemone"
        self.model = model
        self.max_requests = max_requests
        self._urlopen = urlopen or urllib.request.urlopen
        self._sleep = sleep or time.sleep
        self._clock = clock or time.monotonic
        self._timeout = timeout_s
        self._max_attempts = max(1, int(max_attempts))
        self.usage = JevUsage()
        self.budget_exhausted = False

    # ------------------------------------------------------------------ public
    @property
    def remaining(self) -> int:
        return max(0, self.max_requests - self.usage.attempts)

    def decide(self, state: Any, questions: Mapping[str, Mapping[str, Any]]) -> JevResponse:
        if not questions:
            raise JevConfigError("questions must not be empty")
        body = json.dumps(
            {"model": self.model, "state": state, "questions": dict(questions)},
            ensure_ascii=False,
        ).encode("utf-8")
        attempts = 0
        rate_limited = 0
        counted_request = False
        last_status: int | None = None
        while True:
            if self.usage.attempts >= self.max_requests:
                self.budget_exhausted = True
                raise JevBudgetError(
                    f"request cap reached ({self.max_requests}); remaining cases were not executed"
                )
            if not counted_request:
                self.usage.requests += 1
                counted_request = True
            self.usage.attempts += 1
            attempts += 1
            t0 = self._clock()
            try:
                status, headers, raw = self._post(body)
            except urllib.error.HTTPError as exc:  # HTTP status errors
                status = int(getattr(exc, "code", 0) or 0)
                last_status = status
                retry_after = _retry_after_seconds(getattr(exc, "headers", None))
                _drain(exc)
                if status == 429:
                    self.usage.http_429 += 1
                    rate_limited += 1
                    if attempts < self._max_attempts:
                        self._sleep(retry_after if retry_after is not None else min(2.0 * attempts, MAX_RETRY_AFTER_S))
                        continue
                    raise JevRateLimitError("rate limited (429) after retries", status=429, attempts=attempts)
                if status in RETRYABLE_5XX:
                    self.usage.http_5xx += 1
                    if attempts < self._max_attempts:
                        self._sleep(min(1.0 * (2 ** (attempts - 1)), MAX_RETRY_AFTER_S))
                        continue
                    raise JevHTTPError(f"server error ({status}) after {attempts} attempts", status=status, attempts=attempts)
                self.usage.http_other += 1
                raise JevHTTPError(f"http error ({status})", status=status, attempts=attempts)
            except (urllib.error.URLError, TimeoutError, OSError):
                self.usage.transport_errors += 1
                if attempts < self._max_attempts:
                    self._sleep(min(1.0 * (2 ** (attempts - 1)), MAX_RETRY_AFTER_S))
                    continue
                raise JevHTTPError("transport error after retries", status=None, attempts=attempts)
            latency_ms = (self._clock() - t0) * 1000.0
            last_status = status
            try:
                parsed = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, ValueError):
                self.usage.decode_errors += 1
                raise JevDecodeError("response body is not valid JSON")
            try:
                resp = self._parse(parsed, latency_ms=latency_ms, attempts=attempts, rate_limited=rate_limited)
            except JevDecodeError:
                self.usage.decode_errors += 1
                raise
            self.usage.successes += 1
            self.usage.latencies_ms.append(latency_ms)
            if resp.usage_known:
                self.usage.input_tokens += int(resp.input_tokens or 0)
                self.usage.output_tokens += int(resp.output_tokens or 0)
            else:
                self.usage.usage_unknown += 1
            return resp

    # ----------------------------------------------------------------- private
    def _post(self, body: bytes) -> tuple[int, Any, bytes]:
        req = urllib.request.Request(
            self._url,
            data=body,
            method="POST",
            headers={
                "Authorization": f"Bearer {self._key}",
                "Content-Type": "application/json",
                "Accept": "application/json",
            },
        )
        with self._urlopen(req, timeout=self._timeout) as resp:
            status = int(getattr(resp, "status", None) or getattr(resp, "code", 200) or 200)
            raw = resp.read()
            return status, getattr(resp, "headers", None), raw

    @staticmethod
    def _parse(parsed: Any, *, latency_ms: float, attempts: int, rate_limited: int) -> JevResponse:
        if not isinstance(parsed, dict):
            raise JevDecodeError("response JSON is not an object")
        answers = parsed.get("answers")
        if not isinstance(answers, dict) or not answers:
            raise JevDecodeError("response has no answers object")
        clean: dict[str, dict[str, Any]] = {}
        for name, ans in answers.items():
            if not isinstance(ans, dict) or "type" not in ans:
                raise JevDecodeError("answer entry is malformed")
            kind = ans.get("type")
            if kind == "noul":
                if not _is_num(ans.get("noul")):
                    raise JevDecodeError("noul answer missing numeric 'noul'")
            elif kind == "choice":
                if not isinstance(ans.get("choice"), str):
                    raise JevDecodeError("choice answer missing 'choice'")
            elif kind == "score":
                if not _is_num(ans.get("score")):
                    raise JevDecodeError("score answer missing numeric 'score'")
            else:
                raise JevDecodeError("answer has unknown type")
            clean[str(name)] = {k: v for k, v in ans.items()}
        usage = parsed.get("usage")
        in_tok = out_tok = None
        if isinstance(usage, dict) and _is_num(usage.get("input_tokens")):
            in_tok = int(usage.get("input_tokens"))
            out_tok = int(usage.get("output_tokens") or 0) if _is_num(usage.get("output_tokens")) else 0
        model = parsed.get("model") if isinstance(parsed.get("model"), str) else None
        return JevResponse(
            answers=clean,
            model=model,
            input_tokens=in_tok,
            output_tokens=out_tok,
            latency_ms=latency_ms,
            attempts=attempts,
            rate_limited=rate_limited,
        )


def _is_num(v: Any) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _retry_after_seconds(headers: Any) -> float | None:
    if headers is None:
        return None
    try:
        val = headers.get("Retry-After") or headers.get("retry-after")
    except Exception:  # pragma: no cover - defensive against odd header objects
        return None
    if val is None:
        return None
    try:
        secs = float(str(val).strip())
    except ValueError:
        return None
    return max(0.0, min(secs, MAX_RETRY_AFTER_S))


def _drain(exc: Any) -> None:
    try:
        exc.read()
    except Exception:
        pass
