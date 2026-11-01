import type { Component } from 'svelte'

// Page routes. Top-level paths must also be listed in routes.json (the Go server checks catalog
// GUI routes against it and serves index.html for them). Pages load on demand.
type Loader = () => Promise<{ default: Component<any> }>
export const routes: { pattern: string; load: Loader; bare?: boolean; redirect?: string }[] = [
  { pattern: '/', load: () => import('./pages/Home.svelte') },
  { pattern: '/turns', load: () => import('./pages/Turns.svelte') },
  { pattern: '/jobs', load: () => import('./pages/Jobs.svelte') },
  { pattern: '/rooms', load: () => import('./pages/Rooms.svelte') },
  { pattern: '/rooms/:guild/:channel', load: () => import('./pages/Room.svelte') },
  { pattern: '/tools', load: () => import('./pages/Tools.svelte') },
  { pattern: '/tools/new', load: () => import('./pages/ToolNew.svelte') },
  { pattern: '/tools/:id', load: () => import('./pages/Tool.svelte') },
  { pattern: '/persona', load: () => import('./pages/Persona.svelte') },
  { pattern: '/people', load: () => import('./pages/People.svelte') },
  { pattern: '/blocks', load: () => import('./pages/Blocks.svelte') },
  { pattern: '/blocks/:room', load: () => import('./pages/Block.svelte') },
  // pages merged into memory blocks (old links and bookmarks keep working)
  { pattern: '/sources', load: () => import('./pages/Blocks.svelte'), redirect: '/blocks' },
  { pattern: '/index', load: () => import('./pages/Blocks.svelte'), redirect: '/blocks?view=settings' },
  { pattern: '/lab', load: () => import('./pages/Lab.svelte') },
  { pattern: '/memory', load: () => import('./pages/Memory.svelte') },
  { pattern: '/stats', load: () => import('./pages/Stats.svelte') },
  { pattern: '/usage', load: () => import('./pages/Usage.svelte') },
  { pattern: '/connections', load: () => import('./pages/Connections.svelte') },
  { pattern: '/settings', load: () => import('./pages/Settings.svelte') },
  { pattern: '/logs', load: () => import('./pages/Logs.svelte') },
  { pattern: '/maintenance', load: () => import('./pages/Maintenance.svelte') },
  { pattern: '/display', load: () => import('./pages/Display.svelte') },
  { pattern: '/admins', load: () => import('./pages/Display.svelte'), redirect: '/display' },
  { pattern: '/setup', load: () => import('./pages/Setup.svelte'), bare: true },
  { pattern: '/login', load: () => import('./pages/Login.svelte'), bare: true },
]
export const notFound: Loader = () => import('./pages/NotFound.svelte')
