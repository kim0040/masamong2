import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// Run the loader's logic without a browser: replace only its API import and Svelte's state rune.
const source = readFileSync(new URL('../src/lib/i18n.svelte.ts', import.meta.url), 'utf8')
  .replace("import { setApiLang, setErrorHints } from './api.svelte'", 'const { setApiLang, setErrorHints } = globalThis.__i18nTest')
  .replaceAll('import.meta.env.DEV', 'false')
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText

test('loader commits only validated responses and plural selection falls back safely', async () => {
  const originals = { fetch: globalThis.fetch, document: globalThis.document, state: globalThis.$state, test: globalThis.__i18nTest }
  let apiLang = ''
  let hints
  let saves = []
  globalThis.$state = (value) => value
  globalThis.__i18nTest = { setApiLang: (lang) => { apiLang = lang }, setErrorHints: (fn) => { hints = fn } }
  globalThis.document = { documentElement: { lang: '', dir: '' } }
  const reply = (body, ok = true) => { globalThis.fetch = async () => ({ ok, status: ok ? 200 : 503, json: async () => body }) }
  try {
    const { i18n, loadStrings, switchLanguage, pluralCategory, t } = await import(`data:text/javascript,${encodeURIComponent(js)}`)
    reply({ lang: 'fr', strings: {} })
    await assert.rejects(loadStrings('fr'), /Invalid console strings response/)
    assert.equal(i18n.ready, false)
    assert.equal(apiLang, '')
    reply({ lang: 'en', strings: { 'error.OFFLINE': 'Offline', 'tool.cond.unknown': '', count: '{n} changes', 'count.one': '{n} change', 'count.other': '{n} changes' } })
    await switchLanguage('en', (lang) => saves.push(lang))
    assert.equal(i18n.ready, true)
    assert.deepEqual(saves, ['en'])
    assert.equal(t('count', { n: 1 }), '1 change')
    assert.equal(t('count', { n: 2 }), '2 changes')
    assert.equal(t('count', { n: -1 }), '-1 changes')
    assert.equal(hints('OFFLINE'), 'Offline')
    assert.equal(globalThis.document.documentElement.lang, 'en')
    reply(null, false)
    await assert.rejects(switchLanguage('fr', (lang) => saves.push(lang)), /HTTP 503/)
    globalThis.fetch = async () => { throw new TypeError('network') }
    await assert.rejects(loadStrings('fr'), /network/)
    globalThis.fetch = async () => ({ ok: true, json: async () => { throw new SyntaxError('bad JSON') } })
    await assert.rejects(loadStrings('fr'), /bad JSON/)
    for (const payload of [null, [], { lang: 'fr' }, { lang: '', strings: { x: 'yes' } }, { lang: 'fr', strings: { x: 1 } }, { lang: 'fr', strings: { x: '' } }]) {
      reply(payload)
      await assert.rejects(loadStrings('fr'), /Invalid console strings response/)
    }
    assert.equal(i18n.lang, 'en')
    assert.equal(i18n.ready, true)
    assert.equal(t('count', { n: 1 }), '1 change')
    assert.equal(t('tool.cond.unknown'), '')
    assert.equal(apiLang, 'en')
    assert.equal(globalThis.document.documentElement.lang, 'en')
    assert.deepEqual(saves, ['en'])

    reply({ lang: 'ar', strings: {
      count: '{n} items', 'count.one': '{n} one', 'count.other': '{n} other',
      'count.zero': '{n} zero', 'count.two': '{n} two', 'count.few': '{n} few', 'count.many': '{n} many',
    } })
    await loadStrings('ar')
    for (const [n, category] of [[0, 'zero'], [1, 'one'], [2, 'two'], [3, 'few'], [11, 'many'], [100, 'other']]) {
      assert.equal(pluralCategory('ar', n), category)
      assert.equal(t('count', { n }), `${n} ${category}`)
    }
    assert.equal(globalThis.document.documentElement.dir, 'rtl')
    reply({ lang: 'ar', strings: { count: '{n} items', 'count.one': '{n} one', 'count.other': '{n} other', 'count.few': '{n} few' } })
    await loadStrings('ar')
    assert.equal(t('count', { n: 3 }), '3 few')
    assert.equal(t('count', { n: 11 }), '11 other')
    assert.equal(pluralCategory('not_a_locale', 1), 'one')
    assert.equal(pluralCategory('not_a_locale', 2), 'other')
  } finally {
    globalThis.fetch = originals.fetch
    globalThis.document = originals.document
    globalThis.$state = originals.state
    globalThis.__i18nTest = originals.test
  }
})
