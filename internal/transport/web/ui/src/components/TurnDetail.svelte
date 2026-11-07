<script lang="ts">
  import { outcomeTone } from '../lib/outcomes'
  import { shownIn } from '../lib/evidence'
  import { t, toolName } from '../lib/i18n.svelte'
  import { usd, ms, num, datetime } from '../lib/format'
  import Status from './Status.svelte'
  import Button from './Button.svelte'
  import JsonView from './JsonView.svelte'
  import * as I from '../lib/icons'
  // One turn as a timeline: message → decision → tools → evidence → answer.
  let { turn, ledger = null }: { turn: any; ledger?: any } = $props()
  const b = $derived(turn.body ?? {})
  const tools = $derived((b.tools ?? []) as { tool: string; ok: boolean; status?: string; error?: string; data?: any }[])
  const hits = $derived((b.hits ?? []) as any[])
  const plan = $derived((b.plan ?? []) as { tool_name: string; parameters?: any; state?: string }[])
  // body-free call records written next to the turn (model calls, tool calls, Discord delivery)
  const llm = $derived((ledger?.llm_calls ?? []) as any[])
  const toolCalls = $derived((ledger?.tool_calls ?? []) as any[])
  const deliveries = $derived((ledger?.delivery ?? []) as any[])
  // how the room's conversation was chosen for this reply: counts and reasons only
  const attribution = $derived((b.context?.attribution ?? {}) as Record<string, any>)
  const reasons = $derived(Object.entries((attribution.reasons ?? {}) as Record<string, number>))
  const cls = (c: string) => (c ? t(`turns.class.${c}`) : '')
  // what the turn did with the pictures it was called with (chat.vision): body-free mode and reasons
  function visionLine(v: { mode: string; images?: number; model?: string; skipped?: string[] }) {
    const why = [...new Set(v.skipped ?? [])].map((r) => t(`turns.vision.reason.${r}`)).join(', ')
    const head = v.mode === 'native' ? t('turns.vision.native', { n: v.images ?? 0 }) : v.mode === 'described' ? t('turns.vision.described', { n: v.images ?? 0, model: v.model ?? '' }) : t('turns.vision.skipped')
    return why ? `${head} · ${why}` : head
  }
  const tokens = (c: any) => `${num(c.prompt_tokens)} / ${num(c.completion_tokens)} / ${num(c.cached_tokens)}`
  const tone = $derived(outcomeTone(turn.outcome))
  const labHref = $derived(`/lab?guild=${encodeURIComponent(turn.guild_id)}&channel=${encodeURIComponent(turn.channel_id)}&q=${encodeURIComponent(b.message ?? '')}`)
</script>

