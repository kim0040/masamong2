<script lang="ts">
  import { t, has, toolName } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig, patchConfig, type Op } from '../lib/config.svelte'
  import { overview, startOverview, refreshOverview } from '../lib/overview.svelte'
  import { usd, num } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import Ring from '../components/Ring.svelte'
  import Button from '../components/Button.svelte'
  import Confirm from '../components/Confirm.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import Progress from '../components/Progress.svelte'
  import SaveBar from '../components/SaveBar.svelte'
  import Status from '../components/Status.svelte'
  import Segmented from '../components/Segmented.svelte'
  import * as I from '../lib/icons'

  useConfig()
  startOverview()
  const tools = resource<any>('tools.list', () => ({}), { live: ['config', 'turn'] })
  const providers = resource<any>('providers.list', () => ({}), { live: ['config'] })
  const month = $derived(overview.data?.month)
  // the call columns and the spend line follow one window: today's live counters, or usage.period's
  // totals for yesterday and the last 7 or 30 days (the instance's days, as the daily limits)
  type Period = 'today' | 'yesterday' | '7d' | '30d'
  let period = $state<Period>('today')
  const pu = resource<any>('usage.period', () => ({ period }), { live: ['turn'] })
  const isToday = $derived(period === 'today')
  const toolCount = (tl: any): number => (isToday ? tl.used_today ?? 0 : pu.data?.period === period ? pu.data?.tools?.[tl.id] ?? 0 : 0)
  const provCount = (p: any): number => (isToday ? p.used_today ?? 0 : pu.data?.period === period ? pu.data?.providers?.[p.id] ?? 0 : 0)
  const periodSpend = $derived(pu.data?.period === period ? Object.entries<number>(pu.data?.spend_usd ?? {}).filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1]) : [])
  let resetOpen = $state(false)
  let resetBusy = $state(false)

  function resetStamp(at: string) {
    const d = new Date(at)
    if (Number.isNaN(d.getTime())) return { month: '–', day: '–', time: '–:–' }
    return { month: d.getMonth() + 1, day: d.getDate(), time: `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}` }
  }
  async function resetSpend(undo = false) {
    resetBusy = true
    try {
      const r = await call('usage.reset', undo ? { undo: true } : {})
      r.ok ? toast('ok', t(undo ? 'usage.reset_undone' : 'usage.reset_done')) : toast('error', t('common.failed'), r.error?.message)
      await refreshOverview()
    } finally {
      resetBusy = false
    }
  }

  // budget form
  let b = $state({ monthly: '', request: '', turn: '', index: '', alert: '' })
  let bBase = $state('')
  $effect(() => {
    if (!cfg.loaded) return
    const f = { monthly: cfg.doc.limits?.monthly_budget_usd ?? '', request: cfg.doc.limits?.per_request_budget_usd ?? '', turn: cfg.doc.memory?.turn_budget_usd ?? '', index: cfg.doc.memory?.index_budget_usd ?? '', alert: String(cfg.doc.limits?.budget_alert_percent || 80) }
    const j = JSON.stringify(f)
    if (j !== bBase) {
      b = f
      bBase = j
    }
  })
  // tool limit grid: tool → field → value
  type L = { per_day: number; per_user_per_day: number; per_channel_per_day: number; cooldown_sec: number }
  let lim = $state<Record<string, L>>({})
  let lBase = $state('')
  $effect(() => {
    if (!tools.data) return
    const m: Record<string, L> = {}
    for (const tl of tools.data.tools) if (tl.id !== 'help.version') m[tl.id] = { per_day: tl.limits?.per_day ?? 0, per_user_per_day: tl.limits?.per_user_per_day ?? 0, per_channel_per_day: tl.limits?.per_channel_per_day ?? 0, cooldown_sec: tl.limits?.cooldown_sec ?? 0 }
    const j = JSON.stringify(m)
    if (j !== lBase) {
      lim = m
      lBase = j
    }
  })
  const budgetDirty = $derived(bBase ? Object.keys(b).filter((k) => (JSON.parse(bBase) as any)[k] !== (b as any)[k]) : [])
  const limitDirty = $derived.by(() => {
    if (!lBase) return [] as [string, keyof L][]
    const old = JSON.parse(lBase) as Record<string, L>
    const out: [string, keyof L][] = []
    for (const [id, v] of Object.entries(lim)) for (const k of Object.keys(v) as (keyof L)[]) if (Number(old[id]?.[k] ?? 0) !== Number(v[k])) out.push([id, k])
    return out
  })
  // Presets fill the per-person, per-room and wait columns of every tool at once (the daily totals,
  // such as the image cap, stay as they are); nothing is saved until the save bar. Paid calls
  // (images) and replies get their own scale: a reply cap that is too low silences the bot.
  type Preset = 'relaxed' | 'normal' | 'strict'
  const presets: Record<Preset, { user: number; channel: number; wait: number }> = {
    relaxed: { user: 50, channel: 300, wait: 0 },
    normal: { user: 20, channel: 120, wait: 3 },
    strict: { user: 8, channel: 40, wait: 10 },
  }
  const scale: Record<string, { user: number; channel: number; wait: number }> = {
    'images.generate': { user: 0.2, channel: 0.17, wait: 6 },
    'chat.respond': { user: 4, channel: 3.4, wait: 0.2 },
  }
  let preset = $state<Preset | ''>('')
  function applyPreset(v: string) {
    preset = v as Preset
    const p = presets[preset]
    const next: Record<string, L> = {}
    for (const [id, cur] of Object.entries(lim)) {
      const f = scale[id] ?? { user: 1, channel: 1, wait: 1 }
      next[id] = { ...cur, per_user_per_day: Math.max(1, Math.round(p.user * f.user)), per_channel_per_day: Math.max(1, Math.round(p.channel * f.channel)), cooldown_sec: Math.round(p.wait * f.wait) }
    }
    lim = next
    showAll = true
  }
  // the table lists what matters first: tools used today, tools with a limit, and the paid ones
  let showAll = $state(false)
  const listed = $derived(((tools.data?.tools ?? []) as any[]).filter((x) => x.id !== 'help.version' && lim[x.id]))
  const shown = $derived(showAll ? listed : listed.filter((x) => toolCount(x) > 0 || x.used_today > 0 || x.id === 'chat.respond' || x.id === 'images.generate' || Object.values(lim[x.id]).some((v) => v > 0)))
  let saving = $state(false)
  let err = $state('')
  const money = /^\d+(\.\d{1,4})?$/
  // a wrong amount is marked at its field as soon as it is typed, not only when Save is pressed
  const badMoney = (k: string) => (b as any)[k] !== '' && !money.test(String((b as any)[k]))
  const badPct = $derived(!(Math.round(Number(b.alert)) >= 1 && Math.round(Number(b.alert)) <= 100))
  async function save() {
    err = ''
    for (const k of budgetDirty) if (k !== 'alert' && (b as any)[k] !== '' && !money.test((b as any)[k])) return (err = t('usage.err.money'))
    const ops: Op[] = []
    if (budgetDirty.includes('alert')) {
      const pct = Math.round(Number(b.alert))
      if (!(pct >= 1 && pct <= 100)) return (err = t('usage.err.percent'))
      ops.push({ path: ['limits', 'budget_alert_percent'], value: pct })
    }
    const map: Record<string, string[]> = { monthly: ['limits', 'monthly_budget_usd'], request: ['limits', 'per_request_budget_usd'], turn: ['memory', 'turn_budget_usd'], index: ['memory', 'index_budget_usd'] }
    for (const k of budgetDirty.filter((x) => x !== 'alert')) ops.push((b as any)[k] === '' && k !== 'monthly' && k !== 'request' ? { path: map[k], op: 'unset' } : { path: map[k], value: (b as any)[k] || '0' })
    for (const [id, k] of limitDirty) {
      const v = Math.max(0, Math.round(Number(lim[id][k]) || 0))
      ops.push(v > 0 ? { path: ['tools', id, 'limits', k], value: v } : { path: ['tools', id, 'limits', k], op: 'unset' })
    }
    saving = true
    const r = await patchConfig(ops, { success: t('usage.saved') })
    saving = false
    if (r.ok) {
      bBase = ''
      lBase = ''
      void tools.refresh()
    }
  }
  function reset() {
    b = JSON.parse(bBase)
    lim = JSON.parse(lBase)
    preset = ''
  }
