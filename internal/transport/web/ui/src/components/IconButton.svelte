<script lang="ts">
  import type { Component } from 'svelte'
  import type { HTMLButtonAttributes } from 'svelte/elements'

  type Props = HTMLButtonAttributes & { icon: Component<any>; label: string; size?: number; active?: boolean; href?: string; tone?: 'default' | 'danger' }
  let { icon: Icon, label, size = 18, active = false, href, tone = 'default', class: cls = '', ...rest }: Props = $props()
</script>

{#if href}
  <a class="ib {tone} {cls}" class:active {href} aria-label={label} title={label} {...rest as any}><Icon {size} strokeWidth={1.75} aria-hidden="true" /></a>
{:else}
  <button type="button" class="ib {tone} {cls}" class:active aria-label={label} title={label} {...rest}><Icon {size} strokeWidth={1.75} aria-hidden="true" /></button>
{/if}

<style>
  .ib {
    position: relative;
    display: inline-grid;
    place-items: center;
    width: var(--control-h);
    height: var(--control-h);
    border: 0;
    border-radius: var(--r);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    transition-property: background-color, color, scale;
    transition-duration: var(--dur-fast);
    transition-timing-function: var(--ease-out);
  }
  .ib::after {
    content: '';
    position: absolute;
    inset: -4px;
  }
  .ib:active:not(:disabled) {
    scale: 0.96;
  }
  .ib:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  .active {
    background: var(--surface-2);
    color: var(--text);
  }
  .danger {
    color: var(--danger);
  }
  @media (hover: hover) {
    .ib:hover:not(:disabled) {
      background: var(--surface-2);
      color: var(--text);
    }
    .danger:hover:not(:disabled) {
      background: var(--danger-soft);
      color: var(--danger);
    }
  }
</style>
