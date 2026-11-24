// The lab's remembered room and sources: read back checked, and nothing breaks when storage is missing or throws.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const l = await loadLib('labpick.ts')
const memory = () => {
  const m = new Map()
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v) }
}
const broken = { getItem: () => { throw new Error('blocked') }, setItem: () => { throw new Error('blocked') } }

test('a pick written is a pick read', () => {
  const s = memory()
  l.writePick(s, { channel: 'g:c', rooms: ['discord:g:c', 'kakao:x'] })
  assert.deepEqual(l.readPick(s), { channel: 'g:c', rooms: ['discord:g:c', 'kakao:x'] })
})

test('nothing stored, bad JSON and wrong shapes read as empty or are cleaned', () => {
  const empty = { channel: '', rooms: [] }
  assert.deepEqual(l.readPick(memory()), empty)
  assert.deepEqual(l.readPick(null), empty)
  assert.deepEqual(l.readPick(undefined), empty)
  for (const raw of ['{', 'null', '5', '"x"', '[]']) assert.deepEqual(l.readPick({ getItem: () => raw }), empty, raw)
  assert.deepEqual(l.readPick({ getItem: () => JSON.stringify({ channel: 7, rooms: ['a', 3, null, 'b'] }) }), { channel: '', rooms: ['a', 'b'] })
  assert.deepEqual(l.readPick({ getItem: () => JSON.stringify({ channel: 'g:c', rooms: 'a' }) }), { channel: 'g:c', rooms: [] })
})

test('storage that throws never throws out of the helpers', () => {
  assert.deepEqual(l.readPick(broken), { channel: '', rooms: [] })
  assert.doesNotThrow(() => l.writePick(broken, { channel: 'g:c', rooms: [] }))
  assert.doesNotThrow(() => l.writePick(null, { channel: 'g:c', rooms: [] }))
})

test('only the rooms that still exist are kept, in order', () => {
  assert.deepEqual(l.keepKnown(['a', 'gone', 'b'], ['b', 'a', 'c']), ['a', 'b'])
  assert.deepEqual(l.keepKnown([], ['a']), [])
})

test('asking for the browser storage is safe where there is none', () => {
  assert.equal(l.browserStorage(), null)
})

test('the saved room is resolved only after the channel list has loaded, so a room from the later list is not lost', () => {
  const early = [{ value: 'g:configured' }]
  const full = [{ value: 'g:configured' }, { value: 'g:live' }]
  // config answered first, channels.list has not: nothing is chosen yet
  assert.equal(l.resolveChannel('', 'g:live', early, false), '')
  // the list arrived with the saved room: it is kept
  assert.equal(l.resolveChannel('', 'g:live', full, true), 'g:live')
  // the saved room is gone: the first room the bot can see, else the first
  assert.equal(l.resolveChannel('', 'g:old', [{ value: 'a', hidden: true }, { value: 'b' }], true), 'b')
  assert.equal(l.resolveChannel('', '', [{ value: 'a', hidden: true }], true), 'a')
  assert.equal(l.resolveChannel('', 'g:live', [], true), '')
  // an address that names a room wins, loaded or not
  assert.equal(l.resolveChannel('x:y', 'g:live', full, false), 'x:y')
})
