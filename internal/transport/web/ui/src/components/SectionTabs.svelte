<script lang="ts">
  import { route } from '../lib/router.svelte'
  import { t } from '../lib/i18n.svelte'
  import { sectionsFor } from '../lib/nav'

  // The pages one sidebar item holds (Statistics and Costs & limits; Upkeep and Settings file), as
  // links above the page: each keeps its own address, so bookmarks and links into it still work.
  const sections = $derived(sectionsFor(route.path))
  const here = $derived(sections.find((s) => route.path === s.href || route.path.startsWith(s.href + '/'))?.href)
</script>

{#if sections.length > 1}
  <nav class="st" aria-label={t('nav.sections')}>
    {#each sections as s (s.href)}
      <a href={s.href} class:on={here === s.href} aria-current={here === s.href ? 'page' : undefined}>{t(`nav.section.${s.key}`)}</a>
    {/each}
  </nav>
{/if}

<style>
  .st {
    display: flex;
    gap: var(--s-1);
    margin-bottom: var(--s-4);
    padding: 3px;
    width: fit-content;
    max-width: 100%;
    overflow-x: auto;
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  a {
    padding: 6px 14px;
    border-radius: calc(var(--r-lg) - 3px);
    font-size: var(--text-sm);
    font-weight: 550;
    color: var(--text-muted);
    text-decoration: none;
    white-space: nowrap;
    transition: background-color var(--dur) var(--ease), color var(--dur) var(--ease);
  }
  a:hover {
    color: var(--text);
  }
  a.on {
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-border);
  }
  a:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
</style>
