"""Sandbox adapters using masamong2 Action IDs. No Discord token.

Result contract (docs/rag/README.md §3 샌드박스 공통 계약 1):

    execute_named_tool(...) -> {
        "tool":   Action ID (canonical),
        "ok":     True  ⇔ status in OK_STATUSES ("ok" | "empty"),
        "status": one of TOOL_STATUSES,
        "data":   verified payload — present only when ok is True,
        "error":  short machine-safe text  — present only when ok is False,
        "error_kind": category (see ERROR_KINDS) — present only when ok is False,
        "missing": [param names] — needs_input only,
        "source": provider label when known,
    }

`ok=True` never coexists with an internal error. "empty" is a verified zero-result
lookup (the provider answered, nothing matched); it is distinct from unconfigured /
needs_input / error. Provider exceptions are reduced to `error_kind` (+ `http_status`)
and never carry the message body, URL query or any secret.

Consumers that only read `tool` / `ok` / `data` / `error` keep working: those keys have the
same meaning as before, the new keys are additive.
"""

from __future__ import annotations

import json
import re
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Mapping

from tool_parity import canonicalize_tool, default_enabled_ids

USER_AGENT = "masamong2-test-sandbox/0.1 (local eval; not a product bot)"

# --- result status enum -------------------------------------------------------------------
TOOL_STATUSES = (
    "ok",              # verified usable payload
    "empty",           # provider answered, zero matches (verified, not a failure)
    "needs_input",     # required argument missing / placeholder / invalid
    "unconfigured",    # provider or key not configured in this surface
    "disabled",        # tool is off in the channel/runtime policy
    "skipped",         # deliberately not executed in this surface (e.g. paid image)
    "deferred",        # executed elsewhere (memory.search → engine RAG), not here
    "unknown_tool",    # not a catalog Action ID
    "error",           # provider/transport/parse failure with a known outcome (nothing happened)
    "unknown_outcome", # side effect may or may not have happened
)
OK_STATUSES = frozenset({"ok", "empty"})
ERROR_KINDS = (
    "missing_param", "placeholder_param", "invalid_param",
    "provider_unconfigured", "disabled", "skipped", "deferred", "unknown_tool",
    "not_found", "provider_error",
    "timeout", "http_error", "url_error", "invalid_response", "io_error", "tool_error",
    "store_corrupt", "unknown_outcome",
)


class ToolError(RuntimeError):
    pass


JsonFetch = Callable[..., Any]
TextFetch = Callable[..., str]


def _http_json(url: str, *, timeout: float = 8.0, headers: dict[str, str] | None = None) -> Any:
    hdrs = {"User-Agent": USER_AGENT, "Accept": "application/json"}
    if headers:
        hdrs.update(headers)
    req = urllib.request.Request(url, headers=hdrs)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def _http_text(url: str, *, timeout: float = 8.0) -> str:
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": "text/html"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode("utf-8", errors="replace")


# --- result constructors ------------------------------------------------------------------
def make_result(tool: str, status: str, *, data: Any = None, error_kind: str = "", **extra: Any) -> dict[str, Any]:
    """Build the normalized envelope. `data` is attached only on ok/empty."""

    if status not in TOOL_STATUSES:
        raise ValueError(f"unknown tool status {status!r}")
    ok = status in OK_STATUSES
    out: dict[str, Any] = {"tool": tool, "ok": ok, "status": status}
    if ok:
        out["data"] = data if data is not None else {}
    else:
        kind = error_kind or status
        out["error_kind"] = kind
        out["error"] = str(extra.pop("error", "") or kind)
    for key, value in extra.items():
        if value is not None:
            out[key] = value
    return out


def ok_result(tool: str, data: Mapping[str, Any], **extra: Any) -> dict[str, Any]:
    return make_result(tool, "ok", data=dict(data), **extra)


def empty_result(tool: str, data: Mapping[str, Any], **extra: Any) -> dict[str, Any]:
    return make_result(tool, "empty", data=dict(data), **extra)


def needs_input_result(tool: str, missing: list[str], *, error_kind: str = "missing_param", **extra: Any) -> dict[str, Any]:
    return make_result(tool, "needs_input", error_kind=error_kind, error=f"{error_kind}: {','.join(missing)}", missing=list(missing), **extra)


