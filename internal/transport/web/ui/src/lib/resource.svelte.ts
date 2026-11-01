// Cached Action reads (stale-while-revalidate): returning to a page shows the last data instantly and
// refreshes in the background, so navigation never flashes an empty screen.
import { call, canLoadProtectedActions, session, type ActionError } from './api.svelte'
import { onLive } from './live.svelte'

type Entry = { data: any; at: number }
const cache = new Map<string, Entry>()

// Resources on screen whose last refresh failed while they still show older data. Banners.svelte reads
// this to say so once, for every page, instead of each page drawing its own strip.
const staleOnes = new Set<{ refresh: () => Promise<void> }>()
export const staleResources = $state({ count: 0 })
export const refreshStale = () => Promise.all([...staleOnes].map((r) => r.refresh()))

export type Resource<T> = {
  readonly data: T | undefined
  readonly error: ActionError | undefined
  readonly loading: boolean
  readonly stale: boolean
  refresh: () => Promise<void>
}

/** Use inside a component: const r = resource('tools.list', () => ({}), { live: ['config'] }) */
export function resource<T = any>(action: string, input: () => Record<string, unknown> = () => ({}), opts: { live?: string[]; poll?: number } = {}): Resource<T> {
  const s = $state<{ data: T | undefined; error: ActionError | undefined; loading: boolean; stale: boolean }>({
    data: undefined,
    error: undefined,
    loading: true,
    stale: false,
  })
  let key = ''
  let seq = 0
  async function load() {
    if (!canLoadProtectedActions(session.value)) {
      s.loading = false
      s.error = undefined
      return
    }
    const inp = input()
    key = action + JSON.stringify(inp)
    const hit = cache.get(key)
    if (hit) {
      s.data = hit.data
      s.loading = false
    } else if (s.data === undefined) s.loading = true
    const my = ++seq
    const r = await call<T>(action, inp)
    if (my !== seq) return
    s.loading = false
    if (r.ok) {
      s.data = r.output
      s.error = undefined
      s.stale = false
      cache.set(key, { data: r.output, at: Date.now() })
    } else {
      s.error = r.error
      s.stale = s.data !== undefined
    }
  }
  $effect(() => {
    input() // track reactive inputs
    void load()
  })
  $effect(() => {
    if (!opts.live?.length) return
    return onLive(opts.live, () => void load())
  })
  $effect(() => {
    if (!opts.poll) return
    const id = setInterval(() => document.visibilityState === 'visible' && load(), opts.poll)
    return () => clearInterval(id)
  })
  const self = {
    get data() {
      return s.data
    },
    get error() {
      return s.error
    },
    get loading() {
      return s.loading
    },
    get stale() {
      return s.stale
    },
    refresh: load,
  }
  $effect(() => {
    if (!s.stale) return
    staleOnes.add(self)
    staleResources.count = staleOnes.size
    return () => {
      staleOnes.delete(self)
      staleResources.count = staleOnes.size
    }
  })
  return self
}

export function invalidate(prefix: string) {
  for (const k of cache.keys()) if (k.startsWith(prefix)) cache.delete(k)
}
