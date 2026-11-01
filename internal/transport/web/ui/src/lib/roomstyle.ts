// Room style (experimental, plan §5.6) as the console shows it. Pure helpers: they pick which version
// matters and turn the counted numbers into plain values, so the card stays thin and the rules are
// testable without a browser.

export type StyleStats = {
  messages?: number
  people?: number
  median_runes?: number
  p90_runes?: number
  laughs?: string[]
  laugh_share?: number
  emoji_share?: number
  question_share?: number
  languages?: Record<string, number>
}
export type StyleItem = { kind: string; text: string; evidence?: number }
export type StyleState = 'pending' | 'approved' | 'rejected' | 'superseded' | 'stale'
export type StyleVersion = {
  version: number
  state: StyleState | string
  stats?: StyleStats
  items?: StyleItem[]
  note?: string
  messages?: number
  cost_usd?: number
  created_at?: string
  decided_at?: string
}

/** The kinds a model picks, in the order a reader looks for them. */
export const KIND_ORDER = ['slang', 'nickname', 'joke', 'avoid'] as const
/** The server clips a preview message to this many characters. */
export const PREVIEW_MAX = 400
/** History shows at most this many older versions. */
export const HISTORY_MAX = 5

export type VersionView = {
  /** the proposal waiting for a decision */
  pending?: StyleVersion
  /** the version the room uses once the setting is on */
  approved?: StyleVersion
  /** older stale and superseded versions, newest first */
  history: StyleVersion[]
  /** the newest version lost a source message and no approved one is left: a new analysis is needed */
  stale: boolean
}

/** Picks the versions that matter from the show output (newest first, as the server sends them). */
export function pickVersions(versions: StyleVersion[] | null | undefined): VersionView {
  const list = Array.isArray(versions) ? versions : []
  const pending = list.find((v) => v.state === 'pending')
  const approved = list.find((v) => v.state === 'approved')
  const history = list.filter((v) => v.state === 'stale' || v.state === 'superseded').slice(0, HISTORY_MAX)
  // the server adds the approved version at the end when newer ones pushed it out of the newest five
  return { pending, approved, history, stale: list[0]?.state === 'stale' && !approved }
}

const share = (n: unknown) => (typeof n === 'number' && Number.isFinite(n) ? Math.min(1, Math.max(0, n)) : 0)
const count = (n: unknown) => (typeof n === 'number' && Number.isFinite(n) && n > 0 ? Math.round(n) : 0)

export type StatsView = {
  messages: number
  people: number
  median: number
  long: number
  laughs: string[]
  laughShare: number
  emojiShare: number
  questionShare: number
  /** share per language tag, as a percent, largest first */
  languages: { tag: string; pct: number }[]
}

/** The counted numbers as plain values; null when nothing was counted. */
export function statsView(s: StyleStats | null | undefined): StatsView | null {
  if (!s || !count(s.messages)) return null
  const languages = Object.entries(s.languages ?? {})
    .map(([tag, pct]) => ({ tag, pct: count(pct) }))
    .filter((l) => l.tag && l.pct > 0)
    .sort((a, b) => b.pct - a.pct || a.tag.localeCompare(b.tag))
  return {
    messages: count(s.messages),
    people: count(s.people),
    median: count(s.median_runes),
    long: count(s.p90_runes),
    laughs: (s.laughs ?? []).filter(Boolean).slice(0, 4),
    laughShare: share(s.laugh_share),
    emojiShare: share(s.emoji_share),
    questionShare: share(s.question_share),
    languages,
  }
}

export type ItemGroup = { kind: string; items: { text: string; evidence: number }[] }

/** The model-picked items grouped by kind, in reading order; empty groups and unknown kinds are left out. */
export function groupItems(items: StyleItem[] | null | undefined): ItemGroup[] {
  const list = Array.isArray(items) ? items : []
  return KIND_ORDER.map((kind) => ({
    kind,
    items: list.filter((i) => i.kind === kind && i.text).map((i) => ({ text: i.text, evidence: count(i.evidence) })),
  })).filter((g) => g.items.length)
}

export type DryRunView = { enough: boolean; min: number; messages: number; stats: StatsView | null }

/** The free count before a real analysis: whether the room has enough messages. */
export function dryRunView(out: { stats?: StyleStats; enough?: boolean; min_messages?: number } | null | undefined): DryRunView | null {
  if (!out) return null
  const stats = statsView(out.stats)
  return { enough: out.enough === true, min: count(out.min_messages), messages: count(out.stats?.messages), stats }
}

/** The preview message the server will take: trimmed and clipped; '' when there is nothing to send. */
export function previewMessage(raw: string | null | undefined): string {
  return (raw ?? '').trim().slice(0, PREVIEW_MAX)
}