def unconfigured_result(tool: str, **extra: Any) -> dict[str, Any]:
    return make_result(tool, "unconfigured", error_kind="provider_unconfigured", error="provider_unconfigured", **extra)


def disabled_result(tool: str, **extra: Any) -> dict[str, Any]:
    """Same shape engine already emits for a gated tool ({tool, ok:false, error:"disabled"}) plus status."""

    return make_result(tool, "disabled", error_kind="disabled", error="disabled", **extra)


def is_success(result: Mapping[str, Any] | None) -> bool:
    """True only when the envelope is a verified ok/empty payload (never on internal error)."""

    if not isinstance(result, Mapping):
        return False
    return bool(result.get("ok")) and str(result.get("status") or "") in OK_STATUSES and "data" in result


def error_result_from_exception(tool: str, exc: BaseException, **extra: Any) -> dict[str, Any]:
    """Map a provider/transport exception to (status, error_kind) without copying its text."""

    kind = "tool_error"
    status = "error"
    http_status: int | None = None
    if isinstance(exc, urllib.error.HTTPError):
        kind = "http_error"
        http_status = int(getattr(exc, "code", 0) or 0) or None
    elif isinstance(exc, (TimeoutError, socket.timeout)):
        kind = "timeout"
    elif isinstance(exc, urllib.error.URLError):
        kind = "timeout" if isinstance(getattr(exc, "reason", None), (TimeoutError, socket.timeout)) else "url_error"
    elif isinstance(exc, json.JSONDecodeError):
        kind = "invalid_response"
    elif isinstance(exc, (KeyError, IndexError, TypeError)):
        kind = "invalid_response"
    elif isinstance(exc, ValueError):
        kind = "invalid_param"
    elif isinstance(exc, ToolError):
        kind = "tool_error"
    elif isinstance(exc, OSError):
        kind = "io_error"
    return make_result(
        tool,
        status,
        error_kind=kind,
        error=kind,
        exception=type(exc).__name__,
        http_status=http_status,
        **extra,
    )


# --- input validation ---------------------------------------------------------------------
# 플래너가 값을 모를 때 만들어 내는 자리표시자. 그대로 지오코딩하면 "장소를 찾지 못했다"로 끝난다.
_PLACEHOLDER_RE = re.compile(
    r"(사용자|유저|당신|본인|현재\s*위치|내\s*위치|user|your|current|unknown|none|null|n/a|미정|없음|[{}<>\[\]])",
    re.I,
)
_PLACEHOLDER_EXACT = {"지역", "위치", "장소", "region", "location", "city", "here", "여기", "symbol", "ticker", "티커", "종목", "query", "질의"}
# 옛 코드의 암묵 기본값. 이제 실행 경로에서 자동 적용하지 않는다 (tool_defaults 로 출처와 함께 주입해야 한다).
DEFAULT_REGION = "서울"
_SYMBOL_RE = re.compile(r"^[A-Z0-9][A-Z0-9.\-=^]{0,19}$")
_CURRENCY_RE = re.compile(r"^[A-Z]{3}$")


def is_placeholder(value: Any) -> bool:
    text = str(value or "").strip()
    if not text:
        return True
    if text.casefold() in _PLACEHOLDER_EXACT:
        return True
    return bool(_PLACEHOLDER_RE.search(text))


def resolve_region(
    location: Any,
    *,
    tool_defaults: Mapping[str, Any] | None = None,
) -> tuple[str, str]:
    """(region, region_source). region == "" means unresolved (needs_input).

    A configured default is used only when the caller passes it explicitly with its origin:
    tool_defaults = {"region": "부산", "source": "channel.tool_defaults"}. The user's real
    location is never assumed (no silent Seoul).
    """

    text = str(location or "").strip()
    if not is_placeholder(text):
        return text, "argument"
    default = dict(tool_defaults or {})
    region = str(default.get("region") or default.get("location") or "").strip()
    if region and not is_placeholder(region):
        return region, str(default.get("source") or "configured_default")
    return "", ""


def _int_param(value: Any, fallback: int) -> int | None:
    if value is None or value == "":
        return fallback
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


