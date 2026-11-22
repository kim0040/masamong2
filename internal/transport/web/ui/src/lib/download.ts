// Handing the person a file from the console (an export, a log, a report). Six screens built the same
// link by hand (a Blob, an <a download>, a click, revoking the address) and the two CSV exports quoted
// cells differently.

/** What saveFile needs from the page; the browser's own by default, replaceable in a test. */
export type Browser = {
  createObjectURL: (blob: Blob) => string
  revokeObjectURL: (url: string) => void
  createLink: () => { href: string; download: string; click: () => void }
}

const here = (): Browser => ({
  createObjectURL: (b) => URL.createObjectURL(b),
  revokeObjectURL: (u) => URL.revokeObjectURL(u),
  createLink: () => document.createElement('a'),
})

/** Offers `body` as a download called `name`. */
export function saveFile(name: string, body: BlobPart, type: string, browser: Browser = here()): void {
  const url = browser.createObjectURL(new Blob([body], { type }))
  const a = browser.createLink()
  a.href = url
  a.download = name
  a.click()
  browser.revokeObjectURL(url)
}

/** A spreadsheet opens a UTF-8 CSV as UTF-8 only when it starts with this mark. */
export const BOM = '﻿'

/** One CSV cell in quotes, quotes doubled. */
export const quoteCell = (v: unknown): string => `"${String(v ?? '').replaceAll('"', '""')}"`

/** A cell as it is: quoted only when it holds a comma, a quote or a line break. */
export const plainCell = (v: unknown): string => {
  const s = String(v ?? '')
  return /[",\r\n]/.test(s) ? quoteCell(s) : s
}

/** Rows as CSV text: every cell quoted (`all`, what the change history uses) or only where it needs it (`needed`). */
export function csvText(rows: unknown[][], quote: 'all' | 'needed' = 'needed'): string {
  const cell = quote === 'all' ? quoteCell : plainCell
  return rows.map((r) => r.map(cell).join(',')).join('\n')
}

/** `masamong-changes-2026-11-22.csv`: a file name with the day (UTC) in it. */
export const datedName = (stem: string, ext: string, now: Date = new Date()): string => `masamong-${stem}-${now.toISOString().slice(0, 10)}.${ext}`
