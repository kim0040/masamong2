// What the console does with the server's tool list (tools.list): a tool's inputs, providers and
// source come from the server (internal/tools Builtins), so nothing about a tool is repeated here.
// Only presentation lives in this file: how to group tools, and how input values turn into the JSON
// the tool takes.

import { t, has } from './i18n.svelte'

export type ToolInput = { name: string; type: 'text' | 'number' | 'select' | 'bool' | 'url' | 'provider'; required?: boolean; options?: string[]; min?: number; max?: number; example?: string; operator?: boolean }

/** Values as the form holds them: strings, and true/false/undefined for switches. */
export type InputValues = Record<string, string | boolean | undefined>

export type ProviderState = 'ready' | 'missing' | 'none'

/** Whether a tool's providers are usable, from tools.list (any one provider with a key is enough). */
export function providerState(tl: any): ProviderState {
  if (!tl?.provider_kinds?.length) return 'none'
  return tl.provider_ready ? 'ready' : 'missing'
}

// Why a tool cannot answer (tools.list ready_reason) → how the console shows it. The server decides
// readiness once (keys, the model role the tool needs, the memory index); pages only present it.
const reasonState: Record<string, { tone: 'neutral' | 'info' | 'warn'; key: string }> = {
  config: { tone: 'neutral', key: 'tools.state.off' },
  feature_flag: { tone: 'neutral', key: 'tools.state.feature_off' },
  opt_in: { tone: 'neutral', key: 'tools.state.opt_in' },
  not_implemented: { tone: 'info', key: 'tools.state.not_implemented' },
  no_key: { tone: 'warn', key: 'tools.state.no_key' },
  practice_unavailable: { tone: 'neutral', key: 'tools.state.practice_unavailable' },
  no_messages: { tone: 'info', key: 'tools.state.no_memory' },
  needs_reindex: { tone: 'warn', key: 'tools.state.needs_index' },
  embed_model_changed: { tone: 'warn', key: 'tools.state.needs_index' },
  pending_index: { tone: 'warn', key: 'tools.state.needs_index' },
}

/** A tool's status badge from the server's readiness (any other reason: a model it needs is missing). */
export function toolState(tl: any): { tone: 'ok' | 'neutral' | 'info' | 'warn'; label: string } {
  if (tl.ready) return { tone: 'ok', label: t('tools.state.ready') }
  const r = reasonState[tl.ready_reason] ?? { tone: 'warn', key: 'tools.state.needs_model' }
  return { tone: r.tone, label: t(r.key) }
}

/** Something the operator can fix: on, built, and not ready (an empty memory is not a fault). */
export const toolNeedsAttention = (tl: any) => tl.enabled && !tl.ready && (reasonState[tl.ready_reason]?.tone ?? 'warn') === 'warn'

/** An input's label: the tool's own wording first (fortune's "what" is not a reminder's "what"). */
export const inputLabel = (tool: string, name: string) =>
  has(`toolp.${tool}.${name}`) ? t(`toolp.${tool}.${name}`) : has(`toolp.${name}`) ? t(`toolp.${name}`) : name.replace(/_/g, ' ')
export const optionLabel = (tool: string, name: string, v: string) =>
  has(`toolp.${tool}.${name}.${v}`) ? t(`toolp.${tool}.${name}.${v}`) : has(`toolp.${name}.${v}`) ? t(`toolp.${name}.${v}`) : v
/** A localized example when the console has one for this tool, else the server's neutral one. */
export const inputExample = (tool: string, s: ToolInput) => (has(`toolp.ex.${tool}.${s.name}`) ? t(`toolp.ex.${tool}.${s.name}`) : s.example ?? '')

/** Form values from a JSON object (the tool's saved defaults or its sample input). */
export function toValues(inputs: ToolInput[], obj: Record<string, unknown> | undefined): InputValues {
  const out: InputValues = {}
  for (const s of inputs) {
    const v = obj?.[s.name]
    if (v === undefined || v === null) continue
    out[s.name] = s.type === 'bool' ? v === true : String(v)
  }
  return out
}

/** The JSON a tool takes from form values. Keys the form does not show are kept from base. Returns
 * the name of a missing required input instead when requireAll is set. */
export function toObject(inputs: ToolInput[], vals: InputValues, base: Record<string, unknown> = {}, requireAll = false): { obj: Record<string, unknown>; missing?: string } {
  const obj: Record<string, unknown> = { ...base }
  for (const s of inputs) {
    const raw = vals[s.name]
    delete obj[s.name]
    if (s.type === 'bool') {
      if (raw === true || raw === false) obj[s.name] = raw
      continue
    }
    const v = typeof raw === 'string' ? raw.trim() : ''
    if (!v) {
      if (requireAll && s.required) return { obj, missing: s.name }
      continue
    }
    obj[s.name] = s.type === 'number' || (s.type === 'select' && /^-?\d+$/.test(v)) ? Number(v) : v
  }
  return { obj }
}

/** A filled-in example of the JSON a tool takes, for the "insert an example" button. */
export function exampleInput(tool: string, inputs: ToolInput[], sample?: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...(sample ?? {}) }
  for (const s of inputs) {
    if (out[s.name] !== undefined || s.type === 'provider') continue
    const ex = inputExample(tool, s).split(' · ')[0]
    if (s.type === 'bool') out[s.name] = false
    else if (s.type === 'select' && s.options?.length) out[s.name] = /^\d+$/.test(s.options[0]) ? Number(s.options[0]) : s.options[0]
    else if (s.type === 'number') out[s.name] = Number(ex) || s.min || 1
    else if (s.required || ex) out[s.name] = ex || inputLabel(tool, s.name)
  }
  return out
}

export const groupOrder = ['info', 'talk', 'community', 'personal', 'custom', 'later', 'internal'] as const
export type Group = (typeof groupOrder)[number]

const groupOf: Record<string, Group> = {
  'weather.query': 'info', 'disasters.query': 'info', 'finance.quote': 'info', 'finance.fx': 'info', 'search.web': 'info', 'places.search': 'info', 'web.read': 'info',
  'chat.respond': 'talk', 'memory.search': 'talk', 'summaries.channel': 'talk',
  'polls.create': 'community', 'activity.ranking': 'community', 'images.generate': 'community', 'transfer.recent': 'info',
  'reminders.propose': 'personal', 'reminders.update': 'personal',
  'fortune.query': 'personal', 'fortune.subscribe': 'personal', 'notices.status': 'personal', 'notices.subscribe': 'personal',
  'polls.vote': 'internal', 'polls.close': 'internal', 'polls.result': 'internal', 'help.version': 'internal',
}

export function toolGroup(tl: any): Group {
  if (tl.kind === 'http') return 'custom'
  if (!tl.implemented) return 'later'
  return groupOf[tl.id] ?? 'info'
}
