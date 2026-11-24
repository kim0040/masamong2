// Field-level input checks: retention days accept -1 (forever) or whole days, empty or 0 unsets;
// anything else is refused. A timezone name must be one the clock accepts.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const v = await loadLib('validate.ts')

test('retention: an empty field or exactly 0 unsets', () => {
  for (const raw of ['', '   ', '0', '-0', '00']) assert.deepEqual(v.parseRetentionDays(raw), { kind: 'unset' }, JSON.stringify(raw))
})

test('retention: -1 (forever) and whole days are kept', () => {
  assert.deepEqual(v.parseRetentionDays('-1'), { kind: 'set', days: -1 })
  assert.deepEqual(v.parseRetentionDays('30'), { kind: 'set', days: 30 })
  assert.deepEqual(v.parseRetentionDays(' 14 '), { kind: 'set', days: 14 })
})

test('retention: below -1 and non-integers are refused, not rounded', () => {
  for (const raw of ['-2', '-100', '0.4', '0.0', '1.5', 'abc', 'NaN', 'Infinity', '1e2', '0x10', '+30']) {
    assert.deepEqual(v.parseRetentionDays(raw), { kind: 'invalid' }, JSON.stringify(raw))
  }
})

test('a timezone must be a real zone name', () => {
  assert.equal(v.validTimeZone('Asia/Seoul'), true)
  assert.equal(v.validTimeZone('UTC'), true)
  assert.equal(v.validTimeZone('Mars/Olympus'), false)
  assert.equal(v.validTimeZone(''), false)
  assert.equal(v.validTimeZone('not a zone'), false)
  assert.equal(v.validTimeZone('+09:00'), false)
})
