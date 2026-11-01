<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { ArrowUp, ArrowDown, EyeOff, GripVertical } from '../lib/icons'
  // Edit-mode controls on a Home card: drag by the handle, or move with the arrow buttons (keyboard),
  // or hide it. Hidden cards come back from the "hidden cards" bar.
  let { name, first, last, onmove, onhide, ondragstart, ondragend }: {
    name: string; first: boolean; last: boolean; onmove: (dir: -1 | 1) => void; onhide: () => void; ondragstart: () => void; ondragend: () => void
  } = $props()
</script>

<div class="ct" role="toolbar" aria-label={t('home.edit.card', { name })}>
  <span class="grip" draggable="true" role="img" aria-label={t('home.edit.drag')} ondragstart={(e) => { e.dataTransfer?.setData('text/plain', name); ondragstart() }} {ondragend}><GripVertical size={16} aria-hidden="true" /></span>
  <button type="button" aria-label={t('home.edit.earlier', { name })} disabled={first} onclick={() => onmove(-1)}><ArrowUp size={15} aria-hidden="true" /></button>
  <button type="button" aria-label={t('home.edit.later', { name })} disabled={last} onclick={() => onmove(1)}><ArrowDown size={15} aria-hidden="true" /></button>
  <button type="button" aria-label={t('home.edit.hide', { name })} onclick={onhide}><EyeOff size={15} aria-hidden="true" /></button>
</div>

<style>
  .ct {
    position: absolute;
    top: 10px;
    inset-inline-end: 10px;
    z-index: 2;
    display: flex;
    gap: 2px;
    padding: 3px;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
    animation: ct-in 200ms var(--ease) both;
  }
  @keyframes ct-in {
    from {
      opacity: 0;
      scale: 0.9;
    }
  }
  button,
  .grip {
    display: grid;
    place-items: center;
    width: 30px;
    height: 30px;
    border: 0;
    border-radius: calc(var(--r) - 3px);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-out), scale var(--dur-fast) var(--ease-out);
  }
  .grip {
    cursor: grab;
  }
  button:hover:not(:disabled),
  .grip:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  button:active:not(:disabled) {
    scale: 0.96;
  }
  button:disabled {
    opacity: 0.35;
    cursor: default;
  }
</style>
