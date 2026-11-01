import { t, has, toolName } from './i18n.svelte'
import { prefs, setPrefs } from './prefs.svelte'

export type Attention = {
  severity: 'danger' | 'warn' | 'info'
  key: string
  args?: Record<string, unknown>
  route: string
}

// Read status is intentionally separate from the live condition. Reading an item only clears its
// badge; unresolved problems remain visible in Home and in the notification menu. Volatile details
// such as counts and budget percentages do not change an issue's identity. Resolved fingerprints
// are pruned, so the same condition is unread again if it later returns.
const readTTL = 7 * 24 * 60 * 60 * 1000
const maxRead = 200
const identityArgKeys = new Set(['channel_id', 'guild_id', 'name', 'provider', 'safety', 'tool'])

function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(',')}]`
  if (value && typeof value === 'object') {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${JSON.stringify(k)}:${stable(v)}`)
      .join(',')}}`
  }
  return JSON.stringify(value) ?? 'null'
}

export function attentionFingerprint(a: Attention): string {
  const identityArgs = Object.fromEntries(
    Object.entries(a.args ?? {}).filter(([key]) => identityArgKeys.has(key)),
  )
  const source = `${a.severity}|${a.key}|${a.route}|${stable(identityArgs)}`
  let hash = 2166136261
  for (let i = 0; i < source.length; i++) {
    hash ^= source.charCodeAt(i)
    hash = Math.imul(hash, 16777619)
  }
  return `a-${(hash >>> 0).toString(36)}`
}

function recent(at: string | undefined, now = Date.now()): boolean {
  const time = Date.parse(at ?? '')
  return Number.isFinite(time) && now - time < readTTL
}

export function isAttentionRead(a: Attention): boolean {
  return recent(prefs.attention_read?.[attentionFingerprint(a)])
}

function compact(next: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(next)
      .filter(([, at]) => recent(at))
      .sort((a, b) => Date.parse(b[1]) - Date.parse(a[1]))
      .slice(0, maxRead),
  )
}

export function markAttentionRead(a: Attention) {
  setPrefs({ attention_read: compact({ ...(prefs.attention_read ?? {}), [attentionFingerprint(a)]: new Date().toISOString() }) })
}

export function markAllAttentionRead(items: Attention[]) {
  const now = new Date().toISOString()
  const next = { ...(prefs.attention_read ?? {}) }
  for (const item of items) next[attentionFingerprint(item)] = now
  setPrefs({ attention_read: compact(next) })
}

/** Prunes resolved and expired items without writing when nothing changed. */
export function syncAttentionReads(items: Attention[]) {
  const current = new Set(items.map(attentionFingerprint))
  const before = prefs.attention_read ?? {}
  const next = Object.fromEntries(Object.entries(before).filter(([id, at]) => current.has(id) && recent(at)))
  if (Object.keys(before).length !== Object.keys(next).length) setPrefs({ attention_read: next })
}

// attentionText is an item's sentence with IDs made readable: tool IDs become tool names, provider IDs
// ("openmeteo / kma") the services' names from the Connections page, and model roles their names.
export function attentionText(a: Attention): string {
  const args: Record<string, unknown> = { ...(a.args ?? {}) }
  if (typeof args.tool === 'string') args.tool = toolName(args.tool)
  if (typeof args.provider === 'string')
    args.provider = args.provider
      .split('/')
      .map((p) => p.trim())
      .map((p) => (has(`conn.provider.${p}`) ? t(`conn.provider.${p}`) : p))
      .join(' / ')
  if (typeof args.roles === 'string')
    args.roles = args.roles
      .split(',')
      .map((r) => r.trim())
      .map((r) => (has(`role.${r}`) ? t(`role.${r}`) : r))
      .join(', ')
  return t(`attention.${a.key}`, args)
}
