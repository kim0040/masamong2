// Handing out a file, and the two ways the console writes CSV.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const m = await loadLib('download.ts')

function fakeBrowser() {
  const events = []
  const link = { href: '', download: '', click: () => events.push(['click', link.href, link.download]) }
  return {
    events,
    createObjectURL: async () => '',
    browser: {
      createObjectURL: (blob) => {
        events.push(['url', blob.type, blob.size])
        return 'blob:synthetic'
      },
      revokeObjectURL: (u) => events.push(['revoke', u]),
      createLink: () => link,
    },
  }
}

test('saveFile makes the address, clicks the link with the name, then lets the address go', () => {
  const f = fakeBrowser()
  m.saveFile('masamong-logs.txt', 'a\nb', 'text/plain', f.browser)
  assert.deepEqual(f.events, [
    ['url', 'text/plain', 3],
    ['click', 'blob:synthetic', 'masamong-logs.txt'],
    ['revoke', 'blob:synthetic'],
  ])
})

test('quoteCell doubles quotes and shows nothing for a missing value', () => {
  assert.equal(m.quoteCell('say "hi"'), '"say ""hi"""')
  assert.equal(m.quoteCell(undefined), '""')
  assert.equal(m.quoteCell(null), '""')
  assert.equal(m.quoteCell(0), '"0"')
})

test('plainCell quotes only where a comma, a quote or a line break needs it', () => {
  assert.equal(m.plainCell('turns_ok'), 'turns_ok')
  assert.equal(m.plainCell(12), '12')
  assert.equal(m.plainCell('a,b'), '"a,b"')
  assert.equal(m.plainCell('line\nbreak'), '"line\nbreak"')
  assert.equal(m.plainCell('q"q'), '"q""q"')
})

test('csvText: every cell quoted for the change history, only the needed ones for the statistics', () => {
  const rows = [['at', 'summary'], ['2026-11-22', 'a, b']]
  assert.equal(m.csvText(rows, 'all'), '"at","summary"\n"2026-11-22","a, b"')
  assert.equal(m.csvText(rows, 'needed'), 'at,summary\n2026-11-22,"a, b"')
  // the statistics' plain rows come out exactly as before
  assert.equal(m.csvText([['day', 'turns_ok'], ['d1', '3']]), 'day,turns_ok\nd1,3')
})

test('datedName puts the UTC day in the file name', () => {
  assert.equal(m.datedName('changes', 'csv', new Date('2026-11-22T23:59:59Z')), 'masamong-changes-2026-11-22.csv')
})

test('the byte order mark is the one a spreadsheet looks for', () => {
  assert.equal(m.BOM, '﻿')
})
