// Evidence is packed without repeats: a hit whose own messages an earlier hit already showed carries
// only their surroundings. shownIn is that earlier hit's rank (0 when the hit shows its own messages), so
// the page can say so instead of a date that seems to disagree with the text under it.
type Hit = {
  rank?: number
  leaf_shown_in?: number
  leaf_observation_ids?: string[]
  visible_observation_ids?: string[]
}

export function shownIn(hits: Hit[], i: number): number {
  const h = hits[i]
  if (!h) return 0
  if (h.leaf_shown_in) return h.leaf_shown_in
  const own = new Set(h.leaf_observation_ids ?? [])
  if (!own.size || (h.visible_observation_ids ?? []).some((id) => own.has(id))) return 0
  for (let j = 0; j < i; j++) if ((hits[j].visible_observation_ids ?? []).some((id) => own.has(id))) return hits[j].rank ?? j + 1
  return 0
}
