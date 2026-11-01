<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { session, call, practice, setPractice, can } from '../lib/api.svelte'
  import { live } from '../lib/live.svelte'
  import { prefs, setPrefs, densities } from '../lib/prefs.svelte'
  import { overview, refreshOverview } from '../lib/overview.svelte'
  import { attentionText, isAttentionRead, markAllAttentionRead, markAttentionRead, syncAttentionReads, type Attention } from '../lib/attention.svelte'
  import { usd } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { navigate } from '../lib/router.svelte'
  import LogoutConfirm from './LogoutConfirm.svelte'
  import Popover from './Popover.svelte'
  import Segmented from './Segmented.svelte'
  import IconButton from './IconButton.svelte'
  import Status from './Status.svelte'
  import LanguageSelect from './LanguageSelect.svelte'
  import * as I from '../lib/icons'

  let { onsearch, onmenu, pinned = false, menuOpen = false }: { onsearch: () => void; onmenu: () => void; pinned?: boolean; menuOpen?: boolean } = $props()

  // The bar slides away while the page scrolls down and comes back as soon as it scrolls up, so long
  // pages get the whole screen. It stays while pinned (menu or search open), while focus is in it
  // (keyboard users tabbing to it), and near the top of the page.
  let header = $state<HTMLElement>()
  let hidden = $state(false)
  let focused = $state(false)
  let lastY = 0
  let travel = 0
  function scrolled() {
    const y = Math.max(0, window.scrollY)
    const step = y - lastY
    lastY = y
    if (y <= (header?.offsetHeight ?? 56)) {
      travel = 0
      hidden = false
      return
    }
    if (step > 0 !== travel > 0) travel = 0
    travel += step
    if (travel > 24) hidden = true
    else if (travel < -8) hidden = false
  }
  $effect(() => {
    const root = document.documentElement
    if (hidden && !pinned && !focused) root.dataset.topbar = 'hidden'
    else delete root.dataset.topbar
    return () => delete root.dataset.topbar
  })
  const month = $derived(overview.data?.month)
  const paused = $derived(!!overview.data?.pause?.all)
  const attention = $derived((overview.data?.attention ?? []) as Attention[])
  const unread = $derived(attention.filter((a) => !isAttentionRead(a)))
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
  let pausing = $state(false)
  let switching = $state(false)
  let logoutOpen = $state(false)
  $effect(() => {
    // During boot/reconnect the overview is briefly absent. Treating that as "all resolved" would
    // erase persisted read state before the first real dashboard response arrives.
    if (overview.data) syncAttentionReads(attention)
  })
  async function togglePractice(close: () => void) {
    switching = true
    const ok = await setPractice(!practice.on)
    switching = false
    close()
    if (!ok) toast('error', t('practice.failed'))
  }

  async function togglePause() {
    pausing = true
    const was = paused
    const r = await call('runtime.pause', { scope: 'all', paused: was ? 'false' : 'true' })
    pausing = false
    await refreshOverview()
    if (r.ok) toast(was ? 'ok' : 'warn', was ? t('pause.resumed') : t('pause.paused'), was ? undefined : t('pause.paused_body'), was ? undefined : { label: t('pause.undo'), run: () => void call('runtime.pause', { scope: 'all', paused: 'false' }).then(refreshOverview) })
    else toast('error', t('pause.failed'), r.error?.message)
  }
</script>

