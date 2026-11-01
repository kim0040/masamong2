<script lang="ts">
  import Dialog from './Dialog.svelte'
  import Button from './Button.svelte'
  import Input from './Input.svelte'
  import { t } from '../lib/i18n.svelte'
  // Confirmation for writes that cost money, send messages or delete. The confirm button repeats the
  // consequence; destructive ones can require typing a word.
  let { open = $bindable(false), title, body, confirmLabel, danger = false, requireText, onconfirm, details }: {
    open?: boolean; title: string; body?: string; confirmLabel: string; danger?: boolean; requireText?: string; onconfirm: () => Promise<unknown> | unknown; details?: import('svelte').Snippet
  } = $props()
  let typed = $state('')
  let busy = $state(false)
  const ok = $derived(!requireText || typed.trim() === requireText)
  async function go() {
    if (!ok) return
    busy = true
    try {
      await onconfirm()
      open = false
    } finally {
      busy = false
      typed = ''
    }
  }
</script>

<Dialog bind:open {title} description={body} size="sm">
  {@render details?.()}
  {#if requireText}
    <label class="type">
      <span>{t('common.type_to_confirm', { word: requireText })}</span>
      <Input bind:value={typed} autocomplete="off" spellcheck={false} />
    </label>
  {/if}
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (open = false)}>{t('common.cancel')}</Button>
    <Button variant={danger ? 'danger' : 'primary'} loading={busy} disabled={!ok} onclick={go}>{confirmLabel}</Button>
  {/snippet}
</Dialog>

<style>
  .type {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: var(--s-3);
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
</style>
