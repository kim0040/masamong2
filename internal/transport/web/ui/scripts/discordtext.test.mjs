// Discord markup shown as it should be, never as raw markers or tokens.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { loadLib } from './lib-load.mjs'

const d = await loadLib('discordtext.ts')
const kinds = (s) => d.parseDiscord(s).map((n) => n.k)

test('underline, bold, italic, strike and spoiler become nodes, the markers are gone', () => {
  const n = d.parseDiscord('__hiden__ and **bold** and *it* and ~~old~~ and ||secret||')
  assert.deepEqual(n.map((x) => x.k), ['underline', 'text', 'bold', 'text', 'italic', 'text', 'strike', 'text', 'spoiler'])
  assert.deepEqual(n[0].c, [{ k: 'text', v: 'hiden' }])
})

test('markers nest, and an unclosed marker stays as typed', () => {
  assert.deepEqual(d.parseDiscord('**__both__**')[0].c[0].k, 'underline')
  assert.deepEqual(d.parseDiscord('a __b'), [{ k: 'text', v: 'a __b' }])
  assert.deepEqual(d.parseDiscord('2 * 3 * 4'), [{ k: 'text', v: '2 * 3 * 4' }])
  assert.deepEqual(d.parseDiscord('snake_case_name'), [{ k: 'text', v: 'snake_case_name' }])
})

test('mentions, channels, roles and emoji are named, not shown as numbers', () => {
  assert.deepEqual(d.parseDiscord('<@123456789> <@!123456789> <#987654321> <@&555555555> <:smile:123456789> <a:wave:123456789>').filter((n) => n.k !== 'text'), [
    { k: 'user', id: '123456789' }, { k: 'user', id: '123456789' }, { k: 'channel', id: '987654321' }, { k: 'role', id: '555555555' }, { k: 'emoji', name: 'smile' }, { k: 'emoji', name: 'wave' },
  ])
})

test('code is kept as it is, inline and fenced; an escape shows the marker', () => {
  assert.deepEqual(d.parseDiscord('`a *b* c`'), [{ k: 'code', v: 'a *b* c' }])
  assert.deepEqual(d.parseDiscord('x\n```js\nlet a = 1\n```\ny').map((n) => n.k), ['text', 'code', 'text'])
  assert.equal(d.parseDiscord('```js\nlet a = 1\n```')[0].block, true)
  assert.deepEqual(d.parseDiscord('\\*not italic\\*'), [{ k: 'text', v: '*not italic*' }])
})

test('quotes are lines that start with > ', () => {
  assert.deepEqual(kinds('> quoted\n> more\nafter'), ['quote', 'text'])
})

test('an excerpt tag line is split off', () => {
  const s = d.splitExcerpt('[kakao:practice · 2026-05-02 · A, B]\nA: hi\nB: yo')
  assert.deepEqual(s, { head: { room: 'kakao:practice', date: '2026-05-02', speakers: 'A, B' }, body: 'A: hi\nB: yo' })
  assert.deepEqual(d.splitExcerpt('no tag'), { head: null, body: 'no tag' })
  assert.deepEqual(d.splitExcerpt(undefined), { head: null, body: '' })
  const g = d.splitExcerpt('[discord guild=1 channel=2 · 2026-05-02 · A]\nA: hi')
  assert.deepEqual(g, { head: { room: 'discord guild=1 channel=2', date: '2026-05-02', speakers: 'A' }, body: 'A: hi' })
  assert.equal(d.splitExcerpt('[discord:1:2]\nA: hi').body, 'A: hi')
})

test('a message that starts with a bracketed line keeps it', () => {
  for (const m of ['[TODO]\nship checklist', '[공지]\n내일 휴무', '[a · b]\nx']) {
    assert.deepEqual(d.splitExcerpt(m, { raw: true }), { head: null, body: m })
  }
  assert.deepEqual(d.splitExcerpt('[TODO]\nship checklist'), { head: null, body: '[TODO]\nship checklist' })
  assert.equal(d.splitExcerpt('[kakao:practice · 2026-05-02 · A]\nA: hi', { raw: true }).body, '[kakao:practice · 2026-05-02 · A]\nA: hi')
})
