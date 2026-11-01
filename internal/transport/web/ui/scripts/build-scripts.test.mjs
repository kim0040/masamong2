// The build scripts must work on Windows: postbuild resolves dist with a real filesystem path, and the
// Python finder skips a stub that cannot run.
import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { findPython } from './run-python.mjs'

const scripts = fileURLToPath(new URL('.', import.meta.url))

test('postbuild finds dist on every platform (no C:/C:/ path) and precompresses assets', () => {
  const root = mkdtempSync(join(tmpdir(), 'masamong-postbuild-'))
  try {
    mkdirSync(join(root, 'scripts'))
    mkdirSync(join(root, 'dist', 'assets'), { recursive: true })
    mkdirSync(join(root, 'dist', '.vite'))
    copyFileSync(join(scripts, 'postbuild.mjs'), join(root, 'scripts', 'postbuild.mjs'))
    writeFileSync(join(root, 'dist', 'assets', 'index-abc.js'), 'console.log("x")\n'.repeat(200))
    writeFileSync(join(root, 'dist', '.vite', 'manifest.json'), JSON.stringify({ 'index.html': { file: 'assets/index-abc.js', isEntry: true } }))
    const r = spawnSync(process.execPath, [join(root, 'scripts', 'postbuild.mjs')], { encoding: 'utf8' })
    assert.equal(r.status, 0, r.stderr)
    assert.ok(existsSync(join(root, 'dist', 'assets', 'index-abc.js.br')))
    assert.ok(existsSync(join(root, 'dist', 'assets', 'index-abc.js.gz')))
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})

test('findPython skips the Microsoft Store stub and takes the first real Python 3', () => {
  const answers = {
    python3: { status: 9009, stdout: '', stderr: 'Python was not found; run without arguments to install from the Microsoft Store' },
    python: { status: 0, stdout: 'Python 2.7.18\n', stderr: '' },
    py: { status: 0, stdout: 'Python 3.12.1\n', stderr: '' },
  }
  const seen = []
  const found = findPython((cmd, args) => {
    seen.push([cmd, ...args].join(' '))
    return answers[cmd]
  })
  assert.deepEqual(found, { cmd: 'py', pre: ['-3'] })
  assert.deepEqual(seen, ['python3 --version', 'python --version', 'py -3 --version'])
})

test('findPython reports none when no candidate runs', () => {
  assert.equal(findPython(() => ({ error: new Error('ENOENT'), status: null })), null)
})
