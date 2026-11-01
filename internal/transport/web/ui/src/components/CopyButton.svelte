<script lang="ts">
  import { Copy, Check } from '../lib/icons'
  import { t } from '../lib/i18n.svelte'
  let { text, label }: { text: string; label?: string } = $props()
  let done = $state(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      done = true
      setTimeout(() => (done = false), 1400)
    } catch {}
  }
</script>

<button type="button" class="cp" onclick={copy} aria-label={label ?? t('common.copy')} title={label ?? t('common.copy')}>
  <span class="icons" aria-hidden="true">
    <span class="i" class:on={!done}><Copy size={14} /></span>
    <span class="i" class:on={done}><Check size={14} /></span>
  </span>
  <span class="sr-only" role="status">{done ? t('common.copied') : ''}</span>
</button>

<style>
  .cp {
    display: inline-grid;
    place-items: center;
    width: 28px;
    height: 28px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
  }
  .cp:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .icons {
    position: relative;
    display: grid;
    width: 14px;
    height: 14px;
  }
  .i {
    position: absolute;
    inset: 0;
    display: grid;
    opacity: 0;
    scale: 0.25;
    filter: blur(4px);
    transition-property: opacity, scale, filter;
    transition-duration: var(--dur);
    transition-timing-function: cubic-bezier(0.2, 0, 0, 1);
  }
  .i.on {
    opacity: 1;
    scale: 1;
    filter: blur(0);
  }
</style>
