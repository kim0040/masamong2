"""Lite tool planner using masamong2 Action IDs (legacy aliases still parse).

Output contract (기술계획서 §15.2, upgrade 05): `memory_requirement` decides whether the
channel index is searched at all. 잡담·감탄·인사는 `none` → 과거 검색 0회.
"""

from __future__ import annotations

import json
from typing import Any, Mapping

from tool_parity import canonicalize_tool

MEMORY_REQUIREMENTS = ("none", "optional", "required")
TIME_SCOPES = ("recent", "any")
QUESTION_KINDS = ("person_intro", "person_fact", "event", "casual", "other")
# v2 (contract planner_output.time_kind). `time` narrows the search universe; never a score bonus.
TIME_KINDS = ("any", "recent", "today", "yesterday", "last_week", "this_month", "before", "after", "between")
MAX_SEMANTIC_QUERY_CHARS = 300
MAX_KEY_TERMS = 12
MAX_UNRESOLVED = 6
MAX_UNRESOLVED_CHARS = 80
# strict parse states (parse_planner_output). "ok" is the only state whose tool_plan is trusted.
PARSE_STATES = ("ok", "truncated", "invalid", "empty")

# ---------------------------------------------------------------------------
# Prompt pieces. `PLAN_PROMPT` (single user message, `{message}` placeholder) is the legacy
# assembly the engine still formats directly. planner_context.build_planner_messages composes the
# same pieces into a system message + a data-only user message (history / summary / q0).
# ---------------------------------------------------------------------------

PLAN_OUTPUT_SCHEMA = """{"memory_requirement":"none|optional|required",
 "time_scope":"recent|any",
 "time":{"kind":"any|recent|today|yesterday|last_week|this_month|before|after|between","from":null,"to":null},
 "question_kind":"person_intro|person_fact|event|casual|other",
 "person_targets":["질문이 가리키는 사람 이름·별명(채팅에 쓰인 표기 그대로)"],
 "semantic_query":"질문의 뜻을 검색용 서술문으로 풀어 쓴 한 문장(선택)",
 "key_terms":["핵심 명사·고유명사·제품명(선택, 최대 12개)"],
 "unresolved":["확정하지 못한 지시 대상(선택, 예: region: 후보 2곳)"],
 "sensitive":false,
 "tool_plan":[{"tool_name":"...","parameters":{...}}]}"""

PLAN_FIELD_RULES = """time_scope: "최근/요즘/어제/이번주/지금" 등 시점이 최근으로 한정되면 recent, 아니면 any.
time.kind: 시점을 더 정확히 알 수 있으면 today/yesterday/last_week/this_month, 특정 날짜 전후면 before/after/between(from/to는 YYYY-MM-DD). 모르면 time_scope와 같게(recent 또는 any). 기준 시각이 주어지지 않았으면 from/to를 비워라.
semantic_query: 기억이 필요할 때만. 질문을 "…에 대해 말한 내용"처럼 대화 조각을 찾는 서술문으로. 원문을 그대로 복사하지 마라.
key_terms: 검색에 쓸 핵심어만(이름 제외 가능). 조사 없이.
question_kind: 사람 소개·어떤 사람인지(person_intro), 특정 사람이 한 말·행동(person_fact), 사건·약속·결정(event), 잡담·감탄·인사(casual), 그 외(other).
person_targets: 질문에 등장하는 사람 이름·별명만. 없으면 []. 사람이 아닌 일반 명사·대명사는 넣지 마라.
unresolved: "거기/그때/걔"처럼 최근 대화로 풀어야 하는 지시 대상이 두 개 이상 후보가 있거나 대화에 없으면 임의로 고르지 말고 여기에 적어라(없으면 생략). 이때 해당 파라미터·person_targets·from/to는 비운다.
sensitive: 정치 성향·종교·건강·연애·돈처럼 근거 없이 추측하면 안 되는 주제면 true.

memory_requirement (이 채널의 과거 대화 기억이 필요한가):
- none: 잡담·감탄·인사·욕·반응(ㅋㅋ, 안녕, 짜증나), 봇 자체에 대한 일반 질문, 날씨·시세·환율·뉴스처럼 도구만으로 답할 것. 과거 검색을 하지 않는다.
- optional: 현재 대화로 답할 수 있지만 예전 얘기가 있으면 더 좋은 경우.
- required: 사람·별명·예전 대화·약속·"그때/저번에/요즘 누가"처럼 기억 없이는 답할 수 없는 질문."""

