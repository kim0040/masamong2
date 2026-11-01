<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { num, usd, duration } from '../lib/format'
  import { stopReason } from '../lib/blocks'
  import { QUICK_LIMIT, QUICK_MAX_SECONDS, quickWork, quickVerdict, quickOutcome } from '../lib/quickreindex'
  import { toast } from '../lib/toast.svelte'
  import Button from './Button.svelte'
  import KeyValue from './KeyValue.svelte'
  import * as I from '../lib/icons'

  // memory.reindex for this block's room: a small catch-up pass that runs inside the request instead of as a
  // background job, for example after a few new messages. The request holds the page's connection until the
  // pass ends and nothing on the server bounds or cancels that wait, so the preview decides first: the pass
  // only runs when the dry run shows a small job (see QUICK_LIMIT and QUICK_MAX_SECONDS); anything bigger is
  // steered to the background prepare above, which reports progress and can be cancelled. The preview itself
  // is not free of work: it cuts the block's new messages into leaves and stores them (local, no model call).
  // `prepared` and `pendingLeaves` come from the block row and keep the preview from starting that resync on a
  // block that was never prepared or already has a lot waiting.
  let { room, budget, llmOk = true, prepared = true, pendingLeaves = 0, ondone }: { room: string; budget: string; llmOk?: boolean; prepared?: boolean; pendingLeaves?: number; ondone?: () => void } = $props()
  let plan = $state<any>(null)
  let busy = $state<'' | 'preview' | 'run'>('')
  let err = $state('')
  let result = $state<any>(null)

  const work = $derived(plan ? quickWork(plan) : null)
  const verdict = $derived(plan ? quickVerdict(plan) : '')
  const outcome = $derived(result ? quickOutcome(result) : null)
  const blockedUpFront = $derived(!prepared ? 'unprepared' : pendingLeaves >= QUICK_LIMIT ? 'big' : '')

  async function preview() {
    // a second request would run beside the first one on a server with one core; the buttons are disabled
    // while busy, and this guard covers any other way in
    if (busy || blockedUpFront) return
    busy = 'preview'
    err = ''
    result = null
    plan = null // the last plan describes work that may be gone or changed; a failed preview must not leave it behind
    const r = await call('memory.reindex', { rooms: [room], dry_run: true, limit: QUICK_LIMIT })
    busy = ''
    if (r.ok) plan = r.output.report
    else err = r.error?.message ?? t('common.failed')
  }
  async function run() {
    if (busy || !plan || verdict !== 'ok' || !llmOk) return
    const v = parseFloat(budget)
    if (Number.isNaN(v) || v < 0 || v > 1000) return void (err = t('wizard.err.budget'))
    busy = 'run'
    err = ''
    const r = await call('memory.reindex', { rooms: [room], limit: QUICK_LIMIT, budget_usd: String(v) })
    busy = ''
    plan = null // whatever happened, the numbers shown before the run are out of date
    if (!r.ok) return void (err = `${t('block.quick.run_failed')} ${r.error?.message ?? t('common.failed')}`)
    result = r.output.report
    const o = quickOutcome(result)
    if (o.complete) toast('ok', t('block.quick.done'))
    else toast('warn', t('block.quick.partial'))
    ondone?.()
  }
</script>

<details class="quick">
  <summary class="small">{t('block.quick.title')}</summary>
  <div class="stack body">
    <p class="xs muted">{t('block.quick.desc', { n: QUICK_LIMIT, sec: QUICK_MAX_SECONDS })}</p>
    {#if blockedUpFront}
      <p class="small muted">{t(blockedUpFront === 'unprepared' ? 'block.quick.unprepared' : 'block.quick.too_big_known')}</p>
    {/if}
    {#if plan && work}
      {#if verdict === 'failed'}
        <div class="callout warn" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.quick.sync_failed', { n: work.failedRooms })}</span></div>
      {:else if verdict === 'nothing'}
        <p class="small muted">{t('block.nothing_to_do')}</p>
      {:else}
        <p class="small">{t('block.quick.plan', { leaves: num(work.leaves), embeds: num(work.embeds), cached: num(work.cached), sums: num(work.summaries), msgs: num(work.messages), cost: usd(work.usd), time: work.seconds < 60 ? t('blocks.est.under_minute') : duration(work.seconds) })}</p>
        {#if plan.estimate?.price_basis === 'fallback'}<p class="xs muted">{t('blocks.est.basis_fallback')}</p>{/if}
        {#if verdict === 'big'}
          <div class="callout warn"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.quick.too_big', { n: QUICK_LIMIT, sec: QUICK_MAX_SECONDS })}</span></div>
        {/if}
      {/if}
    {/if}
    {#if err}<p class="small err" role="alert">{err}</p>{/if}
    <div class="row">
      <Button size="sm" icon={I.Search} loading={busy === 'preview'} disabled={busy !== '' || !!blockedUpFront} onclick={preview}>{t('block.quick.preview')}</Button>
      <Button size="sm" variant="primary" icon={I.RefreshCw} loading={busy === 'run'} disabled={busy !== '' || verdict !== 'ok' || !llmOk} onclick={run}>{t('block.quick.run')}</Button>
    </div>
    {#if result && outcome}
      {#if outcome.failedRooms > 0}
        <div class="callout warn" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.quick.sync_failed', { n: outcome.failedRooms })}</span></div>
      {/if}
      {#if outcome.stoppedBy}
        <div class="callout warn" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('block.quick.stopped', { reason: stopReason(outcome.stoppedBy) })}</span></div>
      {/if}
      <KeyValue items={[
        { k: t('index.r.vectors'), v: num(result.leaf_vectors ?? 0) },
        { k: t('index.r.cache'), v: num(result.from_cache ?? 0) },
        { k: t('index.r.summaries'), v: num(result.summaries ?? 0) },
        { k: t('block.quick.r.summary_vectors'), v: num(result.summary_vectors ?? 0) },
        { k: t('block.quick.r.message_vectors'), v: num(result.message_vectors ?? 0) },
        { k: t('index.r.spent'), v: usd(result.spent_usd ?? 0) },
        { k: t('index.r.elapsed'), v: result.elapsed ?? '–' },
        ...(outcome.stoppedBy ? [{ k: t('index.r.stopped'), v: stopReason(outcome.stoppedBy) }] : []),
      ]} />
    {/if}
  </div>
</details>

<style>
  .quick summary {
    cursor: pointer;
  }
  .body {
    margin-top: var(--s-3);
  }
  .err {
    color: var(--danger);
  }
</style>
