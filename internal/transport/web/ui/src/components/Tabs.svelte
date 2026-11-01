<script lang="ts">
  import type { Component } from 'svelte'
  // ARIA tabs with roving tabindex and a sliding indicator. Panels are rendered by the parent with
  // id="{id}-panel-{tab}" and aria-labelledby="{id}-tab-{tab}".
  let { tabs, active = $bindable(), id, onchange }: { tabs: { id: string; label: string; icon?: Component<any>; count?: number }[]; active: string; id: string; onchange?: (t: string) => void } = $props()
  let list: HTMLDivElement | undefined = $state()
  let ind = $state({ x: 0, w: 0, ready: false })

  function select(t: string, focus = false) {
    active = t
    onchange?.(t)
    if (focus) (list?.querySelector(`[data-tab="${CSS.escape(t)}"]`) as HTMLElement | null)?.focus()
  }
  function key(e: KeyboardEvent) {
    const i = tabs.findIndex((t) => t.id === active)
    let n = -1
    if (e.key === 'ArrowRight') n = (i + 1) % tabs.length
    else if (e.key === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length
    else if (e.key === 'Home') n = 0
    else if (e.key === 'End') n = tabs.length - 1
    if (n >= 0) {
      e.preventDefault()
      select(tabs[n].id, true)
    }
  }
  $effect(() => {
    void active
    void tabs.length
    const el = list?.querySelector(`[data-tab="${CSS.escape(active)}"]`) as HTMLElement | null
    if (el) {
      ind = { x: el.offsetLeft, w: el.offsetWidth, ready: true }
      el.scrollIntoView({ block: 'nearest', inline: 'nearest' })
    }
  })
</script>

<div class="tabs" role="tablist" bind:this={list} onkeydown={key} tabindex="-1">
  {#each tabs as t (t.id)}
    <button
      type="button"
      role="tab"
      id="{id}-tab-{t.id}"
      data-tab={t.id}
      aria-selected={t.id === active}
      aria-controls="{id}-panel-{t.id}"
      tabindex={t.id === active ? 0 : -1}
      onclick={() => select(t.id)}
    >
      {#if t.icon}<t.icon size={15} strokeWidth={1.75} aria-hidden="true" />{/if}
      {t.label}
      {#if t.count !== undefined}<span class="count num">{t.count}</span>{/if}
    </button>
  {/each}
  <span class="ind" class:ready={ind.ready} aria-hidden="true" style:translate="{ind.x}px 0" style:width="{ind.w}px"></span>
</div>

<style>
  .tabs {
    position: relative;
    display: flex;
    gap: 2px;
    overflow-x: auto;
    scrollbar-width: thin;
    box-shadow: inset 0 -1px 0 var(--border);
    outline: none;
  }
  button {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: calc(var(--control-h) + 4px);
    padding: 0 12px;
    border: 0;
    background: none;
    color: var(--text-muted);
    font-weight: 550;
    font-size: var(--text-base);
    white-space: nowrap;
    cursor: pointer;
    border-radius: var(--r) var(--r) 0 0;
    transition: color var(--dur-fast) var(--ease-out), background-color var(--dur-fast) var(--ease-out);
  }
  button[aria-selected='true'] {
    color: var(--text);
  }
  @media (hover: hover) {
    button:hover {
      color: var(--text);
      background: var(--surface-2);
    }
  }
  button:focus-visible {
    outline-offset: -2px;
  }
  .count {
    min-width: 20px;
    padding: 0 6px;
    border-radius: 999px;
    background: var(--surface-3);
    font-size: var(--text-xs);
    line-height: 18px;
    text-align: center;
  }
  .ind {
    position: absolute;
    left: 0;
    bottom: 0;
    height: 2px;
    border-radius: 2px;
    background: var(--accent);
    opacity: 0;
  }
  .ind.ready {
    opacity: 1;
    transition-property: translate, width;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
  }
</style>
