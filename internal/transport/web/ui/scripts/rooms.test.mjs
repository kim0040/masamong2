// How rooms are named and grouped: the placeholder Discord sends for a channel the bot cannot see, bare numbers,
// the server each room belongs to, and what a picker leaves out.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const r = await loadLib('rooms.ts')
const w = { hidden: (t) => `hidden …${t}`, server: (t) => `server …${t}`, kakao: 'Kakao' }

test('a name that names nothing is a placeholder', () => {
  for (const n of ['___hidden___', '#___hidden___', '__hidden__', '___HIDDEN___', '', '  ', '#', '______', null, undefined]) assert.equal(r.placeholderName(n), true, String(n))
  for (const n of ['logs', '#general', 'hidden', 'hidden-room', 'a_hidden_b', '12-general', '2024', '#2024', '1', '123456']) assert.equal(r.placeholderName(n), false, n)
})

test('a channel label is its name, or the plain label with the end of its ID', () => {
  assert.deepEqual(r.channelLabel('#logs', '111222333444', w), { label: '#logs', hidden: false })
  assert.deepEqual(r.channelLabel('logs', '111222333444', w), { label: '#logs', hidden: false })
  assert.deepEqual(r.channelLabel('___hidden___', '111222333444', w), { label: 'hidden …333444', hidden: true })
  assert.deepEqual(r.channelLabel(undefined, '42', w), { label: 'hidden …42', hidden: true })
})

test('a name equal to the channel ID is a placeholder only for that channel', () => {
  assert.equal(r.placeholderName('1100000000000000001', '1100000000000000001'), true)
  assert.equal(r.placeholderName('2024', '1100000000000000001'), false)
})

test('a real channel named only with digits stays a normal channel', () => {
  assert.deepEqual(r.channelLabel('2024', '1100000000000000001', w), { label: '#2024', hidden: false })
  assert.deepEqual(r.channelLabel('1', '1100000000000000001', w), { label: '#1', hidden: false })
})

test('a configured room whose name fell back to its ID (Discord offline) shows its ID, not "cannot see"', () => {
  const id = '1100000000000000001'
  assert.deepEqual(r.channelLabel(id, id, w), { label: `#${id}`, hidden: false })
  assert.deepEqual(r.channelLabel(`#${id}`, id, w), { label: `#${id}`, hidden: false })
})

test('a server named with digits is still a name, a server named by its own ID is not', () => {
  assert.equal(r.serverLabel('2024', '9988776655', w), '2024')
})

test('a server is named, or shown by the end of its ID', () => {
  assert.equal(r.serverLabel('Friends', '9988776655', w), 'Friends')
  assert.equal(r.serverLabel('9988776655', '9988776655', w), 'server …776655')
  assert.equal(r.serverLabel('', '9988776655', w), 'server …776655')
})

test('a room key is read for its server and channel; a Kakao key is not', () => {
  assert.deepEqual(r.parseRoomKey('discord:11:22'), { guild: '11', channel: '22' })
  assert.equal(r.parseRoomKey('kakao:practice'), null)
  assert.equal(r.parseRoomKey('discord:11'), null)
})

const blocks = [
  { source_room: 'discord:1:100', platform: 'discord', message_count: 5, own_channel: { guild_id: '1', channel_id: '100', guild_name: '1', name: '___hidden___' } },
  { source_room: 'discord:1:200', platform: 'discord', message_count: 0 },
  { source_room: 'discord:2:300', platform: 'discord', message_count: 9 },
  { source_room: 'discord:2:301', platform: 'discord', message_count: 3 },
  { source_room: 'kakao:a', platform: 'kakao', label: 'Camping', message_count: 40 },
]
const channels = { guilds: [{ id: '1', name: 'Alpha' }, { id: '2', name: 'Beta' }], channels: [{ guild_id: '1', id: '200', name: 'logs' }, { guild_id: '2', id: '300', name: 'logs' }, { guild_id: '2', id: '301', name: 'chat' }] }

