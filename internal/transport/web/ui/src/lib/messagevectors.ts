// Turning on message-level vectors (memory.message_vectors) embeds every eligible message of the
// indexed rooms on its own, so the console shows what that costs before it saves the setting. The
// estimate comes from memory.blocks.list with message_vectors_estimate: the engine counts the messages
// that still need a vector, the ones the embedding cache already holds and prices the rest. Messages that
// are not in a prepared leaf yet cannot be priced; the engine reports how many there are (an upper bound)
// and the confirm says so instead of showing zeros as if they were exact.

/** The switch as the server applies it: memory.message_vectors is on while the field is absent (the
 * default since the measurement), and only an explicit false is off. */
export function messageVectorsOn(memory?: { message_vectors?: boolean | null } | null): boolean {
  return memory?.message_vectors !== false
}

/** What turning the setting on would do; missing numbers never reach the confirm as anything but 0. */
export type MessageVectorAsk = {
  messages: number
  tokens: number
  usd: number
  seconds: number
	dimensions: number
	diskBytes: number
  fallbackPrice: boolean
  /** upper bound of the eligible messages that sit in no prepared leaf yet: not part of the figures above */
  unprepared: number
}

/** The estimate in a memory.blocks.list answer, or null when the answer carried none. */
export function messageVectorAsk(output?: { memory?: { message_vectors?: { estimate?: Record<string, unknown> } } }): MessageVectorAsk | null {
  const e = output?.memory?.message_vectors?.estimate
  if (!e || typeof e !== 'object') return null
  const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0)
	return { messages: n(e.messages), tokens: n(e.tokens), usd: n(e.usd), seconds: n(e.seconds), dimensions: n(e.dimensions), diskBytes: n(e.disk_bytes), fallbackPrice: e.price_basis === 'fallback', unprepared: n(e.unprepared) }
}

/** How the confirm describes the estimate: the figures are exact, exact for the prepared part with a note
 * about more messages, or nothing can be priced before the blocks are prepared. */
export function messageVectorAskMode(a: MessageVectorAsk): 'exact' | 'partial' | 'unpriced' {
  if (a.unprepared <= 0) return 'exact'
  return a.messages > 0 ? 'partial' : 'unpriced'
}

type ActionLike = { ok: boolean; output?: any; error?: { message?: string } | null }

/** What happened when the setting was turned on: saved or not, and per block started / already running / failed. */
export type EnableOutcome = { saved: boolean; saveError?: string; started: number; running: number; failed: number; firstError?: string }

/** Saves the setting and, only if that worked, prepares every block that can be prepared. A save that
 * fails (a conflicting edit, a storage error) stops here: no paid work is started for a setting that is
 * still off. Blocks that are already preparing, or that report an already running job, are counted
 * apart from the ones that failed to start. */
export async function enableMessageVectors(opts: {
  save: () => Promise<ActionLike>
  blocks: any[]
  preparing: (b: any) => boolean
  prepare: (b: any) => Promise<ActionLike>
}): Promise<EnableOutcome> {
  const out: EnableOutcome = { saved: false, started: 0, running: 0, failed: 0 }
  const saved = await opts.save()
  if (!saved.ok) {
    out.saveError = saved.error?.message
    return out
  }
  out.saved = true
  for (const b of opts.blocks) {
    if (opts.preparing(b)) {
      out.running++
      continue
    }
    let r: ActionLike
    try {
      r = await opts.prepare(b)
    } catch (e) {
      r = { ok: false, error: { message: e instanceof Error ? e.message : String(e) } }
    }
    if (!r.ok) {
      out.failed++
      out.firstError ??= r.error?.message
    } else if (r.output?.already_running) out.running++
    else out.started++
  }
  return out
}
