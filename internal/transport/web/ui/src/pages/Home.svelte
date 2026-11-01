<script lang="ts">
  import { t, toolName } from '../lib/i18n.svelte'
  import { overview, overviewSource, startOverview } from '../lib/overview.svelte'
  import { resource } from '../lib/resource.svelte'
  import { usd, num, ms, relative, duration, shortDate } from '../lib/format'
  import { route } from '../lib/router.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Status from '../components/Status.svelte'
  import Stat from '../components/Stat.svelte'
  import Ring from '../components/Ring.svelte'
  import Chart from '../components/Chart.svelte'
  import Sparkline from '../components/Sparkline.svelte'
  import Button from '../components/Button.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Mascot from '../components/Mascot.svelte'
  import Progress from '../components/Progress.svelte'
  import CardTools from '../components/CardTools.svelte'
  import { prefs, setPrefs } from '../lib/prefs.svelte'
  import { practice, setPractice } from '../lib/api.svelte'
  import { cfg, useConfig } from '../lib/config.svelte'
  import { attentionText, isAttentionRead, markAllAttentionRead, markAttentionRead, type Attention } from '../lib/attention.svelte'
  import * as I from '../lib/icons'
  import { outcomeColor, outcomeTone } from '../lib/outcomes'

  startOverview()
  const series = resource('stats.series', () => ({ days: 14 }), { live: ['turn'] })
  const o = $derived(overview.data)
  const GREET_VARIANTS = 10
  const hour = new Date().getHours()
  const period =
    hour >= 2 && hour < 6
      ? 'dawn'
      : hour >= 6 && hour < 12
        ? 'morning'
        : hour >= 12 && hour < 18
          ? 'afternoon'
          : hour >= 18 && hour < 22
            ? 'evening'
            : 'night'
  const greetIdx = Math.floor(Math.random() * GREET_VARIANTS)
  const today = $derived(o?.today?.turns ?? {})
  const answered = $derived(today.answered ?? 0)
  const refused = $derived((today.limited ?? 0) + (today.budget ?? 0) + (today.paused ?? 0) + (today.disabled ?? 0))
  const failed = $derived(today.error ?? 0)
  const checklist = $derived((o?.checklist ?? []) as { id: string; done: boolean; route: string }[])
  const doneN = $derived(checklist.filter((c) => c.done).length)
  const showChecklist = $derived(checklist.length > 0 && (doneN < checklist.length || route.query.get('checklist') === '1'))

  const statusOrder = ['daemon', 'discord', 'db', 'llm', 'memory', 'jobs']
  const icons: Record<string, any> = { daemon: I.Server, discord: I.Bot, db: I.Database, llm: I.Sparkles, memory: I.Brain, jobs: I.ListChecks }
  const routeFor: Record<string, string> = { daemon: '/logs', discord: '/connections', db: '/maintenance', llm: '/connections', memory: '/blocks', jobs: '/jobs' }
  function tone(s: string): 'ok' | 'warn' | 'danger' | 'neutral' {
    if (s === 'ready' || s === 'ok') return 'ok'
    if (s === 'degraded' || s === 'unconfigured') return 'warn'
    if (s === 'error') return 'danger'
    return 'neutral'
  }
  const chartSeries = $derived(
    Object.entries<number[]>(series.data?.turns ?? {})
      .sort(([a], [b]) => (a === 'answered' ? -1 : b === 'answered' ? 1 : a.localeCompare(b)))
      .map(([k, data], i) => ({ key: k, label: t(`outcome.${k}`), data, color: outcomeColor(k, i) })),
  )
  const labels = $derived((series.data?.days ?? []).map(shortDate))
  const latency = $derived((series.data?.latency_ms ?? []) as number[])
  const answeredSeries = $derived((series.data?.turns?.answered ?? []) as number[])
  // layout: order and hidden cards are per-admin preferences
  useConfig()
  // the month's spend by kind, largest first: the four biggest by name, the rest (and the unused) in one line
  const spendKinds = $derived(Object.entries<number>(o?.month?.by_kind ?? {}).sort((a, b) => b[1] - a[1]))
  const spendTop = $derived(spendKinds.filter(([, v]) => v > 0).slice(0, 4))
  const spendRest = $derived(spendKinds.filter(([k]) => !spendTop.some(([x]) => x === k)))
  const cards = ['kpis', 'budget', 'attention', 'checklist', 'activity', 'tools', 'jobs', 'recent'] as const
  type CardId = (typeof cards)[number]
  let editing = $state(false)
  let dragging = $state<string | null>(null)
  let attentionExpanded = $state(false)
  let checklistExpanded = $state(false)
  const attentionItems = $derived((o?.attention ?? []) as Attention[])
  const visibleAttention = $derived(attentionExpanded ? attentionItems : attentionItems.slice(0, 4))
  const unreadAttention = $derived(attentionItems.filter((a) => !isAttentionRead(a)))
  const visibleChecklist = $derived(checklistExpanded ? checklist : checklist.slice(0, 5))
  const order = $derived.by(() => {
    const saved = (prefs.home_order ?? []) as string[]
    const known = saved.filter((c) => (cards as readonly string[]).includes(c))
    return [...known, ...cards.filter((c) => !known.includes(c))] as CardId[]
  })
  const hidden = $derived(new Set(prefs.home_hidden ?? []))
  const visible = $derived(order.filter((c) => !hidden.has(c)))
  const orderVars = $derived(order.map((c, i) => `--o-${c}:${i}`).join(';'))
  function move(id: CardId, dir: -1 | 1) {
    const v = [...visible]
    const i = v.indexOf(id)
    const j = i + dir
    if (j < 0 || j >= v.length) return
    ;[v[i], v[j]] = [v[j], v[i]]
    setPrefs({ home_order: [...v, ...order.filter((c) => hidden.has(c))] })
  }
  function dropOn(target: CardId) {
    if (!dragging || dragging === target) return
    const v = order.filter((c) => c !== dragging)
    v.splice(v.indexOf(target), 0, dragging as CardId)
    setPrefs({ home_order: v })
    dragging = null
  }
  function hide(id: CardId) {
    setPrefs({ home_hidden: [...(prefs.home_hidden ?? []), id] })
  }
  function show(id: string) {
    setPrefs({ home_hidden: (prefs.home_hidden ?? []).filter((x) => x !== id) })
  }
  function resetLayout() {
    setPrefs({ home_hidden: [], home_order: [] })
  }
  const tools = (id: CardId) => ({ name: t(`home.card.${id}`), first: visible[0] === id, last: visible.at(-1) === id, onmove: (d: -1 | 1) => move(id, d), onhide: () => hide(id), ondragstart: () => (dragging = id), ondragend: () => (dragging = null) })
  const dropProps = (id: CardId) => (editing ? { ondragover: (e: DragEvent) => e.preventDefault(), ondrop: (e: DragEvent) => { e.preventDefault(); dropOn(id) } } : {})
  const costDaily = $derived.by(() => {
    const c = series.data?.cost_usd ?? {}
    const days = series.data?.days?.length ?? 0
    return Array.from({ length: days }, (_, i) => Object.values<number[]>(c).reduce((a, s) => a + (s[i] ?? 0), 0))
  })

