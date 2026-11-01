<script lang="ts">
  import type { Snippet } from 'svelte'
  import { X } from '../lib/icons'
  import { t } from '../lib/i18n.svelte'
  // Native <dialog>: showModal() gives focus trapping, inert background and Escape for free.
  let { open = $bindable(false), title, description, children, footer, size = 'md', onclose }: { open?: boolean; title: string; description?: string; children?: Snippet; footer?: Snippet; size?: 'sm' | 'md' | 'lg' | 'xl'; onclose?: () => void } = $props()
  let el: HTMLDialogElement | undefined = $state()
  const uid = $props.id()
  // typing in the dialog makes a stray click on the backdrop harmless: it would discard what was typed
  let touched = false
  $effect(() => {
    if (!el) return
    if (open && !el.open) {
      touched = false
      el.showModal()
    } else if (!open && el.open) el.close()
  })
  // Enter in a text field does what the footer's main button does (not while an IME is composing, not in
  // a field that belongs to its own form)
  function enter(e: KeyboardEvent) {
    const field = e.target
    if (e.key !== 'Enter' || e.isComposing || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return
    if (!(field instanceof HTMLInputElement) || field.form || ['checkbox', 'radio', 'button', 'submit', 'file', 'range'].includes(field.type)) return
    const main = el?.querySelector<HTMLButtonElement>('footer .btn.primary, footer .btn.danger')
    if (main && !main.disabled) {
      e.preventDefault()
      main.click()
    }
  }
</script>

<dialog bind:this={el} class={size} aria-labelledby="{uid}-t" aria-describedby={description ? `${uid}-d` : undefined}
  onclose={() => { open = false; onclose?.() }}
  oninput={() => (touched = true)}
  onkeydown={enter}
  onclick={(e) => { if (e.target === el && !touched) open = false }}>
  <div class="inner">
    <header>
      <div>
        <h2 id="{uid}-t">{title}</h2>
        {#if description}<p id="{uid}-d">{description}</p>{/if}
      </div>
      <button type="button" class="close" aria-label={t('common.close')} onclick={() => (open = false)}><X size={18} aria-hidden="true" /></button>
    </header>
    <div class="content">{@render children?.()}</div>
    {#if footer}<footer>{@render footer()}</footer>{/if}
  </div>
</dialog>

<style>
  dialog {
    width: min(100% - 32px, 520px);
    max-height: min(100% - 48px, 860px);
    padding: 0;
    border: 0;
    border-radius: var(--r-xl);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-pop);
    overscroll-behavior: contain;
    opacity: 0;
    scale: 0.96;
    transition-property: opacity, scale, overlay, display;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
    transition-behavior: allow-discrete;
  }
  dialog[open] {
    opacity: 1;
    scale: 1;
  }
  @starting-style {
    dialog[open] {
      opacity: 0;
      scale: 0.96;
    }
  }
  dialog::backdrop {
    background: var(--scrim);
    opacity: 0;
    transition-property: opacity, overlay, display;
    transition-duration: var(--dur);
    transition-behavior: allow-discrete;
  }
  dialog[open]::backdrop {
    opacity: 1;
  }
  @starting-style {
    dialog[open]::backdrop {
      opacity: 0;
    }
  }
  .sm {
    width: min(100% - 32px, 420px);
  }
  .lg {
    width: min(100% - 32px, 760px);
  }
  .xl {
    width: min(100% - 32px, 1080px);
  }
  /* The dialog's own max-height is a percentage of an auto height, which a child cannot inherit: left as it was,
     the content never scrolled by itself and the footer (Apply, Cancel) sat at the bottom of a page-tall dialog,
     out of sight on a phone. The same limit is stated here in viewport units, so header and footer stay on
     screen and only .content scrolls. */
  .inner {
    display: flex;
    flex-direction: column;
    max-height: min(100dvh - 48px, 860px);
  }
  header {
    display: flex;
    gap: var(--s-3);
    align-items: flex-start;
    padding: var(--s-5) var(--s-5) var(--s-3);
  }
  header > div {
    flex: 1;
  }
  h2 {
    font-size: var(--text-lg);
  }
  header p {
    margin-top: 4px;
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .close {
    display: grid;
    place-items: center;
    width: 32px;
    height: 32px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .close:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .content {
    padding: 0 var(--s-5) var(--s-5);
    overflow: auto;
    min-height: 0;
  }
  footer {
    display: flex;
    justify-content: flex-end;
    gap: var(--s-2);
    flex-wrap: wrap;
    padding: var(--s-3) var(--s-5) var(--s-5);
  }
</style>