# --- adapters -----------------------------------------------------------------------------
def weather_query(
    location: str,
    day_offset: int = 0,
    *,
    fetch_json: JsonFetch | None = None,
    region_source: str = "argument",
) -> dict[str, Any]:
    """Weather for an already-resolved region. Caller resolves placeholders (resolve_region).

    Returns the inner payload with `status` in {"ok", "not_found"}; execute_named_tool maps it.
    """

    fetch = fetch_json or _http_json
    q = str(location or "").strip()
    if is_placeholder(q):
        return {"status": "needs_input", "missing": ["location"]}
    geo = fetch(
        "https://geocoding-api.open-meteo.com/v1/search?"
        + urllib.parse.urlencode({"name": q, "count": 1, "language": "ko", "format": "json"})
    )
    results = geo.get("results") if isinstance(geo, dict) else None
    if not results:
        return {"status": "not_found", "region": q, "region_source": region_source, "live": False}
    hit = results[0]
    lat, lon = hit["latitude"], hit["longitude"]
    forecast = fetch(
        "https://api.open-meteo.com/v1/forecast?"
        + urllib.parse.urlencode(
            {
                "latitude": lat,
                "longitude": lon,
                "current": "temperature_2m,apparent_temperature,weather_code",
                "daily": "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max",
                "timezone": "Asia/Seoul",
                "forecast_days": 3,
            }
        )
    )
    if not isinstance(forecast, dict) or not isinstance(forecast.get("current"), dict):
        return {"status": "invalid_response"}
    daily = forecast.get("daily") or {}
    idx = max(0, min(int(day_offset or 0), 2))
    current = forecast.get("current") or {}
    observed = current.get("time") or ""
    observed_at = ""
    if observed:
        observed_at = str(observed)
        if len(observed_at) == 16:
            observed_at += ":00"
        if "T" in observed_at and not observed_at.endswith("Z") and "+" not in observed_at[10:]:
            observed_at += "+09:00"
    return {
        "status": "ok",
        "live": True,
        "region": hit.get("name") or q,
        "region_source": region_source,
        "country": hit.get("country"),
        "temp_c": current.get("temperature_2m"),
        "day_offset": idx,
        "current_weather": (
            f"{current.get('temperature_2m')}°C "
            f"(체감 {current.get('apparent_temperature')}°C) code={current.get('weather_code')}"
        ),
        "forecast": {
            "tmax": (daily.get("temperature_2m_max") or [None])[idx] if daily else None,
            "tmin": (daily.get("temperature_2m_min") or [None])[idx] if daily else None,
            "precip": (daily.get("precipitation_probability_max") or [None])[idx] if daily else None,
        },
        "observed_at": observed_at,
        "source": "open-meteo",
        "product_provider": "kma",
    }


def disasters_query(provider: Callable[[], Mapping[str, Any]] | None = None) -> dict[str, Any]:
    """`provider` returns {"earthquakes": [...], "warnings": [...]} from a configured source.

    Without a provider the sandbox has no KMA key → status "unconfigured" (not a safety claim,
    not a success). With a provider that returns nothing → "empty" (verified, no active alert).
    """

    if provider is None:
        return {
            "status": "unconfigured",
            "live": False,
            "note": "no_provider_not_safety_claim",
            "message": "샌드박스는 기상청 키가 없으면 특보를 조회하지 않는다. 제품 Action은 disasters.query 그대로다.",
            "product_provider": "kma",
        }
    raw = provider()
    if not isinstance(raw, Mapping):
        return {"status": "invalid_response"}
    quakes = list(raw.get("earthquakes") or [])
    warnings = list(raw.get("warnings") or [])
    return {
        "status": "ok" if (quakes or warnings) else "empty",
        "live": True,
        "earthquakes": quakes,
        "warnings": warnings,
        "note": "" if (quakes or warnings) else "no_active_alert_not_safety_claim",
        "source": str(raw.get("source") or "provider"),
        "product_provider": "kma",
    }


