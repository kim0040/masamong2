<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { latest } from '../lib/seqload'
  import { resource } from '../lib/resource.svelte'
  import { datetime, num, usd, pct } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { parseBudget, rangeParts, modelKey, modelName, phase, isBusy, progress, nextSteps, serverSpend, planSpend, pendingWarning, pendingClaims, MAX_BUDGET_USD, type DryRun, type Run, type ModelKey, type Range } from '../lib/peoplebackfill'
  import Card from './Card.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Select from './Select.svelte'
  import Segmented from './Segmented.svelte'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Progress from './Progress.svelte'
  import Confirm from './Confirm.svelte'
  import KeyValue from './KeyValue.svelte'
  import PersonBackfillReview from './PersonBackfillReview.svelte'
  import Skeleton from './Skeleton.svelte'
  import * as I from '../lib/icons'

  // The paid fill of person memory from past conversations (memory.people.backfill): a free estimate, a
  // 60-window pilot on both models, a look at the pilot's results, then the real run on one model. Only an
  // operator starts it and nothing starts it again by itself. It uses the Anthropic key set up for the bot.
  const channels = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const servers = $derived(((channels.data?.guilds ?? []) as any[]).map((g) => ({ value: g.id as string, label: (g.name || g.id) as string })))
  let guild = $state('')
  $effect(() => {
    if (!servers.some((s) => s.value === guild)) guild = servers[0]?.value ?? ''
  })

  let budget = $state('3')
  const cap = $derived(parseBudget(budget))

  let plan = $state<DryRun | null>(null)
  let planBusy = $state(false)
  let planError = $state('')
  // an estimate belongs to one server and one cap: changing either leaves it behind
  $effect(() => {
    void guild
    void budget
    untrack(() => {
      plan = null
      planError = ''
    })
  })
  async function estimate() {
    if (!guild || cap === null) return
    planBusy = true
    planError = ''
    const r = await call<DryRun>('memory.people.backfill', { mode: 'dry_run', guild_id: guild, budget_usd: String(cap) })
    planBusy = false
    if (r.ok) plan = r.output ?? null
    else planError = r.error?.message ?? t('common.failed')
  }

  // the newest run of this server; it moves on its own, so it is asked again while it works
  let run = $state<Run | null>(null)
  let running = $state(false)
  let statusReady = $state(false)
  const loads = latest()
  async function loadStatus(poll = false) {
    if (!guild) return
    const live = loads.start()
    const r = await call('memory.people.backfill', { mode: 'status', guild_id: guild }, { poll })
    if (!live()) return
    statusReady = true
    if (r.ok) {
      run = (r.output?.run ?? null) as Run | null
      running = !!r.output?.running
    }
  }
  $effect(() => {
    void guild
    untrack(() => {
      run = null
      running = false
      statusReady = false
      void loadStatus()
    })
  })
  const busy = $derived(isBusy(run, running))
  $effect(() => {
    if (!busy) return
    const id = setInterval(() => document.visibilityState === 'visible' && void loadStatus(true), 4000)
    return () => clearInterval(id)
  })

  const ph = $derived(phase(run))
  const next = $derived(nextSteps(plan, run, busy))
  const pilotModels = $derived(Object.values(run?.pilot?.models ?? {}))
  const prog = $derived(progress(run))
  // the cap is one total per server: spent and left are every run's, never the newest run's alone
  const spend = $derived(serverSpend(run))
  const planMoney = $derived(planSpend(plan))
  const pending = $derived(pendingClaims(run))
  const costs = $derived([
    { key: 'haiku' as ModelKey, name: 'Haiku 5.5', range: plan?.cost_usd?.haiku?.batch, within: plan?.within_budget?.haiku },
    { key: 'sonnet' as ModelKey, name: 'Sonnet 5.5', range: plan?.cost_usd?.sonnet?.batch, within: plan?.within_budget?.sonnet },
  ])
  const money = (r: Range | undefined) => {
    const p = rangeParts(r)
    return p ? (p.low === p.high ? usd(p.high) : t('pb.range', { low: usd(p.low), high: usd(p.high) })) : '–'
  }
  const phaseTone = { none: 'neutral', piloting: 'busy', piloted: 'info', running: 'busy', stopped: 'warn', done: 'ok' } as const

  // model of the real run: the one chosen before, or the operator's pick
  let model = $state<ModelKey>('haiku')
  $effect(() => {
    const k = modelKey(run?.model)
    if (k) model = k
  })
  const modelLocked = $derived(!!modelKey(run?.model))

  let pilotOpen = $state(false)
  let runOpen = $state(false)
  let requeueOpen = $state(false)
  let reviewOpen = $state(false)

  async function startPilot() {
    if (!plan?.token || cap === null) return
    const r = await call('memory.people.backfill', { mode: 'pilot', guild_id: guild, budget_usd: String(cap), confirm: plan.token })
    if (!r.ok) {
      toast('error', t('common.failed'), r.error?.message)
      // the stored conversations moved on since the estimate: the refusal carries the new one
      const fresh = r.error?.args?.dry_run as DryRun | undefined
      if (r.error?.code === 'STALE_REVISION' && fresh) plan = fresh
      return
    }
    toast('ok', t('pb.pilot_started'))
    await loadStatus()
  }
  async function startRun(requeue = false) {
    if (!run?.token) return
    const r = await call('memory.people.backfill', { mode: 'run', guild_id: guild, model, budget_usd: String(run.budget_usd ?? cap ?? 3), confirm: run.token, ...(requeue ? { requeue_unknown: true } : {}) })
    if (!r.ok) return void toast('error', t('common.failed'), r.error?.message)
    toast('ok', t('pb.run_started'))
    await loadStatus()
  }

  const stopText = (s: string | undefined) => (s && ['budget', 'restart', 'error', 'submit_unknown'].includes(s) ? t(`pb.stop.${s}`) : '')
  const pilotCount = $derived(plan?.pilot_windows ?? run?.pilot?.windows ?? 0)
