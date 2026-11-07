<script lang="ts">
  import { t, toolName } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { num } from '../lib/format'
  import Segmented from './Segmented.svelte'
  import Button from './Button.svelte'

  // turns.marks on the Turns page: the verdicts the operator gave, in total and per tool. Counts only; they
  // are kept apart from the ratings people gave (FeedbackSummary) and never change how the bot behaves.
  type Totals = { right: number; wrong: number; stale: number }
  type ToolRow = { tool: string } & Totals
  let days = $state<'7' | '30' | '90'>('30')
  const sum = resource<{ total: Totals; tools: ToolRow[] }>('turns.marks', () => ({ days: Number(days) }), { live: ['turn'] })
  const total = $derived(sum.data?.total ?? { right: 0, wrong: 0, stale: 0 })
  const all = $derived(total.right + total.wrong + total.stale)
  const tools = $derived(sum.data?.tools ?? [])
</script>

{#if !sum.data && sum.error}
  <div class="ops" role="alert">
    <strong class="small">{t('turns.mark.title')}</strong>
    <span class="small bad">{t('turns.marks.failed')}{sum.error.message ? ` (${sum.error.message})` : ''}</span>
    <Button size="sm" variant="ghost" loading={sum.loading} onclick={() => void sum.refresh()}>{t('common.retry')}</Button>
  </div>
{:else if sum.data}
  <div class="ops" title={t('turns.marks.hint')}>
    <strong class="small">{t('turns.mark.title')}</strong>
    <Segmented size="sm" label={t('turns.fbs.period')} bind:value={days} options={(['7', '30', '90'] as const).map((d) => ({ value: d, label: t('turns.fbs.days', { n: d }) }))} />
    {#if all === 0}
      <span class="small muted">{t('turns.marks.none')}</span>
    {:else}
      <span class="chip">{t('turns.mark.right')} <span class="num">{num(total.right)}</span></span>
      <span class="chip bad">{t('turns.mark.wrong')} <span class="num">{num(total.wrong)}</span></span>
      <span class="chip warn">{t('turns.mark.stale')} <span class="num">{num(total.stale)}</span></span>
      <span class="small muted num">{t('turns.marks.accuracy', { pct: Math.round((100 * total.right) / all) })}</span>
      {#if tools.length}
        <details class="by-tool">
          <summary class="small">{t('turns.marks.by_tool')}</summary>
          <table class="table">
            <thead><tr><th>{t('turns.marks.tool')}</th><th class="r">{t('turns.mark.right')}</th><th class="r">{t('turns.mark.wrong')}</th><th class="r">{t('turns.mark.stale')}</th><th class="r">{t('turns.marks.total')}</th></tr></thead>
            <tbody>
              {#each tools as r (r.tool)}
                <tr><td>{toolName(r.tool)}</td><td class="r num">{num(r.right)}</td><td class="r num">{num(r.wrong)}</td><td class="r num">{num(r.stale)}</td><td class="r num">{num(r.right + r.wrong + r.stale)}</td></tr>
              {/each}
            </tbody>
          </table>
        </details>
      {/if}
    {/if}
  </div>
{/if}

<style>
  .ops {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-3);
    margin-bottom: var(--s-4);
  }
  .bad {
    color: var(--danger);
  }
  .chip.warn {
    color: var(--warn);
  }
  .by-tool {
    flex-basis: 100%;
  }
  .by-tool summary {
    cursor: pointer;
    width: fit-content;
  }
  .by-tool table {
    margin-top: var(--s-2);
    max-width: 520px;
  }
</style>
