<script lang="ts">
  import { t, has } from '../lib/i18n.svelte'

  // Setting-by-setting changes between two config documents ({path, before, after}); a path shows its
  // settings-form label when there is one.
  let { changes = [] }: { changes: { path: string; before?: unknown; after?: unknown }[] } = $props()
  // bookkeeping the server keeps next to a setting (view revision, fingerprint, timestamp) is noise here
  const internal = /(\.memory_view\.(revision|scope_fingerprint|updated_at))$/
  const shown = $derived(changes.filter((c) => !internal.test(c.path)))

  const label = (path: string) => (has(`cfg.${path}`) ? t(`cfg.${path}`) : '')
  function show(v: unknown): string {
    if (v === undefined || v === null) return ''
    if (Array.isArray(v)) return v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ')
    if (typeof v === 'object') return JSON.stringify(v)
    return String(v)
  }
</script>

{#if shown.length === 0}
  <p class="small muted">{t('changes.none')}</p>
{:else}
  <ul class="changes">
    {#each shown as c (c.path)}
      <li>
        <div class="p">{#if label(c.path)}<strong class="small">{label(c.path)}</strong>{/if}<code class="xs mono break">{c.path}</code></div>
        <div class="v small">
          {#if c.before === undefined || c.before === null}<span class="tag add">{t('changes.added')}</span>
          {:else}<span class="old break">{show(c.before)}</span>{/if}
          {#if c.after === undefined || c.after === null}<span class="tag del">{t('changes.removed')}</span>
          {:else}{#if c.before !== undefined && c.before !== null}<span class="arrow" aria-hidden="true">→</span>{/if}<span class="new break">{show(c.after)}</span>{/if}
        </div>
      </li>
    {/each}
  </ul>
{/if}

<style>
  .changes {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 2px;
    max-height: 56vh;
    overflow: auto;
  }
  li {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(0, 3fr);
    gap: var(--s-3);
    padding: 8px 10px;
    border-radius: var(--r);
    background: var(--surface-2);
  }
  @media (max-width: 640px) {
    li {
      grid-template-columns: 1fr;
    }
  }
  .p {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .p code {
    color: var(--text-subtle);
  }
  .v {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 6px;
    min-width: 0;
  }
  .old {
    color: var(--danger);
    text-decoration: line-through;
    text-decoration-thickness: 1px;
  }
  .new {
    color: var(--ok);
    font-weight: 600;
  }
  .arrow {
    color: var(--text-subtle);
  }
  .tag {
    font-size: var(--text-xs);
    padding: 0 6px;
    border-radius: 999px;
  }
  .tag.add {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tag.del {
    background: var(--danger-soft);
    color: var(--danger);
  }
</style>
