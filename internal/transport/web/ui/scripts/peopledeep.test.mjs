// A /people deep link opens the exact person it names, says so when nothing matches, and survives login.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const d = await loadLib('peopledeep.ts')
const servers = [
  { guild_id: 'A', name: 'Alpha', people: [{ user_id: '2', name: 'Bo' }] },
  { guild_id: 'B', name: 'Beta', people: [{ user_id: '1', name: 'Ann' }, { user_id: '2', name: 'Bo' }] },
]

test('a link that names a server opens that server, not another one that has the same person', () => {
  assert.deepEqual(d.deepPerson(servers, '2', 'B'), { user_id: '2', name: 'Bo', guild_id: 'B', guild_name: 'Beta' })
  assert.equal(d.deepPerson(servers, '1', 'A'), null)
})

test('a link without a server takes the first server that has the person', () => {
  assert.equal(d.deepPerson(servers, '2', '')?.guild_id, 'A')
  assert.equal(d.deepPerson(servers, '1', '')?.guild_id, 'B')
})

test('nothing matches: null, whatever the answer looks like', () => {
  assert.equal(d.deepPerson(servers, '9', ''), null)
  assert.equal(d.deepPerson([], '1', ''), null)
  assert.equal(d.deepPerson(undefined, '1', ''), null)
  assert.equal(d.deepPerson([{ guild_id: 'A' }], '1', 'A'), null)
})

test('the address kept through login carries its query', () => {
  assert.equal(d.pathWithQuery('/people', new URLSearchParams('person=1&guild=A')), '/people?person=1&guild=A')
  assert.equal(d.pathWithQuery('/people', new URLSearchParams('')), '/people')
  assert.equal(d.pathWithQuery('/people', undefined), '/people')
  const next = '/login?next=' + encodeURIComponent(d.pathWithQuery('/people', new URLSearchParams('person=1&guild=A')))
  assert.equal(new URL(next, 'http://x').searchParams.get('next'), '/people?person=1&guild=A')
})
