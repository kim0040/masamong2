// The draft of an edit form: when the server's value is taken in, and what counts as edited.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const d = await loadLib('draft.ts')

test('the first read of the server fills the form; the same read again leaves what is typed alone', () => {
  const saved = { name: 'a', on: false }
  const first = d.adopt(saved, '')
  assert.deepEqual(first.form, saved)
  assert.equal(first.base, '{"name":"a","on":false}')
  // the effect runs again with the same server value: nothing to take in
  assert.equal(d.adopt({ name: 'a', on: false }, first.base), null)
})

test('a server value that moved is taken in (a save elsewhere, a refresh)', () => {
  const first = d.adopt({ name: 'a' }, '')
  const next = d.adopt({ name: 'b' }, first.base)
  assert.deepEqual(next.form, { name: 'b' })
})

test('clearing the base after a save takes the server value in again', () => {
  const first = d.adopt({ name: 'a' }, '')
  assert.notEqual(d.adopt({ name: 'a' }, ''), null)
  assert.equal(d.adopt({ name: 'a' }, first.base), null)
})

test('nothing is dirty before a snapshot exists', () => {
  assert.deepEqual(d.dirtyKeys('', { a: 1 }), [])
})

test('dirtyKeys lists the fields that differ from the snapshot, deep values included', () => {
  const base = d.snapshot({ name: 'a', n: 3, levels: [{ region: 'x', level: '1' }] })
  assert.deepEqual(d.dirtyKeys(base, { name: 'a', n: 3, levels: [{ region: 'x', level: '1' }] }), [])
  assert.deepEqual(d.dirtyKeys(base, { name: 'b', n: 3, levels: [{ region: 'x', level: '1' }] }), ['name'])
  assert.deepEqual(d.dirtyKeys(base, { name: 'a', n: 3, levels: [{ region: 'x', level: '2' }] }), ['levels'])
  // typed text is not the number it looks like: the server's value was a number
  assert.deepEqual(d.dirtyKeys(base, { name: 'a', n: '3', levels: [{ region: 'x', level: '1' }] }), ['n'])
})

test('skip leaves out a field that is not shown, even when it was edited', () => {
  const base = d.snapshot({ a: 1, hidden: 1 })
  assert.deepEqual(d.dirtyKeys(base, { a: 2, hidden: 2 }, (k) => k === 'hidden'), ['a'])
})

test('a form with a field the snapshot lacks counts it as changed', () => {
  assert.deepEqual(d.dirtyKeys(d.snapshot({ a: 1 }), { a: 1, b: 'x' }), ['b'])
})

test('changedCells compares numbers, so text typed into a number field that equals the saved one is not a change', () => {
  const base = d.snapshot({ 'chat.respond': { per_day: 0, cooldown_sec: 3 }, 'weather.query': { per_day: 10, cooldown_sec: 0 } })
  assert.deepEqual(d.changedCells(base, { 'chat.respond': { per_day: '0', cooldown_sec: 3 }, 'weather.query': { per_day: 10, cooldown_sec: 0 } }), [])
  assert.deepEqual(d.changedCells(base, { 'chat.respond': { per_day: 0, cooldown_sec: 5 }, 'weather.query': { per_day: 11, cooldown_sec: 0 } }), [['chat.respond', 'cooldown_sec'], ['weather.query', 'per_day']])
  // a tool the snapshot does not know yet starts from 0
  assert.deepEqual(d.changedCells(base, { 'new.tool': { per_day: 0 } }), [])
  assert.deepEqual(d.changedCells(base, { 'new.tool': { per_day: 2 } }), [['new.tool', 'per_day']])
  assert.deepEqual(d.changedCells('', { x: { a: 1 } }), [])
})
