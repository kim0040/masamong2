// How rooms are named and grouped: the placeholder Discord sends for a channel the bot cannot see, bare numbers,
// the server each room belongs to, and what a picker leaves out.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const r = await loadLib('rooms.ts')
const w = { hidden: (t) => `hidden …${t}`, server: (t) => `server …${t}`, kakao: 'Kakao' }

test('a name that names nothing is a placeholder', () => {
  for (const n of ['___hidden___', '#___hidden___', '__hidden__', '___HIDDEN___', '', '  ', '#', '123456', '#123456', '______', null, undefined]) assert.equal(r.placeholderName(n), true, String(n))
  for (const n of ['logs', '#general', 'hidden', 'hidden-room', 'a_hidden_b', '12-general']) assert.equal(r.placeholderName(n), false, n)
})

test('a channel label is its name, or the plain label with the end of its ID', () => {
  assert.deepEqual(r.channelLabel('#logs', '111222333444', w), { label: '#logs', hidden: false })
  assert.deepEqual(r.channelLabel('logs', '111222333444', w), { label: '#logs', hidden: false })
  assert.deepEqual(r.channelLabel('___hidden___', '111222333444', w), { label: 'hidden …333444', hidden: true })
  assert.deepEqual(r.channelLabel(undefined, '42', w), { label: 'hidden …42', hidden: true })
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
