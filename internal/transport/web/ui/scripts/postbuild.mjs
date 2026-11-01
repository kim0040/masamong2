// After `vite build`: precompress text assets (the Go server serves .br/.gz when accepted) and enforce
// the performance budget from docs/admin-gui-ia.md — the build fails when the first load grows past it.
import { readFileSync, writeFileSync, readdirSync, statSync, rmSync, existsSync } from 'node:fs'
import { join, extname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { brotliCompressSync, gzipSync, constants } from 'node:zlib'

// fileURLToPath, not URL.pathname: on Windows the pathname is /C:/… and is not a filesystem path
const dist = fileURLToPath(new URL('../dist/', import.meta.url))
const BUDGET_INITIAL_JS = 150 * 1024 // gzip, first load
const BUDGET_CHUNK_JS = 60 * 1024 // gzip, any lazy page chunk

function walk(dir) {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n)
    return statSync(p).isDirectory() ? walk(p) : [p]
  })
}

const gz = (p) => gzipSync(readFileSync(p), { level: 9 }).length
for (const p of walk(dist)) {
  if (!['.js', '.css', '.svg', '.json', '.html', '.webmanifest', '.txt'].includes(extname(p))) continue
  const raw = readFileSync(p)
  if (raw.length < 1024) continue
  writeFileSync(p + '.br', brotliCompressSync(raw, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }))
  writeFileSync(p + '.gz', gzipSync(raw, { level: 9 }))
}

const manifestPath = join(dist, '.vite', 'manifest.json')
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
const entry = Object.values(manifest).find((c) => c.isEntry)
const seen = new Set()
let initial = 0
;(function visit(key) {
  const c = manifest[key] ?? Object.values(manifest).find((x) => x.file === key)
  if (!c || seen.has(c.file)) return
  seen.add(c.file)
  initial += gz(join(dist, c.file))
  for (const css of c.css ?? []) if (!seen.has(css)) { seen.add(css); initial += gz(join(dist, css)) }
  for (const i of c.imports ?? []) visit(i)
})(Object.keys(manifest).find((k) => manifest[k] === entry))

const problems = []
if (initial > BUDGET_INITIAL_JS) problems.push(`initial load ${(initial / 1024).toFixed(1)} KiB gz > ${BUDGET_INITIAL_JS / 1024} KiB`)
for (const c of Object.values(manifest)) {
  if (!c.file.endsWith('.js') || seen.has(c.file)) continue
  const size = gz(join(dist, c.file))
  if (size > BUDGET_CHUNK_JS) problems.push(`${c.file} ${(size / 1024).toFixed(1)} KiB gz > ${BUDGET_CHUNK_JS / 1024} KiB`)
}
console.log(`console bundle: first load ${(initial / 1024).toFixed(1)} KiB gz (budget ${BUDGET_INITIAL_JS / 1024} KiB)`)
rmSync(join(dist, '.vite'), { recursive: true, force: true })
if (problems.length) {
  console.error('performance budget exceeded:\n  ' + problems.join('\n  '))
  process.exit(1)
}
