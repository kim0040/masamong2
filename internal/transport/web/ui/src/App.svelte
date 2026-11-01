<script lang="ts">
  import { brand } from 'virtual:brand'
  import type { Component } from 'svelte'
  import { routes, notFound } from './routes'
  import { route, match, navigate, startRouter } from './lib/router.svelte'
  import { loadRoute, prefetchRoute } from './lib/route-load'
  import { session, loadSession, setUnauthorizedHandler, practice } from './lib/api.svelte'
  import { loadStrings, t, i18n } from './lib/i18n.svelte'
  import { prefs, loadPrefs, applyCachedPrefs } from './lib/prefs.svelte'
  import { connectLive, disconnectLive, live } from './lib/live.svelte'
  import { startOverview } from './lib/overview.svelte'
  import { useConfig } from './lib/config.svelte'
  import Shell from './components/Shell.svelte'
  import Loader from './components/Loader.svelte'
  import Toaster from './components/Toaster.svelte'
  import Stopped from './components/Stopped.svelte'
  import LoadState from './components/LoadState.svelte'

  let booted = $state(false)
  let bootError = $state('')
  let page = $state<{ C: Component<any> | null; params: Record<string, string>; bare: boolean }>({ C: null, params: {}, bare: true })
  let slow = $state(false)
  let routeLoadFailed = $state(false)
  let routeRetry = $state(0)
  let routeLoadGeneration = 0
  const loaded = new Map<string, Component<any>>()

  function retryRouteLoad() {
    routeLoadFailed = false
    routeRetry++
    return Promise.resolve()
  }

  const routeFailure = $derived({ data: null, error: { message: t('load.failed') }, refresh: retryRouteLoad })

  async function boot() {
    applyCachedPrefs()
    startRouter()
    try {
      const s = await loadSession()
      await loadStrings(prefs.lang ?? s.lang)
      setUnauthorizedHandler(async () => {
        disconnectLive()
        await loadSession()
        if (route.path !== '/login') navigate('/login?next=' + encodeURIComponent(route.path), { replace: true })
      })
      if (s.authenticated) await afterLogin()
      else if (route.path !== '/login') navigate('/login' + (route.path !== '/' ? '?next=' + encodeURIComponent(route.path) : ''), { replace: true })
      booted = true
    } catch {
      bootError = 'offline'
    }
  }

  async function afterLogin() {
    await loadPrefs()
    if (prefs.lang && prefs.lang !== i18n.lang) await loadStrings(prefs.lang)
    connectLive()
    if (session.value?.ready || practice.on) {
      startOverview()
      useConfig()
    } else if (route.path !== '/setup') navigate('/setup', { replace: true })
  }

  $effect(() => {
    void routeRetry
    const path = route.path
    void route.seq
    let found: (typeof routes)[number] | undefined
    let params: Record<string, string> = {}
    for (const r of routes) {
      const m = match(r.pattern, path)
      if (m) {
        found = r
        params = m
        break
      }
    }
    if (found?.redirect) {
      navigate(found.redirect, { replace: true })
      return
    }
    const key = found?.pattern ?? '*'
    const load = found?.load ?? notFound
    const bare = found?.bare ?? false
    if (loaded.has(key)) {
      routeLoadFailed = false
      page = { C: loaded.get(key)!, params, bare }
      return
    }
    const generation = ++routeLoadGeneration
    routeLoadFailed = false
    slow = false
    const timer = setTimeout(() => (slow = true), 160)
    void loadRoute(load, (component) => {
      clearTimeout(timer)
      loaded.set(key, component)
      if (generation === routeLoadGeneration && route.path === path) page = { C: component, params, bare }
    }, () => {
      clearTimeout(timer)
      if (generation === routeLoadGeneration && route.path === path) {
        slow = false
        routeLoadFailed = true
      }
    })
  })

  // prefetch a page's code when the pointer rests on its link
  function prefetch(e: PointerEvent) {
    const a = (e.target as Element).closest?.('a[href^="/"]') as HTMLAnchorElement | null
    if (!a) return
    const path = new URL(a.href).pathname
    for (const r of routes) if (match(r.pattern, path) && !loaded.has(r.pattern)) void prefetchRoute(r.load, (component) => loaded.set(r.pattern, component))
  }

  // ⌘K / Ctrl+K handled in the shell; "g h" style shortcuts could live here later.
  boot()
</script>

<svelte:document onpointerover={prefetch} />

{#if bootError}
  <div class="boot-err">
    <img src={brand('mascot/mong-oops.webp')} alt="" width="140" height="140" />
    <h1>Masamong</h1>
    <p>The console cannot reach the service. <a href="/lite" data-native>Open the emergency console</a> or reload.</p>
  </div>
{:else if live.lifecycle === 'stopping' || live.lifecycle === 'stopped'}
  <Stopped />
{:else if !booted || !i18n.ready}
  <div class="boot"><Loader label="" /></div>
{:else if page.bare || !session.value?.authenticated}
  {#if routeLoadFailed}<LoadState res={routeFailure} />{:else if page.C}<page.C params={page.params} {afterLogin} />{:else if slow}<div class="boot"><Loader label={t('common.loading')} /></div>{/if}
{:else}
  <Shell>
    {#if routeLoadFailed}
      <LoadState res={routeFailure} />
    {:else if page.C}
      {#key page.C}<page.C params={page.params} />{/key}
    {:else if slow}
      <Loader label={t('common.loading')} />
    {/if}
  </Shell>
{/if}
<Toaster />

<style>
  .boot,
  .boot-err {
    min-height: 100dvh;
    display: grid;
    place-content: center;
    text-align: center;
    gap: var(--s-3);
    padding: var(--s-6);
  }
  .boot-err img {
    margin: 0 auto;
  }
</style>
