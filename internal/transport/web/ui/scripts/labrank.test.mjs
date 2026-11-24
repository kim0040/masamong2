// Every ranking the Lab can show has a name in both languages, including the "<mode>_fallback" the search
// reports when the reranker was unavailable (internal/memory/search/retrieve.go sets applied = mode + "_fallback").
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

for (const lang of ['ko', 'en']) {
  test(`${lang}: every ranking and its fallback has a label`, () => {
    const strings = JSON.parse(readFileSync(new URL(`../../../../../locales/ui/${lang}.json`, import.meta.url), 'utf8'))
    for (const mode of ['auto', 'hybrid', 'rerank', 'heuristic']) {
      assert.ok(strings[`lab.rank.${mode}`], `lab.rank.${mode}`)
      assert.ok(strings[`lab.rank.${mode}_fallback`], `lab.rank.${mode}_fallback`)
    }
  })
}