def finance_quote(symbol: str, *, fetch_json: JsonFetch | None = None) -> dict[str, Any]:
    fetch = fetch_json or _http_json
    ticker = str(symbol or "").strip().upper()
    if not ticker or is_placeholder(ticker) or not _SYMBOL_RE.match(ticker):
        return {"status": "needs_input", "missing": ["symbol"]}
    url = (
        "https://query1.finance.yahoo.com/v8/finance/chart/"
        + urllib.parse.quote(ticker)
        + "?"
        + urllib.parse.urlencode({"interval": "1d", "range": "1d"})
    )
    data = fetch(url, timeout=10.0)
    chart = data.get("chart") if isinstance(data, dict) else None
    result = ((chart or {}).get("result") or [None])[0]
    if not isinstance(result, dict):
        return {"status": "not_found", "symbol": ticker}
    meta = result.get("meta") or {}
    price = meta.get("regularMarketPrice")
    if price is None:
        return {"status": "not_found", "symbol": ticker}
    at = ""
    ts = meta.get("regularMarketTime")
    if isinstance(ts, (int, float)) and ts > 0:
        at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(int(ts)))
    return {
        "status": "ok",
        "live": True,
        "source": "yahoo-chart",
        "product_provider": "finnhub",
        "symbol": meta.get("symbol") or ticker,
        "price": price,
        "currency": meta.get("currency"),
        "at": at,
        "quote": {
            "symbol": meta.get("symbol") or ticker,
            "price": price,
            "currency": meta.get("currency"),
            "exchange": meta.get("exchangeName"),
        },
    }


def finance_fx(base: str = "USD", *, fetch_json: JsonFetch | None = None) -> dict[str, Any]:
    fetch = fetch_json or _http_json
    code = str(base or "USD").strip().upper() or "USD"
    if is_placeholder(code) or not _CURRENCY_RE.match(code):
        return {"status": "needs_input", "missing": ["base"]}
    data = fetch(f"https://open.er-api.com/v6/latest/{urllib.parse.quote(code)}", timeout=10.0)
    if not isinstance(data, dict):
        return {"status": "invalid_response"}
    if str(data.get("result") or "").lower() != "success":
        # provider error code is a short token ("unsupported-code"); keep only [a-z0-9_-].
        first = str(data.get("error-type") or "").strip().split()[:1]
        code_txt = re.sub(r"[^a-z0-9_\-]", "", first[0].lower())[:32] if first else ""
        return {"status": "provider_error", "base": code, "provider_code": code_txt or "fx_failed"}
    rates = data.get("rates") or {}
    want = ["KRW", "JPY", "EUR", "CNY", "GBP"]
    sel = {c: rates[c] for c in want if c in rates}
    if not sel:
        return {"status": "empty", "base": data.get("base_code") or code, "rates": {}, "source": "open.er-api.com"}
    return {
        "status": "ok",
        "live": True,
        "source": "open.er-api.com",
        "product_provider": "exchangerate-api",
        "base": data.get("base_code") or code,
        "rates": sel,
        "updated_at": str(data.get("time_last_update_utc") or ""),
    }


def search_web(query: str, *, fetch_text: TextFetch | None = None) -> dict[str, Any]:
    fetch = fetch_text or _http_text
    q = (query or "").strip()
    if is_placeholder(q):
        return {"status": "needs_input", "missing": ["query"]}
    url = "https://html.duckduckgo.com/html/?" + urllib.parse.urlencode({"q": q})
    html = fetch(url, timeout=10.0)
    blocks = re.findall(
        r'<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>',
        html,
        flags=re.I | re.S,
    )
    snippets = re.findall(r'class="result__snippet"[^>]*>(.*?)</(?:a|td)>', html, flags=re.I | re.S)
    clean = []
    for i, (href, title) in enumerate(blocks[:5]):
        text = re.sub(r"<[^>]+>", "", title)
        text = re.sub(r"\s+", " ", text).strip()
        snip = ""
        if i < len(snippets):
            snip = re.sub(r"<[^>]+>", "", snippets[i])
            snip = re.sub(r"\s+", " ", snip).strip()[:240]
        link = urllib.parse.unquote(href.replace("&amp;", "&"))
        if link.startswith("//"):
            link = "https:" + link
        if "uddg=" in link:
            parsed = urllib.parse.parse_qs(urllib.parse.urlparse(link.replace("&amp;", "&")).query)
            if parsed.get("uddg"):
                link = parsed["uddg"][0]
        if text:
            item = {"title": text[:160], "snippet": snip}
            if link.startswith("http"):
                item["url"] = link
            clean.append(item)
    return {
        "status": "ok" if clean else "empty",
        "query": q,
        "results": clean,
        "source": "duckduckgo-html",
        "product_provider": "kakao",
    }


