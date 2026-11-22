// The rules of the Connections page that need no screen: which group a service belongs to, whether a role can
// answer, which prices and fallbacks may be saved and as what, and which models a picker lists. The page's
// components draw these answers; node --test checks them.

export type Group = 'model' | 'web' | 'data'
export type ModelCaps = Record<string, { api: string; rerank: boolean; host?: string }>

// limits the page checks before it saves a price or a call time limit
export const PRICE_MAX_USD_PER_MTOK = 1000
export const TIMEOUT_MIN_S = 5
export const TIMEOUT_MAX_S = 600
/** 0 means "the default"; otherwise whole seconds within the limits */
export const validTimeout = (secs: number): boolean => Number.isInteger(secs) && (secs === 0 || (secs >= TIMEOUT_MIN_S && secs <= TIMEOUT_MAX_S))

/** A price as typed (USD per million tokens): blank is 0 (unset); null when it is not a number, below 0 or above the limit. */
export function parseUsd(text: string): number | null {
  const v = text.trim() === '' ? 0 : Number(text)
  return Number.isNaN(v) || v < 0 || v > PRICE_MAX_USD_PER_MTOK ? null : v
}

/** A time limit as typed: blank is 0 (the default). Not checked yet: see validTimeout. */
export const parseSeconds = (text: string): number => (text.trim() === '' ? 0 : Number(text))

// ---- services and roles ----

/** The model roles in the order the table lists them. */
export const ROLES = ['response', 'planner', 'summary', 'embed', 'rerank', 'vision', 'image']
/** Roles that may stay empty (the server's config.OptionalModelRoles): the table offers to clear them. */
export const OPTIONAL_ROLES = ['vision', 'image']

/** The practice mode's offline model is a model provider too, though it needs no key and lists no models. */
export const isPractice = (p: any): boolean => p?.kind === 'practice'

export const groupOf = (p: any, caps: ModelCaps, webKinds: string[]): Group => (caps[p.id] || isPractice(p) ? 'model' : webKinds.includes(p.kind) ? 'web' : 'data')

/** Splits the services by group; each group shows working services as cards and the optional ones still waiting for a key as one line. */
export function splitGroups(providers: any[], group: (p: any) => Group, waiting: (p: any, g: Group) => boolean): Record<Group, { cards: any[]; waiting: any[] }> {
  const out: Record<Group, { cards: any[]; waiting: any[] }> = { model: { cards: [], waiting: [] }, web: { cards: [], waiting: [] }, data: { cards: [], waiting: [] } }
  for (const p of providers) {
    const g = group(p)
    out[g][waiting(p, g) ? 'waiting' : 'cards'].push(p)
  }
  return out
}

export type RoleState = 'unset' | 'ok' | 'no_key' | 'rejected' | 'unknown'

/** Whether a role can answer now: its provider needs a key that was not refused. `data` is providers.list's answer. */
export function roleState(model: { model?: string; provider?: string } | undefined, byId: Record<string, any>, data: any): RoleState {
  if (!model?.model) return 'unset'
  if (!data) return 'unknown'
  if (!byId[model.provider ?? '']?.configured) return 'no_key'
  return data.health?.[model.provider ?? '']?.rejected ? 'rejected' : 'ok'
}

export type Blocked = { id: string; rejected: boolean; roles: string[] }

/** Stopped roles, one entry per provider that holds them up (instead of the same warning on every row). */
export function blockedRoles(roles: string[], state: (role: string) => RoleState, providerOf: (role: string) => string): Blocked[] {
  const by = new Map<string, Blocked>()
  for (const role of roles) {
    const st = state(role)
    if (st !== 'no_key' && st !== 'rejected') continue
    const id = providerOf(role)
    if (!by.has(id)) by.set(id, { id, rejected: st === 'rejected', roles: [] })
    by.get(id)!.roles.push(role)
  }
  return [...by.values()]
}

/** The roles whose model comes from this provider. */
export const rolesUsing = (models: Record<string, any> | undefined, id: string): string[] =>
  Object.entries(models ?? {})
    .filter(([, m]) => m?.provider === id)
    .map(([r]) => r)

// ---- reasoning effort ----

export const EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']
export const chatRole = (role: string): boolean => role === 'planner' || role === 'response'
/** roles that take a reasoning level (the server's config.FallbackRoles plus the picture describer) */
export const effortRole = (role: string): boolean => chatRole(role) || role === 'summary' || role === 'vision'
/** roles whose call has a time limit: the chat roles and the picture describer */
export const timeoutRole = (role: string): boolean => chatRole(role) || role === 'vision'

/** What the model's list says about its reasoning ({reasoning, efforts, thinking_off}; empty: unknown). */
export type Reasoning = { reasoning?: boolean; efforts?: string[] }

/** The effort levels and whether each can be picked: levels the model's list says it lacks stay visible but
 * cannot be picked (a level already saved stays pickable, so the dialog never shows a value it cannot hold). */
export const effortLevels = (r: Reasoning | undefined, current: string): { value: string; disabled: boolean }[] =>
  EFFORTS.map((e) => ({ value: e, disabled: e !== current && (r?.reasoning === false ? e !== 'none' : Array.isArray(r?.efforts) && !r.efforts.includes(e)) }))

/** The label key for a model's reasoning: thinks / does not think / not known (a list that says nothing). */
export const reasonKey = (r: Reasoning | undefined): 'conn.reason_yes' | 'conn.reason_no' | 'conn.reason_unknown' => (r?.reasoning === true ? 'conn.reason_yes' : r?.reasoning === false ? 'conn.reason_no' : 'conn.reason_unknown')

