// Memory blocks as the console shows them: one block per chat room (an imported KakaoTalk export or a
// Discord channel's captured history). Helpers here turn the server's block rows into what people
// read: a state with a next step, a period, and a reason when preparation stopped.
import { t, has } from './i18n.svelte'
import { num, duration } from './format'
import { i18n } from './i18n.svelte'

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral'

export type BlockState = 'empty' | 'needs_prep' | 'preparing' | 'partial' | 'ready' | 'failed'

export function blockState(b: any): BlockState {
  if (b?.job && ['pending', 'leased', 'running'].includes(b.job.status)) return 'preparing'
  if (!b?.message_count) return 'empty'
  if (b.status === 'failed' && (b.embedded ?? 0) < (b.leaf_count ?? 0)) return 'failed'
  if ((b.embedded ?? 0) === 0) return 'needs_prep'
  if ((b.pending_leaves ?? 0) > 0) return 'partial'
  return 'ready'
}

export const stateTone: Record<BlockState, Tone> = {
  empty: 'neutral', needs_prep: 'warn', preparing: 'info', partial: 'info', ready: 'ok', failed: 'danger',
}

/** Why a run stopped, in words (budget, provider, cancel…). */
export function stopReason(code: string | undefined): string {
  if (!code) return ''
  const key = `blocks.stop.${code.trim().replace(/\s+/g, '_')}`
  const out = t(key)
  return out === code || out === key.split('.').pop() ? code : out
}

/** "2023. 1. ~ 2024. 3." style period in the console language. */
export function period(first?: string, last?: string): string {
  if (!first) return ''
  const f = (s: string) => {
    const d = new Date(s.length <= 19 ? s : s)
    if (Number.isNaN(d.getTime())) return s.slice(0, 10)
    return new Intl.DateTimeFormat(i18n.lang || 'en', { year: 'numeric', month: 'short' }).format(d)
  }
  const a = f(first)
  const b = last ? f(last) : a
  return a === b ? a : `${a} – ${b}`
}

/** Time left in the console language ("약 3분"), or '' before there is an estimate. */
export function etaText(job: any): string {
  const s = job?.eta_s
  if (s === undefined || s === null || s < 0) return ''
  return s < 60 ? t('blocks.est.under_minute') : duration(s)
}

/** A compact progress line for a running job. */
export function jobLine(job: any): string {
  if (!job) return ''
  const phase = job.progress_phase ? t(`blocks.phase.${job.progress_phase}`) : t('blocks.phase.queued')
  if (job.progress_total) return `${phase} ${num(job.progress_done)}/${num(job.progress_total)}`
  return phase
}

export function isKakao(b: any) {
  return b?.platform === 'kakao'
}

/** The URL of a block's page (room keys contain colons). */
export function blockHref(room: string) {
  return '/blocks/' + encodeURIComponent(room)
}

/** A KakaoTalk room key from a file name: kakao:<slug>. */
export function roomFromFile(name: string): string {
  const slug = name
    .replace(/\.[^.]+$/, '')
    .toLowerCase()
    .replace(/[^a-z0-9가-힣ㄱ-ㅎㅏ-ㅣ]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 40)
  return 'kakao:' + (slug || 'chat')
}

/** Why a search found nothing, when the scope can say: the room has no memory block attached (the search
 * looks only at blocks attached to the room), or the attached ones had no match. */
export function emptyScope(blocks: any[], channel: { guild_id: string; channel_id: string } | null): 'no_block' | 'no_match' {
  if (!channel) return 'no_match'
  const attached = blocks.some((b) => (b.channels ?? []).some((c: any) => c.guild_id === channel.guild_id && c.channel_id === channel.channel_id))
  return attached ? 'no_match' : 'no_block'
}

/** A state code of the memory search as a label ("no memories in scope"), never the raw code. */
export function stateLabel(code: string | undefined): string {
  if (!code) return ''
  const key = `memstate.${code}`
  return has(key) ? t(key) : code.replaceAll('_', ' ')
}
