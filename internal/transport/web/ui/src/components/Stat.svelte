<script lang="ts">
  import type { Snippet } from 'svelte'
  import { Tween } from 'svelte/motion'
  import { cubicOut } from 'svelte/easing'
  import { motionAllowed } from '../lib/prefs.svelte'
  // A KPI whose number counts to its new value.
  let { label, value, format = (n: number) => String(Math.round(n)), hint, children, tone }: { label: string; value: number | undefined; format?: (n: number) => string; hint?: string; children?: Snippet; tone?: 'ok' | 'warn' | 'danger' } = $props()
  const tw = new Tween(0, { duration: 700, easing: cubicOut })
  $effect(() => {
    if (value === undefined) return
    tw.set(value, { duration: motionAllowed() ? 700 : 0 })
  })
</script>

<div class="stat">
  <div class="label">{label}</div>
  <div class="value num {tone ?? ''}">{value === undefined ? '–' : format(tw.current)}</div>
  {#if hint}<div class="hint">{hint}</div>{/if}
  {@render children?.()}
</div>

<style>
  .stat {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .label {
    font-size: var(--text-sm);
    color: var(--text-muted);
    font-weight: 550;
  }
  .value {
    font-size: var(--text-3xl);
    font-weight: 700;
    letter-spacing: -0.03em;
    line-height: 1.1;
  }
  .ok {
    color: var(--ok);
  }
  .warn {
    color: var(--warn);
  }
  .danger {
    color: var(--danger);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
