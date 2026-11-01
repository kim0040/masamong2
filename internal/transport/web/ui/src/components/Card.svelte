<script lang="ts">
  import { getContext, setContext, type Snippet } from 'svelte'
  import type { HTMLAttributes } from 'svelte/elements'
  let { title, description, actions, children, padded = true, tone = 'default', id, class: cls = '', footer, ...rest }: Omit<HTMLAttributes<HTMLElement>, 'title'> & { title?: string; description?: string; actions?: Snippet; children?: Snippet; footer?: Snippet; padded?: boolean; tone?: 'default' | 'muted' | 'accent'; id?: string; class?: string } = $props()
  const uid = $props.id()
  // headings inside a titled card sit one level below its h2 (EmptyState reads this); an untitled card
  // passes its parent's level through
  const outer = getContext<{ level?: number } | undefined>('heading-level')
  setContext('heading-level', { get level() { return title ? 3 : outer?.level } })
</script>

<section class="card {tone} {cls}" {id} aria-labelledby={title ? `${uid}-t` : undefined} {...rest}>
  {#if title || actions}
    <header>
      <div class="titles">
        {#if title}<h2 id="{uid}-t">{title}</h2>{/if}
        {#if description}<p class="desc">{description}</p>{/if}
      </div>
      {#if actions}<div class="actions">{@render actions()}</div>{/if}
    </header>
  {/if}
  <div class="inner" class:body={padded}>{@render children?.()}</div>
  {#if footer}<footer>{@render footer()}</footer>{/if}
</section>

<style>
  .card {
    background: var(--surface);
    border-radius: var(--r-xl);
    box-shadow: var(--shadow-border);
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .muted {
    background: var(--surface-2);
    box-shadow: none;
  }
  .accent {
    background: linear-gradient(in oklab 135deg, var(--accent-soft), var(--surface) 70%);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--s-2) var(--s-3);
    padding: var(--pad-card) var(--pad-card) 0;
  }
  .titles {
    flex: 1 1 16rem; /* narrower than this, the actions move under the title */
    min-width: 0;
  }
  h2 {
    font-size: var(--text-md);
    font-weight: 650;
  }
  .desc {
    margin-top: 2px;
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
  .actions {
    display: flex;
    gap: var(--s-2);
    align-items: center;
    flex-wrap: wrap;
  }
  .inner {
    flex: 1;
    min-width: 0;
  }
  .body {
    padding: var(--pad-card);
  }
  /* a card stretched to its row keeps an empty state in the middle instead of at the top */
  .inner:has(> :global(.empty:only-child)) {
    display: grid;
    align-content: center;
  }
  footer {
    padding: var(--s-3) var(--pad-card);
    box-shadow: inset 0 1px 0 var(--border);
  }
</style>
