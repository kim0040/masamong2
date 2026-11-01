import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// Exercise the component's async handler with stubbed Svelte runes and dependencies.
const component = readFileSync(new URL('../src/components/LanguageSelect.svelte', import.meta.url), 'utf8')
const script = component.match(/<script lang="ts">([\s\S]*?)<\/script>/)?.[1]
assert.ok(script)
const js = ts.transpileModule(script.replace(/^\s*import .*$/gm, ''), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText

 test('failed switch restores the selection, clears busy, and shows a localized error toast', async () => {
  const logs = []
  let fail
  const lang = { lang: 'en' }
  globalThis.__languageSelectTest = {
    t: (key) => key === 'common.failed' ? 'Failed' : key,
    i18n: lang,
    languageLabel: (tag) => tag,
    session: { value: { locales: ['en', 'fr'] } },
    setPrefs: () => {},
    toast: (...args) => logs.push(args),
    switchLanguage: () => new Promise((_, reject) => { fail = reject }),
  }
  const setup = `const { t, i18n, languageLabel, session, setPrefs, toast, switchLanguage } = globalThis.__languageSelectTest;
    const $props = () => ({}); const $derived = (value) => value; const $state = (value) => value;`
  try {
    const { change, isBusy } = await import(`data:text/javascript,${encodeURIComponent(`${setup}\n${js}\nexport { change }; export const isBusy = () => busy;`)}`)
    const select = { value: 'fr' }
    const changing = change({ currentTarget: select })
    assert.equal(isBusy(), true)
    fail(new Error('unavailable'))
    await changing
    assert.equal(isBusy(), false)
    assert.equal(select.value, 'en')
    assert.deepEqual(logs, [['error', 'Failed']])
  } finally {
    delete globalThis.__languageSelectTest
  }
})
