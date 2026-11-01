// The decisions behind the DM conversation card (config `dm`), kept apart from the component so they can
// be tested without a browser. The config stores 0 for "the default", -1 for "no limit" and 1..max for a
// number; the card shows those as three choices and a number box.
import type { Op } from './config.svelte'

export type DMAllow = 'members' | 'operators' | 'off'
export type LimitMode = 'default' | 'custom' | 'none'
// n is what the number box holds: a string when set from the config, a number (or null when empty) once typed in
export type Limit = { mode: LimitMode; n: string | number | null }
export type DMDraft = { allow: DMAllow; user: Limit; day: Limit }
export type DMRaw = { allow?: string; per_user_per_day?: number; per_day?: number } | undefined | null

export const DM_ALLOW: DMAllow[] = ['members', 'operators', 'off']
/** What an unset limit means (the code's defaults) and the most the server accepts. */
export const DM_USER = { default: 30, max: 10000 }
export const DM_DAY = { default: 100, max: 100000 }

const bound = { user: DM_USER, day: DM_DAY }

function limitFrom(v: unknown): Limit {
  const n = typeof v === 'number' && Number.isFinite(v) ? Math.trunc(v) : 0
  if (n === -1) return { mode: 'none', n: '' }
  if (n === 0) return { mode: 'default', n: '' }
  return { mode: 'custom', n: String(n) }
}

/** The form's starting point from the config document's `dm` (raw values: empty means the default). */
export function dmDraft(raw: DMRaw): DMDraft {
  const allow = DM_ALLOW.includes(raw?.allow as DMAllow) ? (raw!.allow as DMAllow) : 'members'
  return { allow, user: limitFrom(raw?.per_user_per_day), day: limitFrom(raw?.per_day) }
}

/** The number a limit stands for once saved: its default, -1 for no limit, or what was typed (NaN if not a whole number). */
export function limitValue(l: Limit): number {
  if (l.mode === 'none') return -1
  if (l.mode === 'default') return 0
  const s = String(l.n ?? '').trim()
  return /^\d+$/.test(s) ? Number(s) : NaN
}

/** Effective count behind a limit: the default when unset, Infinity when there is no limit. */
export function effective(which: 'user' | 'day', l: Limit): number {
  const v = limitValue(l)
  if (v === 0) return bound[which].default
  if (v === -1) return Infinity
  return v
}

/** Client-side check of the typed numbers (the server checks again). A key per field that is wrong. */
export function dmErrors(d: DMDraft): { user?: 'range'; day?: 'range' } {
  const bad = (which: 'user' | 'day', l: Limit) => {
    if (l.mode !== 'custom') return false
    const v = limitValue(l)
    return !(Number.isInteger(v) && v >= 1 && v <= bound[which].max)
  }
  return { user: bad('user', d.user) ? 'range' : undefined, day: bad('day', d.day) ? 'range' : undefined }
}

/** The config edits that take the stored `dm` to the draft: only what changed, "default" removes the key. */
export function dmOps(d: DMDraft, raw: DMRaw): Op[] {
  const from = dmDraft(raw)
  const ops: Op[] = []
  if (d.allow !== from.allow) ops.push({ path: ['dm', 'allow'], value: d.allow })
  for (const [which, key] of [['user', 'per_user_per_day'], ['day', 'per_day']] as const) {
    const now = limitValue(d[which])
    if (now === limitValue(from[which])) continue
    ops.push(now === 0 ? { path: ['dm', key], op: 'unset' } : { path: ['dm', key], value: now })
  }
  return ops
}

/** One person may use more than all DMs together: the all-DM limit is then the one that runs out first. */
export function dmOrderWarning(d: DMDraft): boolean {
  const u = effective('user', d.user)
  const a = effective('day', d.day)
  return Number.isFinite(u) && Number.isFinite(a) && u > a
}
