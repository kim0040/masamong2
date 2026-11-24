// The rules of the Connections page: groups, role states, what may be saved, which models a picker lists.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const c = await loadLib('connections.ts')

test('a time limit is 0 (default) or whole seconds from 5 to 600', () => {
  for (const ok of [0, 5, 90, 600]) assert.equal(c.validTimeout(ok), true, String(ok))
  for (const bad of [1, 4, 601, 1.5, NaN, -5]) assert.equal(c.validTimeout(bad), false, String(bad))
})

test('a typed price: blank is 0, a number up to 1000 is kept, anything else is refused', () => {
  assert.equal(c.parseUsd(''), 0)
  assert.equal(c.parseUsd('  '), 0)
  assert.equal(c.parseUsd('0.14'), 0.14)
  assert.equal(c.parseUsd('1000'), 1000)
  assert.equal(c.parseUsd('1000.5'), null)
  assert.equal(c.parseUsd('-1'), null)
  assert.equal(c.parseUsd('abc'), null)
})

test('the time limit typed: blank is 0', () => {
  assert.equal(c.parseSeconds(''), 0)
  assert.equal(c.parseSeconds('30'), 30)
  assert.ok(Number.isNaN(c.parseSeconds('x')))
})

const caps = { openrouter: { api: 'openrouter', rerank: false }, deepseek: { api: 'compatible', rerank: false } }
const webKinds = ['tavily', 'exa']

test('a service is a model provider (also the practice one), a web service, or data', () => {
  assert.equal(c.groupOf({ id: 'openrouter', kind: 'openrouter' }, caps, webKinds), 'model')
  assert.equal(c.groupOf({ id: 'practice', kind: 'practice' }, caps, webKinds), 'model')
  assert.equal(c.groupOf({ id: 'exa', kind: 'exa' }, caps, webKinds), 'web')
  assert.equal(c.groupOf({ id: 'kma', kind: 'kma' }, caps, webKinds), 'data')
})

test('splitGroups puts services that wait for a key on their own line, per group', () => {
  const list = [{ id: 'openrouter', kind: 'openrouter' }, { id: 'exa', kind: 'exa' }, { id: 'tavily', kind: 'tavily' }, { id: 'kma', kind: 'kma' }]
  const out = c.splitGroups(list, (p) => c.groupOf(p, caps, webKinds), (p, g) => g !== 'model' && p.id === 'tavily')
  assert.deepEqual(out.model.cards.map((p) => p.id), ['openrouter'])
  assert.deepEqual(out.web.cards.map((p) => p.id), ['exa'])
  assert.deepEqual(out.web.waiting.map((p) => p.id), ['tavily'])
  assert.deepEqual(out.data.cards.map((p) => p.id), ['kma'])
  assert.deepEqual(out.model.waiting, [])
})

const byId = { a: { configured: true }, b: { configured: false } }

test('a role can answer when it has a model, the page has read the providers, and its provider has a key that was not refused', () => {
  assert.equal(c.roleState(undefined, byId, {}), 'unset')
  assert.equal(c.roleState({ provider: 'a' }, byId, {}), 'unset')
  assert.equal(c.roleState({ provider: 'a', model: 'm' }, byId, undefined), 'unknown')
  assert.equal(c.roleState({ provider: 'b', model: 'm' }, byId, {}), 'no_key')
  assert.equal(c.roleState({ provider: 'zzz', model: 'm' }, byId, {}), 'no_key')
  assert.equal(c.roleState({ provider: 'a', model: 'm' }, byId, { health: { a: { rejected: true } } }), 'rejected')
  assert.equal(c.roleState({ provider: 'a', model: 'm' }, byId, { health: {} }), 'ok')
})

test('blocked roles are listed once per provider that holds them up, in role order', () => {
  const state = { response: 'no_key', planner: 'no_key', summary: 'ok', embed: 'rejected', rerank: 'unset' }
  const provider = { response: 'b', planner: 'b', summary: 'a', embed: 'c', rerank: 'a' }
  const out = c.blockedRoles(['response', 'planner', 'summary', 'embed', 'rerank'], (r) => state[r], (r) => provider[r])
  assert.deepEqual(out, [{ id: 'b', rejected: false, roles: ['response', 'planner'] }, { id: 'c', rejected: true, roles: ['embed'] }])
})

test('rolesUsing lists the roles a provider serves', () => {
  assert.deepEqual(c.rolesUsing({ response: { provider: 'a' }, embed: { provider: 'b' }, summary: { provider: 'a' } }, 'a'), ['response', 'summary'])
  assert.deepEqual(c.rolesUsing(undefined, 'a'), [])
})

