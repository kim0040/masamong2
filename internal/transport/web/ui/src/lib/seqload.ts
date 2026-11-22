// "The latest answer wins": a screen that asks again before the last answer came back must not draw the
// old answer over the new one. Every place that used to count requests by hand (`const my = ++seq` …
// `if (my !== seq) return`) takes a ticket here instead.
//
//   const loads = latest()
//   async function load() {
//     const live = loads.start()          // a newer start() (or cancel()) makes this ticket stale
//     const r = await call('x.list')
//     if (!live()) return                 // an older answer arrived late: drop it
//     …
//   }

export type Ticket = () => boolean

export type Latest = {
  /** Takes a ticket. Calling it later says whether no newer ticket was taken since. */
  start: () => Ticket
  /** Makes every ticket taken so far stale without taking a new one (the screen moved on or closed). */
  cancel: () => void
  /** Runs one request: its value, or `stale` when a newer request started while it was away. */
  run: <T>(fn: () => Promise<T>) => Promise<{ stale: true } | { stale: false; value: T }>
}

export function latest(): Latest {
  let n = 0
  const start = (): Ticket => {
    const mine = ++n
    return () => mine === n
  }
  return {
    start,
    cancel: () => void n++,
    async run(fn) {
      const live = start()
      const value = await fn()
      return live() ? { stale: false, value } : { stale: true }
    },
  }
}
