// "Apply to the server's other rooms" writes the room's saved alert switch and area levels to a set
// captured at the confirm: the same server's rooms the bot can see, never hidden or offline ones.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const m = await loadLib('watchapply.ts')

const channels = [
  { guild_id: 'g1', id: 'c1', name: 'general', live: true },
  { guild_id: 'g1', id: 'c2', name: 'chat', live: true },
  { guild_id: 'g1', id: 'c3', name: '2024', live: true }, // a real channel may be named only digits
  { guild_id: 'g1', id: 'c4', name: '___hidden___', live: true }, // on the gateway but unreadable
  { guild_id: 'g1', id: 'c5', name: 'c5', live: false }, // configured; not on the gateway now
  { guild_id: 'g1', id: 'c6', name: 'stage', live: false, practice: true }, // a practice sample
  { guild_id: 'g2', id: 'c2', name: 'chat', live: true }, // another server
]

test('the targets are this server\u2019s other rooms the bot can see', () => {
  assert.deepEqual(m.watchApplyTargets(channels, 'g1', 'c1'), [
    { guild_id: 'g1', channel_id: 'c2' },
    { guild_id: 'g1', channel_id: 'c3' },
    { guild_id: 'g1', channel_id: 'c6' },
  ])
})

test('hidden, offline and other servers\u2019 rooms are never targets', () => {
  const ids = m.watchApplyTargets(channels, 'g1', 'c2').map((x) => x.channel_id)
  assert.ok(!ids.includes('c4'), 'hidden channel')
  assert.ok(!ids.includes('c5'), 'offline configured room')
  assert.ok(!ids.includes('c2'), 'the room itself')
  assert.deepEqual(m.watchApplyTargets(channels, 'g2', 'c9'), [{ guild_id: 'g2', channel_id: 'c2' }])
})

test('a server with no visible other rooms has no targets', () => {
  assert.deepEqual(m.watchApplyTargets([{ guild_id: 'g', id: 'a', name: 'x', live: true }], 'g', 'a'), [])
  assert.deepEqual(m.watchApplyTargets(undefined, 'g', 'a'), [])
})
