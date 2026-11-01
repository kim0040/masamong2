import { practice } from './api.svelte'
// Live updates over Server-Sent Events. Pages subscribe to change kinds (config, job, turn, audit,
// runtime, …) and refetch; bursts are coalesced so a busy bot never floods the browser.
// lifecycle: the service said it is stopping or restarting (from this console, the CLI or the menu bar icon),
// so a lost connection is expected rather than a fault.
export const live = $state<{ status: 'connecting' | 'live' | 'offline'; lastEvent: number; revision: number; lifecycle: '' | 'stopping' | 'stopped' | 'restart' }>({
  status: 'connecting',
  lastEvent: 0,
  revision: 0,
  lifecycle: '',
})

type Handler = () => void
const handlers = new Map<string, Set<Handler>>()
const pending = new Set<Handler>()
let flushTimer: ReturnType<typeof setTimeout> | undefined
let es: EventSource | null = null

function schedule(h: Handler) {
  pending.add(h)
  if (flushTimer) return
  flushTimer = setTimeout(() => {
    flushTimer = undefined
    const list = [...pending]
    pending.clear()
    if (document.visibilityState === 'hidden') {
      // catch up when the tab is visible again
      list.forEach((fn) => hiddenQueue.add(fn))
      return
    }
    list.forEach((fn) => fn())
  }, 400)
}
const hiddenQueue = new Set<Handler>()
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') {
    const list = [...hiddenQueue]
    hiddenQueue.clear()
    list.forEach((fn) => fn())
  }
})

export function onLive(kinds: string[], fn: Handler): () => void {
  for (const k of kinds) {
    if (!handlers.has(k)) handlers.set(k, new Set())
    handlers.get(k)!.add(fn)
  }
  return () => kinds.forEach((k) => handlers.get(k)?.delete(fn))
}

export function connectLive() {
  if (es) return
  es = new EventSource(practice.on ? '/api/events?practice=1' : '/api/events')
  es.addEventListener('hello', (e) => {
    live.status = 'live'
    if (live.lifecycle === 'restart') live.lifecycle = ''
    live.lastEvent = Date.now()
    try {
      live.revision = JSON.parse((e as MessageEvent).data).revision
    } catch {}
  })
  es.addEventListener('change', (e) => {
    live.status = 'live'
    live.lastEvent = Date.now()
    try {
      const d = JSON.parse((e as MessageEvent).data)
      if (d.revision) live.revision = d.revision
      if (d.kind === 'runtime' && (d.ref === 'stop' || d.ref === 'restart')) noteLifecycle(d.ref)
      handlers.get(d.kind)?.forEach(schedule)
      handlers.get('*')?.forEach(schedule)
    } catch {}
  })
  es.onerror = () => {
    live.status = es?.readyState === EventSource.CLOSED ? 'offline' : 'connecting'
  }
  es.onopen = () => (live.status = 'live')
}

let healthTimer: ReturnType<typeof setInterval> | undefined

// noteLifecycle records a stop or restart the service announced. After a stop nothing reconnects on its own:
// the event stream closes and the page asks /api/health until someone starts the service again, then reloads.
export function noteLifecycle(l: 'stop' | 'restart') {
  if (l === 'restart') {
    if (live.lifecycle !== '') return
    live.lifecycle = 'restart'
    // a restart that never comes back is an outage again: the offline banner takes over
    setTimeout(() => live.lifecycle === 'restart' && (live.lifecycle = ''), 45000)
    return
  }
  if (healthTimer) return
  live.lifecycle = 'stopping'
  es?.close()
  es = null
  const check = async () => {
    try {
      const r = await fetch('/api/health', { cache: 'no-store' })
      if (r.ok && live.lifecycle === 'stopped') {
        clearInterval(healthTimer)
        location.reload()
      }
    } catch {
      live.lifecycle = 'stopped' // it really went away; the next answer is a new start
      live.status = 'offline'
    }
  }
  healthTimer = setInterval(check, 2000)
  setTimeout(check, 700)
}

export function disconnectLive() {
  es?.close()
  es = null
  live.status = 'offline'
}
