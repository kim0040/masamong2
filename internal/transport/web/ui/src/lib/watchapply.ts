// "Apply to the server's other rooms": which rooms one room's saved alert settings copy to.
// The set is captured when the operator confirms, so navigating while the writes run cannot move
// a write to another server.

export type WatchApplyTarget = { guild_id: string; channel_id: string }

/** A name Discord sends for a channel the bot cannot read is a placeholder, never a real name. */
const hiddenName = (name: string | undefined): boolean => {
  const n = (name ?? '').trim().replace(/^#/, '')
  return /^_+hidden_*$/i.test(n) || /^_{3,}$/.test(n)
}

/** The copy targets: this server's rooms the bot can see — live on the gateway (or a practice
 * sample) under a real name; never a hidden channel, an offline configured room or this room. */
export function watchApplyTargets(channels: any[] | undefined, guildId: string, channelId: string): WatchApplyTarget[] {
  return (channels ?? [])
    .filter((x) => x.guild_id === guildId && x.id !== channelId && (x.live === true || x.practice === true) && !hiddenName(x.name))
    .map((x) => ({ guild_id: x.guild_id, channel_id: x.id }))
}
