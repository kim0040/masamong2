// /people?person=<id>&guild=<server> links, and keeping a page's query through the login screen.

type Row = { user_id: string; [k: string]: any }
type ServerGroup = { guild_id: string; name?: string; people?: Row[] }
export type DeepPerson = Row & { guild_id: string; guild_name?: string }

/** The exact person a link names, from a `people.servers` answer asked for that account (`user_id`, and `guild_id`
 * when the link names a server). A link with a server matches only that server; one without takes the first
 * server that has the person. Null when nothing matches, so the page can say so instead of opening someone else. */
export function deepPerson(servers: ServerGroup[] | undefined, id: string, guild: string): DeepPerson | null {
  for (const s of servers ?? []) {
    if (guild && s.guild_id !== guild) continue
    const p = (s.people ?? []).find((x) => x.user_id === id)
    if (p) return { ...p, guild_id: s.guild_id, guild_name: s.name }
  }
  return null
}

/** The address to come back to after login: the path and its query. */
export function pathWithQuery(path: string, query: URLSearchParams | undefined): string {
  const q = query?.toString() ?? ''
  return q ? `${path}?${q}` : path
}
