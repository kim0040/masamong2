// Accessibility smoke checks over the page sources: mouse-only affordances must have a real,
// labeled control for keyboard and screen readers.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'

const pages = new URL('../src/pages/', import.meta.url)

test('a clickable table row carries a nested, labeled button doing the same thing', () => {
  for (const file of readdirSync(pages).filter((f) => f.endsWith('.svelte'))) {
    const src = readFileSync(new URL(file, pages), 'utf8')
    // Each <tr ... onclick ...> up to its </tr>: pointer users get the row; everyone else needs the button.
    for (const m of src.matchAll(/<tr\b[^>]*onclick[^>]*>([\s\S]*?)<\/tr>/g)) {
      const body = m[1]
      const btn = body.match(/<button\b[^>]*>([\s\S]*?)<\/button>/)
      assert.ok(btn, `${file}: clickable <tr> has no nested button for keyboard users`)
      // The button needs an accessible name: an aria-label or inner content (text or Svelte
      // expressions like {u.name}) a screen reader can speak.
      const named = /aria-label=/.test(btn[0]) || /[{][\s\S]*[}]|[^<>{}\s]/.test(btn[1])
      assert.ok(named, `${file}: the row's button has no accessible name`)
    }
  }
})

test('elements with onclick that are not buttons use a role or live inside a component', () => {
  // A div/span/tr with onclick is an a11y smell unless a nested control covers it (checked above for tr).
  // Guard the pattern from spreading silently: list the elements each page puts onclick on.
  const offenders = []
  for (const file of readdirSync(pages).filter((f) => f.endsWith('.svelte'))) {
    const src = readFileSync(new URL(file, pages), 'utf8')
    for (const m of src.matchAll(/<(div|span|td|li)\b[^>]*\bonclick\b/g)) offenders.push(`${file}: <${m[1]} onclick>`)
  }
  assert.deepEqual(offenders, [], 'onclick belongs on buttons/links, not plain elements')
})

test('icon-only controls are not left unlabeled', () => {
  for (const file of readdirSync(pages).filter((f) => f.endsWith('.svelte'))) {
    const src = readFileSync(new URL(file, pages), 'utf8')
    // <Button icon={...} ...></Button> with no children text needs an aria-label or title.
    for (const m of src.matchAll(/<Button\b[^>]*icon=\{[^}]*\}[^>]*>([\s\S]*?)<\/Button>/g)) {
      if (m[1].trim()) continue // has visible text
      assert.ok(/aria-label=|title=/.test(m[0]), `${file}: icon-only <Button> without a label: ${m[0].slice(0, 120)}`)
    }
  }
})
