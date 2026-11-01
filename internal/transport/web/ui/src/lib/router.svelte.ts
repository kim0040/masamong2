// History router. Links stay real <a href> (Cmd/Ctrl/middle-click work); plain left clicks are
// intercepted and swapped with a View Transition when motion is allowed.
import { motionAllowed } from './prefs.svelte'

export const route = $state<{ path: string; query: URLSearchParams; seq: number }>({
  path: location.pathname,
  query: new URLSearchParams(location.search),
  seq: 0,
})

const historyIndexKey = '__masamongHistoryIndex'
let beforeNavigate: (() => boolean | Promise<boolean>) | null = null
let unload: ((e: BeforeUnloadEvent) => void) | null = null
let historyIndex = history.state?.[historyIndexKey] ?? 0
let currentHref = location.href
let routerStarted = false
let popPending = false
let programmaticPop: { kind: 'restore' | 'approved'; index: number; href: string } | null = null
let pendingPop: {
  targetIndex: number | null
  targetHref: string
  previousIndex: number
  previousHref: string
  restored: boolean
  decision: boolean | null
} | null = null

function stateAt(index: number) {
  const state = history.state
  return { ...(state && typeof state === 'object' ? state : {}), [historyIndexKey]: index }
}

/** A page with unsaved edits can ask before leaving (returns false to stay, or a promise of that). */
export function setNavigationGuard(fn: (() => boolean | Promise<boolean>) | null) {
  beforeNavigate = fn
  if (fn && !unload) {
    unload = (e) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', unload)
  } else if (!fn && unload) {
    window.removeEventListener('beforeunload', unload)
    unload = null
  }
}

function apply(url: URL) {
  const update = () => {
    route.path = url.pathname
    route.query = new URLSearchParams(url.search)
    route.seq++
  }
  const doc = document as Document & { startViewTransition?: (cb: () => void) => unknown }
  if (doc.startViewTransition && motionAllowed()) doc.startViewTransition(update)
  else update()
}

export function navigate(href: string, opts: { replace?: boolean } = {}) {
  const url = new URL(href, location.origin)
  if (url.origin !== location.origin) {
    location.href = href
    return
  }
  const go = () => {
    if (opts.replace) {
      history.replaceState(stateAt(historyIndex), '', url)
    } else {
      historyIndex++
      history.pushState(stateAt(historyIndex), '', url)
    }
    currentHref = url.href
    apply(url)
    if (!opts.replace) window.scrollTo({ top: 0 })
    document.getElementById('main')?.focus({ preventScroll: true })
  }
  const allowed = beforeNavigate?.() ?? true
  if (allowed instanceof Promise) void allowed.then((ok) => ok && go()).catch(() => {})
  else if (allowed) go()
}

export function setQuery(params: Record<string, string | null | undefined>) {
  const url = new URL(location.href)
  for (const [k, v] of Object.entries(params)) {
    if (v === null || v === undefined || v === '') url.searchParams.delete(k)
    else url.searchParams.set(k, v)
  }
  history.replaceState(stateAt(historyIndex), '', url)
  currentHref = url.href
  route.query = new URLSearchParams(url.search)
}

function historyIndexOf(state: unknown): number | null {
  if (!state || typeof state !== 'object') return null
  const value = (state as Record<string, unknown>)[historyIndexKey]
  return typeof value === 'number' && Number.isInteger(value) ? value : null
}

function restoreTraversal(fromIndex: number | null, previousIndex: number, previousHref: string): boolean {
  if (fromIndex !== null && fromIndex !== previousIndex) {
    programmaticPop = { kind: 'restore', index: previousIndex, href: previousHref }
    history.go(previousIndex - fromIndex)
    return false
  }
  history.replaceState(stateAt(previousIndex), '', previousHref)
  return true
}

function continuePendingPop() {
  const pending = pendingPop
  if (!pending || !pending.restored || pending.decision === null) return
  if (!pending.decision) {
    pendingPop = null
    popPending = false
    return
  }
  if (pending.targetIndex === null || pending.targetIndex === pending.previousIndex) {
    pendingPop = null
    popPending = false
    historyIndex = pending.previousIndex
    currentHref = pending.targetHref
    history.replaceState(stateAt(pending.previousIndex), '', pending.targetHref)
    apply(new URL(pending.targetHref))
    return
  }
  programmaticPop = { kind: 'approved', index: pending.targetIndex, href: pending.targetHref }
  history.go(pending.targetIndex - pending.previousIndex)
}

function onPopState(e: PopStateEvent) {
  const url = new URL(location.href)
  const targetIndex = historyIndexOf(e.state)
  if (programmaticPop && targetIndex === programmaticPop.index && url.href === programmaticPop.href) {
    const action = programmaticPop
    programmaticPop = null
    if (action.kind === 'approved') {
      historyIndex = action.index
      currentHref = action.href
      pendingPop = null
      popPending = false
      apply(url)
    } else if (pendingPop) {
      pendingPop.restored = true
      continuePendingPop()
    }
    return
  }
  if (popPending) {
    restoreTraversal(targetIndex, historyIndex, currentHref)
    return
  }

  const previousIndex = historyIndex
  const previousHref = currentHref
  const allowed = beforeNavigate?.() ?? true
  const commit = () => {
    historyIndex = targetIndex ?? previousIndex
    currentHref = url.href
    apply(url)
  }
  const refuse = () => restoreTraversal(targetIndex, previousIndex, previousHref)

  if (allowed instanceof Promise) {
    popPending = true
    const pending = {
      targetIndex,
      targetHref: url.href,
      previousIndex,
      previousHref,
      restored: false,
      decision: null as boolean | null,
    }
    pendingPop = pending
    pending.restored = refuse() // put the address bar back while the translated confirmation is open
    void allowed.then((ok) => {
      if (pendingPop !== pending) return
      pending.decision = ok
      continuePendingPop()
    }).catch(() => {
      if (pendingPop === pending) {
        pendingPop = null
        popPending = false
      }
    })
  } else if (allowed) {
    commit()
  } else {
    refuse()
  }
}

export function startRouter() {
  if (routerStarted) return
  routerStarted = true
  const savedIndex = historyIndexOf(history.state)
  if (savedIndex === null) history.replaceState(stateAt(historyIndex), '', location.href)
  else historyIndex = savedIndex
  currentHref = location.href
  window.addEventListener('popstate', onPopState)
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    const a = (e.target as Element).closest('a')
    if (!a || a.target || a.hasAttribute('download') || a.dataset.native !== undefined) return
    const href = a.getAttribute('href')
    if (!href || href.startsWith('#') || href.startsWith('/api/') || href.startsWith('/lite')) return
    const url = new URL(href, location.href)
    if (url.origin !== location.origin) return
    e.preventDefault()
    navigate(url.pathname + url.search)
  })
}

/** Matches '/tools/:id' against a path; returns params or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean)
  const s = path.split('/').filter(Boolean)
  if (p.length !== s.length) return null
  const out: Record<string, string> = {}
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(':')) out[p[i].slice(1)] = decodeURIComponent(s[i])
    else if (p[i] !== s[i]) return null
  }
  return out
}