test('rooms are named with their server, the same channel name in two servers stays apart', () => {
  const items = r.roomItems(blocks, channels, w)
  assert.deepEqual(items.map((i) => [i.label, i.server, i.hidden, i.kind]), [
    ['hidden …100', 'Alpha', true, 'discord'],
    ['#logs', 'Alpha', false, 'discord'],
    ['#logs', 'Beta', false, 'discord'],
    ['#chat', 'Beta', false, 'discord'],
    ['Camping', 'Kakao', false, 'kakao'],
  ])
})

test('the picker groups by server, leaves out empty rooms, puts Kakao last and readable rooms first', () => {
  const items = r.roomItems(blocks, channels, w)
  const g = r.groupRooms(items, { showEmpty: false, filter: '' })
  assert.deepEqual(g.map((x) => [x.server, x.items.map((i) => i.label), x.emptyHidden]), [
    ['Alpha', ['hidden …100'], 1],
    ['Beta', ['#logs', '#chat'], 0],
    ['Kakao', ['Camping'], 0],
  ])
  const all = r.groupRooms(items, { showEmpty: true, filter: '' })
  assert.deepEqual(all[0].items.map((i) => i.label), ['#logs', 'hidden …100'])
  assert.deepEqual(r.groupRooms(items, { showEmpty: true, filter: 'beta' }).map((x) => x.server), ['Beta'])
  assert.deepEqual(r.groupRooms(items, { showEmpty: true, filter: 'chat' }).flatMap((x) => x.items.map((i) => i.label)), ['#chat'])
})

test('a hit from a room nobody has named is named from its key', () => {
  const items = r.roomItems(blocks, channels, w)
  assert.deepEqual(r.roomName('discord:2:300', items, w), { label: '#logs', server: 'Beta' })
  assert.deepEqual(r.roomName('discord:77:8899001', items, w), { label: 'hidden …899001', server: 'server …77' })
  assert.deepEqual(r.roomName('kakao:old', items, w), { label: 'old', server: 'Kakao' })
})

test('rooms of a server whose name fell back to IDs stay readable in the picker', () => {
  const offline = [{ source_room: 'discord:7:1100000000000000001', platform: 'discord', message_count: 2 }]
  const items = r.roomItems(offline, { guilds: [{ id: '7', name: '7' }], channels: [{ guild_id: '7', id: '1100000000000000001', name: '1100000000000000001' }] }, w)
  assert.deepEqual(items.map((i) => [i.label, i.hidden, i.server]), [['#1100000000000000001', false, 'server …7']])
})

test('two servers with the same name stay two groups, told apart by the end of their IDs', () => {
  const b = ['g1000001', 'g2000002'].map((g) => ({ source_room: `discord:${g}:${g}c`, message_count: 1 }))
  const guilds = ['g1000001', 'g2000002'].map((id) => ({ id, name: 'Same' }))
  const chans = guilds.map((g) => ({ guild_id: g.id, id: g.id + 'c', name: 'general' }))
  const groups = r.groupRooms(r.roomItems(b, { guilds, channels: chans }, w), { showEmpty: false, filter: '' })
  assert.equal(groups.length, 2)
  assert.deepEqual(groups.map((g) => g.server), ['Same …000001', 'Same …000002'])
  assert.equal(new Set(groups.map((g) => g.id)).size, 2)
  assert.deepEqual(groups.map((g) => g.items.map((i) => i.key)), [['discord:g1000001:g1000001c'], ['discord:g2000002:g2000002c']])
  // a filter that leaves one of the twins does not change its name
  const one = r.groupRooms(r.roomItems(b, { guilds, channels: chans }, w), { showEmpty: false, filter: 'general' })
  assert.equal(one.length, 2)
  assert.deepEqual(r.groupRooms(r.roomItems(b, { guilds, channels: chans }, w), { showEmpty: false, filter: '000002' }).length, 0)
})
