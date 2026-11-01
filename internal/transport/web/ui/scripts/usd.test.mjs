import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

const source = readFileSync(new URL('../src/lib/format.ts', import.meta.url), 'utf8')
  .replace("import { i18n } from './i18n.svelte'", 'const i18n = globalThis.__formatLang')
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText

test('usd shows three significant digits under ten cents', async () => {
  const previous = globalThis.__formatLang
  globalThis.__formatLang = { lang: 'en' }
  try {
    const { usd } = await import(`data:text/javascript,${encodeURIComponent(js)}#usd`)
    assert.equal(usd(0.014), '$0.014')
    assert.equal(usd(0.00014), '$0.00014')
    assert.equal(usd(0.0123456), '$0.0123')
    assert.equal(usd(0.05), '$0.05')
    assert.equal(usd(0), '$0.00')
    assert.equal(usd(0.14), '$0.14')
    assert.equal(usd(12.5), '$12.50')
    assert.equal(usd(undefined), '–')
  } finally {
    globalThis.__formatLang = previous
  }
})
