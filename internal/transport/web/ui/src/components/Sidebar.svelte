<script lang="ts">
  import { tick } from 'svelte'
  import { nav, activeFor } from '../lib/nav'
  import { route } from '../lib/router.svelte'
  import { t } from '../lib/i18n.svelte'
  import { prefs, setPrefs } from '../lib/prefs.svelte'
  import { overview } from '../lib/overview.svelte'
  import { isAttentionRead, type Attention } from '../lib/attention.svelte'
  import { PanelLeft, CircleHelp, ExternalLink, SlidersHorizontal } from '../lib/icons'

  let { mobileOpen = $bindable(false) }: { mobileOpen?: boolean } = $props()
  const active = $derived(activeFor(route.path))
  const collapsed = $derived(prefs.sidebar === 'collapsed')
  let listEl: HTMLElement | undefined = $state()
  let pill = $state({ y: 0, h: 0, ready: false })

  // routes with something needing attention get a dot
  const dots = $derived(new Set<string>(((overview.data?.attention ?? []) as Attention[]).filter((a) => a.severity !== 'info' && !isAttentionRead(a)).map((a) => activeFor('/' + (a.route?.split('/')[1] ?? '')))))
  const checklist = $derived((overview.data?.checklist ?? []) as { done: boolean }[])
  const doneN = $derived(checklist.filter((c) => c.done).length)

  function measurePill() {
    const el = listEl?.querySelector(`a[data-href="${active}"]`) as HTMLElement | null
    if (el) pill = { y: el.offsetTop, h: el.offsetHeight, ready: true }
  }
  $effect(() => {
    void active
    void collapsed
    void tick().then(() => requestAnimationFrame(measurePill))
  })
  $effect(() => {
    if (!listEl) return
    const observer = new ResizeObserver(measurePill)
    observer.observe(listEl)
    return () => observer.disconnect()
  })
  $effect(() => {
    void route.path
    mobileOpen = false
  })

  // Below 900 px the menu is a drawer. Closed, it is off screen and must not be reachable by Tab or a
  // screen reader (inert); open, it is modal: focus moves into it and stays there, the page behind is
  // inert, Escape and the backdrop close it, and focus goes back to where it came from.
  let navEl: HTMLElement | undefined = $state()
  let narrow = $state(false)
  $effect(() => {
    const mq = matchMedia('(max-width: 900px)')
    narrow = mq.matches
    const sync = () => (narrow = mq.matches)
    mq.addEventListener('change', sync)
    return () => mq.removeEventListener('change', sync)
  })
  const modal = $derived(narrow && mobileOpen)
  // what Tab can reach: a control that is not drawn (the collapse button is hidden here) is no stop
  const focusables = () => [...(navEl?.querySelectorAll<HTMLElement>('a[href], button:not([disabled])') ?? [])].filter((el) => el.getClientRects().length > 0)
  $effect(() => {
    if (!modal) return
    const from = document.activeElement as HTMLElement | null
    const pathAtOpen = route.path
    const main = document.getElementById('main')
    main?.setAttribute('inert', '')
    void tick().then(() => (navEl?.querySelector<HTMLElement>('a[aria-current]') ?? focusables()[0])?.focus())
    return () => {
      main?.removeAttribute('inert')
      const now = document.activeElement
      if (now && now !== document.body && !navEl?.contains(now)) return // focus already went somewhere on purpose
      // after choosing a page the router moves focus to the page; otherwise back to the menu button
      if (route.path !== pathAtOpen) main?.focus({ preventScroll: true })
      else if (from?.isConnected) from.focus()
    }
  })
  function drawerKeys(e: KeyboardEvent) {
    if (!modal) return
    if (e.key === 'Escape') {
      e.preventDefault()
      mobileOpen = false
    } else if (e.key === 'Tab') {
      const list = focusables()
      const first = list[0]
      const last = list[list.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last?.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first?.focus()
      }
    }
  }
</script>

