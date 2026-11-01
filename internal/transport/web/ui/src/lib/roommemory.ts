// Room memory (ADR-17) as the console shows it. Pure helpers: they return translation keys and plain
// values, so the pages stay thin and the rules (budget thresholds, source names, estimate checks) are
// testable without a browser.

export type Budget = { limit_usd?: number; spent_usd?: number }
export type BudgetState = 'unknown' | 'off' | 'ok' | 'state_paused' | 'stopped'

/** The room state pauses at this share of the monthly budget; extraction continues to 100%. */
export const STATE_SHARE = 0.8

export type BudgetView = { state: BudgetState; limit: number; spent: number; remaining: number; pct: number; tone: 'ok' | 'warn' | 'danger' | 'neutral' }

/** Where the month's derived-memory spend stands against its budget (0 = room memory is off). */
export function budgetView(b?: Budget | null): BudgetView {
  if (!b || typeof b.limit_usd !== 'number' || typeof b.spent_usd !== 'number') return { state: 'unknown', limit: 0, spent: 0, remaining: 0, pct: 0, tone: 'neutral' }
  const limit = b.limit_usd
  const spent = Math.max(0, b.spent_usd)
  if (limit <= 0) return { state: 'off', limit: 0, spent, remaining: 0, pct: 0, tone: 'neutral' }
  const pct = Math.min(100, (spent / limit) * 100)
  const remaining = Math.max(0, limit - spent)
  if (spent >= limit) return { state: 'stopped', limit, spent, remaining: 0, pct, tone: 'danger' }
  if (spent / limit >= STATE_SHARE - 1e-9) return { state: 'state_paused', limit, spent, remaining, pct, tone: 'warn' }
  return { state: 'ok', limit, spent, remaining, pct, tone: 'ok' }
}

/** discord | kakao:<block id> from an episode's source. */
export function parseSource(source: string | undefined): { kind: 'discord' | 'kakao' | 'other'; block: string } {
  if (!source) return { kind: 'other', block: '' }
  if (source === 'discord') return { kind: 'discord', block: '' }
  if (source.startsWith('kakao:')) return { kind: 'kakao', block: source.slice('kakao:'.length) }
  return { kind: 'other', block: '' }
}

/** The room picker's value: opaque to the reader, stable for the page. */
export function roomValue(kind: 'discord' | 'kakao', a: string, b = ''): string {
  return kind === 'discord' ? `d\u0000${a}\u0000${b}` : `k\u0000${a}`
}

export type RoomPick = { kind: 'discord'; guild_id: string; channel_id: string } | { kind: 'kakao'; room: string } | null

export function parseRoomValue(v: string | undefined): RoomPick {
  const p = (v ?? '').split('\u0000')
  if (p[0] === 'd' && p[1] && p[2]) return { kind: 'discord', guild_id: p[1], channel_id: p[2] }
  if (p[0] === 'k' && p[1]) return { kind: 'kakao', room: p[1] }
  return null
}

/** The list Action's input for a picked room. */
export function listInput(pick: RoomPick): Record<string, unknown> {
  if (!pick) return { limit: 1 }
  return pick.kind === 'discord' ? { guild_id: pick.guild_id, channel_id: pick.channel_id, limit: 200 } : { room: pick.room, limit: 200 }
}

export type EpisodeFilter = { kind: string; status: string }

export function filterEpisodes<T extends { kind?: string; status?: string }>(list: T[], f: EpisodeFilter): T[] {
  return list.filter((e) => (!f.kind || e.kind === f.kind) && (!f.status || e.status === f.status))
}

/** The distinct values of a field, in a fixed order when known and first-seen order otherwise. */
export function distinct(list: { [k: string]: any }[], field: string, order: string[] = []): string[] {
  const seen = [...new Set(list.map((e) => e[field]).filter(Boolean) as string[])]
  return [...order.filter((v) => seen.includes(v)), ...seen.filter((v) => !order.includes(v))]
}

export const EPISODE_KINDS = ['plan', 'decision', 'event', 'joke', 'fact']
export const EPISODE_STATUSES = ['open', 'decided', 'done', 'dropped', 'expired']

export const statusTone: Record<string, 'ok' | 'warn' | 'danger' | 'info' | 'neutral'> = {
  open: 'info', decided: 'ok', done: 'neutral', dropped: 'neutral', expired: 'warn',
}

/** How many of an episode's items stay once the forgotten ones are gone: nothing (it is deleted whole). */
export function forgetPreview(ep: { items?: number; hidden?: number; participants?: number }) {
  const items = ep.items ?? 0
  const hidden = Math.min(ep.hidden ?? 0, items)
  return { items, hidden, visible: items - hidden }
}

export type Estimate = {
  messages?: number
  segments?: number
  est_input_tokens?: number
  est_output_tokens?: number
  est_cost_usd?: number
  price_known?: boolean
  derived_budget_usd?: number
  derived_spent_usd?: number
}

export type EstimateView = {
  budget: BudgetView
  /** nothing to extract: no conversation of two or more messages */
  empty: boolean
  /** the budget is off or spent: running now would stop at once */
  blocked: boolean
  /** the estimate is more than what is left of the monthly budget */
  over: boolean
  priceKnown: boolean
  cost: number
  /** the cap for this run, in dollars with cents, that covers the estimate */
  defaultCap: string
}

/** What a dry run means for the approve step: whether it fits the month's budget and a sensible cap. */
export function estimateView(e: Estimate | null | undefined): EstimateView {
  const budget = budgetView({ limit_usd: e?.derived_budget_usd, spent_usd: e?.derived_spent_usd })
  const cost = Math.max(0, e?.est_cost_usd ?? 0)
  const priceKnown = !!e?.price_known
  const empty = !e || (e.segments ?? 0) <= 0
  const blocked = budget.state === 'off' || budget.state === 'stopped'
  const over = priceKnown && budget.limit > 0 && cost > budget.remaining
  // the job's own cap defaults to 1.00, or the estimate with 20% margin when it is more
  const cap = Math.max(1, Math.ceil(cost * 1.2 * 100) / 100)
  return { budget, empty, blocked, over, priceKnown, cost, defaultCap: cap.toFixed(2) }
}

/** A cap typed for a run: dollars with up to four decimals, more than 0 and at most 1000. */
export function validCap(v: string): boolean {
  if (!/^\d+(\.\d{1,4})?$/.test(v.trim())) return false
  const n = parseFloat(v)
  return n > 0 && n <= 1000
}

/** What the extract Action's answer means after approval. */
export function jobOutcome(job: { done?: boolean; stopped?: string; episodes?: number } | null | undefined): 'queued' | 'done' | 'stopped_budget' | 'stopped_error' {
  if (!job) return 'queued'
  if (job.done) return 'done'
  if (job.stopped === 'budget') return 'stopped_budget'
  if (job.stopped === 'error') return 'stopped_error'
  return 'queued'
}
