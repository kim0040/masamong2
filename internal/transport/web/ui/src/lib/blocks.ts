// Memory blocks as the console shows them: one block per chat room (an imported KakaoTalk export or a
// Discord channel's captured history). Helpers here turn the server's block rows into what people
// read: a state with a next step, a period, and a reason when preparation stopped.
import { t, has } from './i18n.svelte'
import { num, duration, relative as relativeTime } from './format'
import { i18n } from './i18n.svelte'

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral'

export type BlockState = 'empty' | 'needs_prep' | 'preparing' | 'partial' | 'ready' | 'failed'

export function blockState(b: any): BlockState {
  // the provider holds this room's batch: still preparing, even between two runs of the job
  if (b?.waiting_batch) return b.batch?.state === 'submit_unknown' ? 'failed' : 'preparing'
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
  let phase = job.progress_phase ? t(`blocks.phase.${job.progress_phase}`) : t('blocks.phase.queued')
  // a batch's label carries the provider's state ("batch running 3/10"): say it in the console language
  if (job.progress_phase === 'batch') {
    const m = /^batch\s+([a-z]+)/.exec(job.progress_label ?? '')
    if (m) phase = `${phase} · ${t(`blocks.batch.state.${m[1]}`)}`
  }
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

export type PrepareMode = 'live' | 'batch'

/** The line for a block whose work is at the provider: still waiting for results, or collecting the
 * batches that were already sent after a cancel or a refused submission. */
export function batchWaitLine(b: any): string {
  const vars = { n: num(b?.batch?.items ?? 0), when: relativeTime(b?.batch?.submitted_at) }
  return t(b?.batch?.state === 'draining' ? 'blocks.batch.draining' : 'blocks.batch.waiting', vars)
}

/** A block's saved default mode ("" means live). */
export function modeOf(b: any): PrepareMode {
  return b?.mode === 'batch' ? 'batch' : 'live'
}

/** Both prices from one dry run asked with mode=batch: the live total and the batch total. Null before
 * there is an estimate; `saves` is false when the provider cannot batch any part (the prices are equal). */
export function batchOffer(est: any): { live: number; batch: number; saves: boolean } | null {
  const live = est?.total_usd
  const batch = est?.batch_usd
  if (typeof live !== 'number' || typeof batch !== 'number') return null
  return { live, batch, saves: batch < live - 1e-9 }
}

/** The mode to start with: batch only when the person chose it and the provider can batch something. */
export function effectiveMode(mode: PrepareMode, est: any): PrepareMode {
  const o = batchOffer(est)
  return mode === 'batch' && (!o || o.saves) ? 'batch' : 'live'
}