# Ordered: rendering order == product catalog order. Keys are catalog Action IDs.
TOOL_DESCRIPTIONS: dict[str, str] = {
    "memory.search": "이 채널 기억(사람·예전 대화·별명). parameters.query 에 검색용으로 다듬은 짧은 질의(이름·핵심어)를 넣어라.",
    "weather.query": "날씨. parameters.region(모르면 생략), day_offset(0오늘/1내일/2모레)",
    "disasters.query": "특보·지진",
    "finance.quote": "주식 현재가. parameters.symbol",
    "finance.fx": "환율. parameters.base",
    "search.web": "인터넷 검색(뉴스·최신 소식·사실 확인). parameters.query, fresh(day/week/month, 최근 것만 필요할 때)",
    "images.generate": "그림. 샌드박스는 생성하지 않음",
    "polls.create": "Discord 투표. parameters.question (최대 300), options 2–10개 각 55자, duration_hours 1–768(기본 24), allow_multiselect. options를 비우면 예/아니요.",
    "help.version": "명령어·도움말 요청, 봇이 할 수 있는 일을 물을 때",
}

PLAN_GENERAL_RULES = """규칙:
1. 기억/사람/예전 얘기면 memory_requirement required + memory.search.
2. 날씨나 우산·옷차림처럼 날씨에 달린 질문이면 weather.query. 뉴스/오늘 이슈/최신 정보, 지금 그 자리에 누가 있는지·순위·가격처럼 시간이 지나면 바뀌는 사실이면 search.web. 주가면 finance.quote, 환율이면 finance.fx. 투표 만들어 달라면 polls.create. 봇이 뭘 할 수 있는지·명령어를 물으면 help.version.
3. 잡담·감탄이면 memory_requirement none, tool_plan [] (빈 배열). memory.search를 넣지 마라.
4. 필요하면 여러 개를 나열. 보통 0~2개.
5. 사용자 위치·이름 같은 값을 모르면 "사용자 지역" 같은 자리표시자를 만들지 말고 그 파라미터를 생략하라.
6. 위 목록에 없는 도구는 tool_plan에 넣지 마라. 방·채널·계정 ID, 시간대, 허용 범위 같은 값을 파라미터로 지어내지 마라.
7. JSON만."""


def render_tool_section(tool_ids: list[str] | None) -> str:
    """Tool description block for the planner. None → full catalog; [] → no external tools."""

    if tool_ids is None:
        ids = list(TOOL_DESCRIPTIONS)
    else:
        seen: list[str] = []
        for raw in tool_ids:
            name = canonicalize_tool(str(raw or ""))
            if name in TOOL_DESCRIPTIONS and name not in seen:
                seen.append(name)
        ids = seen
    if not ids:
        return "도구: 지금 사용할 수 있는 도구가 없다. tool_plan은 [] 로 두어라. memory_requirement는 그래도 판단하라."
    lines = ["도구 (제품 Action ID와 동일):"]
    for name in ids:
        lines.append(f"- {name}: {TOOL_DESCRIPTIONS[name]}")
    if "memory.search" not in ids:
        lines.append("(이 채널의 기억 검색은 꺼져 있다. memory_requirement는 그래도 판단하되 memory.search를 tool_plan에 넣지 마라.)")
    return "\n".join(lines)


PLAN_HEADER = "너는 마사몽 의도 분석기다. JSON만 출력하라.\n" + PLAN_OUTPUT_SCHEMA + "\n\n" + PLAN_FIELD_RULES


def _brace_escape(text: str) -> str:
    return text.replace("{", "{{").replace("}", "}}")


PLAN_PROMPT = (
    _brace_escape(PLAN_HEADER)
    + "\n\n"
    + _brace_escape(render_tool_section(None))
    + "\n\n"
    + _brace_escape(PLAN_GENERAL_RULES)
    + "\n\n사용자 메시지:\n{message}\n"
)

_EXTERNAL = {
    "weather.query",
    "disasters.query",
    "finance.quote",
    "finance.fx",
    "search.web",
    "images.generate",
    "polls.create",
    "runtime.doctor",
    "help.version",
}

_FALLBACK_STEP = {"tool_name": "memory.search", "parameters": {}}


def _extract_json_state(raw: str) -> tuple[dict[str, Any] | None, str]:
    """(object, state). state ∈ PARSE_STATES: empty (no text), truncated (an object was opened but
    never closed / cut mid-way), invalid (text but no usable object), ok."""

    text = (raw or "").strip()
    if not text:
        return None, "empty"
    if "```" in text:
        parts = text.split("```")
        text = (parts[1] if len(parts) > 1 else parts[0]).strip()
        if text.lower().startswith("json"):
            text = text[4:].strip()
    start = text.find("{")
    end = text.rfind("}")
    if start < 0:
        return None, "invalid"
    if end < start:
        return None, "truncated"
    try:
        obj = json.loads(text[start : end + 1])
    except json.JSONDecodeError:
        depth = 0
        for ch in text[start:]:
            if ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
        return None, "truncated" if depth > 0 else "invalid"
    return (obj, "ok") if isinstance(obj, dict) else (None, "invalid")


