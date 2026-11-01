<script lang="ts">
  import { outcomeTone } from '../lib/outcomes'
  import { t, toolName, i18n } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { route } from '../lib/router.svelte'
  import { cfg, useConfig, patchConfig, loadConfig } from '../lib/config.svelte'
  import { overview, refreshOverview } from '../lib/overview.svelte'
  import { toast } from '../lib/toast.svelte'
  import { relative, datetime, num, usd } from '../lib/format'
  import { blockState, stateTone, blockHref, isKakao } from '../lib/blocks'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Status from '../components/Status.svelte'
  import Button from '../components/Button.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Select from '../components/Select.svelte'
  import Field from '../components/Field.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Switch from '../components/Switch.svelte'
  import SaveBar from '../components/SaveBar.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import Popover from '../components/Popover.svelte'
  import Input from '../components/Input.svelte'
  import Dialog from '../components/Dialog.svelte'
  import Timeline from '../components/Timeline.svelte'
  import ParticipationCard from '../components/ParticipationCard.svelte'
  import RoomStyleCard from '../components/RoomStyleCard.svelte'
  import ConversationCard from '../components/ConversationCard.svelte'
  import RemindersList from '../components/RemindersList.svelte'
  import { personaOptionsFor } from '../lib/persona'
  import * as I from '../lib/icons'

  let { params }: { params: { guild: string; channel: string } } = $props()
  useConfig()
  const g = $derived(params.guild)
  const c = $derived(params.channel)
  const list = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const personas = resource<any>('personas.list')
  const tools = resource<any>('tools.list', () => ({}), { live: ['config'] })
  const blocks = resource<any>('memory.blocks.list', () => ({}), { live: ['config', 'memory', 'job'] })
  const turns = resource<any>('turns.list', () => ({ guild_id: g, channel_id: c, limit: 20, lab: 'all' }), { live: ['turn'] })
  const ops = resource<any>('channels.ops_log', () => ({ guild_id: g, channel_id: c }), { live: ['config'] })
  const perms = resource<any>('channels.permissions', () => ({ guild_id: g, channel_id: c }), { live: ['discord', 'config'] })

  let tab = $state(route.query.get('tab') ?? 'behavior')
  const ch = $derived((list.data?.channels ?? []).find((x: any) => x.id === c && x.guild_id === g))
  const guildName = $derived.by(() => {
    const n = (list.data?.guilds ?? []).find((x: any) => x.id === g)?.name
    return n && n !== g ? n : ''
  })
  const pol = $derived(cfg.doc?.guilds?.[g]?.channels?.[c] ?? {})
  const paused = $derived(!!overview.data?.pause?.channels?.[`${g}:${c}`] || !!overview.data?.pause?.guilds?.[g])

  // editable copy of the behavior form
  let form = $state({ respond: 'mention', persona_ref: '', rules: '', capture: false, logging_level: 'ops', memory_use: 'on', disaster_alerts: 'on' })
  let base = $state('')
  let saving = $state(false)
  $effect(() => {
    if (!ch) return
    const f = { respond: ch.respond, persona_ref: ch.persona_ref, rules: pol.rules ?? '', capture: !!ch.capture, logging_level: ch.logging_level, memory_use: ch.memory_use, disaster_alerts: ch.disaster_alerts === false ? 'off' : 'on' }
    const s = JSON.stringify(f)
    if (s !== base) {
      form = f
      base = s
    }
  })
  const dirty = $derived(base ? Object.entries(form).filter(([k, v]) => (JSON.parse(base) as any)[k] !== v).length : 0)
  async function save() {
    saving = true
    const r = await call('channels.configure', { guild_id: g, channel_id: c, ...form, capture: form.capture ? 'true' : 'false' })
    saving = false
    if (r.ok) {
      toast('ok', t('rooms.saved'))
      base = ''
      await list.refresh()
    } else toast('error', t('config.save_failed'), r.error?.message)
  }

  async function setPause(v: boolean) {
    const r = await call('runtime.pause', { scope: 'channel', guild_id: g, channel_id: c, paused: v ? 'true' : 'false' })
    await refreshOverview()
    r.ok ? toast(v ? 'warn' : 'ok', v ? t('rooms.paused_toast') : t('rooms.resumed_toast')) : toast('error', r.error?.message ?? '')
  }

  // per-channel tool switch: default (follow instance) | on | off
  const toolState = (id: string): 'default' | 'on' | 'off' => (pol.tools?.[id] === true ? 'on' : pol.tools?.[id] === false ? 'off' : 'default')
  async function setTool(id: string, v: 'default' | 'on' | 'off') {
    const path = ['guilds', g, 'channels', c, 'tools', id]
    await patchConfig(v === 'default' ? [{ path, op: 'unset' }] : [{ path, value: v === 'on' }], { success: t('rooms.tool_saved') })
  }

  // memory: this channel's own conversation plus the blocks connected to it
  const dir = $derived(((blocks.data?.channels ?? []) as any[]).find((x) => x.guild_id === g && x.channel_id === c))
  const allBlocks = $derived(((blocks.data?.blocks ?? []) as any[]))
  const ownBlock = $derived(allBlocks.find((b) => b.source_room === `discord:${g}:${c}`))
  const attached = $derived(((dir?.attached ?? []) as any[]).map((a) => ({ ...a, block: allBlocks.find((b) => b.source_room === a.source_room) })))
  const attachable = $derived(allBlocks.filter((b) => (b.message_count ?? 0) > 0 && b.source_room !== `discord:${g}:${c}` && !attached.some((a) => a.source_room === b.source_room)))
  async function attach(b: any, close: () => void) {
    close()
    const r = await call('channels.view.attach', { guild_id: g, channel_id: c, block_id: b.block_id })
    r.ok ? toast('ok', t('rooms.block_attached')) : toast('error', t('rooms.block_failed'), r.error?.message)
    void blocks.refresh()
  }
  async function detach(id: string) {
    const r = await call('channels.view.detach', { guild_id: g, channel_id: c, block_id: id })
    r.ok ? toast('ok', t('rooms.block_detached')) : toast('error', t('rooms.block_failed'), r.error?.message)
    void blocks.refresh()
  }
  // try before saving: a lab turn (nothing sent to Discord) with the edited persona and rules
  const persona = $derived((personas.data?.personas ?? []).find((p: any) => p.id === form.persona_ref))
  // persona choices: the room's language first, other languages labelled, hidden ones left out
  const roomLang = $derived.by(() => {
    const cur = (personas.data?.personas ?? []).find((p: any) => p.id === form.persona_ref)
    return cur?.locale || cfg.doc?.guilds?.[g]?.locale || cfg.doc?.i18n?.default_locale || i18n.lang
  })
  const personaOptions = $derived(personaOptionsFor((personas.data?.personas ?? []) as any[], roomLang, form.persona_ref))
  const savedForm = $derived(base ? JSON.parse(base) : form)
  const draftDiffers = $derived(savedForm.persona_ref !== form.persona_ref || (savedForm.rules ?? '') !== (form.rules ?? ''))
  type Try = { id: number; q: string; compare: boolean; draft?: any; saved?: any; pending: boolean }
  let tryMsg = $state('')
  let tryCompare = $state(true)
  let tries = $state<Try[]>([])
  let trySeq = 0
  async function tryDraft() {
    const q = tryMsg.trim()
    if (!q) return
    tryMsg = ''
    const id = ++trySeq
    const compare = tryCompare && draftDiffers
    tries = [{ id, q, compare, pending: true }, ...tries].slice(0, 6)
    const input = { guild_id: g, channel_id: c, message: q, speaker: t('lab.default_speaker') }
    const [a, b] = await Promise.all([
      call('chat.respond', { ...input, persona_ref: form.persona_ref, draft_rules: form.rules ?? '' }),
      compare ? call('chat.respond', input) : Promise.resolve(null),
    ])
    const x = tries.find((y) => y.id === id)
    if (!x) return
    x.pending = false
    x.draft = a.ok ? a.output : { error: a.error?.message }
    if (b) x.saved = b.ok ? b.output : { error: b.error?.message }
  }
  function tryKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault()
      void tryDraft()
    }
  }

  // memory workspace: catch-up, summary and timeline of this channel
  const backfill = resource<any>('memory.backfill', () => ({ status: true }), { live: ['memory', 'job'] })
  const lastBackfill = $derived(((backfill.data?.last?.channels ?? []) as any[]).find((x) => x.guild_id === g && x.channel_id === c))
  let catching = $state(false)
  async function catchUp() {
    catching = true
    const r = await call('memory.backfill', { guild_id: g, channel_id: c })
    catching = false
    if (r.ok) toast('ok', t('room.backfill_done', { n: num(r.output.report?.stored ?? 0) }))
    else toast('error', t('common.failed'), r.error?.message)
    void backfill.refresh()
  }
  const summary = resource<any>('summaries.channel', () => ({ guild_id: g, channel_id: c, saved: true }))
  let summarizing = $state(false)
  async function summarize(refresh: boolean) {
    summarizing = true
    const r = await call('summaries.channel', { guild_id: g, channel_id: c, ...(refresh ? { refresh: true } : {}) })
    summarizing = false
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    if (r.output.status === 'empty') return toast('warn', r.output.card?.text ?? t('room.summary_too_few'))
    void summary.refresh()
  }

  // copy this room's saved settings to other rooms
  let copyOpen = $state(false)
  let copyWhat = $state<Record<string, boolean>>({ reply: true, memory: false, logging: false, tools: false })
  let copyTo = $state<Set<string>>(new Set())
  let copying = $state(false)
  const others = $derived(((list.data?.channels ?? []) as any[]).filter((x) => !(x.id === c && x.guild_id === g)))
  const guildLabel = (id: string) => (list.data?.guilds ?? []).find((x: any) => x.id === id)?.name ?? id
  function toggleCopy(k: string) {
    const s = new Set(copyTo)
    s.has(k) ? s.delete(k) : s.add(k)
    copyTo = s
  }
  async function copySettings() {
    copying = true
    const src = savedForm
    const input: Record<string, unknown> = {}
    if (copyWhat.reply) Object.assign(input, { respond: src.respond, persona_ref: src.persona_ref, rules: src.rules ?? '' })
    if (copyWhat.memory) Object.assign(input, { memory_use: src.memory_use, capture: src.capture ? 'true' : 'false' })
    if (copyWhat.logging) input.logging_level = src.logging_level
    const tools = { ...(pol.tools ?? {}) }
    let ok = 0
    const toolOps: any[] = []
    for (const k of copyTo) {
      const [tg, tc] = k.split(':')
      if (Object.keys(input).length && !(await call('channels.configure', { guild_id: tg, channel_id: tc, ...input })).ok) continue
      ok++
      const path = ['guilds', tg, 'channels', tc, 'tools']
      if (copyWhat.tools) toolOps.push(Object.keys(tools).length ? { path, value: tools } : { path, op: 'unset' })
    }
    if (toolOps.length) {
      await loadConfig(true) // channels.configure moved the revision on
      if (!(await patchConfig(toolOps, { quiet: true })).ok) ok = 0
    }
    copying = false
    toast(ok === copyTo.size ? 'ok' : 'warn', t('room.copy_done', { n: ok, total: copyTo.size }))
    copyOpen = false
    copyTo = new Set()
    void list.refresh()
  }

  let invite = $state('')
  let inviteErr = $state('')
  async function getInvite() {
    // preselect this room's server so the invite (or a re-invite with more permissions) lands here
    const r = await call('channels.invite', { guild_id: g })
    if (r.ok) invite = r.output.url
    else inviteErr = r.error?.message ?? ''
  }
