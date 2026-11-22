// Pause and resume: one input shape, one reading of the pause state, one answer.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const log = []
globalThis.__call = async (action, input) => {
  log.push(['call', action, input])
  return globalThis.__answer
}
globalThis.__refresh = async () => void log.push(['refresh'])
const m = await loadLib('pause.ts', [
  ["import { call } from './api.svelte'", 'const call = (...a) => globalThis.__call(...a)'],
  ["import { refreshOverview } from './overview.svelte'", 'const refreshOverview = () => globalThis.__refresh()'],
])

test('the flag goes out as the text true or false, with the scope and the room', () => {
  assert.deepEqual(m.pauseInput(m.PAUSE_ALL, true), { scope: 'all', paused: 'true' })
  assert.deepEqual(m.pauseInput(m.PAUSE_ALL, false), { scope: 'all', paused: 'false' })
  assert.deepEqual(m.pauseInput({ scope: 'channel', guild_id: 'g1', channel_id: 'c1' }, true), { scope: 'channel', guild_id: 'g1', channel_id: 'c1', paused: 'true' })
})

test('all-paused and room-paused read the overview the same way every screen needs', () => {
  assert.equal(m.allPaused(undefined), false)
  assert.equal(m.allPaused({}), false)
  assert.equal(m.allPaused({ all: { at: 'x' } }), true)
  const pause = { channels: { 'g1:c1': {} }, guilds: { g2: {} } }
  assert.equal(m.roomPaused(pause, 'g1', 'c1'), true)
  assert.equal(m.roomPaused(pause, 'g1', 'c2'), false)
  // the whole server is paused: every room in it is
  assert.equal(m.roomPaused(pause, 'g2', 'anything'), true)
  assert.equal(m.roomPaused(null, 'g1', 'c1'), false)
})

test('setPaused calls the Action, then reads the overview again, and says whether it worked', async () => {
  log.length = 0
  globalThis.__answer = { ok: true }
  assert.deepEqual(await m.setPaused(m.PAUSE_ALL, true), { ok: true, message: undefined })
  assert.deepEqual(log, [['call', 'runtime.pause', { scope: 'all', paused: 'true' }], ['refresh']])
})

test('a failed pause is not ok (the old palette and banner said "resumed" anyway), and the overview is still read', async () => {
  log.length = 0
  globalThis.__answer = { ok: false, error: { code: 'X', message: 'nope' } }
  assert.deepEqual(await m.setPaused(m.PAUSE_ALL, false), { ok: false, message: 'nope' })
  assert.equal(log.at(-1)[0], 'refresh')
})
