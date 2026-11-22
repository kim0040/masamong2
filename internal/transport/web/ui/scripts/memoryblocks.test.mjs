// Preparing a memory block: the estimate, the budget a person types, and starting the job.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { loadLib } from './lib-load.mjs'

const sent = []
globalThis.__call = async (action, input) => {
  sent.push({ action, input })
  return globalThis.__answer
}
globalThis.__prepare = await loadLib('prepareresult.ts')
const m = await loadLib('memoryblocks.ts', [
  ["import { call } from './api.svelte'", 'const call = (...a) => globalThis.__call(...a)'],
  ["import { prepareStart, type PrepareStart } from './prepareresult'", 'const { prepareStart } = globalThis.__prepare'],
])

test('a budget is a number from 0 to 1000 dollars', () => {
  assert.equal(m.parseBudget('1.50'), 1.5)
  assert.equal(m.parseBudget('0'), 0)
  assert.equal(m.parseBudget('1000'), 1000)
  assert.equal(m.parseBudget('1000.01'), null)
  assert.equal(m.parseBudget('-1'), null)
  assert.equal(m.parseBudget(''), null)
  assert.equal(m.parseBudget('abc'), null)
})

test('the estimate is one dry run that carries both prices', async () => {
  sent.length = 0
  globalThis.__answer = { ok: true, output: { report: { estimate: { total_usd: '0.40' } } } }
  const r = await m.estimatePrepare('kakao:synthetic')
  assert.deepEqual(r, { ok: true, estimate: { report: { estimate: { total_usd: '0.40' } } } })
  assert.deepEqual(sent, [{ action: 'memory.blocks.prepare', input: { source_room: 'kakao:synthetic', dry_run: true, mode: 'batch' } }])
  globalThis.__answer = { ok: false, error: { message: 'no such block' } }
  assert.deepEqual(await m.estimatePrepare('kakao:synthetic'), { ok: false, message: 'no such block' })
  globalThis.__answer = { ok: false }
  assert.deepEqual(await m.estimatePrepare('kakao:synthetic'), { ok: false, message: '' })
})

test('starting with a budget sends it as text with the chosen mode', async () => {
  sent.length = 0
  globalThis.__answer = { ok: true, output: { mode: 'batch' } }
  const r = await m.startPrepare('kakao:synthetic', { budget: 2.5, mode: 'batch' })
  assert.deepEqual(sent[0].input, { source_room: 'kakao:synthetic', background: true, budget_usd: '2.5', mode: 'batch' })
  assert.deepEqual(r, { ok: true, start: { tone: 'ok', key: 'blocks.batch_started', batch: true } })
})

test('the list\'s quick start sends no budget and no mode', async () => {
  sent.length = 0
  globalThis.__answer = { ok: true, output: {} }
  await m.startPrepare('kakao:synthetic')
  assert.deepEqual(sent[0].input, { source_room: 'kakao:synthetic', background: true })
})

test('the answer is read one way for every screen: resumed, already running, batch, live', async () => {
  const start = async (output) => {
    globalThis.__answer = { ok: true, output }
    return (await m.startPrepare('r')).start.key
  }
  assert.equal(await start({ batch_resumed: true }), 'blocks.batch_resumed')
  assert.equal(await start({ already_running: true }), 'blocks.already_running')
  assert.equal(await start({ mode: 'batch' }), 'blocks.batch_started')
  assert.equal(await start({}), 'blocks.prepare_started')
})

test('only a resumed batch or an already running job is news to show where the screen moves on by itself', () => {
  const p = globalThis.__prepare
  assert.equal(p.prepareIsNews(p.prepareStart({ batch_resumed: true })), true)
  assert.equal(p.prepareIsNews(p.prepareStart({ already_running: true })), true)
  assert.equal(p.prepareIsNews(p.prepareStart({ mode: 'batch' })), false)
  assert.equal(p.prepareIsNews(p.prepareStart({})), false)
})

test('a refused start is a failure with the server\'s reason', async () => {
  globalThis.__answer = { ok: false, error: { code: 'BUDGET', message: 'over the budget' } }
  assert.deepEqual(await m.startPrepare('r', { budget: 1, mode: 'live' }), { ok: false, message: 'over the budget' })
})

test('every screen that starts a preparation goes through the one flow (no copy of the three-way answer is left)', () => {
  for (const page of ['pages/Block.svelte', 'pages/Blocks.svelte', 'components/ImportWizard.svelte']) {
    const source = readFileSync(new URL(`../src/${page}`, import.meta.url), 'utf8')
    assert.doesNotMatch(source, /batch_resumed|already_running/, `${page} reads the start answer itself`)
    assert.doesNotMatch(source, /'memory\.blocks\.prepare'/, `${page} calls memory.blocks.prepare itself`)
  }
})