</script>

<Card title={t('pb.title')} description={t('pb.desc')}>
  <div class="pb">
    <div class="callout"><span class="ic"><I.Info size={16} aria-hidden="true" /></span><span>{t('pb.note')}</span></div>

    {#if !channels.data}
      <Skeleton lines={3} />
    {:else if servers.length === 0}
      <p class="small muted">{t('pb.no_servers')}</p>
    {:else}
      <div class="top">
        <Field id="pb-server" label={t('pb.server')}>
          {#snippet children({ id })}<Select {id} bind:value={guild} options={servers} />{/snippet}
        </Field>
        {#if statusReady && ph !== 'none'}<Status tone={phaseTone[ph]} label={t(`pb.phase.${ph}`)} />{/if}
      </div>

      <section aria-labelledby="pb-h1" class="step">
        <h3 id="pb-h1" class="h">{t('pb.s1')}</h3>
        <p class="small muted">{t('pb.s1_desc')}</p>
        <div class="budget">
          <Field id="pb-budget" label={t('pb.budget')} hint={t('pb.budget_hint')} error={cap === null ? t('pb.budget_bad', { max: num(MAX_BUDGET_USD) }) : undefined}>
            {#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={budget} inputmode="decimal" autocomplete="off" />{/snippet}
          </Field>
          <Button icon={I.Coins} loading={planBusy} disabled={cap === null || busy} onclick={estimate}>{t('pb.estimate')}</Button>
        </div>
        {#if planError}<div role="alert" class="callout danger"><span class="ic"><I.CircleAlert size={16} aria-hidden="true" /></span><span>{planError}</span></div>{/if}
        {#if plan}
          {#if plan.provider_ready === false}
            <div role="alert" class="callout warn"><span class="ic"><I.TriangleAlert size={16} aria-hidden="true" /></span><span>{t('pb.no_provider')}</span><Button size="sm" variant="ghost" href="/connections">{t('pb.open_connections')}</Button></div>
          {/if}
          {#if pendingWarning(plan)}
            <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} aria-hidden="true" /></span><span>{t('pb.warn.no_sensitive_pending')}</span></div>
          {/if}
          {#if (plan.windows ?? 0) === 0}
            <p class="small muted">{t('pb.no_windows')}</p>
          {:else}
            <KeyValue items={[
              { k: t('pb.k.rooms'), v: t('pb.n_rooms', { n: num(plan.rooms) }) },
              { k: t('pb.k.windows'), v: t('pb.n_windows', { sent: num(plan.sent_windows), skipped: num(plan.skipped_windows) }) },
              { k: t('pb.k.lines'), v: t('pb.n_lines', { lines: num(plan.lines), merged: num(plan.merged_lines) }) },
              { k: t('pb.k.tokens'), v: t('pb.n_tokens', { input: num(plan.tokens?.input), output: num(plan.tokens?.output) }) },
              { k: t('pb.k.until'), v: datetime(plan.until) },
              ...(planMoney ? [{ k: t('pb.k.spent'), v: t('pb.spent_left', { spent: usd(planMoney.spent), left: usd(planMoney.left), cap: usd(planMoney.cap) }) }] : []),
            ]} />
            <div class="tbl"><table class="table">
              <caption class="sr-only">{t('pb.costs')}</caption>
              <thead><tr><th>{t('pb.col.model')}</th><th class="r">{t('pb.col.cost')}</th><th>{t('pb.col.cap', { cap: usd(plan.budget_usd) })}</th></tr></thead>
              <tbody>
                <tr><td>{t('pb.row.pilot')}</td><td class="r num">{money(plan.cost_usd?.pilot)}</td><td></td></tr>
                {#each costs as c (c.key)}
                  <tr><td>{t('pb.row.run', { model: c.name })}</td><td class="r num">{money(c.range)}</td>
                    <td>{#if c.within !== undefined}<Status size="sm" tone={c.within ? 'ok' : 'warn'} label={c.within ? t('pb.within') : t('pb.over')} />{/if}</td></tr>
                {/each}
              </tbody>
            </table></div>
            <p class="xs muted">{t('pb.costs_hint')}</p>
          {/if}
        {/if}
      </section>

      <section aria-labelledby="pb-h2" class="step">
        <h3 id="pb-h2" class="h">{t('pb.s2')}</h3>
        <p class="small muted">{t('pb.s2_desc', { n: num(pilotCount || 60) })}</p>
        <div class="row">
          <Button icon={I.FlaskConical} disabled={!next.pilot || (plan?.windows ?? 0) === 0} onclick={() => (pilotOpen = true)}>{t('pb.pilot')}</Button>
          {#if !plan && ph === 'none'}<span class="xs muted">{t('pb.needs_estimate')}</span>{/if}
        </div>
        {#if run && (ph === 'piloting' || run.pilot?.models)}
          {#if ph === 'piloting'}
            <p class="small" aria-live="polite">{t('pb.piloting', { cost: usd(run.spent_usd) })}</p>
          {/if}
          {#if pilotModels.length}
            <div class="tbl"><table class="table">
              <caption class="sr-only">{t('pb.pilot_results')}</caption>
              <thead><tr><th>{t('pb.col.model')}</th><th class="r">{t('pb.col.answered')}</th><th class="r">{t('pb.col.facts')}</th><th class="r">{t('pb.col.rejects')}</th><th class="r">{t('pb.col.refused')}</th><th class="r">{t('pb.col.spent')}</th></tr></thead>
              <tbody>
                {#each pilotModels as m (m.model)}
                  <tr><td>{modelName(m.model)}</td><td class="r num">{num(m.answered)}/{num(run.pilot?.windows)}</td><td class="r num">{num(m.facts)}</td><td class="r num">{num(m.rejected)} ({pct(m.reject_rate ?? 0)})</td><td class="r num">{num(m.refused)}</td><td class="r num">{usd(m.cost_usd)}</td></tr>
                {/each}
              </tbody>
            </table></div>
            <p class="xs muted">{t('pb.pilot_counts', { names: num(run.pilot?.name_links), diff: num(run.pilot?.disagreements), heard: num(run.pilot?.hearsay) })}</p>
          {/if}
          {#if next.review}<div><Button icon={I.ListChecks} onclick={() => (reviewOpen = true)}>{t('pb.review')}</Button></div>{/if}
        {/if}
      </section>

      <section aria-labelledby="pb-h3" class="step">
        <h3 id="pb-h3" class="h">{t('pb.s3')}</h3>
        {#if ph === 'none' || ph === 'piloting' || !run?.pilot_done}
          <p class="small muted">{t('pb.s3_wait')}</p>
        {:else}
          <p class="small muted">{t('pb.s3_desc')}</p>
          <div class="row">
            {#if modelLocked}
              <span class="small">{t('pb.model_locked', { model: modelName(run.model) })}</span>
            {:else}
              <Segmented label={t('pb.model')} bind:value={model} size="sm" options={[{ value: 'haiku', label: t('pb.model.haiku') }, { value: 'sonnet', label: t('pb.model.sonnet') }]} />
            {/if}
            <span class="small muted">{spend ? t('pb.cap_left', { cap: usd(spend.cap), left: usd(spend.left) }) : t('pb.cap', { cap: usd(run.budget_usd) })}</span>
          </div>
          <div class="row">
            {#if next.run}<Button variant="primary" icon={I.Play} onclick={() => (runOpen = true)}>{t('pb.run')}</Button>{/if}
            {#if next.resume}<Button variant="primary" icon={I.Play} onclick={() => (runOpen = true)}>{t('pb.resume')}</Button>{/if}
          </div>
        {/if}
        {#if run && ph !== 'none' && ph !== 'piloted' && ph !== 'piloting' && run.windows}
          <div class="prog" aria-live="polite">
            <p class="small">{t('pb.progress_n', { done: num(prog.done), total: num(prog.total) })}</p>
            <Progress value={prog.done} max={prog.total || 1} label={t('pb.progress')} tone={ph === 'done' ? 'ok' : ph === 'stopped' ? 'warn' : 'accent'} />
            <KeyValue items={[
              { k: t('pb.w.applied'), v: num(run.windows.applied) },
              { k: t('pb.w.queued'), v: num((run.windows.queued ?? 0) + (run.windows.submitted ?? 0)) },
              { k: t('pb.w.failed'), v: num(run.windows.failed) },
              { k: t('pb.w.refused'), v: num(run.windows.refused) },
              { k: t('pb.w.skipped'), v: num(run.windows.skipped) },
              { k: t('pb.w.claims'), v: t('pb.w.claims_v', { claims: num(run.claims), rejected: num(run.rejected) }) },
              ...(pending ? [{ k: t('pb.w.pending'), v: t('pb.w.pending_v', { n: num(pending) }) }] : []),
              { k: t('pb.w.spent'), v: spend ? t('pb.w.spent_v', { spent: usd(spend.spent, 4), cap: usd(spend.cap), left: usd(spend.left) }) : '–' },
            ]} />
            {#if spend}<Progress value={Math.min(spend.spent, spend.cap)} max={spend.cap} label={t('pb.spent_bar')} tone={spend.left <= 0 ? 'danger' : 'accent'} />{/if}
            {#if pending}
              <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} aria-hidden="true" /></span><span>{t('pb.pending_note', { n: num(pending) })}</span></div>
            {/if}
            {#if ph === 'stopped' && stopText(run.stopped_by)}
              <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} aria-hidden="true" /></span><span>{stopText(run.stopped_by)}</span>
                {#if run.stopped_by === 'submit_unknown'}<Button size="sm" variant="ghost" onclick={() => (requeueOpen = true)}>{t('pb.requeue')}</Button>{/if}</div>
            {/if}
          </div>
        {/if}
      </section>
    {/if}
  </div>
</Card>

<Confirm bind:open={pilotOpen} title={t('pb.pilot_title')} body={t('pb.pilot_body', { n: num(pilotCount || 60), cost: money(plan?.cost_usd?.pilot) })} confirmLabel={t('pb.pilot_confirm')} onconfirm={startPilot} />
<Confirm bind:open={runOpen} title={t('pb.run_title')} body={t('pb.run_body', { model: model === 'haiku' ? 'Haiku 5.5' : 'Sonnet 5.5', n: num(run?.windows_total), cap: usd(run?.budget_usd) })} confirmLabel={t('pb.run_confirm')} onconfirm={() => startRun()} />
<Confirm bind:open={requeueOpen} title={t('pb.requeue_title')} body={t('pb.requeue_body')} confirmLabel={t('pb.requeue_confirm')} onconfirm={() => startRun(true)} />
<PersonBackfillReview bind:open={reviewOpen} guild={guild} runId={run?.id ?? ''} />

<style>
  .pb {
    display: grid;
    gap: var(--s-4);
    min-width: 0;
  }
  .top {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--s-3);
  }
  .top :global(.field) {
    min-width: min(100%, 260px);
  }
  .step {
    display: grid;
    gap: var(--s-3);
    padding-top: var(--s-4);
    box-shadow: inset 0 1px 0 var(--border);
    min-width: 0;
  }
  .h {
    margin: 0;
    font-size: var(--text-md);
  }
  .budget {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--s-3);
  }
  .budget :global(.field) {
    flex: 0 1 260px;
  }
  @media (min-width: 640px) {
    .budget :global(.btn) {
      margin-top: 26px; /* level with the input, under the label */
    }
  }
  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-3);
  }
  .tbl {
    overflow-x: auto;
  }
  .prog {
    display: grid;
    gap: var(--s-3);
  }
</style>
