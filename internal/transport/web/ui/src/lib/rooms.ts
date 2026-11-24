// How a room is named on screen. Discord gives "___hidden___" for a channel the bot cannot see, and a
// room whose name is not known falls back to its number; neither says what the room is. One helper names
// every room in every picker and list: the channel's own name when there is one, else a plain label with
// the end of its ID, always with the server it belongs to.

export type RoomWords = {
  /** "Channel the bot cannot see …123456" */
  hidden: (tail: string) => string
  /** "Server …123456" */
  server: (tail: string) => string
  /** the group of rooms imported from KakaoTalk */
  kakao: string
}

export type RoomItem = { key: string; label: string; hidden: boolean; server: string; kind: 'discord' | 'kakao'; count: number }

/** The last six characters of an ID: enough to tell two rooms apart, short enough to read. */
export const tail = (id: string): string => (id.length > 6 ? id.slice(-6) : id)

/** A name that names nothing: blank, only digits, or the placeholder Discord sends for a channel the bot cannot see. */
export function placeholderName(name: string | null | undefined): boolean {
  const n = (name ?? '').trim().replace(/^#/, '')
  return n === '' || /^\d+$/.test(n) || /^_+hidden_*$/i.test(n) || /^_{3,}$/.test(n)
}

/** The channel's label: "#name", or the plain "cannot see" label with the ID's tail. */
export function channelLabel(name: string | null | undefined, id: string, w: Pick<RoomWords, 'hidden'>): { label: string; hidden: boolean } {
  if (placeholderName(name)) return { label: w.hidden(tail(id)), hidden: true }
  return { label: `#${(name as string).trim().replace(/^#/, '')}`, hidden: false }
}

/** A server's name, or "Server …tail" when it is only an ID (or a placeholder). */
export function serverLabel(name: string | null | undefined, id: string, w: Pick<RoomWords, 'server'>): string {
  const n = (name ?? '').trim()
  return n && n !== id && !placeholderName(n) ? n : w.server(tail(id))
}

/** "discord:<server>:<channel>" → its two IDs; anything else (kakao:…) is null. */
export function parseRoomKey(key: string): { guild: string; channel: string } | null {
  const m = /^discord:([^:]+):([^:]+)$/.exec(key)
  return m ? { guild: m[1], channel: m[2] } : null
}

type Blocks = { source_room: string; label?: string; platform?: string; message_count?: number; own_channel?: { guild_id: string; channel_id: string; guild_name?: string; name?: string }; channels?: { guild_id: string; channel_id: string; guild_name?: string; name?: string }[] }
type Channels = { guilds?: { id: string; name?: string }[]; channels?: { guild_id: string; id: string; name?: string }[] } | undefined

/** Every room with memory, named: from the block list, with the channel and server names Discord gave. */
export function roomItems(blocks: Blocks[] | undefined, channels: Channels, w: RoomWords): RoomItem[] {
  const guildName = (g: string, hint?: string) => (channels?.guilds ?? []).find((x) => x.id === g)?.name || hint
  const channelName = (g: string, c: string, hint?: string) => (channels?.channels ?? []).find((x) => x.guild_id === g && x.id === c)?.name ?? hint
  const out: RoomItem[] = []
  for (const b of blocks ?? []) {
    const key = b.source_room
    const ids = parseRoomKey(key)
    if (!ids || b.platform === 'kakao') {
      out.push({ key, label: b.label || key, hidden: false, server: w.kakao, kind: 'kakao', count: b.message_count ?? 0 })
      continue
    }
    const own = b.own_channel ?? (b.channels ?? []).find((c) => c.guild_id === ids.guild && c.channel_id === ids.channel)
    const { label, hidden } = channelLabel(channelName(ids.guild, ids.channel, own?.name), ids.channel, w)
    out.push({ key, label, hidden, server: serverLabel(guildName(ids.guild, own?.guild_name), ids.guild, w), kind: 'discord', count: b.message_count ?? 0 })
  }
  return out
}

/** The name of a room key (a search hit's source), from the named rooms or, when unknown, built from the key itself. */
export function roomName(key: string, items: RoomItem[], w: RoomWords): { label: string; server: string } {
  const hit = items.find((r) => r.key === key)
  if (hit) return { label: hit.label, server: hit.server }
  const ids = parseRoomKey(key)
  if (!ids) return { label: key.replace(/^kakao:/, '') || key, server: w.kakao }
  return { label: channelLabel('', ids.channel, w).label, server: w.server(tail(ids.guild)) }
}

export type RoomGroup = { server: string; kind: 'discord' | 'kakao'; items: RoomItem[]; emptyHidden: number }

/** Rooms by server (Discord servers by name, the KakaoTalk imports last), the ones with no messages left out
 * unless asked for, narrowed by a typed filter on the room or server name. */
export function groupRooms(items: RoomItem[], opts: { showEmpty: boolean; filter: string }): RoomGroup[] {
  const f = opts.filter.trim().toLowerCase()
  const by = new Map<string, RoomGroup>()
  for (const r of items) {
    if (f && !`${r.label} ${r.server}`.toLowerCase().includes(f)) continue
    const k = `${r.kind}:${r.server}`
    let g = by.get(k)
    if (!g) by.set(k, (g = { server: r.server, kind: r.kind, items: [], emptyHidden: 0 }))
    if (r.count === 0 && !opts.showEmpty) g.emptyHidden++
    else g.items.push(r)
  }
  const groups = [...by.values()].filter((g) => g.items.length || g.emptyHidden)
  for (const g of groups) g.items.sort((a, b) => Number(a.hidden) - Number(b.hidden) || b.count - a.count || a.label.localeCompare(b.label))
  return groups.sort((a, b) => (a.kind === b.kind ? a.server.localeCompare(b.server) : a.kind === 'kakao' ? 1 : -1))
}