def _extract_json(raw: str) -> dict[str, Any] | None:
    return _extract_json_state(raw)[0]


def _salvage_requirement(raw: str) -> str | None:
    """memory_requirement from a truncated response. It is the first, closed-enum field, so a
    literal match is safe; nothing else is salvaged (tool arguments may be cut mid-way)."""

    text = raw or ""
    key = text.find('"memory_requirement"')
    if key < 0:
        return None
    tail = text[key + len('"memory_requirement"') : key + 60]
    for value in MEMORY_REQUIREMENTS:
        if f'"{value}"' in tail:
            return value
    return None


def _steps(plan: Any) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    if not isinstance(plan, list):
        return out
    for item in plan:
        if not isinstance(item, dict):
            continue
        name = canonicalize_tool(str(item.get("tool_name") or item.get("name") or "").strip())
        if not name:
            continue
        params = item.get("parameters") if isinstance(item.get("parameters"), dict) else {}
        out.append({"tool_name": name, "parameters": params})
    return out


def intent_from_plan(parsed: Mapping[str, Any] | None) -> dict[str, Any] | None:
    """Retrieval intent from planner output. None when the planner emitted no intent fields
    (legacy output) so the engine falls back to its regex heuristics."""

    if not parsed or not parsed.get("parsed"):
        return None
    scope = parsed.get("time_scope")
    kind = parsed.get("question_kind")
    if (
        scope is None
        and kind is None
        and parsed.get("person_targets") is None
        and parsed.get("unresolved") is None
        and parsed.get("time") is None
        and parsed.get("semantic_query") is None
        and parsed.get("key_terms") is None
    ):
        return None
    sensitive = bool(parsed.get("sensitive"))
    time = parsed.get("time") if isinstance(parsed.get("time"), dict) else None
    if time is None:
        time = {"kind": "recent" if scope == "recent" else "any", "from": None, "to": None}
    return {
        "recent": scope == "recent" or time.get("kind") == "recent",
        "intro": kind == "person_intro",
        "politics": sensitive,
        "prefer_missing_over_guess": sensitive,
        "kind": kind or "other",
        "targets": list(parsed.get("person_targets") or []),
        "time": time,
        "semantic_query": str(parsed.get("semantic_query") or ""),
        "key_terms": list(parsed.get("key_terms") or []),
        "memory_search_query": memory_query(list(parsed.get("tool_plan") or [])),
        "unresolved": list(parsed.get("unresolved") or []),
        "source": "planner",
    }


def _parse_time(obj: Any, scope: str | None) -> dict[str, Any] | None:
    """`time` object → {kind, from, to}. Missing → derived from time_scope; bad kind → derived."""

    kind = None
    frm = to = None
    if isinstance(obj, dict):
        raw_kind = str(obj.get("kind") or "").strip().lower()
        kind = raw_kind if raw_kind in TIME_KINDS else None
        for key in ("from", "to"):
            val = obj.get(key)
            if isinstance(val, str) and 4 <= len(val.strip()) <= 40:
                if key == "from":
                    frm = val.strip()
                else:
                    to = val.strip()
    if kind is None:
        if scope is None and not isinstance(obj, dict):
            return None
        kind = "recent" if scope == "recent" else "any"
    if kind not in {"before", "after", "between"}:
        frm = to = None
    return {"kind": kind, "from": frm, "to": to}


def _str_list(value: Any, *, max_items: int, max_chars: int) -> list[str] | None:
    if not isinstance(value, list):
        return None
    out: list[str] = []
    for item in value:
        text = str(item or "").strip()
        if 1 <= len(text) <= max_chars and text not in out and len(out) < max_items:
            out.append(text)
    return out


def _unparsed(state: str, *, strict: bool, requirement: str | None = None) -> dict[str, Any]:
    steps: list[dict[str, Any]] = []
    if not strict:
        steps = [dict(_FALLBACK_STEP)]
    elif requirement == "required":
        steps = [dict(_FALLBACK_STEP)]
    return {
        "memory_requirement": requirement,
        "tool_plan": steps,
        "parsed": False,
        "parse_state": state,
        "time_scope": None,
        "question_kind": None,
        "person_targets": None,
        "sensitive": None,
        "time": None,
        "semantic_query": None,
        "key_terms": None,
        "unresolved": None,
    }


