<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import { route, navigate } from '../lib/router.svelte'
  import { num, usd, relative, datetime, duration } from '../lib/format'
  import { etaText, blockState, stateTone, period, jobLine, stopReason, isKakao, modeOf, batchOffer, effectiveMode, batchWaitLine, type PrepareMode } from '../lib/blocks'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Status from '../components/Status.svelte'
  import Progress from '../components/Progress.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Switch from '../components/Switch.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import Confirm from '../components/Confirm.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import ConnectDialog from '../components/ConnectDialog.svelte'
  import QuickReindex from '../components/QuickReindex.svelte'
  import BatchChoice from '../components/BatchChoice.svelte'
  import { indexHasWork, quickWork } from '../lib/quickreindex'
  import * as I from '../lib/icons'

  let { params }: { params: { room: string } } = $props()
  useConfig()
  const room = $derived(decodeURIComponent(params.room))
  const data = resource<any>('memory.blocks.list', () => ({}), { live: ['config', 'job', 'memory'] })
  const b = $derived(((data.data?.blocks ?? []) as any[]).find((x) => x.source_room === room))
  const channels = $derived((data.data?.channels ?? []) as any[])
  const st = $derived(b ? blockState(b) : 'empty')
  const running = $derived(st === 'preparing')
  // a batch submission that got no answer: only the operator can settle it (check the provider, then discard)
  const unsettled = $derived(b?.batch?.state === 'submit_unknown')

  // name and note
  let label = $state('')
  let note = $state('')
  let seen = $state('')
  $effect(() => {
    if (!b) return
    const key = b.label + '\u0000' + (b.note ?? '')
    if (key !== seen) {
      label = b.label
      note = b.note ?? ''
      seen = key
    }
  })
  const dirty = $derived(!!b && (label.trim() !== b.label || note.trim() !== (b.note ?? '')))
  let saving = $state(false)
  let nameErr = $state('')
  async function saveMeta() {
    nameErr = ''
    if (!label.trim()) return void (nameErr = t('wizard.err.label'))
    saving = true
    const input: Record<string, unknown> = { source_room: room }
    if (label.trim() !== b.label) input.label = label.trim()
    if (note.trim() !== (b.note ?? '')) input.note = note.trim()
    const r = await call('memory.blocks.update', input)
    saving = false
    if (r.ok) {
      toast('ok', t('block.saved'))
      seen = ''
      void data.refresh()
    } else nameErr = r.error?.message ?? t('common.failed')
  }

  // prepare: estimate (free) → budget → background job
  let estimate = $state<any>(null)
  let estimating = $state(false)
  let budget = $state('')
  let estErr = $state('')
  async function runEstimate() {
    estimating = true
    estErr = ''
    // mode=batch makes the one dry run carry both prices (total_usd now, batch_usd as a batch)
    const r = await call('memory.blocks.prepare', { source_room: room, dry_run: true, mode: 'batch' })
    estimating = false
    if (r.ok) estimate = r.output
    else estErr = r.error?.message ?? ''
  }
  $effect(() => {
    if (b && !estimate && !estimating && b.message_count && !running) void runEstimate()
  })
  $effect(() => {
    if (b && !budget) budget = b.budget_usd || cfg.doc?.memory?.index_budget_usd || '1.00'
  })
  let mode = $state<PrepareMode>('live')
  let modeSeen = $state('')
  $effect(() => {
    // follows the saved default until the person picks one
    if (b && modeSeen !== (b.mode ?? '') + '|' + b.source_room) {
      mode = modeOf(b)
      modeSeen = (b.mode ?? '') + '|' + b.source_room
    }
  })
  const chosen = $derived(effectiveMode(mode, estimate?.report?.estimate))
  async function saveModeDefault() {
    const r = await call('memory.blocks.update', { source_room: room, mode })
    r.ok ? toast('ok', t('block.mode_saved')) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }
  let starting = $state(false)
  async function start() {
    const v = parseFloat(budget)
    if (Number.isNaN(v) || v < 0 || v > 1000) return void (estErr = t('wizard.err.budget'))
    starting = true
    const r = await call('memory.blocks.prepare', { source_room: room, background: true, budget_usd: String(v), mode: chosen })
    starting = false
    if (r.ok) {
      if (r.output.batch_resumed) toast('info', t('blocks.batch_resumed'), b.label)
      else if (r.output.already_running) toast('ok', t('blocks.already_running'), b.label)
      else toast('ok', r.output.mode === 'batch' ? t('blocks.batch_started') : t('blocks.prepare_started'), b.label)
      estimate = null
    } else estErr = r.error?.message ?? t('common.failed')
    void data.refresh()
  }
  async function cancel() {
    const r = await call('jobs.cancel', { id: b.job.id })
    r.ok ? toast('warn', t('blocks.cancelled')) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }
  async function saveBudgetDefault() {
    const r = await call('memory.blocks.update', { source_room: room, budget_usd: budget.trim() })
    r.ok ? toast('ok', t('block.budget_saved')) : toast('error', t('common.failed'), r.error?.message)
  }
  const est = $derived(estimate?.report?.estimate)
  // message vectors and leaves that only lack a cached or summary vector are work too (est.leaf_embeds and est.summaries miss them)
  const hasWork = $derived(indexHasWork(estimate?.report))
  const lastResult = $derived.by(() => {
    try {
      return b?.job?.result ? JSON.parse(b.job.result) : null
    } catch {
      return null
    }
  })
  $effect(() => {
    if (route.query.get('prepare') === '1' && b) document.getElementById('prepare')?.scrollIntoView({ block: 'start' })
  })

  // Discord: capture and background updates
  async function setCapture(v: boolean) {
    const oc = b.own_channel
    const ops: any[] = [{ path: ['guilds', oc.guild_id, 'channels', oc.channel_id, 'capture'], value: v }]
    if (v && !cfg.doc?.memory?.capture) ops.push({ path: ['memory', 'capture'], value: true })
    await patchConfig(ops, { success: v ? t('blocks.capture_on_toast', { name: b.label }) : t('block.capture_off_toast') })
    void data.refresh()
  }
  async function setAuto(v: boolean) {
    const r = await call('memory.blocks.update', { source_room: room, auto_update: v })
    r.ok ? toast('ok', t('block.saved')) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }

  let connectOpen = $state(false)
  async function detach(c: any) {
    const r = await call('channels.view.detach', { guild_id: c.guild_id, channel_id: c.channel_id, block_id: c.block_id })
    r.ok ? toast('ok', t('connect.detached', { block: b.label, channel: c.name })) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }

  let discardOpen = $state(false)
  async function discardBatch() {
    const r = await call('memory.blocks.update', { source_room: room, discard_batch: true })
    r.ok ? toast('ok', t('block.batch_discarded')) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }

  let delOpen = $state(false)
  async function remove() {
    const r = await call('memory.blocks.delete', { source_room: room, confirm: b.label })
    if (r.ok) {
      toast('ok', t('block.deleted', { name: b.label, n: num(r.output.messages_removed) }))
      navigate('/blocks')
    } else toast('error', t('common.failed'), r.error?.message)
  }
