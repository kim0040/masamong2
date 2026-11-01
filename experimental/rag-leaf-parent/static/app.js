(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const thread = $("thread");
  const welcome = $("welcome");
  const form = $("composer");
  const input = $("composer-input");
  const sendBtn = $("send-btn");
  const live = $("live-status");
  const formError = $("form-error");
  const statWindows = $("stat-windows");
  const statBudget = $("stat-budget");
  const statRun = $("stat-run");
  const prepareBanner = $("prepare-banner");
  const prepareLabel = $("prepare-label");
  const prepareBar = $("prepare-bar");
  const rankingSelect = $("ranking-select");
  const channelList = $("channel-list");
  const channelHeading = $("channel-heading");
  const channelNote = $("channel-note");
  const datasetBadge = $("dataset-badge");
  const personaText = $("persona-text");
  const rulesText = $("rules-text");
  const respondMode = $("respond-mode");
  const savePersona = $("save-persona");
  const personaStatus = $("persona-status");
  const speakerInput = $("speaker-input");
  const conversationList = $("conversation-list");
  const newChatBtn = $("new-chat-btn");
  const runStats = $("run-stats");
  const runIdEl = $("run-id");

  const RANKING_SHORT = { auto: "자동", hybrid: "R0", heuristic: "R1", rerank: "R2 재순위", cohere: "R2 재순위" };
  const STAGES = [
    ["plan_ms", "계획", "--seg-plan"],
    ["tools_ms", "도구", "--seg-tools"],
    ["embed_ms", "임베딩", "--seg-embed"],
    ["hybrid_ms", "검색", "--seg-search"],
    ["rerank_ms", "재순위", "--seg-rerank"],
    ["expand_ms", "원문", "--seg-expand"],
    ["generate_ms", "작성", "--seg-write"],
  ];
  // 틀림 이유: 로그(debug_report.py)의 tags 코드. 코퍼스와 무관한 테스트 분류다.
  const WRONG_TAGS = [
    ["wrong_person", "사람이 틀림"],
    ["wrong_time", "시점이 틀림"],
    ["hallucination", "없는 내용을 지어냄"],
    ["should_abstain", "모른다고 했어야 함"],
    ["missed", "있는데 못 찾음"],
    ["wrong_evidence", "근거가 엉뚱함"],
    ["partial", "일부만 맞음"],
    ["tool", "도구 문제"],
    ["style", "말투·형식"],
    ["slow", "너무 느림"],
  ];
  // 입력 틀. [괄호]는 사용자가 바꾸는 자리이며 특정 데이터셋의 이름을 넣지 않는다.
  const TEMPLATES = [
    {
      title: "인물",
      note: "표시명이 바뀐 사람, 이름이 비슷한 두 사람으로도 해 보세요. 진단의 ‘인물’ 칸을 봅니다.",
      items: ["[이름]이 [주제]에 대해 뭐라고 했어?", "[이름]은 요즘 뭐 한대?", "[이름]이랑 [이름]이 같이 얘기한 거 있어?"],
    },
    {
      title: "없는 내용 (모른다고 해야 정상)",
      note: "대화에 없던 걸 물어 보세요. ‘근거 없음’이면 거절해야 하고, 근거가 붙었으면 점수가 하한 근처인지 봅니다.",
      items: ["우리 [한 번도 안 한 얘기] 얘기한 적 있어?", "[이름]이 [없는 일] 했다고 했지?"],
    },
    {
      title: "시간",
      note: "막연한 ‘요즘’과 날짜를 짚은 질문을 비교합니다. 진단의 ‘시간 창’을 봅니다.",
      items: ["요즘 무슨 얘기 많이 해?", "최근에 [주제] 얘기 나온 적 있어?", "[YYYY년 M월]쯤에 무슨 일 있었어?"],
    },
    {
      title: "속도·반복",
      note: "같은 질문을 다시 보내면 임베딩이 캐시로 0초여야 합니다.",
      items: ["[아무 질문]"],
    },
  ];

  /** @type {{role: string, content: string}[]} */
  const history = [];
  let busy = false;
  let conversationId = "";
  let activeChannelId = "";
  let knownChannels = [];
  let labInventory = null;

  const store = {
    get(key) {
      try { return localStorage.getItem(key); } catch { return null; }
    },
    set(key, value) {
      try { localStorage.setItem(key, value); } catch { /* 저장 불가: 무시 */ }
    },
  };

  if (window.marked) {
    marked.setOptions({ gfm: true, breaks: true, headerIds: false, mangle: false });
  }

  // ---------- 공용 ----------

  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value == null || value === false) continue;
        if (key === "class") node.className = value;
        else if (key === "text") node.textContent = value;
        else if (key === "html") node.innerHTML = value;
        else if (key.startsWith("on") && typeof value === "function") node.addEventListener(key.slice(2), value);
        else if (key === "style") node.setAttribute("style", value);
        else node.setAttribute(key, value === true ? "" : String(value));
      }
    }
    for (const child of children.flat()) {
      if (child == null || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  function tag(text, kind, title) {
    return el("span", { class: `tag${kind ? " " + kind : ""}`, title: title || null }, text);
  }

  function sec(ms) {
    const n = Number(ms);
    if (!Number.isFinite(n)) return "—";
    return n >= 10000 ? `${(n / 1000).toFixed(0)}초` : `${(n / 1000).toFixed(1)}초`;
  }

  function money(value) {
    const n = Number(value);
    return Number.isFinite(n) ? `$${n.toFixed(4)}` : "—";
  }

  function escapeHtml(text) {
    return String(text).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  function renderMarkdown(text) {
    const raw = String(text || "");
    if (!raw) return "";
    if (window.marked && window.DOMPurify) {
      return DOMPurify.sanitize(marked.parse(raw), { USE_PROFILES: { html: true } });
    }
    return escapeHtml(raw).replace(/\n/g, "<br>");
  }

  function scrollToEnd() {
    thread.scrollTop = thread.scrollHeight;
  }

  let reporting = false;
  function reportClientError(detail) {
    // 서버 로그에 남지 않는 화면 쪽 실패(스트림 끊김·fetch 실패·JS 예외)를 debug_turns에 남긴다.
    if (reporting) return;
    reporting = true;
    fetch("/api/debug/client", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...detail, conversation_id: conversationId || null, url: location.pathname }),
    })
      .catch(() => {})
      .finally(() => {
        reporting = false;
      });
  }

  window.addEventListener("error", (ev) => {
    reportClientError({ type: "js_error", message: String(ev.message || ""), where: `${ev.filename || ""}:${ev.lineno || ""}` });
  });
  window.addEventListener("unhandledrejection", (ev) => {
    const reason = ev.reason;
    reportClientError({ type: "unhandled_rejection", message: String((reason && reason.message) || reason || ""), where: "promise" });
  });

  async function getJson(url, options) {
    const res = await fetch(url, options);
    let data = {};
    try {
      data = await res.json();
    } catch {
      /* 본문 없음 */
    }
    if (!res.ok) {
      const message = data.error || `요청 실패 (HTTP ${res.status})`;
      reportClientError({ type: "http_error", status: String(res.status), message, where: url });
      throw new Error(message);
    }
    return data;
  }

  function postJson(url, body) {
    return getJson(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
  }

  function setError(message) {
    formError.hidden = !message;
    formError.textContent = message || "";
    if (message) {
      input.setAttribute("aria-invalid", "true");
      input.setAttribute("aria-describedby", "form-error");
    } else {
      input.removeAttribute("aria-invalid");
      input.removeAttribute("aria-describedby");
    }
  }

  function setLive(message) {
    live.textContent = message || "";
  }

  function paintRunStats(s) {
    statRun.textContent = `${s.turns}턴`;
    if (!s.turns && !s.errors) {
      runStats.textContent = "아직 보낸 질문이 없습니다.";
      return;
    }
    runStats.textContent =
      `${s.turns}턴 · 맞음 ${s.ok} · 틀림 ${s.bad} · 미판정 ${s.unjudged}` +
      (s.warned ? ` · 경고 ${s.warned}` : "") +
      (s.errors ? ` · 오류 ${s.errors}` : "") +
      (s.spent_usd ? ` · ${money(s.spent_usd)}` : "");
  }

  // 숫자의 원본은 서버 로그(이번 서버 실행 = run id). 새로고침해도 같다.
  let runTimer = 0;
  function updateRunStats() {
    clearTimeout(runTimer);
    runTimer = setTimeout(() => {
      getJson("/api/debug/run").then(paintRunStats).catch(() => {});
    }, 250);
  }

  // ---------- 테마 ----------

  function applyTheme(theme) {
    if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
    else delete document.documentElement.dataset.theme;
  }
  applyTheme(store.get("sandbox-theme"));
  $("theme-toggle").addEventListener("click", () => {
    const dark = document.documentElement.dataset.theme
      ? document.documentElement.dataset.theme === "dark"
      : window.matchMedia("(prefers-color-scheme: dark)").matches;
    const next = dark ? "light" : "dark";
    applyTheme(next);
    store.set("sandbox-theme", next);
  });

  // ---------- 준비 진행률 ----------

  function applyPrepare(progress) {
    const row = progress && typeof progress === "object" ? progress : null;
    const running = row && row.status === "running";
    const idle = row && row.status === "idle";
    if (!row || (!running && !idle)) {
      prepareBanner.hidden = true;
      return Boolean(running);
    }
    prepareBanner.hidden = false;
    const pct = Number(row.overall_pct);
    prepareBar.value = Number.isFinite(pct) ? pct : 0;
    prepareLabel.textContent = String(row.label || "");
    return Boolean(running);
  }

  let preparePollTimer = 0;
  async function pollPrepare() {
    let wait = 15000;
    try {
      const res = await fetch("/api/prepare/progress");
      wait = applyPrepare(await res.json()) ? 800 : 15000;
    } catch {
      /* 서버 재시작 중 */
    }
    clearTimeout(preparePollTimer);
    preparePollTimer = setTimeout(pollPrepare, wait);
  }

  // ---------- 채널·대화 ----------

  function channelDisplayName(ch) {
    const raw = String((ch && ch.label) || (ch && ch.channel_id) || "");
    if (!raw) return "채널";
    return /^[#\p{L}]/u.test(raw) && !/^\d+$/.test(raw) ? raw : `# ${raw}`;
  }

  function renderChannelList(bot) {
    knownChannels = (bot && bot.channels) || [];
    activeChannelId = String((bot && bot.active_channel_id) || "");
    channelList.replaceChildren();
    const idle = [];
    for (const ch of knownChannels) {
      const btn = el(
        "button",
        {
          type: "button",
          class: "item",
          disabled: !ch.enabled,
          "aria-current": ch.channel_id === activeChannelId ? "true" : null,
          title: `채널 ${ch.channel_id}`,
          onclick: ch.enabled ? () => selectChannel(ch.channel_id) : null,
        },
        el("span", { class: "item-name", text: channelDisplayName(ch) }),
        el("span", {
          class: "item-meta",
          text: ch.indexed ? (ch.enabled ? `대화 가능 · ${ch.respond}` : "허용되지 않음") : "인덱스 없음",
        })
      );
      if (ch.enabled || ch.channel_id === activeChannelId) channelList.append(el("li", null, btn));
      else idle.push(el("li", null, btn));
    }
    if (idle.length) {
      // 인덱스가 없거나 허용되지 않은 채널은 접어 둔다(테스트 중에는 고를 수 없다).
      channelList.append(el("li", null, el("details", { class: "idle-channels" },
        el("summary", { text: `대화할 수 없는 채널 ${idle.length}개` }),
        el("ul", { class: "list" }, idle))));
    }
    const active = knownChannels.find((c) => c.channel_id === activeChannelId);
    channelHeading.textContent = active ? channelDisplayName(active) : "채널";
    channelHeading.title = active ? `채널 ${active.channel_id}` : "";
    return active;
  }

  async function refreshStatus() {
    const data = await getJson("/api/status");
    applyPrepare(data.prepare);
    const runInfo = data.run || {};
    datasetBadge.textContent = runInfo.dataset || runInfo.data_dir || "데이터셋";
    datasetBadge.title = [
      runInfo.dataset && `설정 experiment: ${runInfo.dataset}`,
      runInfo.collection && `컬렉션: ${runInfo.collection}`,
      runInfo.data_dir && `인덱스 폴더: ${runInfo.data_dir}`,
    ].filter(Boolean).join("\n");
    runIdEl.textContent = runInfo.server_run_id || "—";
    const bot = data.bot || {};
    const active = renderChannelList(bot);
    if (!data.ready) {
      statWindows.textContent = "준비 중";
      if (!(data.prepare && data.prepare.status === "running")) {
        setError("인덱스가 없습니다. python3 prepare.py 를 실행하세요.");
      }
      return data;
    }
    const leaves = data.index?.leaf_count ?? data.index?.window_count;
    const msgs = data.index?.message_count;
    statWindows.textContent = typeof leaves === "number"
      ? `${leaves.toLocaleString("ko-KR")}조각${msgs != null ? ` · ${Number(msgs).toLocaleString("ko-KR")}말` : ""}`
      : "—";
    statBudget.textContent = money(data.session_remaining_usd);
    const ranking = data.config?.ranking || "auto";
    if (rankingSelect && !store.get("playground-ranking")) rankingSelect.value = ranking;
    const view = bot.view || active?.view || {};
    const n = (view.blocks || []).length;
    channelNote.textContent = active?.indexed
      ? n
        ? `이 채널 기억: 블록 ${n}개(붙인 방만 검색). 답마다 진단·근거를 열어 볼 수 있습니다.`
        : "이 채널의 예전 대화를 검색합니다. 답마다 진단·근거를 열어 볼 수 있습니다."
      : "기억 인덱스가 있는 채널만 대화할 수 있습니다.";
    if (!personaText.dataset.dirty) personaText.value = bot.persona || "";
    if (!rulesText.dataset.dirty) rulesText.value = bot.rules || "";
    respondMode.value = bot.respond || (bot.mention_required ? "mention" : "always");
    await loadConversations(bot.active_channel_id || "");
    renderLabBlocks(data).catch(() => {});
    return data;
  }

  function clearThread() {
    thread.querySelectorAll("article.msg, .session-summary-note").forEach((node) => node.remove());
  }

  function showWelcome() {
    if (!welcome.isConnected) thread.prepend(welcome);
  }

  function paintConversation(conv) {
    conversationId = String((conv && conv.id) || "");
    history.length = 0;
    clearThread();
    const msgs = (conv && Array.isArray(conv.messages) ? conv.messages : []).filter(
      (m) => m && (m.role === "user" || m.role === "assistant") && String(m.content || "").trim()
    );
    const summary = conv && String(conv.summary || "").trim();
    if (summary) {
      thread.append(el("p", {
        class: "session-summary-note",
        text: msgs.length ? "앞부분은 요약해 두었습니다. 최근 말만 아래에 원문으로 있습니다." : "이전 대화 요약이 있습니다. 새 메시지를 보내면 이어서 봅니다.",
      }));
    }
    if (!msgs.length) {
      showWelcome();
      return;
    }
    welcome.remove();
    for (const m of msgs) {
      const text = String(m.content || "");
      if (m.role === "user") appendUserMessage(text);
      else appendStoredBot(text, m.debug_id || "");
      history.push({ role: m.role, content: text });
    }
    scrollToEnd();
  }

  function renderConversationList(data) {
    conversationList.replaceChildren();
    const rows = data.conversations || [];
    if (!rows.length) {
      conversationList.append(el("li", { class: "list-empty", text: "저장된 대화가 없습니다." }));
      return;
    }
    for (const row of rows) {
      const bits = [];
      if (row.n) bits.push(`${row.n}말`);
      if (row.has_summary) bits.push("요약 있음");
      conversationList.append(
        el("li", null, el(
          "button",
          {
            type: "button",
            class: "item",
            "aria-current": row.id === data.active_id ? "true" : null,
            onclick: () => selectConversation(row.id),
          },
          el("span", { class: "item-name", text: row.title || "새 대화" }),
          el("span", { class: "item-meta", text: bits.join(" · ") || "비어 있음" })
        ))
      );
    }
  }

  async function fetchConversations(channelId) {
    return getJson(`/api/conversations${channelId ? `?channel_id=${encodeURIComponent(channelId)}` : ""}`);
  }

  async function refreshConversationList(channelId) {
    const data = await fetchConversations(channelId);
    renderConversationList(data);
    if (data.conversation && data.conversation.id) conversationId = String(data.conversation.id);
  }

  async function loadConversations(channelId) {
    const data = await fetchConversations(channelId);
    renderConversationList(data);
    paintConversation(data.conversation || {});
  }

  async function postConversation(action, extra) {
    const data = await postJson("/api/conversations", { action, ...(extra || {}) });
    renderConversationList(data);
    paintConversation(data.conversation || {});
  }

  async function selectConversation(id) {
    try {
      await postConversation("select", { id });
      closeRail();
    } catch (err) {
      setError(err.message);
    }
  }

  async function selectChannel(channelId) {
    try {
      await postJson("/api/bot", { channel_id: channelId });
    } catch (err) {
      setError(err.message);
      return;
    }
    delete personaText.dataset.dirty;
    delete rulesText.dataset.dirty;
    await refreshStatus();
    closeRail();
  }

  // ---------- 메시지 ----------

  function appendUserMessage(content) {
    welcome.remove();
    const article = el(
      "article",
      { class: "msg msg-user", "aria-label": "내 메시지" },
      el("div", { class: "bubble", text: content }),
      el(
        "div",
        { class: "user-actions" },
        el("button", { type: "button", class: "act", text: "다시 보내기", title: "같은 질문을 그대로 다시 보냅니다", onclick: () => sendMessage(content) }),
        el("button", { type: "button", class: "act", text: "고쳐 쓰기", onclick: () => fillComposer(content, false) })
      )
    );
    thread.append(article);
    scrollToEnd();
  }

  function appendStoredBot(content, debugId) {
    const article = el(
      "article",
      { class: "msg msg-bot", "aria-label": "봇 응답" },
      el("p", { class: "msg-role", text: debugId ? "마사몽" : "마사몽 · 저장된 답 (기록 id 없음)" }),
      el("div", { class: "bubble md", html: content ? renderMarkdown(content) : "<p class='md-empty'>응답이 비어 있습니다.</p>" })
    );
    thread.append(article);
    if (!debugId) return;
    // 새로고침 뒤에도 진단·근거·판정 버튼을 다시 붙인다(서버 로그의 compact 기록).
    getJson(`/api/debug/turn?compact=1&id=${encodeURIComponent(debugId)}`)
      .then((data) => {
        const rec = data.turn || {};
        const last = (rec.flags || []).filter((f) => f.ok === true || f.ok === false).pop();
        attachFooter(article, { ...rec, debug_id: debugId }, rec.hits || [], null, {
          stored: true,
          verdict: last ? last.ok : null,
          tags: last ? last.tags : [],
        });
      })
      .catch(() => {
        article.querySelector(".msg-role").textContent = "마사몽 · 기록을 찾지 못함";
      });
  }

  function diagChips(diag, hits, timing) {
    const chips = [];
    if (!diag) return chips;
    const rk = diag.ranking || {};
    if (!diag.rag_used) {
      chips.push(tag("검색 안 함", "info", `memory_requirement=${diag.memory_requirement || "—"}`));
    } else {
      chips.push(tag(RANKING_SHORT[rk.applied] || rk.applied || "—", null,
        `요청 ${rk.requested || "—"} → 적용 ${rk.applied || "—"}\n이유 ${rk.auto_reason || "—"} · 확신 ${rk.confidence || "—"}`));
      const scores = (diag.hits || []).map((h) => h.score).filter((s) => typeof s === "number");
      const best = scores.length ? Math.max(...scores) : null;
      if (hits.length) {
        chips.push(tag(`근거 ${hits.length}${best != null ? ` · 최고 ${best.toFixed(2)}` : ""}`, "ok"));
      } else if (diag.rerank && diag.rerank.abstained) {
        chips.push(tag("근거 없음 · 하한 미만", "warn", `모든 후보가 재순위 하한 ${diag.rerank.floor} 미만`));
      } else {
        chips.push(tag("후보 없음", "warn", `rag_state=${diag.rag_state || "—"}`));
      }
    }
    const ps = diag.persons;
    if (ps && (ps.targets || []).length) {
      const miss = (ps.unresolved || []).length;
      if ((ps.matched_speakers || []).length && !miss) {
        chips.push(tag(`인물 ✓ ${ps.targets.join(", ")}`, "ok", `화자: ${(ps.matched_speakers || []).join(", ")}`));
      } else {
        chips.push(tag(`인물 ✗ ${(ps.unresolved || ps.targets).join(", ")}`, "bad", "허용 범위의 화자와 연결하지 못함"));
      }
      if ((ps.ambiguous || []).length) chips.push(tag(`모호 ${ps.ambiguous.join(", ")}`, "warn"));
    }
    const t = diag.time || {};
    if (t.applied) {
      chips.push(tag(`시간 창 ${timeWindowText(t)}`, "info", `후보 ${t.pool_n ?? "—"}`));
    } else if (t.planner_kind && !["none", "any"].includes(t.planner_kind)) {
      chips.push(tag(`시간 ${t.planner_kind} · 창 없음`, "warn", `플래너 ${t.planner_from || ""}~${t.planner_to || ""}`));
    }
    const it = diag.intent || {};
    if (diag.rag_used && it.source && it.source !== "planner") {
      chips.push(tag("기본 의도 (플래너 없음)", "warn", `intent.source=${it.source}`));
    }
    const shown = new Set(["planner_fallback", "person_unresolved", "person_ambiguous", "abstained_floor", "no_evidence"]);
    for (const w of diag.warnings || []) {
      if (!shown.has(w.code)) chips.push(tag(w.label, "warn", w.code));
    }
    if (diag.embed && diag.embed.source === "hit") chips.push(tag("임베딩 캐시", "info"));
    const total = Number((timing || {}).total_ms);
    if (Number.isFinite(total) && total > 0) chips.push(tag(sec(total), null, "단계 합계 (진단에서 단계별 시간)"));
    return chips;
  }

  function timeWindowText(t) {
    if (t.from || t.to) return `${t.kind || ""}${t.hard ? " 확정" : ""} ${String(t.from || "…").slice(0, 10)} ~ ${String(t.to || "…").slice(0, 10)}`;
    if (t.kind === "recent") {
      const bits = [];
      if (t.recent_window_days) bits.push(`${t.recent_window_days}일`);
      if (t.recent_window_leaves) bits.push(`최대 ${t.recent_window_leaves}조각`);
      return `최근${bits.length ? ` (${bits.join(" · ")})` : ""}`;
    }
    return t.kind || "";
  }

  function dlRows(rows) {
    const dl = el("dl", { class: "diag-grid" });
    for (const [label, value] of rows) {
      if (value == null || value === "" || (Array.isArray(value) && !value.length)) continue;
      dl.append(el("dt", { text: label }), el("dd", null, value));
    }
    return dl;
  }

  function timingBar(timing) {
    const parts = STAGES.map(([key, label, color]) => [label, Number((timing || {})[key]) || 0, color]).filter(([, ms]) => ms >= 5);
    const total = parts.reduce((sum, [, ms]) => sum + ms, 0);
    if (!total) return null;
    const bar = el("div", { class: "timebar", role: "img", "aria-label": "단계별 시간" });
    const legend = el("div", { class: "timelegend" });
    for (const [label, ms, color] of parts) {
      bar.append(el("span", { style: `width:${(ms / total) * 100}%;background:var(${color})`, title: `${label} ${sec(ms)}` }));
      legend.append(el("span", null, el("i", { style: `background:var(${color})` }), `${label} ${sec(ms)}`));
    }
    legend.append(el("span", null, `= ${sec(total)}`));
    return el("div", null, bar, legend);
  }

  function diagPanel(diag, timing) {
    const box = el("section", { class: "panel-box", hidden: true, "aria-label": "진단" });
    if (!diag) {
      box.append(el("p", { class: "hint", text: "진단 정보가 없습니다." }));
      return box;
    }
    const rk = diag.ranking || {};
    const it = diag.intent || {};
    const q = diag.queries || {};
    const t = diag.time || {};
    const ps = diag.persons;
    const rr = diag.rerank || {};
    const cand = diag.candidates || {};
    const emb = diag.embed || {};
    const scoreChips = (rr.top || []).map((s) => tag(`${s.score.toFixed(2)}${s.above_floor ? " ✓" : " ✗"}`, s.above_floor ? "ok" : null, s.doc_id));
    box.append(
      dlRows([
        ["검색 경로", diag.rag_used ? `${rk.requested || "—"} → ${rk.applied || "—"} · ${rk.auto_reason || "—"} · 확신 ${rk.confidence || "—"}${rk.recovery ? " · 복구 1회" : ""}` : `검색 안 함 (memory ${diag.memory_requirement || "—"})`],
        ["확신 근거", (rk.confidence_reasons || []).join(", ")],
        ["상태", `${diag.rag_state || "—"} · memory ${diag.memory_requirement || "—"}`],
        ["의도", `출처 ${it.source || "—"} · 종류 ${it.kind || "—"}${(it.targets || []).length ? ` · 대상 ${it.targets.join(", ")}` : ""}`],
        ["질의", [q.q0 && `q0 원문: ${q.q0}`, q.q1 && `q1 의미: ${q.q1}`, q.q2 && `q2 키워드: ${q.q2}`].filter(Boolean).join("\n") || null],
        ["시간", t.applied
          ? `창 ${timeWindowText(t)} · 후보 ${t.pool_n ?? "—"}${t.widened ? " · 넓혀 재검색" : ""}${t.planner_kind ? ` (플래너 ${t.planner_kind})` : ""}`
          : t.planner_kind && !["none", "any"].includes(t.planner_kind) ? `플래너 ${t.planner_kind} ${t.planner_from || ""}~${t.planner_to || ""} → 창 없음` : "제약 없음"],
        ["인물", ps ? `${(ps.targets || []).join(", ")} → 화자 ${(ps.matched_speakers || []).join(", ") || "없음"}${(ps.ambiguous || []).length ? ` · 모호 ${ps.ambiguous.join(", ")}` : ""}${(ps.unresolved || []).length ? ` · 미해결 ${ps.unresolved.join(", ")}` : ""}${ps.account_linked_n != null ? ` · 계정 연결 ${ps.account_linked_n} / 표시명만 ${ps.label_only_n}` : ""}` : null],
        ["후보", cand.union_n ? `합집합 ${cand.union_n} · 풀 ${cand.pool_n ?? "—"} · ${(cand.retrievers || []).join(", ")}` : null],
        ["재순위", rr.performed ? el("div", null, `하한 ${rr.floor} · 남김 ${rr.kept_n ?? "—"} · 상위 점수 `, el("span", { class: "scores" }, scoreChips)) : null],
        ["임베딩", emb.source ? `q0 ${emb.source}${emb.hedged ? " (헤지)" : ""}${emb.q1_source ? ` · q1 ${emb.q1_source}` : ""}` : null],
      ])
    );
    const warns = diag.warnings || [];
    if (warns.length) box.append(el("ul", { class: "diag-warn" }, warns.map((w) => el("li", null, `${w.label} `, el("code", { text: w.code })))));
    const bar = timingBar(timing);
    if (bar) box.append(bar);
    return box;
  }

  function evidencePanel(hits, diag) {
    const box = el("section", { class: "panel-box", hidden: true, "aria-label": "근거" });
    const scoreBy = {};
    for (const h of (diag && diag.hits) || []) scoreBy[h.doc_id] = h.score;
    const list = el("ul", { class: "evidence-list" });
    for (const hit of hits) {
      const score = scoreBy[hit.doc_id];
      const card = el(
        "li",
        { class: "ev" },
        el(
          "div",
          { class: "ev-head" },
          el("b", { text: `#${hit.rank ?? ""}` }),
          hit.date ? el("span", { text: hit.date }) : null,
          el("span", { text: hit.label || hit.source_room_id || "" }),
          (hit.speakers || []).length ? el("span", { text: hit.speakers.slice(0, 6).join(", ") }) : null,
          typeof score === "number" ? tag(`점수 ${score.toFixed(2)}`, score >= ((diag && diag.rerank && diag.rerank.floor) || 0) ? "ok" : "warn") : null
        ),
        hit.summary ? el("p", { class: "ev-summary", text: `요약(검색용): ${hit.summary}` }) : null,
        el("p", { class: "ev-text", text: hit.excerpt || "" })
      );
      if (String(hit.excerpt || "").length > 280) {
        const more = el("button", { type: "button", class: "link-btn ev-more", text: "더 보기" });
        more.addEventListener("click", () => {
          const open = card.classList.toggle("is-open");
          more.textContent = open ? "접기" : "더 보기";
        });
        card.append(more);
      }
      list.append(card);
    }
    box.append(list);
    return box;
  }

  function summarizeDebug(row) {
    if (!row || typeof row !== "object") return row;
    const rt = row.rank_trace || {};
    return {
      debug_id: row.debug_id,
      run: row.run,
      path: row.path,
      diagnosis: row.diagnosis,
      models: row.models,
      intent: row.intent,
      plan: row.plan,
      plan_raw: row.plan_raw,
      tools: row.tools,
      timing: row.timing,
      usage: row.usage,
      spent_usd: row.spent_usd,
      prompt: row.prompt,
      history: row.history,
      error: row.error,
      flags: row.flags,
      hybrid_top10: rt.hybrid_top10,
      candidate_sources: rt.candidate_sources,
      rerank_scores: (rt.rerank_scores || []).slice(0, 20),
      rerank_order: (rt.rerank_order || []).slice(0, 20),
    };
  }

  function tracePanel(debugId) {
    const box = el("section", { class: "panel-box", hidden: true, "aria-label": "추적" });
    const pre = el("pre", { class: "trace-json", text: "불러오는 중…" });
    box.append(pre);
    box.load = async () => {
      if (pre.dataset.loaded) return;
      try {
        const data = await getJson(`/api/debug/turn?id=${encodeURIComponent(debugId)}`);
        pre.textContent = JSON.stringify(summarizeDebug(data.turn || data), null, 2);
        pre.dataset.loaded = "1";
      } catch (err) {
        pre.textContent = err.message;
      }
    };
    return box;
  }

  function feedbackPanel(info, onSent) {
    const box = el("section", { class: "panel-box", hidden: true, "aria-label": "틀린 이유" });
    const chosen = new Set();
    const tags = el("div", { class: "feedback-tags" });
    for (const [code, label] of WRONG_TAGS) {
      const chip = el("button", { type: "button", class: "chip-btn", "aria-pressed": "false", text: label });
      chip.addEventListener("click", () => {
        if (chosen.has(code)) chosen.delete(code);
        else chosen.add(code);
        chip.setAttribute("aria-pressed", chosen.has(code) ? "true" : "false");
      });
      tags.append(chip);
    }
    const note = el("textarea", { rows: 2, placeholder: "기대한 답이나 어디가 틀렸는지 (선택)", "aria-label": "메모" });
    const send = el("button", { type: "button", class: "primary", text: "틀림 보내기" });
    send.addEventListener("click", async () => {
      send.disabled = true;
      try {
        await postJson("/api/debug/flag", {
          debug_id: info.debug_id, ok: false, tags: [...chosen], note: note.value.trim(), conversation_id: conversationId || null,
        });
        box.hidden = true;
        onSent([...chosen]);
      } catch (err) {
        send.disabled = false;
        setError(err.message);
      }
    });
    note.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) send.click();
    });
    box.append(el("p", { class: "hint", text: "해당하는 이유를 고르세요 (여러 개 가능)." }), tags, el("div", { class: "feedback-row" }, note, send));
    return box;
  }

  function renderToolCard(t) {
    const data = t && t.data;
    if (!data) return null;
    const tool = t.tool || "";
    if (tool === "polls.create") {
      const poll = data.poll || data;
      const q = (poll.question && poll.question.text) || poll.question || "";
      const answers = Array.isArray(poll.answers) ? poll.answers : [];
      if (!q && !answers.length) return null;
      return el(
        "div",
        { class: "discord-poll", role: "group" },
        el("p", { class: "discord-poll__q", text: q }),
        el("ul", { class: "discord-poll__answers" }, answers.map((a) => {
          const media = a.poll_media || a;
          const emoji = (media.emoji && media.emoji.name) || "";
          return el("li", { text: `${emoji ? emoji + " " : ""}${media.text || ""}`.trim() });
        })),
        el("p", { class: "discord-poll__meta", text: `${poll.duration || data.duration_hours || 24}시간 · ${poll.allow_multiselect ? "복수 선택" : "하나 선택"}` })
      );
    }
    if (tool === "search.web" && Array.isArray(data.results) && data.results.length) {
      const links = data.results.slice(0, 5).filter((r) => r && r.url)
        .map((r) => el("a", { class: "discord-link-btn", href: r.url, target: "_blank", rel: "noopener noreferrer", text: r.title || r.url }));
      return links.length ? el("nav", { class: "discord-links" }, links) : null;
    }
    const at = data.observed_at || data.at || data.updated_at;
    if ((tool === "weather.query" || tool === "finance.quote" || tool === "finance.fx") && at) {
      const ts = Date.parse(at);
      return el("p", { class: "discord-time", text: Number.isFinite(ts) ? `기준 ${new Date(ts).toLocaleString("ko-KR")}` : String(at) });
    }
    return null;
  }

  /**
   * 답 아래 진단 줄·버튼·패널. 새 턴(done 이벤트)과 새로고침 뒤 저장된 답(compact 기록)이 같이 쓴다.
   * opts.stored: 이번 실행 통계에 세지 않는다. opts.verdict: 이미 남긴 판정(true/false)을 표시만 한다.
   */
  function attachFooter(article, info, finalHits, early, opts) {
    const o = opts || {};
    const diag = info.diagnosis || (early && early.diagnosis) || null;
    const timing = info.timing || (early && early.timing) || {};
    const chips = diagChips(diag, finalHits, timing);
    if (chips.length) article.append(el("div", { class: "diag-strip" }, chips));

    const tools = info.tools || [];
    const cards = tools.map(renderToolCard).filter(Boolean);
    if (tools.length) {
      article.append(el("div", { class: "diag-strip" }, tools.map((t) => tag(`${t.tool || "?"}${t.ok === false ? " 실패" : ""}`, t.ok === false ? "bad" : null, t.status || ""))));
    }
    if (cards.length) article.append(el("div", { class: "tool-cards" }, cards));

    const panels = [];
    const actions = el("div", { class: "turn-actions" });
    function closePanels() {
      for (const [p, b] of panels) {
        p.hidden = true;
        b.setAttribute("aria-expanded", "false");
      }
    }
    function bindToggle(btn, panel, onOpen) {
      panels.push([panel, btn]);
      btn.addEventListener("click", () => {
        const open = panel.hidden;
        closePanels();
        panel.hidden = !open;
        btn.setAttribute("aria-expanded", open ? "true" : "false");
        if (open && onOpen) onOpen();
      });
      return btn;
    }
    function toggler(label, panel, onOpen) {
      return bindToggle(el("button", { type: "button", class: "act", "aria-expanded": "false", text: label }), panel, onOpen);
    }

    if (info.debug_id) {
      const okBtn = el("button", { type: "button", class: "act", text: "맞음" });
      const badBtn = el("button", { type: "button", class: "act", "aria-expanded": "false", text: "틀림…" });
      function mark(next, tags, refresh = true) {
        okBtn.classList.toggle("is-ok", next === true);
        badBtn.classList.toggle("is-bad", next === false);
        badBtn.textContent = next === false ? `틀림${tags && tags.length ? ` · ${tags.length}` : ""}` : "틀림…";
        if (refresh) updateRunStats();
      }
      okBtn.addEventListener("click", async () => {
        try {
          await postJson("/api/debug/flag", { debug_id: info.debug_id, ok: true, tags: [], note: "", conversation_id: conversationId || null });
          closePanels();
          mark(true);
        } catch (err) {
          setError(err.message);
        }
      });
      const fb = feedbackPanel(info, (tags) => mark(false, tags));
      bindToggle(badBtn, fb, () => fb.querySelector("textarea")?.focus());
      actions.append(okBtn, badBtn);
      article.append(fb);
      if (o.verdict === true || o.verdict === false) mark(o.verdict, o.tags, false);
    }

    const dp = diagPanel(diag, timing);
    actions.append(toggler("진단", dp));
    article.append(dp);
    if (finalHits.length) {
      const ep = evidencePanel(finalHits, diag);
      actions.append(toggler(`근거 ${finalHits.length}`, ep));
      article.append(ep);
    }
    if (info.debug_id) {
      const tp = tracePanel(info.debug_id);
      actions.append(toggler("추적", tp, tp.load));
      article.append(tp);
      actions.append(el("span", { class: "spacer" }));
      const copy = el("button", { type: "button", class: "link-btn debug-id", text: info.debug_id, title: "debug_id 복사 (로그에서 이 턴 찾기)" });
      copy.addEventListener("click", () => navigator.clipboard?.writeText(info.debug_id).then(() => { copy.textContent = "복사함"; }).catch(() => {}));
      actions.append(copy);
    }
    // 버튼 줄을 패널보다 위에 둔다.
    const firstPanel = article.querySelector(".panel-box");
    article.insertBefore(actions, firstPanel || null);
  }


  function createBotShell() {
    welcome.remove();
    const status = el("p", { class: "msg-status", text: "기억에서 찾는 중…" });
    const body = el("div", { class: "bubble md is-empty" });
    const article = el("article", { class: "msg msg-bot is-streaming", "aria-label": "봇 응답", "aria-busy": "true" },
      el("p", { class: "msg-role", text: "마사몽" }), status, body);
    thread.append(article);
    scrollToEnd();

    let raw = "";
    let raf = 0;
    let hits = [];
    let early = null;

    function paint() {
      raf = 0;
      body.classList.toggle("is-empty", !raw);
      body.innerHTML = raw ? renderMarkdown(raw) : "";
      scrollToEnd();
    }

    return {
      setStatus(label) {
        status.hidden = !label;
        status.textContent = label || "";
      },
      appendDelta(text) {
        raw += text;
        if (!raf) raf = requestAnimationFrame(paint);
      },
      setHits(next) {
        hits = Array.isArray(next) ? next : [];
      },
      setEarly(next) {
        early = next;
      },
      finish(finalText, info) {
        if (raf) cancelAnimationFrame(raf);
        raw = finalText != null ? String(finalText) : raw;
        status.hidden = true;
        body.classList.toggle("is-empty", !raw);
        body.innerHTML = raw ? renderMarkdown(raw) : "<p class='md-empty'>응답이 비어 있습니다.</p>";
        article.classList.remove("is-streaming");
        article.removeAttribute("aria-busy");
        const finalHits = Array.isArray(info.hits) ? info.hits : hits;
        attachFooter(article, info || {}, finalHits, early);
        scrollToEnd();
        return raw || "응답이 비어 있습니다.";
      },
      fail(message) {
        if (raf) cancelAnimationFrame(raf);
        status.hidden = true;
        article.classList.remove("is-streaming");
        article.removeAttribute("aria-busy");
        body.classList.remove("is-empty");
        body.innerHTML = `${raw ? renderMarkdown(raw) : ""}<p class="md-error">${escapeHtml(message)}</p>`;
        if (early && early.diagnosis) attachFooter(article, early, hits, null);
      },
    };
  }

  // ---------- 보내기 ----------

  function setBusy(next) {
    busy = next;
    sendBtn.disabled = next;
    sendBtn.classList.toggle("is-busy", next);
    sendBtn.setAttribute("aria-busy", next ? "true" : "false");
  }

  function autoGrow() {
    input.style.height = "auto";
    input.style.height = `${Math.min(input.scrollHeight + 2, 224)}px`;
  }

  function fillComposer(text, selectPlaceholder) {
    input.value = text;
    autoGrow();
    input.focus();
    const m = selectPlaceholder ? /\[[^\]]*\]/.exec(text) : null;
    if (m) input.setSelectionRange(m.index, m.index + m[0].length);
    else input.setSelectionRange(text.length, text.length);
  }

  async function readSse(response, onEvent) {
    if (!response.body) throw new Error("응답을 열 수 없습니다.");
    const reader = response.body.getReader();
    const decoder = new TextDecoder("utf-8");
    let buffer = "";
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let sep;
      while ((sep = buffer.indexOf("\n\n")) >= 0) {
        const chunk = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        for (const line of chunk.split("\n")) {
          const trimmed = line.trim();
          if (!trimmed.startsWith("data:")) continue;
          const payload = trimmed.slice(5).trim();
          if (!payload || payload === "[DONE]") continue;
          let event;
          try {
            event = JSON.parse(payload);
          } catch {
            continue;
          }
          onEvent(event);
        }
      }
    }
  }

  async function sendMessage(text) {
    const message = String(text || "").trim();
    if (busy) return;
    if (!message) {
      setError("메시지를 입력해 주세요.");
      input.focus();
      return;
    }
    if (/\[[^\]]*\]/.test(message) && !confirm("[괄호] 자리가 남아 있습니다. 그대로 보낼까요?")) {
      fillComposer(message, true);
      return;
    }
    setError("");
    appendUserMessage(message);
    history.push({ role: "user", content: message });
    input.value = "";
    autoGrow();
    setBusy(true);
    setLive("기억에서 찾는 중…");
    const bot = createBotShell();
    let finished = false;
    const started = performance.now();
    try {
      const res = await fetch("/api/chat/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
        body: JSON.stringify({
          message,
          history: history.slice(0, -1),
          ranking: rankingSelect.value,
          speaker: speakerInput.value || "테스터",
          conversation_id: conversationId || undefined,
        }),
      });
      if (!res.ok) {
        let errMsg = `요청에 실패했습니다 (HTTP ${res.status})`;
        try {
          errMsg = (await res.json()).error || errMsg;
        } catch {
          /* 본문 없음 */
        }
        throw new Error(errMsg);
      }
      await readSse(res, (event) => {
        if (!event || typeof event !== "object") return;
        if (event.event === "status") {
          const label = event.label || "진행 중…";
          bot.setStatus(label);
          setLive(`${label} ${((performance.now() - started) / 1000).toFixed(1)}초`);
        } else if (event.event === "debug") {
          bot.setEarly(event);
        } else if (event.event === "hits") {
          bot.setHits(event.hits);
        } else if (event.event === "delta") {
          bot.setStatus("");
          setLive(`작성 중… ${((performance.now() - started) / 1000).toFixed(1)}초`);
          bot.appendDelta(event.text || "");
        } else if (event.event === "done") {
          const answer = bot.finish(event.answer, event);
          if (event.conversation_id) conversationId = String(event.conversation_id);
          history.push({ role: "assistant", content: answer });
          updateRunStats();
          refreshConversationList("").catch(() => {});
          statBudget.textContent = money(event.session_remaining_usd);
          setLive(`이번 턴 ${money(event.spent_usd)} · 체감 ${((performance.now() - started) / 1000).toFixed(1)}초 · 남은 예산 ${money(event.session_remaining_usd)}`);
          finished = true;
        } else if (event.event === "error") {
          throw new Error(event.error || event.label || "요청에 실패했습니다");
        }
      });
      if (!finished) throw new Error("응답이 중간에 끊겼습니다.");
    } catch (err) {
      const msg = err instanceof Error ? err.message : "알 수 없는 오류";
      bot.fail(msg);
      setError(msg);
      setLive("");
      history.pop();
      updateRunStats();
      reportClientError({ type: "turn_failed", message: msg, where: "chat_stream" });
    } finally {
      setBusy(false);
      input.focus();
    }
  }

  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    sendMessage(input.value);
  });
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      sendMessage(input.value);
    }
  });
  input.addEventListener("input", autoGrow);

  const templatePopover = $("template-popover");
  const templatesToggle = $("templates-toggle");

  function setTemplatesOpen(open) {
    templatePopover.hidden = !open;
    templatesToggle.setAttribute("aria-expanded", open ? "true" : "false");
  }

  function renderTemplates(root, onPick) {
    root.replaceChildren();
    root.append(el("div", { class: "templates" }, TEMPLATES.map((group) =>
      el(
        "section",
        { class: "template-group" },
        el("h3", { text: group.title }),
        el("p", { text: group.note }),
        el("div", { class: "template-list" }, group.items.map((text) =>
          el("button", {
            type: "button",
            class: "chip-btn",
            text,
            onclick: () => {
              fillComposer(text, true);
              if (onPick) onPick();
            },
          })))
      ))));
  }

  templatesToggle.addEventListener("click", () => setTemplatesOpen(templatePopover.hidden));

  // ---------- 레일 (좁은 화면) ----------

  const rail = $("rail");
  const railScrim = $("rail-scrim");
  const railToggle = $("rail-toggle");
  function closeRail() {
    rail.classList.remove("is-open");
    railScrim.hidden = true;
    railToggle.setAttribute("aria-expanded", "false");
  }
  railToggle.addEventListener("click", () => {
    const open = !rail.classList.contains("is-open");
    rail.classList.toggle("is-open", open);
    railScrim.hidden = !open;
    railToggle.setAttribute("aria-expanded", open ? "true" : "false");
  });
  railScrim.addEventListener("click", closeRail);

  $("copy-run-id").addEventListener("click", () => {
    const text = runIdEl.textContent || "";
    navigator.clipboard?.writeText(text).then(() => { $("copy-run-id").textContent = "복사함"; }).catch(() => {});
  });

  // ---------- 서랍 ----------

  const drawer = $("drawer");
  const drawerScrim = $("drawer-scrim");
  const TABS = { review: ["tab-review", "panel-review"], channel: ["tab-channel", "panel-channel"], lab: ["tab-lab", "panel-lab"] };
  let drawerTab = "";

  function openDrawer(name) {
    drawer.hidden = false;
    drawerScrim.hidden = false;
    drawerTab = name;
    for (const [key, [tabId, panelId]] of Object.entries(TABS)) {
      $(tabId).setAttribute("aria-selected", key === name ? "true" : "false");
      $(panelId).hidden = key !== name;
    }
    if (name === "review") loadReviewSets().catch((err) => setReviewStatus(err.message));
    if (name === "lab" && !labInventory) loadLab(false).catch(() => setLabStatus("목록을 못 불러왔습니다."));
    store.set("sandbox-drawer-tab", name);
  }
  function closeDrawer() {
    drawer.hidden = true;
    drawerScrim.hidden = true;
    drawerTab = "";
  }
  $("open-review").addEventListener("click", () => openDrawer("review"));
  $("open-settings").addEventListener("click", () => openDrawer("channel"));
  $("drawer-close").addEventListener("click", closeDrawer);
  drawerScrim.addEventListener("click", closeDrawer);
  for (const [key, [tabId]] of Object.entries(TABS)) $(tabId).addEventListener("click", () => openDrawer(key));

  function inField(target) {
    return target && (target.closest("textarea, input, select, [contenteditable]") != null);
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      if (!drawer.hidden) closeDrawer();
      else if (!templatePopover.hidden) setTemplatesOpen(false);
      else closeRail();
      return;
    }
    if (ev.key === "/" && !inField(ev.target) && drawer.hidden) {
      ev.preventDefault();
      input.focus();
      return;
    }
    if (drawerTab === "review" && !drawer.hidden && !inField(ev.target)) {
      if (ev.key === "1") setVerdict("ok");
      else if (ev.key === "2") setVerdict("fix");
      else if (ev.key === "3") setVerdict("drop");
      else if (ev.key === "ArrowRight") moveReview(1);
      else if (ev.key === "ArrowLeft") moveReview(-1);
    }
  });

  // ---------- 검수 ----------

  const reviewSet = $("review-set");
  const reviewPending = $("review-pending");
  const reviewCard = $("review-card");
  const review = { set: "", items: [], pos: 0 };
  const VERDICT_LABEL = { ok: "좋음", fix: "고칠 것", drop: "버림" };

  function setReviewStatus(text) {
    $("review-status").textContent = text || "";
  }

  async function loadReviewSets() {
    const data = await getJson("/api/review/sets");
    const sets = data.sets || [];
    const current = reviewSet.value || store.get("sandbox-review-set") || "";
    reviewSet.replaceChildren();
    if (!sets.length) {
      reviewSet.append(el("option", { value: "", text: "검수 시트 없음" }));
      setReviewStatus(".masamong-scratch 아래에 *.review.json 이 없습니다.");
      reviewCard.replaceChildren();
      return;
    }
    for (const s of sets) {
      const bound = Math.round((s.bound_share || 0) * 100);
      reviewSet.append(el("option", { value: s.set, text: `${s.set} · ${s.reviewed}/${s.n} 검수 · 이 인덱스와 ${bound}% 일치` }));
    }
    reviewSet.value = sets.some((s) => s.set === current) ? current : sets[0].set;
    await loadReviewItems();
  }

  async function loadReviewItems() {
    review.set = reviewSet.value;
    store.set("sandbox-review-set", review.set);
    if (!review.set) return;
    const data = await getJson(`/api/review/items?set=${encodeURIComponent(review.set)}`);
    review.items = data.items || [];
    review.pos = 0;
    if (reviewPending.checked) {
      const first = review.items.findIndex((it) => !it.verdict);
      review.pos = first >= 0 ? first : 0;
    }
    renderReview();
  }

  function visibleIndices() {
    return review.items.map((it, i) => [it, i]).filter(([it]) => !reviewPending.checked || !it.verdict).map(([, i]) => i);
  }

  function moveReview(step) {
    const idx = visibleIndices();
    if (!idx.length) return;
    const at = idx.indexOf(review.pos);
    const next = at < 0 ? idx.find((i) => (step > 0 ? i > review.pos : i < review.pos)) ?? idx[0] : idx[Math.min(idx.length - 1, Math.max(0, at + step))];
    review.pos = next;
    renderReview();
  }

  async function setVerdict(verdict, note) {
    const item = review.items[review.pos];
    if (!item) return;
    const nextNote = note != null ? note : ($("review-note")?.value ?? item.note ?? "");
    try {
      await postJson("/api/review/verdict", { set: review.set, question_id: item.question_id, verdict, note: nextNote });
    } catch (err) {
      setReviewStatus(err.message);
      return;
    }
    item.verdict = verdict;
    item.note = nextNote;
    const done = review.items.filter((it) => it.verdict).length;
    setReviewStatus(`저장함 · ${done}/${review.items.length} 검수`);
    const opt = reviewSet.selectedOptions[0];
    if (opt) opt.textContent = opt.textContent.replace(/\d+\/\d+ 검수/, `${done}/${review.items.length} 검수`);
    if (reviewPending.checked) {
      const next = review.items.findIndex((it, i) => i > review.pos && !it.verdict);
      const wrap = review.items.findIndex((it) => !it.verdict);
      review.pos = next >= 0 ? next : wrap >= 0 ? wrap : review.pos;
    }
    renderReview();
  }

  async function askSandbox(item) {
    closeDrawer();
    if (item.channel_id && item.channel_id !== activeChannelId) {
      const ch = knownChannels.find((c) => c.channel_id === item.channel_id);
      if (ch && ch.enabled) await selectChannel(item.channel_id);
      else setError("이 문항의 방은 지금 인덱스의 채널이 아닙니다. 현재 채널로 보냅니다.");
    }
    sendMessage(item.query || "");
  }

  function renderReview() {
    reviewCard.replaceChildren();
    const idx = visibleIndices();
    if (!review.items.length) {
      reviewCard.append(el("p", { class: "hint", text: "문항이 없습니다." }));
      return;
    }
    if (!idx.length) {
      reviewCard.append(el("p", { class: "hint", text: "미검수 문항이 없습니다. ‘미검수만’을 끄면 전체를 다시 볼 수 있습니다." }));
      return;
    }
    if (!idx.includes(review.pos)) review.pos = idx[0];
    const item = review.items[review.pos];
    const done = review.items.filter((it) => it.verdict).length;
    reviewCard.append(
      el("div", { class: "review-nav" },
        el("button", { type: "button", class: "ghost sm", text: "← 이전", onclick: () => moveReview(-1) }),
        el("span", { class: "count", text: `${idx.indexOf(review.pos) + 1} / ${idx.length} (전체 ${done}/${review.items.length} 검수)` }),
        el("button", { type: "button", class: "ghost sm", text: "다음 →", onclick: () => moveReview(1) })),
      el("p", { class: "review-q", text: item.query || "(질문 없음)" }),
      el("div", { class: "review-meta" },
        tag(item.type || "유형?"),
        item.expected_abstention ? tag("답 없음이 정답", "warn") : tag("답 있음", "ok"),
        item.room ? tag(item.room) : null,
        item.anchor_time ? tag(String(item.anchor_time).replace("T", " ").slice(0, 16)) : null,
        item.verdict ? tag(`현재 판정: ${VERDICT_LABEL[item.verdict] || item.verdict}`, "info") : null),
      el("p", { class: "review-ref" }, el("b", { text: "기준 답: " }), item.reference_answer || "—")
    );
    const ev = item.evidence;
    if (item.expected_abstention) {
      reviewCard.append(el("p", { class: "hint", text: "다른 컬렉션에서 만든 문항입니다. 이 방 대화로는 답할 수 없어야 정상입니다. 질문이 이 방에서도 우연히 답이 되는지 봐 주세요." }));
    } else if (!ev || !ev.bound) {
      reviewCard.append(el("p", { class: "hint", text: "지금 켠 인덱스에 이 문항의 원문이 없습니다. 실행기에서 해당 데이터셋으로 다시 켜세요." }));
    } else {
      reviewCard.append(
        el("p", { class: "hint", text: "원문 (노란 줄이 정답 근거로 지정된 메시지):" }),
        el("ul", { class: "review-lines" }, (ev.lines || []).map((line) =>
          el("li", { class: line.wanted ? "wanted" : null },
            el("span", { class: "t", text: String(line.time || "").replace("T", " ").slice(0, 16) }),
            el("span", { class: "s", text: line.speaker || "" }),
            el("span", { text: line.text || "" }))))
      );
    }
    const note = el("textarea", { id: "review-note", rows: 2, placeholder: "고칠 점 메모 (선택)", "aria-label": "검수 메모" });
    note.value = item.note || "";
    reviewCard.append(
      el("p", { class: "hint", text: "질문이 원문만으로 답할 수 있고, 기준 답이 맞고, 질문에 정답이 새지 않으면 ‘좋음’." }),
      el("div", { class: "verdicts" },
        ["ok", "fix", "drop"].map((v, i) =>
          el("button", {
            type: "button",
            class: "chip-btn",
            "aria-pressed": item.verdict === v ? "true" : "false",
            text: `${i + 1} ${VERDICT_LABEL[v]}`,
            onclick: () => setVerdict(v),
          })),
        el("button", { type: "button", class: "chip-btn", text: "판정 지우기", onclick: () => setVerdict("") })),
      note,
      el("div", { class: "verdicts" },
        el("button", { type: "button", class: "ghost sm", text: "메모 저장", onclick: () => setVerdict(item.verdict || "", note.value) }),
        el("button", { type: "button", class: "ghost sm", text: "샌드박스에 물어보기", onclick: () => askSandbox(item) }))
    );
  }

  reviewSet.addEventListener("change", () => loadReviewItems().catch((err) => setReviewStatus(err.message)));
  reviewPending.addEventListener("change", renderReview);

  // ---------- 채널 설정 ----------

  savePersona.addEventListener("click", async () => {
    personaStatus.textContent = "저장 중…";
    try {
      await postJson("/api/bot", { persona: personaText.value, rules: rulesText.value, respond: respondMode.value || "always" });
      personaStatus.textContent = "저장했습니다. 다음 메시지부터 적용됩니다.";
      delete personaText.dataset.dirty;
      delete rulesText.dataset.dirty;
    } catch (err) {
      personaStatus.textContent = err.message;
    }
  });
  personaText.addEventListener("input", () => { personaText.dataset.dirty = "1"; });
  rulesText.addEventListener("input", () => { rulesText.dataset.dirty = "1"; });
  speakerInput.value = store.get("sandbox-speaker") || speakerInput.value;
  speakerInput.addEventListener("change", () => store.set("sandbox-speaker", speakerInput.value));

  newChatBtn.addEventListener("click", async () => {
    if (busy) return;
    try {
      await postConversation("new");
      closeRail();
      input.focus();
    } catch (err) {
      setError(err.message);
    }
  });

  rankingSelect.addEventListener("change", () => store.set("playground-ranking", rankingSelect.value));
  let savedRanking = store.get("playground-ranking");
  if (savedRanking === "cohere") savedRanking = "rerank"; // 옛 벤더명 → 중립 이름
  if (savedRanking) rankingSelect.value = savedRanking;

  // ---------- 실험실 ----------

  const labStatus = $("lab-status");
  const labEstimate = $("lab-estimate");
  const labRooms = $("lab-rooms");
  const labRerank = $("lab-rerank");
  const labRanking = $("lab-ranking");
  const labSessionBudget = $("lab-session-budget");
  const labPrepareBudget = $("lab-prepare-budget");
  const labLeafSize = $("lab-leaf-size");
  const labLeafLimit = $("lab-leaf-limit");
  const labSummary = $("lab-summary");
  const labBlocks = $("lab-blocks");
  const labCollection = $("lab-collection");

  function setLabStatus(text) {
    labStatus.textContent = text || "";
  }
  function setLabBlocksStatus(text) {
    $("lab-blocks-status").textContent = text || "";
  }

  async function renderLabBlocks(statusData) {
    const payload = await getJson("/api/blocks");
    const blocks = payload.blocks || [];
    const bot = (statusData && statusData.bot) || {};
    const active = (bot.channels || []).find((c) => c.channel_id === bot.active_channel_id) || {};
    const bound = new Set((active.view && active.view.blocks) || (bot.view && bot.view.blocks) || []);
    labBlocks.replaceChildren();
    if (!blocks.length) {
      setLabBlocksStatus("준비된 블록이 없습니다. 인덱스를 만들면 방마다 생깁니다.");
      return;
    }
    setLabBlocksStatus(`활성 채널에 블록 ${bound.size}개. 같은 임베딩 공간만 붙일 수 있습니다.`);
    for (const block of blocks) {
      labBlocks.append(el("label", { class: "lab-room check" },
        el("input", { type: "checkbox", "data-block-id": block.block_id, checked: bound.has(block.block_id) }),
        el("span", { text: `${block.label} · ${block.platform} · leaf ${Number(block.leaf_count || 0).toLocaleString("ko-KR")}` })));
    }
  }

  function fillLabConfig(cfg) {
    if (!cfg) return;
    labRerank.value = cfg.rerank_model || "";
    labRanking.value = cfg.ranking || "auto";
    labSessionBudget.value = cfg.session_budget_usd ?? "";
    labPrepareBudget.value = cfg.prepare_budget_usd ?? "";
    labLeafSize.value = cfg.leaf_size ?? "";
    labLeafLimit.value = cfg.leaf_limit ?? "";
    labSummary.checked = !!cfg.summary_enabled;
  }

  function renderLabRooms(data) {
    labRooms.replaceChildren();
    if (!labCollection.dataset.ready) {
      labCollection.replaceChildren();
      for (const col of data.collections || []) {
        labCollection.append(el("option", {
          value: col.id,
          selected: col.id === data.active_collection,
          text: `${col.id} (${Number(col.messages || 0).toLocaleString("ko-KR")})`,
        }));
      }
      labCollection.dataset.ready = "1";
    }
    const active = labCollection.value || data.active_collection;
    for (const col of data.collections || []) {
      labRooms.append(el("p", { class: "hint", text: `${col.id} · 방 ${col.room_count} · 메시지 ${Number(col.messages || 0).toLocaleString("ko-KR")} · 인덱스 ${col.indexed_rooms || 0}` }));
      for (const room of col.rooms || []) {
        labRooms.append(el("label", { class: "lab-room check" },
          el("input", {
            type: "checkbox",
            "data-room-id": room.id,
            "data-collection": col.id,
            checked: !!room.indexed && col.id === data.active_collection && col.id === active,
            disabled: col.id !== active,
          }),
          el("span", { text: `${room.label} · ${Number(room.messages).toLocaleString("ko-KR")}개${room.indexed ? " · 붙어 있음" : ""}` })));
      }
    }
  }

  async function loadLab(refresh) {
    setLabStatus(refresh ? "원문 목록을 다시 세는 중…" : "원문 목록을 불러오는 중…");
    const [inv, cfg] = await Promise.all([
      getJson("/api/lab/inventory" + (refresh ? "?refresh=1" : "")),
      getJson("/api/lab/config"),
    ]);
    labInventory = inv;
    fillLabConfig(cfg);
    if (refresh) delete labCollection.dataset.ready;
    renderLabRooms(labInventory);
    setLabStatus(labInventory.disclaimer || "");
  }

  labCollection.addEventListener("change", () => {
    if (labInventory) renderLabRooms(labInventory);
  });
  $("lab-refresh").addEventListener("click", () => loadLab(true).catch(() => setLabStatus("새로고침 실패")));
  $("lab-estimate-all").addEventListener("click", async () => {
    setLabStatus("전체 예상 계산 중…");
    try {
      const est = await postJson("/api/lab/estimate", { collections: ["all"], uncapped: true, summary_enabled: labSummary.checked });
      const usd = est.usd || {};
      labEstimate.hidden = false;
      labEstimate.textContent =
        `메시지 ${Number(est.messages || 0).toLocaleString("ko-KR")} · leaf ${Number(est.leaves || 0).toLocaleString("ko-KR")} · ` +
        `예상 $${usd.low}–$${usd.high} (중간 $${usd.mid}, 요약 $${usd.llm}, 임베딩 $${usd.embed}). ${est.disclaimer || ""}`;
      setLabStatus("전체 원문 준비 기준 예상입니다. 제품 승격이 아닙니다.");
    } catch (err) {
      setLabStatus(err.message);
    }
  });
  $("lab-save-config").addEventListener("click", async () => {
    try {
      const data = await postJson("/api/lab/config", {
        rerank_model: labRerank.value,
        ranking: labRanking.value,
        session_budget_usd: Number(labSessionBudget.value),
        prepare_budget_usd: Number(labPrepareBudget.value),
        leaf_size: Number(labLeafSize.value),
        leaf_limit: Number(labLeafLimit.value),
        summary_enabled: labSummary.checked,
      });
      setLabStatus(data.note || "저장했습니다.");
    } catch (err) {
      setLabStatus(err.message);
    }
  });
  $("lab-save-rooms").addEventListener("click", async () => {
    const chosen = [...labRooms.querySelectorAll("input[type=checkbox]")].filter((b) => b.checked && !b.disabled).map((b) => b.dataset.roomId);
    try {
      await postJson("/api/lab/config", { rooms: chosen, collection: labCollection.value });
      setLabStatus("방을 저장했습니다. 인덱스는 prepare를 다시 돌려야 합니다.");
    } catch (err) {
      setLabStatus(err.message);
    }
  });
  $("lab-save-view").addEventListener("click", async () => {
    const chosen = [...labBlocks.querySelectorAll("input[type=checkbox]")].filter((b) => b.checked).map((b) => b.dataset.blockId);
    try {
      const data = await postJson("/api/bot", { view_blocks: chosen });
      setLabBlocksStatus(`묶음 저장 · revision ${data.view?.revision || "?"}`);
      refreshStatus().catch(() => {});
    } catch (err) {
      setLabBlocksStatus(err.message);
    }
  });
  $("lab-export").addEventListener("click", async () => {
    const data = await getJson("/api/lab/export");
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const a = el("a", { href: URL.createObjectURL(blob), download: "sandbox-lab.json" });
    a.click();
    URL.revokeObjectURL(a.href);
    setLabStatus("설정 파일을 받았습니다. 원문·키는 들어 있지 않습니다.");
  });
  $("lab-import").addEventListener("change", async (ev) => {
    const file = ev.target.files && ev.target.files[0];
    if (!file) return;
    try {
      await getJson("/api/lab/import", { method: "POST", headers: { "Content-Type": "application/json" }, body: await file.text() });
      setLabStatus("가져왔습니다. 인덱스는 prepare가 필요할 수 있습니다.");
      loadLab(false).catch(() => {});
    } catch (err) {
      setLabStatus(err.message);
    }
  });

  // ---------- 시작 ----------

  renderTemplates($("templates"));
  renderTemplates(templatePopover, () => setTemplatesOpen(false));
  updateRunStats();
  autoGrow();
  refreshStatus().catch(() => {
    setError("상태를 불러오지 못했습니다. 서버가 켜져 있는지 확인해 주세요.");
  });
  pollPrepare();
  input.focus();
})();
