<script lang="ts">
  let { value, max = 100, label, tone = 'accent' }: { value?: number; max?: number; label: string; tone?: 'accent' | 'ok' | 'warn' | 'danger' } = $props()
  const pct = $derived(value === undefined ? undefined : Math.max(0, Math.min(100, (100 * value) / (max || 1))))
</script>

<div class="pg {tone}" role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={max} aria-valuenow={value} class:indet={pct === undefined}>
  <span class="fill" style:width={pct === undefined ? undefined : `${pct}%`}></span>
</div>

<style>
  .pg {
    position: relative;
    height: 6px;
    border-radius: 999px;
    background: var(--surface-3);
    overflow: hidden;
  }
  .fill {
    position: absolute;
    inset: 0 auto 0 0;
    border-radius: inherit;
    background: var(--accent);
    transition: width 500ms var(--ease);
  }
  .ok .fill {
    background: var(--ok);
  }
  .warn .fill {
    background: var(--warn);
  }
  .danger .fill {
    background: var(--danger);
  }
  .indet .fill {
    width: 35%;
    animation: indet 1.2s var(--ease) infinite;
  }
  @keyframes indet {
    from {
      translate: -100% 0;
    }
    to {
      translate: 300% 0;
    }
  }
</style>