POLL_STORE = "sandbox_polls.json"


def polls_create(
    data_dir: Path,
    question: str,
    options: list[str],
    *,
    duration_hours: int = 24,
    allow_multiselect: bool = False,
) -> dict[str, Any]:
    """Validate first; nothing is written unless the request is valid and the store is readable."""

    q = (question or "").strip()
    if not q or is_placeholder(q):
        return {"status": "needs_input", "missing": ["question"]}
    if len(q) > 300:
        return {"status": "invalid_param", "missing": ["question"], "reason": "question max 300 characters (Discord poll)"}
    opts = [str(o).strip()[:55] for o in options if str(o).strip()]
    if not opts:
        opts = ["예", "아니요"]
    if len(opts) < 2:
        return {"status": "invalid_param", "missing": ["options"], "reason": "at least two answers"}
    opts = opts[:10]
    hours = int(duration_hours or 24)
    hours = max(1, min(768, hours))
    emojis = ["1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"]
    answers = [
        {
            "poll_media": {
                "text": label,
                "emoji": {"name": emojis[i] if i < len(emojis) else ""},
            }
        }
        for i, label in enumerate(opts)
    ]
    path = Path(data_dir) / POLL_STORE
    rows: list[dict[str, Any]] = []
    if path.is_file():
        try:
            loaded = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            # 기존 저장소가 손상됐으면 덮어쓰지 않는다 (저장 없음).
            return {"status": "store_corrupt"}
        if not isinstance(loaded, list):
            return {"status": "store_corrupt"}
        rows = loaded
    poll_id = f"p{int(time.time())}"
    row = {
        "id": poll_id,
        "question": {"text": q},
        "answers": answers,
        "duration": hours,
        "allow_multiselect": bool(allow_multiselect),
        "layout_type": 1,
        "created_at": time.time(),
    }
    rows.append(row)
    tmp = path.with_suffix(".json.tmp")
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp.write_text(json.dumps(rows, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    except OSError:
        try:
            tmp.unlink()
        except OSError:
            pass
        return {"status": "io_error"}
    try:
        tmp.replace(path)
    except OSError:
        # replace 는 원자적이지만 예외 뒤 상태는 확정할 수 없다.
        return {"status": "unknown_outcome"}
    return {"status": "ok", "poll": row, "source": "discord-poll-create-request"}


def help_version() -> dict[str, Any]:
    return {
        "status": "ok",
        "commands": [
            "!날씨 <지역>",
            "!특보",
            "!시세 <티커>",
            "!환율 [USD]",
            "!검색 <질의>",
            "!이미지 <장면> (샌드박스는 생성하지 않음)",
            "!투표 질문 | 항목1, 항목2  (비우면 예/아니요, 기본 24시간)",
            "!상태",
            "!도움말",
        ],
    }


# --- dispatcher ---------------------------------------------------------------------------
_INNER_TO_STATUS = {
    # inner adapter status → (envelope status, error_kind)
    "needs_input": ("needs_input", "missing_param"),
    "invalid_param": ("needs_input", "invalid_param"),
    "unconfigured": ("unconfigured", "provider_unconfigured"),
    "not_found": ("error", "not_found"),
    "provider_error": ("error", "provider_error"),
    "invalid_response": ("error", "invalid_response"),
    "store_corrupt": ("error", "store_corrupt"),
    "io_error": ("error", "io_error"),
    "unknown_outcome": ("unknown_outcome", "unknown_outcome"),
}


def _wrap(tool: str, inner: Mapping[str, Any]) -> dict[str, Any]:
    """Lift an adapter's inner `status` into the envelope; ok only for ok/empty."""

    payload = dict(inner)
    status = str(payload.pop("status", "") or "")
    if status == "ok":
        return ok_result(tool, payload, source=payload.get("source"))
    if status == "empty":
        return empty_result(tool, payload, source=payload.get("source"))
    env_status, kind = _INNER_TO_STATUS.get(status, ("error", "tool_error"))
    extra: dict[str, Any] = {}
    for key in ("missing", "region", "region_source", "symbol", "base", "provider_code", "reason", "note", "product_provider"):
        if payload.get(key) not in (None, "", []):
            extra[key] = payload[key]
    if env_status == "needs_input":
        return needs_input_result(tool, list(payload.get("missing") or []), error_kind=kind, **{k: v for k, v in extra.items() if k != "missing"})
    if env_status == "unconfigured":
        return unconfigured_result(tool, **extra)
    return make_result(tool, env_status, error_kind=kind, error=kind, **extra)


def execute_named_tool(
    name: str,
    params: Mapping[str, Any] | None,
    *,
    data_dir: Path | None = None,
    index_meta: Mapping[str, Any] | None = None,
    tool_defaults: Mapping[str, Mapping[str, Any]] | None = None,
    providers: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """Run one catalog Action and return the normalized envelope (see module docstring).

    tool_defaults: {"weather.query": {"region": "부산", "source": "channel.tool_defaults"}} —
        explicit, sourced defaults (bot_runtime.resolve_tool_defaults). Never implied.
    providers: {"json": fetch_json, "text": fetch_text, "disasters": callable} — fake transports
        for tests; absent keys use the live HTTP helpers (disasters → unconfigured).
    """

    params = dict(params or {})
    action_id = canonicalize_tool(name)
    defaults = dict(tool_defaults or {})
    prov = dict(providers or {})
    fetch_json = prov.get("json")
    fetch_text = prov.get("text")
    try:
        if action_id == "memory.search":
            return make_result(action_id, "deferred", error_kind="deferred", error="deferred: engine_rag", deferred="engine_rag")
        if action_id == "weather.query":
            region, region_source = resolve_region(
                params.get("region") or params.get("location") or params.get("query"),
                tool_defaults=defaults.get("weather.query"),
            )
            if not region:
                return needs_input_result(action_id, ["location"], error_kind="placeholder_param", hint="지역을 물어봐라. 사용자의 위치를 가정하지 않는다.")
            offset = _int_param(params.get("day_offset"), 0)
            if offset is None:
                return needs_input_result(action_id, ["day_offset"], error_kind="invalid_param")
            return _wrap(action_id, weather_query(region, offset, fetch_json=fetch_json, region_source=region_source))
        if action_id == "disasters.query":
            return _wrap(action_id, disasters_query(prov.get("disasters")))
        if action_id == "finance.quote":
            symbol = str(params.get("symbol") or params.get("query") or params.get("q") or "")
            return _wrap(action_id, finance_quote(symbol, fetch_json=fetch_json))
        if action_id == "finance.fx":
            return _wrap(action_id, finance_fx(str(params.get("base") or params.get("query") or "USD"), fetch_json=fetch_json))
        if action_id == "search.web":
            query = str(params.get("query") or params.get("q") or params.get("symbol") or "")
            return _wrap(action_id, search_web(query, fetch_text=fetch_text))
        if action_id == "images.generate":
            return make_result(
                action_id, "skipped", error_kind="skipped",
                error="sandbox skips paid image generation", failure_kind="skipped",
            )
        if action_id == "polls.create":
            # 플래너 프롬프트는 `options`, 옛 출력·Discord 용어는 `answers`. 둘 다 받는다.
            options = params.get("options")
            if not isinstance(options, list):
                options = params.get("answers")
            if not isinstance(options, list):
                options = []
            hours = _int_param(params.get("duration_hours"), 24)
            if hours is None:
                return needs_input_result(action_id, ["duration_hours"], error_kind="invalid_param")
            return _wrap(
                action_id,
                polls_create(
                    Path(data_dir or "."),
                    str(params.get("question") or ""),
                    [str(x) for x in options],
                    duration_hours=hours,
                    allow_multiselect=bool(params.get("allow_multiselect")),
                ),
            )
        if action_id == "runtime.doctor":
            meta = dict(index_meta or {})
            return ok_result(
                action_id,
                {
                    "ok": True,
                    "surface": "test-sandbox",
                    "leaf_count": meta.get("leaf_count"),
                    "message_count": meta.get("message_count"),
                },
            )
        if action_id == "help.version":
            return _wrap(action_id, help_version())
        return make_result(action_id or name, "unknown_tool", error_kind="unknown_tool", error="unknown tool")
    except (ToolError, OSError, TimeoutError, urllib.error.URLError, ValueError, json.JSONDecodeError, KeyError, IndexError, TypeError) as exc:
        return error_result_from_exception(action_id or name, exc)


DEFAULT_ENABLED = default_enabled_ids()
