// The Lab's answer-selection button holds text only: no control nested in it. DiscordText draws a spoiler as a
// button unless it is told the text sits inside a button (`still`).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const src = (f) => readFileSync(new URL(`../src/${f}`, import.meta.url), 'utf8')

test('DiscordText inside the Lab answer buttons is still, so no button sits inside a button', () => {
  const lab = src('pages/Lab.svelte')
  const buttons = [...lab.matchAll(/<button\b[^>]*class="a"[^>]*>([\s\S]*?)<\/button>/g)]
  assert.ok(buttons.length > 0)
  for (const b of buttons) {
    const uses = [...b[1].matchAll(/<DiscordText\b[^>]*>/g)].map((m) => m[0])
    assert.ok(uses.length > 0)
    for (const u of uses) assert.match(u, /\bstill\b/, u)
    assert.doesNotMatch(b[1], /<(button|Button|IconButton|a)\b/, 'an interactive element inside the answer button')
  }
})

test('a still DiscordText draws a spoiler without a button of its own', () => {
  const dt = src('components/DiscordText.svelte')
  const still = dt.indexOf("n.k === 'spoiler' && still")
  const button = dt.indexOf("n.k === 'spoiler'}<button")
  assert.ok(still > 0 && button > still, 'the still spoiler branch comes before the button branch')
  assert.doesNotMatch(dt.slice(still, button), /<button/)
})
