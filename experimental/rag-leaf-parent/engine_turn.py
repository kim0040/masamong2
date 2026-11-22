"""Sandbox engine, turn half: prompt assembly, sessions, chat() and chat_stream() (re-exported by engine.py)."""

from __future__ import annotations

import json
import os
import time
from datetime import datetime, timezone
from typing import Any, Mapping

import engine_index
from timeparse import normalize_iso
from engine_index import (
    IndexBundle,
    PlaygroundError,
    _compact_timing,
    _elapsed_ms,
    apply_client_usage,
    configured_models,
    load_config,
    load_ledger,
    make_client,
    prepare_status_payload,
    remaining_session_budget,
)
from engine_query import resolve_intent
from engine_retrieve import (
    prefetch_query_vector,
    retrieve,
)

from client import (  # noqa: E402
    ClientError,
    PlayClient,
    StreamError,
)
from retrieval_post import TimeReference as RetrievalTimeReference  # noqa: E402
from rag_upgrade.playground_debug import (  # noqa: E402
    RankingError,
    append_debug_error,
    append_debug_turn,
    new_debug_id,
    normalize_ranking,
)
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
from bot_tools import (  # noqa: E402
    execute_named_tool,
    is_success,
)
from turn_diagnosis import build_diagnosis  # noqa: E402
from tool_parity import (  # noqa: E402
    canonicalize_tool,
    parse_bang_command,
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
            "data_dir": engine_index.context().paths.data.name,
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
        now = datetime.fromisoformat(normalize_iso(str(raw["now"])))
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
        engine_index.context().paths.data, cfg=index.cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
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
                data_dir=engine_index.context().paths.data,
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
        engine_index.context().paths.data, cfg=index.cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
    )
    channel_id = str(runtime.get("active_channel_id") or channel_id_from_room(_indexed_rooms(index)[0]))
    conv = get_conversation(engine_index.context().paths.data, channel_id, conversation_id)
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
        put_conversation(engine_index.context().paths.data, channel_id, conv)
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
        engine_index.context().paths.data,
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


def _check_turn(cfg: dict[str, Any], message: str | None, ranking: str | None) -> tuple[str, float, str]:
    """Both transports refuse the same requests the same way: (message, remaining session budget, ranking mode)."""

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
    return message, remaining, rank_mode


def _models_block(cfg: dict[str, Any], client: PlayClient, rank_trace: dict[str, Any]) -> dict[str, Any]:
    return {
        "response": client.llm_model,
        "planner": configured_models(cfg)["planner"],
        "summary": configured_models(cfg)["summary"],
        "embed": client.embed_model,
        "rerank": rank_trace.get("rerank_model")
        or rank_trace.get("rerank_model_requested")
        or configured_models(cfg)["rerank"],
    }


def _intent_of(bundle: dict[str, Any] | None, message: str, cfg: dict[str, Any]) -> dict[str, Any]:
    return (bundle or {}).get("intent") or resolve_intent(message, None, cfg)


def _error_row(
    index: IndexBundle,
    client: PlayClient,
    bundle: dict[str, Any] | None,
    *,
    path: str,
    rank_trace: dict[str, Any],
    hits: list[dict[str, Any]],
    session_id: str,
    message: str,
    speaker: str | None,
    answer: str | None,
    partial_chars: int | None,
) -> dict[str, Any]:
    """The debug row for a turn that failed. `partial_chars` is only known on the stream path."""

    cfg = index.cfg
    extra = {
        "path": path,
        **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=None, answer=answer),
        "intent": _intent_of(bundle, message, cfg),
        "rank_trace": rank_trace,
        "plan": (bundle or {}).get("plan"),
        "plan_raw": (bundle or {}).get("plan_raw"),
        "tools": (bundle or {}).get("tool_results"),
        "channel_id": (bundle or {}).get("channel_id"),
        "speaker": speaker,
        "usage": client.usage.as_dict(),
        "config": _config_snap(cfg),
    }
    if partial_chars is not None:
        extra["partial_chars"] = partial_chars
    return extra


