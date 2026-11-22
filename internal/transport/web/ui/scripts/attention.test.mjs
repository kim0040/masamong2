// Notification read-state: fingerprints ignore volatile detail, read marks expire after a week,
// resolved items are pruned, and the store is capped.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

function load(file, replacements) {
  let source = readFileSync(new URL(`../src/lib/${file}`, import.meta.url), 'utf8')
  for (const [from, to] of replacements) {
    assert.ok(source.includes(from), `${file} no longer contains: ${from}`)
    source = source.replace(from, to)
  }
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
  return import(`data:text/javascript,${encodeURIComponent(js)}#${Math.random()}`)
}

globalThis.__i18n = {
  has: (k) => k in { 'conn.provider.fake': 1, 'role.chat': 1 },
  t: (k, args) => (args ? `${k}(${JSON.stringify(args)})` : k),
  toolName: (id) => `tool:${id}`,
}
// A mutable prefs store like prefs.svelte's, recording every write. The module binds `prefs` once,
// so tests reset by mutating this object, never by replacing it.
const store = { writes: [] }
globalThis.__prefs = {
  prefs: {},
  setPrefs: (patch) => { store.writes.push(patch); Object.assign(globalThis.__prefs.prefs, patch) },
}
const reset = (read) => {
  for (const k of Object.keys(globalThis.__prefs.prefs)) delete globalThis.__prefs.prefs[k]
  if (read) globalThis.__prefs.prefs.attention_read = read
  store.writes = []
}
const mod = () => load('attention.svelte.ts', [
  ["import { t, has, toolName } from './i18n.svelte'", 'const { t, has, toolName } = globalThis.__i18n'],
  ["import { prefs, setPrefs } from './prefs.svelte'", 'const { prefs, setPrefs } = globalThis.__prefs'],
])

const item = (over = {}) => ({ severity: 'warn', key: 'low_budget', route: '/usage', args: { channel_id: 'c1' }, ...over })

test('the fingerprint ignores volatile args but tracks identity args', async () => {
  const { attentionFingerprint } = await mod()
  const base = item()
  assert.equal(attentionFingerprint(base), attentionFingerprint(item({ args: { channel_id: 'c1', count: 3, pct: 91 } })), 'counts and percentages do not reopen an issue')
  assert.notEqual(attentionFingerprint(base), attentionFingerprint(item({ args: { channel_id: 'c2' } })))
  assert.notEqual(attentionFingerprint(base), attentionFingerprint(item({ key: 'other' })))
  // Object key order does not matter.
  assert.equal(
    attentionFingerprint(item({ args: { channel_id: 'c1', tool: 'a.b' } })),
    attentionFingerprint(item({ args: { tool: 'a.b', channel_id: 'c1' } })),
  )
})

test('a read mark applies while fresh and expires after a week', async () => {
  const { isAttentionRead, markAttentionRead, attentionFingerprint } = await mod()
  reset()
  const a = item()
  assert.equal(isAttentionRead(a), false)
  markAttentionRead(a)
  assert.equal(isAttentionRead(a), true)
  const nineDaysAgo = new Date(Date.now() - 9 * 24 * 60 * 60 * 1000).toISOString()
  reset({ [attentionFingerprint(a)]: nineDaysAgo })
  assert.equal(isAttentionRead(a), false, 'a week-old read mark no longer counts')
})

test('markAllAttentionRead stamps every item once', async () => {
  const { markAllAttentionRead, isAttentionRead } = await mod()
  reset()
  const items = [item(), item({ args: { channel_id: 'c2' } })]
  markAllAttentionRead(items)
  assert.ok(items.every(isAttentionRead))
})

test('syncAttentionReads prunes resolved and expired ids and stays quiet when nothing changed', async () => {
  const { syncAttentionReads, attentionFingerprint } = await mod()
  const live = item()
  const now = new Date().toISOString()
  const old = new Date(Date.now() - 9 * 24 * 60 * 60 * 1000).toISOString()
  reset({ [attentionFingerprint(live)]: now, gone_issue: now, stale: old })
  syncAttentionReads([live])
  assert.deepEqual(globalThis.__prefs.prefs.attention_read, { [attentionFingerprint(live)]: now })
  assert.equal(store.writes.length, 1)
  store.writes = []
  syncAttentionReads([live])
  assert.equal(store.writes.length, 0, 'no write when the store is already clean')
})

test('the read store is capped at the newest 200 entries', async () => {
  const { markAttentionRead } = await mod()
  const old = new Date(Date.now() - 60 * 1000).toISOString()
  reset(Object.fromEntries(Array.from({ length: 220 }, (_, i) => [`a-old${i}`, old])))
  markAttentionRead(item())
  assert.equal(Object.keys(globalThis.__prefs.prefs.attention_read).length, 200)
})

test('attentionText localizes tool ids, provider ids and roles, leaving unknown ids as-is', async () => {
  const { attentionText } = await mod()
  const out = attentionText({ severity: 'warn', key: 'k', route: '/x', args: { tool: 'memory.search', provider: 'fake / unknown', roles: 'chat,other' } })
  const args = JSON.parse(out.slice(out.indexOf('(') + 1, -1))
  assert.equal(args.tool, 'tool:memory.search')
  assert.equal(args.provider, 'conn.provider.fake / unknown')
  assert.equal(args.roles, 'role.chat, other')
})
