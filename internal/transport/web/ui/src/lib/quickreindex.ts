// The decisions behind the Block page's "update a little right now" flow (memory.reindex), kept apart from
// the component so they can be tested without a browser. They read an IndexReport: the dry run's answer
// to "what would a pass do" and the real run's answer to "what did it do".

/** Pieces (leaves, message vectors) one quick pass may touch. The engine applies it to leaf work and to
 * message-vector work separately and cuts the rest off, so a dry run that reports this many of either may
 * have more waiting: it is treated as "too big", never as "done after this click". */
export const QUICK_LIMIT = 200

/** Longest estimated run the quick flow starts. The request holds the page's connection for the whole pass
 * and nothing on the server bounds that wait (no write timeout, no deadline on the Action), so on a 1-core
 * box with a slow link a long pass is indistinguishable from a hang and nothing can cancel it. A minute is
 * about what a person waits at a button without reloading; the engine's default rates put 20 summaries
 * (3 s each) at that mark and 200 at ten minutes. Anything longer belongs to the background job, which
 * shows progress and can be cancelled. */
export const QUICK_MAX_SECONDS = 60

export type QuickWork = {
  /** leaves that still need a vector, a summary or a summary vector */
  leaves: number
  /** of those: vectors to embed, vectors reused from the cache, summaries to write */
  embeds: number
  cached: number
  summaries: number
  /** message vectors to embed (memory.message_vectors) */
  messages: number
  /** leaves the dry run's resync added, changed or removed in the selected rooms */
  synced: number
  seconds: number
  usd: number
  /** how many selected rooms could not be resynced */
  failedRooms: number
}

const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)

export function quickWork(report: any): QuickWork {
  const est = report?.estimate ?? {}
  const synced = ((report?.rooms ?? []) as any[]).reduce((a, r) => a + n(r?.new) + n(r?.changed) + n(r?.removed), 0)
  return {
    leaves: n(report?.pending),
    embeds: n(est.leaf_embeds),
    cached: n(est.cached_leaves),
    summaries: n(est.summaries),
    messages: n(est.message_embeds),
    synced,
    seconds: n(est.seconds),
    usd: n(est.total_usd),
    failedRooms: Object.keys(report?.failed_rooms ?? {}).length,
  }
}

/** Whether a pass would do anything. The estimate alone misses work: a leaf whose vector is cached or that
 * only lacks a summary vector is in `pending` but not in leaf_embeds, and message vectors are counted
 * separately from leaves. */
export function indexHasWork(report: any): boolean {
  const w = quickWork(report)
  return w.leaves > 0 || w.messages > 0 || w.embeds > 0 || w.summaries > 0
}

/** nothing: no work. failed: a room could not be resynced, so the numbers are incomplete. big: more than
 * the quick flow takes on (see QUICK_LIMIT, QUICK_MAX_SECONDS); also what a resync of this size means:
 * a large unsynced import is cut into leaves before any limit applies, and the loaded index is rebuilt
 * afterwards. ok: small enough to run in the request. */
export type QuickVerdict = 'nothing' | 'failed' | 'big' | 'ok'

export function quickVerdict(report: any): QuickVerdict {
  const w = quickWork(report)
  if (w.failedRooms > 0) return 'failed'
  if (!indexHasWork(report)) return 'nothing'
  if (w.leaves >= QUICK_LIMIT || w.messages >= QUICK_LIMIT || w.synced >= QUICK_LIMIT || w.seconds > QUICK_MAX_SECONDS) return 'big'
  return 'ok'
}

/** How a finished pass ended: rooms that could not be resynced and the stop reason, if any. Either one
 * means the work is not done, so the success toast must not fire. */
export function quickOutcome(report: any): { failedRooms: number; stoppedBy: string; complete: boolean } {
  const failedRooms = Object.keys(report?.failed_rooms ?? {}).length
  const stoppedBy = typeof report?.stopped_by === 'string' ? report.stopped_by : ''
  return { failedRooms, stoppedBy, complete: failedRooms === 0 && stoppedBy === '' }
}
