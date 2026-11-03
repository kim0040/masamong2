<script lang="ts">
  import { tick } from 'svelte'
  import { t, has } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { tp, platform } from '../lib/platform'
  import { call, postJSON, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { onLive } from '../lib/live.svelte'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import { toast } from '../lib/toast.svelte'
  import { num, ms, relative, datetime, usd, duration } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Status from '../components/Status.svelte'
  import Dialog from '../components/Dialog.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import Select from '../components/Select.svelte'
  import Switch from '../components/Switch.svelte'
  import Segmented from '../components/Segmented.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import Confirm from '../components/Confirm.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import IconButton from '../components/IconButton.svelte'
  import LimitDialog from '../components/LimitDialog.svelte'
  import { embedReindexAsk, type EmbedAsk } from '../lib/embedswitch'
  import { widthChoices, widthDirection, widthState, narrowWithoutRerank, widthBasis, sameWidthBasis, widthRead, widthApplied, widthApproval, widthRequest, widthReplyFits, recommendedWidth, recTicks, recAfterRefresh, recLiveStart, recLiveChange, recLiveLoaded, type EmbedWidthInfo, type WidthChoice, type WidthBasis, type WidthApproval, type RecWidth, type RecLive } from '../lib/embedwidth'
  import * as I from '../lib/icons'

  useConfig()
  const providers = resource<any>('providers.list', () => ({}), { live: ['config'] })
  const channels = resource<any>('channels.list', () => ({}), { live: ['discord', 'config'], poll: 30000 })

  // key entry
  let keyOpen = $state(false)
  let keyFor = $state('')
  let keyMode = $state<'value' | 'ref'>('value')
  let keyValue = $state('')
  let keyRef = $state('env:')
  let keyErr = $state('')
  let keyBusy = $state(false)
  function openKey(id: string) {
    keyFor = id
    keyMode = 'value'
    keyValue = ''
    keyRef = providers.data?.providers?.find((p: any) => p.id === id)?.credential_ref ?? 'env:'
    keyErr = ''
    keyOpen = true
  }
  async function saveKey() {
    keyErr = ''
    keyBusy = true
    let r: any
    try {
      if (keyMode === 'value') {
        if (!keyValue.trim()) return (keyErr = t('conn.key_required'))
        const res = await postJSON('/api/secret', keyFor === 'discord' ? { target: 'discord', value: keyValue.trim() } : { target: 'provider', id: keyFor, value: keyValue.trim() })
        r = res.body
      } else {
        if (!(providers.data?.keychain ? /^(env|file|keychain):.+/ : /^(env|file):.+/).test(keyRef)) return (keyErr = t('toolnew.err.cred_ref'))
        r = keyFor === 'discord' ? await patchConfig([{ path: ['discord', 'token_ref'], value: keyRef }], { quiet: true }) : await call('providers.credentials', { id: keyFor, credential_ref: keyRef })
      }
    } finally {
      keyBusy = false
    }
    // what was typed stays in the field when saving failed, so it can be sent again
    if (!r?.ok) return (keyErr = r?.error?.message ?? t('common.failed'))
    keyValue = ''
    keyOpen = false
    const where = r?.output?.stored_in ? tp(`conn.stored_in.${r.output.stored_in}`) : undefined
    toast('ok', t('conn.key_saved', { id: provName(keyFor) }), keyFor === 'discord' ? t('setup.discord.restart_hint') : where, keyFor === 'discord' ? { label: t('maint.restart_now'), run: () => void call('runtime.restart') } : undefined)
    // a saved key is not yet a working one: try it right away (the provider's cheapest free request)
    if (keyFor === 'discord') void checkBot()
    else {
      const ok = await test(keyFor)
      // a new model provider no role uses yet: offer to fill the roles it can serve
      if (ok && modelCaps[keyFor] && !usedByRole(keyFor).length) void openRecommend(keyFor)
    }
    void providers.refresh()
  }
  // test
  let testing = $state<Record<string, boolean>>({})
  async function test(id: string): Promise<boolean> {
    testing[id] = true
    const r = await call('providers.test', { id })
    testing[id] = false
    // a host whose model list opens without a key (NanoGPT) could not show the key works yet
    if (r.ok && r.output?.model_list === 'unavailable' && r.output?.key_check !== 'chat') toast('warn', t('conn.no_list_test', { id: provName(id) }), t('conn.no_list_test_body'))
    else if (r.ok && r.output?.key_check === 'unverified') toast('warn', t('conn.key_unverified', { id: provName(id) }), t('conn.key_unverified_body'))
    else if (r.ok) toast('ok', t('conn.test_ok', { id: provName(id) }), r.output?.latency_ms ? ms(r.output.latency_ms) : undefined)
    else toast('error', t('conn.test_fail', { id: provName(id) }), r.error?.message)
    void providers.refresh()
    return r.ok
  }
  // what the server says about hosts, so this page knows none of them by name
  const modelCaps = $derived((providers.data?.model_providers ?? {}) as Record<string, { api: string; rerank: boolean; host?: string }>)
  const webKinds = $derived(((providers.data?.web_hosts ?? []) as any[]).map((h) => h.kind))
  // the practice mode's offline model is a model provider too, though it needs no key and lists no models
  const isPractice = (p: any) => p.kind === 'practice'
  const groupOf = (p: any): 'model' | 'web' | 'data' => (modelCaps[p.id] || isPractice(p) ? 'model' : webKinds.includes(p.kind) ? 'web' : 'data')
  // each group shows working services as cards; optional ones still waiting for a key are one line
  // each below them, so a fresh instance is not a wall of "missing key" cards
  const waitingKey = (p: any, g: string) => g !== 'model' && !p.configured && !p.builtin && !keyless(p) && p.status !== 'disabled' && !providers.data?.health?.[p.id]?.rejected
  const groups = $derived.by(() => {
    const out: Record<'model' | 'web' | 'data', { cards: any[]; waiting: any[] }> = { model: { cards: [], waiting: [] }, web: { cards: [], waiting: [] }, data: { cards: [], waiting: [] } }
    for (const p of providers.data?.providers ?? []) {
      const g = groupOf(p)
      out[g][waitingKey(p, g) ? 'waiting' : 'cards'].push(p)
    }
    return out
  })
  const serviceGroups = $derived((['web', 'data'] as const).map((g) => [g, groups[g]] as const).filter(([, l]) => l.cards.length + l.waiting.length))
  const hostOf = (p: any) =>
    modelCaps[p.id]
      ? (providers.data?.hosts ?? []).find((h: any) => (modelCaps[p.id].host ? h.id === modelCaps[p.id].host : h.api === modelCaps[p.id].api && h.api !== 'compatible'))
      : (providers.data?.web_hosts ?? []).find((h: any) => h.kind === p.kind)
  const removable = (p: any) => !p.builtin && !isPractice(p) && groupOf(p) !== 'data'
  const usedByRole = (id: string) => Object.entries<any>(cfg.doc?.models ?? {}).filter(([, m]) => m?.provider === id).map(([r]) => r)

  // adding a provider: a listed model host, a web service, or any OpenAI-compatible address
  let addOpen = $state(false)
  let addSel = $state('')
  let addId = $state('')
  let addURL = $state('')
  // the request format of a custom address: OpenAI chat completions, or Anthropic's Messages API
  let addAPI = $state<'compatible' | 'anthropic'>('compatible')
  let addErr = $state('')
  let addBusy = $state(false)
  const present = (sel: string) => {
    const [kind, v] = sel.split(':')
    // a built-in (keyless Jina) does not count: adding it for real lets it take a key
    return (providers.data?.providers ?? []).some((p: any) => !p.builtin && (kind === 'host' ? p.id === v : p.kind === v))
  }
  function openAdd() {
    addSel = ''
    addId = ''
    addURL = ''
    addAPI = 'compatible'
    addErr = ''
    addOpen = true
  }
  async function addProvider() {
    addErr = ''
    const [kind, v] = addSel.split(':')
    let input: Record<string, unknown>
    if (kind === 'host') input = { host: v }
    else if (kind === 'kind') input = { kind: v }
    else {
      if (!addId.trim() || !/^https:\/\//.test(addURL.trim())) return void (addErr = t('conn.add_custom_invalid'))
      input = { api: addAPI, id: addId.trim(), base_url: addURL.trim() }
    }
    addBusy = true
    const r = await call('providers.add', input)
    addBusy = false
    if (!r.ok) return void (addErr = r.error?.message ?? t('common.failed'))
    addOpen = false
    await providers.refresh()
    const id = String(r.output.id)
    const host = (providers.data?.web_hosts ?? []).find((h: any) => h.kind === v)
    if (host?.keyless) toast('ok', t('conn.added', { id: provName(id) }))
    else openKey(id)
  }

  // where the provider in the key dialog hands out keys
  const keyHost = $derived(keyFor && keyFor !== 'discord' ? hostOf((providers.data?.providers ?? []).find((p: any) => p.id === keyFor) ?? {}) : null)

  // removing one
  let removeFor = $state('')
  let removeOpen = $state(false)
  async function removeProvider() {
    const r = await call('providers.remove', { id: removeFor })
    if (r.ok) toast('ok', t('conn.removed', { id: provName(removeFor) }))
    else toast('error', t('common.failed'), r.error?.message)
    removeFor = ''
    void providers.refresh()
  }

  // filling every role a model provider can serve with its newest suitable models. Each changed role
  // can be left out; the embedding model starts left out while memory is indexed (a new one empties
  // memory search until every room is embedded again), and choosing it shows what that costs.
  let recFor = $state('')
  let rec = $state<any>(null)
  let recBusy = $state(false)
  let recOn = $state<Record<string, boolean>>({})
  let recReindex = $state(true)
  // A settings change announced while the first preview is still being read is remembered, and the preview is read
  // again as soon as the first answer is in (Apply and the width proposals wait for that).
  let recLive: RecLive = { loading: false, dirty: false }
  async function openRecommend(id: string) {
    recFor = id
    rec = null
    recBusy = true
    recLive = recLiveStart()
    const r = await call('models.recommend', { provider: id })
    recBusy = false
    const loaded = recLiveLoaded(recLive, r.ok)
    recLive = loaded.state
    if (!r.ok) {
      recFor = ''
      return toast('error', t('conn.rec_failed'), r.error?.message)
    }
    recSeq++
    recRefreshing = false
    recApplied = {}
    rec = r.output
    recOn = recTicks(rec)
    recReindex = true
    if (loaded.refresh) void refreshRecommend()
  }
  // The width proposals (and the picks themselves) are true for the saved settings the preview was made on. A role
  // saved from this very dialog (the cheaper-model buttons) or changed in another tab moves those settings, so the
  // preview is read again: the ticks the operator set stay where the new preview still proposes the role, and
  // Apply waits for the new answer.
  let recRefreshing = $state(false)
  let recSeq = 0
  let recApplied: Record<string, { provider: string; model: string }> = {}
  async function refreshRecommend() {
    const id = recFor
    if (!id || !rec) return
    const my = ++recSeq
    recRefreshing = true
    const r = await call('models.recommend', { provider: id })
    if (my !== recSeq) return
    recRefreshing = false
    if (recFor !== id) return
    if (!r.ok) {
      // the preview on screen is for settings that have moved on, and a new one cannot be had: do not apply it
      recFor = ''
      return toast('error', t('conn.rec_failed'), r.error?.message)
    }
    // a confirmation asked about the old preview's picks is void
    if (embedOpen && embedRecApply) {
      embedOpen = false
      embedRecApply = false
    }
    rec = recAfterRefresh(r.output, recApplied)
    recOn = recTicks(rec, recOn)
  }
  $effect(() => {
    if (!recFor) return
    return onLive(['config'], () => {
      const c = recLiveChange(recLive, recBusy)
      recLive = c.state
      if (c.refresh) void refreshRecommend()
    })
  })
  // cost-effective alternatives per role (the server ranks them); one click sets the role like the picker does
  let budgetBusy = $state('')
  async function useBudget(role: string, model: string) {
    budgetBusy = `${role}|${model}`
    const r = await call('models.select', { role, provider: recFor, model })
    budgetBusy = ''
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    toast('ok', t('conn.model_set', { role: t(`role.${role}`), model }))
    rec.budget[role] = rec.budget[role].map((b: any) => ({ ...b, current: b.model === model }))
    // the curated pick for this role no longer applies: "Apply" must not send it and put the old model back
    recOn[role] = false
    recApplied[role] = { provider: recFor, model }
    for (const p of rec.picks ?? []) if (p.role === role) Object.assign(p, { current_provider: recFor, current_model: model, replaced: p.model !== model, changed: false })
    void providers.refresh()
    // the saved settings moved: the width proposals made on the old ones are read again
    void refreshRecommend()
  }
  const recEmbed = $derived(!!rec?.reindex && !!recOn.embed)
  // the width the server would store for the picks as ticked now: with or without the rerank pick
  const recW = $derived(recRefreshing ? null : recommendedWidth(rec, recOn))
  const recWidthText = (w: RecWidth) => (w.dimensions === 0 ? (w.full > 0 ? t('conn.rec_width_whole', { n: w.full }) : t('conn.width_unknown')) : t('conn.rec_width', { n: w.dimensions }))
  async function applyRecommend(includeEmbed = false) {
    if (recRefreshing) return
    recBusy = true
    // exactly what the preview showed and the operator kept: no second model list
    const picks = Object.fromEntries((rec?.picks ?? []).filter((x: any) => x.changed && recOn[x.role]).map((x: any) => [x.role, x.model]))
    const r = await call('models.recommend', { provider: recFor, apply: true, picks, include_embed: recEmbed || includeEmbed, reindex: recEmbed && recReindex })
    recBusy = false
    if (!r.ok) {
      // memory was indexed after the preview: the server refuses with the estimate, so ask like the model picker does
      const ask = picks.embed && !includeEmbed ? embedReindexAsk(r.error) : null
      if (!ask) return toast('error', t('common.failed'), r.error?.message)
      embedAsk = { ...ask, provider: recFor, model: picks.embed }
      embedRecApply = true
      embedOpen = true
      return
    }
    const jobs = (r.output?.reindex_jobs ?? []).length
    if (jobs) toast('ok', t('conn.rec_applied', { id: provName(recFor) }), t('conn.rec_reindex_started', { n: jobs }), { label: t('conn.rec_jobs_go'), run: () => void (location.href = '/jobs') })
    else toast('ok', t('conn.rec_applied', { id: provName(recFor) }), r.output?.embed_changes ? t('conn.rec_reindex') : undefined, r.output?.embed_changes ? { label: t('conn.rec_reindex_go'), run: () => void (location.href = '/blocks') } : undefined)
    recFor = ''
    void providers.refresh()
  }

  const refKind = (ref: string) =>
    ref?.startsWith('keychain:') ? t('conn.ref.keychain') : ref?.startsWith('file:') ? (ref.includes('/secrets/') ? t('conn.ref.stored') : t('conn.ref.file')) : ref?.startsWith('env:') ? t('conn.ref.env', { name: ref.slice(4) }) : t('conn.ref.none')
  const provName = (id: string) => (id === 'discord' ? 'Discord' : has(`conn.provider.${id}`) ? t(`conn.provider.${id}`) : id)
  const keyless = (p: any) => p.kind === 'open_meteo' || p.kind === 'exchangerate' || p.builtin || isPractice(p) || !!hostOf(p)?.keyless
  const pstate = (p: any): { tone: any; label: string } =>
    p.status === 'disabled' ? { tone: 'neutral', label: t('tools.state.off') } : providers.data?.health?.[p.id]?.rejected ? { tone: 'danger', label: t('conn.badge_rejected') } : p.configured ? { tone: 'ok', label: keyless(p) && !p.key_present ? t('tool.keyless') : t('tools.state.ready') } : { tone: 'warn', label: t('tools.state.no_key') }
  const prices = $derived(providers.data?.model_prices ?? {})
  function priceText(role: string): string {
    const p = prices[role]
    if (!p || p.source === 'unknown') return t('conn.price_unknown')
    const out = p.out_per_mtok ? ` / ${usd(p.out_per_mtok)}` : ''
    return `${usd(p.in_per_mtok)}${out}`
  }
  const priceSource = (role: string) => {
    const p = prices[role]
    return p?.source === 'published' ? t('conn.price_src.published', { date: p.checked ?? '' }) : t(`conn.price_src.${p?.source}`)
  }
  // a published price is a snapshot the release carried; past ~4 months it may no longer be the host's
  const priceStale = (role: string) => {
    const p = prices[role]
    return p?.source === 'published' && !!p.checked && Date.now() - Date.parse(p.checked) > 120 * 864e5
  }
  // an operator price for hosts that do not list prices (USD per million tokens)
  let priceFor = $state('')
  let priceIn = $state('')
  let priceOut = $state('')
  let priceCached = $state('')
  let priceErr = $state('')
  // how long a chat model may take before the other chat model answers (planner and response only)
  let timeoutSecs = $state('')
  let timeoutErr = $state('')
  // how much a reasoning model thinks on the role's calls (models.<role>.effort; '' = low); every chat
  // role takes one (the server's config.FallbackRoles), a fallback may name its own
  let effort = $state('')
  const efforts = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']
  const chatRole = (role: string) => role === 'planner' || role === 'response'
  const effortRole = (role: string) => chatRole(role) || role === 'summary' || role === 'vision'
  // what the server knows about a model's reasoning ({reasoning, efforts, thinking_off}; empty: unknown)
  const reasoningOf = (provider: string, model: string): any => providers.data?.model_reasoning?.[`${provider}/${model}`] ?? {}
  // the capability label: thinks / does not think / not known (a list that says nothing)
  const reasonKey = (r: any) => (r?.reasoning === true ? 'conn.reason_yes' : r?.reasoning === false ? 'conn.reason_no' : 'conn.reason_unknown')
  // the effort choices; levels the model's list says it lacks stay visible but cannot be picked (a level
  // already saved stays pickable, so the dialog never shows a value it cannot hold)
  // which calls the level covers: the summary role's job is thinking-free unless set, the others low
  const effortHintKey = (role: string) => (role === 'summary' ? 'conn.effort_hint_summary' : 'conn.effort_hint')
  const effortOptions = (r: any, current: string) => [
    { value: '', label: t('conn.effort_default') },
    ...efforts.map((e) => ({ value: e, label: t(`conn.effort.${e}`), disabled: e !== current && (r?.reasoning === false ? e !== 'none' : Array.isArray(r?.efforts) && !r.efforts.includes(e)) })),
  ]
  // roles whose call has a time limit: the chat roles and the picture describer
  const timeoutRole = (role: string) => chatRole(role) || role === 'vision'
  function openPrice(role: string) {
    priceFor = role
    priceIn = String(cfg.doc?.models?.[role]?.price_in_per_mtok ?? '')
    priceOut = String(cfg.doc?.models?.[role]?.price_out_per_mtok ?? '')
    priceCached = String(cfg.doc?.models?.[role]?.price_cached_in_per_mtok ?? '')
    timeoutSecs = String(cfg.doc?.models?.[role]?.timeout_seconds ?? '')
    effort = cfg.doc?.models?.[role]?.effort ?? ''
    priceErr = ''
    timeoutErr = ''
  }
  async function savePrice() {
    const a = priceIn.trim() === '' ? 0 : Number(priceIn)
    const b = priceOut.trim() === '' ? 0 : Number(priceOut)
    const c = priceCached.trim() === '' ? 0 : Number(priceCached)
    if ([a, b, c].some((v) => Number.isNaN(v) || v < 0 || v > 1000)) return void (priceErr = t('conn.price_invalid'))
    const secs = timeoutSecs.trim() === '' ? 0 : Number(timeoutSecs)
    if (timeoutRole(priceFor) && (!Number.isInteger(secs) || (secs !== 0 && (secs < 5 || secs > 600)))) return void (timeoutErr = t('conn.timeout_invalid'))
    const base = ['models', priceFor]
    const ops: any[] = [a ? { path: [...base, 'price_in_per_mtok'], value: a } : { path: [...base, 'price_in_per_mtok'], op: 'unset' }, b ? { path: [...base, 'price_out_per_mtok'], value: b } : { path: [...base, 'price_out_per_mtok'], op: 'unset' },
      c ? { path: [...base, 'price_cached_in_per_mtok'], value: c } : { path: [...base, 'price_cached_in_per_mtok'], op: 'unset' }]
    if (timeoutRole(priceFor)) ops.push(secs ? { path: [...base, 'timeout_seconds'], value: secs } : { path: [...base, 'timeout_seconds'], op: 'unset' })
    if (effortRole(priceFor)) ops.push(effort ? { path: [...base, 'effort'], value: effort } : { path: [...base, 'effort'], op: 'unset' })
    const r = await patchConfig(ops, { success: t('conn.price_saved') })
    if (r.ok) {
      priceFor = ''
      void providers.refresh()
    }
  }

  // a provider's daily call limit (LimitDialog)
  let limitP = $state<any>(null)
  let limitOpen = $state(false)

  // fallbacks (models.<role>.fallbacks): asked in order when the role's model fails; the server's
  // config.FallbackRoles and MaxFallbacks
  const fallbackRoles = ['response', 'planner', 'summary', 'vision']
  const maxFallbacks = 3
  type FB = { provider: string; model: string; price_in: string; price_out: string; timeout: string; effort: string }
  let fbFor = $state('')
  let fbRows = $state<FB[]>([])
  let fbErr = $state('')
  let fbBusy = $state(false)
  let fbModels = $state<Record<string, any[]>>({})
  // a fallback's reasoning: what its provider's list says, else what the server knows of a saved one
  const fbReasoning = (r: FB) => {
    const m = (fbModels[r.provider] ?? []).find((x) => x.id === r.model.trim())
    return m && (m.reasoning !== undefined || m.efforts) ? m : reasoningOf(r.provider, r.model.trim())
  }
  const resting = $derived(Object.fromEntries(((providers.data?.resting_models ?? []) as any[]).map((r) => [r.key, r])))
  const modelProviderIds = $derived(Object.keys(modelCaps).sort())
  function openFallbacks(role: string) {
    fbFor = role
    fbErr = ''
    fbRows = ((cfg.doc?.models?.[role]?.fallbacks ?? []) as any[]).map((f) => ({ provider: f.provider, model: f.model,
      price_in: f.price_in_per_mtok ? String(f.price_in_per_mtok) : '', price_out: f.price_out_per_mtok ? String(f.price_out_per_mtok) : '', timeout: f.timeout_seconds ? String(f.timeout_seconds) : '', effort: f.effort ?? '' }))
    for (const r of fbRows) void loadFbModels(r.provider)
  }
  function addFallback() {
    const used = new Set([cfg.doc?.models?.[fbFor]?.provider, ...fbRows.map((r) => r.provider)])
    const provider = modelProviderIds.find((id) => !used.has(id)) ?? modelProviderIds[0] ?? ''
    fbRows = [...fbRows, { provider, model: cfg.doc?.models?.[fbFor]?.model ?? '', price_in: '', price_out: '', timeout: '', effort: '' }]
    void loadFbModels(provider)
  }
  function moveFallback(i: number, d: number) {
    const rows = [...fbRows]
    ;[rows[i], rows[i + d]] = [rows[i + d], rows[i]]
    fbRows = rows
  }
  async function loadFbModels(provider: string) {
    if (!provider || fbModels[provider]) return
    const r = await call('models.list', { provider, role: '' })
    if (r.ok) fbModels = { ...fbModels, [provider]: ((r.output.models ?? []) as any[]).filter((m) => m.id) }
  }
  async function saveFallbacks() {
    const m = cfg.doc?.models?.[fbFor]
    const seen = new Set([`${m?.provider}|${m?.model}`])
    const list: any[] = []
    for (const r of fbRows) {
      const model = r.model.trim()
      if (!r.provider || !model) return void (fbErr = t('conn.fb_need_model'))
      if (seen.has(`${r.provider}|${model}`)) return void (fbErr = t('conn.fb_duplicate', { model }))
      seen.add(`${r.provider}|${model}`)
      const pin = r.price_in.trim() === '' ? 0 : Number(r.price_in)
      const pout = r.price_out.trim() === '' ? 0 : Number(r.price_out)
      if ([pin, pout].some((v) => Number.isNaN(v) || v < 0 || v > 1000)) return void (fbErr = t('conn.price_invalid'))
      const secs = r.timeout.trim() === '' ? 0 : Number(r.timeout)
      if (!Number.isInteger(secs) || (secs !== 0 && (secs < 5 || secs > 600))) return void (fbErr = t('conn.timeout_invalid'))
      const f: any = { provider: r.provider, model }
      if (pin) f.price_in_per_mtok = pin
      if (pout) f.price_out_per_mtok = pout
      if (secs) f.timeout_seconds = secs
      if (r.effort) f.effort = r.effort
      list.push(f)
    }
    fbBusy = true
    const path = ['models', fbFor, 'fallbacks']
    const res = await patchConfig([list.length ? { path, value: list } : { path, op: 'unset' }], { success: t('conn.fb_saved') })
    fbBusy = false
    if (res.ok) {
      fbFor = ''
      void providers.refresh()
    } else fbErr = res.error?.message ?? t('common.failed')
  }

  // models
  const roles = ['response', 'planner', 'summary', 'embed', 'rerank', 'vision', 'image']
  // roles that may stay empty (the server's config.OptionalModelRoles): the table offers to clear them
  const optionalRoles = ['vision', 'image']
  let clearFor = $state('')
  let clearOpen = $state(false)
  async function clearRole() {
    const role = clearFor
    const r = await call('models.select', { role, clear: true })
    if (r.ok) toast('ok', t('conn.role_cleared', { role: t(`role.${role}`) }))
    else toast('error', t('common.failed'), r.error?.message)
    clearFor = ''
    void providers.refresh()
  }
  const budgetRoles = $derived(roles.filter((r) => rec?.budget?.[r]?.length))
  let modelsFor = $state('')
  let modelProv = $state('')
  let modelList = $state<any[]>([])
  let modelFilter = $state('')
  let modelSort = $state<'list' | 'price_asc' | 'price_desc' | 'ctx'>('list')
  let loadingModels = $state(false)
  // the role's provider when it is a model provider, else the first model provider there is
  const roleProvider = (role: string) => {
    const cur = cfg.doc?.models?.[role]?.provider
    return cur && modelCaps[cur] ? cur : Object.keys(modelCaps)[0] ?? ''
  }
  async function browse(role: string, provider = roleProvider(role)) {
    modelsFor = role
    modelProv = provider
    modelFilter = ''
    modelSort = 'list'
    modelList = []
    noList = false
    typedModel = ''
    if (!provider) return
    loadingModels = true
    const r = await call('models.list', { provider, role: role === 'embed' ? 'embedding' : role === 'image' ? 'image' : '' })
    loadingModels = false
    modelList = r.ok ? r.output.models ?? [] : []
    noList = r.ok && r.output?.status === 'no_list'
    if (!r.ok) toast('error', t('conn.models_failed'), r.error?.message)
  }
  // an Anthropic-format host that answers /messages only has no list: the model ID is typed
  let noList = $state(false)
  let typedModel = $state('')
  // an indexed embedding model is only switched after the operator confirms the re-index it implies:
  // the server refuses (with the estimate) until the request carries include_embed
  let embedAsk = $state<(EmbedAsk & { provider: string; model: string }) | null>(null)
  let embedOpen = $state(false)
  // the confirm belongs to the recommendation dialog's Apply (all its picks), not to a single model pick
  let embedRecApply = false
  async function pickModel(role: string, model: string, includeEmbed = false) {
    const provider = modelProv
    const r = await call('models.select', includeEmbed ? { role, provider, model, include_embed: true } : { role, provider, model })
    if (r.ok) {
      toast('ok', t('conn.model_set', { role: t(`role.${role}`), model }), includeEmbed ? t('conn.rec_reindex') : undefined, includeEmbed ? { label: t('conn.rec_reindex_go'), run: () => void (location.href = '/blocks') } : undefined)
      modelsFor = ''
      // the picker (and a confirm that was on top of it) are gone: keyboard focus goes back to the role's button
      await tick()
      document.querySelector<HTMLElement>(`[data-change-model="${role}"]`)?.focus()
      return
    }
    const ask = role === 'embed' && !includeEmbed ? embedReindexAsk(r.error) : null
    if (ask) {
      embedAsk = { ...ask, provider, model }
      embedRecApply = false
      embedOpen = true
    } else toast('error', r.error?.message ?? '')
  }
  // the estimate the refusal carried; without it the generic note, never "0 pieces in 0 rooms"
  const embedBody = (a: EmbedAsk | null) =>
    !a ? '' : !a.known ? t('conn.embed_change_note') : t('conn.embed_switch_body', { leaves: num(a.leaves), rooms: num(a.rooms) }) + (a.tokens > 0 ? ` ${t('conn.embed_switch_tokens', { tokens: num(a.tokens) })}` : '')
  async function confirmEmbedSwitch() {
    if (!embedAsk) return
    if (embedRecApply) {
      embedRecApply = false
      embedOpen = false
      await tick()
      await applyRecommend(true)
      return
    }
    const { provider, model } = embedAsk
    modelProv = provider
    // close the confirm first: closing the picker under an open modal would leave focus on <body>
    embedOpen = false
    await tick()
    await pickModel('embed', model, true)
  }
  // chat roles list chat models only (the host may also list speech, video or music models)
  const roleKind = (role: string) => (role === 'embed' ? 'embedding' : role === 'image' ? 'image' : role === 'rerank' ? 'rerank' : 'chat')
  // the blended price per million tokens the server's budget ranking uses; null when the list has no price
  const blended = (m: any): number | null => (typeof m.in_per_mtok === 'number' ? (3 * m.in_per_mtok + (m.out_per_mtok ?? 0)) / 4 : null)
  // a list that says nothing about picture input (name-based lists) cannot narrow the vision role
  const listKnowsImages = $derived(modelList.some((m) => m.image_input !== undefined))
  const imageUnknown = $derived(modelsFor === 'vision' && modelList.length > 0 && !listKnowsImages)
  const filteredModels = $derived.by(() => {
    let list = modelList
      .filter((m) => m.kind === 'unknown' || m.kind === roleKind(modelsFor) || (modelsFor === 'image' && m.image_output))
      .filter((m) => !modelFilter || `${m.id} ${m.name}`.toLowerCase().includes(modelFilter.toLowerCase()))
    // only a model the list says takes no pictures is hidden; one it says nothing about stays, marked
    if (modelsFor === 'vision') list = list.filter((m) => m.image_input !== false && !m.image_output)
    if (modelSort === 'price_asc' || modelSort === 'price_desc') {
      const dir = modelSort === 'price_asc' ? 1 : -1
      // models without a price go last either way
      list = [...list].sort((a, b) => {
        const x = blended(a), y = blended(b)
        return x === null && y === null ? 0 : x === null ? 1 : y === null ? -1 : (x - y) * dir
      })
    } else if (modelSort === 'ctx') list = [...list].sort((a, b) => (b.context_length ?? 0) - (a.context_length ?? 0))
    return list.slice(0, 80)
  })
  const perM = (v: number | undefined) => (typeof v === 'number' ? usd(v) : t('conn.price_none'))
  const isCurrent = (m: any) => cfg.doc?.models?.[modelsFor]?.model === m.id && cfg.doc?.models?.[modelsFor]?.provider === modelProv

  // whether each role can answer now: its provider needs a key that was not refused
  const provById = $derived(Object.fromEntries(((providers.data?.providers ?? []) as any[]).map((p) => [p.id, p])))
  const roleState = (role: string): 'unset' | 'ok' | 'no_key' | 'rejected' | 'unknown' => {
    const m = cfg.doc?.models?.[role]
    if (!m?.model) return 'unset'
    if (!providers.data) return 'unknown'
    if (!provById[m.provider]?.configured) return 'no_key'
    return providers.data.health?.[m.provider]?.rejected ? 'rejected' : 'ok'
  }
  // stopped roles, one line per provider that holds them up (instead of the same warning on every row)
  const blocked = $derived.by(() => {
    const by = new Map<string, { id: string; rejected: boolean; roles: string[] }>()
    for (const role of roles) {
      const st = roleState(role)
      if (st !== 'no_key' && st !== 'rejected') continue
      const id = cfg.doc.models[role].provider
      if (!by.has(id)) by.set(id, { id, rejected: st === 'rejected', roles: [] })
      by.get(id)!.roles.push(role)
    }
    return [...by.values()]
  })
  // the embedding width: what is stored (cut to the leading part of the model's vector, or all of it) and the
  // widths the model can be cut to. The practice model keeps the width of its own offline index, so it shows none.
  const ew = $derived(providers.data?.embed_width as EmbedWidthInfo | undefined)
  const embedShowsWidth = $derived(!!ew && !isPractice(provById[cfg.doc?.models?.embed?.provider] ?? {}))
  const wstate = $derived(widthState(ew))
  const widthOptions = $derived(widthChoices(ew))
  const widthNoRerank = $derived(narrowWithoutRerank(ew))
  const widthText = (info: EmbedWidthInfo) =>
    wstate === 'full' ? t('conn.width_full', { n: info.effective }) : wstate === 'narrow' ? t('conn.width_narrow', { n: info.effective, full: info.full_width }) : wstate === 'over' ? t('conn.width_over', { n: info.effective, full: info.full_width }) : wstate === 'custom' ? t('conn.width_custom', { n: info.effective }) : t('conn.width_unknown')
  // the tags beside a width: which one is the full width, the recommended one, the one in use
  const widthTags = (c: WidthChoice) => [c.full && t('conn.width_tag_full'), c.recommended && t('conn.width_tag_recommended'), c.current && t('conn.current')].filter(Boolean).join(' · ')
  let widthOpen = $state(false)
  let widthPick = $state(-1)
  let widthBusy = $state(false)
  let widthLowerOpen = $state(false)
  let widthRaiseOpen = $state(false)
  // What the operator approved on the screen (the basis and the direction it announced) is kept with the
  // confirmation, so the include_embed request that follows goes out with the same revision and direction rather
  // than the current ones.
  let widthAsk = $state<(EmbedAsk & { value: number; approval: WidthApproval }) | null>(null)
  let widthLowerApproval: WidthApproval | null = null
  let widthLoading = $state(false)
  // The picker works on its own read of providers.list's embed_width, and on nothing else. That object is made from
  // one configuration snapshot on the server: the revision it names, the model and provider, the stored width
  // and the choices all belong together. They are never mixed with config.document or the page's cached
  // providers data, which may be older or newer; a read that fails leaves nothing to apply (null), never the
  // previous answer. The revision goes out with the request as base_revision, and a change made elsewhere
  // meanwhile (another tab, a live event) voids the answers instead of applying them to a setting the operator
  // never saw.
  let wi = $state<EmbedWidthInfo | null>(null)
  const widthBase = $derived(widthBasis(wi))
  const pickState = $derived(widthState(wi))
  const pickOptions = $derived(widthChoices(wi))
  const pickNoRerank = $derived(narrowWithoutRerank(wi))
  const widthChoice = $derived(pickOptions.find((c) => c.value === widthPick))
  const widthDir = $derived(widthChoice ? widthDirection(wi, widthChoice) : 'same')
  let widthVoidedAt = 0
  let widthReadSeq = 0
  const currentWidthPick = () => pickOptions.find((c) => c.current)?.value ?? -1
  // Read the width again. 'stale' means a newer read took over (this answer is dropped). A different basis closes
  // every confirmation that was asked on the old one.
  async function loadWidth(notify: boolean): Promise<'ok' | 'failed' | 'stale'> {
    const my = ++widthReadSeq
    widthLoading = true
    const info = widthRead(await call('providers.list', {}))
    if (my !== widthReadSeq) return 'stale'
    widthLoading = false
    const before = widthBase
    wi = info
    if (sameWidthBasis(before, widthBase) && info) return 'ok'
    widthLowerOpen = false
    widthRaiseOpen = false
    widthAsk = null
    widthLowerApproval = null
    await tick()
    widthPick = currentWidthPick()
    if (notify && before && Date.now() - widthVoidedAt > 2000) toast('warn', t('conn.width_stale_title'), t('conn.width_stale_body'))
    widthVoidedAt = Date.now()
    return info ? 'ok' : 'failed'
  }
  async function openWidth() {
    wi = null
    const st = await loadWidth(false)
    if (st === 'stale') return
    if (st === 'failed') return toast('error', t('conn.width_read_failed'), t('conn.width_read_failed_body'))
    widthVoidedAt = 0
    widthOpen = true
  }
  $effect(() => {
    if (!widthOpen) return
    return onLive(['config'], () => {
      if (!widthBusy) void loadWidth(true)
    })
  })
  // lowering only cuts what is stored (no API call), so it is confirmed with that promise; raising embeds again
  function applyWidth() {
    if (!widthChoice || widthLoading) return
    const approval = widthApproval(widthBase, widthDir)
    if (!approval) return
    if (approval.direction === 'lower') {
      widthLowerApproval = approval
      widthLowerOpen = true
    } else void sendWidth(widthChoice, approval)
  }
  async function sendWidth(choice: WidthChoice, approval: WidthApproval, includeEmbed = false) {
    widthBusy = true
    const req = widthRequest(approval, choice.value, includeEmbed)
    const r = await call('models.embed_width', req.input, { base_revision: req.baseRevision })
    widthBusy = false
    if (r.ok) {
      widthOpen = false
      void providers.refresh()
      await tick()
      document.querySelector<HTMLElement>('[data-change-width]')?.focus()
      // the width was already what was asked for (changed elsewhere meanwhile): nothing was cut or embedded
      const done = widthApplied(r.output)
      if (done === 'unchanged') return toast('info', t('conn.width_unchanged', { n: choice.width }))
      const again = done === 'reembed'
      toast('ok', t('conn.width_set', { n: choice.width }), again ? t('conn.width_reindex') : t('conn.width_cut_done'), again ? { label: t('conn.rec_reindex_go'), run: () => void (location.href = '/blocks') } : undefined)
      return
    }
    // changed elsewhere since the picker opened: read it again and ask again, as every config save does
    if (r.error?.code === 'CONFIG_CONFLICT') {
      toast('warn', t('config.conflict_title'), t('config.conflict_body'))
      void providers.refresh()
      // only providers.list is read again, and the confirmation that was asked on the old basis is dropped
      widthLowerOpen = false
      widthRaiseOpen = false
      widthAsk = null
      widthLowerApproval = null
      wi = null
      widthVoidedAt = Date.now()
      await loadWidth(false)
      return
    }
    // memory is indexed: the server refuses the raise with the estimate until the request carries include_embed
    const ask = !includeEmbed ? embedReindexAsk(r.error) : null
    if (ask) {
      // the estimate was made for the width the request was sent on: when the picker has moved on meanwhile, it is
      // dropped and read again instead of being asked about on the new basis
      if (!widthReplyFits(approval, widthBase)) {
        widthAsk = null
        widthRaiseOpen = false
        toast('warn', t('conn.width_stale_title'), t('conn.width_stale_body'))
        await loadWidth(false)
        return
      }
      widthAsk = { ...ask, value: choice.value, approval }
      widthRaiseOpen = true
    } else toast('error', r.error?.message ?? t('common.failed'))
  }
  async function confirmWidthLower() {
    const choice = widthChoice
    const approval = widthLowerApproval
    widthLowerOpen = false
    widthLowerApproval = null
    await tick()
    if (choice && approval) await sendWidth(choice, approval)
  }
  const widthRaiseBody = (a: EmbedAsk | null) =>
    !a ? '' : !a.known ? t('conn.width_raise_note') : t('conn.width_raise_body', { leaves: num(a.leaves), rooms: num(a.rooms) }) + (a.tokens > 0 ? ` ${t('conn.embed_switch_tokens', { tokens: num(a.tokens) })}` : '')
  async function confirmWidthRaise() {
    const choice = pickOptions.find((c) => c.value === widthAsk?.value)
    const approval = widthAsk?.approval
    widthRaiseOpen = false
    await tick()
    if (choice && approval) await sendWidth(choice, approval, true)
  }
  // gateway health (read-only): the heartbeat decides the state; event age is only a hint, a quiet server is normal
  const gw = $derived(channels.data?.live ? channels.data.gateway : undefined)
  const gwTone = $derived(gw?.state === 'ok' ? 'ok' : gw?.state === 'check' ? 'warn' : 'neutral')
  const gwAge = (sec: number | undefined) => (sec === undefined || sec < 0 ? t('conn.gw.none') : t('conn.gw.ago', { when: sec < 60 ? t('conn.gw.secs', { n: num(sec) }) : duration(sec) }))
  const roleNames = (list: string[]) => list.map((r) => t(`role.${r}`)).join(', ')

  // the summary row: the two connections the bot cannot work without, then the optional services
  type Tile = { id: string; icon: any; label: string; tone: any; text: string; action?: { label: string; icon: any; run: () => void } }
  const tiles = $derived.by((): Tile[] => {
    const modelProviders = groups.model.cards
    const stopped = blocked.reduce((n, b) => n + b.roles.length, 0)
    const working = roles.filter((r) => roleState(r) === 'ok').length
    const model: Tile = !modelProviders.length
      ? { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: 'danger', text: t('conn.ov.model_none'), action: { label: t('conn.add_provider'), icon: I.Plus, run: openAdd } }
      : blocked.length
        ? { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: 'danger', text: blocked[0].rejected ? t('conn.ov.model_rejected', { id: provName(blocked[0].id) }) : t('conn.ov.model_missing', { n: stopped }),
            action: can('providers.credentials') ? { label: blocked[0].rejected ? t('conn.replace_key') : t('conn.add_key'), icon: I.KeyRound, run: () => openKey(blocked[0].id) } : undefined }
        : { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: providers.data ? 'ok' : 'neutral', text: providers.data ? t('conn.ov.model_ok', { n: working }) : t('common.loading') }
    const live = !!channels.data?.live
    const on = !!cfg.doc?.discord?.enabled
    const discord: Tile = { id: 'discord', icon: I.Bot, label: t('conn.discord'), tone: live ? 'ok' : on ? 'warn' : 'neutral', text: live ? t('conn.ov.discord_on', { n: channels.data.guilds?.length ?? 0 }) : on && channels.data && !channels.data.discord_token ? t('conn.ov.discord_no_token') : on ? t('conn.ov.discord_down') : t('conn.ov.discord_off') }
    const svc = (['web', 'data'] as const).flatMap((g) => groups[g].cards).filter((p) => pstate(p).tone === 'ok').length
    const wait = (['web', 'data'] as const).reduce((n, g) => n + groups[g].waiting.length, 0)
    const services: Tile = { id: 'services', icon: I.Globe, label: t('conn.ov.services'), tone: svc ? 'ok' : 'neutral', text: [t('conn.ov.services_on', { n: svc }), wait ? t('conn.ov.services_wait', { n: wait }) : ''].filter(Boolean).join(' · ') }
    // the practice instance has no search or data services: no tile for an empty section
    return serviceGroups.length ? [model, discord, services] : [model, discord]
  })
  function jump(id: string) {
    const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches
    document.getElementById(id)?.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'start' })
  }
  const groupIcon = { model: I.Sparkles, web: I.Globe, data: I.CloudSun }

  // discord
  async function setDiscord(key: string, value: unknown) {
    await patchConfig([{ path: ['discord', key], value }], { success: t('conn.discord_saved') })
  }
  // Claude on OpenRouter only on the operator's own Anthropic key (providers.<id>.byok_only)
  const byokClaude = (id: string) => cfg.doc?.providers?.[id]?.byok_only?.anthropic === 'anthropic'
  async function setByokClaude(id: string, on: boolean) {
    const path = ['providers', id, 'byok_only', 'anthropic']
    await patchConfig([on ? { path, value: 'anthropic' } : { path, op: 'unset' }], { success: t('conn.byok_saved') })
  }
  let invite = $state('')
  async function makeInvite() {
    const r = await call('channels.invite')
    if (r.ok) invite = r.output.url
    else toast('warn', t('conn.invite_failed'), r.error?.message)
  }
  // who the stored token belongs to, asked from Discord (the bot id is stored for the invite link)
  let bot = $state<any>(null)
  let botBusy = $state(false)
  let botErr = $state('')
  async function checkBot() {
    botBusy = true
    botErr = ''
    const r = await call('discord.check', { apply: true })
    botBusy = false
    if (r.ok) bot = r.output
    else {
      bot = null
      if (r.error?.args?.reason !== 'no_token') botErr = r.error?.message ?? t('common.failed')
    }
  }
