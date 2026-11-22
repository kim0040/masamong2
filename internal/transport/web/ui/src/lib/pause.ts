// Pausing the bot: all of it (the top bar, the banner, the command palette) or one room (its page).
// Four screens built the Action input by hand and two of them said "resumed" even when the call failed.
import { call } from './api.svelte'
import { refreshOverview } from './overview.svelte'

export type PauseTarget = { scope: 'all' } | { scope: 'channel'; guild_id: string; channel_id: string }

export const PAUSE_ALL: PauseTarget = { scope: 'all' }

/** runtime.pause's input: the server takes the flag as the text "true" or "false". */
export const pauseInput = (target: PauseTarget, paused: boolean) => ({ ...target, paused: paused ? 'true' : 'false' })

type PauseState = { all?: unknown; guilds?: Record<string, unknown>; channels?: Record<string, unknown> } | undefined | null

/** Everything is paused (overview's pause.all). */
export const allPaused = (pause: PauseState): boolean => !!pause?.all

/** A room is paused by its own switch or by its server's. */
export const roomPaused = (pause: PauseState, guild: string, channel: string): boolean => !!pause?.channels?.[`${guild}:${channel}`] || !!pause?.guilds?.[guild]

export type Paused = { ok: boolean; message: string | undefined }

/** Pauses or resumes, then reads the overview again (the pause state on every screen comes from it), whether the call worked or not. */
export async function setPaused(target: PauseTarget, paused: boolean): Promise<Paused> {
  const r = await call('runtime.pause', pauseInput(target, paused))
  await refreshOverview()
  return { ok: r.ok, message: r.error?.message }
}
