<script lang="ts">
  import { t, toolName } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { usd, num, ms, shortDate } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Chart from '../components/Chart.svelte'
  import Heatmap from '../components/Heatmap.svelte'
  import Stat from '../components/Stat.svelte'
  import Button from '../components/Button.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Dialog from '../components/Dialog.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import StoredTotals from '../components/StoredTotals.svelte'
  import { call } from '../lib/api.svelte'
  import { toast } from '../lib/toast.svelte'
  import * as I from '../lib/icons'
  import { outcomeColor } from '../lib/outcomes'

  let days = $state<'7' | '14' | '30' | '90'>('14')
  const s = resource<any>('stats.series', () => ({ days: Number(days) }), { live: ['turn'] })
  let tab = $state('activity')
  const labels = $derived((s.data?.days ?? []).map(shortDate))
  const palette = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)', 'var(--chart-6)']
  const turnSeries = $derived(Object.entries<number[]>(s.data?.turns ?? {}).map(([k, data], i) => ({ key: k, label: t(`outcome.${k}`), data, color: outcomeColor(k, i) })))
  const costSeries = $derived(Object.entries<number[]>(s.data?.cost_usd ?? {}).map(([k, data], i) => ({ key: k, label: t(`spend.${k}`), data, color: palette[i % palette.length] })))
  const toolSeries = $derived(Object.entries<number[]>(s.data?.tools ?? {}).filter(([k]) => k !== 'help.version').sort((a, b) => b[1].reduce((x, y) => x + y, 0) - a[1].reduce((x, y) => x + y, 0)).slice(0, 6).map(([k, data], i) => ({ key: k, label: toolName(k), data, color: palette[i % palette.length] })))
  const sum = (arr: number[] | undefined) => (arr ?? []).reduce((a, b) => a + b, 0)
  const totalTurns = $derived(turnSeries.reduce((a, x) => a + sum(x.data), 0))
  const answered = $derived(sum(s.data?.turns?.answered))
  const totalCost = $derived(costSeries.reduce((a, x) => a + sum(x.data), 0))
  const avgLatency = $derived.by(() => {
    const l = (s.data?.latency_ms ?? []) as number[]
    const vals = l.filter((x) => x > 0)
    return vals.length ? vals.reduce((a, b) => a + b, 0) / vals.length : 0
  })

  // period report (reports.create): a summary to read, download as Markdown or keep as JSON
  let repOpen = $state(false)
  let repDays = $state<'7' | '30' | '90'>('30')
  let rep = $state<any>(null)
  let repBusy = $state(false)
  async function makeReport() {
    repBusy = true
    const r = await call('reports.create', { days: Number(repDays) })
    repBusy = false
    if (r.ok) rep = r.output
    else toast('error', t('common.failed'), r.error?.message)
  }
  function reportMarkdown(r: any): string {
    const lines = [`# ${t('stats.report.title', { instance: r.instance })}`, '', `${r.period.from} → ${r.period.to} (${r.period.timezone})`, '',
      `## ${t('stats.report.turns')}`, `- ${t('stats.k.turns')}: ${num(r.turns.total)}`, `- ${t('stats.k.latency')}: ${ms(r.turns.avg_latency_ms)}`,
      ...Object.entries<number>(r.turns.by_outcome ?? {}).map(([k, v]) => `- ${t(`outcome.${k}`)}: ${num(v)}`), '',
      `## ${t('stats.report.cost')}`, `- ${t('stats.k.cost')}: ${usd(r.spend.total_usd)}${r.spend.budget_usd ? ` / ${usd(r.spend.budget_usd)}` : ''}`,
      ...(r.spend.by_kind ?? []).map((x: any) => `- ${t(`spend.${x.key}`)}: ${usd(x.value)}`), '',
      `## ${t('stats.report.tools')}`, ...(r.tools ?? []).map((x: any) => `- ${toolName(x.key)}: ${num(x.value)}`), '',
      `## ${t('stats.report.rooms')}`, ...(r.channels ?? []).map((x: any) => `- ${x.key}: ${num(x.value)}`), '',
      `## ${t('stats.report.ops')}`, `- ${t('stats.report.changes')}: ${num(r.changes.total)} (${t('stats.report.failed', { n: r.changes.failed })})`,
      ...(r.uptime ? [`- ${t('logs.uptime_pct')}: ${Number(r.uptime.uptime_pct).toFixed(1)}%`, `- ${t('logs.crashes')}: ${num(r.uptime.crashes)}`] : []),
      ...Object.entries<number>(r.reviews ?? {}).map(([k, v]) => `- ${t(`lab.verdict.${k}`)}: ${num(v)}`), '', `_${t('stats.report.generated', { at: r.generated_at, version: r.version })}_`, '']
    return lines.join('\n')
  }
  function download(kind: 'md' | 'json') {
    const body = kind === 'md' ? reportMarkdown(rep) : JSON.stringify(rep, null, 2)
    const blob = new Blob([body], { type: kind === 'md' ? 'text/markdown' : 'application/json' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `masamong-report-${rep.period.from}-${rep.period.to}.${kind}`
    a.click()
    URL.revokeObjectURL(a.href)
  }

  function csv() {
    const rows = [['day', ...turnSeries.map((x) => 'turns_' + x.key), ...costSeries.map((x) => 'usd_' + x.key), ...toolSeries.map((x) => 'tool_' + x.key)]]
    ;(s.data?.days ?? []).forEach((day: string, i: number) => rows.push([day, ...turnSeries.map((x) => String(x.data[i] ?? 0)), ...costSeries.map((x) => String(x.data[i] ?? 0)), ...toolSeries.map((x) => String(x.data[i] ?? 0))]))
    const blob = new Blob([rows.map((r) => r.join(',')).join('\n')], { type: 'text/csv' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `masamong-stats-${days}d.csv`
    a.click()
    URL.revokeObjectURL(a.href)
  }
</script>

<PageHeader title={t('stats.title')} description={t('stats.desc')}>
  {#snippet actions()}
    <Segmented label={t('stats.range')} bind:value={days} options={['7', '14', '30', '90'].map((v) => ({ value: v as any, label: t('stats.days', { n: v }) }))} />
    <Button icon={I.Download} onclick={csv} disabled={!s.data}>{t('stats.csv')}</Button>
    <Button variant="primary" icon={I.FileBarChart} onclick={() => { rep = null; repOpen = true }}>{t('stats.report.make')}</Button>
  {/snippet}
</PageHeader>

<div class="kpis">
  <Card><Stat label={t('stats.k.turns')} value={totalTurns} format={(n) => num(n)} /></Card>
  <Card><Stat label={t('stats.k.answer_rate')} value={totalTurns ? (100 * answered) / totalTurns : undefined} format={(n) => `${Math.round(n)}%`} hint={t('stats.k.answer_rate_hint')} /></Card>
  <Card><Stat label={t('stats.k.latency')} value={avgLatency || undefined} format={(n) => ms(n)} /></Card>
  <Card><Stat label={t('stats.k.cost')} value={totalCost} format={(n) => usd(n)} /></Card>
</div>

<StoredTotals />

<Tabs id="stats" bind:active={tab} tabs={[{ id: 'activity', label: t('stats.tab.activity') }, { id: 'tools', label: t('stats.tab.tools') }, { id: 'cost', label: t('stats.tab.cost') }, { id: 'quality', label: t('stats.tab.quality') }]} />
<div class="panel" role="tabpanel" id="stats-panel-{tab}" aria-labelledby="stats-tab-{tab}">
  {#if !s.data}
    <LoadState res={s}><Card><Skeleton block height={260} /></Card></LoadState>
  {:else if tab === 'activity'}
    <div class="cols">
      <Card title={t('stats.turns_title')} description={t('stats.turns_desc')}><Chart {labels} series={turnSeries} label={t('stats.turns_title')} height={260} /></Card>
      <Card title={t('stats.latency_title')} description={t('stats.latency_desc')}><Chart {labels} kind="lines" series={[{ key: 'lat', label: t('stats.k.latency'), data: s.data.latency_ms ?? [], color: 'var(--chart-2)' }]} label={t('stats.latency_title')} format={(n) => ms(n)} height={260} gaps /></Card>
      <Card title={t('stats.heat_title')} description={t('stats.heat_desc', { tz: s.data.timezone })} class="wide"><Heatmap data={s.data.heatmap ?? []} label={t('stats.heat_title')} /></Card>
    </div>
  {:else if tab === 'tools'}
    <Card title={t('stats.tools_title')} description={t('stats.tools_desc')}><Chart {labels} series={toolSeries} stacked={false} label={t('stats.tools_title')} height={280} /></Card>
  {:else if tab === 'cost'}
    <Card title={t('stats.cost_title')} description={t('stats.cost_desc')}><Chart {labels} series={costSeries} label={t('stats.cost_title')} format={(n) => usd(n)} height={280} /></Card>
  {:else}
    <Card title={t('stats.quality_title')} description={t('stats.quality_desc')}>
      <div class="row"><Button href="/lab" icon={I.FlaskConical}>{t('stats.to_lab')}</Button></div>
    </Card>
  {/if}
</div>

<Dialog bind:open={repOpen} title={t('stats.report.make')} description={t('stats.report.desc')} size="lg">
  <div class="stack">
    <div class="row"><Segmented label={t('stats.range')} bind:value={repDays} options={['7', '30', '90'].map((v) => ({ value: v as any, label: t('stats.days', { n: v }) }))} onchange={() => (rep = null)} />
      <Button icon={I.FileBarChart} loading={repBusy} onclick={makeReport}>{t('stats.report.build')}</Button></div>
    {#if rep}
      <div class="report enter">
        <h3>{t('stats.report.title', { instance: rep.instance })}</h3>
        <p class="xs muted">{rep.period.from} → {rep.period.to} · {rep.period.timezone}</p>
        <div class="rkpis">
          <div><span class="xs muted">{t('stats.k.turns')}</span><strong class="num">{num(rep.turns.total)}</strong></div>
          <div><span class="xs muted">{t('stats.k.latency')}</span><strong class="num">{ms(rep.turns.avg_latency_ms)}</strong></div>
          <div><span class="xs muted">{t('stats.k.cost')}</span><strong class="num">{usd(rep.spend.total_usd)}</strong></div>
          {#if rep.uptime}<div><span class="xs muted">{t('logs.uptime_pct')}</span><strong class="num">{Number(rep.uptime.uptime_pct).toFixed(1)}%</strong></div>{/if}
        </div>
        <div class="rcols">
          <div><div class="section-title">{t('stats.report.turns')}</div><KeyValue items={Object.entries(rep.turns.by_outcome ?? {}).map(([k, v]) => ({ k: t(`outcome.${k}`), v: num(v as number) }))} /></div>
          <div><div class="section-title">{t('stats.report.tools')}</div><KeyValue items={(rep.tools ?? []).map((x: any) => ({ k: toolName(x.key), v: num(x.value) }))} /></div>
          <div><div class="section-title">{t('stats.report.cost')}</div><KeyValue items={(rep.spend.by_kind ?? []).map((x: any) => ({ k: t(`spend.${x.key}`), v: usd(x.value) }))} /></div>
          <div><div class="section-title">{t('stats.report.ops')}</div><KeyValue items={[{ k: t('stats.report.changes'), v: `${num(rep.changes.total)} (${t('stats.report.failed', { n: rep.changes.failed })})` }, ...(rep.uptime ? [{ k: t('logs.crashes'), v: num(rep.uptime.crashes) }] : [])]} /></div>
        </div>
      </div>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (repOpen = false)}>{t('common.close')}</Button>
    <Button icon={I.Download} disabled={!rep} onclick={() => download('json')}>JSON</Button>
    <Button variant="primary" icon={I.Download} disabled={!rep} onclick={() => download('md')}>{t('stats.report.markdown')}</Button>
  {/snippet}
</Dialog>

<style>
  .report {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding: var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .rkpis {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  }
  .rkpis div {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .rkpis strong {
    font-size: var(--text-xl);
  }
  .rcols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 220px), 1fr));
  }
  .kpis {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    margin-bottom: var(--s-4);
  }
  .panel {
    margin-top: var(--s-4);
  }
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 520px), 1fr));
  }
  .cols > :global(.wide) {
    grid-column: 1 / -1;
  }
</style>
