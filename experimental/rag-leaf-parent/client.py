"""체감 테스트용 OpenRouter 클라이언트 (병렬 호출 지원)."""

from __future__ import annotations

import json
import math
import os
import random
import socket
import time
import urllib.error
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from threading import Condition, Lock, get_ident
from typing import Any, Callable, TypeVar

def _repo_root() -> Path:
    here = Path(__file__).resolve().parent
    for cand in [here, *here.parents]:
        if (cand / "docs").is_dir() and (cand / ".gitignore").is_file():
            return cand
    return here.parents[1]


ROOT = _repo_root()
SECRETS = Path(os.environ.get("EVAL_SECRETS", ROOT / ".private" / "eval" / "secrets.env"))
RETRYABLE = {408, 409, 429, 500, 502, 503, 504, 520, 524}
MAX_ATTEMPTS = 6
# 다른 스레드의 진행 중 예약이 정산되기를 기다리는 최대 시간(초). 넘으면 BudgetError.
RESERVE_WAIT_S = 300.0

T = TypeVar("T")
R = TypeVar("R")


class ClientError(RuntimeError):
    pass


class BudgetError(ClientError):
    """전송 전 예산 차단. 물리 호출이 발생하지 않았고 비용도 없다."""


class StreamError(ClientError):
    """스트림이 성공 종료(`done`) 없이 끝났다.

    status: provider_error | truncated | empty | malformed | transport
    partial_chars: 예외 전까지 소비자에게 흘린 delta 문자 수(본문은 담지 않는다)
    attempt_id: 해당 물리 호출의 usage.events attempt_id
    """

    def __init__(self, message: str, *, status: str, partial_chars: int = 0, attempt_id: str = "") -> None:
        super().__init__(message)
        self.status = status
        self.partial_chars = int(partial_chars)
        self.attempt_id = attempt_id


DEFAULT_EMBED_QUERY_INSTRUCTION = "주어진 한국어 채팅 질문에 답하는 대화 조각을 찾아라"


def _uses_qwen3_embed_instruction(model: str) -> bool:
    name = (model or "").lower()
    return "qwen3-embedding" in name or "qwen/qwen3-embedding" in name


def format_embed_query(text: str, *, instruction: str | None = None) -> str:
    inst = (instruction or DEFAULT_EMBED_QUERY_INSTRUCTION).strip() or DEFAULT_EMBED_QUERY_INSTRUCTION
    body = (text or "").strip()
    return f"Instruct: {inst}\nQuery: {body}"


def _embed_inputs(
    texts: list[str],
    *,
    model: str,
    kind: str,
    instruction: str | None,
) -> list[str]:
    if kind != "query" or not _uses_qwen3_embed_instruction(model):
        return list(texts)
    return [format_embed_query(t, instruction=instruction) for t in texts]


