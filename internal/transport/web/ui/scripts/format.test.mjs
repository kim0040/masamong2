import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

const source = readFileSync(new URL('../src/lib/format.ts', import.meta.url), 'utf8')
  .replace("import { i18n } from './i18n.svelte'", 'const i18n = globalThis.__formatLang')
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText

test('compact duration preserves ko/en and uses available Intl units for additional locales', async (t) => {
  const previous = globalThis.__formatLang
  const lang = { lang: 'ko' }
  globalThis.__formatLang = lang
  try {
    const { duration } = await import(`data:text/javascript,${encodeURIComponent(js)}`)
    assert.equal(duration(90000), '1일 1시간')
    assert.equal(duration(3660), '1시간 1분')
    assert.equal(duration(0), '0분')
    lang.lang = 'en'
    assert.equal(duration(90000), '1d 1h')
    assert.equal(duration(3660), '1h 1m')
    assert.equal(duration(0), '0m')

    lang.lang = 'zz-ZZ'
    assert.equal(duration(3660), '1h 1m')
    if (!Intl.NumberFormat.supportedLocalesOf(['fr']).length) return t.skip('French Intl data unavailable')
    lang.lang = 'fr'
    const unit = (n, name) => new Intl.NumberFormat('fr', { style: 'unit', unit: name, unitDisplay: 'narrow' }).format(n)
    assert.equal(duration(90000), `${unit(1, 'day')} ${unit(1, 'hour')}`)
    assert.equal(duration(3660), `${unit(1, 'hour')} ${unit(1, 'minute')}`)
    assert.equal(duration(0), unit(0, 'minute'))
    const numberFormat = Intl.NumberFormat
    try {
      Intl.NumberFormat = class extends numberFormat {
        constructor(locale, options) {
          if (options?.style === 'unit') throw new RangeError('units unavailable')
          super(locale, options)
        }
      }
      assert.equal(duration(3660), '1h 1m')
    } finally {
      Intl.NumberFormat = numberFormat
    }
  } finally {
    globalThis.__formatLang = previous
  }
})
