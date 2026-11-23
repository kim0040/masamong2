// The one place a typed key is sent to /api/secret and the answer read.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { loadLib } from './lib-load.mjs'

const root = fileURLToPath(new URL('../src', import.meta.url))

// every .svelte and .ts file of the console, for the "no screen builds this by hand any more" checks
function sources() {
  const out = []
  const walk = (dir) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name)
      if (e.isDirectory()) walk(p)
      else if (/\.(svelte|ts)$/.test(e.name)) out.push([relative(root, p).split(sep).join('/'), readFileSync(p, 'utf8')])
    }
  }
  walk(root)
  return out
}

const sent = []
globalThis.__postJSON = async (path, body) => {
  sent.push({ path, body })
  return globalThis.__answer
}
const { saveSecret, validRef } = await loadLib('secret.ts', [["import { postJSON } from './api.svelte'", 'const postJSON = (...a) => globalThis.__postJSON(...a)']])

test('the value is trimmed and sent with its target', async () => {
  sent.length = 0
  globalThis.__answer = { status: 200, body: { ok: true, output: { stored_in: 'keychain' } } }
  await saveSecret({ target: 'provider', id: 'openrouter' }, '  sk-synthetic  ')
  assert.deepEqual(sent, [{ path: '/api/secret', body: { target: 'provider', id: 'openrouter', value: 'sk-synthetic' } }])
  await saveSecret({ target: 'discord' }, 'tok')
  assert.deepEqual(sent[1].body, { target: 'discord', value: 'tok' })
})

test('a provider or Discord answer is an Action result: ok and where it was stored', async () => {
  globalThis.__answer = { status: 200, body: { ok: true, output: { stored_in: 'file' } } }
  assert.deepEqual(await saveSecret({ target: 'discord' }, 'x'), { ok: true, message: '', storedIn: 'file', credentialRef: '' })
})

test('a tool key answers with the reference to save with the tool', async () => {
  globalThis.__answer = { status: 200, body: { ok: true, credential_ref: 'file:/data/secrets/t.key', stored_in: 'file' } }
  const r = await saveSecret({ target: 'file', id: 't' }, 'x')
  assert.equal(r.ok, true)
  assert.equal(r.credentialRef, 'file:/data/secrets/t.key')
  assert.equal(r.storedIn, 'file')
})

test('a refused key is not ok and carries the reason, whichever way the failure arrives', async () => {
  globalThis.__answer = { status: 400, body: { ok: false, error: { code: 'INVALID', message: 'too short' } } }
  assert.deepEqual(await saveSecret({ target: 'discord' }, 'x'), { ok: false, message: 'too short', storedIn: '', credentialRef: '' })
  // HTTP 200 but the Action itself failed
  globalThis.__answer = { status: 200, body: { ok: false, error: { message: 'no such provider' } } }
  assert.equal((await saveSecret({ target: 'provider', id: 'x' }, 'x')).ok, false)
  // a network failure: status 0
  globalThis.__answer = { status: 0, body: { ok: false, error: { code: 'OFFLINE', message: '' } } }
  const off = await saveSecret({ target: 'discord' }, 'x')
  assert.equal(off.ok, false)
  assert.equal(off.message, '')
})

test('a non-200 answer is a failure even when the body says nothing about ok', async () => {
  globalThis.__answer = { status: 500, body: {} }
  assert.equal((await saveSecret({ target: 'discord' }, 'x')).ok, false)
})

test('a reference must be env:, file: or (with a keychain) keychain:, and name something', () => {
  assert.equal(validRef('env:KEY', false), true)
  assert.equal(validRef('file:/x', false), true)
  assert.equal(validRef('keychain:k', false), false)
  assert.equal(validRef('keychain:k', true), true)
  assert.equal(validRef('env:', true), false)
  assert.equal(validRef('plain', true), false)
})

test('only the secret lib talks to /api/secret', () => {
  for (const [file, text] of sources()) if (file !== 'lib/secret.ts') assert.doesNotMatch(text, /\/api\/secret/, `${file} sends a key itself`)
})
