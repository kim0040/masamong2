// A person's memory in one server (memory.mine.show / memory.mine.forget), as the console shows it. Pure
// helpers so the page stays thin and the rules are testable without a browser.

export type MineItem = { id: string; kind: string; text: string; created_at: string; explicit?: boolean }

/** One statement about the person (memory.mine.show person). origin: self = their own words, explicit =
 * they asked the bot to remember it, said = someone else said it, told = someone asked the bot to remember it. */
export type Claim = { id: string; kind: string; text: string; origin: string; said_by?: string[]; observed_at: string; evidence_n?: number; state?: string; room_id?: string }

/** The items the person asked to have remembered, apart from the ones that only name them. */
export function splitMine(items: MineItem[] | null | undefined): { told: MineItem[]; other: MineItem[] } {
  const list = Array.isArray(items) ? items : []
  return { told: list.filter((i) => i.explicit), other: list.filter((i) => !i.explicit) }
}

/** What forgetting would remove, in all: nothing means there is nothing to confirm. Kept cards and person
 * statements are both "person memory" to the reader, so they are one number. */
export function forgetTotal(p: { items?: number; episodes?: number; cards?: number; person_items?: number } | null | undefined): number {
  return (p?.items ?? 0) + (p?.episodes ?? 0) + personTotal(p)
}

/** The person-memory part of a forget preview: the statements and any older kept card lines. */
export function personTotal(p: { cards?: number; person_items?: number } | null | undefined): number {
  return (p?.cards ?? 0) + (p?.person_items ?? 0)
}

type Group = { guild_id: string; name?: string; people: { user_id: string; last_seen: string }[] }

const when = (iso: string | undefined) => {
  const ms = Date.parse(iso ?? '')
  return Number.isNaN(ms) ? 0 : ms
}

/** The servers one person is in (people.servers groups, filtered by exact membership), latest activity first. */
export function memberServers(groups: Group[] | null | undefined, userId: string): { value: string; label: string; last: string }[] {
  const out: { value: string; label: string; last: string }[] = []
  for (const g of groups ?? []) {
    const person = (g.people ?? []).find((p) => p.user_id === userId)
    if (person) out.push({ value: g.guild_id, label: g.name || g.guild_id, last: person.last_seen })
  }
  return out.sort((a, b) => when(b.last) - when(a.last) || a.value.localeCompare(b.value))
}

/** Which server to show: the current one while the person is still in it, else the one the operator came
 * from, else the server with the latest activity (never just the first in the list). */
export function pickServer(servers: { value: string; last: string }[], current: string, preferred: string): string {
  if (servers.some((s) => s.value === current)) return current
  if (servers.some((s) => s.value === preferred)) return preferred
  let best = ''
  let bestAt = -1
  for (const s of servers) {
    const at = when(s.last)
    if (at > bestAt) {
      best = s.value
      bestAt = at
    }
  }
  return best
}

/** The summary line: statements in their own words, what others said, and what was asked to be remembered. */
export function claimSummary(claims: Claim[] | null | undefined): { own: number; others: number; told: number; last: string } {
  let own = 0
  let others = 0
  let told = 0
  let last = ''
  for (const c of Array.isArray(claims) ? claims : []) {
    if (c.origin === 'self') own++
    else if (c.origin === 'said') others++
    else told++ // explicit and told: asked to be remembered
    if (when(c.observed_at) > when(last)) last = c.observed_at
  }
  return { own, others, told, last }
}

/** Direct = the person's own words or their own request; everything else was passed on by someone else. */
export const isDirect = (c: Pick<Claim, 'origin'>) => c.origin === 'self' || c.origin === 'explicit'

/** The newest statement first, as the bot would rank ties. */
export function sortClaims<T extends Claim>(claims: T[] | null | undefined): T[] {
  return [...(Array.isArray(claims) ? claims : [])].sort((a, b) => when(b.observed_at) - when(a.observed_at) || a.id.localeCompare(b.id))
}