def _turn_row(
    index: IndexBundle,
    client: PlayClient,
    bundle: dict[str, Any],
    *,
    path: str,
    rank_trace: dict[str, Any],
    hits: list[dict[str, Any]],
    session_id: str,
    message: str,
    speaker: str | None,
    answer: str,
    spent: float,
    timing: dict[str, float],
    messages: list[dict[str, str]],
    history: list[dict[str, str]],
) -> dict[str, Any]:
    """The debug row for a turn that answered."""

    cfg = index.cfg
    return {
        "path": path,
        **_log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer),
        "intent": _intent_of(bundle, message, cfg),
        "rank_trace": rank_trace,
        "spent_usd": spent,
        "answer": answer,
        "answer_preview": answer[:400],
        "answer_chars": len(answer),
        "plan": bundle.get("plan"),
        "plan_raw": bundle.get("plan_raw"),
        "tools": bundle.get("tool_results"),
        "channel_id": bundle.get("channel_id"),
        "speaker": speaker,
        "timing": timing,
        "usage": client.usage.as_dict(),
        "models": _models_block(cfg, client, rank_trace),
        "prompt": _prompt_stats(messages),
        "history": _history_stats(history, keep=int(cfg.get("max_history_turns") or 0)),
        "config": _config_snap(cfg),
        "ranking_applied": rank_trace.get("applied"),
        "rag_used": bundle.get("rag_used"),
        "rag_state": bundle.get("rag_state"),
        "memory_requirement": bundle.get("memory_requirement"),
        "search_query": bundle.get("search_query"),
    }


def _result_payload(
    index: IndexBundle,
    client: PlayClient,
    bundle: dict[str, Any],
    *,
    rank_trace: dict[str, Any],
    hits: list[dict[str, Any]],
    session_id: str,
    session_summary: bool | None,
    message: str,
    rank_mode: str,
    answer: str,
    spent: float,
    new_total: float,
    debug_id: str,
    timing: dict[str, float],
) -> dict[str, Any]:
    """What the caller gets back. `session_summary` is the chat() field the stream's `done` event never had."""

    cfg = index.cfg
    payload: dict[str, Any] = {
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
        "intent": _intent_of(bundle, message, cfg),
        "plan": bundle.get("plan"),
        "memory_requirement": bundle.get("memory_requirement"),
        "rag_state": bundle.get("rag_state"),
        "search_query": bundle.get("search_query"),
        "tools": bundle.get("tool_results"),
        "channel_id": bundle.get("channel_id"),
        "conversation_id": session_id,
    }
    if session_summary is not None:
        payload["session_summary"] = session_summary
    payload.update(
        {
            "timing": timing,
            "diagnosis": _log_context(index, bundle, rank_trace, hits, conversation_id=session_id, timing=timing, answer=answer).get("diagnosis"),
            "server_run_id": SERVER_RUN_ID,
            "usage_detail": client.usage.as_dict(),
            "models": _models_block(cfg, client, rank_trace),
        }
    )
    return payload


def _stream_answer(client: PlayClient, messages: list[dict[str, str]], parts: list[str]):
    """Relay the model's stream as `delta` events and collect the text in `parts`."""

    for piece in client.chat_stream(messages, max_tokens=500, temperature=0.2, reasoning="low"):
        if piece.get("type") == "delta":
            text = str(piece.get("text") or "")
            parts.append(text)
            yield {"event": "delta", "text": text}
        elif piece.get("type") == "done":
            break
        elif piece.get("type") == "error":
            raise StreamError("stream error payload", status="provider_error", partial_chars=sum(len(p) for p in parts))


def _persist_turn(
    index: IndexBundle,
    client: PlayClient,
    bundle: dict[str, Any],
    *,
    session_channel: str,
    session_id: str,
    message: str,
    answer: str,
    speaker: str | None,
    debug_id: str,
) -> str:
    """Append the turn to its saved conversation; returns the conversation id to report."""

    saved = _persist_session(
        index,
        channel_id=session_channel or str(bundle.get("channel_id") or ""),
        conversation_id=session_id,
        user=message,
        assistant=answer,
        speaker=str(speaker or ""),
        client=client,
        debug_id=debug_id,
    )
    return str(saved.get("id") or session_id)


