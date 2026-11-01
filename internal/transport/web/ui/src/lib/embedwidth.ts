// The embedding width shown and changed on the Connections page. The server (providers.list → embed_width)
// says what is stored and what the model can be cut to; this file only turns that into choices and
// decides which way a change goes, so the page never guesses a width the server would refuse.

/** providers.list's embed_width. effective and full_width are 0 when the model's full width is not known.
 * widths, when the server sends it, is the list of widths it accepts (0 = the full width); older servers leave
 * it out and the page derives the same list from full_width. rerank_usable is the server's own judgment of the
 * rerank role (a configured provider that offers /rerank and a model name), the one the runtime and the
 * recommendations use; the page never derives it. revision, provider and model name the one committed configuration
 * everything else in the object was read from. */
export type EmbedWidthInfo = { dimensions: number; effective: number; full_width: number; truncatable: boolean; recommended: number; widths?: number[]; rerank_usable?: boolean; revision?: number; provider?: string; model?: string }

/** The narrower widths the server accepts below a model's full width (smaller ones would also change the
 * message-vector space and embed again), widest first. */
export const NARROW_WIDTHS = [2048, 1024]
const MIN_NARROW = Math.min(...NARROW_WIDTHS)

/** full: the model's whole vector; narrow: its leading part; over: a width set by hand that is larger than the
 * model's full width (the settings file says so; the stored space is not the model's native one); custom: a
 * width set by hand on a model whose full width is unknown; unknown: the full width of a model nothing is
 * known about. */
export type WidthState = 'full' | 'narrow' | 'over' | 'custom' | 'unknown'

export function widthState(info?: EmbedWidthInfo | null): WidthState {
  if (!info) return 'unknown'
  if (info.effective > 0 && info.full_width > 0) return info.effective === info.full_width ? 'full' : info.effective < info.full_width ? 'narrow' : 'over'
  return info.effective > 0 ? 'custom' : 'unknown'
}

/** value is what models.embed_width takes (0 = the full width); width is the number of dimensions it means. */
export type WidthChoice = { value: number; width: number; full: boolean; recommended: boolean; current: boolean }

/** The full width and every narrower width the server allows, widest first. Nothing for a model that cannot be
 * cut, and nothing when the full width is all there is (a model whose full width is 1024 has no narrower choice). */
export function widthChoices(info?: EmbedWidthInfo | null): WidthChoice[] {
  if (!info?.truncatable || info.full_width <= 0) return []
  const full = info.full_width
  const allowed = Array.isArray(info.widths) ? info.widths : full > MIN_NARROW ? [0, ...NARROW_WIDTHS.filter((w) => w < full)] : []
  const values = [...new Set(allowed.filter((v) => Number.isInteger(v) && v >= 0 && (v === 0 || v < full)))]
  const out = values.map((v): WidthChoice => ({ value: v, width: v === 0 ? full : v, full: v === 0, recommended: v !== 0 && v === info.recommended, current: info.effective === (v === 0 ? full : v) }))
  out.sort((a, b) => b.width - a.width)
  return out.length > 1 ? out : []
}

/** lower: cut the stored vectors (no API call); raise: embed again; same: nothing to do. Going back to the full
 * width (0) from a hand-set width larger than it is a raise although the number gets smaller: that space cannot
 * be cut to the model's native one, so the server embeds again (the same rule as models.embed_width). */
export function widthDirection(info: EmbedWidthInfo | null | undefined, choice: WidthChoice): 'lower' | 'raise' | 'same' {
  const now = info?.effective ?? 0
  if (choice.full && widthState(info) === 'over' && info!.dimensions !== 0 && info!.dimensions !== 4096) return 'raise'
  if (choice.width === now) return 'same'
  return choice.width < now ? 'lower' : 'raise'
}

/** Whether the server judged the rerank role usable. A server that does not say (older) counts as usable: the
 * note below must not claim a missing reranker it was never told about. */
export function rerankUsable(info: EmbedWidthInfo | null | undefined): boolean {
  return info?.rerank_usable !== false
}

/** Without a usable rerank model a narrow width finds fewer conversation-flow questions (measured 2026-10-13):
 * worth a short note beside the setting, never an automatic change. */
