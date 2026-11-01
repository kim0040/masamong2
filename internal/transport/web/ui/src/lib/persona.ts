import { t, has, localeName } from './i18n.svelte'

/** A persona's display name: built-ins are named in the console language, the operator's own as typed. */
export function personaTitle(p: { id: string; label?: string; builtin?: boolean } | undefined, fallback = ''): string {
  if (!p) return fallback
  if (p.builtin && has(`persona.name.${p.id}`)) return t(`persona.name.${p.id}`)
  return p.label || p.id
}

/** Persona choices for a room, grouped by language with the room's language first (a flat list of 20+
 * names in five languages is hard to scan); hidden ones are left out (the current choice stays visible
 * even when hidden). */
export function personaOptionsFor(personas: any[], lang: string, current = ''): { value: string; label: string; group?: string }[] {
  const all = personas.filter((p) => !p.hidden || p.id === current)
  return all
    .map((p, i) => ({ p, i }))
    .sort((a, b) => (a.p.locale === lang ? 0 : 1) - (b.p.locale === lang ? 0 : 1) || a.i - b.i)
    .map(({ p }) => ({ value: p.id, label: personaTitle(p), group: p.locale ? localeName(p.locale) : undefined }))
}

/** The persona a room of this language uses when it names none, mirroring the server
 * (config.DefaultPersonaCandidates + application.defaultPersonaFor): warm_<tag> when the language has one
 * and the polite default is on, then friendly_<tag>, friendly_en, friendly_ko; each candidate's _v2 first.
 * Only built-in personas are candidates (the server picks from config.BuiltinPersonas). Hiding a voice
 * skips its _v2 as well; when every candidate is hidden the first built-in one is used, and friendly_ko
 * when there is none. `all` is the persona list the server returned. */
export function defaultPersonaId(all: { id: string; hidden?: boolean; builtin?: boolean }[], lang: string, polite: boolean): string {
  const key = (lang || 'en').toLowerCase().replace(/-/g, '_')
  const byId = new Map(all.filter((p) => p.builtin).map((p) => [p.id, p]))
  const ids = [...(polite && byId.has(`warm_${key}`) ? [`warm_${key}`] : []), `friendly_${key}`, 'friendly_en', 'friendly_ko']
  const order = ids.flatMap((id) => (byId.has(`${id}_v2`) ? [`${id}_v2`, id] : [id]))
  let first = ''
  for (const id of new Set(order)) {
    const p = byId.get(id)
    if (!p) continue
    first ||= id
    if (p.hidden || byId.get(id.replace(/_v2$/, ''))?.hidden) continue
    return id
  }
  return first || 'friendly_ko'
}

// a short code of a name, for names with no Latin letters
function nameCode(label: string): string {
  let h = 2166136261 // FNV-1a
  for (const c of label.trim()) h = Math.imul(h ^ c.codePointAt(0)!, 16777619)
  return (h >>> 0).toString(36).slice(0, 6)
}

/** The ID a new persona gets from its name (it stays editable): lowercase Latin letters and digits, or
 * persona_<code> for a name with none (한국어, 日本語), so two such personas do not become persona and
 * persona_2; a taken ID gets a number. */
export function personaId(label: string, taken: Set<string>): string {
  let base = label.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 30)
  if (!/^[a-z]/.test(base) || base.length < 2) base = ['persona', base || (label.trim() ? nameCode(label) : '')].filter(Boolean).join('_')
  let id = base
  for (let i = 2; taken.has(id); i++) id = `${base}_${i}`
  return id
}
