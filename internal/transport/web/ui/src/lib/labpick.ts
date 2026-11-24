// The answer lab remembers the room and the sources last picked, in this browser only. Storage can be missing
// or throw (a private window, blocked site data), so every read and write is guarded and the page works
// without it; what is read back is checked, and only what still exists is used.

const KEY = 'masamong.lab.pick'

export type LabPick = { channel: string; rooms: string[] }
type Store = Pick<Storage, 'getItem' | 'setItem'> | null | undefined

/** The saved pick; empty when nothing usable is stored. */
export function readPick(store: Store): LabPick {
  const none: LabPick = { channel: '', rooms: [] }
  try {
    const v = JSON.parse(store?.getItem(KEY) ?? 'null')
    if (!v || typeof v !== 'object') return none
    return {
      channel: typeof v.channel === 'string' ? v.channel : '',
      rooms: Array.isArray(v.rooms) ? v.rooms.filter((x: unknown): x is string => typeof x === 'string').slice(0, 200) : [],
    }
  } catch {
    return none
  }
}

/** Saves the pick; a failure (full or blocked storage) is ignored. */
export function writePick(store: Store, pick: LabPick): void {
  try {
    store?.setItem(KEY, JSON.stringify({ channel: pick.channel, rooms: pick.rooms }))
  } catch {
    /* the pick is a convenience */
  }
}

/** The picks that still exist (a room that was removed or renamed away is dropped), in their order. */
export const keepKnown = (picked: string[], known: Iterable<string>): string[] => {
  const set = new Set(known)
  return picked.filter((k) => set.has(k))
}

/** The browser's storage, or null when asking for it throws. */
export function browserStorage(): Storage | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage
  } catch {
    return null
  }
}
