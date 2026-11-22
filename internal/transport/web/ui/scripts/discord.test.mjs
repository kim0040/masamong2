// Asking Discord who the token belongs to, and for an invite link.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative } from 'node:path'
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
      else if (/\.(svelte|ts)$/.test(e.name)) out.push([relative(root, p), readFileSync(p, 'utf8')])
    }
  }
  walk(root)
  return out
}

const sent = []
globalThis.__call = async (action, input) => {
  sent.push({ action, input })
  return globalThis.__answer
}
const { checkBot, makeInvite } = await loadLib('discord.ts', [["import { call } from './api.svelte'", 'const call = (...a) => globalThis.__call(...a)']])

test('checkBot stores the bot id (apply) and returns the bot', async () => {
  sent.length = 0
  globalThis.__answer = { ok: true, output: { bot_name: 'Synthetic', bot_id: '123' } }
  assert.deepEqual(await checkBot(), { bot: { bot_name: 'Synthetic', bot_id: '123' }, reason: '', message: undefined })
  assert.deepEqual(sent, [{ action: 'discord.check', input: { apply: true } }])
})

test('no saved token is not an error to show; a refused token is, with its reason', async () => {
  globalThis.__answer = { ok: false, error: { code: 'X', message: 'no token yet', args: { reason: 'no_token' } } }
  assert.deepEqual(await checkBot(), { bot: null, reason: 'no_token', message: undefined })
  globalThis.__answer = { ok: false, error: { code: 'X', message: 'Discord refused the token', args: { reason: 'token_rejected' } } }
  assert.deepEqual(await checkBot(), { bot: null, reason: 'token_rejected', message: 'Discord refused the token' })
})

test('a failure without a reason or a message still has a (blank) message to show', async () => {
  globalThis.__answer = { ok: false, error: { code: 'OFFLINE', message: '' } }
  assert.deepEqual(await checkBot(), { bot: null, reason: '', message: '' })
  globalThis.__answer = { ok: false }
  assert.deepEqual(await checkBot(), { bot: null, reason: '', message: '' })
})

test('makeInvite asks for a link, naming the server only when one is given', async () => {
  sent.length = 0
  globalThis.__answer = { ok: true, output: { url: 'https://discord.example/invite' } }
  assert.deepEqual(await makeInvite(), { url: 'https://discord.example/invite', message: '' })
  assert.deepEqual(await makeInvite('777'), { url: 'https://discord.example/invite', message: '' })
  assert.deepEqual(sent.map((s) => s.input), [{}, { guild_id: '777' }])
  globalThis.__answer = { ok: false, error: { message: 'no bot id yet' } }
  assert.deepEqual(await makeInvite(), { url: '', message: 'no bot id yet' })
})

test('only the discord lib asks Discord to check the token or make an invite', () => {
  for (const [file, text] of sources()) if (file !== 'lib/discord.ts') assert.doesNotMatch(text, /'(discord\.check|channels\.invite)'/, `${file} asks Discord itself`)
})
