// "Latest answer wins": a request that was overtaken by a newer one is dropped, whatever order the
// answers come back in.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const { latest } = await loadLib('seqload.ts')

// a promise that is settled by hand, so a test chooses the order the answers arrive in
function later() {
  let done
  const p = new Promise((resolve) => (done = resolve))
  return { p, done }
}

test('a ticket is live until a newer one is taken', () => {
  const l = latest()
  const first = l.start()
  assert.equal(first(), true)
  const second = l.start()
  assert.equal(first(), false)
  assert.equal(second(), true)
})

test('cancel makes every ticket stale and does not take one', () => {
  const l = latest()
  const a = l.start()
  l.cancel()
  assert.equal(a(), false)
  const b = l.start()
  assert.equal(b(), true)
})

test('two latests do not disturb each other', () => {
  const a = latest()
  const b = latest()
  const ta = a.start()
  b.start()
  b.start()
  assert.equal(ta(), true)
})

test('run: an old answer that arrives after a newer one is stale, in either arrival order', async () => {
  const l = latest()
  const slow = later()
  const fast = later()
  const first = l.run(() => slow.p)
  const second = l.run(() => fast.p)
  // the newer request answers first, then the older one limps in
  fast.done('new')
  slow.done('old')
  assert.deepEqual(await second, { stale: false, value: 'new' })
  assert.deepEqual(await first, { stale: true })
})

test('run: the old answer first, then the new one: only the new one counts', async () => {
  const l = latest()
  const a = later()
  const b = later()
  const first = l.run(() => a.p)
  const second = l.run(() => b.p)
  a.done('old')
  b.done('new')
  assert.deepEqual(await first, { stale: true })
  assert.deepEqual(await second, { stale: false, value: 'new' })
})

test('run: a lone request is never stale', async () => {
  const l = latest()
  assert.deepEqual(await l.run(async () => 7), { stale: false, value: 7 })
})

test('run: cancel while a request is away makes its answer stale', async () => {
  const l = latest()
  const a = later()
  const r = l.run(() => a.p)
  l.cancel()
  a.done(1)
  assert.deepEqual(await r, { stale: true })
})
