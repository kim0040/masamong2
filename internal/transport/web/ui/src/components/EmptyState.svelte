<script lang="ts">
  import { getContext, type Snippet } from 'svelte'
  import Mascot from './Mascot.svelte'
  // level: h3 inside a titled card, else h2 right under the page's h1
  let { pose = 'box', title, description, children, compact = false, level = getContext<{ level?: number } | undefined>('heading-level')?.level ?? 2 }: { pose?: string; title: string; description?: string; children?: Snippet; compact?: boolean; level?: number } = $props()
</script>

<div class="empty" class:compact>
  <div class="art enter"><Mascot {pose} size={compact ? 96 : 150} /></div>
  <svelte:element this={`h${level}`} class="enter title" style:--i="1">{title}</svelte:element>
  {#if description}<p class="enter" style:--i="2">{description}</p>{/if}
  {#if children}<div class="actions enter" style:--i="3">{@render children()}</div>{/if}
</div>

<style>
  .empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: var(--s-2);
    padding: var(--s-10) var(--s-6);
  }
  .compact {
    padding: var(--s-6) var(--s-4);
  }
  .art {
    margin-bottom: var(--s-2);
  }
  .title {
    font-size: var(--text-md);
  }
  p {
    max-width: 44ch;
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .actions {
    margin-top: var(--s-3);
    display: flex;
    gap: var(--s-2);
    flex-wrap: wrap;
    justify-content: center;
  }
</style>
