<script lang="ts">
  import { flip } from 'svelte/animate'
  import { fly, fade } from 'svelte/transition'
  import { toasts, dismiss } from '../lib/toast.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import { CircleCheck, CircleAlert, Info, TriangleAlert, X } from '../lib/icons'
  import { t } from '../lib/i18n.svelte'
  const icons = { ok: CircleCheck, error: CircleAlert, info: Info, warn: TriangleAlert }
  const d = $derived(motionAllowed() ? 1 : 0)
</script>

<div class="toaster" role="status" aria-live="polite">
  {#each toasts as ts (ts.id)}
    {@const Icon = icons[ts.kind]}
    <div class="toast {ts.kind}" animate:flip={{ duration: 220 * d }} in:fly={{ y: 16, duration: 260 * d }} out:fade={{ duration: 150 * d }}>
      <span class="ic"><Icon size={18} aria-hidden="true" /></span>
      <div class="txt">
        <strong>{ts.title}</strong>
        {#if ts.body}<p>{ts.body}</p>{/if}
        {#if ts.action}<button type="button" class="act" onclick={() => { ts.action?.run(); dismiss(ts.id) }}>{ts.action.label}</button>{/if}
      </div>
      <button type="button" class="x" aria-label={t('common.dismiss')} onclick={() => dismiss(ts.id)}><X size={16} aria-hidden="true" /></button>
    </div>
  {/each}
</div>

<style>
  .toaster {
    position: fixed;
    z-index: 80;
    inset-inline-end: var(--s-4);
    bottom: calc(var(--s-4) + env(safe-area-inset-bottom));
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    width: min(380px, calc(100vw - 32px));
    pointer-events: none;
  }
  .toast {
    pointer-events: auto;
    display: flex;
    gap: var(--s-3);
    align-items: flex-start;
    padding: 12px 12px 12px 14px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
  }
  .ic {
    display: flex;
    padding-top: 1px;
  }
  .ok .ic {
    color: var(--ok);
  }
  .error .ic {
    color: var(--danger);
  }
  .warn .ic {
    color: var(--warn);
  }
  .info .ic {
    color: var(--info);
  }
  .txt {
    flex: 1;
    min-width: 0;
    font-size: var(--text-sm);
  }
  .txt p {
    color: var(--text-muted);
    margin-top: 2px;
    overflow-wrap: anywhere;
  }
  .act {
    margin-top: 6px;
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-text);
    font-weight: 600;
    cursor: pointer;
  }
  .x {
    display: grid;
    place-items: center;
    width: 28px;
    height: 28px;
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .x:hover {
    background: var(--surface-2);
  }
</style>
