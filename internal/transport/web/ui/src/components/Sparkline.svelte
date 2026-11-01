<script lang="ts">
  let { data, height = 36, label, color = 'var(--chart-1)' }: { data: number[]; height?: number; label: string; color?: string } = $props()
  const W = 120
  const max = $derived(Math.max(1, ...data))
  const pts = $derived(data.map((v, i) => [data.length < 2 ? W : (i * W) / (data.length - 1), height - 3 - ((height - 6) * v) / max]))
  const line = $derived(pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(''))
  const area = $derived(pts.length ? `${line}L${W},${height}L0,${height}Z` : '')
  const gid = $props.id()
</script>

<svg class="spark" viewBox="0 0 {W} {height}" preserveAspectRatio="none" role="img" aria-label={label} style:height="{height}px">
  <defs>
    <linearGradient id={gid} x1="0" x2="0" y1="0" y2="1">
      <stop offset="0%" stop-color={color} stop-opacity="0.28" />
      <stop offset="100%" stop-color={color} stop-opacity="0" />
    </linearGradient>
  </defs>
  {#if pts.length}
    <path d={area} fill="url(#{gid})" />
    <path class="ln" d={line} fill="none" stroke={color} stroke-width="1.75" vector-effect="non-scaling-stroke" stroke-linejoin="round" stroke-linecap="round" pathLength="1" />
  {/if}
</svg>

<style>
  .spark {
    display: block;
    width: 100%;
    overflow: visible;
  }
  .ln {
    stroke-dasharray: 1;
    stroke-dashoffset: 0;
    animation: draw 900ms var(--ease) both;
  }
  @keyframes draw {
    from {
      stroke-dashoffset: 1;
    }
  }
</style>
