// Field-level input checks shared by the settings pages: a bad value is refused with an inline
// error instead of being rounded or clamped into a different setting.

export type RetentionDays = { kind: 'unset' } | { kind: 'set'; days: number } | { kind: 'invalid' }

/** What a retention days field means: empty or exactly 0 unsets (the default applies), -1 keeps
 * forever, any other whole number of days is stored. Fractions and values below -1 are refused —
 * before, they were silently rounded to unset or clamped to forever. */
export function parseRetentionDays(raw: string): RetentionDays {
  const s = raw.trim()
  if (s === '') return { kind: 'unset' }
  if (!/^-?\d+$/.test(s)) return { kind: 'invalid' }
  const n = Number(s)
  if (n === 0) return { kind: 'unset' }
  if (n < -1) return { kind: 'invalid' }
  return { kind: 'set', days: n }
}

/** true for a name the platform's clock accepts, so a typo is refused instead of silently falling
 * back to UTC in the scheduler. */
export function validTimeZone(v: string): boolean {
  // an offset such as "+09:00" passes Intl in some runtimes but is not a zone name the server loads
  if (/^[+-]/.test(v.trim())) return false
  try {
    new Intl.DateTimeFormat('en', { timeZone: v })
    return true
  } catch {
    return false
  }
}