@dataclass
class Usage:
    """클라이언트 인스턴스 하나의 비용 장부.

    cost_usd = verified + estimated + unknown. 장부에 반영할 보수적 총액이며 실패 후에도 남는다.
    - verified_cost_usd: 공급자가 usage.cost 로 보고한 금액
    - estimated_cost_usd: 응답은 받았으나 공급자 비용이 없어 호출 종류별 추정치를 쓴 금액
    - unknown_cost_usd: 전송 후 과금 여부를 확인 못 한 시도에 예약액을 그대로 남긴 금액
    - reserved_cost_usd: 아직 정산되지 않은 진행 중 예약(정산·실패·취소 시 0으로 돌아간다)
    events[i] 는 물리 시도 1개당 1개이며 seq(인스턴스 내 단조 증가)와 attempt_id(전역 고유)를 가진다.
    """

    cost_usd: float = 0.0
    calls: int = 0
    errors: dict[str, int] = field(default_factory=dict)
    events: list[dict[str, Any]] = field(default_factory=list)
    attempts: int = 0
    verified_cost_usd: float = 0.0
    estimated_cost_usd: float = 0.0
    unknown_cost_usd: float = 0.0
    unknown_cost_attempts: int = 0
    reserved_cost_usd: float = 0.0
    failed_attempts: int = 0

    def summary(self) -> str:
        return (
            f"calls={self.calls} cost=${self.cost_usd:.6f} errors={dict(self.errors)} "
            f"attempts={self.attempts} verified=${self.verified_cost_usd:.6f} "
            f"estimated=${self.estimated_cost_usd:.6f} unknown=${self.unknown_cost_usd:.6f}"
            f"/{self.unknown_cost_attempts}"
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "cost_usd": self.cost_usd,
            "calls": self.calls,
            "errors": dict(self.errors),
            "events": list(self.events[-24:]),
            "attempts": self.attempts,
            "verified_cost_usd": self.verified_cost_usd,
            "estimated_cost_usd": self.estimated_cost_usd,
            "unknown_cost_usd": self.unknown_cost_usd,
            "unknown_cost_attempts": self.unknown_cost_attempts,
            "reserved_cost_usd": self.reserved_cost_usd,
            "failed_attempts": self.failed_attempts,
            "cursor": len(self.events),
            "cost_settled": self.reserved_cost_usd <= 0.0,
        }

    def delta_since(self, cursor: int) -> dict[str, Any]:
        """영속 장부 반영용. cursor(이전 as_dict()["cursor"]) 이후 정산된 이벤트와 비용 합계.

        각 이벤트는 정확히 한 번 반환되므로 호출자는 attempt_id 로 중복 반영을 막고,
        반환된 cursor 를 다음 호출에 넘긴다. 진행 중 예약(reserved)은 포함하지 않는다.
        """

        start = max(0, int(cursor))
        events = list(self.events[start:])
        return {
            "cursor": start + len(events),
            "cost_usd": float(sum(float(e.get("cost", 0.0)) for e in events)),
            "unknown_cost_usd": float(
                sum(float(e.get("cost", 0.0)) for e in events if e.get("cost_basis") == "reservation")
            ),
            "attempt_ids": [str(e.get("attempt_id", "")) for e in events],
            "events": events,
        }


@dataclass
class _Attempt:
    attempt_id: str
    seq: int
    kind: str
    model: str | None
    reservation: float
    estimate: float
    started: float


def _parse_usage_cost(raw: Any) -> float | None:
    if not isinstance(raw, dict) or "cost" not in raw:
        return None
    try:
        reported = float(raw["cost"])
    except (TypeError, ValueError):
        return None
    if not math.isfinite(reported) or reported < 0:
        return None
    return reported


def _billing_unknown(exc: BaseException) -> bool:
    """전송 후 예외가 과금 가능성을 남기는지. 연결 자체가 안 된 경우만 과금 없음으로 본다."""

    reason = getattr(exc, "reason", exc)
    if isinstance(reason, (ConnectionRefusedError, socket.gaierror)):
        return False
    return True


def load_secrets(path: Path = SECRETS) -> dict[str, str]:
    if not path.exists():
        raise ClientError(f"시크릿 파일 없음: {path}")
    out: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        out[key.strip()] = value.strip().strip('"').strip("'")
    if "OPENROUTER_API_KEY" not in out:
        raise ClientError("OPENROUTER_API_KEY 가 없다")
    return out


def map_parallel(
    items: list[T],
    fn: Callable[[T], R],
    *,
    workers: int,
) -> list[R]:
    """순서 보존 병렬 map."""

    if not items:
        return []
    workers = max(1, min(int(workers), len(items)))
    if workers == 1 or len(items) == 1:
        return [fn(item) for item in items]
    indexed: list[tuple[int, R]] = []
    with ThreadPoolExecutor(max_workers=workers) as pool:
        futures = {pool.submit(fn, item): i for i, item in enumerate(items)}
        for fut in as_completed(futures):
            indexed.append((futures[fut], fut.result()))
    indexed.sort(key=lambda pair: pair[0])
    return [value for _, value in indexed]


