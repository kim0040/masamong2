// What the Connections page's parts share: the two lists the server owns (providers and channels), what they
// say about each service and role, and testing a service. The page creates one while it is built and hands it
// to its components, so none of them reads the lists again or knows a host by name.
import { t, has } from './i18n.svelte'
import { call } from './api.svelte'
import { resource } from './resource.svelte'
import { cfg } from './config.svelte'
import { toast } from './toast.svelte'
import { ms } from './format'
import { widthChoices, widthState, narrowWithoutRerank, type EmbedWidthInfo } from './embedwidth'
import { ROLES, blockedRoles, effortLevels, groupOf, isPractice, roleState, rolesUsing, splitGroups, type Group, type ModelCaps, type Reasoning, type RoleState } from './connections'

const CHANNELS_POLL_MS = 30_000
// the host answers a role's reasoning lookup (caps_pending) within about 20 s: read the table again at these delays
const CAPS_RECHECK_MS = [2000, 5000, 10_000]

export class Connections {
  readonly providers = resource<any>('providers.list', () => ({}), { live: ['config'] })
  readonly channels = resource<any>('channels.list', () => ({}), { live: ['discord', 'config'], poll: CHANNELS_POLL_MS })
  /** services whose connection test is running, by id */
  testing = $state<Record<string, boolean>>({})
  #capsTimers: ReturnType<typeof setTimeout>[] = []

