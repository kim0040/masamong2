// Per-admin UI preferences. The server (admin.prefs) is the source of truth; localStorage is a cache
// so boot.js can apply theme/density/motion before the first paint.
import { call, session } from './api.svelte'

/** Three clearly different sizes: text, spacing and controls all scale together. */
export type Density = 'compact' | 'normal' | 'large'
export const densities: Density[] = ['compact', 'normal', 'large']
/** Older builds saved 'comfortable' for the middle size. */
export function normalizeDensity(v: unknown): Density {
  return v === 'compact' || v === 'large' ? v : 'normal'
}

export type Prefs = {
  theme: 'system' | 'light' | 'dark'
  lang?: string
  /** Persona page language: empty = automatic (the console language, which follows the system). */
  persona_lang?: string
  density: Density
  motion: 'system' | 'full' | 'reduced'
  sidebar: 'expanded' | 'collapsed'
  tips: Record<string, boolean>
  tutorial: { done?: boolean; step?: number; skipped?: string[] }
  home_hidden: string[]
  home_order: string[]
  attention_read: Record<string, string>
}

const defaults: Prefs = { theme: 'system', density: 'normal', motion: 'system', sidebar: 'expanded', tips: {}, tutorial: {}, home_hidden: [], home_order: [], attention_read: {} }

function cached(): Partial<Prefs> {
  try {
    return JSON.parse(localStorage.getItem('masamong.ui') || '{}')
  } catch {
    return {}
  }
}

export const prefs = $state<Prefs>({ ...defaults, ...cached() })
prefs.density = normalizeDensity(prefs.density)

const media = window.matchMedia('(prefers-reduced-motion: reduce)')
const dark = window.matchMedia('(prefers-color-scheme: dark)')

export function motionAllowed(): boolean {
  if (prefs.motion === 'reduced') return false
  if (prefs.motion === 'full') return true
  return !media.matches
}

export function effectiveTheme(): 'light' | 'dark' {
  if (prefs.theme === 'system') return dark.matches ? 'dark' : 'light'
  return prefs.theme
}

/** Theme swaps snap: transitions are suppressed for one frame so colors do not smear. */
function withoutTransitions(fn: () => void) {
  const style = document.createElement('style')
  style.append(document.createTextNode('*,*::before,*::after{transition:none !important}'))
  document.head.append(style)
  fn()
  void document.body.offsetHeight
  requestAnimationFrame(() => requestAnimationFrame(() => style.remove()))
}

function applyDOM() {
  const d = document.documentElement
  withoutTransitions(() => {
    if (prefs.theme === 'system') d.removeAttribute('data-theme')
    else d.setAttribute('data-theme', prefs.theme)
    d.setAttribute('data-density', prefs.density)
    if (prefs.motion === 'system') d.removeAttribute('data-motion')
    else d.setAttribute('data-motion', prefs.motion)
  })
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', effectiveTheme() === 'dark' ? '#0E0E10' : '#FAFAF9')
  try {
    localStorage.setItem('masamong.ui', JSON.stringify({ theme: prefs.theme, density: prefs.density, motion: prefs.motion, lang: prefs.lang, sidebar: prefs.sidebar }))
  } catch {}
}

dark.addEventListener('change', () => prefs.theme === 'system' && applyDOM())

let timer: ReturnType<typeof setTimeout> | undefined
let pending: Partial<Prefs> = {}

/** Updates preferences now and saves them to the server shortly after (batched). */
export function setPrefs(patch: Partial<Prefs>) {
  Object.assign(prefs, patch)
  applyDOM()
  pending = { ...pending, ...patch }
  clearTimeout(timer)
  // before setup there is no instance to store preferences in; they are sent once it exists
  if (!session.value?.ready) return
  timer = setTimeout(() => {
    const body = pending
    pending = {}
    void call('admin.prefs.set', { prefs: body })
  }, 400)
}

export async function loadPrefs() {
  if (!session.value?.ready) return applyDOM()
  if (Object.keys(pending).length) {
    const body = pending
    pending = {}
    await call('admin.prefs.set', { prefs: body })
  }
  const r = await call<{ prefs: Partial<Prefs> }>('admin.prefs')
  if (r.ok && r.output) Object.assign(prefs, defaults, r.output.prefs)
  prefs.density = normalizeDensity(prefs.density)
  applyDOM()
}

export function applyCachedPrefs() {
  applyDOM()
}
