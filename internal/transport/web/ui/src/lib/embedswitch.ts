// Switching the embedding model while memory is indexed empties memory search until every room is
// indexed again, so models.select / models.recommend refuse it unless include_embed is true. The refusal
// carries a stable message key (never matched by its English text) and the estimate to show.
export const EMBED_REINDEX_KEY = 'models.embed_reindex_required'

/** known is false when the refusal carried no estimate: the confirm then shows the generic note instead of zeros. */
export type EmbedAsk = { known: boolean; rooms: number; leaves: number; tokens: number }

/** The estimate to ask the operator about, or null when the error is something else. */
export function embedReindexAsk(error?: { message_key?: string; args?: Record<string, unknown> }): EmbedAsk | null {
  if (error?.message_key !== EMBED_REINDEX_KEY) return null
  const raw = error.args?.reindex
  if (!raw || typeof raw !== 'object') return { known: false, rooms: 0, leaves: 0, tokens: 0 }
  const r = raw as Record<string, unknown>
  return { known: true, rooms: Number(r.rooms) || 0, leaves: Number(r.leaves) || 0, tokens: Number(r.tokens) || 0 }
}
