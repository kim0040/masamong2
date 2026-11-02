<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { usd } from '../lib/format'
  import { batchOffer, type PrepareMode } from '../lib/blocks'
  import Segmented from './Segmented.svelte'
  import Button from './Button.svelte'

  // How to run a preparation: now at the normal price, or as the provider's batch job at about half the
  // price, back within hours up to a day. One dry run (asked with mode=batch) gives both prices. The
  // saved default is the block's own, like its budget.
  let { mode = $bindable<PrepareMode>('live'), est, saved = 'live', onsave, id }: { mode?: PrepareMode; est?: any; saved?: PrepareMode; onsave?: () => void; id: string } = $props()
  const offer = $derived(batchOffer(est))
  const hintId = $derived(id + '-hint')
</script>

{#if offer && !offer.saves}
  <p class="xs muted" id={hintId}>{t('blocks.mode.unavailable')}</p>
{:else}
  <div class="choice">
    <span class="xs muted" id={id + '-label'}>{t('blocks.mode.label')}</span>
    <div class="row">
      <Segmented label={t('blocks.mode.label')} bind:value={mode} size="sm" options={[{ value: 'live', label: t('blocks.mode.live') }, { value: 'batch', label: t('blocks.mode.batch') }]} />
      {#if onsave}<Button size="sm" variant="ghost" onclick={onsave} disabled={mode === saved}>{t('blocks.mode.save_default')}</Button>{/if}
    </div>
    <p class="xs muted" id={hintId} aria-live="polite">
      {#if offer}{t('blocks.mode.prices', { live: usd(offer.live), batch: usd(offer.batch) })} {/if}{mode === 'batch' ? t('blocks.mode.batch_hint') : t('blocks.mode.live_hint')}
    </p>
  </div>
{/if}

<style>
  .choice {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
</style>
