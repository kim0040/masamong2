<script lang="ts">
  import type { HTMLSelectAttributes } from 'svelte/elements'
  import { ChevronDown } from '../lib/icons'
  let { value = $bindable(), options, invalid = false, ...rest }: HTMLSelectAttributes & { options: { value: string; label: string; disabled?: boolean; group?: string }[]; invalid?: boolean } = $props()
</script>

<span class="wrap">
  <select class="sel" bind:value aria-invalid={invalid || undefined} {...rest}>
    {#each options.filter((o) => !o.group) as o (o.value)}<option value={o.value} disabled={o.disabled}>{o.label}</option>{/each}
    {#each [...new Set(options.map((o) => o.group).filter(Boolean))] as g (g)}
      <optgroup label={g}>{#each options.filter((o) => o.group === g) as o (o.value)}<option value={o.value} disabled={o.disabled}>{o.label}</option>{/each}</optgroup>
    {/each}
  </select>
  <span class="chev" aria-hidden="true"><ChevronDown size={16} /></span>
</span>

<style>
  .wrap {
    position: relative;
    display: inline-flex;
    width: 100%;
    min-width: 0;
  }
  .sel {
    appearance: none;
    width: 100%;
    min-height: var(--control-h);
    padding: 0 36px 0 12px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text);
    font-size: var(--text-base);
    cursor: pointer;
    text-overflow: ellipsis;
  }
  .sel:hover:not(:disabled) {
    box-shadow: var(--shadow-border-hover);
  }
  .sel:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
  .chev {
    position: absolute;
    inset-inline-end: 10px;
    top: 50%;
    translate: 0 -50%;
    pointer-events: none;
    color: var(--text-muted);
    display: inline-flex;
  }
  @media (max-width: 640px) {
    .sel {
      font-size: 16px;
    }
  }
</style>
