<script lang="ts">
  import { untrack } from 'svelte'
  import { scrollable } from '../lib/scrollable'
  import { t, has, toolName, toolDesc } from '../lib/i18n.svelte'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig, patchConfig, type Op } from '../lib/config.svelte'
  import { navigate, route } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import { num, ms, shortDate, relative } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Status from '../components/Status.svelte'
  import Button from '../components/Button.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import SaveBar from '../components/SaveBar.svelte'
  import Chart from '../components/Chart.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import Confirm from '../components/Confirm.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Progress from '../components/Progress.svelte'
  import LimitDialog from '../components/LimitDialog.svelte'
  import Switch from '../components/Switch.svelte'
  import NoticeOps from '../components/NoticeOps.svelte'
  import FortuneOps from '../components/FortuneOps.svelte'
  import DisasterOps from '../components/DisasterOps.svelte'
  import ToolInputs from '../components/ToolInputs.svelte'
  import IconButton from '../components/IconButton.svelte'
  import { toValues, toObject, exampleInput, inputLabel, toolState, type ToolInput, type InputValues } from '../lib/toolmeta'
  import * as I from '../lib/icons'

  let { params }: { params: { id: string } } = $props()
  useConfig()
  const id = $derived(params.id)
  const tools = resource<any>('tools.list', () => ({}), { live: ['config', 'turn'] })
  const catalog = resource<any>('help.version')
  const series = resource<any>('stats.series', () => ({ days: 30 }), { live: ['turn'] })
  const providers = resource<any>('providers.list', () => ({}), { live: ['config'] })
  const tl = $derived(((tools.data?.tools ?? []) as any[]).find((x) => x.id === id))
  const settings = $derived(cfg.doc?.tools?.[id] ?? {})
  const isHTTP = $derived(tl?.kind === 'http' || !!settings.http)
  // the tool's inputs as the server describes them (built-ins: internal/tools; HTTP tools: their params)
  const inputs = $derived<ToolInput[]>(tl?.inputs ?? [])
  const provName = (pid: string) => (has(`conn.provider.${pid}`) ? t(`conn.provider.${pid}`) : pid)
  const providerOptions = $derived(((tl?.provider_order ?? tl?.providers ?? []) as string[]).map((pid) => ({ value: pid, label: provName(pid) })))
  // JSON compared by content, not by key order or spacing
  const canon = (v: unknown): string => JSON.stringify(v, (_k, x) => (x && typeof x === 'object' && !Array.isArray(x) ? Object.fromEntries(Object.entries(x).sort(([a], [b]) => a.localeCompare(b))) : x))
  const parseObj = (s: string): Record<string, unknown> | null => {
    try {
      const v = JSON.parse(s || '{}')
      return v && typeof v === 'object' && !Array.isArray(v) ? v : null
    } catch {
      return null
    }
  }
  let tab = $state(route.query.get('tab') ?? 'overview')

  // settings form (tri-state switches: default/on/off map to unset/true/false)
  type Tri = 'default' | 'on' | 'off'
  const tri = (v: unknown): Tri => (v === true ? 'on' : v === false ? 'off' : 'default')
  let form = $state({ enabled: 'default' as Tri, chat: 'default' as Tri, commands: 'default' as Tri, aliases: '', description: '', route: '', defaults: '{}', per_day: 0, per_user_per_day: 0, per_channel_per_day: 0, cooldown_sec: 0, url: '', credential_ref: '', pick: '', max_chars: 0, timeout_sec: 0 })
  let base = $state('')
  $effect(() => {
    if (!cfg.loaded) return
    const s = settings
    const f = {
      enabled: tri(s.enabled), chat: tri(s.chat), commands: tri(s.commands), aliases: (s.aliases ?? []).join(', '), description: s.description ?? '', route: s.route ?? '',
      defaults: JSON.stringify(s.defaults ?? {}, null, 2), per_day: s.limits?.per_day ?? 0, per_user_per_day: s.limits?.per_user_per_day ?? 0, per_channel_per_day: s.limits?.per_channel_per_day ?? 0,
      cooldown_sec: s.limits?.cooldown_sec ?? 0, url: s.http?.url ?? '', credential_ref: s.http?.credential_ref ?? '', pick: (s.http?.pick ?? []).join(', '), max_chars: s.http?.max_chars ?? 0, timeout_sec: s.http?.timeout_sec ?? 0,
    }
    const j = JSON.stringify(f)
    if (j !== base) {
      form = f
      base = j
    }
  })
  const changed = $derived(base ? Object.keys(form).filter((k) => (JSON.parse(base) as any)[k] !== (form as any)[k]) : [])
  let saving = $state(false)
  let defaultsErr = $state('')
  async function save() {
    defaultsErr = ''
    let defaults: any = {}
    try {
      defaults = JSON.parse(form.defaults || '{}')
      if (typeof defaults !== 'object' || Array.isArray(defaults)) throw new Error()
    } catch {
      defaultsErr = t('tool.defaults_invalid')
      tab = 'settings'
      return
    }
    const p = (k: string) => ['tools', id, k]
    const ops: Op[] = []
    for (const k of ['enabled', 'chat', 'commands'] as const) if (changed.includes(k)) ops.push(form[k] === 'default' ? { path: p(k), op: 'unset' } : { path: p(k), value: form[k] === 'on' })
    const list = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean)
    if (changed.includes('aliases')) ops.push(list(form.aliases).length ? { path: p('aliases'), value: list(form.aliases) } : { path: p('aliases'), op: 'unset' })
    for (const k of ['description', 'route'] as const) if (changed.includes(k)) ops.push(form[k].trim() ? { path: p(k), value: form[k].trim() } : { path: p(k), op: 'unset' })
    if (changed.includes('defaults')) ops.push(Object.keys(defaults).length ? { path: p('defaults'), value: defaults } : { path: p('defaults'), op: 'unset' })
    for (const k of ['per_day', 'per_user_per_day', 'per_channel_per_day', 'cooldown_sec'] as const)
      if (changed.includes(k)) ops.push(Number(form[k]) > 0 ? { path: [...p('limits'), k], value: Math.round(Number(form[k])) } : { path: [...p('limits'), k], op: 'unset' })
    if (isHTTP) {
      if (changed.includes('url')) ops.push({ path: [...p('http'), 'url'], value: form.url.trim() })
      if (changed.includes('credential_ref')) ops.push(form.credential_ref.trim() ? { path: [...p('http'), 'credential_ref'], value: form.credential_ref.trim() } : { path: [...p('http'), 'credential_ref'], op: 'unset' })
      if (changed.includes('pick')) ops.push(list(form.pick).length ? { path: [...p('http'), 'pick'], value: list(form.pick) } : { path: [...p('http'), 'pick'], op: 'unset' })
      for (const k of ['max_chars', 'timeout_sec'] as const) if (changed.includes(k)) ops.push(Number(form[k]) > 0 ? { path: [...p('http'), k], value: Math.round(Number(form[k])) } : { path: [...p('http'), k], op: 'unset' })
    }
    saving = true
    const r = await patchConfig(ops, { success: t('tools.saved', { tool: toolName(id) }) })
    saving = false
    if (r.ok) {
      base = ''
      void tools.refresh()
    }
  }

  // defaults: a form of the tool's inputs; the JSON behind it stays editable under "advanced" for keys
  // the form does not show
  let defVals = $state<InputValues>({})
  let defMode = $state<'form' | 'json'>('form')
  let defLoaded = ''
  $effect(() => {
    const b = base
    if (!b || b === defLoaded) return
    defLoaded = b
    untrack(() => (defVals = toValues(inputs, parseObj(form.defaults) ?? {})))
  })
  $effect(() => {
    const v = defVals
    if (defMode !== 'form') return
    untrack(() => {
      const cur = parseObj(form.defaults) ?? {}
      const next = toObject(inputs, v, cur).obj
      if (canon(next) !== canon(cur)) form.defaults = JSON.stringify(next, null, 2)
    })
  })
  function setDefMode(m: 'form' | 'json') {
    if (m === 'form') {
      const cur = parseObj(form.defaults)
      if (!cur) return void (defaultsErr = t('tool.defaults_invalid'))
      defVals = toValues(inputs, cur)
    }
    defaultsErr = ''
    defMode = m
  }

  // test: the same form of the tool's inputs; JSON stays available, with an example to start from
  let testInput = $state('{}')
  let testing = $state(false)
  let testRes = $state<any>(null)
  let testErr = $state('')
  let testMode = $state<'form' | 'json'>('form')
  let testVals = $state<InputValues>({})
  $effect(() => {
    if (tl && !inputs.length) testMode = 'json'
  })
  function insertExample() {
    testInput = JSON.stringify(exampleInput(id, inputs, tl?.sample), null, 2)
    testErr = ''
  }
  async function runTest() {
    testErr = ''
    let input = '{}'
    if (testMode === 'form' && inputs.length) {
      const { obj, missing } = toObject(inputs, testVals, {}, true)
      if (missing) return void (testErr = t('tool.test_missing', { name: inputLabel(id, missing) }))
      input = JSON.stringify(obj)
    } else {
      try {
        JSON.parse(testInput || '{}')
      } catch {
        testErr = t('tool.test_input_invalid')
        return
      }
      input = testInput || '{}'
    }
    testing = true
    const r = await call('tools.test', { id, input })
    testing = false
    testRes = r.ok ? r.output : { ok: false, error: r.error }
  }
  // one line a person reads first; the full response stays below
  const summary = $derived.by(() => {
    const o = testRes?.output
    if (!o || typeof o !== 'object') return ''
    if (id === 'weather.query' && o.status === 'ok') {
      const cond = o.condition ? t(`tool.cond.${o.condition}`) : ''
      const fc = o.forecast && (o.day_offset ?? 0) > 0 ? ` · ${o.forecast.date} ${o.forecast.max_c ?? '–'}°/${o.forecast.min_c ?? '–'}°` : ''
      return `${o.region} · ${o.temp_c ?? '–'}°C ${cond}${fc} · ${o.source === 'kma' ? t('tool.src.kma') : t('tool.src.open_meteo')}`
    }
    if (o.message) return String(o.message)
    return ''
  })

  // per-room
  const rooms = $derived.by(() => {
    const out: { g: string; c: string; v: Tri }[] = []
    for (const [g, gv] of Object.entries<any>(cfg.doc?.guilds ?? {})) for (const [c, cv] of Object.entries<any>(gv?.channels ?? {})) out.push({ g, c, v: tri(cv?.tools?.[id]) })
    return out
  })
  async function setRoom(g: string, c: string, v: Tri) {
    const path = ['guilds', g, 'channels', c, 'tools', id]
    await patchConfig(v === 'default' ? [{ path, op: 'unset' }] : [{ path, value: v === 'on' }], { success: t('rooms.tool_saved') })
  }

  // operations (tool-specific)
  const polls = resource<any>('polls.result', () => ({}), { live: ['config'] })
  let pollQuestion = $state('')
  let pollOptions = $state('')
  let pollScope = $state('default')
  let pollHours = $state('24')
  let pollMulti = $state(false)
  let pollBusy = $state(false)
  let pollParticipant = $state('console-test')
  let pollSelections = $state<Record<string, string[]>>({})
  let delOpen = $state(false)
  async function remove() {
    const r = await patchConfig([{ path: ['tools', id], op: 'unset' }], { success: t('tool.deleted') })
    if (r.ok) navigate('/tools')
  }
  async function createPoll() {
    pollBusy = true
    const r = await call('polls.create', { question: pollQuestion.trim(), options_text: pollOptions, scope: pollScope.trim() || 'default', duration_hours: Number(pollHours), allow_multiselect: pollMulti })
    pollBusy = false
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    pollQuestion = ''
    pollOptions = ''
    toast('ok', t('tool.poll_created'))
    void polls.refresh()
  }
  function togglePollOption(p: any, option: string, checked: boolean) {
    const current = pollSelections[p.id] ?? []
    pollSelections[p.id] = checked ? (p.max_select > 1 ? [...new Set([...current, option])] : [option]) : current.filter((x) => x !== option)
  }
  async function votePoll(p: any) {
    const r = await call('polls.vote', { id: p.id, scope: p.scope, base_revision: p.revision, participant: pollParticipant.trim(), option_ids: pollSelections[p.id] ?? [] })
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    toast('ok', t('tool.poll_voted'))
    void polls.refresh()
  }
  async function closePoll(p: any) {
    const r = await call('polls.close', { id: p.id, scope: p.scope, base_revision: p.revision })
    r.ok ? toast('ok', t('tool.poll_closed')) : toast('error', r.error?.message ?? '')
    void polls.refresh()
  }

  let limitP = $state<any>(null)
  let limitOpen = $state(false)
  const provs = $derived(((tl?.providers ?? []) as string[]).map((pid) => (providers.data?.providers ?? []).find((p: any) => p.id === pid)).filter(Boolean) as any[])
  // search.web: the order the search services are tried in (web_search.order; empty = built-in order)
  const searchOrder = $derived<string[]>(tl?.provider_order ?? [])
  const searchAll = $derived<string[]>([...searchOrder, ...((tl?.providers ?? []) as string[]).filter((p) => !searchOrder.includes(p))])
  async function moveSearch(pid: string, dir: -1 | 1) {
    const list = [...searchOrder]
    const i = list.indexOf(pid)
    const j = i + dir
    if (i < 0 || j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j], list[i]]
    const r = await patchConfig([{ path: ['web_search', 'order'], value: list }], { success: t('tool.search_order_saved') })
    if (r.ok) void tools.refresh()
  }
  async function resetSearchOrder() {
    const r = await patchConfig([{ path: ['web_search', 'order'], op: 'unset' }], { success: t('tool.search_order_saved') })
    if (r.ok) void tools.refresh()
  }
  const usageSeries = $derived(series.data?.tools?.[id] ?? [])
  const labels = $derived((series.data?.days ?? []).map(shortDate))
  // Deferred catalog entries do not get an operations surface or issue background Actions.
  const opsKind = $derived(id.startsWith('polls.') ? 'polls' : id.startsWith('notices.') || id === 'transfer.recent' ? 'notices' : id.startsWith('fortune.') ? 'fortune' : id === 'disasters.query' ? 'disasters' : '')
  const effectiveDesc = $derived(tl?.kind === 'http' ? tl?.description : toolDesc(id) || tl?.description)