</script>

<PageHeader title={t(`home.greet.${period}.${greetIdx}`)} docTitle={t('nav.home')} description={o ? t('home.subtitle', { instance: o.instance, uptime: duration(o.uptime_sec), rev: o.revision }) : ''}>
  {#snippet actions()}
    <Button data-tour="edit" variant={editing ? 'primary' : 'ghost'} icon={I.LayoutGrid} aria-pressed={editing} onclick={() => (editing = !editing)}>{editing ? t('home.edit.done') : t('home.edit.start')}</Button>
    <Button href="/lab" icon={I.FlaskConical}>{t('home.test_chat')}</Button>
    <Button href="/turns" variant="ghost" iconEnd={I.ArrowRight}>{t('home.all_turns')}</Button>
  {/snippet}
</PageHeader>

{#if !o}
  <LoadState res={overviewSource}><div class="grid" style:--min="240px">{#each Array(6) as _}<Card><Skeleton lines={2} /></Card>{/each}</div></LoadState>
{:else}
  {#if practice.on}
    <Card tone="muted" class="practice-intro enter">
      <div class="pi">
        <Mascot pose="guide" size={96} />
        <div class="stack" style:gap="8px">
          <strong>{t('practice.home_title')}</strong>
          <p class="small muted">{t('practice.home_body')}</p>
          <div class="row wrap">
            <Button size="sm" href="/lab?guild=practice&channel=lounge&q={encodeURIComponent(t('practice.sample_q'))}" icon={I.FlaskConical}>{t('practice.try_lab')}</Button>
            <Button size="sm" variant="ghost" href="/rooms/practice/lounge" icon={I.Hash}>{t('practice.try_room')}</Button>
            <Button size="sm" variant="ghost" href="/memory" icon={I.Brain}>{t('practice.try_memory')}</Button>
            <Button size="sm" variant="ghost" href="/tools" icon={I.Wrench}>{t('practice.try_tools')}</Button>
          </div>
        </div>
      </div>
    </Card>
  {:else if !cfg.doc?.guilds?.practice && o.revision}
    <!-- nothing: practice is offered from the account menu and the empty activity card -->
  {/if}
  {#if editing}
    <div class="editbar enter" role="region" aria-label={t('home.edit.region')}>
      <span class="small">{t('home.edit.hint')}</span>
      {#if hidden.size}
        <span class="small muted">{t('home.edit.hidden')}</span>
        {#each [...hidden] as h}<button type="button" class="chip addback" onclick={() => show(h)}><I.Plus size={12} aria-hidden="true" />{t(`home.card.${h}`)}</button>{/each}
      {/if}
      <span class="spacer"></span>
      <Button size="sm" variant="ghost" icon={I.RotateCcw} onclick={resetLayout}>{t('home.edit.reset')}</Button>
    </div>
  {/if}
  <!-- status strip -->
  <div class="status-strip" data-tour="status">
    {#each statusOrder as k, i}
      {@const st = o.status[k] ?? { status: 'unknown' }}
      {@const Icon = icons[k]}
      <a class="st-card enter" style:--i={i} href={routeFor[k]}>
        <span class="st-ic {tone(st.status)}"><Icon size={18} strokeWidth={1.75} aria-hidden="true" /></span>
        <span class="st-txt">
          <span class="st-name">{t(`status.${k}`)}</span>
          <Status tone={tone(st.status)} label={t(`state.${st.status}`)} size="sm" />
        </span>
      </a>
    {/each}
  </div>

  <div class="bento" class:editing style={orderVars}>
    <!-- KPIs -->
{#if !hidden.has('kpis')}
    <Card class="kpis {dragging === 'kpis' ? 'drag' : ''}" {...dropProps('kpis')}>
      {#if editing}<CardTools {...tools('kpis')} />{/if}
      <div class="kpi-row">
        <Stat label={t('home.kpi.answered')} value={answered} format={(n) => num(n)}>
          <Sparkline data={answeredSeries} label={t('home.kpi.answered_trend')} />
        </Stat>
        <Stat label={t('home.kpi.refused')} value={refused} format={(n) => num(n)} tone={refused > 0 ? 'warn' : undefined} hint={t('home.kpi.refused_hint')} />
        <Stat label={t('home.kpi.failed')} value={failed} format={(n) => num(n)} tone={failed > 0 ? 'danger' : undefined} />
        <Stat label={t('home.kpi.latency')} value={latency.at(-1) || undefined} format={(n) => ms(n)}>
          {#if latency.some((v) => v > 0)}<Sparkline data={latency} label={t('home.kpi.latency_trend')} color="var(--chart-2)" />{/if}
        </Stat>
      </div>
    </Card>
    {/if}

    <!-- budget -->
    {#if !hidden.has('budget')}
    <Card title={t('home.budget.title')} class="budget {dragging === 'budget' ? 'drag' : ''}" {...dropProps('budget')}>
      {#if editing}<CardTools {...tools('budget')} />{/if}
      {#snippet actions()}<Button href="/usage" size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('home.budget.manage')}</Button>{/snippet}
      <div class="budget-body">
        {#if o.month.budget_usd > 0}
          <Ring value={o.month.spent_usd} max={o.month.budget_usd} label={t('home.budget.title')} center={usd(o.month.spent_usd)} sub={t('home.budget.of', { budget: usd(o.month.budget_usd, 0) })} />
        {:else}
          <Ring value={0} max={1} label={t('home.budget.title')} center={usd(o.month.spent_usd)} sub={t('home.budget.no_limit')} />
        {/if}
        <div class="stack" style:gap="8px">
          <div class="small muted">{t('home.budget.today', { cost: usd(o.today.cost_usd) })}</div>
          <Sparkline data={costDaily} label={t('home.budget.trend')} color="var(--chart-4)" height={44} />
          {#each spendTop as [k, v]}
            <div class="row small"><span class="muted">{t(`spend.${k}`)}</span><span class="spacer"></span><span class="num">{usd(v)}</span></div>
          {/each}
          {#if spendRest.length}
            <div class="row small"><span class="muted">{t('home.budget.rest', { n: spendRest.length })}</span><span class="spacer"></span><span class="num">{usd(spendRest.reduce((a, [, v]) => a + v, 0))}</span></div>
          {/if}
        </div>
      </div>
    </Card>
    {/if}

    <!-- attention -->
    {#if !hidden.has('attention')}
    <Card title={t('home.attention.title')} description={t('home.attention.desc')} class="attention {dragging === 'attention' ? 'drag' : ''}" {...dropProps('attention')} data-tour="attention">
      {#if editing}<CardTools {...tools('attention')} />{/if}
      {#snippet actions()}{#if unreadAttention.length}<Button size="sm" variant="ghost" onclick={() => markAllAttentionRead(attentionItems)}>{t('topbar.mark_all_read')}</Button>{/if}{/snippet}
      {#if o.attention.length === 0}
        <div class="clear"><Mascot pose="party" size={88} /><div><strong>{t('home.attention.clear')}</strong><p class="small muted">{t('home.attention.clear_body')}</p></div></div>
      {:else}
        <ul class="att">
          {#each visibleAttention as a, i}
            <li class="enter" class:read={isAttentionRead(a)} style:--i={i}>
              <Status tone={a.severity === 'danger' ? 'danger' : a.severity === 'warn' ? 'warn' : 'info'} label={t(`severity.${a.severity}`)} size="sm" />
              <span class="att-t">{#if !isAttentionRead(a)}<span class="att-dot" aria-hidden="true"></span><span class="sr-only">{t('topbar.unread')} — </span>{/if}{attentionText(a)}</span>
              <Button href={a.route} size="sm" variant="secondary" onclick={() => markAttentionRead(a)}>{t(`attention.fix.${a.key}`)}</Button>
            </li>
          {/each}
        </ul>
        {#if attentionItems.length > 4}<button type="button" class="att-more" aria-expanded={attentionExpanded} onclick={() => (attentionExpanded = !attentionExpanded)}>{attentionExpanded ? t('home.attention.less') : t('home.attention.more', { n: attentionItems.length - 4 })}</button>{/if}
      {/if}
    </Card>
    {/if}

    {#if showChecklist}
      {#if !hidden.has('checklist')}
      <Card title={t('checklist.title')} description={t('checklist.desc', { done: doneN, total: checklist.length })} class="checklist {dragging === 'checklist' ? 'drag' : ''}" tone="accent" {...dropProps('checklist')}>
        {#if editing}<CardTools {...tools('checklist')} />{/if}
        <Progress value={doneN} max={checklist.length} label={t('checklist.title')} tone={doneN === checklist.length ? 'ok' : 'accent'} />
        <ol class="cl">
          {#each visibleChecklist as c, i}
            <li class:done={c.done} class="enter" style:--i={i}>
              <span class="tick" aria-hidden="true">{#if c.done}<I.Check size={14} strokeWidth={3} />{/if}</span>
              <span class="cl-t">{t(`checklist.${c.id}`)}<span class="sr-only"> — {c.done ? t('common.done') : t('common.todo')}</span></span>
              {#if !c.done}<a href={c.route} class="cl-go">{t('checklist.go')}<I.ArrowRight size={14} aria-hidden="true" /></a>{/if}
            </li>
          {/each}
        </ol>
        {#if checklist.length > 5}<button type="button" class="att-more" aria-expanded={checklistExpanded} onclick={() => (checklistExpanded = !checklistExpanded)}>{checklistExpanded ? t('checklist.less') : t('checklist.more', { n: checklist.length - 5 })}</button>{/if}
      </Card>
      {/if}
    {/if}

    <!-- activity chart -->
    {#if !hidden.has('activity')}
    <Card title={t('home.activity.title')} description={t('home.activity.desc')} class="activity {dragging === 'activity' ? 'drag' : ''}" {...dropProps('activity')}>
      {#if editing}<CardTools {...tools('activity')} />{/if}
      {#snippet actions()}<Button href="/stats" size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('home.activity.more')}</Button>{/snippet}
      {#if series.data}
        {#if chartSeries.length}
          <Chart {labels} series={chartSeries} label={t('home.activity.title')} height={230} />
        {:else}
          <EmptyState compact pose="chart" title={t('home.activity.empty')} description={t('home.activity.empty_body')}><Button href="/lab" size="sm">{t('home.test_chat')}</Button></EmptyState>
        {/if}
      {:else}<Skeleton block height={230} />{/if}
    </Card>
    {/if}

    <!-- tools -->
    {#if !hidden.has('tools')}
    <Card title={t('home.tools.title')} class="tools {dragging === 'tools' ? 'drag' : ''}" {...dropProps('tools')}>
      {#if editing}<CardTools {...tools('tools')} />{/if}
      {#snippet actions()}<Button href="/tools" size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('home.tools.all')}</Button>{/snippet}
      {#if o.top_tools.length === 0}
        <p class="small muted">{t('home.tools.none')}</p>
      {:else}
        {@const max = Math.max(...o.top_tools.map((x: any) => x.n))}
        <ul class="bars">
          {#each o.top_tools as tl, i}
            <li>
              <a href="/tools/{encodeURIComponent(tl.tool)}">{toolName(tl.tool)}</a>
              <span class="bar" aria-hidden="true"><span style:width="{(100 * tl.n) / max}%" style:--i={i}></span></span>
              <span class="num small">{num(tl.n)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </Card>
    {/if}

    <!-- jobs -->
    {#if !hidden.has('jobs')}
    <Card title={t('home.jobs.title')} class="jobs {dragging === 'jobs' ? 'drag' : ''}" {...dropProps('jobs')}>
      {#if editing}<CardTools {...tools('jobs')} />{/if}
      {#snippet actions()}<Button href="/jobs" size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('home.jobs.all')}</Button>{/snippet}
      {#if o.jobs.length === 0}
        <p class="small muted">{t('home.jobs.none')}</p>
      {:else}
        <ul class="jl">
          {#each o.jobs.slice(0, 5) as j}
            <li><span class="small">{t(`jobs.type.${j.type}`)}</span><Status tone="busy" label={t(`jobs.status.${j.status}`)} size="sm" /></li>
          {/each}
        </ul>
        {#if o.jobs.length > 5}<p class="jobs-more xs muted">{t('home.jobs.more', { n: o.jobs.length - 5 })}</p>{/if}
      {/if}
    </Card>
    {/if}

    <!-- recent turns -->
    {#if !hidden.has('recent')}
    <Card title={t('home.recent.title')} class="recent {dragging === 'recent' ? 'drag' : ''}" {...dropProps('recent')} padded={false}>
      {#if editing}<CardTools {...tools('recent')} />{/if}
      {#snippet actions()}<Button href="/turns" size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('home.all_turns')}</Button>{/snippet}
      {#if (o.recent ?? []).length === 0}
        <EmptyState compact pose="think" title={t('home.recent.empty')} description={t('home.recent.empty_body')} />
      {:else}
        <ul class="turns">
          {#each o.recent as tr, i (tr.id)}
            <li class="enter" style:--i={i}>
              <a href="/turns?id={tr.id}">
                <span class="tm small muted num">{relative(tr.at)}</span>
                <span class="msg ellipsis">{#if tr.body?.message}{tr.body.message}{:else}<span class="muted">{t(tr.body?.body_free ? 'turns.no_text' : 'turns.no_message')}</span>{/if}</span>
                <span class="meta">
                  {#each tr.tools as tool}<span class="chip">{toolName(tool)}</span>{/each}
                  {#if tr.hits}<span class="chip accent"><I.Brain size={12} aria-hidden="true" />{tr.hits}</span>{/if}
                  <Status tone={outcomeTone(tr.outcome)} label={t(`outcome.${tr.outcome}`)} size="sm" />
                </span>
              </a>
            </li>
          {/each}
        </ul>
      {/if}
    </Card>
    {/if}
  </div>
{/if}

<style>
  /* six status cards: 6, 3 or 2 to a row, never a lone card on the last row */
  .status-strip {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--s-3);
    margin-bottom: var(--s-4);
  }
  .st-card {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    padding: 12px 14px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text);
    text-decoration: none;
    transition-property: box-shadow, translate;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
    container-type: inline-size;
  }
  /* a narrow card (six to a row, long translations) gives the icon's room to the words */
  @container (max-width: 190px) {
    .st-ic {
      display: none;
    }
  }
  @media (hover: hover) {
    .st-card:hover {
      box-shadow: var(--shadow-border-hover);
      translate: 0 -1px;
    }
  }
  .st-ic {
    flex: none;
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: var(--r);
    background: var(--surface-2);
    color: var(--text-muted);
  }
  .st-ic.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .st-ic.warn {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .st-ic.danger {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .st-txt {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 3px;
    min-width: 0;
  }
  .st-name {
    font-weight: 600;
    font-size: var(--text-sm);
  }
  @media (min-width: 720px) {
    .status-strip {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
  }
  @media (min-width: 1280px) {
    .status-strip {
      grid-template-columns: repeat(6, minmax(0, 1fr));
    }
  }
  /* Cards on one row share its height, and a moved or hidden card leaves no hole: the next one that
     fits fills it (dense). */
  .bento {
    display: grid;
    align-items: stretch;
    grid-auto-flow: row dense;
    gap: var(--s-4);
    grid-template-columns: repeat(12, minmax(0, 1fr));
  }
  .bento > :global(.card) {
    grid-column: span 12;
    position: relative;
  }
  .bento > :global(.kpis) { order: var(--o-kpis, 0); }
  .bento > :global(.budget) { order: var(--o-budget, 1); }
  .bento > :global(.attention) { order: var(--o-attention, 2); }
  .bento > :global(.checklist) { order: var(--o-checklist, 3); }
  .bento > :global(.activity) { order: var(--o-activity, 4); }
  .bento > :global(.tools) { order: var(--o-tools, 5); }
  .bento > :global(.jobs) { order: var(--o-jobs, 6); }
  .bento > :global(.recent) { order: var(--o-recent, 7); }
  .bento.editing > :global(.card) {
    outline: 1.5px dashed var(--border-strong);
    outline-offset: 2px;
  }
  .bento > :global(.card.drag) {
    opacity: 0.5;
  }
  .editbar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--s-2);
    padding: 10px 14px;
    margin-bottom: var(--s-4);
    border-radius: var(--r-lg);
    background: var(--accent-soft);
  }
  .addback {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    border: 0;
    cursor: pointer;
  }
  .pi {
    display: flex;
    gap: var(--s-4);
    align-items: center;
    flex-wrap: wrap;
  }
  .wrap {
    flex-wrap: wrap;
  }
  :global(.practice-intro) {
    margin-bottom: var(--s-4);
    box-shadow: 0 0 0 1.5px var(--practice) !important;
  }
  @media (min-width: 1100px) {
    .bento > :global(:is(.kpis, .attention, .activity)) {
      grid-column: span 8;
    }
    .bento > :global(:is(.budget, .checklist, .tools, .jobs)) {
      grid-column: span 4;
    }
    /* the chart takes two rows so today's tools and the running jobs stack beside it */
    .bento > :global(.activity) {
      grid-row: span 2;
    }
    .bento > :global(.recent) {
      grid-column: span 12;
    }
    /* a card whose partner is hidden (or the checklist is finished) takes the whole row */
    .bento:not(:has(> :global(.budget))) > :global(.kpis),
    .bento:not(:has(> :global(.kpis))) > :global(.budget),
    .bento:not(:has(> :global(.checklist))) > :global(.attention),
    .bento:not(:has(> :global(.attention))) > :global(.checklist) {
      grid-column: span 12;
    }
    .bento:not(:has(> :global(:is(.tools, .jobs)))) > :global(.activity) {
      grid-column: span 12;
      grid-row: auto;
    }
    .bento:not(:has(> :global(.tools))) > :global(.activity),
    .bento:not(:has(> :global(.jobs))) > :global(.activity) {
      grid-row: auto;
    }
  }
  :global(.kpis) {
    container-type: inline-size;
  }
  /* the figures sit in the middle when the budget card beside them is taller */
  .bento > :global(.kpis) > :global(.inner) {
    display: grid;
    align-content: center;
  }
  /* four figures: one row when the card is wide, two by two when it is not */
  .kpi-row {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--s-6);
  }
  @container (max-width: 600px) {
    .kpi-row {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  .budget-body {
    display: flex;
    gap: var(--s-5);
    align-items: center;
    flex-wrap: wrap;
  }
  .budget-body > .stack {
    flex: 1;
    min-width: 160px;
  }
  .clear {
    display: flex;
    align-items: center;
    gap: var(--s-3);
  }
  .att {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
  }
  .att li {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
  }
  .att li.read {
    color: var(--text-muted);
  }
  .att-t {
    flex: 1 1 160px;
    min-width: 0;
    font-size: var(--text-sm);
    overflow-wrap: anywhere;
  }
  .att-dot {
    display: inline-block;
    width: 7px;
    height: 7px;
    margin-inline-end: 7px;
    border-radius: 999px;
    background: var(--accent);
  }
  .att-more {
    margin-top: var(--s-3);
    padding: 4px 0;
    border: 0;
    background: none;
    color: var(--accent-text);
    font-size: var(--text-sm);
    font-weight: 650;
    cursor: pointer;
  }
  .jobs-more {
    margin-top: var(--s-3);
    overflow-wrap: anywhere;
  }
  .cl {
    list-style: none;
    margin: var(--s-4) 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .cl li {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    min-height: 34px;
    font-size: var(--text-sm);
  }
  .tick {
    display: grid;
    place-items: center;
    width: 20px;
    height: 20px;
    border-radius: 999px;
    box-shadow: inset 0 0 0 1.5px var(--border-strong);
    color: #fff;
    transition: background-color var(--dur) var(--ease);
  }
  .done .tick {
    background: var(--ok);
    box-shadow: none;
    animation: tick 400ms var(--ease);
  }
  @keyframes tick {
    40% {
      scale: 1.25;
    }
  }
  .done .cl-t {
    color: var(--text-muted);
    text-decoration: line-through;
    text-decoration-color: var(--border-strong);
  }
  .cl-t {
    flex: 1;
  }
  .cl-go {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    min-height: 24px; /* a touch target of at least 24 px (WCAG 2.5.8) */
    font-weight: 600;
    text-decoration: none;
    white-space: nowrap;
  }
  .bars {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .bars li {
    display: grid;
    grid-template-columns: minmax(80px, 1fr) 2fr auto;
    align-items: center;
    gap: var(--s-3);
  }
  .bars a {
    padding-block: 4px; /* a touch target of at least 24 px (WCAG 2.5.8) */
    color: var(--text);
    text-decoration: none;
    font-size: var(--text-sm);
    font-weight: 550;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .bar {
    height: 8px;
    border-radius: 999px;
    background: var(--surface-2);
    overflow: hidden;
  }
  .bar span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--chart-1);
    transform-origin: left;
    animation: bar 700ms var(--ease) both;
    animation-delay: calc(var(--i) * 70ms);
  }
  @keyframes bar {
    from {
      transform: scaleX(0);
    }
  }
  .jl {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .jl li {
    display: flex;
    justify-content: space-between;
    gap: var(--s-2);
  }
  .turns {
    list-style: none;
    margin: 0;
    padding: 0 0 6px;
  }
  .turns a {
    display: grid;
    grid-template-columns: 90px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--s-3);
    padding: 10px var(--pad-card);
    color: var(--text);
    text-decoration: none;
    transition: background-color var(--dur-fast) var(--ease-out);
  }
  .turns a:hover {
    background: var(--surface-2);
  }
  .meta {
    display: flex;
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  @media (max-width: 640px) {
    .turns a {
      grid-template-columns: 1fr;
      gap: 4px;
    }
    .meta {
      justify-content: flex-start;
    }
  }
</style>