// ---- saving a price and the call limits ----

type Op = { path: string[]; value: unknown } | { path: string[]; op: 'unset' }
const setOrUnset = (path: string[], value: unknown): Op => (value ? { path, value } : { path, op: 'unset' })

/** The config changes for a role's price dialog: a zero or blank price, limit or level removes the setting. */
export function priceOps(role: string, v: { in: number; out: number; cached: number; secs: number; effort: string }): Op[] {
  const base = ['models', role]
  const ops: Op[] = [setOrUnset([...base, 'price_in_per_mtok'], v.in), setOrUnset([...base, 'price_out_per_mtok'], v.out), setOrUnset([...base, 'price_cached_in_per_mtok'], v.cached)]
  if (timeoutRole(role)) ops.push(setOrUnset([...base, 'timeout_seconds'], v.secs))
  if (effortRole(role)) ops.push(setOrUnset([...base, 'effort'], v.effort))
  return ops
}

// ---- fallbacks ----

export const FALLBACK_ROLES = ['response', 'planner', 'summary', 'vision']
export const MAX_FALLBACKS = 3
export type FB = { provider: string; model: string; price_in: string; price_out: string; timeout: string; effort: string }

/** What is wrong with a fallback list, as a message key (and its values) for the screen to word. */
export type FallbackProblem = { key: 'conn.fb_need_model' | 'conn.price_invalid' | 'conn.timeout_invalid' } | { key: 'conn.fb_duplicate'; model: string }

/** The fallbacks a role's dialog saves, in order, or the first problem found. The role's own model may not appear again. */
export function buildFallbacks(rows: FB[], primary: { provider?: string; model?: string } | undefined): { ok: true; list: any[] } | { ok: false; problem: FallbackProblem } {
  const seen = new Set([`${primary?.provider}|${primary?.model}`])
  const list: any[] = []
  for (const r of rows) {
    const model = r.model.trim()
    if (!r.provider || !model) return { ok: false, problem: { key: 'conn.fb_need_model' } }
    if (seen.has(`${r.provider}|${model}`)) return { ok: false, problem: { key: 'conn.fb_duplicate', model } }
    seen.add(`${r.provider}|${model}`)
    const pin = parseUsd(r.price_in)
    const pout = parseUsd(r.price_out)
    if (pin === null || pout === null) return { ok: false, problem: { key: 'conn.price_invalid' } }
    const secs = parseSeconds(r.timeout)
    if (!validTimeout(secs)) return { ok: false, problem: { key: 'conn.timeout_invalid' } }
    const f: any = { provider: r.provider, model }
    if (pin) f.price_in_per_mtok = pin
    if (pout) f.price_out_per_mtok = pout
    if (secs) f.timeout_seconds = secs
    if (r.effort) f.effort = r.effort
    list.push(f)
  }
  return { ok: true, list }
}

/** The config change that stores a role's fallbacks; an empty list removes them. */
export const fallbackOps = (role: string, list: any[]): Op[] => {
  const path = ['models', role, 'fallbacks']
  return [list.length ? { path, value: list } : { path, op: 'unset' }]
}

/** The rows a role's dialog starts with, from the saved fallbacks. */
export const fallbackRows = (saved: any[] | undefined): FB[] =>
  (saved ?? []).map((f) => ({
    provider: f.provider,
    model: f.model,
    price_in: f.price_in_per_mtok ? String(f.price_in_per_mtok) : '',
    price_out: f.price_out_per_mtok ? String(f.price_out_per_mtok) : '',
    timeout: f.timeout_seconds ? String(f.timeout_seconds) : '',
    effort: f.effort ?? '',
  }))

// ---- the model picker ----

/** Chat roles list chat models only (the host may also list speech, video or music models). */
export const roleKind = (role: string): string => (role === 'embed' ? 'embedding' : role === 'image' ? 'image' : role === 'rerank' ? 'rerank' : 'chat')

/** The blended price per million tokens the server's budget ranking uses; null when the list has no price. */
export const blended = (m: any): number | null => (typeof m.in_per_mtok === 'number' ? (3 * m.in_per_mtok + (m.out_per_mtok ?? 0)) / 4 : null)

export type ModelSort = 'list' | 'price_asc' | 'price_desc' | 'ctx'
export const MODEL_LIST_MAX = 80

/** The models the picker shows for a role: of its kind, matching the search, in the chosen order, at most 80. */
export function filterModels(models: any[], role: string, filter: string, sort: ModelSort): any[] {
  let list = models
    .filter((m) => m.kind === 'unknown' || m.kind === roleKind(role) || (role === 'image' && m.image_output))
    .filter((m) => !filter || `${m.id} ${m.name}`.toLowerCase().includes(filter.toLowerCase()))
  // only a model the list says takes no pictures is hidden; one it says nothing about stays, marked
  if (role === 'vision') list = list.filter((m) => m.image_input !== false && !m.image_output)
  if (sort === 'price_asc' || sort === 'price_desc') {
    const dir = sort === 'price_asc' ? 1 : -1
    // models without a price go last either way
    list = [...list].sort((a, b) => {
      const x = blended(a),
        y = blended(b)
      return x === null && y === null ? 0 : x === null ? 1 : y === null ? -1 : (x - y) * dir
    })
  } else if (sort === 'ctx') list = [...list].sort((a, b) => (b.context_length ?? 0) - (a.context_length ?? 0))
  return list.slice(0, MODEL_LIST_MAX)
}