class PlayClient:
    def __init__(
        self,
        *,
        budget_usd: float,
        secrets: dict[str, str] | None = None,
        max_workers: int = 8,
        llm_model: str | None = None,
        embed_model: str | None = None,
        llm_provider: str | None = None,
        max_request_cost_usd: float | None = None,
        urlopen: Callable[..., Any] | None = None,
        sleep: Callable[[float], None] | None = None,
    ) -> None:
        """모델은 인자 > secrets.env(EVAL_*_MODEL) > 기본값. 샌드박스는 config.json `models`를 인자로 넘긴다.

        max_request_cost_usd: 물리 호출 1회의 예약 상한. None이면 호출 종류별 추정치(fallback)만 예약한다.
        urlopen/sleep: 테스트용 주입 지점. 기본은 urllib.request.urlopen / time.sleep.
        """

        cfg = secrets or load_secrets()
        self._key = cfg["OPENROUTER_API_KEY"]
        self.base = cfg.get("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")
        self.llm_model = str(llm_model or cfg.get("EVAL_LLM_MODEL") or "z-ai/glm-5.3-flash")
        provider = cfg.get("EVAL_LLM_PROVIDER", "DeepInfra") if llm_provider is None else llm_provider
        self.llm_provider = str(provider or "").strip()
        self.embed_model = str(embed_model or cfg.get("EVAL_EMBED_MODEL") or "qwen/qwen3-embedding-8b")
        self.budget_usd = float(budget_usd)
        self.max_workers = max(1, int(max_workers))
        self.max_request_cost_usd: float | None = None
        if max_request_cost_usd is not None:
            cap = float(max_request_cost_usd)
            if not math.isfinite(cap) or cap <= 0:
                raise ClientError("max_request_cost_usd 는 양수여야 한다")
            self.max_request_cost_usd = cap
        self.client_id = uuid.uuid4().hex[:12]
        self.usage = Usage()
        self._lock = Lock()
        self._settled = Condition(self._lock)
        self._owner: dict[int, int] = {}
        self._seq = 0
        self._urlopen = urlopen if urlopen is not None else urllib.request.urlopen
        self._sleep = sleep if sleep is not None else time.sleep

    # ---- 비용 회계: reserve → (settle | fail | unknown) 는 물리 시도 1개당 정확히 한 번 ----

    def _reserve(self, *, kind: str, model: str | None, estimate: float) -> _Attempt:
        """전송 전 원자적 예산 검사와 예약. 실패하면 BudgetError(전송·비용 없음)."""

        estimate = max(0.0, float(estimate))
        reservation = estimate if self.max_request_cost_usd is None else max(estimate, self.max_request_cost_usd)
        me = get_ident()
        with self._lock:
            deadline = None
            while True:
                if self.usage.cost_usd >= self.budget_usd or self.usage.cost_usd + reservation > self.budget_usd + 1e-12:
                    raise BudgetError("턴 예산이 소진됐다")
                projected = self.usage.cost_usd + self.usage.reserved_cost_usd + reservation
                if projected <= self.budget_usd + 1e-12:
                    break
                # 다른 스레드의 진행 중 예약 때문에만 모자라면 정산을 기다린다(병렬 수 × 예약액 문제).
                if not any(owner != me for owner in self._owner.values()):
                    raise BudgetError("턴 예산이 소진됐다")
                if deadline is None:
                    deadline = time.monotonic() + RESERVE_WAIT_S
                left = deadline - time.monotonic()
                if left <= 0:
                    raise BudgetError("턴 예산이 소진됐다")
                self._settled.wait(timeout=left)
            self._seq += 1
            self._owner[self._seq] = me
            self.usage.reserved_cost_usd += reservation
            self.usage.attempts += 1
            return _Attempt(
                attempt_id=f"{self.client_id}:{self._seq}",
                seq=self._seq,
                kind=kind,
                model=model,
                reservation=reservation,
                estimate=estimate,
                started=time.time(),
            )

    def _release(self, attempt: _Attempt) -> None:
        """호출자가 self._lock을 쥔 상태에서: 예약 소유를 풀고 기다리는 예약을 깨운다."""

        self._owner.pop(attempt.seq, None)
        self._settled.notify_all()

    def _event(self, attempt: _Attempt, *, status: str, cost: float, basis: str, raw: Any) -> dict[str, Any]:
        event: dict[str, Any] = {
            "seq": attempt.seq,
            "attempt_id": attempt.attempt_id,
            "kind": attempt.kind,
            "status": status,
            "cost": float(cost),
            "cost_basis": basis,
            "elapsed_ms": int((time.time() - attempt.started) * 1000),
        }
        if attempt.model:
            event["model"] = attempt.model
        if isinstance(raw, dict):
            for key in ("prompt_tokens", "completion_tokens", "total_tokens", "cost"):
                if key in raw:
                    event[key] = raw[key]
        return event

    def _settle(self, attempt: _Attempt, body: Any, *, status: str = "ok") -> dict[str, Any]:
        """응답을 받은 시도를 정산한다. 예산 초과라도 예외 없이 기록하고, 다음 예약에서 막는다."""

        raw = body.get("usage") if isinstance(body, dict) else None
        reported = _parse_usage_cost(raw)
        if reported is None:
            cost, basis = attempt.estimate, "estimate"
        else:
            cost, basis = reported, "provider"
        with self._lock:
            self._release(attempt)
            self.usage.reserved_cost_usd = max(0.0, self.usage.reserved_cost_usd - attempt.reservation)
            self.usage.cost_usd += cost
            if basis == "provider":
                self.usage.verified_cost_usd += cost
            else:
                self.usage.estimated_cost_usd += cost
            if status == "ok":
                self.usage.calls += 1
            else:
                self.usage.failed_attempts += 1
            event = self._event(attempt, status=status, cost=cost, basis=basis, raw=raw)
            self.usage.events.append(event)
        return event

    def _fail(self, attempt: _Attempt, *, billed: str, status: str = "failed") -> None:
        """응답 본문 없이 끝난 시도. billed='no' 면 비용 0, 'unknown' 이면 예약액을 미확정 비용으로 남긴다."""

        with self._lock:
            self._release(attempt)
            self.usage.reserved_cost_usd = max(0.0, self.usage.reserved_cost_usd - attempt.reservation)
            self.usage.failed_attempts += 1
            if billed == "unknown":
                self.usage.cost_usd += attempt.reservation
                self.usage.unknown_cost_usd += attempt.reservation
                self.usage.unknown_cost_attempts += 1
                cost, basis = attempt.reservation, "reservation"
            else:
                cost, basis = 0.0, "none"
            self.usage.events.append(self._event(attempt, status=status, cost=cost, basis=basis, raw=None))

    def _note_error(self, label: str) -> None:
        with self._lock:
            self.usage.errors[label] = self.usage.errors.get(label, 0) + 1

    def _backoff(self, attempt: int) -> None:
        self._sleep(min(2**attempt, 12) * (0.35 + random.random() * 0.4))

    def _request(self, path: str, data: bytes, *, stream: bool = False) -> urllib.request.Request:
        headers = {
            "Authorization": f"Bearer {self._key}",
            "Content-Type": "application/json",
        }
        if stream:
            headers["Accept"] = "text/event-stream"
        return urllib.request.Request(self.base + path, method="POST", data=data, headers=headers)

    def _post(
        self,
        path: str,
        payload: dict[str, Any],
        *,
        timeout: int = 180,
        fallback: float,
        kind: str = "api",
        model: str | None = None,
    ) -> dict[str, Any]:
        """비스트림 POST. 응답 본문을 받은 시도는 반환/예외와 무관하게 먼저 정산한다."""

        data = json.dumps(payload).encode("utf-8")
        last = "error"
        for attempt_no in range(MAX_ATTEMPTS):
            attempt = self._reserve(kind=kind, model=model, estimate=fallback)
            req = self._request(path, data)
            body: Any = None
            try:
                with self._urlopen(req, timeout=timeout) as resp:
                    body = json.loads(resp.read().decode("utf-8"))
            except urllib.error.HTTPError as exc:
                code = int(exc.code)
                last = f"HTTP {code}"
                self._note_error(last)
                self._fail(attempt, billed="no")
                if code not in RETRYABLE or attempt_no == MAX_ATTEMPTS - 1:
                    raise ClientError(f"API 요청 실패 ({last})") from exc
            except Exception as exc:  # noqa: BLE001
                last = type(exc).__name__
                self._note_error(last)
                self._fail(attempt, billed="unknown" if _billing_unknown(exc) else "no")
                if attempt_no == MAX_ATTEMPTS - 1:
                    raise ClientError(f"API 요청 실패 ({last})") from exc
            else:
                if not isinstance(body, dict):
                    self._fail(attempt, billed="unknown")
                    raise ClientError("API 응답 형식이 잘못됐다")
                if "error" in body and "choices" not in body and "data" not in body:
                    last = "provider_error"
                    self._note_error(last)
                    # 200 + error 객체: usage가 있으면 그 비용을 실패로 정산, 없으면 과금 미확정으로 남긴다.
                    if isinstance(body.get("usage"), dict):
                        self._settle(attempt, body, status="failed")
                    else:
                        self._fail(attempt, billed="unknown")
                    if attempt_no == MAX_ATTEMPTS - 1:
                        raise ClientError("API 제공자 오류")
                else:
                    self._settle(attempt, body, status="ok")
                    return body
            self._backoff(attempt_no)
        raise ClientError(f"API 요청 실패 ({last})")

    def embed(
        self,
        texts: list[str],
        *,
        kind: str = "document",
        instruction: str | None = None,
    ) -> list[list[float]]:
        if not texts:
            return []
        payload_texts = _embed_inputs(
            texts,
            model=self.embed_model,
            kind=kind,
            instruction=instruction,
        )
        fallback = max(0.0002, 0.00002 * sum(max(len(t), 1) for t in payload_texts) / 1000.0)
        body = self._post(
            "/embeddings",
            {"model": self.embed_model, "input": payload_texts},
            fallback=fallback,
            kind="embed",
            model=self.embed_model,
        )
        data = body.get("data")
        if not isinstance(data, list) or len(data) != len(texts):
            raise ClientError("임베딩 행 수가 맞지 않는다")
        out: list[list[float]] = []
        for row in sorted(data, key=lambda item: int(item.get("index", 0))):
            vec = row.get("embedding")
            if not isinstance(vec, list) or not vec:
                raise ClientError("임베딩 벡터가 비었다")
            out.append([float(x) for x in vec])
        return out

    def embed_many(
        self,
        texts: list[str],
        *,
        batch_size: int = 32,
        workers: int | None = None,
        kind: str = "document",
        instruction: str | None = None,
    ) -> list[list[float]]:
        """배치를 나눠 병렬 임베딩한다. 결과는 입력 순서를 유지한다."""

        if not texts:
            return []
        batch_size = max(1, int(batch_size))
        batches = [texts[i : i + batch_size] for i in range(0, len(texts), batch_size)]
        workers = self.max_workers if workers is None else max(1, int(workers))

        def _one(batch: list[str]) -> list[list[float]]:
            return self.embed(batch, kind=kind, instruction=instruction)

        parts = map_parallel(batches, _one, workers=min(workers, len(batches)))
        out: list[list[float]] = []
        for part in parts:
            out.extend(part)
        if len(out) != len(texts):
            raise ClientError("병렬 임베딩 결과 길이가 맞지 않는다")
        return out

    def chat(
        self,
        messages: list[dict[str, str]],
        *,
        max_tokens: int = 512,
        temperature: float = 0.2,
        reasoning: str | None = "low",
        model: str | None = None,
    ) -> str:
        payload: dict[str, Any] = {
            "model": model or self.llm_model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
        }
        if self.llm_provider and not model:
            # provider 고정은 기본 LLM에만. 역할별로 바꾼 모델은 OpenRouter 라우팅에 맡긴다.
            payload["provider"] = {
                "order": [self.llm_provider],
                "allow_fallbacks": True,
            }
        if reasoning:
            payload["reasoning"] = {"effort": reasoning}
        last = "채팅 본문이 비었다"
        for attempt in range(MAX_ATTEMPTS):
            body = self._post("/chat/completions", payload, fallback=0.01, kind="chat", model=str(payload["model"]))
            choices = body.get("choices")
            if not isinstance(choices, list) or not choices:
                last = "채팅 응답이 비었다"
            else:
                message = choices[0].get("message") if isinstance(choices[0], dict) else None
                content = message.get("content") if isinstance(message, dict) else None
                if isinstance(content, str) and content.strip():
                    return content
                last = "채팅 본문이 비었다"
            if attempt == MAX_ATTEMPTS - 1:
                raise ClientError(last)
            self._backoff(attempt)
        raise ClientError(last)

    def chat_stream(
        self,
        messages: list[dict[str, str]],
        *,
        max_tokens: int = 512,
        temperature: float = 0.2,
        reasoning: str | None = "low",
        model: str | None = None,
    ):
        """토큰 단위로 흘린다. yield dict: {"type":"delta","text"} * → 정확히 하나의 {"type":"done",...}.

        terminal 계약(소비자는 둘 중 하나만 본다):
        - 성공: `[DONE]` 표지를 받은 뒤 정산하고 `done` 하나를 yield 한 뒤 끝난다.
          done = {"type":"done","cost":누적 cost_usd,"call_cost":이 호출 비용,"cost_basis":provider|estimate,
                  "attempt_id","finish_reason","end":"done_marker","chars":흘린 문자 수}
        - 실패: `done` 없이 StreamError(status=provider_error|truncated|empty|malformed|transport)
          또는 ClientError/BudgetError 를 raise 한다. delta 가 일부 나간 뒤에도 성공 done 을 만들지 않는다.
        - 소비자가 generator 를 close 하면(GeneratorExit) 응답을 닫고 예약을 정리하며
          usage 를 받았으면 그 비용, 아니면 예약액을 미확정 비용으로 남긴다(status=cancelled).
        응답이 시작된 요청은 재시도하지 않는다. 재시도는 응답 이전의 재시도 가능한 HTTP 오류·연결 거부에만 한다.
        """

        payload: dict[str, Any] = {
            "model": model or self.llm_model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "stream": True,
            "stream_options": {"include_usage": True},
        }
        if self.llm_provider and not model:
            payload["provider"] = {
                "order": [self.llm_provider],
                "allow_fallbacks": True,
            }
        if reasoning:
            payload["reasoning"] = {"effort": reasoning}

        data = json.dumps(payload).encode("utf-8")
        model_name = str(payload["model"])
        last = "error"
        for attempt_no in range(MAX_ATTEMPTS):
            attempt = self._reserve(kind="chat_stream", model=model_name, estimate=0.01)
            req = self._request("/chat/completions", data, stream=True)
            try:
                resp = self._urlopen(req, timeout=300)
            except urllib.error.HTTPError as exc:
                code = int(exc.code)
                last = f"HTTP {code}"
                self._note_error(last)
                self._fail(attempt, billed="no")
                if code not in RETRYABLE or attempt_no == MAX_ATTEMPTS - 1:
                    raise ClientError(f"API 요청 실패 ({last})") from exc
                self._backoff(attempt_no)
                continue
            except Exception as exc:  # noqa: BLE001
                last = type(exc).__name__
                self._note_error(last)
                unknown = _billing_unknown(exc)
                self._fail(attempt, billed="unknown" if unknown else "no")
                if unknown or attempt_no == MAX_ATTEMPTS - 1:
                    raise ClientError(f"API 요청 실패 ({last})") from exc
                self._backoff(attempt_no)
                continue
            # 응답이 열렸다: 이 시도는 재시도하지 않고 반드시 한 번 정산/실패 처리한다.
            yield from self._consume_stream(resp, attempt)
            return
        raise ClientError(f"API 요청 실패 ({last})")

    def _consume_stream(self, resp: Any, attempt: _Attempt):
        usage_body: dict[str, Any] | None = None
        chars = 0
        done_marker = False
        finish_reason: str | None = None
        error_label: str | None = None
        malformed = 0

        def _finish_failure(status: str, message: str) -> StreamError:
            if usage_body is not None:
                self._settle(attempt, usage_body, status="failed")
            else:
                self._fail(attempt, billed="unknown", status="failed")
            self._note_error(f"stream_{status}")
            return StreamError(message, status=status, partial_chars=chars, attempt_id=attempt.attempt_id)

        try:
            with resp:
                for raw in resp:
                    line = raw.decode("utf-8", errors="replace").strip()
                    if not line or line.startswith(":"):
                        continue
                    if not line.startswith("data:"):
                        continue
                    payload_text = line[5:].strip()
                    if payload_text == "[DONE]":
                        done_marker = True
                        break
                    try:
                        chunk = json.loads(payload_text)
                    except json.JSONDecodeError:
                        malformed += 1
                        break
                    if not isinstance(chunk, dict):
                        malformed += 1
                        break
                    if isinstance(chunk.get("usage"), dict):
                        usage_body = {"usage": chunk["usage"]}
                    err = chunk.get("error")
                    if err:
                        code = err.get("code") if isinstance(err, dict) else None
                        error_label = f"sse_error_{code}" if code is not None else "sse_error"
                        # 본문(message)은 기록하지 않는다. 남은 줄은 usage 회수를 위해서만 읽는다.
                        continue
                    choices = chunk.get("choices")
                    if not isinstance(choices, list) or not choices:
                        continue
                    first = choices[0] if isinstance(choices[0], dict) else {}
                    reason = first.get("finish_reason")
                    if isinstance(reason, str) and reason:
                        finish_reason = reason
                        if reason == "error":
                            error_label = error_label or "finish_error"
                    if error_label:
                        continue
                    delta = first.get("delta")
                    if not isinstance(delta, dict):
                        continue
                    piece = delta.get("content")
                    if isinstance(piece, str) and piece:
                        chars += len(piece)
                        yield {"type": "delta", "text": piece}
        except Exception as exc:  # noqa: BLE001 - 읽기 중 전송 오류
            label = type(exc).__name__
            self._note_error(label)
            raise _finish_failure("transport", f"API 요청 실패 ({label})") from exc
        except BaseException:
            # 소비자 취소(GeneratorExit)·인터럽트: 응답은 with 로 닫혔다. 예약 정리 + 비용 보존, yield 금지.
            if usage_body is not None:
                self._settle(attempt, usage_body, status="cancelled")
            else:
                self._fail(attempt, billed="unknown", status="cancelled")
            self._note_error("stream_cancelled")
            raise

        if error_label:
            self._note_error(error_label)
            raise _finish_failure("provider_error", "스트림이 제공자 오류로 끝났다")
        if malformed:
            raise _finish_failure("malformed", "스트림 이벤트가 손상됐다")
        if not done_marker:
            raise _finish_failure("truncated", "스트림이 종료 표지 없이 끊겼다")
        if chars == 0:
            raise _finish_failure("empty", "채팅 본문이 비었다")
        event = self._settle(attempt, usage_body or {}, status="ok")
        yield {
            "type": "done",
            "cost": float(self.usage.cost_usd),
            "call_cost": float(event["cost"]),
            "cost_basis": event["cost_basis"],
            "attempt_id": attempt.attempt_id,
            "finish_reason": finish_reason,
            "end": "done_marker",
            "chars": chars,
        }

    def chat_many(
        self,
        jobs: list[list[dict[str, str]]],
        *,
        max_tokens: int = 512,
        temperature: float = 0.2,
        reasoning: str | None = "low",
        workers: int | None = None,
        model: str | None = None,
    ) -> list[str]:
        workers = self.max_workers if workers is None else max(1, int(workers))

        def _one(messages: list[dict[str, str]]) -> str:
            return self.chat(
                messages,
                max_tokens=max_tokens,
                temperature=temperature,
                reasoning=reasoning,
                model=model,
            )

        return map_parallel(jobs, _one, workers=min(workers, max(1, len(jobs))))