def _run_turn(
    index: IndexBundle,
    *,
    message: str,
    history: list[dict[str, str]] | None,
    ranking: str | None,
    speaker: str | None,
    conversation_id: str | None,
    persist: bool,
    stream: bool,
):
    """One chat turn for both transports.

    `stream=True` yields the SSE events (status, debug, hits, tools, delta, then done or error) and returns None.
    `stream=False` yields nothing and returns the result payload. The two keep the order each always had where
    they differ: the stream saves the conversation before it writes the debug row, chat() after.
    """

    cfg = index.cfg
    message, remaining, rank_mode = _check_turn(cfg, message, ranking)

    if stream:
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
    stream_failed: StreamError | None = None
    try:
        if stream:
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
        if stream:
            yield {
                "event": "debug",
                "debug_id": None,
                "ranking": rank_mode,
                "ranking_applied": rank_trace.get("applied"),
                "intent": _intent_of(bundle, message, cfg),
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
        if stream:
            yield from _stream_answer(client, messages, parts)
        else:
            parts.append(client.chat(messages, max_tokens=500, temperature=0.2, reasoning="low") or "")
        generate_ms = _elapsed_ms(t_gen)
    except (ClientError, PlaygroundError) as exc:
        # a cut stream is reported to the caller as an `error` event; every other failure is raised
        cut = stream and isinstance(exc, StreamError)
        append_debug_error(
            engine_index.context().paths.data,
            query=message,
            ranking=rank_mode,
            error=exc,
            extra=_error_row(
                index,
                client,
                bundle,
                path="stream" if stream else "chat",
                rank_trace=rank_trace,
                hits=hits,
                session_id=session_id,
                message=message,
                speaker=speaker,
                answer="".join(parts) if stream else None,
                partial_chars=(exc.partial_chars if cut else sum(len(p) for p in parts)) if stream else None,
            ),
        )
        if cut:
            stream_failed = exc
        else:
            apply_client_usage(client, extra_usd=float(rank_trace.get("rerank_cost_usd") or 0.0))
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
        return None

    timing = _compact_timing((bundle or {}).get("timing") or {}, rank_trace, {"generate_ms": generate_ms})
    answer_text = "".join(parts).strip()
    row = dict(
        path="stream" if stream else "chat",
        rank_trace=rank_trace,
        hits=hits,
        message=message,
        speaker=speaker,
        answer=answer_text,
        spent=spent,
        timing=timing,
        messages=messages,
        history=history,
    )
    if stream:
        debug_id = new_debug_id()
        if persist and session_id:
            session_id = _persist_turn(
                index, client, bundle, session_channel=session_channel, session_id=session_id,
                message=message, answer=answer_text, speaker=speaker, debug_id=debug_id,
            )
        append_debug_turn(
            engine_index.context().paths.data,
            debug_id=debug_id,
            query=message,
            ranking=rank_mode,
            hits=hits,
            extra=_turn_row(index, client, bundle, session_id=session_id, **row),
        )
    else:
        debug_id = append_debug_turn(
            engine_index.context().paths.data,
            query=message,
            ranking=rank_mode,
            hits=hits,
            extra=_turn_row(index, client, bundle, session_id=session_id, **row),
        )
        if persist and session_id:
            session_id = _persist_turn(
                index, client, bundle, session_channel=session_channel, session_id=session_id,
                message=message, answer=answer_text, speaker=speaker, debug_id=debug_id,
            )
    payload = _result_payload(
        index,
        client,
        bundle,
        rank_trace=rank_trace,
        hits=hits,
        session_id=session_id,
        session_summary=None if stream else bool(session_summary),
        message=message,
        rank_mode=rank_mode,
        answer=answer_text,
        spent=spent,
        new_total=new_total,
        debug_id=debug_id,
        timing=timing,
    )
    if stream:
        yield {"event": "done", **payload}
        return None
    return payload


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
    turn = _run_turn(
        index,
        message=message,
        history=history,
        ranking=ranking,
        speaker=speaker,
        conversation_id=conversation_id,
        persist=persist,
        stream=False,
    )
    try:
        while True:
            next(turn)
    except StopIteration as done:
        return done.value


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

    yield from _run_turn(
        index,
        message=message,
        history=history,
        ranking=ranking,
        speaker=speaker,
        conversation_id=conversation_id,
        persist=persist,
        stream=True,
    )


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
        "debug_turns": str(engine_index.context().paths.data / "debug_turns.jsonl"),
        "debug_dumps": str(engine_index.context().paths.data / "debug_dumps"),
        "prepare": prepare_status_payload(),
        "run": {
            "server_run_id": SERVER_RUN_ID,
            "dataset": cfg.get("experiment"),
            "collection": cfg.get("collection"),
            "data_dir": engine_index.context().paths.data.name,
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
        engine_index.context().paths.data, cfg=cfg, indexed_rooms=_indexed_rooms(index), blocks=index.blocks
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
