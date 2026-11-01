<script lang="ts">
  import type { Component, Snippet } from 'svelte'
  import type { HTMLButtonAttributes } from 'svelte/elements'
  import { LoaderCircle } from '../lib/icons'

  type Props = HTMLButtonAttributes & {
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger' | 'danger-ghost'
    size?: 'sm' | 'md' | 'lg'
    loading?: boolean
    href?: string
    target?: string // with href: '_blank' opens a new tab (the router leaves such links alone)
    rel?: string
    icon?: Component<any>
    iconEnd?: Component<any>
    static?: boolean
    full?: boolean
    children?: Snippet
  }
  let { variant = 'secondary', size = 'md', loading = false, disabled = false, href, icon: Icon, iconEnd: IconEnd, static: isStatic = false, full = false, type = 'button', children, onclick, ...rest }: Props = $props()
  const iconSize = $derived(size === 'sm' ? 14 : 16)
</script>

{#if href}
  <a class="btn {variant} {size}" class:static={isStatic} class:full {href} {...rest as any} {onclick}>
    {#if Icon}<Icon size={iconSize} strokeWidth={2} aria-hidden="true" />{/if}
    {@render children?.()}
    {#if IconEnd}<IconEnd size={iconSize} strokeWidth={2} aria-hidden="true" />{/if}
  </a>
{:else}
  <!-- While loading the button is only marked busy, not disabled: a disabled button drops the keyboard focus, and the
       focus would then not come back to it when the dialog or confirmation opened by this click is closed again -->
  <button class="btn {variant} {size}" class:static={isStatic} class:full {type} {disabled} aria-busy={loading || undefined} aria-disabled={loading || undefined} {...rest} onclick={(e) => (loading ? e.preventDefault() : onclick?.(e))}>
    {#if loading}<span class="spin" aria-hidden="true"><LoaderCircle size={iconSize} /></span>{:else if Icon}<Icon size={iconSize} strokeWidth={2} aria-hidden="true" />{/if}
    {@render children?.()}
    {#if IconEnd && !loading}<IconEnd size={iconSize} strokeWidth={2} aria-hidden="true" />{/if}
  </button>
{/if}

<style>
  .btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--s-2);
    min-height: var(--control-h);
    padding: 0 14px;
    border: 0;
    border-radius: var(--r);
    font-size: var(--text-base);
    font-weight: 550;
    line-height: 1;
    white-space: nowrap;
    text-decoration: none;
    cursor: pointer;
    transition-property: scale, background-color, box-shadow, color, opacity;
    transition-duration: var(--dur-fast);
    transition-timing-function: var(--ease-out);
    user-select: none;
    -webkit-tap-highlight-color: transparent;
  }
  .btn:active:not(:disabled):not([aria-busy='true']):not(.static) {
    scale: 0.96;
  }
  .btn:disabled {
    cursor: not-allowed;
    opacity: 0.55;
  }
  .btn[aria-busy='true'] {
    opacity: 0.85;
    cursor: progress;
  }
  .full {
    width: 100%;
  }
  .sm {
    min-height: 28px;
    padding: 0 10px;
    font-size: var(--text-sm);
    border-radius: var(--r-sm);
    gap: 6px;
  }
  .lg {
    min-height: calc(var(--control-h) + 8px);
    padding: 0 20px;
    font-size: var(--text-md);
    border-radius: var(--r-lg);
  }
  .primary {
    background: var(--accent);
    color: var(--on-accent);
  }
  .secondary {
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-border);
  }
  .ghost {
    background: transparent;
    color: var(--text);
  }
  .danger {
    background: var(--danger-solid);
    color: var(--on-danger);
  }
  .danger-ghost {
    background: transparent;
    color: var(--danger);
  }
  @media (hover: hover) {
    .primary:hover:not(:disabled):not([aria-busy='true']) {
      background: var(--accent-hover);
    }
    .secondary:hover:not(:disabled):not([aria-busy='true']) {
      box-shadow: var(--shadow-border-hover);
      background: var(--surface-2);
    }
    .ghost:hover:not(:disabled):not([aria-busy='true']) {
      background: var(--surface-2);
    }
    .danger:hover:not(:disabled):not([aria-busy='true']) {
      filter: brightness(1.08);
    }
    .danger-ghost:hover:not(:disabled):not([aria-busy='true']) {
      background: var(--danger-soft);
    }
  }
  .spin {
    display: inline-flex;
    animation: spin 0.9s linear infinite;
  }
</style>
