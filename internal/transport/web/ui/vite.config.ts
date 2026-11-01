import { defineConfig, type Plugin } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { createHash } from 'node:crypto'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative } from 'node:path'

// virtual:brand gives every file under public/brand an address with the hash of its content
// (brand('mascot/mong-lab.webp') → /brand/mascot/mong-lab.webp?v=1a2b3c4d): a redrawn picture gets a
// new address by itself, and the server caches versioned brand files for good.
function brandVersions(): Plugin {
  const id = 'virtual:brand'
  const resolved = '\0' + id
  const root = join(import.meta.dirname, 'public', 'brand')
  const walk = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)]))
  return {
    name: 'brand-versions',
    resolveId: (s) => (s === id ? resolved : undefined),
    load(s) {
      if (s !== resolved) return
      const v: Record<string, string> = {}
      // the pictures the app shows (icons and logos have their own paths in index.html)
      for (const f of walk(root).filter((f) => /\.webp$/.test(f)).sort()) {
        this.addWatchFile(f)
        v[relative(root, f).split('\\').join('/')] = createHash('sha256').update(readFileSync(f)).digest('hex').slice(0, 8)
      }
      return `const v = ${JSON.stringify(v)}\nexport const brand = (p) => '/brand/' + p + (v[p] ? '?v=' + v[p] : '')\n`
    },
  }
}

// The Go daemon serves dist/ (embedded). `npm run dev` proxies the API to a running daemon.
export default defineConfig({
  plugins: [svelte(), brandVersions()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    manifest: true,
    cssCodeSplit: true,
    assetsInlineLimit: 0,
    reportCompressedSize: false,
    modulePreload: { polyfill: false },
  },
  server: {
    proxy: { '/api': { target: 'http://127.0.0.1:8787', ws: false }, '/login': 'http://127.0.0.1:8787', '/lite': 'http://127.0.0.1:8787' },
  },
})
