<script lang="ts">
  import type { Snippet } from 'svelte'
  import TopBar from './TopBar.svelte'
  import Sidebar from './Sidebar.svelte'
  import Palette from './Palette.svelte'
  import Banners from './Banners.svelte'
  import Tour from './Tour.svelte'
  import SectionTabs from './SectionTabs.svelte'
  import { t } from '../lib/i18n.svelte'

  let { children }: { children: Snippet } = $props()
  let paletteOpen = $state(false)
  let menuOpen = $state(false)

  function key(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault()
      paletteOpen = !paletteOpen
    } else if (e.key === '/' && !paletteOpen && !(e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || e.target instanceof HTMLSelectElement)) {
      e.preventDefault()
      paletteOpen = true
    }
  }
</script>

<svelte:window onkeydown={key} />

<a class="skip-link" href="#main" data-native>{t('nav.skip')}</a>
<TopBar onsearch={() => (paletteOpen = true)} onmenu={() => (menuOpen = !menuOpen)} pinned={menuOpen || paletteOpen} {menuOpen} />
<div class="body">
  <Sidebar bind:mobileOpen={menuOpen} />
  <main id="main" tabindex="-1">
    <Banners />
    <div class="page"><SectionTabs />{@render children()}</div>
  </main>
</div>
<Palette bind:open={paletteOpen} />
<Tour />

<style>
  .body {
    display: flex;
    min-height: calc(100dvh - var(--topbar-h));
  }
  main {
    flex: 1;
    min-width: 0;
    outline: none;
    view-transition-name: page;
  }
  .page {
    padding: var(--s-6) clamp(16px, 2.2vw, 40px) var(--s-10);
  }
</style>
