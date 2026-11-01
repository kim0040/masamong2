<script lang="ts">
  import { fly } from 'svelte/transition'
  import { t } from '../lib/i18n.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import { setNavigationGuard } from '../lib/router.svelte'
  import Button from './Button.svelte'
  import Confirm from './Confirm.svelte'
  import * as I from '../lib/icons'
  // Shown while a form has unsaved edits. Leaving the page asks first.
  let { count, onsave, onreset, saving = false, note }: { count: number; onsave: () => void; onreset: () => void; saving?: boolean; note?: string } = $props()
  const d = $derived(motionAllowed() ? 1 : 0)
  // leaving asks in the console's own dialog (a native confirm() looks foreign and is not translated)
  let leaveOpen = $state(false)
  let pending: ((ok: boolean) => void) | null = null
  const ask = () => new Promise<boolean>((resolve) => ((pending = resolve), (leaveOpen = true)))
  $effect(() => {
    if (!leaveOpen && pending) {
      pending(false)
      pending = null
    }
  })
  function leave() {
    pending?.(true)
    pending = null
  }
  $effect(() => {
    if (count > 0) {
      setNavigationGuard(ask)
      return () => {
        setNavigationGuard(null)
      }
    }
  })
</script>

<Confirm bind:open={leaveOpen} title={t('savebar.leave_title')} body={t('savebar.leave')} confirmLabel={t('savebar.leave_confirm')} onconfirm={leave} />
{#if count > 0}
  <div class="sb" role="region" aria-label={t('savebar.label')} in:fly={{ y: 24, duration: 260 * d }} out:fly={{ y: 12, duration: 150 * d }}>
    <span class="dot" aria-hidden="true"></span>
    <span class="txt"><strong>{t('savebar.count', { n: count })}</strong>{#if note}<span class="note">{note}</span>{/if}</span>
    <Button variant="ghost" size="sm" icon={I.Undo2} onclick={onreset} disabled={saving}>{t('savebar.reset')}</Button>
    <Button variant="primary" size="sm" icon={I.Save} onclick={onsave} loading={saving}>{t('savebar.save')}</Button>
  </div>
{/if}

<style>
  .sb {
    position: fixed;
    z-index: 55;
    left: 50%;
    bottom: calc(var(--s-5) + env(safe-area-inset-bottom));
    translate: -50% 0;
    display: flex;
    align-items: center;
    gap: var(--s-2);
    width: max-content;
    max-width: calc(100vw - 32px);
    padding: 8px 8px 8px 16px;
    border-radius: 999px;
    background: var(--surface);
    box-shadow: var(--shadow-pop);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 999px;
    background: var(--accent);
    animation: pulse-dot 1.6s var(--ease) infinite;
  }
  @keyframes pulse-dot {
    50% {
      scale: 1.4;
      opacity: 0.6;
    }
  }
  .txt {
    display: flex;
    flex-direction: column;
    font-size: var(--text-sm);
    margin-inline-end: var(--s-3);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
