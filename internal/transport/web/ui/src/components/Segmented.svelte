<script lang="ts" generics="V extends string">
  import type { Component } from 'svelte'
  // Radio group styled as a segmented control; native radios give arrow-key navigation for free.
  let { value = $bindable(), options, label, onchange, size = 'md' }: { value: V; options: { value: V; label: string; icon?: Component<any> }[]; label: string; onchange?: (v: V) => void; size?: 'sm' | 'md' } = $props()
  const name = $props.id()
  const index = $derived(Math.max(0, options.findIndex((o) => o.value === value)))
</script>

<div class="seg {size}" role="radiogroup" aria-label={label} style:--n={options.length} style:--i={index}>
  <span class="pill" aria-hidden="true"></span>
  {#each options as o (o.value)}
    <label class:on={o.value === value}>
      <input type="radio" {name} value={o.value} checked={o.value === value} onchange={() => { value = o.value; onchange?.(o.value) }} />
      {#if o.icon}<o.icon size={14} strokeWidth={2} aria-hidden="true" />{/if}
      <span>{o.label}</span>
    </label>
  {/each}
</div>

<style>
  .seg {
    position: relative;
    max-width: 100%;
    overflow-x: auto;
    scrollbar-width: thin;
    display: inline-grid;
    grid-auto-flow: column;
    grid-auto-columns: minmax(0, 1fr);
    padding: 3px;
    border-radius: var(--r);
    background: var(--surface-2);
    box-shadow: inset 0 0 0 1px var(--border);
  }
  .pill {
    position: absolute;
    top: 3px;
    bottom: 3px;
    inset-inline-start: 3px;
    width: calc((100% - 6px) / var(--n));
    border-radius: calc(var(--r) - 3px);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    translate: calc(100% * var(--i)) 0;
    transition: translate var(--dur) var(--ease);
  }
  :global([dir='rtl']) .pill {
    translate: calc(-100% * var(--i)) 0;
  }
  label {
    position: relative;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    min-height: calc(var(--control-h) - 6px);
    padding: 0 12px;
    border-radius: calc(var(--r) - 3px);
    font-size: var(--text-sm);
    font-weight: 550;
    color: var(--text-muted);
    cursor: pointer;
    /* wide enough: one line; squeezed (a phone), long labels wrap instead of hiding the last option */
    text-align: center;
    line-height: 1.25;
    padding-block: 3px;
    overflow-wrap: anywhere;
    transition: color var(--dur-fast) var(--ease-out);
  }
  .sm label {
    min-height: 24px;
    padding: 0 8px;
    font-size: var(--text-xs);
  }
  label.on {
    color: var(--text);
  }
  input {
    position: absolute;
    opacity: 0;
    width: 1px;
    height: 1px;
  }
  label:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
</style>