<svelte:window onscroll={scrolled} />
<header class="tb" bind:this={header} onfocusin={() => (focused = true)} onfocusout={(e) => (focused = !!header?.contains(e.relatedTarget as Node))}>
  <IconButton icon={I.Menu} label={t('nav.menu')} class="menu" onclick={onmenu} aria-expanded={menuOpen} aria-controls="sidebar" />
  <a class="brand" href="/" aria-label={t('nav.home')}>
    <img src="/brand/logo-mark.svg" alt="" width="30" height="30" class="logo" />
    <span class="name">{t('app.name')}</span>
    {#if session.value?.instance && !practice.on}<span class="inst chip">{session.value.instance}</span>{/if}
  </a>
  {#if practice.on}<a class="prac" href="/" title={t('practice.badge_title')}><I.FlaskConical size={14} aria-hidden="true" /><span class="prac-label">{t('practice.badge')}</span></a>{/if}
  <span class="live {live.status}" title={t(`live.${live.status}`)} role="status"><span class="d" aria-hidden="true"></span><span class={live.status === 'live' ? 'sr-only' : 'lt'}>{t(`live.${live.status}`)}</span></span>

  <button type="button" class="search" onclick={onsearch} aria-label={t('palette.placeholder_short')} aria-keyshortcuts={isMac ? 'Meta+K' : 'Control+K'}>
    <I.Search size={16} aria-hidden="true" /><span class="ph">{t('palette.placeholder_short')}</span><kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd>
  </button>

  <div class="right">
    {#if month && month.budget_usd > 0}
      <a class="cost" href="/usage" title={t('topbar.month_cost')}>
        <I.Coins size={15} aria-hidden="true" />
        <span class="num">{usd(month.spent_usd)}</span><span class="of">/ {usd(month.budget_usd, 0)}</span>
      </a>
    {/if}
    <button type="button" class="pause" class:on={paused} onclick={togglePause} disabled={pausing || !overview.data || !can('runtime.pause')} aria-pressed={paused}>
      {#if paused}<I.Play size={15} aria-hidden="true" /><span class="lbl">{t('pause.resume')}</span>{:else}<I.Pause size={15} aria-hidden="true" /><span class="lbl">{t('pause.pause_all')}</span>{/if}
    </button>

    <Popover width={340}>
      {#snippet trigger({ toggle, expanded, id })}
        <button type="button" class="ib" onclick={toggle} aria-expanded={expanded} aria-controls={id} aria-label={t('topbar.notifications', { n: unread.length })}>
          <I.Bell size={18} strokeWidth={1.75} aria-hidden="true" />
          {#if unread.length}<span class="badge num">{unread.length > 99 ? '99+' : unread.length}</span>{/if}
        </button>
      {/snippet}
      {#snippet children({ close })}
        <div class="notice-head">
          <div class="menu-h">{t('topbar.attention')}</div>
          {#if unread.length}<button type="button" class="mark-all" onclick={() => markAllAttentionRead(attention)}>{t('topbar.mark_all_read')}</button>{/if}
        </div>
        {#if attention.length === 0}
          <p class="none">{t('topbar.all_clear')}</p>
        {:else}
          <ul class="att">
            {#each attention as a}
              <li class:read={isAttentionRead(a)}><a href={a.route} onclick={() => { markAttentionRead(a); close() }}><span class="att-state"><Status tone={a.severity === 'danger' ? 'danger' : a.severity === 'warn' ? 'warn' : 'info'} label={t(`severity.${a.severity}`)} size="sm" />{#if !isAttentionRead(a)}<span class="unread-dot" aria-hidden="true"></span><span class="sr-only">{t('topbar.unread')}</span>{/if}</span><span class="att-copy">{attentionText(a)}</span></a></li>
            {/each}
          </ul>
        {/if}
      {/snippet}
    </Popover>

    <Popover width={300}>
      {#snippet trigger({ toggle, expanded, id })}
        <button type="button" class="ib" onclick={toggle} aria-expanded={expanded} aria-controls={id} aria-label={t('topbar.display')}>
          {#if prefs.theme === 'dark'}<I.Moon size={18} strokeWidth={1.75} aria-hidden="true" />{:else if prefs.theme === 'light'}<I.Sun size={18} strokeWidth={1.75} aria-hidden="true" />{:else}<I.Monitor size={18} strokeWidth={1.75} aria-hidden="true" />{/if}
        </button>
      {/snippet}
      {#snippet children()}
        <div class="menu-body">
          <div class="menu-h">{t('display.theme')}</div>
          <Segmented label={t('display.theme')} value={prefs.theme} onchange={(v) => setPrefs({ theme: v })} options={[
            { value: 'light', label: t('display.light'), icon: I.Sun }, { value: 'dark', label: t('display.dark'), icon: I.Moon }, { value: 'system', label: t('display.system'), icon: I.Monitor }]} />
          <div class="menu-h">{t('display.language')}</div>
          <LanguageSelect />
          <div class="menu-h">{t('display.density')}</div>
          <Segmented label={t('display.density')} value={prefs.density} onchange={(v) => setPrefs({ density: v })} options={densities.map((d) => ({ value: d, label: t(`display.density.${d}`) }))} />
          <div class="menu-h">{t('display.motion')}</div>
          <Segmented label={t('display.motion')} value={prefs.motion} onchange={(v) => setPrefs({ motion: v })} options={[{ value: 'full', label: t('display.motion_full') }, { value: 'reduced', label: t('display.motion_reduced') }, { value: 'system', label: t('display.system') }]} />
        </div>
      {/snippet}
    </Popover>

    <Popover width={260}>
      {#snippet trigger({ toggle, expanded, id })}
        <button type="button" class="ib" onclick={toggle} aria-expanded={expanded} aria-controls={id} aria-label={t('topbar.account')}><I.CircleUser size={18} strokeWidth={1.75} aria-hidden="true" /></button>
      {/snippet}
      {#snippet children({ close })}
        <div class="who">
          <span class="who-n">{t('topbar.signed_in')}</span>
          <span class="who-r">{session.value?.instance ?? ''}</span>
        </div>
        <div class="items">
          <button type="button" class="prac-toggle" class:on={practice.on} onclick={() => togglePractice(close)} disabled={switching} aria-pressed={practice.on}>
            <I.FlaskConical size={16} aria-hidden="true" />{practice.on ? t('practice.leave') : t('practice.enter')}
          </button>
          <button type="button" onclick={() => { close(); navigate('/display') }}><I.SlidersHorizontal size={16} aria-hidden="true" />{t('topbar.display_page')}</button>
          <button type="button" onclick={() => { close(); navigate('/setup?tour=1') }}><I.BookOpen size={16} aria-hidden="true" />{t('topbar.tutorial')}</button>
          <a href="/lite" data-native><I.ExternalLink size={16} aria-hidden="true" />{t('nav.lite')}</a>
          <button type="button" onclick={() => { close(); logoutOpen = true }}><I.LogOut size={16} aria-hidden="true" />{t('topbar.logout')}</button>
        </div>
      {/snippet}
    </Popover>
  </div>
</header>
<LogoutConfirm bind:open={logoutOpen} />

<style>
  .tb {
    position: sticky;
    top: 0;
    z-index: 40;
    display: flex;
    align-items: center;
    gap: var(--s-3);
    height: var(--topbar-h);
    padding: 0 var(--s-4);
    background: color-mix(in oklab, var(--bg) 88%, transparent);
    backdrop-filter: saturate(1.4) blur(10px);
    box-shadow: inset 0 -1px 0 var(--border);
    transition: transform var(--dur) var(--ease);
  }
  :global(:root[data-topbar='hidden']) .tb {
    transform: translateY(-100%);
  }
  .tb :global(.menu) {
    display: none;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
    color: var(--text);
    text-decoration: none;
    min-width: 0;
  }
  .brand,
  .logo {
    flex: none;
  }
  .logo {
    border-radius: 999px;
  }
  .brand:hover .logo {
    animation: wiggle 500ms var(--ease);
  }
  @keyframes wiggle {
    30% {
      rotate: -8deg;
    }
    60% {
      rotate: 6deg;
    }
  }
  .name {
    font-weight: 700;
    font-size: var(--text-md);
    letter-spacing: -0.01em;
  }
  .inst {
    max-width: 140px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  :global(html[data-practice]) .tb {
    box-shadow: inset 0 -2px 0 var(--practice);
  }
  .prac {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    height: 24px;
    padding: 0 9px;
    border-radius: 999px;
    background: var(--practice-soft);
    color: var(--practice);
    font-size: var(--text-xs);
    font-weight: 700;
    text-decoration: none;
    white-space: nowrap;
    animation: badge-in 300ms var(--ease) both;
  }
  .ro {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    white-space: nowrap;
  }
  .who {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 8px 10px 10px;
    margin-bottom: 4px;
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .who-n {
    font-weight: 650;
    overflow-wrap: anywhere;
  }
  .who-r {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .items .prac-toggle.on {
    color: var(--practice);
  }
  .live {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 20px;
    height: 20px;
    justify-content: center;
  }
  .lt {
    font-size: var(--text-xs);
    color: var(--text-muted);
    white-space: nowrap;
  }
  .live .d {
    width: 8px;
    height: 8px;
    border-radius: 999px;
    background: var(--text-subtle);
  }
  .live.live .d {
    background: var(--ok);
    box-shadow: 0 0 0 0 color-mix(in oklab, var(--ok) 50%, transparent);
    animation: pulse 2.4s var(--ease) infinite;
  }
  .live.connecting .d {
    background: var(--warn);
  }
  .live.offline .d {
    background: var(--danger);
  }
  @keyframes pulse {
    70% {
      box-shadow: 0 0 0 6px transparent;
    }
  }
  .search {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex: 0 1 380px;
    margin-inline: auto;
    height: 34px;
    padding: 0 8px 0 12px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text-subtle);
    cursor: pointer;
    transition: box-shadow var(--dur-fast) var(--ease-out);
  }
  .search:hover {
    box-shadow: var(--shadow-border-hover);
  }
  .ph {
    flex: 1;
    text-align: start;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  kbd {
    padding: 1px 6px;
    border-radius: 4px;
    background: var(--surface-2);
    font-size: 11px;
    color: var(--text-muted);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .right {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .cost {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 32px;
    padding: 0 10px;
    border-radius: var(--r);
    color: var(--text-muted);
    font-size: var(--text-sm);
    text-decoration: none;
    white-space: nowrap;
  }
  .cost:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .cost .num {
    color: var(--text);
    font-weight: 600;
  }
  .of {
    color: var(--text-subtle);
  }
  .pause {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 32px;
    padding: 0 12px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    font-size: var(--text-sm);
    font-weight: 600;
    cursor: pointer;
    white-space: nowrap;
    transition-property: background-color, color, scale;
    transition-duration: var(--dur-fast);
    transition-timing-function: var(--ease-out);
  }
  .pause:active:not(:disabled) {
    scale: 0.96;
  }
  .pause.on {
    background: var(--warn-soft);
    color: var(--warn);
    box-shadow: none;
  }
  .pause:disabled {
    opacity: 0.6;
  }
  .ib {
    position: relative;
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .ib:hover,
  .ib[aria-expanded='true'] {
    background: var(--surface-2);
    color: var(--text);
  }
  .badge {
    position: absolute;
    top: 3px;
    inset-inline-end: 2px;
    min-width: 16px;
    height: 16px;
    padding: 0 4px;
    border-radius: 999px;
    background: var(--accent);
    color: var(--on-accent);
    font-size: 10px;
    font-weight: 700;
    line-height: 16px;
    animation: badge-in 400ms var(--ease) both;
  }
  @keyframes badge-in {
    from {
      scale: 0.25;
      opacity: 0;
    }
  }
  .menu-h {
    padding: 8px 8px 4px;
    font-size: var(--text-xs);
    font-weight: 650;
    color: var(--text-subtle);
  }
  .notice-head {
    display: flex;
    align-items: center;
    gap: var(--s-2);
  }
  .notice-head .menu-h {
    flex: 1;
  }
  .mark-all {
    min-height: 28px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--accent-text);
    font-size: var(--text-xs);
    font-weight: 650;
    cursor: pointer;
  }
  .mark-all:hover {
    background: var(--accent-soft);
  }
  .menu-body {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 2px;
  }
  .menu-body :global(.seg) {
    width: 100%;
  }
  .none {
    padding: 8px;
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .att {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .att a {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 4px;
    padding: 8px;
    border-radius: var(--r);
    color: var(--text);
    text-decoration: none;
    font-size: var(--text-sm);
  }
  .att li.read a {
    color: var(--text-muted);
  }
  .att-state {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .unread-dot {
    width: 7px;
    height: 7px;
    border-radius: 999px;
    background: var(--accent);
  }
  .att-copy {
    overflow-wrap: anywhere;
  }
  .att a:hover {
    background: var(--surface-2);
  }
  .items {
    display: flex;
    flex-direction: column;
  }
  .items button,
  .items a {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 36px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text);
    font-size: var(--text-sm);
    text-decoration: none;
    cursor: pointer;
    text-align: start;
  }
  .items button:hover,
  .items a:hover {
    background: var(--surface-2);
  }
  @media (max-width: 900px) {
    .tb :global(.menu) {
      display: inline-grid;
    }
    .inst,
    .ro,
    .cost .of {
      display: none;
    }
  }
  @media (max-width: 720px) {
    .search .ph,
    .search kbd,
    .name,
    .cost {
      display: none;
    }
    .search {
      flex: 0 0 36px;
      padding: 0;
      justify-content: center;
      margin-inline: auto 0;
    }
    .pause {
      padding: 0 10px;
    }
  }
  @media (max-width: 480px) {
    .tb {
      gap: 4px;
      padding: 0 8px;
    }
    .right {
      gap: 0;
    }
    .pause .lbl {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
    }
    .pause {
      width: 36px;
      padding: 0;
      justify-content: center;
    }
    .prac {
      width: 28px;
      padding: 0;
      justify-content: center;
    }
    .prac-label {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
    }
  }
  @media (max-width: 400px) {
    .brand {
      display: none;
    }
  }
</style>
