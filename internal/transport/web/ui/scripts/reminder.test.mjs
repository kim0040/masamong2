import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

const source = readFileSync(new URL('../src/lib/reminder.ts', import.meta.url), 'utf8')
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
const { repeatText } = await import(`data:text/javascript,${encodeURIComponent(js)}`)

const en = {
  'rem.repeat.daily': 'Every day at {time}',
  'rem.repeat.weekly': 'Every {days} at {time}',
  'rem.repeat.monthly': 'Day {day} of every month at {time}',
  'rem.repeat.monthly_last': 'Day {day} of every month at {time} (last day in short months)',
  'rem.repeat.hours': 'Every {n} hours',
  'rem.repeat.until': 'until {date}',
  'rem.repeat.count': '{n} times in all',
  'weekday.1': 'Mon',
  'weekday.3': 'Wed',
}
const tr = (key, params = {}) => en[key].replace(/\{(\w+)\}/g, (_, k) => String(params[k]))
// the console's formatter: the date in the reminder's own zone
const until = (iso, tz) => new Intl.DateTimeFormat('en-CA', { timeZone: tz || 'UTC' }).format(new Date(iso))

test('a repeating reminder reads as its rule with the end', () => {
  assert.equal(repeatText({ repeat: { kind: 'daily', time: '08:00' } }, tr, until), 'Every day at 08:00')
  assert.equal(repeatText({ repeat: { kind: 'weekly', time: '09:00', weekdays: [1, 3] } }, tr, until), 'Every Mon, Wed at 09:00')
  assert.equal(repeatText({ repeat: { kind: 'monthly', time: '10:00', day: 15 } }, tr, until), 'Day 15 of every month at 10:00')
  assert.match(repeatText({ repeat: { kind: 'monthly', time: '10:00', day: 31 } }, tr, until), /last day/)
  assert.equal(repeatText({ repeat: { kind: 'hours', every: 6 }, repeat_until: '2026-12-31T14:59:59Z', repeat_max: 3 }, tr, until), 'Every 6 hours · until 2026-12-31 · 3 times in all')
})

test('the rule names the reminder zone and the end is a date in that zone', () => {
  assert.equal(repeatText({ timezone: 'Asia/Seoul', repeat: { kind: 'daily', time: '08:00' } }, tr, until), 'Every day at 08:00 (Asia/Seoul)')
  // the end moment is 23:59:59 of the last day in the reminder zone; in UTC (Los Angeles case) it is already the next day
  assert.equal(repeatText({ timezone: 'Asia/Seoul', repeat: { kind: 'daily', time: '08:00' }, repeat_until: '2026-12-31T14:59:59Z' }, tr, until), 'Every day at 08:00 (Asia/Seoul) · until 2026-12-31')
  assert.equal(repeatText({ timezone: 'America/Los_Angeles', repeat: { kind: 'daily', time: '08:00' }, repeat_until: '2027-01-01T07:59:59Z' }, tr, until), 'Every day at 08:00 (America/Los_Angeles) · until 2026-12-31')
})

test('a reminder that does not repeat has no rule text', () => {
  assert.equal(repeatText({}, tr, until), '')
  assert.equal(repeatText({ repeat: null }, tr, until), '')
})
