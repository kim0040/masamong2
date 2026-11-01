<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { num, usd } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { estimateView, validCap, jobOutcome } from '../lib/roommemory'
  import Dialog from './Dialog.svelte'
  import Button from './Button.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Progress from './Progress.svelte'
  import Skeleton from './Skeleton.svelte'
  import * as I from '../lib/icons'

  // Room memory for an imported KakaoTalk block: a free estimate first (dry run), then an approval that
  // queues the extraction. It continues in the background, and the budget it can spend is this month's
  // room memory budget (memory.derived_budget_usd) plus the cap typed here for this run.
  let { open = $bindable(false), block }: { open?: boolean; block: any } = $props()
  let est = $state<any>(null)
  let loading = $state(false)
  let err = $state('')
  let cap = $state('')
  let starting = $state(false)
  const v = $derived(estimateView(est))
  const capBad = $derived(cap !== '' && !validCap(cap))

  async function estimate() {
    loading = true
    err = ''
    est = null
    const r = await call('memory.episodes.extract', { source_room: block.source_room, dry_run: true })
    loading = false
    if (!r.ok) return void (err = r.error?.message ?? t('common.failed'))
    est = r.output
    cap = estimateView(est).defaultCap
  }
  $effect(() => {
    if (open) untrack(() => void estimate())
  })

  async function start() {
    if (!validCap(cap)) return void (err = t('wizard.err.budget'))
    starting = true
    err = ''
    const r = await call('memory.episodes.extract', { source_room: block.source_room, budget_usd: cap.trim() })
    starting = false
    if (!r.ok) return void (err = r.error?.message ?? t('common.failed'))
    const outcome = jobOutcome(r.output.job)
    toast(outcome === 'stopped_budget' || outcome === 'stopped_error' ? 'warn' : 'ok', t(`rm.ex.toast.${outcome}`), block.label)
    open = false
  }
</script>

<Dialog bind:open title={t('rm.ex.title', { name: block.label })} description={t('rm.ex.desc')}>
  <div class="stack">
    {#if loading}
      <p class="small muted" role="status">{t('rm.ex.estimating')}</p>
      <Skeleton lines={3} />
    {:else if est}
      {#if v.empty}
        <p class="small muted">{t('rm.ex.empty')}</p>
      {:else}
        <dl class="est">
          <div><dt class="xs muted">{t('rm.ex.messages')}</dt><dd class="num">{num(est.messages)}</dd></div>
          <div><dt class="xs muted">{t('rm.ex.segments')}</dt><dd class="num">{num(est.segments)}</dd></div>
          <div><dt class="xs muted">{t('rm.ex.tokens')}</dt><dd class="num">{num(est.est_input_tokens)} / {num(est.est_output_tokens)}</dd></div>
          <div><dt class="xs muted">{t('rm.ex.cost')}</dt><dd class="num">{v.priceKnown ? usd(v.cost) : t('rm.ex.price_unknown')}</dd></div>
        </dl>
        <div class="stack tight">
          <div class="row between small"><span class="muted">{t('rm.ex.remaining')}</span><span class="num">{#if v.budget.state === 'off'}{t('rm.budget.off')}{:else}{t('rm.ex.left', { left: usd(v.budget.remaining), limit: usd(v.budget.limit) })}{/if}</span></div>
          {#if v.budget.limit > 0}<Progress value={v.budget.spent} max={v.budget.limit} label={t('rm.budget_title')} tone={v.budget.tone === 'neutral' ? 'accent' : v.budget.tone} />{/if}
        </div>
        {#if v.blocked}
          <div class="callout danger" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{v.budget.state === 'off' ? t('rm.ex.blocked_off') : t('rm.ex.blocked_stopped')} <a href="/blocks?view=settings" onclick={() => (open = false)}>{t('rm.budget_edit')}</a></span></div>
        {:else if v.over}
          <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('rm.ex.over')}</span></div>
        {/if}
        <Field id="ex-cap" label={t('rm.ex.cap')} hint={t('rm.ex.cap_hint')} error={capBad ? t('wizard.err.budget') : undefined}>
          {#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={cap} inputmode="decimal" />{/snippet}
        </Field>
        <p class="xs muted">{t('rm.ex.note')}</p>
      {/if}
    {/if}
    {#if err}<p class="small err" role="alert">{err}</p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (open = false)}>{t('common.cancel')}</Button>
    <Button variant="ghost" icon={I.RefreshCw} loading={loading} onclick={estimate}>{t('block.re_estimate')}</Button>
    <Button variant="primary" icon={I.Sparkles} loading={starting} disabled={!est || v.empty || v.blocked || capBad || cap === ''} onclick={start}>{t('rm.ex.start')}</Button>
  {/snippet}
</Dialog>

<style>
  .est {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 96px), 1fr));
    margin: 0;
  }
  .est > div {
    display: grid;
    gap: 2px;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    min-width: 0;
  }
  .est dd {
    margin: 0;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .tight {
    gap: 6px;
  }
  .between {
    justify-content: space-between;
  }
  .err {
    color: var(--danger);
  }
</style>