</script>

<!-- what the model list says a model can do; nothing is shown for what it does not say -->
{#snippet capTags(m: any, chat: boolean, vision = false)}
  {#if m.image_input || m.supports_tools || m.known_family || (chat && m.supports_tools === false) || m.price_source === 'published' || m.price_source === 'reference' || (vision && m.image_input === undefined) || m.reasoning !== undefined}
    <div class="tags">
      {#if m.reasoning !== undefined}<span class="tag" class:dim={!m.reasoning}>{t(reasonKey(m))}</span>{/if}
      {#if m.image_input}<span class="tag ok">{t('conn.tag_image_in')}</span>{:else if vision && m.image_input === undefined}<span class="tag dim">{t('conn.tag_image_unknown')}</span>{/if}
      {#if m.supports_tools}<span class="tag">{t('conn.tag_tools')}</span>{:else if chat && m.supports_tools === false}<span class="tag warn">{t('conn.tag_no_tools')}</span>{/if}
      {#if m.known_family}<span class="tag">{t('conn.tag_known')}</span>{/if}
      {#if m.price_source === 'published' || m.price_source === 'reference'}<span class="tag dim">{t(`conn.tag_price_${m.price_source}`)}</span>{/if}
    </div>
  {/if}
{/snippet}

{#snippet providerGroup(g: 'model' | 'web' | 'data', list: any[], waiting: any[])}
  {#if list.length}<div class="providers">
    {#each list as p, i (p.id)}
      {@const ps = pstate(p)}
      {@const host = hostOf(p)}
      {@const GIcon = groupIcon[g]}
      <article class="prov enter" style:--i={i}>
        <header><span class="pi {g}" aria-hidden="true"><GIcon size={18} /></span><div class="pn"><h3>{provName(p.id)}</h3>{#if provName(p.id) !== p.id}<span class="xs subtle">{p.id}{#if modelCaps[p.id] && modelCaps[p.id].api !== p.id} · {t(`conn.api.${modelCaps[p.id].api}`)}{/if}</span>{/if}</div><span class="spacer"></span><Status tone={ps.tone} label={ps.label} size="sm" /></header>
        <div class="small muted">{isPractice(p) ? t('conn.practice_note') : p.builtin ? t('conn.builtin') : keyless(p) && !p.key_present ? t('conn.keyless_note') : refKind(p.credential_ref)}</div>
        {#if has(`tool.prov_note.${p.kind}`)}<div class="xs muted">{t(`tool.prov_note.${p.kind}`)}</div>{/if}
        {#if modelCaps[p.id] && !modelCaps[p.id].rerank}<div class="xs muted">{t('conn.no_rerank')}</div>{/if}
        {#if modelCaps[p.id]?.api === 'openrouter' && !isPractice(p) && cfg.doc}<Switch checked={byokClaude(p.id)} disabled={!can('config.patch')} onchange={(v) => setByokClaude(p.id, v)} label={t('conn.byok_claude')} hint={t('conn.byok_claude_hint')} />{/if}
        {#if !p.builtin && !isPractice(p)}
          <div class="quota xs"><span class="muted num">{p.daily_limit ? t('tool.provider_quota_n', { used: num(p.used_today ?? 0), limit: num(p.daily_limit) }) : t('limit.used_n', { used: num(p.used_today ?? 0) })}</span>
            <button type="button" class="quota-edit" disabled={!can('config.patch')} onclick={() => ((limitP = p), (limitOpen = true))}><I.Pencil size={12} aria-hidden="true" />{p.daily_limit ? t('limit.edit') : t('limit.set')}</button></div>
        {:else if p.daily_limit}<div class="xs muted num">{t('tool.provider_quota_n', { used: num(p.used_today), limit: num(p.daily_limit) })}</div>{/if}
        {#if p.error}<div class="xs subtle">{p.error}</div>{/if}
        {#if providers.data?.health?.[p.id]?.rejected}
          {@const h = providers.data.health[p.id]}
          <div class="callout danger tight"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{t('conn.key_rejected', { when: relative(h.last_error_at) })}</span></div>
        {/if}
        {#if providers.data?.health?.[p.id] && (providers.data.health[p.id].last_ok || providers.data.health[p.id].last_error_at || providers.data.health[p.id].key_updated_at)}
          {@const h = providers.data.health[p.id]}
          <dl class="health xs">
            {#if h.last_ok}<dt>{t('conn.last_ok')}</dt><dd title={datetime(h.last_ok, true)}>{relative(h.last_ok)}</dd>{/if}
            {#if h.last_error_at}<dt>{t('conn.last_error')}</dt><dd title={h.last_error}>{relative(h.last_error_at)}</dd>{/if}
            {#if h.key_updated_at}<dt>{t('conn.key_age')}</dt><dd title={datetime(h.key_updated_at, true)} class:old={Date.now() - Date.parse(h.key_updated_at) > 180 * 864e5}>{relative(h.key_updated_at)}</dd>{/if}
          </dl>
        {/if}
        <div class="row acts">
          {#if !p.builtin && !isPractice(p)}<Button size="sm" variant={!p.configured && !keyless(p) ? 'primary' : 'secondary'} icon={I.KeyRound} disabled={!can('providers.credentials')} onclick={() => openKey(p.id)}>{p.key_present ? t('conn.replace_key') : keyless(p) ? t('conn.add_key_optional') : t('conn.add_key')}</Button>{/if}
          <Button size="sm" variant="ghost" icon={I.Activity} loading={testing[p.id]} disabled={!p.configured} onclick={() => test(p.id)}>{t('conn.test')}</Button>
          {#if modelCaps[p.id] && p.configured}<Button size="sm" variant="ghost" icon={I.Sparkles} onclick={() => openRecommend(p.id)}>{t('conn.rec_open')}</Button>{/if}
          {#if host?.key_url && !p.key_present && !p.builtin}<a class="xs" href={host.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a>{/if}
          <span class="spacer"></span>
          {#if removable(p)}
            {@const inUse = usedByRole(p.id)}
            <IconButton icon={I.Trash2} size={16} tone="danger" disabled={inUse.length > 0} label={inUse.length ? t('conn.remove_in_use', { roles: roleNames(inUse) }) : t('conn.remove', { id: provName(p.id) })} onclick={() => ((removeFor = p.id), (removeOpen = true))} />
          {/if}
        </div>
      </article>
    {/each}
  </div>{/if}
  {#if waiting.length}
    <div class="waiting">
      <p class="xs muted">{list.length ? t('conn.waiting_more') : t('conn.waiting')}</p>
      <ul>
        {#each waiting as p (p.id)}
          {@const host = hostOf(p)}
          <li>
            <span class="grow"><strong class="small">{provName(p.id)}</strong>{#if has(`tool.prov_note.${p.kind}`)}<span class="xs muted"> · {t(`tool.prov_note.${p.kind}`)}</span>{/if}</span>
            {#if host?.key_url}<a class="xs" href={host.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a>{/if}
            <Button size="sm" icon={I.KeyRound} disabled={!can('providers.credentials')} onclick={() => openKey(p.id)}>{t('conn.add_key')}</Button>
            {#if removable(p)}<IconButton icon={I.Trash2} size={16} tone="danger" label={t('conn.remove', { id: provName(p.id) })} onclick={() => ((removeFor = p.id), (removeOpen = true))} />{/if}
          </li>
        {/each}
      </ul>
    </div>
  {/if}
{/snippet}

<PageHeader title={t('conn.title')} description={t('conn.desc')}>
  {#snippet actions()}<Button variant="primary" icon={I.Plus} onclick={openAdd}>{t('conn.add_provider')}</Button>{/snippet}
</PageHeader>

<!-- until both lists are here the page shows placeholders, not "Discord: Off" or "price unknown", which
     look like real answers while a slow call is still on its way -->
{#if !providers.data || !channels.data}
  <LoadState res={[providers, channels]}>
    <div class="grid" style:--min="240px">{#each Array(3) as _}<Card><Skeleton lines={2} /></Card>{/each}</div>
    <div style:margin-top="var(--s-5)"><Card><Skeleton lines={6} /></Card></div>
  </LoadState>
{:else}
<ul class="overview" aria-label={t('conn.overview')}>
  {#each tiles as tl (tl.id)}
    {@const TIcon = tl.icon}
    <li class="tile {tl.tone}">
      <button type="button" class="tile-go" onclick={() => jump(tl.id)} title={t('conn.open_section')}>
        <span class="ti" aria-hidden="true"><TIcon size={18} /></span>
        <span class="tt"><span class="tl small">{tl.label}</span><Status tone={tl.tone} label={tl.text} size="sm" /></span>
      </button>
      {#if tl.action}<Button size="sm" variant="primary" icon={tl.action.icon} onclick={tl.action.run}>{tl.action.label}</Button>{/if}
    </li>
  {/each}
</ul>

<section class="block" id="models" aria-labelledby="models-h">
  <div class="sec-head"><h2 id="models-h" class="sec-title">{t('conn.group.model')}</h2><span class="xs muted">{t('conn.group.model_hint')}</span></div>
  {@render providerGroup('model', groups.model.cards, groups.model.waiting)}
  {#each blocked as b (b.id)}
    <div class="callout danger blocked" role="status"><span class="ic"><I.CircleAlert size={16} /></span>
      <span class="small">{t(b.rejected ? 'conn.rejected_keys' : 'conn.missing_keys', { id: provName(b.id), roles: roleNames(b.roles) })}</span>
    </div>
  {/each}
  <h3 class="sub-title">{t('conn.models')}</h3>
  <Card padded={false}>
    <div use:scrollable class="table-scroll"><table class="table roles">
      <thead><tr><th>{t('conn.role')}</th><th>{t('conn.model')}</th><th>{t('conn.price')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
      <tbody>
        {#each roles as role}
          {@const st = roleState(role)}
          {@const m = cfg.doc?.models?.[role]}
          <tr>
            <td class="c-role"><strong>{t(`role.${role}`)}</strong><div class="xs muted">{t(`role.${role}_hint`)}</div></td>
            <td class="c-model"><span class="mono small break">{m?.model ?? '–'}</span>
              {#if m?.provider}<div class="xs muted">{provName(m.provider)}{#if st === 'no_key' || st === 'rejected'} · <span class="warn-text">{st === 'rejected' ? t('conn.badge_rejected') : t('tools.state.no_key')}</span>{/if}</div>{/if}
              {#if m?.provider && resting[`${m.provider}/${m.model}`]}<div class="xs warn-text">{t('conn.fb_resting', { when: relative(resting[`${m.provider}/${m.model}`].until) })}</div>{/if}
              {#if effortRole(role) && m?.model}<div class="xs muted">{t(reasonKey(reasoningOf(m.provider, m.model)))} · {t('conn.effort_now', { level: m.effort ? t(`conn.effort.${m.effort}`) : t('conn.effort_default') })}</div>{/if}
              {#if fallbackRoles.includes(role) && m?.fallbacks?.length}
                <ol class="fb-list xs">
                  {#each m.fallbacks as f, i (i)}
                    <li><span class="muted">{t('conn.fb_line', { n: i + 1 })}</span> <span class="mono break">{f.model}</span> <span class="muted">· {provName(f.provider)} · {t(reasonKey(reasoningOf(f.provider, f.model)))}{#if f.effort} · {t('conn.effort_now', { level: t(`conn.effort.${f.effort}`) })}{/if}</span>{#if resting[`${f.provider}/${f.model}`]} <span class="warn-text">· {t('conn.fb_resting', { when: relative(resting[`${f.provider}/${f.model}`].until) })}</span>{/if}</li>
                  {/each}
                </ol>
              {/if}
              {#if role === 'rerank' && modelCaps[m?.provider ?? ''] && !modelCaps[m.provider].rerank}<div class="xs warn-text">{t('conn.rerank_unsupported')}</div>{/if}
              {#if role === 'embed' && embedShowsWidth && ew}
                <div class="width-line"><span class="xs muted">{widthText(ew)}</span>{#if widthOptions.length}<Button size="sm" variant="ghost" icon={I.SlidersHorizontal} data-change-width="embed" disabled={!can('models.embed_width')} onclick={openWidth}>{t('conn.width_change')}</Button>{/if}</div>
                {#if !ew.truncatable}<div class="xs muted">{wstate === 'custom' ? t('conn.width_custom_note') : t('conn.width_unsupported')}</div>{/if}
                {#if widthNoRerank}<div class="xs muted">{t('conn.width_norerank')}</div>{/if}
              {/if}</td>
            <td class="c-price small"><button type="button" class="price-edit" title={t('conn.set_price')} onclick={() => openPrice(role)}><span class="num" class:muted={(prices[role]?.source ?? 'unknown') === 'unknown'}>{priceText(role)}</span><I.Pencil size={13} aria-hidden="true" /><span class="sr-only">{t('conn.set_price')}</span></button>{#if prices[role]?.cached_per_mtok}<div class="xs muted">{t('conn.price_cached_short', { usd: usd(prices[role].cached_per_mtok) })}</div>{/if}{#if prices[role]?.source && prices[role].source !== 'unknown'}<div class="xs muted">{priceSource(role)}</div>{/if}{#if priceStale(role)}<div class="xs warn-text">{t('conn.price_stale')}</div>{/if}</td>
            <td class="c-acts r nowrap"><Button size="sm" variant="ghost" icon={I.Search} data-change-model={role} onclick={() => browse(role)}>{t('conn.change_model')}</Button>{#if fallbackRoles.includes(role) && m?.model}<Button size="sm" variant="ghost" icon={I.Layers} disabled={!can('config.patch')} onclick={() => openFallbacks(role)}>{m?.fallbacks?.length ? t('conn.fb_button_n', { n: m.fallbacks.length }) : t('conn.fb_button')}</Button>{/if}{#if optionalRoles.includes(role) && m?.model}<Button size="sm" variant="ghost" icon={I.X} disabled={!can('models.select')} onclick={() => ((clearFor = role), (clearOpen = true))}>{t('conn.clear_role')}</Button>{/if}</td>
          </tr>
        {/each}
      </tbody>
    </table></div>
    {#if roles.some((r) => (prices[r]?.source ?? 'unknown') === 'unknown')}<p class="xs muted pnote">{t('conn.price_src.unknown')}</p>{/if}
  </Card>
</section>

<div class="block" id="discord">
  <Card title={t('conn.discord')} description={t('conn.discord_desc')}>
    {#snippet actions()}<Status tone={tiles[1].tone} label={tiles[1].text} />{/snippet}
    {#if cfg.loaded}
      <div class="dgrid">
        <div class="stack">
          <h3 class="sub-title">{t('conn.discord_conn')}</h3>
          <Switch checked={!!cfg.doc.discord?.enabled} onchange={(v) => setDiscord('enabled', v)} label={t('conn.discord_enabled')} hint={t('conn.restart_needed')} />
          <div class="row"><span class="small grow">{t('conn.token')}: <span class="muted">{refKind(cfg.doc.discord?.token_ref)}</span></span><Button size="sm" icon={I.KeyRound} onclick={() => openKey('discord')}>{t('conn.set_token')}</Button></div>
          <div class="bot">
            <div class="row"><span class="small grow">{bot ? t('conn.bot_found', { name: bot.bot_name, id: bot.bot_id }) : botBusy ? t('conn.bot_checking') : t('conn.bot_none')}</span><Button size="sm" variant="ghost" icon={I.RefreshCw} loading={botBusy} disabled={!channels.data?.discord_token} onclick={checkBot}>{t('conn.bot_check')}</Button></div>
            {#if botErr}<p class="xs err" role="alert">{botErr}</p>{/if}
            {#if bot}
              {#if bot.message_content_blocked}<div class="callout danger tight"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{t('conn.intent_blocked')} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a></span></div>
              {:else}<p class="xs muted">{bot.message_content_portal ? t('conn.intent_on') : t('conn.intent_off')}{#if !bot.message_content_portal} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a>{/if}</p>{/if}
            {/if}
          </div>
          {#if gw}
            <div class="gw">
              <div class="row"><span class="small grow">{t('conn.gw.title')}</span><Status tone={gwTone} size="sm" label={t(`conn.gw.${gw.state}`)} /><Button size="sm" variant="ghost" icon={I.RefreshCw} loading={channels.loading} onclick={() => void channels.refresh()}>{t('conn.gw.refresh')}</Button></div>
              {#if gw.state === 'check'}<div class="callout warn tight" role="status"><span class="ic"><I.TriangleAlert size={14} /></span><span class="xs">{t('conn.gw.check_hint')}</span></div>
              {:else if gw.state === 'unknown'}<p class="xs muted">{t('conn.gw.unknown_hint')}</p>{/if}
              <dl class="health xs">
                <dt>{t('conn.gw.latency')}</dt><dd class="num">{gw.latency_ms >= 0 ? ms(gw.latency_ms) : t('conn.gw.none')}</dd>
                <dt>{t('conn.gw.ack')}</dt><dd class="num" class:old={gw.state === 'check'}>{gwAge(gw.ack_age_sec)}</dd>
                <dt>{t('conn.gw.event')}</dt><dd class="num" title={t('conn.gw.event_hint')}>{gwAge(gw.event_age_sec)}</dd>
                <dt>{t('conn.gw.sessions')}</dt><dd class="num">{t('conn.gw.sessions_v', { ready: num(gw.ready_count), resumed: num(gw.resumed_count), down: num(gw.disconnect_count) })}</dd>
              </dl>
            </div>
          {/if}
          {#if invite}<div class="row"><a href={invite} target="_blank" rel="noopener" class="small break grow">{invite}</a><CopyButton text={invite} /></div>{:else}<div class="row"><Button size="sm" icon={I.Link} onclick={makeInvite}>{t('room.invite_make')}</Button></div>{/if}
        </div>
        <div class="stack">
          <h3 class="sub-title">{t('conn.discord_cmds')}</h3>
          <Field id="pfx" label={t('conn.prefix')} hint={t('conn.prefix_hint')}>{#snippet children({ id, describedby })}<div class="narrow"><Input {id} aria-describedby={describedby} value={cfg.doc.discord?.command_prefix ?? '!'} maxlength={3} onchange={(e) => setDiscord('command_prefix', (e.currentTarget as HTMLInputElement).value)} /></div>{/snippet}</Field>
          <Switch checked={!!cfg.doc.discord?.register_commands} onchange={(v) => setDiscord('register_commands', v)} label={t('conn.slash')} hint={t('conn.slash_hint')} />
          <Switch checked={!!cfg.doc.discord?.message_content} onchange={(v) => setDiscord('message_content', v)} label={t('conn.content_intent')} hint={t('conn.content_intent_hint')} />
        </div>
      </div>
    {/if}
  </Card>
</div>

{#if serviceGroups.length}
  <section class="block" id="services" aria-labelledby="services-h">
    <div class="sec-head"><h2 id="services-h" class="sec-title">{t('conn.services')}</h2></div>
    {#each serviceGroups as [g, { cards: list, waiting }] (g)}
      <h3 class="group-title">{t(`conn.group.${g}`)}<span class="xs muted"> · {t(`conn.group.${g}_hint`)}</span></h3>
      {@render providerGroup(g, list, waiting)}
    {/each}
  </section>
{/if}
{/if}

<Dialog open={!!priceFor} onclose={() => (priceFor = '')} title={t('conn.price_title', { role: priceFor ? t(`role.${priceFor}`) : '' })} description={t('conn.price_desc')} size="sm">
  <div class="stack">
    <Field id="pin" label={t('conn.price_in')}>{#snippet children({ id })}<Input {id} bind:value={priceIn} inputmode="decimal" placeholder="0.14" />{/snippet}</Field>
    <Field id="pout" label={t('conn.price_out')}>{#snippet children({ id })}<Input {id} bind:value={priceOut} inputmode="decimal" placeholder="0.42" />{/snippet}</Field>
    <Field id="pcached" label={t('conn.price_cached')} hint={t('conn.price_cached_hint')} error={priceErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} {invalid} aria-describedby={describedby} bind:value={priceCached} inputmode="decimal" placeholder={prices[priceFor]?.cached_per_mtok ? String(prices[priceFor].cached_per_mtok) : '0.014'} />{/snippet}</Field>
    <p class="xs muted">{t('conn.price_note')}</p>
    {#if timeoutRole(priceFor)}
      <Field id="ptimeout" label={t('conn.timeout')} hint={t('conn.timeout_hint')} error={timeoutErr || undefined}>{#snippet children({ id, invalid })}<Input {id} {invalid} bind:value={timeoutSecs} inputmode="numeric" placeholder={priceFor === 'planner' || priceFor === 'vision' ? '30' : '90'} />{/snippet}</Field>
    {/if}
    {#if effortRole(priceFor)}
      {@const pm = cfg.doc?.models?.[priceFor]}
      {@const pr = reasoningOf(pm?.provider ?? '', pm?.model ?? '')}
      <Field id="peffort" label={t('conn.effort')} hint={`${t(effortHintKey(priceFor))} ${t(`${reasonKey(pr)}_hint`)}`}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={effort} options={effortOptions(pr, effort)} />{/snippet}</Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (priceFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" onclick={savePrice}>{t('common.save')}</Button>
  {/snippet}
</Dialog>

<Dialog open={!!fbFor} onclose={() => (fbFor = '')} title={t('conn.fb_title', { role: fbFor ? t(`role.${fbFor}`) : '' })} description={t('conn.fb_desc')} size="md">
  <div class="stack">
    <p class="small">{t('conn.fb_primary')} <span class="mono">{cfg.doc?.models?.[fbFor]?.model ?? '–'}</span> <span class="muted">· {provName(cfg.doc?.models?.[fbFor]?.provider ?? '')}</span></p>
    {#if !fbRows.length}<p class="small muted">{t('conn.fb_none')}</p>{/if}
    <ol class="fb-rows">
      {#each fbRows as r, i (i)}
        <li class="fb-row">
          <div class="fb-head"><strong class="small">{t('conn.fb_line', { n: i + 1 })}</strong><span class="spacer"></span>
            <IconButton icon={I.ArrowUp} size={16} label={t('conn.fb_up')} disabled={i === 0} onclick={() => moveFallback(i, -1)} />
            <IconButton icon={I.ArrowDown} size={16} label={t('conn.fb_down')} disabled={i === fbRows.length - 1} onclick={() => moveFallback(i, 1)} />
            <IconButton icon={I.Trash2} size={16} tone="danger" label={t('conn.fb_remove')} onclick={() => (fbRows = fbRows.filter((_, j) => j !== i))} />
          </div>
          <div class="fb-grid">
            <Field id={`fbp${i}`} label={t('conn.fb_provider')}>{#snippet children({ id })}<Select {id} bind:value={r.provider} onchange={() => loadFbModels(r.provider)} options={modelProviderIds.map((p) => ({ value: p, label: provName(p) }))} />{/snippet}</Field>
            <Field id={`fbm${i}`} label={t('conn.fb_model')} hint={t('conn.fb_model_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={r.model} mono list={`fbl${i}`} autocomplete="off" />{/snippet}</Field>
            <datalist id={`fbl${i}`}>{#each fbModels[r.provider] ?? [] as fm (fm.id)}<option value={fm.id}></option>{/each}</datalist>
            <Field id={`fbi${i}`} label={t('conn.price_in')}>{#snippet children({ id })}<Input {id} bind:value={r.price_in} inputmode="decimal" placeholder={t('conn.fb_price_auto')} />{/snippet}</Field>
            <Field id={`fbo${i}`} label={t('conn.price_out')}>{#snippet children({ id })}<Input {id} bind:value={r.price_out} inputmode="decimal" placeholder={t('conn.fb_price_auto')} />{/snippet}</Field>
            <Field id={`fbt${i}`} label={t('conn.timeout')}>{#snippet children({ id })}<Input {id} bind:value={r.timeout} inputmode="numeric" placeholder={fbFor === 'response' ? '90' : fbFor === 'summary' ? '60' : '30'} />{/snippet}</Field>
            <Field id={`fbe${i}`} label={t('conn.effort')} hint={t(reasonKey(fbReasoning(r)))}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={r.effort} options={[{ value: '', label: t('conn.fb_effort_role') }, ...effortOptions(fbReasoning(r), r.effort).slice(1)]} />{/snippet}</Field>
          </div>
        </li>
      {/each}
    </ol>
    {#if fbRows.length < maxFallbacks}<div><Button size="sm" variant="secondary" icon={I.Plus} disabled={!modelProviderIds.length} onclick={addFallback}>{t('conn.fb_add')}</Button></div>{:else}<p class="xs muted">{t('conn.fb_max', { n: maxFallbacks })}</p>{/if}
    <p class="xs muted">{t('conn.fb_price_note')}</p>
    {#if fbErr}<div class="callout danger tight" role="alert"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{fbErr}</span></div>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (fbFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={fbBusy} onclick={saveFallbacks}>{t('common.save')}</Button>
  {/snippet}
</Dialog>

{#if limitP}<LimitDialog bind:open={limitOpen} id={limitP.id} name={provName(limitP.id)} limit={limitP.daily_limit ?? 0} used={limitP.used_today ?? 0} model={!!modelCaps[limitP.id]} onsaved={() => void providers.refresh()} />{/if}

<Dialog bind:open={keyOpen} title={keyFor === 'discord' ? t('conn.set_token') : t('conn.key_for', { id: provName(keyFor) })} description={providers.data?.keychain ? t('conn.key_desc_keychain') : tp('conn.key_desc')} size="sm">
  <div class="stack">
    {#if keyHost?.key_url}<p class="small">{t('conn.key_where', { name: keyHost.name })} <a href={keyHost.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a></p>{/if}
    <Segmented label={t('conn.key_mode')} bind:value={keyMode} options={[{ value: 'value', label: t('conn.key_mode.value') }, { value: 'ref', label: t('conn.key_mode.ref') }]} />
    {#if keyMode === 'value'}
      <Field id="kv" label={t('conn.key')} hint={t('conn.key_hint')} error={keyErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} type="password" aria-describedby={describedby} {invalid} bind:value={keyValue} autocomplete="off" />{/snippet}</Field>
    {:else}
      <Field id="kr" label={t('tool.credential_ref')} hint={platform() === 'windows' ? `${t('toolnew.cred_hint')} ${t('conn.env_note.windows')}` : t('toolnew.cred_hint')} error={keyErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={keyRef} mono />{/snippet}</Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (keyOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={keyBusy} icon={I.KeyRound} onclick={saveKey}>{t('conn.save_key')}</Button>
  {/snippet}
</Dialog>

<Dialog open={!!modelsFor} onclose={() => (modelsFor = '')} title={t('conn.pick_model', { role: t(`role.${modelsFor}`) })} description={t('conn.pick_model_desc')} size="lg">
  <div class="stack">
    {#if Object.keys(modelCaps).length > 1}
      <Field id="mprov" label={t('conn.model_provider')} hint={t('conn.model_provider_hint')}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={modelProv} onchange={(e) => browse(modelsFor, (e.currentTarget as HTMLSelectElement).value)} options={Object.keys(modelCaps).map((pid) => ({ value: pid, label: provName(pid) }))} />{/snippet}</Field>
    {:else if modelProv}<p class="small muted">{t('conn.model_provider_one', { name: provName(modelProv) })}</p>{/if}
    {#if imageUnknown}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.vision_unknown_note')}</span></div>{/if}
    {#if chatRole(modelsFor) && modelList.some((m) => m.supports_tools === false)}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.no_tools_note')}</span></div>{/if}
    {#if modelsFor === 'embed'}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.embed_change_note')}</span></div>{/if}
    {#if modelsFor === 'rerank' && modelCaps[modelProv] && !modelCaps[modelProv].rerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.rerank_unsupported')}</span></div>{/if}
    <div class="browse-bar">
      <div class="grow"><Input type="search" bind:value={modelFilter} placeholder={t('conn.model_search')} aria-label={t('conn.model_search')} /></div>
      <Select aria-label={t('conn.sort')} bind:value={modelSort} options={[{ value: 'list', label: t('conn.sort_list') }, { value: 'price_asc', label: t('conn.sort_price_asc') }, { value: 'price_desc', label: t('conn.sort_price_desc') }, { value: 'ctx', label: t('conn.sort_ctx') }]} />
    </div>
    {#if noList}
      <div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.no_list_note')}</span></div>
      <div class="browse-bar">
        <div class="grow"><Field id="typedm" label={t('conn.typed_model')} hint={t('conn.fb_model_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={typedModel} mono autocomplete="off" />{/snippet}</Field></div>
        <Button variant="primary" disabled={!typedModel.trim()} onclick={() => pickModel(modelsFor, typedModel.trim())}>{t('conn.use')}</Button>
      </div>
    {:else if loadingModels}<p class="small muted">{t('common.loading')}</p>{:else if !filteredModels.length}<p class="small muted">{t('conn.no_models')}</p>{:else}
      <div use:scrollable class="table-scroll models">
        <table class="table">
          <thead><tr><th>{t('conn.model')}</th><th class="r">{t('conn.ctx')}</th><th class="r">{t('conn.col_in')}</th><th class="r">{t('conn.col_out')}</th><th class="r">{t('conn.col_cached')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {#each filteredModels as m (m.id)}
              <tr><td><div class="small">{m.name}</div><div class="xs mono subtle">{m.id}</div>{@render capTags(m, chatRole(modelsFor), modelsFor === 'vision')}</td>
                <td class="r num xs">{m.context_length ? num(m.context_length) : '–'}</td>
                <td class="r num xs" class:muted={m.in_per_mtok === undefined}>{perM(m.in_per_mtok)}</td>
                <td class="r num xs">{m.in_per_mtok === undefined ? '–' : perM(m.out_per_mtok)}</td>
                <td class="r num xs">{m.cached_per_mtok === undefined ? '–' : perM(m.cached_per_mtok)}</td>
                <td class="r"><Button size="sm" variant={isCurrent(m) ? 'secondary' : 'ghost'} disabled={isCurrent(m)} onclick={() => pickModel(modelsFor, m.id)}>{isCurrent(m) ? t('conn.current') : t('conn.use')}</Button></td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</Dialog>

<Dialog bind:open={widthOpen} title={t('conn.width_title')} description={t('conn.width_desc')} size="sm">
  <div class="stack">
    <div class="widths" role="radiogroup" aria-label={t('conn.width_title')}>
      {#each pickOptions as c (c.value)}
        <label class="host" class:on={widthPick === c.value}>
          <input type="radio" name="embed-width" value={c.value} bind:group={widthPick} />
          <span><strong class="small num">{c.width}</strong>{#if widthTags(c)}<span class="xs muted wtags">· {widthTags(c)}</span>{/if}<br />
            <span class="xs muted">{c.full ? t('conn.width_opt_full') : c.recommended ? t('conn.width_opt_recommended') : t('conn.width_opt_middle')}</span></span>
        </label>
      {/each}
    </div>
    {#if !wi}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.width_read_failed_body')}</span><Button size="sm" variant="ghost" onclick={() => void loadWidth(false)}>{t('conn.width_reread')}</Button></div>{/if}
    {#if pickState === 'over'}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.width_over_note', { full: wi?.full_width ?? 0 })}</span></div>{/if}
    {#if pickNoRerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.width_norerank')}</span></div>{/if}
    {#if widthDir === 'raise'}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.width_raise_note')}</span></div>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (widthOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Check} loading={widthBusy} disabled={widthDir === 'same' || !widthBase || widthLoading} onclick={applyWidth}>{t('conn.width_apply')}</Button>
  {/snippet}
</Dialog>

<Dialog bind:open={addOpen} title={t('conn.add_provider')} description={t('conn.add_desc')} size="lg">
  <div class="stack">
    {#each [['model', providers.data?.hosts ?? []], ['web', providers.data?.web_hosts ?? []]] as [g, hosts] (g)}
      <div class="section-title">{t(`conn.group.${g}`)}</div>
      <div class="hosts">
        {#each hosts as h ((h.api ?? h.kind) + h.id)}
          {@const sel = g === 'model' ? `host:${h.id}` : `kind:${h.kind}`}
          <label class="host" class:on={addSel === sel} class:dim={present(sel)}>
            <input type="radio" name="host" value={sel} bind:group={addSel} disabled={present(sel)} />
            <span><strong class="small">{h.name}</strong>{#if present(sel)}<span class="xs muted"> · {t('conn.added_already')}</span>{/if}<br /><span class="xs muted">{g === 'model' ? t(has(`conn.host_note.${h.id}`) ? `conn.host_note.${h.id}` : `conn.host_note.${h.api}`) : t(`tool.prov_note.${h.kind}`)}</span></span>
          </label>
        {/each}
        {#if g === 'model'}
          <label class="host" class:on={addSel === 'custom'}>
            <input type="radio" name="host" value="custom" bind:group={addSel} />
            <span><strong class="small">{t('conn.custom')}</strong><br /><span class="xs muted">{t('conn.host_note.compatible')}</span></span>
          </label>
        {/if}
      </div>
    {/each}
    {#if addSel === 'custom'}
      <Segmented label={t('conn.custom_api')} bind:value={addAPI} options={[{ value: 'compatible', label: t('conn.api.compatible') }, { value: 'anthropic', label: t('conn.api.anthropic') }]} />
      <p class="xs muted">{t(`conn.custom_api_hint.${addAPI}`)}</p>
      <div class="form-grid">
        <Field id="aid" label={t('conn.custom_id')} hint={t('conn.custom_id_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={addId} placeholder="deepseek" mono />{/snippet}</Field>
        <Field id="aurl" label={t('conn.custom_url')} hint={t(addAPI === 'anthropic' ? 'conn.custom_url_hint_anthropic' : 'conn.custom_url_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={addURL} placeholder={addAPI === 'anthropic' ? 'https://api.example.com/anthropic' : 'https://api.deepseek.com/v1'} mono type="url" />{/snippet}</Field>
      </div>
    {/if}
    {#if addErr}<p class="small err" role="alert">{addErr}</p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (addOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Plus} loading={addBusy} disabled={!addSel} onclick={addProvider}>{t('conn.add')}</Button>
  {/snippet}
</Dialog>

<Dialog open={!!recFor} onclose={() => (recFor = '')} title={t('conn.rec_title', { id: provName(recFor) })} description={t('conn.rec_desc')} size="lg">
  {#if recBusy && !rec}<p class="small muted">{t('common.loading')}</p>
  {:else if rec}
    <div class="stack">
      {#if !rec.picks?.length}<p class="small muted">{t('conn.rec_none')}</p>{:else}
        <div use:scrollable class="table-scroll"><table class="table picks">
          <thead><tr><th><span class="sr-only">{t('conn.rec_use')}</span></th><th>{t('conn.role')}</th><th>{t('conn.rec_now')}</th><th>{t('conn.rec_new')}</th><th class="r">{t('conn.price')}</th></tr></thead>
          <tbody>
            {#each rec.picks as r (r.role)}
              <tr class:same={!r.changed || !recOn[r.role]}>
                <td>{#if r.changed}<input type="checkbox" bind:checked={recOn[r.role]} aria-label={t('conn.rec_use_role', { role: t(`role.${r.role}`) })} />{/if}</td>
                <td><strong class="small">{t(`role.${r.role}`)}</strong>{#if r.replaced}<div class="xs muted">{t('conn.rec_replaced')}</div>{:else if !r.changed}<div class="xs muted">{t('conn.rec_same')}</div>{/if}</td><td class="xs mono muted break">{r.current_model || '–'}<div>{provName(r.current_provider ?? '')}</div></td><td class="small mono break">{r.model}{#if r.role === 'embed' && recW}<div class="xs rec-width"><strong>{recWidthText(recW)}</strong>{#if has(`conn.rec_width_why.${recW.reason}`)}<div class="muted">{t(`conn.rec_width_why.${recW.reason}`)}</div>{/if}</div>{/if}</td>
                <td class="r xs num">{r.in_per_mtok !== undefined ? `${usd(r.in_per_mtok)}${r.out_per_mtok ? ' / ' + usd(r.out_per_mtok) : ''}` : t('conn.price_unknown')}</td></tr>
            {/each}
          </tbody>
        </table></div>
      {/if}
      <h3 class="sub-title">{t('conn.rec_budget_title')}</h3>
      {#if !budgetRoles.length}<p class="small muted">{t('conn.rec_budget_none')}</p>{:else}
        <p class="xs muted">{t('conn.rec_budget_desc')}</p>
        <div use:scrollable class="table-scroll"><table class="table budget">
          <thead><tr><th>{t('conn.role')}</th><th>{t('conn.model')}</th><th class="r">{t('conn.ctx')}</th><th class="r">{t('conn.col_in')}</th><th class="r">{t('conn.col_out')}</th><th class="r">{t('conn.rec_blended')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {#each budgetRoles as role (role)}
              {#each rec.budget[role] as b, i (b.model)}
                <tr class:first={i === 0}>
                  <td>{#if i === 0}<strong class="small">{t(`role.${role}`)}</strong>{/if}</td>
                  <td><div class="small">{b.name || b.model}</div><div class="xs mono subtle break">{b.model}</div>{@render capTags(b, role === 'response' || role === 'planner')}</td>
                  <td class="r num xs">{b.context_length ? num(b.context_length) : '–'}</td>
                  <td class="r num xs">{usd(b.in_per_mtok)}</td><td class="r num xs">{usd(b.out_per_mtok)}</td><td class="r num xs">{usd(b.blended_per_mtok)}</td>
                  <td class="r"><Button size="sm" variant={b.current ? 'secondary' : 'ghost'} disabled={b.current || (role === 'embed' && !!rec.embed_indexed)} loading={budgetBusy === `${role}|${b.model}`} aria-label={`${t('conn.rec_use_this')}: ${t(`role.${role}`)} · ${b.name || b.model}`} onclick={() => useBudget(role, b.model)}>{b.current ? t('conn.current') : t('conn.rec_use_this')}</Button></td>
                </tr>
              {/each}
            {/each}
          </tbody>
        </table></div>
        {#if rec.embed_indexed && rec.budget?.embed}<p class="xs muted">{t('conn.rec_budget_embed_locked')}</p>{/if}
      {/if}
      {#if !rec.rerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.rec_no_rerank')}</span></div>{/if}
      {#if rec.reindex}
        <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span>
          <div class="stack tight">
            <span class="small">{recEmbed ? t('conn.rec_embed_on', { leaves: num(rec.reindex.leaves), rooms: num(rec.reindex.rooms), tokens: num(rec.reindex.tokens), usd: rec.reindex.usd !== undefined ? usd(rec.reindex.usd) : t('conn.price_unknown') }) : t('conn.rec_embed_off', { leaves: num(rec.reindex.leaves) })}</span>
            {#if recEmbed}<Switch bind:checked={recReindex} label={t('conn.rec_reindex_now')} />{/if}
          </div>
        </div>
      {:else if rec.embed_changes && recOn.embed}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.embed_change_note')}</span></div>{/if}
    </div>
  {/if}
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (recFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Check} loading={(recBusy || recRefreshing) && !!rec} disabled={recRefreshing || !rec?.picks?.some((r: any) => r.changed && recOn[r.role])} onclick={() => applyRecommend()}>{t('conn.rec_apply')}</Button>
  {/snippet}
</Dialog>

<Confirm bind:open={embedOpen} title={t('conn.embed_switch_title')} body={embedBody(embedAsk)} confirmLabel={t('conn.embed_switch_confirm')} onconfirm={confirmEmbedSwitch} />

<Confirm bind:open={widthLowerOpen} title={t('conn.width_lower_title', { n: widthChoice?.width ?? 0 })} body={t('conn.width_lower_body')} confirmLabel={t('conn.width_lower_confirm')} onconfirm={confirmWidthLower} />

<Confirm bind:open={widthRaiseOpen} title={t('conn.width_raise_title', { n: pickOptions.find((c) => c.value === widthAsk?.value)?.width ?? 0 })} body={widthRaiseBody(widthAsk)} confirmLabel={t('conn.width_raise_confirm')} onconfirm={confirmWidthRaise} />

<Confirm bind:open={clearOpen} title={t('conn.clear_title', { role: clearFor ? t(`role.${clearFor}`) : '' })} body={clearFor ? t(`conn.clear_body.${clearFor}`) : ''} confirmLabel={t('conn.clear_role')} onconfirm={clearRole} />

<Confirm bind:open={removeOpen} title={t('conn.remove', { id: provName(removeFor) })} body={usedByRole(removeFor).length ? t('conn.remove_in_use', { roles: usedByRole(removeFor).map((r) => t(`role.${r}`)).join(', ') }) : t('conn.remove_body')} confirmLabel={t('conn.remove_confirm')} danger onconfirm={removeProvider} />

<style>
  .sec-head {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 4px var(--s-3);
    margin-bottom: var(--s-3);
  }
  .sec-title {
    font-size: var(--text-lg);
    font-weight: 650;
  }
  .sub-title {
    margin: var(--s-5) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .stack > .sub-title {
    margin: 0;
  }
  /* summary row: what works, what is missing, and the one button that fixes it */
  .overview {
    list-style: none;
    margin: 0 0 var(--s-6);
    padding: 0;
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr));
  }
  .tile {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2);
    padding: var(--s-2) var(--s-3) var(--s-2) var(--s-2);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .tile.danger {
    box-shadow: var(--shadow-border), inset 3px 0 0 var(--danger);
  }
  .tile.warn {
    box-shadow: var(--shadow-border), inset 3px 0 0 var(--warn);
  }
  .tile-go {
    flex: 1 1 180px;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: var(--s-3);
    padding: var(--s-2);
    border: 0;
    border-radius: var(--r-lg);
    background: none;
    color: inherit;
    font: inherit;
    text-align: start;
    cursor: pointer;
  }
  @media (hover: hover) {
    .tile-go:hover {
      background: var(--surface-2);
    }
  }
  .tile-go:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .ti {
    flex: none;
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: var(--surface-2);
    color: var(--text-muted);
  }
  .tile.ok .ti {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tile.warn .ti {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .tile.danger .ti {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .tt {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 4px;
    min-width: 0;
  }
  .tl {
    font-weight: 650;
  }
  .blocked {
    margin-top: var(--s-3);
  }
  .dgrid {
    display: grid;
    gap: var(--s-5) var(--s-8);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 340px), 1fr));
    align-items: start;
  }
  .price-edit {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin: 0 -6px;
    padding: 4px 6px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
  .price-edit :global(svg) {
    color: var(--text-subtle);
  }
  @media (hover: hover) {
    .price-edit:hover {
      background: var(--surface-2);
    }
    .price-edit:hover :global(svg) {
      color: var(--text);
    }
  }
  .price-edit:focus-visible {
    outline: 2px solid var(--accent);
  }
  .narrow {
    max-width: 8rem;
  }
  .group-title {
    margin: var(--s-4) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .hosts {
    display: grid;
    gap: var(--s-2);
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 240px), 1fr));
  }
  .host {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    padding: 10px 12px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    cursor: pointer;
  }
  .host.on {
    box-shadow: 0 0 0 2px var(--accent);
  }
  .host.dim {
    opacity: 0.6;
    cursor: default;
  }
  .host input {
    margin-top: 3px;
    accent-color: var(--accent);
  }
  .warn-text {
    color: var(--warn);
  }
  .err {
    color: var(--danger);
  }
  tr.same td {
    color: var(--text-muted);
  }
  .pnote {
    padding: var(--s-3) var(--pad-card);
    margin: 0;
  }
  .gw {
    display: grid;
    gap: var(--s-2);
  }
  .health {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 2px 10px;
    margin: 0;
    color: var(--text-muted);
  }
  .health dd {
    margin: 0;
    color: var(--text);
  }
  .health dd.old {
    color: var(--warn);
    font-weight: 600;
  }
  .tight {
    padding: 6px 10px;
  }
  .block {
    margin-bottom: var(--s-8);
    scroll-margin-top: calc(var(--topbar-offset, 0px) + var(--s-4));
  }
  .prov {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding: var(--pad-card);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .prov header {
    display: flex;
    align-items: center;
    gap: var(--s-3);
  }
  .pi {
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: var(--info-soft);
    color: var(--info);
  }
  .pi.model {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .pn {
    min-width: 0;
  }
  .pn h3 {
    font-size: var(--text-md);
    font-weight: 650;
    overflow-wrap: anywhere;
  }
  .acts {
    margin-top: auto;
    flex-wrap: wrap;
  }
  .quota {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--s-2);
  }
  .quota-edit {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 2px 6px;
    border: 0;
    border-radius: var(--r-sm, 6px);
    background: none;
    color: var(--accent-text, var(--accent));
    font: inherit;
    cursor: pointer;
  }
  .quota-edit:hover:not(:disabled) {
    background: var(--surface-hover, var(--surface-2));
  }
  .quota-edit:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .fb-list {
    margin: 4px 0 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 2px;
  }
  .fb-rows {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: var(--s-3);
  }
  .fb-row {
    padding: var(--s-3);
    border-radius: var(--r);
    box-shadow: var(--shadow-border);
  }
  .fb-head {
    display: flex;
    align-items: center;
    gap: 2px;
    margin-bottom: var(--s-2);
  }
  .fb-grid {
    display: grid;
    gap: var(--s-2) var(--s-3);
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }
  @media (max-width: 640px) {
    .fb-grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  /* on a phone each role is a small card: name, then model, then price and the two buttons */
  @media (max-width: 640px) {
    .roles thead {
      display: none;
    }
    .roles tr {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 4px var(--s-3);
      padding: var(--s-3) var(--pad-card);
      box-shadow: inset 0 -1px 0 var(--border);
    }
    .roles tbody tr:last-child {
      box-shadow: none;
    }
    .roles td {
      display: block;
      height: auto;
      padding: 0;
      box-shadow: none;
    }
    .roles .c-role,
    .roles .c-model {
      grid-column: 1 / -1;
    }
    .roles .c-acts {
      align-self: center;
    }
  }
  /* providers three (or two) to a row, so six make two full rows */
  .providers {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
  }
  @media (min-width: 760px) {
    .providers {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (min-width: 1240px) {
    .providers {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
  }
  .waiting {
    margin-top: var(--s-3);
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .waiting ul {
    list-style: none;
    margin: var(--s-2) 0 0;
    padding: 0;
    display: grid;
    gap: var(--s-2);
  }
  .waiting li {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
    padding-top: var(--s-2);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .waiting li:first-child {
    box-shadow: none;
    padding-top: 0;
  }
  .grow {
    flex: 1 1 240px;
    min-width: 0;
  }
  .bot {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .models {
    max-height: 56vh;
  }
  .browse-bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2);
    align-items: center;
  }
  .browse-bar > :global(.wrap) {
    flex: 0 1 200px;
  }
  .tags {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 4px;
  }
  .tag {
    padding: 1px 7px;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-xs);
    white-space: nowrap;
  }
  .tag.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tag.dim {
    background: none;
    box-shadow: var(--shadow-border);
  }
  .tag.warn {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .width-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--s-2);
  }
  .width-line :global(.btn) {
    margin-inline-start: -6px;
  }
  .wtags {
    margin-inline-start: 0.4em;
  }
  .widths {
    display: grid;
    gap: var(--s-2);
  }
  .rec-width {
    margin-top: 4px;
    font-family: var(--font);
  }
  .budget tr.first td {
    box-shadow: inset 0 1px 0 var(--border);
  }
  /* model names break anywhere, so without a floor a phone squeezes them to a few letters per line: the table keeps
     a readable width and scrolls sideways inside its frame instead */
  .picks {
    min-width: 44rem;
  }
  /* the role name column: a long one (Japanese "プランナー") must not break inside the word */
  .picks td:nth-child(2) {
    min-width: 7rem;
  }
  .budget {
    min-width: 44rem;
  }
</style>
