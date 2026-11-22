// Loads one console module (src/lib/*.ts) for a node --test file: the TypeScript is transpiled and the
// imports that need the browser or Svelte are swapped for small stand-ins, as the other tests here do.
//   const m = await loadLib('secret.ts', [["import { postJSON } from './api.svelte'", 'const postJSON = globalThis.__postJSON']])
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

export function loadLib(file, replacements = []) {
  let source = readFileSync(new URL(`../src/lib/${file}`, import.meta.url), 'utf8')
  for (const [from, to] of replacements) {
    assert.ok(source.includes(from), `${file} no longer contains: ${from}`)
    source = source.replace(from, to)
  }
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
  return import(`data:text/javascript,${encodeURIComponent(js)}#${Math.random()}`)
}
