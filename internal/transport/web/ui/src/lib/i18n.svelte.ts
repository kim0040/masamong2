// Console strings come from locales/ui/<lang>.json via /api/i18n. t() falls back to the key's last
// segment only in development builds; English is merged under every language server-side.
import { setApiLang, setErrorHints } from './api.svelte'

const state = $state<{ lang: string; strings: Record<string, string>; ready: boolean }>({ lang: 'ko', strings: {}, ready: false })

export const i18n = {
  get lang() {
    return state.lang
  },
  get ready() {
    return state.ready
  },
}

export async function loadStrings(lang: string) {
  const res = await fetch(`/api/i18n?lang=${encodeURIComponent(lang)}`, { credentials: 'same-origin' })
  if (!res.ok) throw new Error(`Failed to load console strings (HTTP ${res.status})`)
  const body: unknown = await res.json()
  if (!body || typeof body !== 'object' || !('lang' in body) || typeof body.lang !== 'string' || !body.lang.trim() ||
      !('strings' in body) || !body.strings || typeof body.strings !== 'object' || Array.isArray(body.strings) ||
      !Object.keys(body.strings).length || Object.entries(body.strings).some(([key, value]) =>
        typeof value !== 'string' || (!value.trim() && !(value === '' && key === 'tool.cond.unknown')))) {
    throw new Error('Invalid console strings response')
  }
  state.strings = body.strings as Record<string, string>
  state.lang = body.lang
  state.ready = true
  setApiLang(state.lang)
  setErrorHints((code) => state.strings[`error.${code}`] ?? '')
  document.documentElement.lang = state.lang
  document.documentElement.dir = direction(state.lang)
  document.title = state.strings['app.name'] || 'Masamong'
}

// fetchStrings returns one locale's bundle without switching the console (cached per locale): the
// persona editor uses it to show style templates in the persona's own language.
const bundleCache = new Map<string, Record<string, string>>()
export async function fetchStrings(lang: string): Promise<Record<string, string>> {
  const cached = bundleCache.get(lang)
  if (cached) return cached
  const res = await fetch(`/api/i18n?lang=${encodeURIComponent(lang)}`, { credentials: 'same-origin' })
  if (!res.ok) throw new Error(`Failed to load console strings (HTTP ${res.status})`)
  const body: unknown = await res.json()
  if (!body || typeof body !== 'object' || !('strings' in body) || !body.strings || typeof body.strings !== 'object' || Array.isArray(body.strings)) {
    throw new Error('Invalid console strings response')
  }
  const strings = body.strings as Record<string, string>
  bundleCache.set(lang, strings)
  return strings
}

const rtlLanguages = new Set(['ar', 'dv', 'fa', 'he', 'ku', 'ps', 'ur', 'yi'])

export function direction(lang: string): 'ltr' | 'rtl' {
  return rtlLanguages.has(lang.toLowerCase().split('-')[0]) ? 'rtl' : 'ltr'
}

/** Select a CLDR category, retaining the legacy one/other choice if plural rules are unavailable. */
export function pluralCategory(lang: string, n: number): Intl.LDMLPluralRule {
  try {
    return new Intl.PluralRules(lang).select(n)
  } catch {
    return n === 1 ? 'one' : 'other'
  }
}

/** t(key) or t(key, { n: 3 }). Falls back to .other, then the base string. */
export function t(key: string, args?: Record<string, unknown>): string {
  let text = state.strings[key]
  if (args && typeof args.n === 'number') {
    const baseLang = state.lang.toLowerCase().split('-')[0]
    const category = baseLang === 'en' || baseLang === 'ko' ? (args.n === 1 ? 'one' : 'other') : pluralCategory(state.lang, args.n)
    text = state.strings[`${key}.${category}`] ?? state.strings[`${key}.other`] ?? text
  }
  if (text === undefined) {
    if (import.meta.env.DEV) console.warn('missing string', key)
    return state.ready ? key.split('.').pop()!.replace(/_/g, ' ') : ''
  }
  if (args) text = text.replace(/\{(\w+)\}/g, (_, k) => (args[k] === undefined ? '' : String(args[k])))
  return text
}

export function has(key: string) {
  return key in state.strings
}

/** The language's name in the console language (a bundle may define lang.<tag>; the browser knows the rest). */
function exonym(tag: string): string {
  if (has(`lang.${tag}`)) return t(`lang.${tag}`)
  try {
    return new Intl.DisplayNames([state.lang, 'en'], { type: 'language' }).of(tag) ?? tag
  } catch {
    return tag
  }
}

/** A language's name wherever one is shown on its own (chips, lists, progress): its own name for every
 * language, so "English", "한국어" and "日本語" read alike instead of mixing them with names in the
 * console language ("스페인어"). */
export function localeName(tag: string): string {
  return has(`lang.${tag}`) ? t(`lang.${tag}`) : endonym(tag)
}

/** A language's own name ("한국어", "English", "日本語"): what a speaker looks for in a list. */
export function endonym(tag: string): string {
  try {
    const n = new Intl.DisplayNames([tag], { type: 'language' }).of(tag)
    if (n) return n.charAt(0).toLocaleUpperCase(tag) + n.slice(1)
  } catch {}
  return tag
}

/** The label for a language option: its own name, plus the name in the current language when that
 * differs (so "日本語 · 일본어" is findable both ways). */
export function languageLabel(tag: string): string {
  const own = endonym(tag)
  const here = exonym(tag)
  return here && here !== own && here !== tag ? `${own} · ${here}` : own
}

/** Switches the console language: strings load first, then the choice is saved. */
export async function switchLanguage(tag: string, save: (lang: string) => void) {
  await loadStrings(tag)
  save(state.lang)
}

/** A tool's display name: the translated name for built-ins, the ID for operator-defined tools. */
export function toolName(id: string): string {
  return has(`tool.${id}`) ? t(`tool.${id}`) : id
}

/** A built-in tool's default description, or '' for operator tools (their description is in config). */
export function toolDesc(id: string): string {
  return has(`tool.desc.${id}`) ? t(`tool.desc.${id}`) : ''
}