test('the reasoning levels: ones the model lacks are visible but not pickable, a saved one stays pickable', () => {
  const levels = (r, current) => Object.fromEntries(c.effortLevels(r, current).map((l) => [l.value, l.disabled]))
  assert.equal(levels(undefined, '').high, false, 'nothing known: all open')
  assert.deepEqual(Object.entries(levels({ reasoning: false }, '')).filter(([, d]) => !d).map(([v]) => v), ['none'])
  assert.equal(levels({ reasoning: true, efforts: ['low', 'high'] }, '').medium, true)
  assert.equal(levels({ reasoning: true, efforts: ['low', 'high'] }, '').high, false)
  assert.equal(levels({ reasoning: true, efforts: ['low', 'high'] }, 'medium').medium, false, 'a level already saved stays pickable')
})

test('reasonKey words a model as thinking, not thinking, or unknown', () => {
  assert.equal(c.reasonKey({ reasoning: true }), 'conn.reason_yes')
  assert.equal(c.reasonKey({ reasoning: false }), 'conn.reason_no')
  assert.equal(c.reasonKey({}), 'conn.reason_unknown')
  assert.equal(c.reasonKey(undefined), 'conn.reason_unknown')
})

test('which roles take a time limit and a reasoning level', () => {
  assert.deepEqual(['response', 'planner', 'summary', 'vision', 'embed', 'rerank', 'image'].map(c.timeoutRole), [true, true, false, true, false, false, false])
  assert.deepEqual(['response', 'planner', 'summary', 'vision', 'embed', 'rerank', 'image'].map(c.effortRole), [true, true, true, true, false, false, false])
})

test('the price dialog sets what is filled in and removes what is blank or zero', () => {
  assert.deepEqual(c.priceOps('response', { in: 0.14, out: 0, cached: 0.014, secs: 90, effort: 'high' }), [
    { path: ['models', 'response', 'price_in_per_mtok'], value: 0.14 },
    { path: ['models', 'response', 'price_out_per_mtok'], op: 'unset' },
    { path: ['models', 'response', 'price_cached_in_per_mtok'], value: 0.014 },
    { path: ['models', 'response', 'timeout_seconds'], value: 90 },
    { path: ['models', 'response', 'effort'], value: 'high' },
  ])
  // an embedding model has neither a time limit nor a level in this dialog
  assert.equal(c.priceOps('embed', { in: 1, out: 0, cached: 0, secs: 30, effort: 'low' }).length, 3)
  // summary takes a level but no time limit
  assert.deepEqual(c.priceOps('summary', { in: 0, out: 0, cached: 0, secs: 0, effort: '' }).slice(3), [{ path: ['models', 'summary', 'effort'], op: 'unset' }])
})

const row = (o) => ({ provider: 'a', model: 'm1', price_in: '', price_out: '', timeout: '', effort: '', ...o })

test('fallbacks: each row needs a provider and a model, and may not repeat the role\'s own model or an earlier row', () => {
  const primary = { provider: 'a', model: 'main' }
  assert.deepEqual(c.buildFallbacks([row({ model: ' ' })], primary), { ok: false, problem: { key: 'conn.fb_need_model' } })
  assert.deepEqual(c.buildFallbacks([row({ provider: '' })], primary), { ok: false, problem: { key: 'conn.fb_need_model' } })
  assert.deepEqual(c.buildFallbacks([row({ model: 'main' })], primary), { ok: false, problem: { key: 'conn.fb_duplicate', model: 'main' } })
  assert.deepEqual(c.buildFallbacks([row(), row()], primary), { ok: false, problem: { key: 'conn.fb_duplicate', model: 'm1' } })
  // the same model on another provider is a different fallback
  assert.equal(c.buildFallbacks([row(), row({ provider: 'b' })], primary).ok, true)
})

test('fallbacks: a bad price or time limit is named, a good row keeps only what was filled in', () => {
  const primary = { provider: 'a', model: 'main' }
  assert.deepEqual(c.buildFallbacks([row({ price_in: 'x' })], primary), { ok: false, problem: { key: 'conn.price_invalid' } })
  assert.deepEqual(c.buildFallbacks([row({ price_out: '2000' })], primary), { ok: false, problem: { key: 'conn.price_invalid' } })
  assert.deepEqual(c.buildFallbacks([row({ timeout: '3' })], primary), { ok: false, problem: { key: 'conn.timeout_invalid' } })
  assert.deepEqual(c.buildFallbacks([row({ model: ' m2 ', price_in: '0.5', timeout: '60', effort: 'low' })], primary), {
    ok: true,
    list: [{ provider: 'a', model: 'm2', price_in_per_mtok: 0.5, timeout_seconds: 60, effort: 'low' }],
  })
  assert.deepEqual(c.buildFallbacks([row()], primary), { ok: true, list: [{ provider: 'a', model: 'm1' }] })
  assert.deepEqual(c.buildFallbacks([], undefined), { ok: true, list: [] })
})

