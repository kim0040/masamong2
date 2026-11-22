// The two things the console asks Discord for besides settings: "whose bot is this token?" and "give me
// an invite link". Connections, the setup wizard and a room's page each had their own copy and read the
// answers a little differently.
import { call } from './api.svelte'

export type BotCheck = {
  /** discord.check's answer (bot_name, bot_id, message_content_portal, …), null when it failed */
  bot: any | null
  /** why it failed, when the server said (no_token, token_rejected, …); '' otherwise */
  reason: string
  /** the server's message; undefined when there is none to show (no failure, or simply no token saved yet) */
  message: string | undefined
}

/** Asks Discord who the stored token belongs to and keeps the bot id for the invite link. A missing
 * token is not an error to show (the person has not saved one yet). */
export async function checkBot(): Promise<BotCheck> {
  const r = await call('discord.check', { apply: true })
  if (r.ok) return { bot: r.output, reason: '', message: undefined }
  const reason = String(r.error?.args?.reason ?? '')
  return { bot: null, reason, message: reason === 'no_token' ? undefined : (r.error?.message ?? '') }
}

export type Invite = { url: string; message: string }

/** The invite link, preselecting a server when one is named (a room's page re-invites its own). */
export async function makeInvite(guildId?: string): Promise<Invite> {
  const r = await call('channels.invite', guildId ? { guild_id: guildId } : {})
  return r.ok ? { url: String(r.output.url), message: '' } : { url: '', message: r.error?.message ?? '' }
}
