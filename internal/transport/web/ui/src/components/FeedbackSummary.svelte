<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { num } from '../lib/format'
  import Segmented from './Segmented.svelte'
  import Button from './Button.svelte'

  // feedback.summary on the Turns page: how people rated the bot's replies over the last N days (the
  // per-turn counts sit in the list below). Counts only; the Action keeps no text and no account IDs.
  let days = $state<'7' | '30' | '90'>('30')
  const sum = resource<{ good: number; bad: number; reasons: Record<string, number>; implicit?: Record<string, number> }>('feedback.summary', () => ({ days: Number(days) }), { live: ['turn'] })
  const good = $derived(sum.data?.good ?? 0)
  const bad = $derived(sum.data?.bad ?? 0)
  const total = $derived(good + bad)
  const reasons = $derived(Object.entries(sum.data?.reasons ?? {}).sort((a, b) => b[1] - a[1]))
  // Detected from what people did after a reply (recall, thanks …): a hint for choosing turns to look at, never counted in the ratings above
  const implicit = $derived(Object.entries(sum.data?.implicit ?? {}).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1]))
  const reasonLabel = (r: string) => (['wrong', 'forgot', 'tone', 'too_long', 'other'].includes(r) ? t(`turns.feedback_reason.${r}`) : r)
</script>

<!-- A failed first load stays visible with a retry: an empty gap would read as "no ratings" or "no such feature". A refresh that fails over data already shown is the page-wide stale banner's job. -->
{#if !sum.data && sum.error}
  <div class="fbs" role="alert">
    <strong class="small">{t('turns.fbs.title')}</strong>
    <Segmented size="sm" label={t('turns.fbs.period')} bind:value={days} options={(['7', '30', '90'] as const).map((d) => ({ value: d, label: t('turns.fbs.days', { n: d }) }))} />
    <span class="small bad">{t('turns.fbs.failed')}{sum.error.message ? ` (${sum.error.message})` : ''}</span>
    <Button size="sm" variant="ghost" loading={sum.loading} onclick={() => void sum.refresh()}>{t('common.retry')}</Button>
  </div>
{:else if sum.data}
  <div class="fbs" title={t('turns.fbs.hint')}>
    <strong class="small">{t('turns.fbs.title')}</strong>
    <Segmented size="sm" label={t('turns.fbs.period')} bind:value={days} options={(['7', '30', '90'] as const).map((d) => ({ value: d, label: t('turns.fbs.days', { n: d }) }))} />
    {#if total === 0}
      <span class="small muted">{t('turns.fbs.none')}</span>
    {:else}
      <span class="num">👍 {num(good)}</span>
      <span class="num bad">👎 {num(bad)}</span>
      <span class="small muted num">{t('turns.fbs.share', { pct: Math.round((100 * good) / total) })}</span>
      {#if reasons.length}
        <span class="xs muted">{t('turns.fbs.reasons')}</span>
        {#each reasons as [r, n] (r)}<span class="chip">{reasonLabel(r)} <span class="num">{num(n)}</span></span>{/each}
      {/if}
    {/if}
    {#if implicit.length}
      <span class="xs muted" title={t('turns.implicit_title')}>{t('turns.implicit_label')}</span>
      {#each implicit as [k, n] (k)}<span class="chip auto">{t(`turns.implicit.${k}`)} <span class="num">{num(n)}</span></span>{/each}
    {/if}
  </div>
{/if}

<style>
  .chip.auto {
    background: transparent;
    outline: 1px dashed var(--border);
  }
  .fbs {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-3);
    margin-bottom: var(--s-4);
  }
  .bad {
    color: var(--danger);
  }
</style>
