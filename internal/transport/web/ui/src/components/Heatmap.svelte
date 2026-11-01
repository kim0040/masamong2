<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  // weekday × hour. Intensity is also printed in each cell's accessible label.
  let { data, label }: { data: number[][]; label: string } = $props()
  const max = $derived(Math.max(1, ...data.flat()))
  const days = $derived([0, 1, 2, 3, 4, 5, 6].map((d) => t(`weekday.${d}`)))
</script>

<div use:scrollable class="hm" role="table" aria-label={label}>
  <div class="row head" role="row">
    <span role="columnheader"><span class="sr-only">{t('heatmap.day')}</span></span>
    {#each Array(24) as _, h}<span role="columnheader" class="hh">{#if h % 3 === 0}{h}{:else}<span class="sr-only">{h}</span>{/if}</span>{/each}
  </div>
  {#each data as row, d}
    <div class="row" role="row">
      <span role="rowheader" class="dd">{days[d]}</span>
      {#each row as v, h}
        <span role="cell" class="cell" style:--a={v / max} style:--i={d * 24 + h} title="{days[d]} {h}:00 · {v}" aria-label="{days[d]} {h}:00 {v}"></span>
      {/each}
    </div>
  {/each}
</div>

<style>
  .hm {
    display: flex;
    flex-direction: column;
    gap: 3px;
    font-size: 10px;
    color: var(--text-subtle);
    overflow-x: auto;
  }
  .row {
    display: grid;
    grid-template-columns: 28px repeat(24, minmax(10px, 1fr));
    gap: 3px;
    align-items: center;
  }
  .hh,
  .dd {
    text-align: center;
  }
  /* a two-digit hour over a 10px column overflows it instead of breaking into two lines */
  .hh {
    white-space: nowrap;
    overflow: visible;
    min-width: 0;
  }
  .cell {
    aspect-ratio: 1;
    border-radius: 3px;
    background: color-mix(in oklab, var(--chart-1) calc(var(--a) * 100%), var(--surface-2));
    animation: pop 400ms var(--ease) both;
    animation-delay: calc(var(--i) * 2ms);
  }
  @keyframes pop {
    from {
      opacity: 0;
      scale: 0.4;
    }
  }
</style>
