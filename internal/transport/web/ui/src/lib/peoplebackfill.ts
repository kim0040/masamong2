// The paid people backfill (memory.people.backfill and its review), as the console shows it. Pure helpers
// for the page: the plan numbers, the run's state and what the operator may press next.

export type Range = { low?: number; high?: number }
export type ModelKey = 'haiku' | 'sonnet'

export type DryRun = {
  guild_id?: string
  rooms?: number
  windows?: number
  sent_windows?: number
  skipped_windows?: number
  pilot_windows?: number
  lines?: number
  merged_lines?: number
  until?: string
  tokens?: { input?: number; output?: number }
  cost_usd?: { pilot?: Range; haiku?: { batch?: Range }; sonnet?: { batch?: Range } }
  budget_usd?: number
  /** spent and left: every run and pilot of the server so far, against the cap (it is one total per server) */
  spent_usd?: number
  left_usd?: number
  within_budget?: { haiku?: boolean; sonnet?: boolean }
  provider_ready?: boolean
  warnings?: string[]
  token?: string
}

export type PilotModel = { model?: string; answered?: number; facts?: number; rejected?: number; reject_rate?: number; refused?: number; failed?: number; skipped?: number; cost_usd?: number }
export type Run = {
  id: string
  state: string
  model?: string
  budget_usd?: number
  /** this run's own spend; the cap is checked against guild_spent_usd, every run of the server */
  spent_usd?: number
  guild_spent_usd?: number
  /** claims this run stored pending (a server that keeps sensitive topics out): not read until checked */
  claims_pending?: number
  stopped_by?: string
  pilot_done?: boolean
  token?: string
  windows?: { queued?: number; submitted?: number; applied?: number; failed?: number; skipped?: number; refused?: number }
  windows_total?: number
  claims?: number
  rejected?: number
  batches_in_flight?: number
  pilot?: { windows?: number; cost_usd?: number; name_links?: number; disagreements?: number; hearsay?: number; review_items?: number; models?: Record<string, PilotModel> }
}

/** The cap the operator typed: a number above 0 and up to MAX_BUDGET_USD. The server refuses more than this
 *  per server in total (owner: 3 dollars by default, up to 6 for Sonnet). */
export const MAX_BUDGET_USD = 6

export function parseBudget(v: string): number | null {
  const n = Number(String(v).trim().replace(',', '.'))
  return Number.isFinite(n) && n > 0 && n <= MAX_BUDGET_USD ? n : null
}

/** Low to high as the console prints it; one number when both ends agree or only one is known. */
export function rangeParts(r: Range | null | undefined): { low: number; high: number } | null {
  if (!r || (r.low === undefined && r.high === undefined)) return null
  const high = r.high ?? r.low ?? 0
  return { low: Math.min(r.low ?? high, high), high }
}

/** The cap is one total per server across its pilots and runs, so what is spent and left is the server's,
 *  not the newest run's: a run after a 2.90 dollar run has 0.10 left of a 3 dollar cap. Null when the
 *  server total is unknown (no cap, or a status without it): the page then shows no figure rather than
 *  the run's own spend, which would look like money still left. */
export function serverSpend(run: Run | null | undefined): { spent: number; cap: number; left: number } | null {
  const cap = run?.budget_usd
  const spent = run?.guild_spent_usd
  if (typeof cap !== 'number' || !(cap > 0) || typeof spent !== 'number' || !Number.isFinite(spent)) return null
  return { spent, cap, left: Math.max(0, cap - spent) }
}

/** The same figures from an estimate: spent and left are already the server's totals there. */
export function planSpend(plan: DryRun | null | undefined): { spent: number; cap: number; left: number } | null {
  const cap = plan?.budget_usd
  const spent = plan?.spent_usd
  if (typeof cap !== 'number' || !(cap > 0) || typeof spent !== 'number' || !Number.isFinite(spent)) return null
  const left = plan?.left_usd
  return { spent, cap, left: Math.max(0, typeof left === 'number' && Number.isFinite(left) ? left : cap - spent) }
}