</script>

{#if !data.data}
  <LoadState res={data}><Card><Skeleton lines={8} /></Card></LoadState>
{:else if !b}
  <Card><EmptyState pose="lost" title={t('block.not_found')} description={t('block.not_found_body')}><Button href="/blocks">{t('nav.blocks')}</Button></EmptyState></Card>
{:else}
  <PageHeader title={b.label} description={b.note || t(`blocks.platform.${b.platform}`)} crumbs={[{ href: '/blocks', label: t('nav.blocks') }]}>
    {#snippet actions()}
      <Button icon={I.FlaskConical} href="/lab?rooms={encodeURIComponent(room)}">{t('block.try_lab')}</Button>
      <Button variant="primary" icon={I.Link2} onclick={() => (connectOpen = true)} disabled={!channels.length}>{t('blocks.connect')}</Button>
    {/snippet}
    <div class="row">
      <Status tone={stateTone[st]} label={t(`blocks.state.${st}`)} size="sm" />
      <span class="chip">{t(`blocks.platform.${b.platform}`)}</span>
      {#if b.first}<span class="small muted">{period(b.first, b.last)}</span>{/if}
    </div>
  </PageHeader>

  <div class="cols">
    <Card title={t('block.overview')}>
      <KeyValue items={[
        { k: t('block.messages'), v: num(b.message_count) },
        { k: t('block.people'), v: b.speakers ? num(b.speakers) : '–' },
        { k: t('block.period'), v: b.first ? `${datetime(b.first)} – ${datetime(b.last)}` : '–' },
        { k: t('block.pieces'), v: b.leaf_count ? t('block.pieces_of', { done: num(b.embedded), all: num(b.leaf_count) }) : '–' },
        { k: t('block.prepared'), v: b.prepared_at ? `${relative(b.prepared_at)} · ${usd(b.cost_usd ?? 0)}` : t('block.never') },
      ]} />
      <details class="tech">
        <summary class="xs muted">{t('block.tech')}</summary>
        <div class="row xs"><code class="mono break">{b.source_room}</code><CopyButton text={b.source_room} /></div>
        <div class="row xs"><code class="mono break">{b.block_id}</code><CopyButton text={b.block_id} /></div>
      </details>
    </Card>

    <Card title={t('block.name_title')} description={t('block.name_desc')}>
      <form class="stack" onsubmit={(e) => { e.preventDefault(); void saveMeta() }}>
        <Field id="bl-name" label={t('wizard.label')} error={nameErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={label} maxlength={80} />{/snippet}</Field>
        <Field id="bl-note" label={t('block.note')} hint={t('block.note_hint', { n: note.length })}>{#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={note} rows={3} maxlength={500} />{/snippet}</Field>
        <div><Button type="submit" variant="primary" icon={I.Check} loading={saving} disabled={!dirty}>{t('common.save')}</Button></div>
      </form>
    </Card>

    <Card title={t('block.prepare_title')} description={t('block.prepare_desc')} id="prepare">
      <div class="stack">
        {#if running}
          {#if b.waiting_batch}
            <div class="callout" role="status"><span class="ic"><I.Hourglass size={16} /></span><span>{batchWaitLine(b)}</span></div>
            <p class="xs muted">{t(b.batch?.state === 'draining' ? 'block.batch_draining_note' : 'block.batch_note')}</p>
          {:else}
            <Progress value={b.job?.progress_pct ?? 0} label={t('blocks.preparing')} />
            <p class="xs muted">{t('block.running_note')}</p>
          {/if}
          {#if b.job}<div class="row between small"><span class="muted">{jobLine(b.job)}</span><span class="num">{Math.round(b.job.progress_pct ?? 0)}%{etaText(b.job) ? ` · ${t('blocks.eta', { eta: etaText(b.job) })}` : ''}</span></div>{/if}
          {#if b.job}<div><Button variant="danger-ghost" icon={I.X} onclick={cancel}>{t('block.cancel_run')}</Button></div>{/if}
        {:else}
          {#if st === 'ready'}<div class="callout ok"><span class="ic"><I.CircleCheck size={16} /></span><span>{t('block.ready_note')}</span></div>{/if}
          {#if unsettled}
            <div class="callout danger" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.batch_unknown', { when: relative(b.batch?.submitted_at) })}</span></div>
            <div><Button variant="danger-ghost" icon={I.Trash2} onclick={() => (discardOpen = true)}>{t('block.discard_batch')}</Button></div>
          {:else if st === 'failed' && b.error}<div class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span>{stopReason(b.error)}</span></div>{/if}
          {#if estimating}<p class="small muted">{t('wizard.estimating')}</p>{/if}
          {#if est}
            {#if quickWork(estimate?.report).failedRooms > 0}
              <div class="callout warn" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.quick.sync_failed', { n: quickWork(estimate?.report).failedRooms })}</span></div>
            {:else if !hasWork}
              <p class="small muted">{t('block.nothing_to_do')}</p>
            {:else}
              <div class="est">
                <div><span class="xs muted">{t('blocks.est.pieces')}</span><strong class="num">{num(est.leaf_embeds)}</strong>{#if est.cached_leaves}<span class="xs muted">{t('blocks.est.cached', { n: num(est.cached_leaves) })}</span>{/if}</div>
                <div><span class="xs muted">{t('blocks.est.time')}</span><strong>{chosen === 'batch' ? t('blocks.est.batch_time') : est.seconds < 60 ? t('blocks.est.under_minute') : t('blocks.est.approx', { d: duration(est.seconds) })}</strong></div>
                <div><span class="xs muted">{t('blocks.est.cost')}</span><strong class="num">{usd(chosen === 'batch' ? (batchOffer(est)?.batch ?? est.total_usd) : est.total_usd)}</strong></div>
                <div><span class="xs muted">{t('blocks.est.summaries')}</span><strong class="num">{num(est.summaries)}</strong></div>
              </div>
              <p class="xs muted">{est.price_basis === 'fallback' ? t('blocks.est.basis_fallback') : t('blocks.est.basis_prices')}</p>
            {/if}
          {/if}
          {#if estimate && estimate.llm_configured === false}<div class="callout warn"><span class="ic"><I.KeyRound size={16} /></span><span>{t('blocks.no_llm')} <a href="/connections">{t('nav.connections')}</a></span></div>{/if}
          {#if est && hasWork}<BatchChoice id="bl-mode" bind:mode {est} saved={modeOf(b)} onsave={saveModeDefault} />{/if}
          <Field id="bl-budget" label={t('blocks.budget')} hint={t('blocks.budget_hint')} error={estErr || undefined}>
            {#snippet children({ id, describedby, invalid })}<div class="row"><Input {id} aria-describedby={describedby} {invalid} bind:value={budget} inputmode="decimal" /><Button size="sm" variant="ghost" onclick={saveBudgetDefault}>{t('block.budget_default')}</Button></div>{/snippet}
          </Field>
          <div class="row">
            <Button variant="primary" icon={I.Sparkles} loading={starting} disabled={!est || !hasWork || estimate?.llm_configured === false} onclick={start}>{st === 'ready' || st === 'partial' ? t('blocks.update') : t('blocks.prepare_start')}</Button>
            <Button variant="ghost" icon={I.RefreshCw} loading={estimating} onclick={runEstimate}>{t('block.re_estimate')}</Button>
          </div>
          <QuickReindex {room} {budget} llmOk={estimate?.llm_configured !== false} prepared={st === 'ready' || st === 'partial'} pendingLeaves={b.pending_leaves ?? 0} ondone={() => { estimate = null; void data.refresh() }} />
        {/if}
        {#if lastResult && !running}
          <div class="last">
            <div class="small"><strong>{t('block.last_run')}</strong> <span class="muted">· {t(`jobs.status.${b.job.status}`)}</span></div>
            <KeyValue items={[
              { k: t('index.r.vectors'), v: num(lastResult.leaf_vectors ?? 0) },
              { k: t('index.r.cache'), v: num(lastResult.from_cache ?? 0) },
              { k: t('index.r.summaries'), v: num(lastResult.summaries ?? 0) },
              { k: t('index.r.spent'), v: usd(lastResult.spent_usd ?? 0) },
              { k: t('index.r.elapsed'), v: lastResult.elapsed ?? '–' },
              ...(lastResult.mode === 'batch' ? [{ k: t('block.batch_result'), v: t('block.batch_result_v', { batch: num(lastResult.batch_items ?? 0), failed: num(lastResult.batch_failed ?? 0), live: num(lastResult.live_items ?? 0) }) }] : []),
              ...(lastResult.stopped_by || lastResult.error ? [{ k: t('index.r.stopped'), v: stopReason(lastResult.stopped_by) || lastResult.error }] : []),
            ]} />
          </div>
        {/if}
      </div>
    </Card>

    <Card title={t('block.channels_title')} description={t('block.channels_desc')}>
      {#if (b.channels ?? []).length === 0}
        <p class="small muted">{t('blocks.no_links')}</p>
      {:else}
        <ul class="links">
          {#each b.channels ?? [] as c (c.guild_id + c.channel_id)}
            <li><a href="/rooms/{c.guild_id}/{c.channel_id}"><I.Hash size={14} aria-hidden="true" />{c.name}</a><span class="xs muted">{c.guild_name}</span>
              <Button size="sm" variant="danger-ghost" icon={I.Unlink} onclick={() => detach(c)}>{t('block.detach')}</Button></li>
          {/each}
        </ul>
      {/if}
      <div style:margin-top="12px"><Button size="sm" icon={I.Link2} onclick={() => (connectOpen = true)} disabled={!channels.length}>{t('blocks.connect')}</Button></div>
    </Card>

    {#if b.own_channel}
      <Card title={t('block.discord_title')} description={t('block.discord_desc', { channel: b.own_channel.name })}>
        <div class="stack">
          <Switch checked={b.own_channel.capture} onchange={setCapture} label={t('room.capture')} hint={t('block.capture_hint')} />
          {#if b.own_channel.capture && !cfg.doc?.memory?.capture}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('block.capture_global_off')}</span><Button size="sm" variant="primary" onclick={() => patchConfig([{ path: ['memory', 'capture'], value: true }], { success: t('block.capture_global_on') })}>{t('block.capture_global_turn_on')}</Button></div>{/if}
          <Switch checked={b.auto_update} onchange={setAuto} label={t('block.auto_update')} hint={t('block.auto_update_hint', { min: cfg.doc?.memory?.index_interval_min ?? 10 })} />
        </div>
      </Card>
    {/if}

    <Card title={t('block.danger_title')} tone="muted">
      <p class="small muted">{isKakao(b) ? t('block.delete_kakao') : t('block.delete_discord')}</p>
      <div style:margin-top="12px"><Button variant="danger" icon={I.Trash2} disabled={running} onclick={() => (delOpen = true)}>{t('block.delete')}</Button></div>
    </Card>
  </div>

  <ConnectDialog bind:open={connectOpen} block={b} {channels} onsaved={() => void data.refresh()} />
  <Confirm bind:open={discardOpen} danger title={t('block.discard_batch_title')} body={t('block.discard_batch_body')} confirmLabel={t('block.discard_batch')} onconfirm={discardBatch} />
  <Confirm bind:open={delOpen} danger title={t('block.delete_title', { name: b.label })} body={t('block.delete_body', { n: num(b.message_count), links: (b.channels ?? []).length })} requireText={b.label} confirmLabel={t('block.delete')} onconfirm={remove} />
{/if}

<style>
  /* two columns; an odd last card takes the whole row instead of leaving half of it empty */
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
  }
  @media (min-width: 1000px) {
    .cols {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .cols > :global(:last-child:nth-child(odd)) {
      grid-column: 1 / -1;
    }
  }
  .tech {
    margin-top: var(--s-3);
  }
  .tech summary {
    cursor: pointer;
  }
  .tech .row {
    margin-top: 6px;
  }
  .between {
    justify-content: space-between;
  }
  .est {
    display: grid;
    gap: var(--s-2);
    grid-template-columns: repeat(auto-fit, minmax(110px, 1fr));
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .est div {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .est strong {
    font-size: var(--text-lg);
  }
  .last {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding-top: var(--s-3);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .links {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  /* a room that Discord has not named yet is shown by its ID, which has no break points: wrap the row
     and let the ID break, so the Disconnect button stays inside the card on a phone */
  .links li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2);
    min-width: 0;
  }
  .links li a {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    min-width: 0;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .links li span {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .links li :global(button) {
    margin-inline-start: auto;
  }
</style>