def parse_plan(raw: str) -> dict[str, Any]:
    """Return {"memory_requirement": str|None, "tool_plan": [...], "parsed": bool, ...}.

    `memory_requirement` is None when the model did not emit one (legacy output); callers
    then fall back to the tool-name heuristic in `needs_local_rag`. Unparseable output
    degrades to a single memory.search step so the bot still answers (legacy behaviour;
    `parse_planner_output` is the strict variant that does not widen memory access).
    """

    return _parse(raw, strict=False)


def parse_planner_output(raw: str) -> dict[str, Any]:
    """Strict parse for the context-aware planner.

    Same fields as `parse_plan` plus `parse_state` (ok|truncated|invalid|empty). A damaged
    response never turns into an implicit memory.search: tool_plan is [] unless the response
    literally said memory_requirement=required before it was cut (then the single
    memory.search step is kept, as for a complete `required`). Callers map parsed=False to a
    planner-failure state instead of searching everything (planner_context.assess_plan).
    """

    return _parse(raw, strict=True)


def _parse(raw: str, *, strict: bool) -> dict[str, Any]:
    obj, state = _extract_json_state(raw)
    if obj is None:
        salvaged = _salvage_requirement(raw) if state == "truncated" else None
        return _unparsed(state, strict=strict, requirement=salvaged)
    req_raw = str(obj.get("memory_requirement") or "").strip().lower()
    requirement = req_raw if req_raw in MEMORY_REQUIREMENTS else None
    scope_raw = str(obj.get("time_scope") or "").strip().lower()
    kind_raw = str(obj.get("question_kind") or "").strip().lower()
    targets_raw = obj.get("person_targets")
    targets = None
    if isinstance(targets_raw, list):
        targets = []
        for t in targets_raw:
            name = str(t or "").strip()
            if 1 <= len(name) <= 40 and name not in targets:
                targets.append(name)
    steps = _steps(obj.get("tool_plan"))
    if not steps and requirement is None:
        steps = [dict(_FALLBACK_STEP)]
    if requirement == "required" and not any(s["tool_name"] == "memory.search" for s in steps):
        steps.append(dict(_FALLBACK_STEP))
    scope = scope_raw if scope_raw in TIME_SCOPES else None
    time = _parse_time(obj.get("time"), scope)
    if time is not None and time["kind"] == "recent":
        scope = "recent"  # v2 time wins; keep v1 consumers consistent
    sem = obj.get("semantic_query")
    semantic_query = str(sem).strip()[:MAX_SEMANTIC_QUERY_CHARS] if isinstance(sem, str) else None
    key_terms = None
    if isinstance(obj.get("key_terms"), list):
        key_terms = []
        for t in obj["key_terms"]:
            term = str(t or "").strip()
            if 1 <= len(term) <= 40 and term not in key_terms and len(key_terms) < MAX_KEY_TERMS:
                key_terms.append(term)
    return {
        "memory_requirement": requirement,
        "tool_plan": steps,
        "parsed": True,
        "parse_state": "ok",
        "time_scope": scope,
        "question_kind": kind_raw if kind_raw in QUESTION_KINDS else None,
        "person_targets": targets,
        "sensitive": bool(obj.get("sensitive")) if "sensitive" in obj else None,
        "time": time,
        "semantic_query": semantic_query,
        "key_terms": key_terms,
        # v3 (sandbox-only until 08 lands it in the shared contract): what the planner could not
        # resolve from the visible context. None when the field is absent (v1/v2 output).
        "unresolved": _str_list(obj.get("unresolved"), max_items=MAX_UNRESOLVED, max_chars=MAX_UNRESOLVED_CHARS),
    }


def parse_tool_plan(raw: str) -> list[dict[str, Any]]:
    """Legacy shape: tool steps only. Empty/invalid → memory.search fallback."""

    parsed = parse_plan(raw)
    return parsed["tool_plan"] or [dict(_FALLBACK_STEP)]


def memory_query(plan: list[dict[str, Any]]) -> str:
    """Planner-refined retrieval query from the memory.search step, or "" if absent."""

    for step in plan:
        if canonicalize_tool(str(step.get("tool_name") or "")) != "memory.search":
            continue
        params = step.get("parameters") or {}
        q = str(params.get("query") or params.get("q") or "").strip()
        if 2 <= len(q) <= 200:
            return q
    return ""


def needs_local_rag(plan: list[dict[str, Any]], memory_requirement: str | None = None) -> bool:
    names = {canonicalize_tool(str(p.get("tool_name"))) for p in plan}
    if memory_requirement == "none":
        return False
    if memory_requirement == "required":
        return True
    if memory_requirement == "optional":
        return "memory.search" in names
    # legacy heuristic (no requirement emitted)
    if not names or names <= {"help.version"} or names <= {"general_chat"}:
        return False
    if "memory.search" in names:
        return True
    if names <= _EXTERNAL:
        return False
    return True
