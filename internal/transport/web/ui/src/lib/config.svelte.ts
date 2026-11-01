// The config document as JSON (config.document) with path edits (config.patch). Every save carries
// the revision it was based on; a conflict reloads and tells the admin instead of overwriting.
import { call, canLoadProtectedActions, session, type ActionResult } from './api.svelte'
import { onLive } from './live.svelte'
import { toast } from './toast.svelte'
import { t } from './i18n.svelte'

export type Schema = {
  t: string
  fields?: { k: string; s: Schema; omitempty?: boolean }[]
  elem?: Schema
  nullable?: boolean
  enum?: string[]
  implied?: unknown // what an empty value means (the default lives in code)
  format?: 'time' | 'weekday' | 'hour' | 'timezone' | 'locale' | 'usd' | 'multiline'
  min?: number
  max?: number
  readonly?: boolean
  legacy?: boolean // kept for old config files; hidden while unset
}

/** A filled-in example of a value of this shape, for the JSON fields' placeholder and "format" note:
 * every field of an object, one entry of a map or list, and each scalar's default or first choice. */
export function schemaExample(s: Schema, depth = 0): unknown {
  if (depth > 6) return null
  switch (s.t) {
    case 'object':
      return Object.fromEntries((s.fields ?? []).filter((f) => !f.s.readonly && !f.s.legacy).map((f) => [f.k, schemaExample(f.s, depth + 1)]))
    case 'map':
      return s.elem ? { example: schemaExample(s.elem, depth + 1) } : {}
    case 'array':
      return s.elem ? [schemaExample(s.elem, depth + 1)] : []
    case 'bool':
      return s.implied ?? false
    case 'int':
    case 'float':
      return s.implied ?? s.min ?? 0
    case 'string':
      return s.enum?.[0] ?? s.implied ?? ''
  }
  return null
}

export const cfg = $state<{ doc: any; defaults: any; schema: Schema | null; revision: number; loaded: boolean; error?: string }>({ doc: null, defaults: null, schema: null, revision: 0, loaded: false })

let loading: Promise<void> | null = null
export function loadConfig(force = false): Promise<void> {
  if (!canLoadProtectedActions(session.value)) return Promise.resolve()
  if (loading && !force) return loading
  loading = (async () => {
    const r = await call('config.document')
    if (r.ok && r.output) {
      cfg.doc = r.output.document
      cfg.defaults = r.output.defaults
      cfg.schema = r.output.schema
      cfg.revision = r.output.revision
      cfg.loaded = true
      cfg.error = undefined
    } else cfg.error = r.error?.message ?? ''
    loading = null
  })()
  return loading
}

/** The document as a LoadState source: what Settings shows while it is missing. */
export const configSource = {
  get data() {
    return cfg.loaded ? cfg.doc : undefined
  },
  get error() {
    return cfg.error === undefined ? undefined : { message: cfg.error }
  },
  refresh: () => loadConfig(true),
}

let subscribed = false
export function useConfig() {
  if (!subscribed) {
    subscribed = true
    onLive(['config'], () => void loadConfig(true))
  }
  if (!cfg.loaded) void loadConfig()
  return cfg
}

export function getIn(obj: any, path: string[]): any {
  let cur = obj
  for (const p of path) {
    if (cur === null || cur === undefined) return undefined
    cur = cur[p]
  }
  return cur
}

export type Op = { path: string[]; value?: unknown; op?: 'set' | 'unset' }

/** Applies edits; toasts the outcome unless quiet. base is the revision the edits were made on (a form
 * that has held a draft for a while passes its own, so a change made elsewhere meanwhile is a conflict
 * instead of being overwritten); without it the current revision is used. */
export async function patchConfig(ops: Op[], opts: { quiet?: boolean; success?: string; base?: number } = {}): Promise<ActionResult> {
  const r = await call('config.patch', { ops }, { base_revision: opts.base ?? cfg.revision })
  if (r.ok) {
    await loadConfig(true)
    if (!opts.quiet) toast('ok', opts.success ?? t('config.saved'), t('config.saved_rev', { rev: r.output?.revision ?? cfg.revision }))
  } else if (r.error?.code === 'CONFIG_CONFLICT') {
    await loadConfig(true)
    toast('warn', t('config.conflict_title'), t('config.conflict_body'))
  } else if (!opts.quiet) {
    toast('error', t('config.save_failed'), r.error?.message)
  }
  return r
}
