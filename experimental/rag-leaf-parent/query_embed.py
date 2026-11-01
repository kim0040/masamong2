"""질의 임베딩: 캐시·선행 요청·지연 헤지.

턴 지연에서 임베딩 호출은 로컬 검색(수십 ms)보다 훨씬 길고 공급자 편차가 크다(같은 입력이 0.5초~20초).
결과를 바꾸지 않고 기다림만 줄인다.

- 캐시: (임베딩 모델, instruction, 질의) → 정규화 벡터. 같은 질문을 다시 물으면 호출하지 않는다.
- 선행 요청(prefetch): 플래너가 도는 동안 원문 질의(q0)를 미리 임베딩한다. q0는 항상 원문이라 계획 결과와 무관하다.
- 헤지: 첫 요청이 `hedge_after_ms`(기본 0.8초, 정상 응답 0.55~0.7초) 안에 끝나지 않으면 같은 요청을 한 번 더 보내고 먼저 온 쪽을 쓴다.
  늦게 끝난 쪽의 비용도 `client.usage`에 정산되며, `on_late`로 장부에 반영할 수 있다.

벡터는 저장 차원 절단(`fit_query_vector`) 전 원래 차원으로 보관한다.
"""

from __future__ import annotations

import threading
from collections import OrderedDict
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from typing import Any, Callable, Mapping

import numpy as np

DEFAULTS: dict[str, Any] = {
    "cache": True,
    "cache_entries": 512,
    "prefetch": True,
    "hedge_after_ms": 800,
}

# 두 풀로 나눈다: 선행 요청(_PREFETCH)은 실제 호출(_CALLS)을 기다리므로 같은 풀이면 포화 시 교착된다.
# 늦은 쪽 요청은 클라이언트 timeout까지 스레드를 잡을 수 있어 여유 있게 둔다.
_CALLS = ThreadPoolExecutor(max_workers=64, thread_name_prefix="qembed-call")
_PREFETCH = ThreadPoolExecutor(max_workers=32, thread_name_prefix="qembed-prefetch")


def query_embed_config(cfg: Mapping[str, Any] | None) -> dict[str, Any]:
    out = dict(DEFAULTS)
    raw = (cfg or {}).get("query_embed")
    if isinstance(raw, Mapping):
        out.update({k: raw[k] for k in DEFAULTS if k in raw})
    return out


def _normalized(vec: Any) -> np.ndarray:
    arr = np.asarray(vec, dtype=np.float32)
    return arr / (np.linalg.norm(arr) + 1e-9)


class QueryEmbedder:
    def __init__(self, cfg: Mapping[str, Any] | None = None) -> None:
        c = query_embed_config(cfg)
        self.cache_enabled = bool(c["cache"])
        self.max_entries = max(1, int(c["cache_entries"]))
        self.hedge_after_s = max(0.0, float(c["hedge_after_ms"]) / 1000.0)
        self._lock = threading.Lock()
        self._cache: OrderedDict[tuple[str, str, str], np.ndarray] = OrderedDict()
        self._inflight: dict[tuple[str, str, str], Future] = {}
        self.stats = {"hit": 0, "joined": 0, "miss": 0, "hedged": 0}

    @staticmethod
    def _key(client: Any, text: str, instruction: str) -> tuple[str, str, str]:
        return (str(getattr(client, "embed_model", "") or ""), instruction, text)

    def _call(self, client: Any, text: str, instruction: str) -> np.ndarray:
        return _normalized(client.embed([text], kind="query", instruction=instruction)[0])

    def _hedged(
        self,
        client: Any,
        text: str,
        instruction: str,
        on_late: Callable[[], None] | None,
    ) -> tuple[np.ndarray, bool]:
        first = _CALLS.submit(self._call, client, text, instruction)
        if self.hedge_after_s <= 0:
            return first.result(), False
        done, _ = wait([first], timeout=self.hedge_after_s)
        if done:
            return first.result(), False
        second = _CALLS.submit(self._call, client, text, instruction)
        pending = {first, second}
        error: BaseException | None = None
        while pending:
            done, pending = wait(pending, return_when=FIRST_COMPLETED)
            for fut in done:
                if fut.exception() is None:
                    for loser in pending:
                        if on_late is not None:
                            loser.add_done_callback(lambda _f: on_late())
                    with self._lock:
                        self.stats["hedged"] += 1
                    return fut.result(), True
                error = fut.exception()
        assert error is not None
        raise error

    def _resolve(
        self,
        client: Any,
        text: str,
        instruction: str,
        on_late: Callable[[], None] | None,
    ) -> tuple[np.ndarray, bool]:
        key = self._key(client, text, instruction)
        try:
            vec, hedged = self._hedged(client, text, instruction, on_late)
        except BaseException:
            with self._lock:
                self._inflight.pop(key, None)
            raise
        with self._lock:
            self._inflight.pop(key, None)
            if self.cache_enabled:
                self._cache[key] = vec
                self._cache.move_to_end(key)
                while len(self._cache) > self.max_entries:
                    self._cache.popitem(last=False)
        return vec, hedged

    def _lookup(self, key: tuple[str, str, str]) -> tuple[np.ndarray | None, Future | None]:
        with self._lock:
            if self.cache_enabled and key in self._cache:
                self._cache.move_to_end(key)
                return self._cache[key], None
            return None, self._inflight.get(key)

    def prefetch(
        self,
        client: Any,
        text: str,
        *,
        instruction: str,
        on_late: Callable[[], None] | None = None,
    ) -> None:
        """결과를 기다리지 않고 요청만 시작한다. 이미 캐시·진행 중이면 아무것도 하지 않는다."""

        text = str(text or "")
        if not text.strip():
            return
        key = self._key(client, text, instruction)
        with self._lock:
            if (self.cache_enabled and key in self._cache) or key in self._inflight:
                return
            fut = _PREFETCH.submit(self._resolve, client, text, instruction, on_late)
            self._inflight[key] = fut

    def get(
        self,
        client: Any,
        text: str,
        *,
        instruction: str,
        on_late: Callable[[], None] | None = None,
    ) -> tuple[np.ndarray, dict[str, Any]]:
        """정규화된 질의 벡터와 출처(`hit`|`joined`|`miss`, `hedged`)를 반환한다."""

        text = str(text or "")
        key = self._key(client, text, instruction)
        vec, fut = self._lookup(key)
        if vec is not None:
            with self._lock:
                self.stats["hit"] += 1
            return vec, {"source": "hit", "hedged": False}
        if fut is not None:
            vec, hedged = fut.result()
            with self._lock:
                self.stats["joined"] += 1
            return vec, {"source": "joined", "hedged": hedged}
        vec, hedged = self._resolve(client, text, instruction, on_late)
        with self._lock:
            self.stats["miss"] += 1
        return vec, {"source": "miss", "hedged": hedged}
