<script lang="ts">
  import { t, i18n } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, patchConfig, useConfig } from '../lib/config.svelte'
  import { overview } from '../lib/overview.svelte'
  import { toast } from '../lib/toast.svelte'
  import { navigate } from '../lib/router.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Status from '../components/Status.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import Select from '../components/Select.svelte'
  import Field from '../components/Field.svelte'
  import Dialog from '../components/Dialog.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Switch from '../components/Switch.svelte'
  import Confirm from '../components/Confirm.svelte'
  import { personaTitle, personaOptionsFor } from '../lib/persona'
  import * as I from '../lib/icons'

  const list = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const personas = resource<any>('personas.list')
  useConfig()
  let q = $state('')
  let selected = $state<Set<string>>(new Set())
  let bulkRespond = $state('')
  let bulkPersona = $state('')
  let addOpen = $state(false)
  let add = $state({ guild: '', channel: '', respond: 'mention' })
  let addErr = $state('')
  let busy = $state(false)
  let progress = $state<{ done: number; total: number } | null>(null)
  let barH = $state(0) // the bottom bar's height, kept free under the last table row
  let rev = $state(0) // bumped when a row switch has to snap back to what the server says

  const personaLabel = (id: string) => personaTitle(personas.data?.personas?.find((p: any) => p.id === id), id)
  // the language a room's persona choices lead with: its current persona, then the server, then the instance
  const roomLangOf = (c: any) => {
    const cur = (personas.data?.personas ?? []).find((p: any) => p.id === c.persona_ref)
    return cur?.locale || cfg.doc?.guilds?.[c.guild_id]?.locale || cfg.doc?.i18n?.default_locale || i18n.lang
  }
  type Guild = { id: string; name: string; channels: any[]; allowed: boolean; approved: boolean; configured: boolean; live: boolean; members?: number; joined_at?: string; blocked_users: string[] }
  const guilds = $derived.by(() => {
    const out: Guild[] = []
    for (const g of list.data?.guilds ?? []) out.push({ allowed: true, approved: false, configured: false, live: false, blocked_users: [], ...g, channels: [] })
    for (const c of list.data?.channels ?? []) {
      if (q && !(`${c.name} ${c.id}`.toLowerCase().includes(q.toLowerCase()))) continue
      let g = out.find((x) => x.id === c.guild_id)
      if (!g) out.push((g = { id: c.guild_id, name: c.guild_id, channels: [], allowed: true, approved: false, configured: true, live: false, blocked_users: [] }))
      g.channels.push(c)
    }
    // servers waiting for approval first
    return out.filter((g) => g.channels.length || !q).sort((a, b) => Number(a.allowed) - Number(b.allowed))
  })
  const pending = $derived(guilds.filter((g) => !g.allowed))

  // server approval (config access): a server the bot is newly invited to waits here
  async function access(input: Record<string, unknown>, ok: string) {
    const r = await call('access.policy', input)
    r.ok ? toast('ok', ok) : toast('error', t('common.failed'), r.error?.message)
    void list.refresh()
  }
  // strict privacy (config guilds.<id>.privacy): four opt-in switches per server, all off by default; off is stored as unset
  const PRIVACY = ['own_card_only', 'no_hearsay', 'no_sensitive', 'card_channel_only'] as const
  const privacyOn = (g: Guild, k: (typeof PRIVACY)[number]) => !!cfg.doc?.guilds?.[g.id]?.privacy?.[k]
  const privacyCount = (g: Guild) => PRIVACY.filter((k) => privacyOn(g, k)).length
  async function setPrivacy(g: Guild, k: (typeof PRIVACY)[number], v: boolean) {
    await patchConfig([v ? { path: ['guilds', g.id, 'privacy', k], value: true } : { path: ['guilds', g.id, 'privacy', k], op: 'unset' }], { success: t('rooms.privacy.saved') })
  }
  // server log (config guilds.<id>.log): deleted messages and problems in a server go to its log channel;
  // unset = the channel named "logs" when there is one, as the previous Masamong did
  const logOf = (g: Guild) => (g as any).log ?? {}
  const logChoice = (g: Guild) => cfg.doc?.guilds?.[g.id]?.log?.channel || 'off'
  const logOptions = (g: Guild) => {
    const out = [
      { value: 'off', label: t('rooms.log.off') },
      { value: 'auto', label: t('rooms.log.auto') },
      ...g.channels.filter((c) => c.live !== false).map((c) => ({ value: c.id, label: `#${c.name && c.name !== c.id ? c.name : c.id}` })),
    ]
    const cur = logChoice(g)
    if (!out.some((o) => o.value === cur)) out.push({ value: cur, label: `#${cur}` }) // a set channel Discord no longer lists
    return out
  }
  const logSummary = (g: Guild) => {
    const l = logOf(g)
    if (l.source === 'off') return t(g.channels.some((c) => c.name?.toLowerCase() === 'logs') ? 'rooms.log.state_off_logs' : 'rooms.log.state_off')
    if (!l.channel_id) return t('rooms.log.state_none')
    return t('rooms.log.state_on', { name: l.channel_name || l.channel_id })
  }
  async function setLog(g: Guild, k: 'channel' | 'deletes' | 'deleted_text' | 'problems', v: string | boolean) {
    const path = ['guilds', g.id, 'log', k]
    const unset = k === 'channel' ? v === 'off' : v === true // the defaults are stored as unset
    await patchConfig([unset ? { path, op: 'unset' } : { path, value: v }], { success: t('rooms.log.saved') })
    void list.refresh()
  }
  let logTesting = $state('')
  async function testLog(g: Guild) {
    logTesting = g.id
    const r = await call('serverlog.test', { guild_id: g.id })
    logTesting = ''
    r.ok ? toast('ok', t('rooms.log.test_ok')) : toast('error', t('rooms.log.test_failed'), r.error?.message)
    void list.refresh()
  }
  let leaveFor = $state<Guild | null>(null)
  let leaveOpen = $state(false)
  async function leave() {
    const g = leaveFor
    if (!g) return
    const r = await call('channels.leave', { guild_id: g.id, confirm: true })
    r.ok ? toast('ok', t('rooms.server.left', { name: g.name })) : toast('error', t('common.failed'), r.error?.message)
    void list.refresh()
  }
  const serverState = (g: Guild) => (!g.allowed ? { tone: 'warn', label: t('rooms.server.pending') } : g.approved ? { tone: 'ok', label: t('rooms.server.approved') } : list.data?.server_approval ? { tone: 'ok', label: t('rooms.server.configured') } : { tone: 'neutral', label: t('rooms.server.open') })
  const pauseChannels = $derived(overview.data?.pause?.channels ?? {})
  const key = (c: any) => `${c.guild_id}:${c.id}`
  function toggle(c: any) {
    const k = key(c)
    const s = new Set(selected)
    s.has(k) ? s.delete(k) : s.add(k)
    selected = s
  }
  function toggleGuild(g: Guild, on: boolean) {
    const s = new Set(selected)
    for (const c of g.channels) on ? s.add(key(c)) : s.delete(key(c))
    selected = s
  }
  // selected rooms in a server that still waits for approval: they only answer once it is approved
  const heldBack = $derived.by(() => {
    const waiting = new Set((list.data?.guilds ?? []).filter((g: any) => g.allowed === false).map((g: any) => g.id))
    return [...selected].filter((k) => waiting.has(k.split(':')[0])).length
  })
  const clearSelection = () => {
    selected = new Set()
    bulkRespond = ''
    bulkPersona = ''
  }
  // bulk: the persona goes in one change for every selected room; the reply mode is per room, one call after the other so config revisions never race
  async function applyBulk(respond: string, persona: string) {
    const keys = [...selected]
    busy = true
    let ok = keys.length
    let failed: string[] = []
    let why = ''
    if (persona) {
      const r = await call('personas.apply', { id: persona, targets: keys })
      if (!r.ok) {
        ok = 0
        failed = keys
        why = r.error?.message ?? ''
      }
    }
    if (respond && ok) {
      ok = 0
      let done = 0
      progress = { done, total: keys.length }
      for (const k of keys) {
        const [g, c] = k.split(':')
        const r = await call('channels.configure', { guild_id: g, channel_id: c, respond })
        if (r.ok) ok++
        else {
          failed.push(k)
          why = r.error?.message ?? why
        }
        progress = { done: ++done, total: keys.length }
      }
      progress = null
    }
    busy = false
    if (ok === keys.length) toast('ok', t('rooms.bulk_done', { n: ok, total: keys.length }))
    else toast(ok ? 'warn' : 'error', t('rooms.bulk_done', { n: ok, total: keys.length }), why || undefined)
    // the rooms that failed stay ticked, so trying again is one press
    selected = new Set(failed)
    if (!failed.length) {
      bulkRespond = ''
      bulkPersona = ''
    }
    void list.refresh()
  }
  // one room's reply switch straight from the table: on means it chats (mention or always); a room left at the
  // default only answers slash commands, so it reads as off. Switching on sets "when mentioned", off is silent.
  let rowBusy = $state<Set<string>>(new Set()) // rooms whose switch is being saved: one change at a time per room
  async function setRespond(c: any, on: boolean) {
    const k = key(c)
    const respond = on ? 'mention' : 'off'
    rowBusy = new Set(rowBusy).add(k)
    const r = await call('channels.configure', { guild_id: c.guild_id, channel_id: c.id, respond })
    if (r.ok) toast('ok', t('rooms.respond_set', { name: c.name, mode: t(`rooms.respond.${respond}`) }))
    else toast('error', t('common.failed'), r.error?.message)
    await list.refresh()
    const s = new Set(rowBusy)
    s.delete(k)
    rowBusy = s
    if (!r.ok) rev++ // the switch moved by itself: put it back to what the server says
  }
  // Escape lets go of the selection (not while a dialog, a menu or the search box has the key)
  function onkeydown(e: KeyboardEvent) {
    if (e.key !== 'Escape' || e.defaultPrevented || !selected.size || busy || addOpen || leaveOpen) return
    const el = e.target as HTMLElement | null
    if (el?.closest('select, input[type="search"]')) return
    clearSelection()
  }
  // one room's persona straight from the table
  async function setPersona(c: any, id: string) {
    if (!id || id === c.persona_ref) return
    const r = await call('personas.apply', { id, guild_id: c.guild_id, channel_id: c.id })
    r.ok ? toast('ok', t('rooms.persona_set', { name: c.name, persona: personaLabel(id) })) : toast('error', t('common.failed'), r.error?.message)
    void list.refresh()
  }
  const personaOptions = $derived(personaOptionsFor((personas.data?.personas ?? []) as any[], i18n.lang))
  async function addRoom() {
    if (!/^\d{5,25}$/.test(add.guild.trim()) || !/^\d{5,25}$/.test(add.channel.trim())) return (addErr = t('setup.room.ids_invalid'))
    busy = true
    const r = await call('channels.configure', { guild_id: add.guild.trim(), channel_id: add.channel.trim(), respond: add.respond })
    busy = false
    if (!r.ok) return (addErr = r.error?.message ?? '')
    addOpen = false
    toast('ok', t('rooms.added'))
    navigate(`/rooms/${add.guild.trim()}/${add.channel.trim()}`)
  }
  const chats = (r: string) => r === 'mention' || r === 'always'
  const respondTone = (r: string) => (!chats(r) ? 'neutral' : r === 'always' ? 'info' : 'ok')
