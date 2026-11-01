<script lang="ts">
  import type { Snippet } from 'svelte'
  import { route } from '../lib/router.svelte'
  import { t } from '../lib/i18n.svelte'
  import { tours, tourKey } from '../lib/tours'
  import { askTour } from '../lib/tourbus.svelte'
  import { CircleHelp } from '../lib/icons'
  import { useTitle } from '../lib/title.svelte'
  // a page with a tour offers it here instead of opening it by itself on the first visit
  const hasTour = $derived(!!tours[tourKey(route.path)])
  let { title, docTitle, description, actions, crumbs, children }: { title: string; docTitle?: string; description?: string; actions?: Snippet; crumbs?: { href: string; label: string }[]; children?: Snippet } = $props()
  // the tab says the page, not its greeting: docTitle replaces the heading where that is not a name
  useTitle(() => docTitle ?? title)
</script>

<header class="ph">
  {#if crumbs?.length}
    <nav aria-label={t('common.breadcrumb')} class="crumbs enter">
      {#each crumbs as c, i}<a href={c.href}>{c.label}</a><span aria-hidden="true">/</span>{/each}
    </nav>
  {/if}
  <div class="row1">
    <div class="titles">
      <h1 class="enter" style:--i="0">{title}</h1>
      {#if description}<p class="enter" style:--i="1">{description}</p>{/if}
    </div>
    {#if actions || hasTour}<div class="actions enter" style:--i="1">{@render actions?.()}{#if hasTour}<button type="button" class="help" onclick={askTour} aria-label={t('tour.show')} title={t('tour.show')}><CircleHelp size={18} aria-hidden="true" /></button>{/if}</div>{/if}
  </div>
  {@render children?.()}
</header>

<style>
  .ph {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    margin-bottom: var(--s-6);
  }
  .crumbs {
    display: flex;
    gap: 6px;
    font-size: var(--text-sm);
    color: var(--text-subtle);
  }
  .crumbs a {
    color: var(--text-muted);
    text-decoration: none;
  }
  .crumbs a:hover {
    color: var(--text);
    text-decoration: underline;
  }
  .row1 {
    display: flex;
    gap: var(--s-4);
    align-items: flex-end;
    flex-wrap: wrap;
  }
  .titles {
    flex: 1 1 240px;
    min-width: 0;
  }
  h1 {
    overflow-wrap: anywhere;
  }
  p {
    margin-top: 6px;
    color: var(--text-muted);
    max-width: 75ch;
  }
  .actions {
    display: flex;
    gap: var(--s-2);
    flex-wrap: wrap;
    align-items: center;
  }
  .help {
    display: grid;
    place-items: center;
    width: var(--control-h);
    height: var(--control-h);
    border: 0;
    border-radius: var(--r);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    transition: background-color var(--dur) var(--ease), color var(--dur) var(--ease);
  }
  .help:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .help:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
</style>
