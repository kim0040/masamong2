<script lang="ts">
  import { brand } from 'virtual:brand'
  import { scrollable } from '../lib/scrollable'
  import { shownIn } from '../lib/evidence'
  import { emptyScope, stateLabel } from '../lib/blocks'
  import { fly, fade } from 'svelte/transition'
  import { tick } from 'svelte'
  import { t, toolName, i18n } from '../lib/i18n.svelte'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig } from '../lib/config.svelte'
  import { route } from '../lib/router.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import { usd, ms, num, relative } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Button from '../components/Button.svelte'
  import IconButton from '../components/IconButton.svelte'
  import Select from '../components/Select.svelte'
  import Input from '../components/Input.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Switch from '../components/Switch.svelte'
  import Status from '../components/Status.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Confirm from '../components/Confirm.svelte'
  import PlanSimulator from '../components/PlanSimulator.svelte'
  import { personaOptionsFor } from '../lib/persona'
  import * as I from '../lib/icons'

  // The product's own engine as a sandbox: pick a room, ask, see what was retrieved and why, judge it,
  // compare two ranking modes side by side. Nothing is sent to Discord and lab turns never mix with
  // the live conversation log.
  useConfig()
  const blocks = resource<any>('memory.blocks.list')
  const reviews = resource<any>('lab.reviews', () => ({ limit: 300 }), { live: ['audit'] })
  const d = $derived(motionAllowed() ? 1 : 0)

  type LabTurn = { id: number; side: 'A' | 'B'; kind: 'chat' | 'search'; ranking: string; q: string; speaker: string; answer?: string; evidence: any[]; tools?: any[]; plan?: any; timing?: Record<string, number>; cost?: number; rag?: string; error?: string; verdict?: string; pending: boolean; channel?: string; rooms?: string[] }
  let tab = $state(route.query.get('tab') ?? 'chat')
  let kind = $state<'chat' | 'search'>(route.query.get('persona') ? 'chat' : 'search')
  let channelKey = $state(route.query.get('guild') && route.query.get('channel') ? `${route.query.get('guild')}:${route.query.get('channel')}` : '')
  let roomsSel = $state<string[]>((route.query.get('rooms') ?? '').split(',').map((x) => x.trim()).filter(Boolean))
  let rankingA = $state('auto')
  let rankingB = $state('rerank')
  let compare = $state(false)
  let speaker = $state('')
  let msg = $state(route.query.get('q') ?? '')
  let turns = $state<LabTurn[]>([])
  let selectedId = $state<number | null>(null)
  let seq = 0
  let thread: HTMLDivElement | undefined = $state()
  let costOpen = $state(false)
  let persona = $state(route.query.get('persona') ?? '')
  const personas = resource<any>('personas.list')
  // persona choices: the console language first, other languages labelled, hidden ones left out
  const personaOptions = $derived([{ value: '', label: t('lab.persona_room') }, ...personaOptionsFor((personas.data?.personas ?? []) as any[], i18n.lang, persona)])

  // configured rooms and the ones Discord shows, by name (IDs only when the name is unknown)
  const chList = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const channels = $derived.by(() => {
    const guildName = (g: string) => (chList.data?.guilds ?? []).find((x: any) => x.id === g)?.name ?? g
    const out: { value: string; label: string }[] = []
    const seen = new Set<string>()
    for (const ch of (chList.data?.channels ?? []) as any[]) {
      const k = `${ch.guild_id}:${ch.id}`
      seen.add(k)
      out.push({ value: k, label: `#${ch.name || ch.id} · ${guildName(ch.guild_id)}` })
    }
    for (const [g, gv] of Object.entries<any>(cfg.doc?.guilds ?? {})) for (const c of Object.keys(gv?.channels ?? {})) if (!seen.has(`${g}:${c}`)) out.push({ value: `${g}:${c}`, label: `#${c} · ${guildName(g)}` })
    return out
  })
  $effect(() => {
    if (!channelKey && channels.length) channelKey = channels[0].value
  })
  const withMessages = $derived(((blocks.data?.blocks ?? []) as any[]).filter((b) => (b.message_count ?? 0) > 0))
  const allRooms = $derived(withMessages.map((b) => b.source_room as string))
  const roomLabel = (r: string) => withMessages.find((b) => b.source_room === r)?.label ?? r
  const selected = $derived(turns.find((x) => x.id === selectedId) ?? turns.at(-1))
  // an empty result says why when the scope knows: the room has no memory block attached
  const emptyWhy = (tr: LabTurn) => {
    const [g, c] = (tr.channel ?? '').split(':')
    return emptyScope((blocks.data?.blocks ?? []) as any[], tr.rooms?.length || !g || !c ? null : { guild_id: g, channel_id: c })
  }
  const rankings = ['auto', 'hybrid', 'rerank', 'heuristic'].map((v) => ({ value: v, label: t(`lab.rank.${v}`) }))
  const quick = $derived([t('lab.q.1'), t('lab.q.2'), t('lab.q.3'), t('lab.q.4')])

  async function send() {
    const q = msg.trim()
    if (!q) return
    if (kind === 'chat' && !channelKey) return toast('warn', t('lab.pick_channel'))
    if (kind === 'search' && roomsSel.length === 0 && !channelKey) return toast('warn', t('lab.pick_rooms'))
    msg = ''
    const sides: ('A' | 'B')[] = compare ? ['A', 'B'] : ['A']
    const [g, c] = channelKey.split(':')
    await Promise.all(
      sides.map(async (side) => {
        const ranking = side === 'A' ? rankingA : rankingB
        const turn: LabTurn = { id: ++seq, side, kind, ranking, q, speaker: speaker || t('lab.default_speaker'), evidence: [], pending: true, channel: channelKey, rooms: [...roomsSel] }
        turns.push(turn)
        selectedId = turn.id
        await tick()
        thread?.scrollTo({ top: thread.scrollHeight, behavior: motionAllowed() ? 'smooth' : 'auto' })
        const x = turns.find((y) => y.id === turn.id)!
        if (kind === 'chat') {
          const r = await call('chat.respond', { guild_id: g, channel_id: c, message: q, speaker: x.speaker, ranking, detail: 'true', ...(persona ? { persona_ref: persona } : {}) })
          x.pending = false
          if (!r.ok) x.error = r.error?.message
          else Object.assign(x, { answer: r.output.answer, evidence: r.output.evidence ?? [], tools: r.output.tools, plan: r.output.plan, timing: r.output.timing_ms, cost: r.output.cost_usd, rag: r.output.rag_state })
        } else {
          const input: any = { query: q, ranking, top_k: 8 }
          if (roomsSel.length) input.rooms = roomsSel
          else Object.assign(input, { guild_id: g, channel_id: c })
          const r = await call('memory.search', input)
          x.pending = false
          if (!r.ok) x.error = r.error?.message
          else Object.assign(x, { evidence: r.output.hits ?? [], rag: r.output.rag_state, answer: undefined })
        }
      }),
    )
  }
  function keydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault()
      void send()
    }
  }
  async function judge(tr: LabTurn, verdict: 'good' | 'bad' | 'unsure') {
    const r = await call('lab.review', { verdict, question: tr.q, answer: tr.answer ?? (tr.evidence[0]?.excerpt ?? ''), channel_key: channelKey, tags: [tr.kind, tr.ranking], meta: { ranking: tr.ranking, kind: tr.kind, hits: tr.evidence.length, cost: tr.cost ?? 0, rag: tr.rag, doc_ids: tr.evidence.map((e) => e.doc_id).filter(Boolean), ...(tr.kind === 'search' && roomsSel.length ? { rooms: [...roomsSel] } : {}) } })
    if (r.ok) {
      tr.verdict = verdict
      toast('ok', t('lab.judged'))
      void reviews.refresh()
    } else toast('error', r.error?.message ?? '')
  }
  const totalCost = $derived(turns.reduce((a, x) => a + (x.cost ?? 0), 0))
  const totalMs = (tm?: Record<string, number>) => Object.values(tm ?? {}).reduce((a, b) => a + b, 0)

  // batch re-search of the review set (free: search only, no model call)
  let batch = $state<{ q: string; a: number; b: number; topA?: string; topB?: string }[]>([])
  let batching = $state(false)
  async function runBatch() {
    batching = true
    batch = []
    const qs = [...new Set(((reviews.data?.reviews ?? []) as any[]).map((r) => r.question as string))].slice(0, 40)
    const [g, c] = channelKey.split(':')
    for (const q of qs) {
      const base: any = roomsSel.length ? { rooms: roomsSel } : { guild_id: g, channel_id: c }
      const [ra, rb] = await Promise.all([call('memory.search', { ...base, query: q, ranking: rankingA, top_k: 5 }), call('memory.search', { ...base, query: q, ranking: rankingB, top_k: 5 })])
      if (!ra.ok || !rb.ok) {
        toast('error', t('common.failed'), (ra.ok ? rb.error : ra.error)?.message)
        batching = false
        return
      }
      batch.push({ q, a: ra.output?.hits?.length ?? 0, b: rb.output?.hits?.length ?? 0, topA: ra.output?.hits?.[0]?.excerpt, topB: rb.output?.hits?.[0]?.excerpt })
    }
    batching = false
  }
  // scored evaluation over the review set (eval.run): search only, saved runs
  let evalModes = $state<string[]>(['auto', 'hybrid'])
  let evalTopK = $state('5')
  const evalPlan = resource<any>('eval.run', () => ({ dry_run: true, modes: evalModes, top_k: Number(evalTopK) }), { live: ['audit'] })
  const evalRuns = resource<any>('eval.runs', () => ({}), { live: ['eval'] })
  let evalResult = $state<any>(null)
  let evaluating = $state(false)
  async function runEval() {
    evaluating = true
    const r = await call('eval.run', { modes: evalModes, top_k: Number(evalTopK) })
    evaluating = false
    if (r.ok) {
      evalResult = r.output
      void evalRuns.refresh()
    } else toast('error', t('lab.eval_failed'), r.error?.message)
  }
  async function openRun(id: string) {
    const r = await call('eval.runs', { id })
    if (r.ok) {
      const run = (r.output.runs ?? []).find((x: any) => x.id === id)
      evalResult = { id, params: run?.params, summary: run?.summary, items: r.output.items ?? [] }
    } else toast('error', t('common.failed'), r.error?.message)
  }
  const pct = (v: number | undefined) => (v === undefined || v === null ? '–' : `${Math.round(v * 100)}%`)
  const failures = $derived(((evalResult?.items ?? []) as any[]).filter((it) => Object.values<any>(it.modes).some((m) => m.error || (it.verdict === 'good' ? m.retention < 1 : !m.changed))))
  let delFor = $state('')
  let delOpen = $state(false)
  async function delReview() {
    const r = await call('lab.review', { delete: delFor })
    if (r.ok) void reviews.refresh()
    else toast('error', t('common.failed'), r.error?.message)
  }
  function exportReviews() {
    const blob = new Blob([JSON.stringify(reviews.data?.reviews ?? [], null, 2)], { type: 'application/json' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = 'masamong-lab-reviews.json'
    a.click()
    URL.revokeObjectURL(a.href)
  }
</script>

<Confirm bind:open={delOpen} title={t('lab.delete_title')} body={t('lab.delete_body')} confirmLabel={t('lab.delete_confirm')} danger onconfirm={delReview} />

<PageHeader title={t('lab.title')} description={t('lab.desc')}>
  {#snippet actions()}
    <span class="chip accent"><I.FlaskConical size={12} aria-hidden="true" />{t('lab.badge')}</span>
    {#if turns.length}<span class="small muted num">{t('lab.session_cost', { cost: usd(totalCost) })}</span><Button size="sm" variant="ghost" icon={I.Trash2} onclick={() => (turns = [])}>{t('lab.clear')}</Button>{/if}
  {/snippet}
</PageHeader>

<div data-tour="lab-tabs"><Tabs id="lab" bind:active={tab} tabs={[{ id: 'chat', label: t('lab.tab.chat'), icon: I.MessageSquareText }, { id: 'reviews', label: t('lab.tab.reviews'), icon: I.ThumbsUp, count: reviews.data?.reviews?.length }, { id: 'eval', label: t('lab.tab.eval'), icon: I.GitCompare }, { id: 'plan', label: t('lab.tab.plan'), icon: I.Workflow }]} /></div>

<div class="panel" role="tabpanel" id="lab-panel-{tab}" aria-labelledby="lab-tab-{tab}">
  {#if tab === 'chat'}
    <div class="lab">
      <aside class="side" data-tour="lab-setup">
        <Card title={t('lab.setup')}>
          <div class="stack">
            <Segmented label={t('lab.kind')} bind:value={kind} options={[{ value: 'search', label: t('lab.kind.search'), icon: I.Search }, { value: 'chat', label: t('lab.kind.chat'), icon: I.Bot }]} />
            <p class="xs muted">{t(`lab.kind_hint.${kind}`)}</p>
            <label class="lbl" for="ch">{t('lab.channel')}</label>
            <Select id="ch" bind:value={channelKey} options={[{ value: '', label: t('lab.channel_none') }, ...channels]} />
            {#if kind === 'search'}
              <div>
                <div class="lbl">{t('lab.rooms')}</div>
                <div class="rooms">
                  {#each allRooms as r}
                    <button type="button" class="room" title={r} class:on={roomsSel.includes(r)} aria-pressed={roomsSel.includes(r)} onclick={() => (roomsSel = roomsSel.includes(r) ? roomsSel.filter((x) => x !== r) : [...roomsSel, r])}>{roomLabel(r)}</button>
                  {:else}<span class="xs muted">{t('index.no_rooms')}</span>{/each}
                </div>
                <p class="xs muted">{roomsSel.length ? t('lab.rooms_n', { n: roomsSel.length }) : t('lab.rooms_channel')}</p>
              </div>
            {:else}
              <label class="lbl" for="sp">{t('lab.speaker')}</label>
              <Input id="sp" bind:value={speaker} placeholder={t('lab.default_speaker')} />
            {/if}
            {#if kind === 'chat'}
              <label class="lbl" for="pe">{t('lab.persona')}</label>
              <Select id="pe" bind:value={persona} options={personaOptions} />
            {/if}
            <label class="lbl" for="ra">{compare ? t('lab.ranking_a') : t('lab.ranking')}</label>
            <Select id="ra" bind:value={rankingA} options={rankings} />
            <Switch bind:checked={compare} label={t('lab.compare')} hint={t('lab.compare_hint')} />
            {#if compare}
              <label class="lbl" for="rb">{t('lab.ranking_b')}</label>
              <Select id="rb" bind:value={rankingB} options={rankings} />
            {/if}
          </div>
        </Card>
      </aside>

      <section class="conv" aria-label={t('lab.conversation')}>
        <div class="thread" bind:this={thread} aria-live="polite">
          {#if turns.length === 0}
            <div class="welcome">
              <img src={brand('mascot/mong-lab.webp')} alt="" width="140" height="140" />
              <h2>{t('lab.welcome')}</h2>
              <p class="small muted">{t('lab.welcome_body')}</p>
              <div class="quick">{#each quick as q}<button type="button" onclick={() => { msg = q }}>{q}</button>{/each}</div>
            </div>
          {/if}
          {#each turns as tr (tr.id)}
            <div class="turn" class:sel={selected?.id === tr.id} in:fly={{ y: 12, duration: 280 * d }}>
              <div class="q"><span class="who">{tr.speaker}</span><p>{tr.q}</p></div>
              <button type="button" class="a" onclick={() => (selectedId = tr.id)} aria-pressed={selected?.id === tr.id}>
                <img src="/brand/logo-mark.svg" alt="" width="26" height="26" />
                <div class="body">
                  <div class="meta xs">
                    {#if compare}<span class="chip accent">{tr.side}</span>{/if}
                    <span class="chip">{t(`lab.rank.${tr.ranking}`)}</span>
                    {#if tr.pending}<Status tone="busy" label={t('lab.thinking')} size="sm" />{:else if tr.error}<Status tone="danger" label={t('common.failed')} size="sm" />{:else}<span class="muted num">{t('lab.hits_n', { n: tr.evidence.length })}{#if tr.kind === 'chat'}{' · '}{ms(totalMs(tr.timing))} · {usd(tr.cost)}{/if}</span>{/if}
                  </div>
                  {#if tr.pending}
                    <div class="dots" aria-hidden="true"><span></span><span></span><span></span></div>
                  {:else if tr.error}
                    <p class="err">{tr.error}</p>
                  {:else if tr.kind === 'chat'}
                    <p class="ans">{tr.answer || t('lab.silent')}</p>
                  {:else}
                    <p class="ans muted small">{tr.evidence[0]?.excerpt ? tr.evidence[0].excerpt.slice(0, 220) : `${t('lab.no_hits')} ${t(`lab.empty.${emptyWhy(tr)}`)}`}</p>
                  {/if}
                </div>
              </button>
              {#if !tr.pending && !tr.error}
                <div class="judge" role="group" aria-label={t('lab.judge')}>
                  {#each [['good', I.ThumbsUp], ['bad', I.ThumbsDown], ['unsure', I.CircleHelp]] as [v, Icon]}
                    <IconButton icon={Icon as any} label={t(`lab.verdict.${v}`)} active={tr.verdict === v} onclick={() => judge(tr, v as any)} size={16} />
                  {/each}
                </div>
              {/if}
            </div>
          {/each}
        </div>
        <div class="composer" data-tour="lab-composer">
          <textarea bind:value={msg} onkeydown={keydown} rows="1" placeholder={t('lab.placeholder')} aria-label={t('lab.message')}></textarea>
          <Button variant="primary" icon={I.Send} onclick={send} disabled={!msg.trim()}>{t('lab.send')}</Button>
        </div>
        <p class="xs muted hint">{kind === 'chat' ? t('lab.chat_cost_note') : t('lab.search_free_note')}</p>
      </section>

      <aside class="diag" aria-label={t('lab.diagnostics')}>
        <Card title={t('lab.diagnostics')}>
          {#if !selected}
            <p class="small muted">{t('lab.diag_empty')}</p>
          {:else}
            {#key selected.id}
              <div class="stack" in:fade={{ duration: 160 * d }}>
                <div class="row xs"><span class="chip">{selected.kind === 'chat' ? t('lab.kind.chat') : t('lab.kind.search')}</span><span class="chip">{t(`lab.rank.${selected.ranking}`)}</span>{#if selected.rag}<span class="chip">{stateLabel(selected.rag)}</span>{/if}</div>
                {#if selected.timing}
                  <div class="timing">
                    {#each Object.entries(selected.timing) as [k, v]}
                      <div class="tbar"><span class="xs muted">{t(`lab.timing.${k}`)}</span><span class="track"><span style:width="{Math.max(4, (100 * v) / Math.max(1, totalMs(selected.timing)))}%"></span></span><span class="xs num">{ms(v)}</span></div>
                    {/each}
                  </div>
                {/if}
                {#if selected.tools?.length}
                  <div><div class="section-title">{t('turns.step.tools')}</div>
                    {#each selected.tools as tl}<div class="row small" style:margin-top="6px"><Status tone={tl.ok ? 'ok' : 'danger'} label={toolName(tl.tool)} size="sm" />{#if tl.error}<span class="xs err">{tl.error}</span>{/if}</div>{/each}
                  </div>
                {/if}
                <div class="section-title">{t('turns.step.evidence', { n: selected.evidence.length })}</div>
                {#each selected.evidence as h, i}
                  <article class="hit" in:fly={{ y: 8, duration: 220 * d, delay: i * 40 * d }}>
                    <div class="row xs muted"><span class="chip">#{h.rank ?? i + 1}</span><span class="ellipsis">{h.room ?? h.source_room_id}</span><span class="spacer"></span>{#if h.rerank_score !== undefined}<span class="num">R {Number(h.rerank_score).toFixed(3)}</span>{/if}<span class="num">cos {Number(h.dense_cos ?? 0).toFixed(3)}</span></div>
                    <div class="xs muted">{h.date}{h.speakers?.length ? ` · ${h.speakers.join(', ')}` : ''}</div>
                    {#if h.summary}<p class="xs sum">{h.summary}</p>{/if}
                    {#if shownIn(selected.evidence, i)}<p class="xs muted shown">{t('evidence.shown_in', { n: shownIn(selected.evidence, i) })}</p>{/if}
                    <pre>{h.excerpt}</pre>
                  </article>
                {:else}
                  <p class="small muted">{t('lab.no_hits')}</p>
                  <p class="small muted">{t(`lab.empty.${emptyWhy(selected)}`)}</p>
                  {#if emptyWhy(selected) === 'no_block'}<Button size="sm" href="/blocks" icon={I.Link2}>{t('lab.empty.attach')}</Button>{/if}
                {/each}
              </div>
            {/key}
          {/if}
        </Card>
      </aside>
    </div>
  {:else if tab === 'reviews'}
    <Card padded={false} title={t('lab.reviews_title')} description={t('lab.reviews_desc', { good: reviews.data?.counts?.good ?? 0, bad: reviews.data?.counts?.bad ?? 0, unsure: reviews.data?.counts?.unsure ?? 0 })}>
      {#snippet actions()}<Button size="sm" icon={I.Download} onclick={exportReviews} disabled={!reviews.data?.reviews?.length}>{t('lab.export')}</Button>{/snippet}
      {#if !(reviews.data?.reviews ?? []).length}
        <EmptyState compact pose="lab" title={t('lab.reviews_empty')} description={t('lab.reviews_empty_body')} />
      {:else}
        <div use:scrollable class="table-scroll">
          <table class="table">
            <thead><tr><th>{t('lab.col.question')}</th><th>{t('lab.col.verdict')}</th><th>{t('lab.col.tags')}</th><th>{t('lab.col.when')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {#each reviews.data.reviews as rv (rv.id)}
                <tr><td class="q2"><div class="ellipsis" title={rv.question}>{rv.question}</div><div class="xs subtle ellipsis" title={rv.answer}>{rv.answer}</div></td>
                  <td><Status tone={rv.verdict === 'good' ? 'ok' : rv.verdict === 'bad' ? 'danger' : 'neutral'} label={t(`lab.verdict.${rv.verdict}`)} size="sm" /></td>
                  <td>{#each rv.tags as tg}<span class="chip">{tg}</span> {/each}</td>
                  <td class="muted nowrap">{relative(rv.at)}</td>
                  <td class="r"><IconButton icon={I.Trash2} label={t('common.delete')} tone="danger" onclick={() => { delFor = rv.id; delOpen = true }} size={16} /></td></tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card>
  {:else if tab === 'plan'}
    <PlanSimulator />
  {:else}
    <div class="stack">
      <Card title={t('lab.eval_title')} description={t('lab.eval_desc')}>
        {#snippet actions()}<Button variant="primary" icon={I.Play} loading={evaluating} disabled={!evalPlan.data?.cases || !evalModes.length || !can('eval.run')} onclick={runEval}>{t('lab.eval_run')}</Button>{/snippet}
        <div class="stack">
          <div>
            <div class="lbl">{t('lab.eval_modes')}</div>
            <div class="rooms">
              {#each ['auto', 'hybrid', 'rerank'] as m}
                <button type="button" class="room" class:on={evalModes.includes(m)} aria-pressed={evalModes.includes(m)} onclick={() => (evalModes = evalModes.includes(m) ? evalModes.filter((x) => x !== m) : [...evalModes, m])}>{t(`lab.rank.${m}`)}</button>
              {/each}
            </div>
          </div>
          <div class="row">
            <label class="lbl" for="etk">{t('lab.eval_topk')}</label>
            <div style:width="100px"><Select id="etk" bind:value={evalTopK} options={['3', '5', '8', '10'].map((v) => ({ value: v, label: v }))} /></div>
            <span class="spacer"></span>
            {#if evalPlan.data}<span class="small muted">{t('lab.eval_cases', { n: evalPlan.data.cases })}{` · ${evalPlan.data.may_cost ? t('lab.eval_estimate', { cost: usd(evalPlan.data.estimate_usd) }) : t('lab.eval_free')}`}</span>{/if}
          </div>
          {#if evalPlan.data && evalPlan.data.cases === 0}<div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('lab.eval_need_reviews')}</span></div>{/if}
        </div>
      </Card>
      {#if evalResult}
        <Card padded={false} title={t('lab.eval_result')} description={t('lab.eval_result_desc', { n: evalResult.params?.cases ?? 0, space: evalResult.params?.embed_space ?? '' })} class="enter">
          <div use:scrollable class="table-scroll"><table class="table">
            <thead><tr><th>{t('lab.ranking')}</th><th class="r">{t('lab.eval.retention')}</th><th class="r">{t('lab.eval.fixed')}</th><th class="r">{t('lab.eval.empty')}</th><th class="r">{t('lab.eval.errors')}</th><th class="r">{t('lab.eval.latency')}</th></tr></thead>
            <tbody>
              {#each Object.entries<any>(evalResult.summary ?? {}) as [m, sm]}
                <tr><td><strong>{t(`lab.rank.${m}`)}</strong></td><td class="r num"><span class="score" style:--p={sm.retention ?? 0}>{pct(sm.retention)}</span><div class="xs muted">{t('lab.eval.of_good', { n: sm.good })}</div></td>
                  <td class="r num">{pct(sm.fixed_rate)}<div class="xs muted">{t('lab.eval.of_bad', { n: sm.bad })}</div></td><td class="r num">{sm.empty}</td><td class="r num">{sm.errors}</td><td class="r num">{ms(sm.avg_latency_ms)}</td></tr>
              {/each}
            </tbody>
          </table></div>
          {#if failures.length}
            <details class="fails"><summary class="small">{t('lab.eval_failures', { n: failures.length })}</summary>
              <div use:scrollable class="table-scroll"><table class="table">
                <thead><tr><th>{t('lab.col.question')}</th><th>{t('lab.col.verdict')}</th>{#each Object.keys(evalResult.summary ?? {}) as m}<th class="r">{t(`lab.rank.${m}`)}</th>{/each}<th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
                <tbody>{#each failures as it}<tr><td class="q2 ellipsis" title={it.question}>{it.question}</td><td><Status tone={it.verdict === 'good' ? 'ok' : 'danger'} label={t(`lab.verdict.${it.verdict}`)} size="sm" /></td>
                  {#each Object.keys(evalResult.summary ?? {}) as m}<td class="r num small">{#if it.modes[m]?.error}<span class="err">!</span>{:else if it.verdict === 'good'}{pct(it.modes[m]?.retention)}{:else}{it.modes[m]?.changed ? t('lab.eval.changed') : t('lab.eval.same')}{/if}</td>{/each}
                  <td class="r"><Button size="sm" variant="ghost" href="/lab?q={encodeURIComponent(it.question)}" iconEnd={I.ArrowRight}>{t('lab.eval.try')}</Button></td></tr>{/each}</tbody>
              </table></div>
            </details>
          {/if}
        </Card>
      {/if}
      <Card padded={false} title={t('lab.eval_history')} description={t('lab.eval_history_desc')}>
        {#if (evalRuns.data?.runs ?? []).length === 0}
          <p class="small muted pad">{t('lab.eval_history_empty')}</p>
        {:else}
          <div use:scrollable class="table-scroll"><table class="table">
            <thead><tr><th>{t('lab.col.when')}</th><th>{t('lab.eval.generation')}</th>{#each ['auto', 'hybrid', 'rerank'] as m}<th class="r">{t(`lab.rank.${m}`)}</th>{/each}<th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {#each evalRuns.data.runs as run (run.id)}
                <tr><td class="nowrap muted">{relative(run.at)}</td><td class="xs mono break">{run.params?.embed_space ?? ''} · {run.params?.recipe ?? ''}</td>
                  {#each ['auto', 'hybrid', 'rerank'] as m}<td class="r num">{pct(run.summary?.[m]?.retention)}</td>{/each}
                  <td class="r"><Button size="sm" variant="ghost" onclick={() => openRun(run.id)}>{t('lab.eval_open')}</Button></td></tr>
              {/each}
            </tbody>
          </table></div>
        {/if}
      </Card>
      <Card title={t('lab.batch_title')} description={t('lab.batch_desc', { a: t(`lab.rank.${rankingA}`), b: t(`lab.rank.${rankingB}`) })}>
        {#snippet actions()}<Button icon={I.Play} loading={batching} disabled={!(reviews.data?.reviews ?? []).length} onclick={runBatch}>{t('lab.batch_run')}</Button>{/snippet}
        {#if batch.length}
          <div use:scrollable class="table-scroll">
            <table class="table">
              <thead><tr><th>{t('lab.col.question')}</th><th class="r">A</th><th class="r">B</th><th>{t('lab.top_a')}</th><th>{t('lab.top_b')}</th></tr></thead>
              <tbody>{#each batch as b}<tr><td class="q2 ellipsis" title={b.q}>{b.q}</td><td class="r num">{b.a}</td><td class="r num">{b.b}</td><td class="xs muted q2 ellipsis">{b.topA ?? '–'}</td><td class="xs muted q2 ellipsis">{b.topB ?? '–'}</td></tr>{/each}</tbody>
            </table>
          </div>
        {:else}
          <p class="small muted">{t('lab.batch_none')}</p>
        {/if}
      </Card>
    </div>
  {/if}
</div>

<style>
  .panel {
    margin-top: var(--s-4);
  }
  .fails {
    padding: var(--s-2) var(--pad-card) var(--s-3);
  }
  .fails summary {
    cursor: pointer;
    color: var(--text-muted);
    padding: 6px 0;
  }
  .pad {
    padding: var(--s-4) var(--pad-card);
  }
  .score {
    font-weight: 700;
  }
  .lab {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
    align-items: start;
  }
  @media (min-width: 1100px) {
    .lab {
      grid-template-columns: 280px minmax(0, 1fr);
    }
    .diag {
      grid-column: 1 / -1;
    }
  }
  @media (min-width: 1500px) {
    .lab {
      grid-template-columns: 280px minmax(0, 1fr) minmax(360px, 0.8fr);
    }
    .diag {
      grid-column: auto;
      position: sticky;
      top: calc(var(--topbar-offset) + var(--s-4));
      max-height: calc(100dvh - var(--topbar-offset) - var(--s-8));
      overflow: auto;
    }
  }
  .side :global(.card) {
    position: sticky;
    top: calc(var(--topbar-offset) + var(--s-4));
    transition: top var(--dur) var(--ease);
  }
  .lbl {
    font-size: var(--text-sm);
    font-weight: 550;
  }
  .rooms {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: 8px 0 4px;
  }
  .room {
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    height: 28px;
    padding: 0 10px;
    border: 0;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .room.on {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .conv {
    display: flex;
    flex-direction: column;
    min-height: min(78dvh, 820px);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .thread {
    flex: 1;
    overflow: auto;
    padding: var(--s-5);
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
    max-height: 72dvh;
  }
  .welcome {
    margin: auto;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--s-2);
    text-align: center;
    max-width: 480px;
  }
  .quick {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    justify-content: center;
    margin-top: var(--s-3);
  }
  .quick button {
    padding: 6px 12px;
    border: 0;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text);
    font-size: var(--text-sm);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-out);
  }
  .quick button:hover {
    background: var(--accent-soft);
  }
  .turn {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .q {
    align-self: flex-end;
    max-width: 80%;
    padding: 10px 14px;
    border-radius: 16px 16px 4px 16px;
    background: var(--accent-soft);
  }
  .q p {
    white-space: pre-wrap;
  }
  .who {
    font-size: var(--text-xs);
    color: var(--text-muted);
    font-weight: 650;
  }
  .a {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    max-width: 88%;
    padding: 10px 14px 10px 10px;
    border: 0;
    border-radius: 16px 16px 16px 4px;
    background: var(--surface-2);
    color: var(--text);
    text-align: start;
    cursor: pointer;
    transition: box-shadow var(--dur-fast) var(--ease-out);
  }
  .turn.sel .a {
    box-shadow: 0 0 0 2px var(--accent);
  }
  .a img {
    border-radius: 999px;
    flex: none;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
  }
  .meta {
    display: flex;
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
  }
  .ans {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
  .dots {
    display: flex;
    gap: 4px;
    padding: 6px 0;
  }
  .dots span {
    width: 7px;
    height: 7px;
    border-radius: 999px;
    background: var(--text-subtle);
    animation: bounce 1.2s var(--ease) infinite;
  }
  .dots span:nth-child(2) {
    animation-delay: 0.15s;
  }
  .dots span:nth-child(3) {
    animation-delay: 0.3s;
  }
  @keyframes bounce {
    30% {
      translate: 0 -5px;
      opacity: 0.6;
    }
  }
  .judge {
    display: flex;
    gap: 2px;
    margin-inline-start: 36px;
  }
  .composer {
    display: flex;
    gap: var(--s-2);
    align-items: flex-end;
    padding: var(--s-3);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .composer textarea {
    flex: 1;
    min-height: 40px;
    max-height: 200px;
    field-sizing: content;
    padding: 10px 12px;
    border: 0;
    border-radius: var(--r-lg);
    background: var(--surface-2);
    resize: none;
    font-size: var(--text-md);
    line-height: 1.4;
  }
  .composer textarea:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
  .hint {
    padding: 0 var(--s-4) var(--s-3);
  }
  .timing {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .tbar {
    display: grid;
    grid-template-columns: 70px 1fr 64px;
    align-items: center;
    gap: 8px;
  }
  .track {
    height: 6px;
    border-radius: 999px;
    background: var(--surface-2);
    overflow: hidden;
  }
  .track span {
    display: block;
    height: 100%;
    background: var(--chart-2);
    border-radius: inherit;
    animation: bar-in 500ms var(--ease) both;
    transform-origin: left;
  }
  @keyframes bar-in {
    from {
      transform: scaleX(0);
    }
  }
  .hit {
    padding: 10px 12px;
    border-radius: var(--r-lg);
    background: var(--surface-2);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .hit pre {
    margin: 0;
    font-family: var(--font);
    font-size: var(--text-sm);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 200px;
    overflow: auto;
  }
  .sum {
    color: var(--text-muted);
  }
  .q2 {
    max-width: 360px;
  }
</style>
