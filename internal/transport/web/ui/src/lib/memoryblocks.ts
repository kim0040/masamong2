// Preparing a memory block (summaries and search vectors for a room's history): ask what it would cost
// (free), then start the background job inside a budget. A block's page and the import wizard each had
// their own copy of both steps, and the list's quick start read the answer with a third.
import { call } from './api.svelte'
import { prepareStart, type PrepareStart } from './prepareresult'

export const BUDGET_MAX_USD = 1000

/** The budget a person typed (USD), or null when it is not a number between 0 and 1000. */
export function parseBudget(text: string): number | null {
  const v = parseFloat(text)
  return Number.isNaN(v) || v < 0 || v > BUDGET_MAX_USD ? null : v
}

export type Estimated = { ok: true; estimate: any } | { ok: false; message: string }

/** The estimate for a room (a dry run). mode "batch" makes the one run carry both prices (total_usd now, batch_usd as a batch). */
export async function estimatePrepare(room: string): Promise<Estimated> {
  const r = await call('memory.blocks.prepare', { source_room: room, dry_run: true, mode: 'batch' })
  return r.ok ? { ok: true, estimate: r.output } : { ok: false, message: r.error?.message ?? '' }
}

export type Started = { ok: true; start: PrepareStart } | { ok: false; message: string }

/** Starts the job for a room. `limit` adds a budget and a mode; without it the server's own defaults apply (the list's quick start). */
export async function startPrepare(room: string, limit?: { budget: number; mode: string }): Promise<Started> {
  const input: Record<string, unknown> = { source_room: room, background: true }
  if (limit) Object.assign(input, { budget_usd: String(limit.budget), mode: limit.mode })
  const r = await call('memory.blocks.prepare', input)
  return r.ok ? { ok: true, start: prepareStart(r.output) } : { ok: false, message: r.error?.message ?? '' }
}