test('the fallbacks are stored as one list, an empty list removes them, and the dialog starts from what is saved', () => {
  assert.deepEqual(c.fallbackOps('response', [{ provider: 'a', model: 'm' }]), [{ path: ['models', 'response', 'fallbacks'], value: [{ provider: 'a', model: 'm' }] }])
  assert.deepEqual(c.fallbackOps('response', []), [{ path: ['models', 'response', 'fallbacks'], op: 'unset' }])
  assert.deepEqual(c.fallbackRows([{ provider: 'a', model: 'm', price_in_per_mtok: 0.5, timeout_seconds: 60, effort: 'low' }, { provider: 'b', model: 'n' }]), [
    { provider: 'a', model: 'm', price_in: '0.5', price_out: '', timeout: '60', effort: 'low' },
    { provider: 'b', model: 'n', price_in: '', price_out: '', timeout: '', effort: '' },
  ])
  assert.deepEqual(c.fallbackRows(undefined), [])
})

const models = [
  { id: 'chat-a', name: 'Chat A', kind: 'chat', in_per_mtok: 3, out_per_mtok: 9, context_length: 8000 },
  { id: 'chat-b', name: 'Chat B', kind: 'chat', in_per_mtok: 1, out_per_mtok: 1, context_length: 128000, image_input: true },
  { id: 'chat-c', name: 'Chat C', kind: 'chat', context_length: 32000 },
  { id: 'emb-a', name: 'Embed A', kind: 'embedding' },
  { id: 'mystery', name: 'Mystery', kind: 'unknown', image_input: false },
  { id: 'pic', name: 'Picture', kind: 'chat', image_output: true },
]

test('the picker lists the role\'s kind (and models of unknown kind), narrowed by the search', () => {
  assert.deepEqual(c.filterModels(models, 'response', '', 'list').map((m) => m.id), ['chat-a', 'chat-b', 'chat-c', 'mystery', 'pic'])
  assert.deepEqual(c.filterModels(models, 'embed', '', 'list').map((m) => m.id), ['emb-a', 'mystery'])
  assert.deepEqual(c.filterModels(models, 'response', 'zzz', 'list').map((m) => m.id), [])
  assert.deepEqual(c.filterModels(models, 'response', 'CHAT B', 'list').map((m) => m.id), ['chat-b'])
  // a picture model is also offered for the image role even though its kind is chat
  assert.deepEqual(c.filterModels(models, 'image', '', 'list').map((m) => m.id), ['mystery', 'pic'])
})

test('the vision role hides only models the list says take no pictures, and picture generators', () => {
  assert.deepEqual(c.filterModels(models, 'vision', '', 'list').map((m) => m.id), ['chat-a', 'chat-b', 'chat-c'])
})

test('price order puts models without a price last either way; context order is longest first', () => {
  assert.deepEqual(c.filterModels(models, 'response', '', 'price_asc').map((m) => m.id).slice(0, 2), ['chat-b', 'chat-a'])
  assert.deepEqual(c.filterModels(models, 'response', '', 'price_desc').map((m) => m.id).slice(0, 2), ['chat-a', 'chat-b'])
  assert.deepEqual(c.filterModels(models, 'response', '', 'price_asc').slice(-3).map((m) => m.id).sort(), ['chat-c', 'mystery', 'pic'])
  assert.deepEqual(c.filterModels(models, 'response', '', 'ctx').map((m) => m.id).slice(0, 3), ['chat-b', 'chat-c', 'chat-a'])
})

test('at most 80 models are listed', () => {
  const many = Array.from({ length: 200 }, (_, i) => ({ id: `m${i}`, name: `M${i}`, kind: 'chat' }))
  assert.equal(c.filterModels(many, 'response', '', 'list').length, 80)
})

test('the blended price weighs the input three to one over the output', () => {
  assert.equal(c.blended({ in_per_mtok: 4, out_per_mtok: 8 }), 5)
  assert.equal(c.blended({ in_per_mtok: 4 }), 3)
  assert.equal(c.blended({}), null)
})

test('a role\'s reasoning level alone: set, cleared to the default, or not offered', () => {
  assert.deepEqual(c.effortOps('planner', 'high'), [{ path: ['models', 'planner', 'effort'], value: 'high' }])
  assert.deepEqual(c.effortOps('response', ''), [{ path: ['models', 'response', 'effort'], op: 'unset' }])
  assert.deepEqual(c.effortOps('embed', 'high'), [])
  assert.deepEqual(c.effortOps('rerank', 'low'), [])
})

test('a saved level fits a model when the list does not rule it out', () => {
  assert.equal(c.effortFits({ reasoning: true, efforts: ['low', 'high'] }, 'high'), true)
  assert.equal(c.effortFits({ reasoning: true, efforts: ['low', 'high'] }, 'max'), false)
  assert.equal(c.effortFits({ reasoning: false }, 'low'), false)
  assert.equal(c.effortFits({ reasoning: false }, 'none'), true)
  assert.equal(c.effortFits({}, 'xhigh'), true)
  assert.equal(c.effortFits({ reasoning: true, efforts: ['low'] }, ''), true)
})
