/** How a repeating reminder reads in the console. The server stores the rule; the words come from the
 * console's own catalogs so they follow the language chosen here. */
export interface RepeatRule {
  kind: 'daily' | 'weekly' | 'monthly' | 'hours'
  time?: string
  weekdays?: number[]
  day?: number
  every?: number
}

export interface RepeatingReminder {
  timezone?: string // the zone the rule's clock time and the end date are read in
  repeat?: RepeatRule | null
  repeat_until?: string
  repeat_max?: number
}

type Tr = (key: string, params?: Record<string, string | number>) => string

/** The rule as a phrase ("Every day at 08:00 (Asia/Seoul) · 10 times in all"); '' for a reminder that does
 * not repeat. The clock time is the reminder's own zone, so the zone is named next to it. until formats the
 * end moment as a date in that zone (the console's date formatter). */
export function repeatText(r: RepeatingReminder, tr: Tr, until: (iso: string, timezone?: string) => string): string {
  const rule = r.repeat
  if (!rule) return ''
  const time = rule.time ? (r.timezone ? `${rule.time} (${r.timezone})` : rule.time) : ''
  const parts: string[] = []
  if (rule.kind === 'daily') parts.push(tr('rem.repeat.daily', { time }))
  else if (rule.kind === 'weekly') parts.push(tr('rem.repeat.weekly', { days: (rule.weekdays ?? []).map((d) => tr(`weekday.${d}`)).join(', '), time }))
  else if (rule.kind === 'monthly') parts.push(tr((rule.day ?? 0) > 28 ? 'rem.repeat.monthly_last' : 'rem.repeat.monthly', { day: rule.day ?? 0, time }))
  else if (rule.kind === 'hours') parts.push(tr('rem.repeat.hours', { n: rule.every ?? 0 }))
  if (r.repeat_until) parts.push(tr('rem.repeat.until', { date: until(r.repeat_until, r.timezone) }))
  if (r.repeat_max) parts.push(tr('rem.repeat.count', { n: r.repeat_max }))
  return parts.join(' · ')
}
