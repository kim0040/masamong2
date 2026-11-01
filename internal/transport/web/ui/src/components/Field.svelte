<script lang="ts">
  import type { Snippet } from 'svelte'
  // Label + control + hint/error. The control snippet receives the ids it must use.
  let { label, hint, error, id, required = false, children, inline = false }: { label: string; hint?: string; error?: string; id: string; required?: boolean; inline?: boolean; children: Snippet<[{ id: string; describedby: string | undefined; invalid: boolean }]> } = $props()
  const describedby = $derived([hint ? `${id}-hint` : '', error ? `${id}-err` : ''].filter(Boolean).join(' ') || undefined)
</script>

<div class="field" class:inline>
  <label for={id}>{label}{#if required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
  {@render children({ id, describedby, invalid: !!error })}
  {#if hint}<p class="hint" id="{id}-hint">{hint}</p>{/if}
  {#if error}<p class="err" id="{id}-err">{error}</p>{/if}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
  }
  .inline {
    display: grid;
    grid-template-columns: minmax(140px, 220px) 1fr;
    align-items: start;
    column-gap: var(--s-4);
  }
  .inline .hint,
  .inline .err {
    grid-column: 2;
  }
  .inline label {
    padding-top: 8px;
  }
  label {
    font-size: var(--text-sm);
    font-weight: 550;
    color: var(--text);
  }
  .req {
    color: var(--danger);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .err {
    font-size: var(--text-xs);
    color: var(--danger);
    font-weight: 550;
  }
  @media (max-width: 640px) {
    .inline {
      display: flex;
    }
    .inline label {
      padding-top: 0;
    }
  }
</style>