<article class="td">
  <header>
    <Status tone={tone as any} label={t(`outcome.${turn.outcome}`)} />
    <span class="small muted num">{datetime(turn.at, true)}</span>
    {#if turn.lab}<span class="chip">{t('turns.lab')}</span>{/if}
    {#if turn.feedback}<span class="chip" title={t('turns.feedback_title')}>{#if turn.feedback.good}👍 {turn.feedback.good} {/if}{#if turn.feedback.bad}👎 {turn.feedback.bad}{/if}{#if turn.feedback.reasons?.length} · {turn.feedback.reasons.map((r: string) => t(`turns.feedback_reason.${r}`)).join(', ')}{/if}</span>{/if}
    <span class="spacer"></span>
    <span class="small num">{ms(turn.latency_ms)} · {usd(turn.cost_usd)}</span>
  </header>

  {#if b.body_free}<p class="small muted">{t(`turns.body_free.${b.body_free}`)}</p>{/if}
  {#if b.name_call?.decision}<p class="small">{t('turns.name_call_line', { decision: t(`turns.name_call.${b.name_call.decision}`) })}{#if b.name_call.tone} · {t(`turns.tone.${b.name_call.tone}`)}{/if}</p>{/if}

  <ol class="tl">
    <li>
      <span class="dot"><I.MessageSquareText size={14} aria-hidden="true" /></span>
      <div><h3>{t('turns.step.message')}</h3><p class="speaker small muted">{b.speaker} · <a href="/rooms/{turn.guild_id}/{turn.channel_id}">#{turn.channel_id}</a></p><p class="msg">{#if b.message}{b.message}{:else}<span class="muted">{t(b.body_free ? 'turns.no_text' : 'turns.no_message')}</span>{/if}</p>{#if b.vision}<p class="xs muted">{visionLine(b.vision)}</p>{/if}</div>
    </li>
    <li>
      <span class="dot"><I.Sparkles size={14} aria-hidden="true" /></span>
      <div>
        <h3>{t('turns.step.decision')}</h3>
        <p class="small">{t('turns.memory_state', { state: t(`memstate.${b.memory_state ?? 'unknown'}`) })}{b.ranking ? ` · ${t('turns.ranking', { mode: b.ranking })}` : ''}</p>
        {#if attribution.mode}
          <p class="small">{t('turns.ctx.mode', { mode: t(`turns.ctx.mode.${attribution.mode}`) })} · {t('turns.ctx.counts', { included: attribution.included ?? 0, excluded: attribution.excluded ?? 0, candidates: attribution.candidates ?? 0, referenced: attribution.referenced ?? 0 })}</p>
          {#if reasons.length}<p class="xs muted">{#each reasons as [why, n]}<span class="chip mono">{why} ×{n}</span> {/each}</p>{/if}
        {/if}
        {#if plan.length}<ul class="plan">{#each plan as p}<li><code>{p.tool_name}</code>{#if p.state}<span class="chip">{p.state}</span>{/if}{#if p.parameters && Object.keys(p.parameters).length}<span class="small muted mono break"> {JSON.stringify(p.parameters)}</span>{/if}</li>{/each}</ul>{/if}
      </div>
    </li>
    {#if tools.length}
      <li>
        <span class="dot"><I.Wrench size={14} aria-hidden="true" /></span>
        <div>
          <h3>{t('turns.step.tools')}</h3>
          <ul class="tools">
            {#each tools as tr}
              <li><Status tone={tr.ok ? 'ok' : 'danger'} label={toolName(tr.tool)} size="sm" />{#if tr.error}<span class="small err break">{tr.error}</span>{:else if tr.status}<span class="small muted">{tr.status}</span>{/if}</li>
            {/each}
          </ul>
        </div>
      </li>
    {/if}
    {#if hits.length}
      <li>
        <span class="dot"><I.Brain size={14} aria-hidden="true" /></span>
        <div>
          <h3>{t('turns.step.evidence', { n: hits.length })}</h3>
          <ul class="hits">
            {#each hits as h, i}
              <li>
                <div class="row xs muted"><span class="chip">#{h.rank}</span>{h.room} · {h.date}{h.speakers?.length ? ` · ${h.speakers.join(', ')}` : ''}<span class="spacer"></span>{#if h.rerank_score !== undefined}<span class="num">rerank {h.rerank_score.toFixed(3)}</span>{/if}<span class="num">cos {(h.dense_cos ?? 0).toFixed(3)}</span></div>
                {#if h.summary}<p class="small sum">{h.summary}</p>{/if}
                {#if shownIn(hits, i)}<p class="xs muted">{t('evidence.shown_in', { n: shownIn(hits, i) })}</p>{/if}
                <pre class="ex">{h.excerpt}</pre>
              </li>
            {/each}
          </ul>
        </div>
      </li>
    {/if}
    <li>
      <span class="dot accent"><I.Bot size={14} aria-hidden="true" /></span>
      <div><h3>{t('turns.step.answer')}</h3>{#if b.error}<p class="err break">{b.error}</p>{:else}<p class="msg">{b.answer || t('lab.silent')}</p>{/if}</div>
    </li>
    {#if llm.length}
      <li>
        <span class="dot"><I.Activity size={14} aria-hidden="true" /></span>
        <div>
          <h3>{t('turns.step.calls', { n: llm.length })}</h3>
          <ul class="calls">
            {#each llm as c}
              <li>
                <div class="row small"><span class="chip">{t(`role.${c.role}`)}</span><span class="mono break">{c.model}</span>{#if c.fallback}<span class="chip warn">{t('turns.calls.fallback')}</span>{/if}<span class="spacer"></span><span class="num">{ms(c.latency_ms)} · {usd(c.cost_usd, 4)}</span></div>
                <div class="row xs muted"><span class="num" title={t('turns.calls.tokens_hint')}>{t('turns.calls.tokens')} {tokens(c)}</span>{#if c.class}<span class="chip err">{cls(c.class)}</span>{:else if c.finish_reason}<span>{c.finish_reason}</span>{/if}</div>
              </li>
            {/each}
          </ul>
        </div>
      </li>
    {/if}
    {#if toolCalls.length}
      <li>
        <span class="dot"><I.Wrench size={14} aria-hidden="true" /></span>
        <div>
          <h3>{t('turns.step.tool_calls', { n: toolCalls.length })}</h3>
          <ul class="calls">
            {#each toolCalls as c}
              <li>
                <div class="row small"><span class="chip">{toolName(c.tool)}</span>{#if c.provider}<span class="chip mono">{c.provider}</span>{/if}<span class="muted">{c.guard === 'skipped' ? t('turns.tool.skipped') : c.status}</span>{#if c.class}<span class="chip err">{cls(c.class)}</span>{/if}<span class="spacer"></span><span class="num">{ms(c.latency_ms)}{c.cost_usd ? ` · ${usd(c.cost_usd, 4)}` : ''}</span></div>
                <div class="row xs muted">
                  {#if c.param_keys?.length}<span>{t('turns.tool.params')}</span>{#each c.param_keys as k}<span class="chip mono">{k}{#if c.param_sources?.[k]} · {t(`turns.tool.source.${c.param_sources[k]}`)}{/if}</span>{/each}{/if}
                  {#if c.result_chars}<span class="num">{t('turns.tool.result', { n: num(c.result_chars) })}</span>{/if}
                </div>
              </li>
            {/each}
          </ul>
        </div>
      </li>
    {/if}
    {#if deliveries.length}
      <li>
        <span class="dot"><I.Send size={14} aria-hidden="true" /></span>
        <div>
          <h3>{t('turns.step.delivery')}</h3>
          <ul class="calls">
            {#each deliveries as d}
              <li>
                <div class="row small"><Status tone={d.sent === d.parts && !d.class ? 'ok' : 'warn'} label={t('turns.delivery.parts', { sent: d.sent, parts: d.parts })} size="sm" />{#if d.retries}<span class="muted">{t('turns.delivery.retries', { n: d.retries })}</span>{/if}{#if d.class}<span class="chip err">{cls(d.class)}</span>{/if}</div>
                {#if d.reference_dropped}<p class="xs muted">{t('turns.delivery.reference_dropped')}</p>{/if}
              </li>
            {/each}
          </ul>
        </div>
      </li>
    {/if}
  </ol>

  <div class="row">
    <Button href={labHref} size="sm" icon={I.FlaskConical}>{t('turns.rerun')}</Button>
    <Button href="/rooms/{turn.guild_id}/{turn.channel_id}" size="sm" variant="ghost" icon={I.Hash}>{t('turns.room_settings')}</Button>
  </div>
  <JsonView value={turn} />
</article>

<style>
  .td {
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .tl {
    list-style: none;
    margin: 0;
    padding: 0;
    position: relative;
  }
  .tl::before {
    content: '';
    position: absolute;
    inset-inline-start: 13px;
    top: 14px;
    bottom: 14px;
    width: 2px;
    background: var(--border);
  }
  .tl > li {
    position: relative;
    display: flex;
    gap: var(--s-3);
    padding-bottom: var(--s-4);
    animation: enter 360ms var(--ease) both;
  }
  .tl > li:nth-child(2) { animation-delay: 60ms; }
  .tl > li:nth-child(3) { animation-delay: 120ms; }
  .tl > li:nth-child(4) { animation-delay: 180ms; }
  .tl > li:nth-child(5) { animation-delay: 240ms; }
  .tl > li > div {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .dot {
    position: relative;
    z-index: 1;
    display: grid;
    place-items: center;
    flex: none;
    width: 28px;
    height: 28px;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    box-shadow: 0 0 0 3px var(--surface);
  }
  .dot.accent {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  h3 {
    font-size: var(--text-sm);
    padding-top: 4px;
  }
  .msg {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .plan,
  .tools,
  .hits {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .calls {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .calls > li {
    padding: 8px 12px;
    border-radius: var(--r-lg);
    background: var(--surface-2);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .calls .row {
    display: flex;
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
  }
  .chip.err {
    color: var(--danger);
  }
  .chip.warn {
    color: var(--warn);
  }
  .tools li {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .hits li {
    padding: 10px 12px;
    border-radius: var(--r-lg);
    background: var(--surface-2);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .ex {
    margin: 0;
    font-family: var(--font);
    font-size: var(--text-sm);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 180px;
    overflow: auto;
  }
  .sum {
    color: var(--text-muted);
  }
  .err {
    color: var(--danger);
  }
</style>
