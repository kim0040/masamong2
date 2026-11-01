import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// The API client's logic without a browser: only Svelte's state rune and fetch are replaced.
const source = readFileSync(new URL('../src/lib/api.svelte.ts', import.meta.url), 'utf8')
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText

async function load() {
  globalThis.$state = (value) => value
  globalThis.document = { documentElement: { setAttribute() {} } }
  globalThis.sessionStorage = { getItem: () => null }
  return import(`data:text/javascript,${encodeURIComponent(js)}#${Math.random()}`)
}

test('postJSON never throws: a network failure and a non-JSON reply both come back as an error result', async () => {
  const originalFetch = globalThis.fetch
  try {
    const api = await load()
    api.setErrorHints((code) => (code === 'OFFLINE' ? 'cannot reach the service' : ''))

    globalThis.fetch = async () => { throw new TypeError('Failed to fetch') }
    const offline = await api.postJSON('/api/secret', { value: 'x' })
    assert.equal(offline.status, 0)
    assert.equal(offline.body.ok, false)
    assert.equal(offline.body.error.code, 'OFFLINE')
    assert.equal(offline.body.error.message, 'cannot reach the service')
    assert.equal(api.session.offline, true)

    globalThis.fetch = async () => ({ status: 502, json: async () => { throw new SyntaxError('Unexpected token <') } })
    const html = await api.postJSON('/api/secret', { value: 'x' })
    assert.equal(html.status, 502)
    assert.deepEqual(html.body, { ok: false, error: { code: 'UNKNOWN', message: 'HTTP 502' } })

    globalThis.fetch = async () => ({ status: 200, json: async () => ({ ok: true, output: { stored_in: 'file' } }) })
    const ok = await api.postJSON('/api/secret', { value: 'x' })
    assert.equal(ok.status, 200)
    assert.equal(ok.body.output.stored_in, 'file')
    assert.equal(api.session.offline, false)
  } finally {
    globalThis.fetch = originalFetch
  }
})
