// An edit form's draft: a copy of what the server holds, a snapshot of that copy ("base"), and the list of
// fields the person changed since. The room page, a tool's page and the usage page each kept this by hand,
// comparing in different ways (deep and shallow, with and without turning text into numbers).
//
//   let form = $state(…); let base = $state('')
//   $effect(() => { const a = adopt(fromServer(), base); if (a) { form = a.form; base = a.base } })
//   const changed = $derived(dirtyKeys(base, form))
//   after a save: base = ''            // the next read of the server's value is adopted again
//
// A form is a flat object. Two values are the same when they print as the same JSON, which is how the
// snapshot was made: "5" and 5 are different, as the server's own value and a typed one are.

export const snapshot = (value: unknown): string => JSON.stringify(value)

/** What the server holds now, against the snapshot the form was last filled from. The new form and snapshot
 * when the server's value moved (or the form was cleared with an empty base); null when nothing moved, so
 * what the person is typing stays. */
export function adopt<T>(saved: T, base: string): { form: T; base: string } | null {
  const next = snapshot(saved)
  return next === base ? null : { form: saved, base: next }
}

/** The fields that differ from the snapshot. Nothing is dirty before a snapshot exists. `skip` leaves out
 * fields that are not shown (a hidden draft is dropped, never saved). */
export function dirtyKeys(base: string, form: Record<string, unknown>, skip: (key: string) => boolean = () => false): string[] {
  if (!base) return []
  const saved = JSON.parse(base) as Record<string, unknown>
  return Object.keys(form).filter((k) => !skip(k) && snapshot(saved[k]) !== snapshot(form[k]))
}

/** A grid of numbers (row → column → value) against its snapshot: the cells that changed, as [row, column].
 * Values are compared as numbers, because an input hands text back; a cell the snapshot lacks counts as 0. */
export function changedCells(base: string, grid: Record<string, Record<string, number>>): [string, string][] {
  if (!base) return []
  const saved = JSON.parse(base) as Record<string, Record<string, number>>
  const out: [string, string][] = []
  for (const [row, cells] of Object.entries(grid)) for (const col of Object.keys(cells)) if (Number(saved[row]?.[col] ?? 0) !== Number(cells[col])) out.push([row, col])
  return out
}
