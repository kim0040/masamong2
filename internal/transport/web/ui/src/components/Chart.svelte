<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { num } from '../lib/format'
  // Stacked bars or lines over days. Hover or arrow keys show the day's values; "table" gives the same
  // numbers as a data table for screen readers and copying. gaps: a 0 on a line means "no data that day"
  // (an average over no replies), so the line breaks there instead of dropping to zero.
  type Series = { key: string; label: string; data: number[]; color: string }
  let { labels, series, kind = 'bars', height = 220, label, format = (n: number) => num(n), stacked = true, gaps = false }: {
    labels: string[]; series: Series[]; kind?: 'bars' | 'lines'; height?: number; label: string; format?: (n: number) => string; stacked?: boolean; gaps?: boolean
  } = $props()

  let w = $state(600)
  let hover = $state<number | null>(null)
  let showTable = $state(false)
  const padL = 44
  const padB = 24
  const padT = 8
  const n = $derived(labels.length)
  const totals = $derived(labels.map((_, i) => (stacked ? series.reduce((a, s) => a + (s.data[i] ?? 0), 0) : Math.max(0, ...series.map((s) => s.data[i] ?? 0)))))
  const rawMax = $derived(Math.max(0, ...totals))
  const niceMax = $derived(nice(rawMax))
  const plotW = $derived(Math.max(10, w - padL - 8))
  const plotH = $derived(height - padB - padT)
  const bw = $derived(plotW / Math.max(1, n))
  const y = (v: number) => padT + plotH - (plotH * v) / (niceMax || 1)
  // five gridlines, but never two with the same printed label (small ranges collapse to integers)
  const ticks = $derived.by(() => {
    const seen = new Set<string>()
    return [0, 0.25, 0.5, 0.75, 1].map((f) => f * niceMax).filter((v) => {
      const k = format(v)
      if (seen.has(k)) return false
      seen.add(k)
      return true
    })
  })
  const empty = $derived(rawMax === 0)
  const labelEvery = $derived(Math.max(1, Math.ceil(n / Math.max(2, Math.floor(plotW / 56)))))

  function nice(v: number) {
    if (v <= 0) return 1
    const p = Math.pow(10, Math.floor(Math.log10(v)))
    const m = v / p
    return (m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10) * p
  }
  function linePath(s: Series) {
    let pen = false
    return s.data
      .map((v, i) => {
        if (gaps && !v) return ((pen = false), '')
        const seg = `${pen ? 'L' : 'M'}${(padL + bw * i + bw / 2).toFixed(1)},${y(v).toFixed(1)}`
        pen = true
        return seg
      })
      .join('')
  }
  function key(e: KeyboardEvent) {
    if (e.key === 'ArrowRight') hover = Math.min(n - 1, (hover ?? -1) + 1)
    else if (e.key === 'ArrowLeft') hover = Math.max(0, (hover ?? n) - 1)
    else if (e.key === 'Escape') hover = null
    else return
    e.preventDefault()
  }
  function move(e: PointerEvent) {
    const r = (e.currentTarget as SVGElement).getBoundingClientRect()
    const x = e.clientX - r.left - padL
    hover = x < 0 ? null : Math.min(n - 1, Math.max(0, Math.floor(x / bw)))
  }
</script>

