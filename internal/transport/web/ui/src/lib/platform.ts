// The operating system the service runs on (the browser may be on another machine, over SSH), so words
// about the tray icon, file locations and key storage match what the operator actually sees.
import { session } from './api.svelte'
import { has, t } from './i18n.svelte'

export type Platform = 'windows' | 'darwin' | 'linux'

export function platform(): Platform {
  const p = session.value?.platform
  return p === 'windows' || p === 'darwin' ? p : 'linux'
}

/** t(key) with a per-platform variant when the catalog has one: `key.windows`, `key.darwin`, else `key`. */
export function tp(key: string, args?: Record<string, unknown>): string {
  const specific = `${key}.${platform()}`
  return t(has(specific) ? specific : key, args)
}

/** A file path as this system writes it, for an input's placeholder. */
export function examplePath(file: string): string {
  const p = platform()
  return p === 'windows' ? 'C:\\Users\\me\\' + file : p === 'darwin' ? `/Users/me/${file}` : `/home/me/${file}`
}
