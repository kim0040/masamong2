// Presentation logic for the server-owned tool list: grouping, form<->JSON mapping, status badges.
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

const strings = {
  'tools.state.ready': 'ready',
  'tools.state.no_key': 'needs a key',
  'tools.state.needs_model': 'needs a model',
  'toolp.search.web.query': 'what to search',
  'toolp.query': 'a question',
  'toolp.ex.search.web.query': 'snow in Seoul',
  'toolp.search.web.kind.images': 'pictures',
}
globalThis.__i18n = { has: (k) => k in strings, t: (k) => strings[k] ?? k }

const meta = () => load('toolmeta.ts', [["import { t, has } from './i18n.svelte'", 'const { t, has } = globalThis.__i18n']])

test('toolState maps a server readiness reason to a badge, unknown reasons to needs-model', async () => {
  const { toolState } = await meta()
  assert.deepEqual(toolState({ ready: true }), { tone: 'ok', label: 'ready' })
  assert.deepEqual(toolState({ ready: false, ready_reason: 'no_key' }), { tone: 'warn', label: 'needs a key' })
  assert.equal(toolState({ ready: false, ready_reason: 'whatever_new' }).label, 'needs a model')
  assert.equal(toolState({ ready: false, ready_reason: 'whatever_new' }).tone, 'warn')
})

test('toolNeedsAttention: only an enabled, unready tool whose reason warns, not an empty memory', async () => {
  const { toolNeedsAttention } = await meta()
  assert.equal(toolNeedsAttention({ enabled: true, ready: false, ready_reason: 'no_key' }), true)
  assert.equal(toolNeedsAttention({ enabled: true, ready: false, ready_reason: 'no_messages' }), false)
  assert.equal(toolNeedsAttention({ enabled: false, ready: false, ready_reason: 'no_key' }), false)
  assert.equal(toolNeedsAttention({ enabled: true, ready: true }), false)
})

test('input labels prefer the tool wording, then the generic wording, then the spaced name', async () => {
  const { inputLabel, optionLabel, inputExample } = await meta()
  assert.equal(inputLabel('search.web', 'query'), 'what to search')
  assert.equal(inputLabel('other.tool', 'query'), 'a question')
  assert.equal(inputLabel('other.tool', 'page_size'), 'page size')
  assert.equal(optionLabel('search.web', 'kind', 'images'), 'pictures')
  assert.equal(optionLabel('search.web', 'kind', 'news'), 'news')
  assert.equal(inputExample('search.web', { name: 'query' }), 'snow in Seoul')
  assert.equal(inputExample('other.tool', { name: 'q', example: 'srv' }), 'srv')
  assert.equal(inputExample('other.tool', { name: 'q' }), '')
})

test('toValues keeps bools as bools and strings everything else, skipping gaps', async () => {
  const { toValues } = await meta()
  const inputs = [
    { name: 'q', type: 'text' }, { name: 'n', type: 'number' },
    { name: 'flag', type: 'bool' }, { name: 'missing', type: 'text' },
  ]
  assert.deepEqual(toValues(inputs, { q: 'hi', n: 3, flag: true, missing: undefined }), { q: 'hi', n: '3', flag: true })
  assert.deepEqual(toValues(inputs, undefined), {})
})

test('toObject trims, converts numbers, keeps base keys the form does not show, reports a missing required', async () => {
  const { toObject } = await meta()
  const inputs = [
    { name: 'q', type: 'text', required: true }, { name: 'n', type: 'number' },
    { name: 'kind', type: 'select' }, { name: 'flag', type: 'bool' },
  ]
  const r = toObject(inputs, { q: '  hello ', n: '42', kind: '7', flag: false }, { extra: 'kept', q: 'old' })
  assert.deepEqual(r.obj, { extra: 'kept', q: 'hello', n: 42, kind: 7, flag: false })
  assert.equal(r.missing, undefined)
  const miss = toObject(inputs, {}, {}, true)
  assert.equal(miss.missing, 'q')
  // Without requireAll an empty required input is simply absent, and a bool never turns into ''.
  assert.deepEqual(toObject(inputs, { flag: '' }).obj, {})
})

test('a list input keeps its array through the form: saved defaults survive a change to another field', async () => {
  const { toValues, toObject } = await meta()
  const inputs = [
    { name: 'question', type: 'text', required: true }, { name: 'options', type: 'list', max: 10 },
    { name: 'duration_hours', type: 'number' },
  ]
  const saved = { options: ['Tea', 'Coffee'], duration_hours: 24 }
  const vals = toValues(inputs, saved)
  assert.equal(typeof vals.options, 'string')
  assert.deepEqual(toObject(inputs, vals, saved).obj, saved, 'untouched, the saved object comes back as it was')
  // the person changes the duration only
  const changed = toObject(inputs, { ...vals, duration_hours: '48' }, saved).obj
  assert.deepEqual(changed, { options: ['Tea', 'Coffee'], duration_hours: 48 })
  // typed in the form: one entry per comma, blanks dropped; emptied, the key goes
  assert.deepEqual(toObject(inputs, { options: ' Tea , ,Coffee, Milk ' }).obj, { options: ['Tea', 'Coffee', 'Milk'] })
  assert.deepEqual(toObject(inputs, { options: ' ' }, { options: ['Tea'] }).obj, {})
  // entries the form cannot show (objects with an id and a label) are kept, not flattened
  const rich = { options: [{ id: 'a', label: 'Tea' }, { id: 'b', label: 'Coffee' }] }
  assert.deepEqual(toValues(inputs, rich), {})
  assert.deepEqual(toObject(inputs, toValues(inputs, rich), rich).obj, rich)
})

test('exampleInput fills from the sample and localized examples, never a provider input', async () => {
  const { exampleInput } = await meta()
  const inputs = [
    { name: 'query', type: 'text', required: true },
    { name: 'n', type: 'number', min: 5 },
    { name: 'kind', type: 'select', options: ['10', '20'] },
    { name: 'flag', type: 'bool' },
    { name: 'who', type: 'provider' },
  ]
  const out = exampleInput('search.web', inputs)
  assert.equal(out.query, 'snow in Seoul')
  assert.equal(out.n, 5)
  assert.equal(out.kind, 10, 'a numeric option stays a number')
  assert.equal(out.flag, false)
  assert.equal('who' in out, false)
  assert.equal(exampleInput('search.web', inputs, { query: 'preset' }).query, 'preset')
})

test('toolGroup: custom HTTP tools, unimplemented later, then the curated list with an info default', async () => {
  const { toolGroup } = await meta()
  assert.equal(toolGroup({ id: 'weather.query', kind: 'http', implemented: true }), 'custom')
  assert.equal(toolGroup({ id: 'weather.query', implemented: false }), 'later')
  assert.equal(toolGroup({ id: 'weather.query', implemented: true }), 'info')
  assert.equal(toolGroup({ id: 'polls.vote', implemented: true }), 'internal')
  assert.equal(toolGroup({ id: 'brand.new', implemented: true }), 'info')
})