  constructor() {
    $effect(() => () => this.#capsTimers.forEach(clearTimeout))
  }

  // what the server says about hosts, so the page knows none of them by name
  modelCaps = $derived((this.providers.data?.model_providers ?? {}) as ModelCaps)
  webKinds = $derived(((this.providers.data?.web_hosts ?? []) as any[]).map((h) => h.kind))
  provById = $derived(Object.fromEntries(((this.providers.data?.providers ?? []) as any[]).map((p) => [p.id, p])))
  prices = $derived(this.providers.data?.model_prices ?? {})
  /** models that are resting after failures, by "provider/model" */
  resting = $derived(Object.fromEntries(((this.providers.data?.resting_models ?? []) as any[]).map((r) => [r.key, r])))
  modelProviderIds = $derived(Object.keys(this.modelCaps).sort())

  isPractice = isPractice
  provName = (id: string): string => (id === 'discord' ? 'Discord' : has(`conn.provider.${id}`) ? t(`conn.provider.${id}`) : id)
  refKind = (ref: string): string =>
    ref?.startsWith('keychain:') ? t('conn.ref.keychain') : ref?.startsWith('file:') ? (ref.includes('/secrets/') ? t('conn.ref.stored') : t('conn.ref.file')) : ref?.startsWith('env:') ? t('conn.ref.env', { name: ref.slice(4) }) : t('conn.ref.none')
  roleNames = (list: string[]): string => list.map((r) => t(`role.${r}`)).join(', ')

  hostOf = (p: any): any =>
    this.modelCaps[p.id]
      ? (this.providers.data?.hosts ?? []).find((h: any) => (this.modelCaps[p.id].host ? h.id === this.modelCaps[p.id].host : h.api === this.modelCaps[p.id].api && h.api !== 'compatible'))
      : (this.providers.data?.web_hosts ?? []).find((h: any) => h.kind === p.kind)
  keyless = (p: any): boolean => p.kind === 'open_meteo' || p.kind === 'exchangerate' || p.builtin || isPractice(p) || !!this.hostOf(p)?.keyless
  groupOf = (p: any): Group => groupOf(p, this.modelCaps, this.webKinds)
  removable = (p: any): boolean => !p.builtin && !isPractice(p) && this.groupOf(p) !== 'data'
  usedByRole = (id: string): string[] => rolesUsing(cfg.doc?.models, id)
  pstate = (p: any): { tone: any; label: string } =>
    p.status === 'disabled'
      ? { tone: 'neutral', label: t('tools.state.off') }
      : this.providers.data?.health?.[p.id]?.rejected
        ? { tone: 'danger', label: t('conn.badge_rejected') }
        : p.configured
          ? { tone: 'ok', label: this.keyless(p) && !p.key_present ? t('tool.keyless') : t('tools.state.ready') }
          : { tone: 'warn', label: t('tools.state.no_key') }

  // each group shows working services as cards; optional ones still waiting for a key are one line each below them,
  // so a fresh instance is not a wall of "missing key" cards
  waitingKey = (p: any, g: Group): boolean => g !== 'model' && !p.configured && !p.builtin && !this.keyless(p) && p.status !== 'disabled' && !this.providers.data?.health?.[p.id]?.rejected
  groups = $derived(splitGroups(this.providers.data?.providers ?? [], this.groupOf, this.waitingKey))
  serviceGroups = $derived((['web', 'data'] as const).map((g) => [g, this.groups[g]] as const).filter(([, l]) => l.cards.length + l.waiting.length))

  roleState = (role: string): RoleState => roleState(cfg.doc?.models?.[role], this.provById, this.providers.data)
  blocked = $derived(
    blockedRoles(
      ROLES,
      this.roleState,
      (role) => cfg.doc.models[role].provider,
    ),
  )

  // the embedding width: what is stored (cut to the leading part of the model's vector, or all of it) and the widths the
  // model can be cut to. The practice model keeps the width of its own offline index, so it shows none.
  ew = $derived(this.providers.data?.embed_width as EmbedWidthInfo | undefined)
  embedShowsWidth = $derived(!!this.ew && !isPractice(this.provById[cfg.doc?.models?.embed?.provider] ?? {}))
  wstate = $derived(widthState(this.ew))
  widthOptions = $derived(widthChoices(this.ew))
  widthNoRerank = $derived(narrowWithoutRerank(this.ew))

  /** the Discord connection in one line: live, switched on but not answering, no token yet, or off */
  discordStatus = $derived.by((): { tone: 'ok' | 'warn' | 'neutral'; text: string } => {
    const live = !!this.channels.data?.live
    const on = !!cfg.doc?.discord?.enabled
    return {
      tone: live ? 'ok' : on ? 'warn' : 'neutral',
      text: live ? t('conn.ov.discord_on', { n: this.channels.data.guilds?.length ?? 0 }) : on && this.channels.data && !this.channels.data.discord_token ? t('conn.ov.discord_no_token') : on ? t('conn.ov.discord_down') : t('conn.ov.discord_off'),
    }
  })

  /** what the server knows about a model's reasoning ({reasoning, efforts, thinking_off}; empty: unknown) */
  reasoningOf = (provider: string, model: string): Reasoning => this.providers.data?.model_reasoning?.[`${provider}/${model}`] ?? {}
  /** the effort choices as a select's options, with the default first */
  effortOptions = (r: Reasoning | undefined, current: string) => [{ value: '', label: t('conn.effort_default'), disabled: false }, ...effortLevels(r, current).map((l) => ({ value: l.value, label: t(`conn.effort.${l.value}`), disabled: l.disabled }))]

  /** Tests a service's key (the provider's cheapest free request) and says what came back. */
  test = async (id: string): Promise<boolean> => {
    this.testing[id] = true
    const r = await call('providers.test', { id })
    this.testing[id] = false
    // a host whose model list opens without a key (NanoGPT) could not show the key works yet
    if (r.ok && r.output?.model_list === 'unavailable' && r.output?.key_check !== 'chat') toast('warn', t('conn.no_list_test', { id: this.provName(id) }), t('conn.no_list_test_body'))
    else if (r.ok && r.output?.key_check === 'unverified') toast('warn', t('conn.key_unverified', { id: this.provName(id) }), t('conn.key_unverified_body'))
    else if (r.ok) toast('ok', t('conn.test_ok', { id: this.provName(id) }), r.output?.latency_ms ? ms(r.output.latency_ms) : undefined)
    else toast('error', t('conn.test_fail', { id: this.provName(id) }), r.error?.message)
    void this.providers.refresh()
    // the host is asked about its role models' reasoning after the test answers: read the Models table's source
    // again while that lookup is likely running (it takes up to 20 s)
    if (r.ok && r.output?.caps_pending) for (const at of CAPS_RECHECK_MS) this.#capsTimers.push(setTimeout(() => void this.providers.refresh(), at))
    return r.ok
  }
}