export function narrowWithoutRerank(info: EmbedWidthInfo | null | undefined): boolean {
  return !rerankUsable(info) && widthState(info) === 'narrow'
}

/** What a successful models.embed_width did: unchanged when the width already was the one asked for (nothing was cut
 * or embedded, so nothing is announced as done), reembed when memory has to be indexed again, cut when only the
 * leading part of the stored vectors was kept. */
export function widthApplied(output?: { changed?: boolean; reembed?: boolean } | null): 'unchanged' | 'reembed' | 'cut' {
  if (output?.changed === false) return 'unchanged'
  return output?.reembed ? 'reembed' : 'cut'
}

/** What the confirmation was asked about: the config revision, the embedding model and provider, and the width stored
 * on it. All of it comes from one providers.list embed_width (the server fills it from a single configuration
 * snapshot), never from a second read such as config.document: two reads made at different moments can name
 * different revisions, and a revision paired with another revision's width would let the server's CAS pass a
 * narrowing the operator was never asked about. */
export type WidthBasis = { revision: number; provider: string; model: string; dimensions: number; effective: number; full_width: number }

/** The basis of an embed_width, or null when there is none to ask on (nothing read, or an answer that does not
 * name its revision): the page then offers no way to apply and asks for a new read. */
export function widthBasis(info: EmbedWidthInfo | null | undefined): WidthBasis | null {
  if (!info || !Number.isInteger(info.revision) || (info.revision as number) <= 0) return null
  return { revision: info.revision as number, provider: info.provider ?? '', model: info.model ?? '', dimensions: info.dimensions, effective: info.effective, full_width: info.full_width }
}

export function sameWidthBasis(a: WidthBasis | null | undefined, b: WidthBasis | null | undefined): boolean {
  return !!a && !!b && a.revision === b.revision && a.provider === b.provider && a.model === b.model && a.dimensions === b.dimensions && a.effective === b.effective && a.full_width === b.full_width
}

/** What a providers.list call gave for the width picker: the embed_width to work on, or null when the read failed or
 * the answer carries no usable basis. A failed read never falls back to an older answer. */
export function widthRead(result: { ok?: boolean; output?: { embed_width?: EmbedWidthInfo | null } | null } | null | undefined): EmbedWidthInfo | null {
  if (!result?.ok) return null
  const info = result.output?.embed_width
  return widthBasis(info) ? (info as EmbedWidthInfo) : null
}

/** What the operator was shown and approved: the basis the screen was made on and the direction it announced
 * (cut the stored vectors, or embed again). The first request and the include_embed request that follows it both
 * go out with this one approval; the current basis is not read again for the second one. */
export type WidthApproval = { basis: WidthBasis; direction: 'lower' | 'raise' }

export function widthApproval(basis: WidthBasis | null | undefined, direction: 'lower' | 'raise' | 'same'): WidthApproval | null {
  return basis && direction !== 'same' ? { basis, direction } : null
}

/** The body and base_revision of a models.embed_width call for an approval. The server compares direction with its
 * own judgment at the moment it runs and refuses (CONFIG_CONFLICT) when they differ, so a width that moved in
 * between is never cut or embedded on an answer given for the other direction. */
export function widthRequest(approval: WidthApproval, value: number, includeEmbed = false): { input: Record<string, unknown>; baseRevision: number } {
  const input: Record<string, unknown> = { dimensions: value, direction: approval.direction }
  if (includeEmbed) input.include_embed = true
  return { input, baseRevision: approval.basis.revision }
}

/** Whether an answer that came back for an approval still belongs to what the picker shows. When the basis moved
 * while the request was out (a read finished in between), the refusal's estimate was made for the old width: it is
 * dropped and the operator reads again, never asked about on the new basis. */
export function widthReplyFits(approval: WidthApproval | null | undefined, current: WidthBasis | null | undefined): boolean {
  return !!approval && sameWidthBasis(approval.basis, current)
}

/** One width the server proposes for a recommendation. dimensions 0 means the model's full width, then full is
 * its size (0 when the server did not say). */
