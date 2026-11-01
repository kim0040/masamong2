<script lang="ts">
  import { Tween } from 'svelte/motion'
  import { cubicOut } from 'svelte/easing'
  import { motionAllowed } from '../lib/prefs.svelte'
  // A gauge for "x of y" (budget, quota). The number is printed inside; color follows the level.
  let { value, max, label, center, sub, size = 132, warnAt = 0.8 }: { value: number; max: number; label: string; center: string; sub?: string; size?: number; warnAt?: number } = $props()
  const ratio = $derived(max > 0 ? Math.min(1, value / max) : 0)
  const tone = $derived(ratio >= 1 ? 'var(--danger)' : ratio >= warnAt ? 'var(--warn)' : 'var(--accent)')
  const tw = new Tween(0, { duration: 900, easing: cubicOut })
  $effect(() => {
    tw.set(ratio, { duration: motionAllowed() ? 900 : 0 })
  })
  const R = 52
  const C = 2 * Math.PI * R
</script>

<div class="ring" style:width="{size}px" style:height="{size}px" role="img" aria-label="{label}: {center}{sub ? ' ' + sub : ''}">
  <svg viewBox="0 0 120 120" aria-hidden="true">
    <circle cx="60" cy="60" r={R} class="bg" />
    <circle cx="60" cy="60" r={R} class="fg" style:stroke={tone} stroke-dasharray={C} stroke-dashoffset={C * (1 - tw.current)} />
  </svg>
  <div class="c">
    <strong class="num">{center}</strong>
    {#if sub}<span>{sub}</span>{/if}
  </div>
</div>

<style>
  .ring {
    position: relative;
    flex: none;
  }
  svg {
    width: 100%;
    height: 100%;
    rotate: -90deg;
  }
  circle {
    fill: none;
    stroke-width: 10;
  }
  .bg {
    stroke: var(--surface-3);
  }
  .fg {
    stroke-linecap: round;
  }
  .c {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    text-align: center;
  }
  strong {
    font-size: var(--text-xl);
    font-weight: 700;
    letter-spacing: -0.02em;
  }
  span {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
