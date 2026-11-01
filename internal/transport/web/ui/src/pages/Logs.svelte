<script lang="ts">
  import { tick } from 'svelte'
  import { scrollable } from '../lib/scrollable'
  import { t } from '../lib/i18n.svelte'
  import { call, practice } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { datetime, relative, duration } from '../lib/format'
  import { route } from '../lib/router.svelte'
  import Stat from '../components/Stat.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Status from '../components/Status.svelte'
  import Switch from '../components/Switch.svelte'
  import JsonView from '../components/JsonView.svelte'
  import * as I from '../lib/icons'

  let tab = $state(route.query.get('tab') ?? 'live')
  let level = $state<'info' | 'warn' | 'error'>('info')
  let q = $state('')
  let follow = $state(true)
  let entries = $state<any[]>([])
  let lastId = 0
  let box: HTMLDivElement | undefined = $state()
  let grouped = $state(false)
  let logError = $state('')
  let loadingLogs = $state(true)
  let unseen = $state(0)
  let bufferSize = $state(3000)
  let requestSeq = 0

  async function poll(reset = false) {
    const seq = ++requestSeq
    if (reset) {
      entries = []
      lastId = 0
      unseen = 0
      loadingLogs = true
    }
    const requestedAfter = lastId
    const r = await call('runtime.logs', { after_id: requestedAfter, level, q, limit: 400 })
    if (seq !== requestSeq) return
    loadingLogs = false
    if (!r.ok) {
      logError = r.error?.message ?? t('logs.load_failed')
      return
    }
    logError = ''
    const serverLast = Number(r.output.last_id ?? 0)
    bufferSize = Number(r.output.buffer ?? 3000)
    // A daemon restart creates a fresh in-memory ring whose IDs begin at one again.
    if (!reset && serverLast < requestedAfter) {
      entries = []
      lastId = 0
      unseen = 0
      void poll(true)
      return
    }
    const got = r.output.entries as any[]
    lastId = serverLast
    if (got.length) {
      entries = [...entries, ...got].slice(-bufferSize)
      if (follow) {
        await tick()
        box?.scrollTo({ top: box.scrollHeight })
      } else if (!reset) unseen += got.length
    }
  }
  $effect(() => {
    void level, q
    void poll(true)
  })
  $effect(() => {
    if (tab !== 'live') return
    const id = setInterval(() => document.visibilityState === 'visible' && poll(), 2000)
    return () => clearInterval(id)
  })
  async function jumpLatest() {
    follow = true
    unseen = 0
    await tick()
    box?.scrollTo({ top: box.scrollHeight })
  }
  function setFollowing(on: boolean) {
    follow = on
    if (on) void jumpLatest()
  }
  function handleScroll() {
    if (follow && box && box.scrollHeight - box.scrollTop - box.clientHeight > 48) follow = false
  }
  const groups = $derived.by(() => {
    const m = new Map<string, { msg: string; level: string; n: number; first: string; last: string }>()
    for (const e of entries) {
      if (e.level === 'info') continue
      const k = e.level + e.msg
      const g = m.get(k)
      if (g) {
        g.n++
        g.last = e.at
      } else m.set(k, { msg: e.msg, level: e.level, n: 1, first: e.at, last: e.at })
    }
    return [...m.values()].sort((a, b) => b.n - a.n)
  })
  const tone = (l: string) => (l === 'error' ? 'danger' : l === 'warn' ? 'warn' : 'info')
  function showGroup(g: { msg: string; level: string }) {
    level = g.level as typeof level
    q = g.msg
    grouped = false
  }

  const doctor = resource<any>('runtime.doctor', () => ({}), { live: ['config', 'provider'] })
  let histDays = $state<'7' | '30' | '90'>('30')
  const hist = resource<any>('runtime.history', () => ({ days: Number(histDays) }), { live: ['runtime'] })
  // spans as fractions of the window for the timeline bar
  const bar = $derived.by(() => {
    const h = hist.data
    if (!h) return { spans: [], marks: [] as { x: number; kind: string; at: string }[] }
    const from = Date.parse(h.window_from ?? h.since)
    const to = Date.now()
    const w = Math.max(1, to - from)
    const spans = (h.spans ?? []).map((sp: any) => ({ x: (Date.parse(sp.from) - from) / w, w: (Date.parse(sp.to) - Date.parse(sp.from)) / w }))
    const marks = (h.events ?? []).filter((e: any) => e.kind !== 'start').map((e: any) => ({ x: (Date.parse(e.at) - from) / w, kind: e.kind, at: e.at }))
    return { spans, marks }
  })
  const evTone = (k: string) => (k === 'crash' ? 'danger' : k === 'start' ? 'ok' : k === 'restart' ? 'info' : 'neutral') as 'danger' | 'ok' | 'info' | 'neutral'
  let traceId = $state('')
  let trace = $state<any>(null)
  async function lookup() {
    const r = await call('trace.show', { id: traceId.trim() })
    trace = r.ok ? r.output : { error: r.error?.message }
  }
  function download() {
    const lines = entries.map((e) => `${e.at} ${e.level.toUpperCase()} ${e.msg} ${Object.entries(e.attrs ?? {}).map(([k, v]) => `${k}=${v}`).join(' ')}`)
    const blob = new Blob([lines.join('\n')], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = 'masamong-logs.txt'
    a.click()
    URL.revokeObjectURL(a.href)
  }
</script>

<PageHeader title={t('logs.title')} description={t('logs.desc')} />

<Tabs id="logs" bind:active={tab} tabs={[{ id: 'live', label: t('logs.tab.live'), icon: I.ScrollText }, { id: 'doctor', label: t('logs.tab.doctor'), icon: I.ShieldCheck }, { id: 'uptime', label: t('logs.tab.uptime'), icon: I.Activity }, { id: 'trace', label: t('logs.tab.trace'), icon: I.Search }]} />

<div class="panel" role="tabpanel" id="logs-panel-{tab}" aria-labelledby="logs-tab-{tab}">
  {#if tab === 'live'}
    <div class="bar">
      <Segmented label={t('logs.level')} bind:value={level} options={[{ value: 'info', label: t('logs.l.info') }, { value: 'warn', label: t('logs.l.warn') }, { value: 'error', label: t('logs.l.error') }]} />
      <div class="search"><Input type="search" bind:value={q} placeholder={t('logs.search')} aria-label={t('logs.search')} /></div>
      <Switch checked={follow} onchange={setFollowing} label={t('logs.follow')} hint={t('logs.follow_hint')} />
      <Switch bind:checked={grouped} label={t('logs.grouped')} />
      <span class="spacer"></span>
      <Button size="sm" icon={I.Download} onclick={download} disabled={!entries.length}>{t('logs.download')}</Button>
    </div>
    {#if practice.on}<p class="callout info sample-note small">{t('logs.practice_note')}</p>{/if}
    {#if logError}<div class="callout danger log-error" role="alert"><span>{logError}</span><Button size="sm" variant="secondary" icon={I.RefreshCw} onclick={() => poll(true)}>{t('common.retry')}</Button></div>{/if}
    {#if grouped}
      <Card padded={false}>
        {#if groups.length === 0}<p class="small muted" style:padding="20px">{t('logs.no_problems')}</p>{:else}
          <div use:scrollable class="table-scroll"><table class="table"><thead><tr><th>{t('logs.col.level')}</th><th>{t('logs.col.message')}</th><th class="r">{t('logs.col.count')}</th><th>{t('logs.col.first')}</th><th>{t('logs.col.last')}</th></tr></thead>
            <tbody>{#each groups as g}<tr><td><Status tone={tone(g.level) as any} label={t(`logs.l.${g.level}`)} size="sm" /></td><td class="break"><button type="button" class="group-msg" onclick={() => showGroup(g)} title={t('logs.filter_problem')}>{g.msg}</button></td><td class="r num">{g.n}</td><td class="muted nowrap">{datetime(g.first, true)}</td><td class="muted nowrap">{datetime(g.last, true)}</td></tr>{/each}</tbody>
          </table></div>
        {/if}
      </Card>
    {:else}
      <div use:scrollable class="console" bind:this={box} role="log" aria-live="off" aria-label={t('logs.tab.live')} aria-busy={loadingLogs} onscroll={handleScroll}>
        {#each entries as e (e.id)}
          <div class="ln {e.level}"><span class="tm num">{datetime(e.at, true)}</span><span class="lv">{e.level}</span><span class="msg">{e.msg}</span>{#each Object.entries(e.attrs ?? {}) as [k, v]}<span class="kv"><span class="k">{k}</span>={v}</span>{/each}</div>
        {:else}
          <p class="empty">{t('logs.empty')}</p>
        {/each}
      </div>
      <div class="log-foot"><p class="xs muted">{t('logs.buffer_note', { n: bufferSize })}</p>{#if unseen}<Button size="sm" variant="primary" icon={I.ArrowDown} onclick={jumpLatest}>{t('logs.new_lines', { n: unseen })}</Button>{/if}</div>
    {/if}
  {:else if tab === 'doctor'}
    <Card padded={false} title={t('logs.doctor_title')} description={t('logs.doctor_desc')}>
      {#snippet actions()}<Button size="sm" icon={I.RefreshCw} onclick={() => doctor.refresh()}>{t('logs.recheck')}</Button>{/snippet}
      {#if doctor.error}<div class="callout danger resource-error" role="alert"><span>{doctor.error.message}</span><Button size="sm" variant="secondary" onclick={() => doctor.refresh()}>{t('common.retry')}</Button></div>
      {:else if doctor.data}
        <div use:scrollable class="table-scroll"><table class="table">
          <thead><tr><th>{t('logs.col.feature')}</th><th>{t('logs.col.state')}</th><th>{t('logs.col.reason')}</th></tr></thead>
          <tbody>
            {#each Object.entries(doctor.data.features ?? {}) as [k, v]}
              {@const f = v as any}
              <tr><td><strong>{t(`feature.${k}`)}</strong> <span class="xs subtle mono">{k}</span></td><td><Status tone={f.status === 'ready' ? 'ok' : f.status === 'disabled' || f.status === 'off' ? 'neutral' : f.status === 'error' ? 'danger' : 'warn'} label={t(`state.${f.status}`)} size="sm" /></td><td class="small muted break">{f.reason_text ?? f.reason ?? ''}</td></tr>
            {/each}
          </tbody>
        </table></div>
      {:else}<p class="small muted resource-loading">{t('common.loading')}</p>{/if}
    </Card>
  {:else if tab === 'uptime'}
    <div class="bar"><Segmented label={t('stats.range')} bind:value={histDays} options={[{ value: '7', label: t('stats.days', { n: 7 }) }, { value: '30', label: t('stats.days', { n: 30 }) }, { value: '90', label: t('stats.days', { n: 90 }) }]} /></div>
    {#if hist.error}<div class="callout danger log-error" role="alert"><span>{hist.error.message}</span><Button size="sm" variant="secondary" onclick={() => hist.refresh()}>{t('common.retry')}</Button></div>
    {:else if hist.data}
      {#if hist.data.sample}<p class="callout info sample-note small">{t('logs.practice_uptime_note')}</p>{/if}
      <div class="kpis">
        <Stat label={t('logs.uptime_pct')} value={hist.data.uptime_pct} format={(n) => `${n.toFixed(n >= 99.95 ? 0 : 1)}%`} />
        <Stat label={t('logs.crashes')} value={hist.data.crashes} />
        <Stat label={t('logs.running_since')} value={0} format={() => duration(Math.round((Date.now() - Date.parse(hist.data.started_at)) / 1000))} />
      </div>
      <Card title={t('logs.timeline')} description={t('logs.timeline_desc')}>
        <div class="tl" role="img" aria-label={t('logs.timeline_aria', { pct: hist.data.uptime_pct.toFixed(1), n: hist.data.crashes })}>
          {#each bar.spans as sp}<span class="up" style:inset-inline-start="{sp.x * 100}%" style:width="{Math.max(0.2, sp.w * 100)}%"></span>{/each}
          {#each bar.marks as m}<span class="mk {m.kind}" style:inset-inline-start="{m.x * 100}%" title="{t(`logs.ev.${m.kind}`)} · {datetime(m.at, true)}"></span>{/each}
        </div>
        <div class="tl-axis xs muted"><span>{datetime(hist.data.window_from ?? hist.data.since)}</span><span>{t('logs.now')}</span></div>
        <div class="legend xs muted"><span><i class="lg up"></i>{t('logs.ev.up')}</span><span><i class="lg crash"></i>{t('logs.ev.crash')}</span><span><i class="lg restart"></i>{t('logs.ev.restart')}</span><span><i class="lg stop"></i>{t('logs.ev.stop')}</span></div>
      </Card>
      <Card padded={false} title={t('logs.events')} class="mt">
        <div use:scrollable class="table-scroll"><table class="table">
          <thead><tr><th>{t('logs.col.when')}</th><th>{t('logs.col.event')}</th><th>{t('maint.version')}</th></tr></thead>
          <tbody>{#each hist.data.events as e}<tr><td class="nowrap" title={datetime(e.at, true)}>{relative(e.at)}</td><td><Status tone={evTone(e.kind)} label={t(`logs.ev.${e.kind}`)} size="sm" /></td><td class="muted mono small">{e.version ?? ''}</td></tr>{:else}<tr><td colspan="3" class="muted">{t('logs.no_events')}</td></tr>{/each}</tbody>
        </table></div>
      </Card>
    {:else}<p class="small muted">{t('common.loading')}</p>{/if}
  {:else}
    <Card title={t('logs.trace_title')} description={t('logs.trace_desc')}>
      <div class="row"><div style:flex="1"><Input bind:value={traceId} mono placeholder="tr_…" aria-label={t('logs.trace_id')} /></div><Button onclick={lookup} icon={I.Search} disabled={!traceId.trim()}>{t('logs.lookup')}</Button></div>
      {#if trace}{#if trace.error}<p class="small err">{trace.error}</p>{:else}<JsonView value={trace} label={t('logs.trace_view')} />{/if}{/if}
    </Card>
  {/if}
</div>

<style>
  .panel {
    margin-top: var(--s-4);
  }
  .bar {
    display: flex;
    gap: var(--s-3);
    align-items: center;
    flex-wrap: wrap;
    margin-bottom: var(--s-3);
  }
  .search {
    flex: 0 1 280px;
  }
  .console {
    height: min(68dvh, 760px);
    overflow: auto;
    padding: var(--s-3);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text-muted);
    font-family: var(--mono);
    font-size: 13px;
    line-height: 1.6;
  }
  .ln {
    display: flex;
    min-width: 0;
    flex-wrap: wrap;
    gap: 0 10px;
    padding: 1px 6px;
    border-radius: 4px;
    animation: ln-in 240ms var(--ease) both;
  }
  @keyframes ln-in {
    from {
      opacity: 0;
      translate: 0 4px;
    }
  }
  .ln:hover {
    background: var(--surface-2);
  }
  .ln.error {
    background: var(--danger-soft);
  }
  .tm {
    color: var(--text-subtle);
  }
  .lv {
    width: 44px;
    text-transform: uppercase;
    font-weight: 700;
    color: var(--info);
  }
  .warn .lv {
    color: var(--warn);
  }
  .error .lv {
    color: var(--danger);
  }
  .msg {
    color: var(--text);
    min-width: 0;
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  .kv {
    color: var(--text-muted);
    min-width: 0;
    overflow-wrap: anywhere;
    word-break: break-word;
  }
  .k {
    color: var(--accent-text);
  }
  .empty {
    color: var(--text-subtle);
    padding: 20px;
    text-align: center;
  }
  .err {
    color: var(--danger);
    margin-top: 8px;
  }
  .sample-note {
    margin-bottom: var(--s-3);
  }
  .log-error {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    justify-content: space-between;
    margin-bottom: var(--s-3);
  }
  .resource-error,
  .resource-loading {
    margin: var(--pad-card);
  }
  .resource-error {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    justify-content: space-between;
  }
  .log-foot {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    justify-content: space-between;
    min-height: 34px;
    margin-top: 6px;
  }
  .group-msg {
    width: 100%;
    padding: 6px 0;
    border: 0;
    background: none;
    color: inherit;
    text-align: start;
    cursor: pointer;
    overflow-wrap: anywhere;
  }
  .group-msg:hover {
    color: var(--accent-text);
    text-decoration: underline;
    text-underline-offset: 2px;
  }
  .kpis {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 200px), 1fr));
    margin-bottom: var(--s-4);
  }
  .tl {
    position: relative;
    height: 28px;
    border-radius: var(--r);
    background: var(--surface-2);
    overflow: hidden;
  }
  .up {
    position: absolute;
    top: 0;
    bottom: 0;
    background: color-mix(in oklab, var(--ok) 70%, transparent);
    animation: grow 600ms var(--ease) both;
    transform-origin: left;
  }
  @keyframes grow {
    from {
      scale: 0 1;
    }
  }
  .mk {
    position: absolute;
    top: 0;
    bottom: 0;
    width: 3px;
    margin-inline-start: -1px;
    background: var(--text-subtle);
  }
  .mk.crash {
    background: var(--danger);
  }
  .mk.restart {
    background: var(--info);
  }
  .tl-axis,
  .legend {
    display: flex;
    justify-content: space-between;
    margin-top: 6px;
  }
  .legend {
    justify-content: flex-start;
    gap: var(--s-4);
    flex-wrap: wrap;
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .lg {
    width: 10px;
    height: 10px;
    border-radius: 3px;
    background: var(--text-subtle);
  }
  .lg.up {
    background: color-mix(in oklab, var(--ok) 70%, transparent);
  }
  .lg.crash {
    background: var(--danger);
  }
  .lg.restart {
    background: var(--info);
  }
  :global(.mt) {
    margin-top: var(--s-4);
  }
</style>
