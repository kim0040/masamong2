// Runs one of the build's Python scripts with whichever Python this machine has. `python3` is not
// everywhere: on Windows it is often the Microsoft Store stub (it prints nothing useful and exits
// with an error), and the launcher is `py -3`. Each candidate must answer `--version` with a real
// Python 3 before it is used; otherwise the build stops with a message that says what to install.
//   node scripts/run-python.mjs scripts/strings.py [args…]
import { spawnSync } from 'node:child_process'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const candidates = [['python3'], ['python'], ['py', '-3']]

export function findPython(run = spawnSync) {
  for (const [cmd, ...pre] of candidates) {
    const r = run(cmd, [...pre, '--version'], { encoding: 'utf8' })
    if (r.error || r.status !== 0) continue
    if (/^Python 3\.\d+/.test(`${r.stdout}${r.stderr}`.trim())) return { cmd, pre }
  }
  return null
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const found = findPython()
  if (!found) {
    console.error('Python 3 is required to build the console strings (scripts/strings.py) but no working python3, python or "py -3" was found.\nInstall Python 3 from https://www.python.org/downloads/ (on Windows, turn off the Microsoft Store "python" alias under Settings > Apps > Advanced app settings > App execution aliases if it hides the real one).')
    process.exit(1)
  }
  const r = spawnSync(found.cmd, [...found.pre, ...process.argv.slice(2)], { stdio: 'inherit' })
  process.exit(r.status ?? 1)
}