<div class="chart">
  <div class="legend">
    {#each series as s (s.key)}<span class="lg"><i style:background={s.color}></i>{s.label}</span>{/each}
    <button type="button" class="tbl" onclick={() => (showTable = !showTable)} aria-pressed={showTable}>{showTable ? t('chart.show_chart') : t('chart.show_table')}</button>
  </div>
  {#if showTable}
    <div class="table-wrap">
      <table class="table">
        <caption class="sr-only">{label}</caption>
        <thead><tr><th scope="col">{t('chart.day')}</th>{#each series as s}<th scope="col" class="r">{s.label}</th>{/each}</tr></thead>
        <tbody>
          {#each labels as l, i}<tr><th scope="row">{l}</th>{#each series as s}<td class="r num">{format(s.data[i] ?? 0)}</td>{/each}</tr>{/each}
        </tbody>
      </table>
    </div>
  {:else}
    <div class="plot" bind:clientWidth={w}>
      <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions (keyboard users get the same values from the table view) -->
      <svg width={w} {height} role="img" aria-label={label} tabindex="0" onkeydown={key} onpointermove={move} onpointerleave={() => (hover = null)}>
        {#each ticks as tk}
          <line x1={padL} x2={w - 8} y1={y(tk)} y2={y(tk)} class="grid" />
          <text x={padL - 8} y={y(tk)} class="axis" text-anchor="end" dominant-baseline="middle">{tk === 0 ? '0' : format(tk)}</text>
        {/each}
        {#each labels as l, i}
          <!-- counted from the end, so the latest day (today) always has its label -->
          {#if (n - 1 - i) % labelEvery === 0}<text x={padL + bw * i + bw / 2} y={height - 6} class="axis" text-anchor="middle">{l}</text>{/if}
        {/each}
        {#if empty}<text x={padL + plotW / 2} y={padT + plotH / 2} class="none" text-anchor="middle" dominant-baseline="middle">{t('chart.no_data')}</text>{/if}
        {#if hover !== null}<rect x={padL + bw * hover} y={padT} width={bw} height={plotH} class="hov" />{/if}
        {#if kind === 'bars'}
          {#each labels as _, i}
            {@const gap = Math.min(6, bw * 0.25)}
            <g class="col" style:--d="{i * 18}ms">
              {#each series as s, si}
                {@const below = stacked ? series.slice(0, si).reduce((a, x) => a + (x.data[i] ?? 0), 0) : 0}
                {@const v = s.data[i] ?? 0}
                {#if v > 0}
                  <rect x={padL + bw * i + gap / 2 + (stacked ? 0 : ((bw - gap) / series.length) * si)} width={stacked ? bw - gap : (bw - gap) / series.length}
                    y={y(below + v)} height={Math.max(0, y(below) - y(below + v))} rx="3" fill={s.color} />
                {/if}
              {/each}
            </g>
          {/each}
        {:else}
          {#each empty ? [] : series as s (s.key)}
            <path d={linePath(s)} fill="none" stroke={s.color} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" class="ln" pathLength="1" />
            <!-- a day with no neighbour on either side draws no line: a dot shows it -->
            {#each s.data as v, i}
              {#if (!gaps || v) && (gaps ? !s.data[i - 1] && !s.data[i + 1] : n === 1)}<circle cx={padL + bw * i + bw / 2} cy={y(v ?? 0)} r="3.5" fill={s.color} />{/if}
            {/each}
            {#if hover !== null && !(gaps && !s.data[hover])}<circle cx={padL + bw * hover + bw / 2} cy={y(s.data[hover] ?? 0)} r="4" fill={s.color} stroke="var(--surface)" stroke-width="2" />{/if}
          {/each}
        {/if}
      </svg>
      {#if hover !== null}
        <div class="tip" style:left="{Math.min(w - 180, Math.max(0, padL + bw * hover + bw / 2 - 90))}px" aria-live="polite">
          <strong>{labels[hover]}</strong>
          {#each series as s}<div class="trow"><i style:background={s.color}></i><span>{s.label}</span><b class="num">{format(s.data[hover] ?? 0)}</b></div>{/each}
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .chart {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    min-width: 0;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 14px;
    align-items: center;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .lg {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .lg i,
  .trow i {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    display: inline-block;
  }
  .tbl {
    margin-inline-start: auto;
    padding: 2px 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .tbl:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .plot {
    position: relative;
    width: 100%;
  }
  svg {
    display: block;
    overflow: visible;
    outline-offset: 4px;
    touch-action: pan-y;
  }
  .grid {
    stroke: var(--chart-grid);
    stroke-width: 1;
  }
  .axis {
    fill: var(--text-subtle);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
  }
  .hov {
    fill: var(--surface-2);
  }
  .none {
    fill: var(--text-muted);
    font-size: 13px;
  }
  .col {
    transform-box: fill-box;
    transform-origin: bottom;
    animation: grow 600ms var(--ease) both;
    animation-delay: var(--d);
  }
  @keyframes grow {
    from {
      transform: scaleY(0);
      opacity: 0;
    }
  }
  .ln {
    stroke-dasharray: 1;
    animation: draw 900ms var(--ease) both;
  }
  @keyframes draw {
    from {
      stroke-dashoffset: 1;
    }
  }
  .tip {
    position: absolute;
    top: 0;
    width: 180px;
    padding: 8px 10px;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
    font-size: var(--text-xs);
    pointer-events: none;
    z-index: 2;
  }
  .trow {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: 3px;
  }
  .trow span {
    flex: 1;
    color: var(--text-muted);
  }
  .table-wrap {
    max-height: 280px;
    overflow: auto;
  }
  .r {
    text-align: end;
  }
</style>