/** The estimate warns that this server keeps sensitive topics out, which the paid fill does not check:
 *  what it adds waits pending and is not used until something checks it. */
export function pendingWarning(plan: DryRun | null | undefined): boolean {
  const w = plan?.warnings
  return Array.isArray(w) && w.includes('no_sensitive_pending')
}

/** How many claims of the run wait pending (0 when none or unknown). */
export const pendingClaims = (run: Run | null | undefined) => (typeof run?.claims_pending === 'number' && run.claims_pending > 0 ? run.claims_pending : 0)

/** "claude-haiku-5-5" is the haiku run, and so on; anything else is no model. */
export function modelKey(id: string | undefined): ModelKey | '' {
  const m = (id ?? '').toLowerCase()
  return m.includes('haiku') ? 'haiku' : m.includes('sonnet') ? 'sonnet' : ''
}

/** The words of a model id for a row: "claude-haiku-5-5" is "Haiku 5.5". */
export function modelName(id: string | undefined): string {
  const m = /^claude-([a-z]+)-(\d+)-(\d+)$/.exec(id ?? '')
  return m ? `${m[1][0].toUpperCase()}${m[1].slice(1)} ${m[2]}.${m[3]}` : (id ?? '')
}

/** Phase of the newest run: what the panel shows and which button it offers.
 *  none: nothing started; piloting: pilot calls running; piloted: pilot done, review and run possible;
 *  running: batches in flight; stopped: cut short (cap, restart, error); done: all windows settled. */
export type Phase = 'none' | 'piloting' | 'piloted' | 'running' | 'stopped' | 'done'
export function phase(run: Run | null | undefined): Phase {
  switch (run?.state) {
    case 'piloting':
      return 'piloting'
    case 'piloted':
      return 'piloted'
    case 'submitted':
      return 'running'
    case 'stopped':
      return 'stopped'
    case 'done':
      return 'done'
    default:
      return 'none' // no run, or one a newer pilot replaced
  }
}

/** The run is moving without the operator: the page asks again every few seconds. */
export const isBusy = (run: Run | null | undefined, running?: boolean) => !!running || phase(run) === 'piloting' || phase(run) === 'running'

/** Windows settled one way or another, against all of them. */
export function progress(run: Run | null | undefined): { done: number; total: number } {
  const w = run?.windows ?? {}
  const done = (w.applied ?? 0) + (w.skipped ?? 0) + (w.refused ?? 0) + (w.failed ?? 0)
  return { done, total: run?.windows_total ?? done + (w.queued ?? 0) + (w.submitted ?? 0) }
}

/** What the operator may do next with a plan and the newest run. */
export function nextSteps(plan: DryRun | null, run: Run | null | undefined, busy: boolean): { pilot: boolean; run: boolean; resume: boolean; review: boolean } {
  const ph = phase(run)
  const ready = !!plan?.token && plan.provider_ready !== false && !busy
  return {
    pilot: ready && ph !== 'running',
    run: !busy && ph === 'piloted' && !!run?.pilot_done && !!run?.token,
    resume: !busy && ph === 'stopped' && !!run?.pilot_done && !!run?.token,
    review: ph === 'piloted' && !!run?.pilot_done,
  }
}

export type ReviewItem = { part: string; window_id?: string; model?: string; kind?: string; topic?: string; text: string; subject?: string; speakers?: string[]; link?: string; evidence_n?: number }
export const REVIEW_PARTS = ['name', 'disagreement', 'hearsay'] as const

/** The review set in the order the operator reads it: linked by name, models disagreed, heard-from-others sample. */
export function groupReview(items: ReviewItem[] | null | undefined): { part: string; items: ReviewItem[] }[] {
  const list = Array.isArray(items) ? items : []
  return REVIEW_PARTS.map((part) => ({ part, items: list.filter((i) => i.part === part) })).filter((g) => g.items.length > 0)
}
