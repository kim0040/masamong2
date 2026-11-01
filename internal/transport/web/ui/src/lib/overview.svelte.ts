// The dashboard snapshot is shared: the top bar (bell, budget, pause) and Home read the same copy.
import { call, canLoadProtectedActions, session } from './api.svelte'
import { onLive } from './live.svelte'

export const overview = $state<{ data: any; at: number; error?: string }>({ data: null, at: 0 })
let inflight: Promise<void> | null = null
let started = false

export function refreshOverview(): Promise<void> {
  if (!canLoadProtectedActions(session.value)) return Promise.resolve()
  if (inflight) return inflight
  inflight = (async () => {
    const r = await call('dashboard.overview')
    if (r.ok) {
      overview.data = r.output
      overview.at = Date.now()
      overview.error = undefined
    } else overview.error = r.error?.message
    inflight = null
  })()
  return inflight
}

/** The snapshot as a LoadState source: what Home shows while it is missing. */
export const overviewSource = {
  get data() {
    return overview.data ?? undefined
  },
  get error() {
    return overview.error ? { message: overview.error } : undefined
  },
  refresh: refreshOverview,
}

export function startOverview() {
  if (started || !canLoadProtectedActions(session.value)) return
  started = true
  void refreshOverview()
  onLive(['config', 'job', 'turn', 'runtime', 'audit', 'provider', 'discord'], () => void refreshOverview())
  setInterval(() => document.visibilityState === 'visible' && refreshOverview(), 30000)
}