</script>

<PageHeader title={t('usage.title')} description={t('usage.desc')} />

<div class="top">
  <Card title={t('usage.month')}>
    <div class="month">
      {#if month}
        <Ring warnAt={(cfg.doc?.limits?.budget_alert_percent || 80) / 100} value={month.spent_usd} max={month.budget_usd || 1} label={t('usage.month')} center={usd(month.spent_usd)} sub={month.budget_usd ? t('home.budget.of', { budget: usd(month.budget_usd, 0) }) : t('home.budget.no_limit')} size={150} />
        <div class="stack" style:gap="8px" style:flex="1">
          {#each Object.entries(month.by_kind ?? {}) as [k, v]}
            <div class="row small"><span class="muted">{t(`spend.${k}`)}</span><span class="spacer"></span><span class="num">{usd(v as number)}</span></div>
          {:else}<p class="small muted">{t('usage.no_spend')}</p>{/each}
          <p class="xs subtle">{t('usage.basis')}</p>
          {#each Object.entries(month.key_credit ?? {}) as [id, k]}
            {@const kc = k as any}
            {@const out = kc.remaining <= 0.5}
            {@const short = !out && month.budget_usd > 0 && kc.remaining < month.budget_usd - month.spent_usd}
            <div class="key-credit" class:short class:out>
              <span class="small">{t('usage.key_credit', { provider: has(`conn.provider.${id}`) ? t(`conn.provider.${id}`) : id, left: usd(kc.remaining), limit: usd(kc.limit) })}</span>
              <span class="xs subtle">{t(`usage.key_reset.${kc.reset || 'none'}`)}{#if out} · <span class="danger-text">{t('usage.key_credit_out')}</span>{:else if short} · <span class="warn-text">{t('usage.key_credit_short')}</span>{/if}</span>
            </div>
          {/each}
          {#if month.reset}
            <div class="reset-line">
              <p class="xs subtle">{t('usage.reset_active', { ...resetStamp(month.reset.at), before: usd(month.reset.before_usd) })}</p>
              <Button size="sm" variant="ghost" loading={resetBusy} disabled={!can('usage.reset')} onclick={() => void resetSpend(true)}>{t('usage.reset_undo')}</Button>
            </div>
          {:else}
            <Button size="sm" disabled={!can('usage.reset')} loading={resetBusy} onclick={() => (resetOpen = true)}>{t('usage.reset')}</Button>
          {/if}
        </div>
      {:else}<Skeleton lines={4} />
      {/if}
    </div>
  </Card>
  <Card title={t('usage.budgets')} description={t('usage.budgets_desc')}>
    <div class="form-grid">
      {#each ['monthly', 'request', 'turn', 'index'] as k}
        <Field id="b-{k}" label={t(`usage.b.${k}`)} hint={t(`usage.b.${k}_hint`)} error={badMoney(k) ? t('usage.err.money') : undefined}>
          {#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} inputmode="decimal" bind:value={(b as any)[k]} placeholder="0.00" />{/snippet}
        </Field>
      {/each}
      <Field id="b-alert" label={t('usage.b.alert')} hint={t('usage.b.alert_hint')} error={badPct ? t('usage.err.percent') : undefined}>
        {#snippet children({ id, describedby, invalid })}<div class="pct"><Input {id} aria-describedby={describedby} {invalid} type="number" min="1" max="100" inputmode="numeric" bind:value={b.alert} /><span class="muted">%</span></div>{/snippet}
      </Field>
    </div>
    {#if err}<p class="small err" role="alert">{err}</p>{/if}
  </Card>
</div>

<div class="period-bar">
  <span class="small muted">{t('usage.period_label')}</span>
  <Segmented label={t('usage.period')} bind:value={period} options={(['today', 'yesterday', '7d', '30d'] as const).map((v) => ({ value: v, label: t(`usage.period.${v}`) }))} />
  <div class="period-spend">
    {#if pu.data?.period === period}
      <span class="small"><span class="muted">{t('usage.period_spend')}</span> <strong class="num">{usd(pu.data.spent_usd ?? 0)}</strong></span>
      {#each periodSpend.slice(0, 4) as [k, v] (k)}<span class="xs muted chip">{t(`spend.${k}`)} <span class="num">{usd(v)}</span></span>{/each}
      <span class="xs subtle">{pu.data.from === pu.data.to ? pu.data.from : `${pu.data.from} – ${pu.data.to}`}</span>
    {:else}<span class="sk-line"><Skeleton lines={1} /></span>{/if}
  </div>
</div>

<Card padded={false} title={t('usage.tools')} description={t('usage.tools_desc')}>
  <div class="presets">
    <Segmented label={t('usage.preset')} value={preset} onchange={applyPreset} options={(['relaxed', 'normal', 'strict'] as const).map((v) => ({ value: v, label: t(`usage.preset.${v}`) }))} />
    <span class="xs muted">{preset ? t(`usage.preset.${preset}_hint`) : t('usage.preset_hint')}</span>
  </div>
  <div use:scrollable class="table-scroll">
    <table class="table">
      <thead><tr><th>{t('tools.col.tool')}</th><th>{isToday ? t('usage.today') : t(`usage.period.${period}`)}</th>{#each ['per_day', 'per_user_per_day', 'per_channel_per_day', 'cooldown_sec'] as k}<th class="r">{t(`limits.${k}`)}</th>{/each}</tr></thead>
      <tbody>
        {#each shown as tl (tl.id)}
          {#if lim[tl.id]}
            <tr class:hot={tl.id === 'chat.respond'}>
              <td><a class="row-link" href="/tools/{encodeURIComponent(tl.id)}">{toolName(tl.id)}</a>{#if tl.id === 'chat.respond'}<div class="xs muted">{t('usage.chat_note')}</div>{/if}</td>
              <td class="usage-cell">
                <span class="num small">{num(toolCount(tl))}{#if isToday && lim[tl.id].per_day}<span class="muted">&nbsp;/ {num(lim[tl.id].per_day)}</span>{/if}</span>
                {#if isToday && lim[tl.id].per_day}<Progress value={tl.used_today} max={lim[tl.id].per_day} label={t('usage.today')} tone={tl.used_today >= lim[tl.id].per_day ? 'danger' : 'accent'} />{/if}
              </td>
              {#each ['per_day', 'per_user_per_day', 'per_channel_per_day', 'cooldown_sec'] as k}
                <td class="r"><input class="cell num" type="number" min="0" inputmode="numeric" value={(lim[tl.id] as any)[k] || ''} placeholder={t('usage.no_limit')} oninput={(e) => ((lim[tl.id] as any)[k] = Math.max(0, Number(e.currentTarget.value) || 0))} aria-label="{toolName(tl.id)} {t(`limits.${k}`)}" /></td>
              {/each}
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </div>
  {#if shown.length < listed.length}<div class="more-row"><Button size="sm" variant="ghost" icon={I.ChevronDown} onclick={() => (showAll = true)}>{t('usage.show_all', { n: listed.length - shown.length })}</Button></div>{/if}
  <p class="xs muted foot">{t('tool.limits_zero')}</p>
</Card>

<Card padded={false} title={t('usage.providers')} description={t('usage.providers_desc')} class="prov">
  <div use:scrollable class="table-scroll">
    <table class="table">
      <thead><tr><th>{t('usage.provider')}</th><th>{t('usage.key')}</th><th>{isToday ? t('usage.today') : t(`usage.period.${period}`)}</th></tr></thead>
      <tbody>
        {#each providers.data?.providers ?? [] as p (p.id)}
          <tr>
            <td><strong>{has(`conn.provider.${p.id}`) ? t(`conn.provider.${p.id}`) : p.id}</strong> <span class="xs subtle mono">{p.id}</span></td>
            <td class="nowrap"><Status tone={p.configured ? 'ok' : 'warn'} label={p.configured ? t('tools.state.ready') : t('tools.state.no_key')} size="sm" /></td>
            <td class="usage-cell"><span class="num small">{num(provCount(p))}{#if isToday && p.daily_limit}<span class="muted">&nbsp;/ {num(p.daily_limit)}</span>{/if}</span>{#if isToday && p.daily_limit}<Progress value={p.used_today} max={p.daily_limit} label={has(`conn.provider.${p.id}`) ? t(`conn.provider.${p.id}`) : p.id} />{/if}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
</Card>

<SaveBar count={budgetDirty.length + limitDirty.length} {saving} onsave={save} onreset={reset} />
<Confirm bind:open={resetOpen} title={t('usage.reset_title')} body={t('usage.reset_body')} confirmLabel={t('usage.reset_confirm')} onconfirm={() => resetSpend()} />

<style>
  .period-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-4);
    margin-bottom: var(--s-3);
  }
  .period-spend {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 4px var(--s-3);
    min-width: 0;
  }
  .sk-line {
    width: 200px;
  }
  .presets {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
    padding: 0 var(--pad-card) var(--s-3);
  }
  .more-row {
    padding: var(--s-2) var(--pad-card) 0;
  }
  .pct {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .top {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 440px), 1fr));
    margin-bottom: var(--s-4);
  }
  /* the spend card matches the budget form's height; its figures sit in the middle */
  .top > :global(.card) > :global(.inner) {
    display: grid;
    align-content: center;
  }
  .month {
    display: flex;
    gap: var(--s-6);
    align-items: center;
    flex-wrap: wrap;
  }
  .usage-cell {
    min-width: 150px;
  }
  .usage-cell :global(.pg) {
    margin-top: 4px;
  }
  .reset-line {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--s-2);
  }
  .reset-line p {
    margin: 0;
  }
  .key-credit {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--s-2) 0 0;
    border-top: 1px solid var(--border);
  }
  .key-credit.short .small {
    color: var(--warn);
    font-weight: 600;
  }
  .key-credit.out .small,
  .danger-text {
    color: var(--danger);
    font-weight: 600;
  }
  .warn-text {
    color: var(--warn);
  }
  .cell {
    width: 96px;
    height: 32px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: var(--surface-2);
    text-align: end;
  }
  @media (max-width: 640px) {
    .cell {
      font-size: 16px;
    }
  }
  .cell:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
    background: var(--surface);
  }
  tr.hot {
    background: color-mix(in oklab, var(--accent-soft) 45%, transparent);
  }
  .foot {
    padding: var(--s-3) var(--pad-card);
  }
  :global(.prov) {
    margin-top: var(--s-4);
  }
  .err {
    color: var(--danger);
    margin-top: 8px;
  }
</style>