<svelte:window onkeydown={drawerKeys} />
<nav class="sb" id="sidebar" bind:this={navEl} class:collapsed class:open={mobileOpen} inert={narrow && !mobileOpen} aria-label={t('nav.label')}>
  <div class="list" bind:this={listEl}>
    <span class="pill" class:ready={pill.ready} style:translate="0 {pill.y}px" style:height="{pill.h}px" aria-hidden="true"></span>
    {#each nav as g (g.key)}
      <div class="group">
        <div class="gh" aria-hidden={collapsed}>{t(`nav.group.${g.key}`)}</div>
        {#each g.items as it (it.href)}
          <a href={it.href} data-href={it.href} class:active={active === it.href} aria-current={active === it.href ? 'page' : undefined} title={collapsed ? t(`nav.${it.key}`) : undefined}>
            <it.icon size={18} strokeWidth={1.75} aria-hidden="true" />
            <span class="lbl">{t(`nav.${it.key}`)}</span>
            {#if dots.has(it.href)}<span class="dot" aria-label={t('nav.needs_attention')}></span>{/if}
          </a>
        {/each}
      </div>
    {/each}
  </div>
  <div class="foot">
    {#if checklist.length && doneN < checklist.length}
      <a class="progress" href="/?checklist=1" title={t('checklist.title')}>
        <svg viewBox="0 0 36 36" width="28" height="28" aria-hidden="true"><circle cx="18" cy="18" r="14" class="bg" /><circle cx="18" cy="18" r="14" class="fg" stroke-dasharray={88} stroke-dashoffset={88 * (1 - doneN / checklist.length)} /></svg>
        <span class="lbl">{t('checklist.progress', { done: doneN, total: checklist.length })}</span>
      </a>
    {/if}
    <a href="/display" class="lite" class:here={route.path === '/display'} aria-current={route.path === '/display' ? 'page' : undefined} title={t('nav.display')}><SlidersHorizontal size={16} aria-hidden="true" /><span class="lbl">{t('nav.display')}</span></a>
    <a href="/lite" data-native class="lite" title={t('nav.lite')}><ExternalLink size={16} aria-hidden="true" /><span class="lbl">{t('nav.lite')}</span></a>
    <button type="button" class="collapse" onclick={() => setPrefs({ sidebar: collapsed ? 'expanded' : 'collapsed' })} aria-label={collapsed ? t('nav.expand') : t('nav.collapse')} aria-pressed={collapsed}>
      <PanelLeft size={18} strokeWidth={1.75} aria-hidden="true" /><span class="lbl">{collapsed ? t('nav.expand') : t('nav.collapse')}</span>
    </button>
  </div>
</nav>
{#if mobileOpen}<button type="button" class="scrim" tabindex="-1" aria-label={t('common.close')} onclick={() => (mobileOpen = false)}></button>{/if}

<style>
  .sb {
    position: sticky;
    top: var(--topbar-offset);
    height: calc(100dvh - var(--topbar-offset));
    width: var(--sidebar-w);
    flex: none;
    display: flex;
    flex-direction: column;
    padding: var(--s-3) var(--s-2);
    overflow-y: auto;
    overscroll-behavior: contain;
    transition:
      width var(--dur) var(--ease),
      top var(--dur) var(--ease),
      height var(--dur) var(--ease);
    scrollbar-width: thin;
  }
  .collapsed {
    width: 64px;
  }
  .list {
    position: relative;
    flex: 1;
  }
  .pill {
    position: absolute;
    inset-inline: 0;
    top: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    opacity: 0;
    pointer-events: none;
  }
  .pill.ready {
    opacity: 1;
    transition-property: translate, height;
    transition-duration: 260ms;
    transition-timing-function: var(--ease);
  }
  .group {
    margin-bottom: var(--s-3);
  }
  .gh {
    padding: var(--s-2) var(--s-3) 6px;
    font-size: var(--text-xs);
    font-weight: 650;
    letter-spacing: 0.04em;
    color: var(--text-subtle);
    white-space: nowrap;
    overflow: hidden;
    transition: opacity var(--dur) var(--ease);
  }
  .collapsed .gh {
    opacity: 0;
    height: 8px;
    padding: 0;
  }
  a,
  .collapse {
    position: relative;
    display: flex;
    align-items: center;
    gap: var(--s-3);
    height: var(--control-h);
    padding: 0 var(--s-3);
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    font-size: var(--text-base);
    font-weight: 550;
    text-decoration: none;
    white-space: nowrap;
    cursor: pointer;
    width: 100%;
    transition: color var(--dur-fast) var(--ease-out), background-color var(--dur-fast) var(--ease-out);
  }
  a.active {
    color: var(--text);
  }
  a.active :global(svg) {
    color: var(--accent-text);
  }
  @media (hover: hover) {
    a:not(.active):hover,
    .collapse:hover {
      color: var(--text);
      background: var(--surface-2);
    }
  }
  .lbl {
    overflow: hidden;
    text-overflow: ellipsis;
    transition: opacity var(--dur) var(--ease);
  }
  .collapsed .lbl {
    opacity: 0;
    width: 0;
  }
  .collapsed a,
  .collapsed .collapse {
    justify-content: center;
    gap: 0;
    padding-inline: 0;
  }
  .dot {
    position: absolute;
    inset-inline-end: 10px;
    width: 7px;
    height: 7px;
    border-radius: 999px;
    background: var(--warn);
    box-shadow: 0 0 0 2px var(--bg);
  }
  .collapsed .dot {
    inset-inline-end: 8px;
    top: 7px;
  }
  .foot {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding-top: var(--s-3);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .foot a.here {
    color: var(--text);
    background: var(--surface-2);
  }
  .progress circle {
    fill: none;
    stroke-width: 4;
  }
  .progress .bg {
    stroke: var(--surface-3);
  }
  .progress .fg {
    stroke: var(--accent);
    stroke-linecap: round;
    transform: rotate(-90deg);
    transform-origin: center;
    transition: stroke-dashoffset 600ms var(--ease);
  }
  .progress {
    height: 44px;
    margin-inline-start: -4px;
  }
  .collapsed .progress {
    margin-inline: 0;
  }
  .scrim {
    display: none;
  }
  @media (max-width: 900px) {
    .sb {
      position: fixed;
      z-index: 50;
      inset-block: var(--topbar-h) 0;
      inset-inline-start: 0;
      width: min(300px, 86vw);
      background: var(--bg);
      box-shadow: var(--shadow-pop);
      translate: -105% 0;
      transition: translate var(--dur-slow) var(--ease);
    }
    :global([dir='rtl']) .sb {
      translate: 105% 0;
    }
    .sb.open {
      translate: 0 0;
    }
    .collapsed {
      width: min(300px, 86vw);
    }
    .collapsed a,
    .collapsed .collapse {
      justify-content: flex-start;
      gap: var(--s-3);
      padding-inline: var(--s-3);
    }
    .collapsed .lbl {
      opacity: 1;
      width: auto;
    }
    .collapsed .gh {
      opacity: 1;
      width: auto;
      height: auto;
      padding: var(--s-2) var(--s-3) 6px;
    }
    .collapsed .progress {
      margin-inline-start: -4px;
    }
    .collapse {
      display: none;
    }
    .scrim {
      display: block;
      position: fixed;
      z-index: 45;
      inset: var(--topbar-h) 0 0 0;
      border: 0;
      background: var(--scrim);
      animation: fade-in var(--dur) var(--ease) both;
    }
  }
  @keyframes fade-in {
    from {
      opacity: 0;
    }
  }
</style>