</script>

<PageHeader title={ch ? `#${ch.name}` : `#${c}`} description={guildName ? t('room.desc', { guild: guildName }) : t('rooms.server.unnamed')} crumbs={[{ href: '/rooms', label: t('nav.rooms') }]}>
  {#snippet actions()}
    <Button href="/lab?guild={g}&channel={c}" icon={I.FlaskConical}>{t('room.test_here')}</Button>
    <Button data-tour="room-copy" variant="ghost" icon={I.Copy} onclick={() => (copyOpen = true)} disabled={!ch}>{t('room.copy')}</Button>
    {#if paused}<Button variant="primary" icon={I.Play} onclick={() => setPause(false)}>{t('room.resume')}</Button>
    {:else}<Button variant="secondary" icon={I.Pause} onclick={() => setPause(true)}>{t('room.pause')}</Button>{/if}
  {/snippet}
  <div class="row">
    {#if ch}
      <Status tone={ch.live ? 'ok' : ch.practice ? 'info' : 'neutral'} label={ch.live ? t('rooms.live') : ch.practice ? t('rooms.practice') : t('rooms.offline')} size="sm" />
      {#if paused}<Status tone="warn" label={t('rooms.paused')} size="sm" />{/if}
      {#if !ch.configured}<span class="chip">{t('rooms.defaults')}</span>{/if}
    {/if}
    <code class="xs subtle">{g} / {c}</code>
  </div>
</PageHeader>

<div data-tour="room-tabs"><Tabs id="room" bind:active={tab} tabs={[
  { id: 'behavior', label: t('room.tab.behavior'), icon: I.SlidersHorizontal },
  { id: 'memory', label: t('room.tab.memory'), icon: I.Brain },
  { id: 'tools', label: t('room.tab.tools'), icon: I.Wrench },
  { id: 'activity', label: t('room.tab.activity'), icon: I.History },
  { id: 'access', label: t('room.tab.access'), icon: I.ShieldCheck }]} /></div>

<div class="panel" role="tabpanel" id="room-panel-{tab}" aria-labelledby="room-tab-{tab}">
  {#if !list.data}
    <LoadState res={list}><Card><Skeleton lines={6} /></Card></LoadState>
  {:else if !ch}
    <Card><EmptyState pose="lost" title={t('room.not_found')} description={t('room.not_found_body')}><Button href="/rooms">{t('nav.rooms')}</Button></EmptyState></Card>
  {:else if tab === 'behavior'}
    {#if perms.data?.known && (perms.data.thread_missing ?? []).length}
      <div class="callout warn threadwarn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('room.thread_missing', { perms: perms.data.thread_missing.map((p: string) => t(`perm.${p}`)).join(', ') })}</span><Button size="sm" variant="ghost" onclick={() => (tab = 'access')}>{t('room.thread_missing_go')}</Button></div>
    {/if}
    <div class="cols">
      <Card title={t('room.respond_title')} description={t('room.respond_desc')}>
        <div class="stack">
          <Segmented label={t('rooms.respond')} bind:value={form.respond} options={['always', 'mention', 'command', 'off'].map((v) => ({ value: v, label: t(`rooms.respond.${v}`) }))} />
          <p class="small muted">{t(`rooms.respond_hint.${form.respond}`)}</p>
          <Switch checked={form.disaster_alerts === 'on'} disabled={form.respond === 'off'} onchange={(v) => (form.disaster_alerts = v ? 'on' : 'off')} label={t('room.disaster_alerts')} hint={form.respond === 'off' ? t('room.disaster_alerts_off_respond') : t('room.disaster_alerts_hint')} />
          <Field id="persona" label={t('rooms.col.persona')} hint={t('room.persona_hint')}>
            {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={form.persona_ref} options={personaOptions} />{/snippet}
          </Field>
          {#if persona}
            <details class="style">
              <summary class="small">{t('room.persona_style')}</summary>
              <p class="small muted pre">{persona.style}</p>
              <a class="xs" href="/persona">{t('room.persona_edit')}</a>
            </details>
          {/if}
          <Field id="rules" label={t('room.rules')} hint={t('room.rules_hint', { n: (form.rules ?? '').length })}>
            {#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={form.rules} maxlength={2000} rows={5} placeholder={t('room.rules_ph')} />{/snippet}
          </Field>
        </div>
      </Card>
      <Card title={t('room.try_title')} description={t('room.try_desc')} data-tour="room-try">
        {#snippet actions()}<Button size="sm" variant="ghost" href="/lab?guild={g}&channel={c}&persona={encodeURIComponent(form.persona_ref)}" iconEnd={I.ArrowRight}>{t('room.try_more')}</Button>{/snippet}
        <div class="stack">
          <div class="row">
            {#if draftDiffers}<span class="chip accent">{t('room.try_using_draft')}</span>{:else}<span class="chip">{t('room.try_using_saved')}</span>{/if}
          </div>
          <TextArea bind:value={tryMsg} rows={2} placeholder={t('room.try_ph')} aria-label={t('room.try_ph')} onkeydown={tryKey} />
          <div class="row">
            {#if draftDiffers}<Switch bind:checked={tryCompare} label={t('room.try_compare')} />{/if}
            <span class="spacer"></span>
            <Button variant="primary" icon={I.Send} onclick={tryDraft} disabled={!tryMsg.trim()}>{t('room.try_send')}</Button>
          </div>
          {#if tries.length === 0}
            <p class="small muted">{t('room.try_empty')}</p>
          {:else}
            <ul class="tries">
              {#each tries as tr (tr.id)}
                <li>
                  <p class="q small"><I.CircleUser size={14} aria-hidden="true" />{tr.q}</p>
                  <div class="answers" class:two={tr.compare}>
                    {#each tr.compare ? [['draft', tr.draft], ['saved', tr.saved]] : [['draft', tr.draft]] as [side, out]}
                      <div class="ans">
                        {#if tr.compare}<span class="xs muted">{t(`room.try_${side}`)}</span>{/if}
                        {#if tr.pending && !out}<Skeleton lines={2} />
                        {:else if out?.error}<p class="small err">{out.error}</p>
                        {:else if out}<p class="small pre">{out.answer || t('room.try_no_answer')}</p><span class="xs subtle num">{t('room.try_meta', { cost: usd(out.cost_usd ?? 0, 4), n: out.hits ?? 0 })}</span>{/if}
                      </div>
                    {/each}
                  </div>
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      </Card>
      <ConversationCard guild={g} channel={c} />
      <RoomStyleCard guild={g} channel={c} />
      <ParticipationCard guild={g} channel={c} />
      <Card title={t('room.collect_title')} description={t('room.collect_desc')}>
        <div class="stack">
          <Field id="log" label={t('room.logging')} hint={t('room.logging_hint')}>
            {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={form.logging_level} options={['off', 'errors', 'ops'].map((v) => ({ value: v, label: t(`room.logging.${v}`) }))} />{/snippet}
          </Field>
        </div>
      </Card>
    </div>
  {:else if tab === 'memory'}
    <div class="cols">
      <Card title={t('room.memory_title')} description={t('room.memory_desc')}>
        <div class="stack">
          <Switch checked={form.memory_use === 'on'} onchange={(v) => (form.memory_use = v ? 'on' : 'off')} label={t('room.memory_use')} hint={t('room.memory_use_hint')} />
          <Switch bind:checked={form.capture} label={t('room.capture')} hint={t('room.capture_hint')} />
          {#if form.capture && !cfg.doc?.memory?.capture}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('block.capture_global_off')}</span><Button size="sm" variant="primary" onclick={() => patchConfig([{ path: ['memory', 'capture'], value: true }], { success: t('block.capture_global_on') })}>{t('block.capture_global_turn_on')}</Button></div>{/if}
        </div>
      </Card>
      <Card title={t('room.blocks_title')} description={t('room.blocks_desc')}>
        {#snippet actions()}<Button size="sm" variant="ghost" href="/blocks?view=channels" iconEnd={I.ArrowRight}>{t('room.manage_blocks')}</Button>{/snippet}
        <ul class="mem">
          <li class="own">
            <span class="mi"><I.Hash size={15} aria-hidden="true" /></span>
            <span class="ml"><strong>{t('connect.own')}</strong><span class="xs muted">{ownBlock?.message_count ? t('blocks.meta.messages', { n: num(ownBlock.message_count) }) : form.capture ? t('connect.own_waiting') : t('connect.own_off')}</span></span>
            {#if ownBlock?.message_count}<a class="xs" href={blockHref(ownBlock.source_room)}>{t('room.open_block')}</a>{/if}
          </li>
          {#each attached as a (a.block_id)}
            <li>
              <span class="mi">{#if a.block && isKakao(a.block)}<I.MessagesSquare size={15} aria-hidden="true" />{:else}<I.Hash size={15} aria-hidden="true" />{/if}</span>
              <span class="ml">
                {#if a.stale}<strong class="stale">{t('connect.stale')}</strong><span class="xs muted">{t('room.stale_hint')}</span>
                {:else}<a href={blockHref(a.source_room)}><strong>{a.label}</strong></a>{#if a.block}<span class="xs muted">{t('blocks.meta.messages', { n: num(a.block.message_count) })}</span>{/if}{/if}
              </span>
              {#if a.block && blockState(a.block) !== 'ready'}<Status tone={stateTone[blockState(a.block)]} label={t(`blocks.state.${blockState(a.block)}`)} size="sm" />{/if}
              <Button size="sm" variant="danger-ghost" icon={I.X} onclick={() => detach(a.block_id)}>{t('room.detach')}</Button>
            </li>
          {/each}
        </ul>
        <div style:margin-top="12px">
          <Popover width={320} align="start">
            {#snippet trigger({ toggle, expanded, id })}
              <Button size="sm" icon={I.Plus} onclick={toggle} aria-expanded={expanded} aria-controls={id} disabled={!attachable.length}>{t('room.attach')}</Button>
            {/snippet}
            {#snippet children({ close })}
              <ul class="menu">
                {#each attachable as b (b.source_room)}
                  <li><button type="button" onclick={() => attach(b, close)}><span class="mi">{#if isKakao(b)}<I.MessagesSquare size={14} />{:else}<I.Hash size={14} />{/if}</span><span class="ml">{b.label}<span class="xs muted"> · {t('blocks.meta.messages', { n: num(b.message_count) })}</span></span></button></li>
                {/each}
              </ul>
            {/snippet}
          </Popover>
          {#if !attachable.length && !attached.length}<p class="xs muted" style:margin-top="8px">{t('room.no_blocks_yet')} <a href="/blocks?import=1">{t('blocks.import')}</a></p>{/if}
        </div>
      </Card>
      <Card title={t('room.summary_title')} description={t('room.summary_desc')}>
        {#snippet actions()}
          {#if summary.data?.status === 'ok'}<Button size="sm" variant="ghost" icon={I.RotateCcw} loading={summarizing} onclick={() => summarize(true)}>{t('room.summary_refresh')}</Button>{/if}
          <Button size="sm" icon={I.Sparkles} loading={summarizing} onclick={() => summarize(false)}>{summary.data?.status === 'ok' ? t('room.summary_update') : t('room.summary_make')}</Button>
        {/snippet}
        {#if !summary.data}<LoadState res={summary}><Skeleton lines={3} /></LoadState>
        {:else if summary.data.status === 'ok'}
          <p class="small pre">{summary.data.summary}</p>
          <p class="xs subtle num" style:margin-top="8px">{t('room.summary_meta', { at: datetime(summary.data.updated_at), n: num(summary.data.messages) })}</p>
        {:else}<p class="small muted">{savedForm.capture ? t('room.summary_none') : t('room.summary_needs_capture')}</p>{/if}
      </Card>
      <Card title={t('room.backfill_title')} description={t('room.backfill_desc', { days: backfill.data?.days ?? 7 })}>
        {#snippet actions()}<Button size="sm" icon={I.RefreshCw} loading={catching} disabled={!savedForm.capture} onclick={catchUp}>{t('room.backfill_run')}</Button>{/snippet}
        {#if !savedForm.capture}<p class="small muted">{t('room.backfill_needs_capture')}</p>
        {:else if backfill.data && !backfill.data.days}<p class="small muted">{t('room.backfill_off')} <a href="/settings?section=memory">{t('room.backfill_settings')}</a></p>
        {:else if lastBackfill}
          <p class="small">{t('room.backfill_last', { at: relative(backfill.data.last.at), read: num(lastBackfill.read), stored: num(lastBackfill.stored) })}</p>
          {#if lastBackfill.skipped}<p class="xs muted">{lastBackfill.skipped === 'never_captured' || lastBackfill.skipped === 'no_history' ? t(`room.backfill_skip.${lastBackfill.skipped}`) : lastBackfill.skipped}</p>{/if}
          {#if lastBackfill.truncated}<p class="xs muted">{t('room.backfill_truncated')}</p>{/if}
        {:else}<p class="small muted">{t('room.backfill_never')}</p>{/if}
      </Card>
    </div>
    <div style:margin-top="var(--s-4)">
      <Card title={t('room.timeline_title')} description={t('room.timeline_desc')}>
        {#snippet actions()}<Button size="sm" variant="ghost" href="/lab?guild={g}&channel={c}" icon={I.Search}>{t('room.search_memory')}</Button>{/snippet}
        <Timeline scope={{ guild_id: g, channel_id: c }} own={`discord:${g}:${c}`} />
      </Card>
    </div>
  {:else if tab === 'tools'}
    <Card padded={false} title={t('room.tools_title')} description={t('room.tools_desc')}>
      {#if !tools.data}<div style:padding="20px"><LoadState res={tools}><Skeleton lines={6} /></LoadState></div>{:else}
        <div use:scrollable class="table-scroll">
          <table class="table">
            <thead><tr><th>{t('tools.col.tool')}</th><th>{t('room.tools_global')}</th><th>{t('room.tools_here')}</th></tr></thead>
            <tbody>
              {#each tools.data.tools.filter((x: any) => x.id !== 'help.version') as tl (tl.id)}
                <tr>
                  <td><a class="row-link" href="/tools/{encodeURIComponent(tl.id)}">{toolName(tl.id)}</a><div class="xs subtle mono">{tl.id}</div></td>
                  <td><Status tone={tl.enabled ? (tl.chat ? 'ok' : 'info') : 'neutral'} label={!tl.enabled ? t('tools.state.off') : tl.chat ? t('tools.state.chat') : t('tools.state.command_only')} size="sm" /></td>
                  <td><Segmented size="sm" label={t('room.tools_here')} value={toolState(tl.id)} onchange={(v) => setTool(tl.id, v)} options={[{ value: 'default', label: t('common.default') }, { value: 'on', label: t('common.on') }, { value: 'off', label: t('common.off') }]} /></td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card>
  {:else if tab === 'activity'}
    <div class="cols">
      <Card title={t('room.recent_turns')} padded={false}>
        {#if (turns.data?.turns ?? []).length === 0}
          <EmptyState compact pose="think" title={t('home.recent.empty')} />
        {:else}
          <ul class="tl">
            {#each turns.data.turns as tr (tr.id)}
              <li><a href="/turns?id={tr.id}"><span class="ellipsis">{#if tr.body?.message}{tr.body.message}{:else}<span class="muted">{t(tr.body?.body_free ? 'turns.no_text' : 'turns.no_message')}</span>{/if}</span><Status tone={outcomeTone(tr.outcome)} label={t(`outcome.${tr.outcome}`)} size="sm" /><span class="xs muted num nowrap">{relative(tr.at)}</span></a></li>
            {/each}
          </ul>
        {/if}
      </Card>
      <Card title={t('rem.title')} description={t('rem.room_desc')}>
        <RemindersList scope={{ guild_id: g, channel_id: c }} empty={t('rem.room_empty')} />
      </Card>
      <Card title={t('room.ops_log')} padded={false}>
        {#if (ops.data?.entries ?? ops.data?.log ?? []).length === 0}
          <p class="small muted" style:padding="20px">{t('room.no_ops')}</p>
        {:else}
          <ul class="tl">
            {#each ops.data.entries ?? ops.data.log as e}
              <li><div class="op"><span class="chip">{e.kind}</span><span class="small break">{e.summary}</span><span class="xs muted num nowrap">{datetime(e.created_at)}</span></div></li>
            {/each}
          </ul>
        {/if}
      </Card>
    </div>
  {:else if tab === 'access'}
    <div class="cols">
      <Card title={t('room.perms_title')} description={t('room.perms_desc')}>
        {#if !perms.data || !perms.data.known}
          <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('room.perms_unknown')}</span></div>
        {:else}
          <ul class="perms">
            {#each perms.data.required as p}
              {@const miss = perms.data.missing.includes(p)}
              <li><Status tone={miss ? 'danger' : 'ok'} label={miss ? t('room.perm_missing') : t('room.perm_ok')} size="sm" /><span class="small">{t(`perm.${p}`)}</span></li>
            {/each}
          </ul>
          {#if perms.data.missing.length}<p class="small muted">{t('room.perms_fix')}</p>{/if}
          {#if perms.data.recommended?.length}
            <h3 class="small sub">{t('room.perms_recommended')}</h3>
            <ul class="perms">
              {#each perms.data.recommended as p}
                {@const miss = (perms.data.recommended_missing ?? []).includes(p)}
                <li><Status tone={miss ? 'warn' : 'ok'} label={miss ? t('room.perm_missing') : t('room.perm_ok')} size="sm" /><span class="small">{t(`perm.${p}`)}</span>{#if miss}<span class="xs muted">{t(`perm.${p}_why`)}</span>{/if}</li>
              {/each}
            </ul>
          {/if}
        {/if}
      </Card>
      <Card title={t('room.invite_title')} description={t('room.invite_desc')}>
        {#if invite}
          <div class="row"><a href={invite} target="_blank" rel="noopener" class="break small">{invite}</a><CopyButton text={invite} /></div>
        {:else}
          <Button icon={I.Link} onclick={getInvite}>{t('room.invite_make')}</Button>
          {#if inviteErr}<p class="small err">{inviteErr}</p>{/if}
        {/if}
      </Card>
      <Card title={t('room.pause_title')} description={t('room.pause_desc')}>
        <Switch checked={paused} onchange={setPause} label={t('room.pause_switch')} />
      </Card>
    </div>
  {/if}
</div>

<Dialog bind:open={copyOpen} title={t('room.copy_title')} description={t('room.copy_desc')}>
  <div class="stack">
    {#if dirty}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('room.copy_unsaved')}</span></div>{/if}
    <fieldset>
      <legend class="small">{t('room.copy_what')}</legend>
      {#each ['reply', 'memory', 'logging', 'tools'] as k}
        <label class="check"><input type="checkbox" bind:checked={copyWhat[k]} />{t(`room.copy.${k}`)}</label>
      {/each}
    </fieldset>
    <fieldset>
      <legend class="small">{t('room.copy_targets')}</legend>
      {#if others.length === 0}<p class="small muted">{t('room.copy_none')}</p>{/if}
      <div class="targets">
        {#each others as o (o.guild_id + o.id)}
          <label class="check"><input type="checkbox" checked={copyTo.has(`${o.guild_id}:${o.id}`)} onchange={() => toggleCopy(`${o.guild_id}:${o.id}`)} />#{o.name}<span class="xs muted">{guildLabel(o.guild_id)}</span></label>
        {/each}
      </div>
    </fieldset>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (copyOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={copying} disabled={!copyTo.size || !Object.values(copyWhat).some(Boolean)} onclick={copySettings}>{t('room.copy_apply', { n: copyTo.size })}</Button>
  {/snippet}
</Dialog>

<SaveBar count={dirty} saving={saving} onsave={save} onreset={() => (form = JSON.parse(base))} />

<style>
  .panel {
    margin-top: var(--s-4);
  }
  .perms {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 8px;
  }
  .perms li {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 10px;
  }
  .sub {
    margin-top: var(--s-4);
    margin-bottom: var(--s-2);
  }
  .threadwarn {
    margin-bottom: var(--s-4);
    align-items: center;
    flex-wrap: wrap;
  }
  .threadwarn > span:nth-child(2) {
    flex: 1 1 260px;
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
  .tl {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .tl a,
  .op {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto auto;
    align-items: center;
    gap: var(--s-3);
    padding: 10px var(--pad-card);
    color: var(--text);
    text-decoration: none;
  }
  .op {
    grid-template-columns: auto minmax(0, 1fr) auto;
  }
  .mem {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .mem li {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    padding: 8px 10px;
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .mem li.own {
    background: none;
    box-shadow: inset 0 0 0 1px var(--border);
  }
  .mi {
    flex: none;
    display: inline-flex;
    color: var(--text-muted);
  }
  .ml {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .ml a {
    color: var(--text);
    text-decoration: none;
  }
  .stale {
    color: var(--danger);
  }
  .menu {
    list-style: none;
    margin: 0;
    padding: 4px 0;
    max-height: 300px;
    overflow: auto;
  }
  .menu button {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 8px 10px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text);
    text-align: start;
    cursor: pointer;
  }
  .menu button:hover {
    background: var(--surface-2);
  }
  .tl a:hover {
    background: var(--surface-2);
  }
  .err {
    color: var(--danger);
    margin-top: 8px;
  }
  .pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .style summary {
    cursor: pointer;
    color: var(--text-muted);
  }
  .style p {
    margin: 8px 0;
    max-height: 180px;
    overflow: auto;
  }
  .tries {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: var(--s-3);
  }
  .tries .q {
    display: flex;
    align-items: center;
    gap: 6px;
    margin: 0 0 6px;
    color: var(--text-muted);
  }
  .answers {
    display: grid;
    gap: var(--s-2);
  }
  .answers.two {
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 200px), 1fr));
  }
  .ans {
    display: grid;
    gap: 4px;
    padding: 10px 12px;
    border-radius: var(--r);
    background: var(--surface-2);
    min-width: 0;
  }
  .ans p {
    margin: 0;
  }
  fieldset {
    border: 0;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  legend {
    margin-bottom: 6px;
    font-weight: 600;
  }
  .check {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .targets {
    display: grid;
    gap: 6px;
    max-height: 260px;
    overflow: auto;
  }
</style>
