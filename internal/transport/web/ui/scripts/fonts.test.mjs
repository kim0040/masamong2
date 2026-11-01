// CJK text must not be drawn in another language's glyph forms: the Korean system fonts may come first
// only in the Korean stack (WIN-01), and each CJK language names fonts of its own.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const css = readFileSync(new URL('../src/styles/global.css', import.meta.url), 'utf8')

// the --font stack declared in the rule that starts with this selector
function stackFor(selector) {
  const start = css.indexOf(`${selector} {`) >= 0 ? css.indexOf(`${selector} {`) : css.indexOf(`${selector},`)
  assert.ok(start >= 0, `no rule for ${selector}`)
  const block = css.slice(start, css.indexOf('}', start))
  const decl = block.indexOf('--font:')
  assert.ok(decl >= 0, `no --font in the rule for ${selector}`)
  return block
    .slice(decl + '--font:'.length, block.indexOf(';', decl))
    .split(',')
    .map((f) => f.trim().replace(/^'|'$/g, ''))
}
const korean = ['Apple SD Gothic Neo', 'Malgun Gothic', 'Noto Sans KR']

test('Korean keeps its system fonts, Japanese and Chinese do not list them', () => {
  const base = stackFor(':root')
  for (const f of korean) assert.ok(base.includes(f), `${f} stays in the Korean stack`)
  for (const sel of [':root:lang(ja)', ':root:lang(zh)', ':root:lang(zh-Hant)']) {
    const stack = stackFor(sel)
    for (const f of korean) assert.ok(!stack.includes(f), `${f} must not be in ${sel}`)
    assert.ok(stack.includes('Pretendard Variable'), `${sel} still starts with the Latin face`)
  }
})

test('each language names its own system fonts for Windows, macOS and Linux', () => {
  const need = {
    ':root:lang(ja)': ['Yu Gothic UI', 'Hiragino Sans', 'Noto Sans JP'],
    ':root:lang(zh)': ['Microsoft YaHei UI', 'PingFang SC', 'Noto Sans SC'],
    ':root:lang(zh-Hant)': ['Microsoft JhengHei UI', 'PingFang TC', 'Noto Sans TC'],
  }
  for (const [sel, fonts] of Object.entries(need)) for (const f of fonts) assert.ok(stackFor(sel).includes(f), `${sel} lacks ${f}`)
})