</script>

<PageHeader title={toolName(id)} description={effectiveDesc} crumbs={[{ href: '/tools', label: t('nav.tools') }]}>
  {#snippet actions()}
    <Button icon={I.Play} disabled={!tl?.implemented} onclick={() => (tab = 'test')}>{t('tool.try')}</Button>
    {#if isHTTP}<Button variant="danger-ghost" icon={I.Trash2} onclick={() => (delOpen = true)}>{t('tool.delete')}</Button>{/if}
  {/snippet}
  <div class="row">
    <code class="xs subtle">{id}</code>
    {#if tl}
      <!-- the server's readiness, the same the tools list shows -->
      {@const st = toolState(tl)}
      <Status tone={st.tone} label={st.label} size="sm" />
      {#if isHTTP}<span class="chip accent">{t('tools.custom')}</span>{/if}
    {/if}
  </div>
</PageHeader>

{#if !tools.data}
  <LoadState res={tools}><Card><Skeleton lines={6} /></Card></LoadState>
{:else if !tl}
  <Card><EmptyState pose="lost" title={t('tool.not_found')}><Button href="/tools">{t('nav.tools')}</Button></EmptyState></Card>
{:else}
  <Tabs id="tool" bind:active={tab} tabs={[
    { id: 'overview', label: t('tool.tab.overview'), icon: I.Info },
    { id: 'settings', label: t('tool.tab.settings'), icon: I.SlidersHorizontal },
    { id: 'limits', label: t('tool.tab.limits'), icon: I.Gauge },
    { id: 'rooms', label: t('tool.tab.rooms'), icon: I.Hash, count: rooms.filter((r) => r.v !== 'default').length },
    { id: 'test', label: t('tool.tab.test'), icon: I.Play },
    ...(opsKind ? [{ id: 'ops', label: t('tool.tab.ops'), icon: I.Activity }] : []),
    { id: 'stats', label: t('tool.tab.stats'), icon: I.ChartColumn }]} />

  <div class="panel" role="tabpanel" id="tool-panel-{tab}" aria-labelledby="tool-tab-{tab}">
    {#if tab === 'overview'}
      <div class="cols">
        <Card title={t('tool.about')}>
          <KeyValue items={[
            { k: t('tool.kind'), v: isHTTP ? t('tools.custom') : t('tool.builtin') },
            { k: t('tools.sw.enabled'), v: tl.enabled ? t('common.on') : `${t('common.off')}${tl.reason ? ' (' + t(`tool.reason.${tl.reason}`) + ')' : ''}` },
            { k: t('tools.sw.chat'), v: tl.can_chat === false ? t('tools.na.label') : tl.chat ? t('common.on') : t('common.off') },
            { k: t('tools.sw.commands'), v: tl.can_command === false ? t('tools.na.label') : tl.commands ? t('common.on') : t('common.off') },
            { k: t('tool.aliases'), v: (tl.aliases ?? []).map((a: string) => '!' + a).join(' ') },
            { k: t('tools.today'), v: num(tl.used_today) },
            ...(tl.queue ? [{ k: t('tool.queue'), v: tl.queue.waiting || tl.queue.running ? [t('tool.queue_n', { n: tl.queue.waiting }), tl.queue.running ? t('tool.queue_running') : ''].filter(Boolean).join(' · ') : t('tool.queue_idle') }] : []),
          ]} />
        </Card>
        <Card title={t('tool.provider')}>
          {#if isHTTP}
            <KeyValue items={[{ k: 'URL', v: settings.http?.url, mono: true }, { k: t('tool.credential'), v: settings.http?.credential_ref ? t('tool.credential_set') : t('tool.credential_none') }]} />
            <p class="xs muted" style:margin-top="12px">{t('tool.http_safety')}</p>
          {:else if provs.length}
            <div class="stack" style:gap="14px">
              {#each provs as prov (prov.id)}
                <div class="stack" style:gap="8px">
                  <div class="row"><strong>{provName(prov.id)}</strong>
                    <Status tone={prov.status === 'disabled' ? 'neutral' : prov.configured ? 'ok' : 'warn'} label={prov.status === 'disabled' ? t('tools.state.off') : prov.configured ? (prov.builtin || !prov.key_present ? t('tool.keyless') : t('tools.state.ready')) : t('tools.state.no_key')} size="sm" /></div>
                  {#if has(`tool.prov_note.${prov.kind}`)}<p class="xs muted">{t(`tool.prov_note.${prov.kind}`)}</p>{/if}
                  {#if prov.daily_limit}<Progress value={prov.used_today} max={prov.daily_limit} label={t('tool.provider_quota')} />{/if}
                  {#if !prov.builtin}
                    <div class="row quota"><p class="xs muted num">{prov.daily_limit ? t('tool.provider_quota_n', { used: num(prov.used_today ?? 0), limit: num(prov.daily_limit) }) : t('limit.used_n', { used: num(prov.used_today ?? 0) })}</p>
                      <Button size="sm" variant="ghost" icon={I.Pencil} disabled={!can('config.patch')} onclick={() => ((limitP = prov), (limitOpen = true))}>{prov.daily_limit ? t('limit.edit') : t('limit.set')}</Button></div>
                  {:else if prov.daily_limit}<p class="xs muted num">{t('tool.provider_quota_n', { used: prov.used_today, limit: prov.daily_limit })}</p>{/if}
                  {#if !prov.configured && prov.status !== 'disabled'}<div><Button href="/connections" size="sm" icon={I.KeyRound}>{t('tool.add_key')}</Button></div>{/if}
                </div>
              {/each}
            </div>
          {:else}
            <p class="small muted">{t(tl.uses ? `tool.source.${tl.uses}` : 'tool.no_provider')}</p>
          {/if}
        </Card>
        {#if id === 'search.web'}
          <Card title={t('tool.search_order')} description={t('tool.search_order_desc')}>
            {#if !searchAll.length}
              <p class="small muted">{t('tool.search_none')}</p>
            {:else}
              <ol class="order">
                {#each searchAll as pid, i (pid)}
                  {@const usable = searchOrder.includes(pid)}
                  <li class:off={!usable}>
                    <span class="num xs muted">{usable ? i + 1 : '–'}</span>
                    <span class="grow"><strong class="small">{provName(pid)}</strong>{#if !usable}<span class="xs muted"> · {t('tools.state.no_key')}</span>{/if}</span>
                    {#if usable}
                      <IconButton icon={I.ArrowUp} size={16} disabled={i === 0} onclick={() => moveSearch(pid, -1)} label={t('tool.move_up', { name: provName(pid) })} />
                      <IconButton icon={I.ArrowDown} size={16} disabled={i === searchOrder.length - 1} onclick={() => moveSearch(pid, 1)} label={t('tool.move_down', { name: provName(pid) })} />
                    {/if}
                  </li>
                {/each}
              </ol>
              <div class="row">
                <Button size="sm" href="/connections" icon={I.Plus}>{t('tool.search_add')}</Button>
                {#if cfg.doc?.web_search?.order?.length}<Button size="sm" variant="ghost" icon={I.RotateCcw} onclick={resetSearchOrder}>{t('tool.search_order_reset')}</Button>{/if}
              </div>
            {/if}
          </Card>
        {/if}
      </div>
    {:else if tab === 'settings'}
      <div class="cols">
        <Card title={t('tool.switches')} description={t('tool.switches_desc')}>
          <div class="stack">
            {#if tl.opt_in}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t(`tool.optin.${id}`)}</span></div>{/if}
            {#each ['enabled', 'chat', 'commands'] as k}
              {@const na = (k === 'chat' && tl?.can_chat === false) || (k === 'commands' && tl?.can_command === false)}
              <div class="tri"><span class="small"><strong>{t(`tools.sw.${k}`)}</strong><br /><span class="muted xs">{na ? t(`tools.na.${k}`) : t(`tool.sw_hint.${k}`)}</span></span>
                {#if na}<span class="xs muted">{t('tools.na.label')}</span>
                {:else}<Segmented size="sm" label={t(`tools.sw.${k}`)} bind:value={(form as any)[k]} options={[{ value: 'default', label: t('common.default') }, { value: 'on', label: t('common.on') }, { value: 'off', label: t('common.off') }]} />{/if}
              </div>
            {/each}
          </div>
        </Card>
        <Card title={t('tool.planner')} description={t('tool.planner_desc')}>
          <div class="stack">
            <Field id="al" label={t('tool.aliases')} hint={t('tool.aliases_hint')}>
              {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} bind:value={form.aliases} />{/snippet}
            </Field>
            <Field id="ds" label={t('tool.description')} hint={t('tool.description_hint')}>
              {#snippet children({ id: fid, describedby })}<TextArea id={fid} aria-describedby={describedby} bind:value={form.description} rows={3} placeholder={toolDesc(id)} />{/snippet}
            </Field>
            <Field id="rt" label={t('tool.route')} hint={t('tool.route_hint')}>
              {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} bind:value={form.route} />{/snippet}
            </Field>
            <div class="stack defaults">
              <div class="row"><span class="small"><strong>{t('tool.defaults')}</strong><br /><span class="xs muted">{t('tool.defaults_hint')}</span></span></div>
              {#if inputs.length && defMode === 'form'}
                <ToolInputs tool={id} {inputs} bind:values={defVals} providers={providerOptions} idPrefix="df" defaults />
              {:else}
                <Field id="df" label={t('tool.defaults_json')} hint={t('tool.defaults_json_hint')} error={defaultsErr || undefined}>
                  {#snippet children({ id: fid, describedby, invalid })}<TextArea id={fid} aria-describedby={describedby} {invalid} bind:value={form.defaults} rows={5} mono spellcheck={false} placeholder={JSON.stringify(exampleInput(id, inputs, tl?.sample))} />{/snippet}
                </Field>
              {/if}
              {#if defaultsErr && defMode === 'form'}<p class="small err">{defaultsErr}</p>{/if}
              {#if inputs.length}<button type="button" class="linkish xs" onclick={() => setDefMode(defMode === 'form' ? 'json' : 'form')}>{defMode === 'form' ? t('toolp.json') : t('toolp.form')}</button>{/if}
            </div>
          </div>
        </Card>
        {#if isHTTP}
          <Card title={t('tool.http')} description={t('tool.http_desc')}>
            <div class="stack">
              <Field id="url" label="URL" hint={t('toolnew.url_hint', { name: '{name}', credential: '{credential}' })}>
                {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} bind:value={form.url} mono type="url" />{/snippet}
              </Field>
              <Field id="cr" label={t('tool.credential_ref')} hint={t('toolnew.cred_hint')}>
                {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} bind:value={form.credential_ref} mono placeholder="env:MY_API_KEY" />{/snippet}
              </Field>
              <Field id="pk" label={t('tool.pick')} hint={t('toolnew.pick_hint')}>
                {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} bind:value={form.pick} mono placeholder="data.items, meta.count" />{/snippet}
              </Field>
              <div class="form-grid">
                <Field id="mc" label={t('tool.max_chars')}>{#snippet children({ id: fid })}<Input id={fid} type="number" min="0" value={form.max_chars || ''} placeholder={t('settings.default_is', { v: '' }).trim()} oninput={(e) => (form.max_chars = Math.max(0, Number(e.currentTarget.value) || 0))} />{/snippet}</Field>
                <Field id="ts" label={t('tool.timeout')}>{#snippet children({ id: fid })}<Input id={fid} type="number" min="0" value={form.timeout_sec || ''} placeholder={t('settings.default_is', { v: '' }).trim()} oninput={(e) => (form.timeout_sec = Math.max(0, Number(e.currentTarget.value) || 0))} />{/snippet}</Field>
              </div>
            </div>
          </Card>
        {/if}
      </div>
    {:else if tab === 'limits'}
      <div class="cols">
        <Card title={t('tool.limits_title')} description={t('tool.limits_desc')}>
          <div class="form-grid">
            {#each ['per_day', 'per_user_per_day', 'per_channel_per_day', 'cooldown_sec'] as k}
              <Field id={k} label={t(`limits.${k}`)} hint={t(`limits.${k}_hint`)}>
                {#snippet children({ id: fid, describedby })}<Input id={fid} aria-describedby={describedby} type="number" min="0" inputmode="numeric" value={(form as any)[k] || ''} placeholder={t('usage.no_limit')} oninput={(e) => ((form as any)[k] = Math.max(0, Number(e.currentTarget.value) || 0))} />{/snippet}
              </Field>
            {/each}
          </div>
          <p class="xs muted" style:margin-top="12px">{t('tool.limits_zero')}</p>
        </Card>
        <Card title={t('tool.today_usage')}>
          {#if tl.limits?.per_day}
            <Progress value={tl.used_today} max={tl.limits.per_day} label={t('tool.today_usage')} tone={tl.used_today >= tl.limits.per_day ? 'danger' : tl.used_today >= 0.8 * tl.limits.per_day ? 'warn' : 'accent'} />
            <p class="small num" style:margin-top="8px">{t('tool.used_of', { used: num(tl.used_today), limit: num(tl.limits.per_day) })}</p>
          {:else}
            <p class="small"><strong class="num">{num(tl.used_today)}</strong> <span class="muted">{t('tool.no_daily_cap')}</span></p>
          {/if}
        </Card>
      </div>
    {:else if tab === 'rooms'}
      <Card padded={false} title={t('tool.rooms_title')} description={t('tool.rooms_desc')}>
        {#if rooms.length === 0}
          <EmptyState compact pose="guide" title={t('tool.no_rooms')}><Button href="/rooms">{t('nav.rooms')}</Button></EmptyState>
        {:else}
          <div use:scrollable class="table-scroll"><table class="table">
            <thead><tr><th>{t('rooms.col.channel')}</th><th>{t('room.tools_here')}</th></tr></thead>
            <tbody>
              {#each rooms as r (r.g + r.c)}
                <tr><td><a href="/rooms/{r.g}/{r.c}">#{r.c}</a> <span class="xs subtle mono">{r.g}</span></td>
                  <td><Segmented size="sm" label={t('room.tools_here')} value={r.v} onchange={(v) => setRoom(r.g, r.c, v)} options={[{ value: 'default', label: t('common.default') }, { value: 'on', label: t('common.on') }, { value: 'off', label: t('common.off') }]} /></td></tr>
              {/each}
            </tbody>
          </table></div>
        {/if}
      </Card>
    {:else if tab === 'test'}
      <div class="cols">
        <Card title={t('tool.test_title')} description={t('tool.test_desc')}>
          <div class="stack">
            {#if !tl.implemented}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('tool.not_implemented_body')}</span></div>{/if}
            {#if tl.provider_kinds?.length || isHTTP}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('tool.test_external')}</span></div>{/if}
            {#if testMode === 'form' && inputs.length}
              <ToolInputs tool={id} {inputs} bind:values={testVals} providers={providerOptions} idPrefix="tp" />
              {#if Object.keys(settings.defaults ?? {}).length}<p class="xs muted">{t('tool.test_defaults_note')}</p>{/if}
              {#if testErr}<p class="small err">{testErr}</p>{/if}
            {:else}
              <Field id="ti" label={t('tool.test_input')} hint={t('tool.test_input_hint')} error={testErr || undefined}>
                {#snippet children({ id: fid, describedby, invalid })}<TextArea id={fid} aria-describedby={describedby} {invalid} bind:value={testInput} rows={6} mono spellcheck={false} />{/snippet}
              </Field>
              <div><Button size="sm" variant="secondary" icon={I.Sparkles} onclick={insertExample}>{t('tool.insert_example')}</Button></div>
            {/if}
            {#if inputs.length}<button type="button" class="linkish xs" onclick={() => (testMode = testMode === 'form' ? 'json' : 'form')}>{testMode === 'form' ? t('toolp.json') : t('toolp.form')}</button>{/if}
            <div class="row"><Button variant="primary" icon={I.Play} loading={testing} disabled={!tl.implemented} onclick={runTest}>{t('tool.test_run')}</Button></div>
          </div>
        </Card>
        <Card title={t('tool.test_result')}>
          {#if !testRes}
            <p class="small muted">{t('tool.test_none')}</p>
          {:else}
            <div class="stack">
              <div class="row"><Status tone={testRes.ok ? 'ok' : 'danger'} label={testRes.ok ? t('tool.test_ok') : t('tool.test_fail')} />{#if testRes.latency_ms !== undefined}<span class="small muted num">{ms(testRes.latency_ms)}</span>{/if}{#if testRes.status}<span class="chip">{testRes.status}</span>{/if}</div>
              {#if testRes.error}<p class="small err break">{testRes.error.message}</p>{/if}
              {#if summary}<p class="summary">{summary}</p>{/if}
              <details open={!summary}><summary class="xs muted">{t('toolp.raw')}</summary><pre class="out">{JSON.stringify(testRes.output ?? testRes.error ?? {}, null, 2)}</pre></details>
            </div>
          {/if}
        </Card>
      </div>
    {:else if tab === 'ops'}
      {#if opsKind === 'notices'}
        <NoticeOps />
      {:else if opsKind === 'fortune'}
        <FortuneOps />
      {:else if opsKind === 'disasters'}
        <DisasterOps />
      {:else if opsKind === 'polls'}
        <Card title={t('tool.poll_create_title')}>
          <div class="stack">
            <Field id="pq" label={t('tool.poll_question')}>{#snippet children({ id: fid })}<Input id={fid} bind:value={pollQuestion} maxlength={300} />{/snippet}</Field>
            <Field id="po" label={t('tool.poll_options')} hint={t('tool.poll_options_hint')}>{#snippet children({ id: fid, describedby })}<TextArea id={fid} aria-describedby={describedby} bind:value={pollOptions} rows={5} />{/snippet}</Field>
            <div class="form-grid">
              <Field id="ps" label={t('tool.poll_scope')}>{#snippet children({ id: fid })}<Input id={fid} bind:value={pollScope} mono />{/snippet}</Field>
              <Field id="ph" label={t('tool.poll_hours')}>{#snippet children({ id: fid })}<Input id={fid} bind:value={pollHours} type="number" min="1" max="768" />{/snippet}</Field>
            </div>
            <Switch bind:checked={pollMulti} label={t('tool.poll_multi')} />
            <div><Button variant="primary" icon={I.Plus} loading={pollBusy} disabled={!pollQuestion.trim() || pollOptions.split('\n').filter((x) => x.trim()).length < 2} onclick={createPoll}>{t('tool.poll_create')}</Button></div>
          </div>
        </Card>
        <Card padded={false} title={t('tool.polls_title')}>
          {#if (polls.data?.polls ?? []).length === 0}
            <EmptyState compact pose="box" title={t('tool.polls_empty')} />
          {:else}
            <div use:scrollable class="table-scroll"><table class="table">
              <thead><tr><th>{t('tool.poll_question')}</th><th>{t('tool.poll_state')}</th><th class="r">{t('tool.poll_votes')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
              <tbody>
                {#each polls.data.polls as p (p.id)}
                  <tr><td><strong>{p.question ?? p.id}</strong><div class="poll-options">{#each p.options ?? [] as o}<label class="small"><input type="checkbox" checked={(pollSelections[p.id] ?? []).includes(o.id)} disabled={p.status === 'closed'} onchange={(e) => togglePollOption(p, o.id, (e.currentTarget as HTMLInputElement).checked)} /> {o.label}</label>{/each}</div></td><td><Status tone={p.status === 'closed' ? 'neutral' : 'ok'} label={p.status === 'closed' ? t('tool.poll_closed_s') : t('tool.poll_open')} size="sm" /></td><td class="r num">{p.total_selections ?? 0}</td>
                    <td class="r"><div class="stack" style:align-items="flex-end">{#if p.status !== 'closed'}<Button size="sm" variant="primary" disabled={!pollParticipant.trim() || !(pollSelections[p.id] ?? []).length} onclick={() => votePoll(p)}>{t('tool.poll_vote')}</Button><Button size="sm" variant="ghost" onclick={() => closePoll(p)}>{t('tool.poll_close')}</Button>{/if}</div></td></tr>
                {/each}
              </tbody>
            </table></div>
          {/if}
        </Card>
        <Card title={t('tool.poll_participant')} description={t('tool.poll_participant_hint')}><Input bind:value={pollParticipant} maxlength={80} mono /></Card>
      {/if}
    {:else if tab === 'stats'}
      <Card title={t('tool.stats_title')}>
        <Chart {labels} series={[{ key: id, label: t('tool.calls'), data: usageSeries, color: 'var(--chart-1)' }]} label={t('tool.stats_title')} />
      </Card>
    {/if}
  </div>
  <SaveBar count={changed.length} {saving} onsave={save} onreset={() => (form = JSON.parse(base))} note={t('tool.savebar_note')} />
{/if}

<Confirm bind:open={delOpen} title={t('tool.delete_title')} body={t('tool.delete_body')} confirmLabel={t('tool.delete_confirm')} danger requireText={id} onconfirm={remove} />

{#if limitP}<LimitDialog bind:open={limitOpen} id={limitP.id} name={provName(limitP.id)} limit={limitP.daily_limit ?? 0} used={limitP.used_today ?? 0} onsaved={() => void providers.refresh()} />{/if}

<style>
  .panel {
    margin-top: var(--s-4);
  }
  /* two columns; an odd last card takes the whole row instead of leaving half of it empty */
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
  }
  @media (min-width: 1000px) {
    .cols {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .cols > :global(:last-child:nth-child(odd)) {
      grid-column: 1 / -1;
    }
  }
  .tri {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
  }
  .out {
    margin: 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    font-size: var(--text-xs);
    max-height: 420px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .err {
    color: var(--danger);
  }
  .summary {
    font-weight: 600;
  }
  .linkish {
    align-self: flex-start;
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-text);
    text-decoration: underline;
    text-underline-offset: 2px;
    cursor: pointer;
  }
  .order {
    display: grid;
    gap: 4px;
    margin: 0 0 var(--s-3);
    padding: 0;
    list-style: none;
  }
  .order li {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    padding: 6px 8px;
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .order li.off {
    opacity: 0.7;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .defaults {
    gap: var(--s-2);
  }
  .poll-options {
    display: grid;
    gap: 4px;
    margin-top: 8px;
  }
</style>