</script>

<svelte:window {onkeydown} />

<PageHeader title={t('rooms.title')} description={t('rooms.desc')}>
  {#snippet actions()}
    <Button data-tour="rooms-add" icon={I.Plus} onclick={() => { addErr = ''; addOpen = true }}>{t('rooms.add')}</Button>
    <Button href="/connections" variant="ghost" icon={I.Bot}>{t('rooms.invite')}</Button>
  {/snippet}
</PageHeader>

<div class="bar">
  <div class="search"><Input type="search" bind:value={q} placeholder={t('rooms.search')} aria-label={t('rooms.search')} /></div>
  {#if list.data}<Status tone={list.data.live ? 'ok' : list.data.discord_enabled ? 'warn' : 'neutral'} label={list.data.live ? t('rooms.gateway_live') : list.data.discord_enabled ? t('rooms.gateway_down') : t('rooms.gateway_off')} />{/if}
  {#if !selected.size}<span class="hint xs muted">{t('rooms.select_hint')}</span>{/if}
</div>

{#if list.data}
  <Card class="servers" title={t('rooms.server.title')} description={t('rooms.server.desc')} data-tour="rooms-servers">
    {#snippet actions()}<Button href="/connections" size="sm" variant="ghost" icon={I.Link}>{t('rooms.invite')}</Button>{/snippet}
    <div class="srv">
      <Switch checked={!!list.data.server_approval} onchange={(v) => access({ server_approval: v }, t('rooms.server.saved'))} label={t('rooms.server.approval')} hint={t('rooms.server.approval_hint')} />
      {#if pending.length}
        <div class="callout warn"><span class="ic"><I.ShieldAlert size={16} /></span><span>{t('rooms.server.pending_note', { n: pending.length })}</span></div>
      {/if}
    </div>
  </Card>
{/if}

{#if !list.data}
  <LoadState res={list}><Card><Skeleton lines={6} height={18} /></Card></LoadState>
{:else if guilds.length === 0}
  <Card><EmptyState pose="guide" title={t('rooms.empty')} description={t('rooms.empty_body')}><Button variant="primary" icon={I.Plus} onclick={() => (addOpen = true)}>{t('rooms.add')}</Button><Button href="/connections" icon={I.Bot}>{t('rooms.connect_discord')}</Button></EmptyState></Card>
{:else}
  <div class="stack">
    {#each guilds as g, gi (g.id)}
      {@const named = g.name && g.name !== g.id}
      <Card padded={false} class="enter">
        <div class="gh" style:--i={gi}>
          <span class="gicon" aria-hidden="true">{#if named}{g.name.slice(0, 1).toUpperCase()}{:else}<I.Server size={16} />{/if}</span>
          <div><h2>{named ? g.name : t('rooms.server.unnamed')}</h2><p class="xs subtle mono">{g.id}{#if g.members} · {t('rooms.server.members', { n: g.members })}{/if}</p></div>
          <Status tone={serverState(g).tone as any} label={serverState(g).label} size="sm" />
          <span class="spacer"></span>
          <span class="small muted">{t('rooms.channel_count', { n: g.channels.length })}</span>
          {#if !g.allowed}
            <Button size="sm" variant="primary" icon={I.Check} onclick={() => access({ approve: g.id }, t('rooms.server.approved_toast', { name: g.name }))}>{t('rooms.server.approve')}</Button>
          {:else if g.approved}
            <Button size="sm" variant="ghost" onclick={() => access({ revoke: g.id }, t('rooms.server.revoked_toast', { name: g.name }))}>{t('rooms.server.revoke')}</Button>
          {/if}
          {#if g.live}<Button size="sm" variant="danger-ghost" icon={I.LogOut} onclick={() => { leaveFor = g; leaveOpen = true }}>{g.allowed ? t('rooms.server.leave') : t('rooms.server.decline')}</Button>{/if}
        </div>
        {#if !g.allowed}<p class="pend small muted">{t('rooms.server.pending_body')}</p>{/if}
        {#if g.blocked_users.length}
          <div class="blocked">
            <span class="xs muted">{t('rooms.server.blocked')}</span>
            {#each g.blocked_users as id (id)}
              <span class="chip"><code class="xs">{id}</code><button type="button" class="x" onclick={() => access({ guild_id: g.id, unblock: id }, t('rooms.server.unblocked'))} aria-label={t('rooms.server.unblock', { id })}><I.X size={12} aria-hidden="true" /></button></span>
            {/each}
          </div>
        {/if}
        {#if g.allowed && cfg.loaded}
          <details class="priv">
            <summary><span class="small">{t('rooms.privacy.title')}</span> <span class="xs muted">{privacyCount(g) ? t('rooms.privacy.n_on', { n: privacyCount(g) }) : t('rooms.privacy.all_off')}</span></summary>
            <div class="priv-body">
              <p class="xs muted">{t('rooms.privacy.desc')}</p>
              {#each PRIVACY as k (k)}
                <Switch checked={privacyOn(g, k)} onchange={(v) => setPrivacy(g, k, v)} label={t(`rooms.privacy.${k}`)} hint={t(`rooms.privacy.${k}.hint`)} />
              {/each}
            </div>
          </details>
          <details class="priv">
            <summary><span class="small">{t('rooms.log.title')}</span> <span class="xs muted">{logSummary(g)}</span></summary>
            <div class="priv-body">
              <p class="xs muted">{t('rooms.log.desc')}</p>
              <Field id="log-{g.id}" label={t('rooms.log.channel')} hint={t('rooms.log.channel_hint')}>
                {#snippet children(f)}<Select id={f.id} aria-describedby={f.describedby} value={logChoice(g)} options={logOptions(g)} onchange={(e) => setLog(g, 'channel', e.currentTarget.value)} />{/snippet}
              </Field>
              {#if logOf(g).missing?.length}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('rooms.log.missing', { perms: logOf(g).missing.map((p: string) => t(`perm.${p}`)).join(', ') })}</span></div>{/if}
              {#if logOf(g).last}<p class="xs muted">{logOf(g).last.ok ? t('rooms.log.last_ok', { at: new Date(logOf(g).last.at).toLocaleString(i18n.lang) }) : t('rooms.log.last_failed', { at: new Date(logOf(g).last.at).toLocaleString(i18n.lang), reason: logOf(g).last.reason })}</p>{/if}
              <Switch checked={logOf(g).deletes !== false} onchange={(v) => setLog(g, 'deletes', v)} label={t('rooms.log.deletes')} hint={t('rooms.log.deletes.hint')} />
              {#if logOf(g).deletes !== false}<Switch checked={logOf(g).deleted_text !== false} onchange={(v) => setLog(g, 'deleted_text', v)} label={t('rooms.log.deleted_text')} hint={t('rooms.log.deleted_text.hint')} />{/if}
              <Switch checked={logOf(g).problems !== false} onchange={(v) => setLog(g, 'problems', v)} label={t('rooms.log.problems')} hint={t('rooms.log.problems.hint')} />
              <div><Button size="sm" icon={I.Send} disabled={!logOf(g).channel_id || !g.live || logTesting === g.id} onclick={() => testLog(g)}>{t('rooms.log.test')}</Button></div>
            </div>
          </details>
        {/if}
        {#if g.channels.length}
        <div use:scrollable class="table-scroll">
          <!-- the same column widths in every server's table, so the columns line up down the page -->
          <table class="table rooms-t" data-tour="rooms-table">
            <colgroup><col class="w-cb" /><col /><col class="w-rsp" /><col class="w-pc" /><col class="w-mem" /><col class="w-st" /><col class="w-act" /></colgroup>
            <thead><tr><th class="cb"><input type="checkbox" checked={g.channels.every((c) => selected.has(key(c)))} indeterminate={g.channels.some((c) => selected.has(key(c))) && !g.channels.every((c) => selected.has(key(c)))} onchange={(e) => toggleGuild(g, e.currentTarget.checked)} aria-label={t('rooms.select_all', { name: g.name })} /></th><th>{t('rooms.col.channel')}</th><th>{t('rooms.respond')}</th><th>{t('rooms.col.persona')}</th><th>{t('rooms.col.memory')}</th><th>{t('rooms.col.state')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {#each g.channels as c (c.id)}
                <tr class:selected={selected.has(key(c))}>
                  <td class="cb"><input type="checkbox" checked={selected.has(key(c))} onchange={() => toggle(c)} aria-label={t('rooms.select_one', { name: c.name })} /></td>
                  <td><a class="chname" href="/rooms/{c.guild_id}/{c.id}"><I.Hash size={14} aria-hidden="true" />{#if c.name && c.name !== c.id}{c.name}{:else}<span class="mono unnamed" title={t('rooms.channel.unnamed')}>{c.id}</span>{/if}</a>{#if !c.configured}<span class="chip">{t('rooms.defaults')}</span>{/if}</td>
                  <td>
                    <div class="rsp">
                      {#key rev}<Switch checked={chats(c.respond)} disabled={busy || rowBusy.has(key(c))} ariaLabel={t('rooms.respond_of', { name: c.name })} onchange={(v) => setRespond(c, v)} />{/key}
                      <Status tone={respondTone(c.respond) as any} label={t(`rooms.respond.${c.respond}`)} size="sm" />
                    </div>
                  </td>
                  <td class="pc"><Select aria-label={t('rooms.persona_of', { name: c.name })} value={c.persona_ref} options={personaOptionsFor((personas.data?.personas ?? []) as any[], roomLangOf(c), c.persona_ref)} onchange={(e) => setPersona(c, e.currentTarget.value)} /></td>
                  <td>{#if c.memory_use === 'on'}<span class="chip accent"><I.Brain size={12} aria-hidden="true" />{t('common.on')}</span>{:else}<span class="chip">{t('common.off')}</span>{/if}</td>
                  <td>
                    {#if pauseChannels[key(c)]}<Status tone="warn" label={t('rooms.paused')} size="sm" />
                    {:else if c.live}<Status tone="ok" label={t('rooms.live')} size="sm" />
                    {:else if c.practice}<Status tone="info" label={t('rooms.practice')} size="sm" />
                    {:else}<Status tone="neutral" label={t('rooms.offline')} size="sm" />{/if}
                  </td>
                  <td class="r"><Button href="/rooms/{c.guild_id}/{c.id}" size="sm" variant="ghost" iconEnd={I.ChevronRight}>{t('rooms.manage')}</Button></td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        {/if}
      </Card>
    {/each}
  </div>
{/if}

{#if selected.size}
  <div class="sel-gap" style:height="{barH + 32}px" aria-hidden="true"></div>
  <div class="sel" role="region" aria-label={t('rooms.bulk_label')} bind:clientHeight={barH}>
    <div class="sel-row">
      <strong class="small">{t('rooms.selected', { n: selected.size })}</strong>
      {#if progress}<span class="small muted" role="status">{t('rooms.bulk_progress', { done: progress.done, total: progress.total })}</span>{/if}
      <span class="spacer"></span>
      <Button size="sm" variant="primary" icon={I.Power} disabled={busy} onclick={() => applyBulk('mention', '')}>{t('rooms.bulk_on')}</Button>
      <Button size="sm" icon={I.X} disabled={busy} onclick={() => applyBulk('off', '')}>{t('rooms.bulk_off')}</Button>
      <Button size="sm" variant="ghost" disabled={busy} onclick={clearSelection}>{t('rooms.bulk_clear')}</Button>
    </div>
    <details class="sel-more">
      <summary class="small">{t('rooms.bulk_more')}</summary>
      <div class="sel-fields">
        <div class="sel-field"><Select aria-label={t('rooms.respond')} bind:value={bulkRespond} disabled={busy} options={[{ value: '', label: t('rooms.bulk_respond_keep') }, ...['always', 'mention', 'command', 'off'].map((v) => ({ value: v, label: t(`rooms.respond.${v}`) }))]} /></div>
        <div class="sel-field"><Select aria-label={t('rooms.col.persona')} bind:value={bulkPersona} disabled={busy} options={[{ value: '', label: t('rooms.bulk_persona_keep') }, ...personaOptions]} /></div>
        <Button size="sm" variant="primary" disabled={busy || (!bulkRespond && !bulkPersona)} onclick={() => applyBulk(bulkRespond, bulkPersona)}>{t('rooms.apply')}</Button>
      </div>
    </details>
    {#if heldBack}<p class="xs muted note">{t('rooms.bulk_pending_note', { n: heldBack })}</p>{/if}
  </div>
{/if}

<Confirm bind:open={leaveOpen} danger title={t('rooms.server.leave_title', { name: leaveFor?.name ?? '' })} body={t('rooms.server.leave_body')} confirmLabel={leaveFor?.allowed ? t('rooms.server.leave') : t('rooms.server.decline')} onconfirm={leave} />

<Dialog bind:open={addOpen} title={t('rooms.add')} description={t('rooms.add_desc')} size="sm">
  <div class="stack">
    <Field id="ag" label={t('setup.room.guild')} hint={t('setup.room.id_hint')} error={addErr || undefined}>
      {#snippet children({ id, describedby, invalid })}<Input {id} bind:value={add.guild} inputmode="numeric" mono aria-describedby={describedby} {invalid} />{/snippet}
    </Field>
    <Field id="ac" label={t('setup.room.channel')}>
      {#snippet children({ id })}<Input {id} bind:value={add.channel} inputmode="numeric" mono />{/snippet}
    </Field>
    <Field id="ar" label={t('rooms.respond')} hint={t(`rooms.respond_hint.${add.respond}`)}>
      {#snippet children({ id })}<Select {id} bind:value={add.respond} options={['always', 'mention', 'command', 'off'].map((v) => ({ value: v, label: t(`rooms.respond.${v}`) }))} />{/snippet}
    </Field>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (addOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={busy} onclick={addRoom}>{t('rooms.add_confirm')}</Button>
  {/snippet}
</Dialog>

<style>
  .rooms-t {
    table-layout: fixed;
    min-width: 880px;
  }
  .rooms-t .w-cb {
    width: 44px;
  }
  .rooms-t .w-rsp {
    width: 160px;
  }
  .rooms-t .w-pc {
    width: 250px;
  }
  .rooms-t .w-mem {
    width: 84px;
  }
  .rooms-t .w-st {
    width: 112px;
  }
  .rooms-t .w-act {
    width: 96px;
  }
  .rooms-t td:nth-child(2) {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  /* a phone keeps the room, its switch and the way in; persona, memory and state are on the room's page */
  @media (max-width: 640px) {
    .rooms-t {
      table-layout: auto;
      min-width: 0;
    }
    .rooms-t col {
      width: auto;
    }
    .rooms-t th:nth-child(4),
    .rooms-t td:nth-child(4),
    .rooms-t th:nth-child(5),
    .rooms-t td:nth-child(5),
    .rooms-t th:nth-child(6),
    .rooms-t td:nth-child(6) {
      display: none;
    }
    .rooms-t td:nth-child(2) {
      white-space: normal;
    }
  }
  .bar {
    display: flex;
    gap: var(--s-3);
    align-items: center;
    flex-wrap: wrap;
    margin-bottom: var(--s-4);
  }
  .search {
    flex: 0 1 320px;
  }
  .hint {
    margin-inline-start: auto;
  }
  .sel {
    position: fixed;
    z-index: 55;
    left: 50%;
    translate: -50% 0;
    bottom: calc(var(--s-4) + env(safe-area-inset-bottom));
    box-sizing: border-box;
    width: max-content;
    max-width: min(880px, calc(100vw - 32px));
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    max-height: 60vh;
    overflow-y: auto;
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
  }
  .sel-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2);
  }
  .sel-more summary {
    cursor: pointer;
    color: var(--text-muted);
  }
  .sel-fields {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2);
    padding-top: var(--s-2);
  }
  .sel-field {
    flex: 1 1 170px;
    max-width: 240px;
  }
  .sel .note {
    margin: 0;
  }
  .rsp {
    display: flex;
    align-items: center;
    gap: var(--s-2);
  }
  .gh {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--s-3);
    padding: var(--s-4) var(--pad-card);
  }
  :global(.servers) {
    margin-bottom: var(--s-4);
  }
  .srv {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
  }
  .pend {
    padding: 0 var(--pad-card) var(--s-3);
  }
  .priv {
    padding: 0 var(--pad-card) var(--s-3);
  }
  .priv summary {
    display: flex;
    align-items: baseline;
    gap: var(--s-2);
    cursor: pointer;
  }
  .priv-body {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding-top: var(--s-3);
  }
  .blocked {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    padding: 0 var(--pad-card) var(--s-3);
  }
  .blocked .chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .x {
    display: grid;
    place-items: center;
    width: 24px;
    height: 24px;
    border: 0;
    border-radius: 999px;
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .x:hover {
    background: var(--surface-3);
    color: var(--text);
  }
  .gicon {
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: 12px;
    background: linear-gradient(in oklab 135deg, var(--accent), var(--chart-4));
    color: #fff;
    font-weight: 700;
  }
  .cb {
    width: 40px;
  }
  .pc {
    min-width: 170px;
    max-width: 220px;
  }
  .cb input {
    width: 16px;
    height: 16px;
    accent-color: var(--accent);
  }
  .chname {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--text);
    font-weight: 600;
    text-decoration: none;
    margin-inline-end: 6px;
  }
  .unnamed {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .chname:hover {
    text-decoration: underline;
  }
</style>