export type RecWidth = { dimensions: number; reason: string; full: number }

type RecLike = {
  picks?: { role: string; changed?: boolean }[]
  embed_dimensions?: unknown
  embed_dimensions_reason?: string
  embed_full_width?: number
  embed_width_without_rerank_pick?: { dimensions?: unknown; reason?: string; full_width?: number } | null
}

/** The width the server would store for the picks as they stand in the dialog (on = which roles are ticked).
 * The preview carries two proposals when the picks include a rerank model: one for all of them and one for the
 * set without it, because the rerank pick can be what makes reranking usable. Nothing while the embedding row is
 * not ticked: no width changes then. */
export function recommendedWidth(rec: RecLike | null | undefined, on: Record<string, boolean>): RecWidth | null {
  if (!rec || !on.embed) return null
  const rerankSent = (rec.picks ?? []).some((p) => p.role === 'rerank' && p.changed && on.rerank)
  const alt = rec.embed_width_without_rerank_pick
  if (alt && !rerankSent) return typeof alt.dimensions === 'number' ? { dimensions: alt.dimensions, reason: alt.reason ?? '', full: alt.full_width ?? 0 } : null
  if (typeof rec.embed_dimensions !== 'number') return null
  return { dimensions: rec.embed_dimensions, reason: rec.embed_dimensions_reason ?? '', full: rec.embed_full_width ?? 0 }
}

/** Which roles are ticked in the recommendation dialog for a preview. A role the operator already ticked or
 * unticked keeps that choice when a newer preview still proposes it; a role seen for the first time starts ticked,
 * except the embedding while memory is indexed (a new embedding model empties memory search until everything is
 * embedded again: the operator opts in). Roles the preview no longer proposes are dropped. */
export function recTicks(rec: { picks?: { role: string; changed?: boolean }[]; reindex?: unknown } | null | undefined, previous?: Record<string, boolean> | null): Record<string, boolean> {
  return Object.fromEntries((rec?.picks ?? []).filter((p) => p.changed).map((p) => [p.role, previous && p.role in previous ? !!previous[p.role] : !(p.role === 'embed' && rec?.reindex)]))
}

/** A newer preview, with the roles the operator set from the same dialog (the cheaper-model buttons) shown as
 * done: their pick is no longer a change to send. The server already compares against the saved document, so this
 * only restores the "replaced by hand" mark for a pick that still differs from what was chosen. applied is
 * {role: provider and model chosen here}; a role whose saved provider or model is no longer exactly that (another tab
 * moved it, even only to another provider) is the server's own answer again. */
export function recAfterRefresh<T extends { picks?: { role: string; model?: string; current_provider?: string; current_model?: string; changed?: boolean; replaced?: boolean }[] }>(fresh: T, applied: Record<string, { provider: string; model: string }>): T {
  for (const p of fresh.picks ?? []) {
    const chosen = applied[p.role]
    if (chosen !== undefined && p.current_model === chosen.model && p.current_provider === chosen.provider) Object.assign(p, { replaced: p.model !== chosen.model, changed: false })
  }
  return fresh
}

/** The first recommendation preview of a dialog is being read ('loading'), and a settings change that arrives
 * meanwhile cannot be applied to a preview that does not exist yet: it is remembered (dirty) and the preview is read
 * again as soon as the first answer is in, with Apply held back in between. */
export type RecLive = { loading: boolean; dirty: boolean }

export const recLiveStart = (): RecLive => ({ loading: true, dirty: false })

/** A settings change was announced. refresh: read the preview again now (never while it is loading, and not while an
 * apply is in flight, whose own result closes the dialog). */
export function recLiveChange(state: RecLive, applying: boolean): { state: RecLive; refresh: boolean } {
  if (state.loading) return { state: { loading: true, dirty: true }, refresh: false }
  return { state, refresh: !applying }
}

/** The first preview came back (or failed). refresh: a change was seen while it was loading, so it may be out of date. */
export function recLiveLoaded(state: RecLive, ok = true): { state: RecLive; refresh: boolean } {
  return { state: { loading: false, dirty: false }, refresh: ok && state.dirty }
}
