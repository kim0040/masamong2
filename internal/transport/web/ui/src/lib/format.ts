import { i18n } from './i18n.svelte'

// Intl formats every console language by its tag; a new locale bundle needs no change here.
const loc = () => i18n.lang || 'en'

export function num(n: number | undefined | null, digits = 0): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '–'
  return new Intl.NumberFormat(loc(), { maximumFractionDigits: digits, minimumFractionDigits: 0 }).format(n)
}

export function usd(n: number | string | undefined | null, digits = 2): string {
  const v = typeof n === 'string' ? parseFloat(n) : n
  if (v === undefined || v === null || Number.isNaN(v)) return '–'
  // under 10 cents three significant digits ('$0.014', '$0.00014'), so a cheap model's price is not rounded away
  if (v !== 0 && Math.abs(v) < 0.1) return new Intl.NumberFormat(loc(), { style: 'currency', currency: 'USD', maximumSignificantDigits: 3 }).format(v)
  return new Intl.NumberFormat(loc(), { style: 'currency', currency: 'USD', minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v)
}

export function pct(n: number, digits = 0): string {
  return new Intl.NumberFormat(loc(), { style: 'percent', maximumFractionDigits: digits }).format(n)
}

export function bytes(n: number | undefined): string {
  if (!n && n !== 0) return '–'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${num(v, v < 10 && i > 0 ? 1 : 0)} ${u[i]}`
}

export function ms(n: number | undefined): string {
  if (n === undefined || n === null) return '–'
  if (n < 1000) return `${num(n)} ms`
  return `${num(n / 1000, 1)} s`
}

export function duration(sec: number): string {
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const lang = loc()
  const ko = lang.toLowerCase().split('-')[0] === 'ko'
  if (!ko && lang.toLowerCase().split('-')[0] !== 'en' && Number.isFinite(sec)) {
    try {
      if (Intl.NumberFormat.supportedLocalesOf([lang]).length) {
        const unit = (n: number, name: 'day' | 'hour' | 'minute') =>
          new Intl.NumberFormat(lang, { style: 'unit', unit: name, unitDisplay: 'narrow' }).format(n)
        if (d > 0) return `${unit(d, 'day')} ${unit(h, 'hour')}`
        if (h > 0) return `${unit(h, 'hour')} ${unit(m, 'minute')}`
        return unit(Math.max(m, 0), 'minute')
      }
    } catch { /* Older Intl implementations may not support unit formatting. */ }
  }
  if (d > 0) return ko ? `${d}일 ${h}시간` : `${d}d ${h}h`
  if (h > 0) return ko ? `${h}시간 ${m}분` : `${h}h ${m}m`
  return ko ? `${Math.max(m, 0)}분` : `${Math.max(m, 0)}m`
}

export function relative(iso: string | undefined): string {
  if (!iso) return '–'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return iso
  const s = Math.round((t - Date.now()) / 1000)
  const rtf = new Intl.RelativeTimeFormat(loc(), { numeric: 'auto' })
  const a = Math.abs(s)
  if (a < 45) return rtf.format(s, 'second')
  if (a < 2700) return rtf.format(Math.round(s / 60), 'minute')
  if (a < 79200) return rtf.format(Math.round(s / 3600), 'hour')
  return rtf.format(Math.round(s / 86400), 'day')
}

export function datetime(iso: string | undefined, withSeconds = false): string {
  if (!iso) return '–'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return new Intl.DateTimeFormat(loc(), { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: withSeconds ? '2-digit' : undefined }).format(d)
}

/** The calendar date of a moment as read in the given IANA zone (the browser's zone when none): a series
 * that ends at 23:59 in Seoul ends on that day for everyone, wherever the console is opened. */
export function dateIn(iso: string | undefined, timezone?: string): string {
  if (!iso) return '–'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  try {
    return new Intl.DateTimeFormat(loc(), { year: 'numeric', month: 'short', day: 'numeric', timeZone: timezone || undefined }).format(d)
  } catch {
    return new Intl.DateTimeFormat(loc(), { year: 'numeric', month: 'short', day: 'numeric' }).format(d)
  }
}

export function shortDate(day: string): string {
  const d = new Date(day + 'T00:00:00')
  return new Intl.DateTimeFormat(loc(), { month: 'numeric', day: 'numeric' }).format(d)
}
