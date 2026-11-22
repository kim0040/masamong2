// Chart colors and evidence packing: outcome colors stay unique per outcome, tones warn only on
// limits and failures, and a memory hit says which earlier hit already showed its messages.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

function load(file, replacements = []) {
  let source = readFileSync(new URL(`../src/lib/${file}`, import.meta.url), 'utf8')
  for (const [from, to] of replacements) {
    assert.ok(source.includes(from), `${file} no longer contains: ${from}`)
    source = source.replace(from, to)
  }
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
  return import(`data:text/javascript,${encodeURIComponent(js)}#${Math.random()}`)
}

test('every known outcome keeps its own color and unknowns cycle the spares', async () => {
  const { outcomeColor } = await load('outcomes.ts')
  const known = ['answered', 'silent', 'limited', 'budget', 'paused', 'disabled', 'error', 'not_addressed', 'reacted', 'canceled', 'no_vision']
  assert.equal(new Set(known.map((o) => outcomeColor(o))).size, known.length, 'a new outcome must not borrow a color')
  assert.equal(outcomeColor('answered'), 'var(--chart-1)')
  assert.equal(outcomeColor('future_outcome', 0), outcomeColor('future_outcome', 5), 'spare colors wrap around')
  assert.notEqual(outcomeColor('future_a', 0), outcomeColor('future_b', 1))
})

test('outcomeTone: answered ok, error danger, deliberate stays-out neutral, limits warn', async () => {
  const { outcomeTone } = await load('outcomes.ts')
  assert.equal(outcomeTone('answered'), 'ok')
  assert.equal(outcomeTone('error'), 'danger')
  for (const o of ['silent', 'not_addressed', 'reacted', 'canceled']) assert.equal(outcomeTone(o), 'neutral', o)
  for (const o of ['limited', 'budget', 'paused', 'disabled', 'no_vision']) assert.equal(outcomeTone(o), 'warn', o)
})

test('shownIn defers to the server rank, then finds the earlier hit that showed the messages', async () => {
  const { shownIn } = await load('evidence.ts')
  assert.equal(shownIn([], 0), 0)
  assert.equal(shownIn([{ leaf_shown_in: 3 }], 0), 3, 'the server already named the earlier hit')
  const hits = [
    { rank: 1, visible_observation_ids: ['o1', 'o2'] },
    { rank: 2, leaf_observation_ids: ['o2'], visible_observation_ids: ['o3'] },
    { rank: 3, leaf_observation_ids: ['o9'], visible_observation_ids: [] },
  ]
  assert.equal(shownIn(hits, 1), 1, 'o2 was already shown by the first hit')
  assert.equal(shownIn(hits, 2), 0, 'a hit no earlier hit covered shows its own messages')
})

test('shownIn counts a hit as its own when its own leaf is among the visible ids', async () => {
  const { shownIn } = await load('evidence.ts')
  const hits = [
    { rank: 1, visible_observation_ids: ['o1'] },
    { rank: 2, leaf_observation_ids: ['o5'], visible_observation_ids: ['o5', 'o6'] },
    { leaf_observation_ids: [], visible_observation_ids: ['o7'] },
  ]
  assert.equal(shownIn(hits, 1), 0)
  assert.equal(shownIn(hits, 2), 0, 'no leaf ids means nothing was deferred')
})

test('shownIn falls back to the earlier position when the earlier hit has no rank', async () => {
  const { shownIn } = await load('evidence.ts')
  const hits = [
    { visible_observation_ids: ['o1'] },
    { leaf_observation_ids: ['o1'], visible_observation_ids: [] },
  ]
  assert.equal(shownIn(hits, 1), 1)
})
