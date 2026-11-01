<script lang="ts">
  import type { Snippet } from 'svelte'
  // A native checkbox with role="switch": keyboard, label and form behavior come from the platform.
  let { checked = $bindable(false), label, hint, disabled = false, onchange, children, ariaLabel }: { checked?: boolean; label?: string; hint?: string; disabled?: boolean; onchange?: (v: boolean) => void; children?: Snippet; ariaLabel?: string } = $props()
  const uid = $props.id()
</script>

<label class="sw" class:disabled>
  <span class="text">
    {#if label}<span class="lbl">{label}</span>{/if}
    {@render children?.()}
    {#if hint}<span class="hint" id="{uid}-h">{hint}</span>{/if}
  </span>
  <input type="checkbox" role="switch" bind:checked {disabled} aria-label={ariaLabel} aria-describedby={hint ? `${uid}-h` : undefined} onchange={() => onchange?.(checked)} />
  <span class="track" aria-hidden="true"><span class="thumb"></span></span>
</label>

<style>
  .sw {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    cursor: pointer;
    min-height: calc(var(--control-h) - 4px);
  }
  .disabled {
    cursor: not-allowed;
    opacity: 0.6;
  }
  .text {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .lbl {
    font-weight: 550;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  input {
    position: absolute;
    opacity: 0;
    width: 1px;
    height: 1px;
  }
  .track {
    position: relative;
    flex: none;
    width: 38px;
    height: 22px;
    border-radius: 999px;
    background: var(--surface-3);
    box-shadow: inset 0 0 0 1px var(--border-strong);
    transition-property: background-color, box-shadow;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
  }
  .thumb {
    position: absolute;
    top: 3px;
    inset-inline-start: 3px;
    width: 16px;
    height: 16px;
    border-radius: 999px;
    background: #fff;
    box-shadow: 0 1px 2px rgb(0 0 0 / 0.25);
    transition-property: translate, width;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
  }
  input:checked + .track {
    background: var(--accent);
    box-shadow: none;
  }
  input:checked + .track .thumb {
    translate: 16px 0;
  }
  :global([dir='rtl']) input:checked + .track .thumb {
    translate: -16px 0;
  }
  .sw:active .thumb {
    width: 20px;
  }
  input:checked + .track .thumb {
    width: 16px;
  }
  input:focus-visible + .track {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
</style>
