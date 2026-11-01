<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { toast } from '../lib/toast.svelte'
  import { relative, datetime, num } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import Select from '../components/Select.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Status from '../components/Status.svelte'
  import Switch from '../components/Switch.svelte'
  import Dialog from '../components/Dialog.svelte'
  import Confirm from '../components/Confirm.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import RemindersList from '../components/RemindersList.svelte'
  import OperatorAlerts from '../components/OperatorAlerts.svelte'
  import DMConversation from '../components/DMConversation.svelte'
  import PersonMemory from '../components/PersonMemory.svelte'
  import * as I from '../lib/icons'

  // One row per Discord user the instance keeps personal data for. The console records withdrawals and
  // removes data; agreeing is always the person's own act in Discord.
  const users = resource<any>('users.list', () => ({}), { live: ['config', 'audit'] })
  let q = $state('')
  let filter = $state<'all' | 'subscribed' | 'withdrawn' | 'blocked'>('all')
  let serverFilter = $state('all')
  let serverQuery = $state('')
  $effect(() => {
    const next = q.trim().toLowerCase()
    const timer = setTimeout(() => (serverQuery = next), 250)
    return () => clearTimeout(timer)
  })
  const serverPeople = resource<any>('people.servers', () => (serverQuery ? { q: serverQuery } : {}), { live: ['config', 'audit', 'memory', 'discord'] })
  const serverOptions = $derived([
    { value: 'all', label: t('people.servers.all') },
    ...((serverPeople.data?.servers ?? []) as any[]).map((s) => ({ value: s.guild_id as string, label: (s.name || s.guild_id) as string })),
  ])
  $effect(() => {
    if (serverFilter !== 'all' && !serverOptions.some((s) => s.value === serverFilter)) serverFilter = 'all'
  })
  const serverPeopleCurrent = $derived(serverPeople.data?.query === serverQuery)
  const selectedServerGroups = $derived(((serverPeople.data?.servers ?? []) as any[]).filter((s) => serverFilter === 'all' || s.guild_id === serverFilter))
  const serverTotal = $derived(selectedServerGroups.reduce((n, s) => n + (s.total_count ?? 0), 0))
  const serverShown = $derived(selectedServerGroups.flatMap((s) => (s.people ?? []).map((p: any) => ({ ...p, guild_id: s.guild_id, guild_name: s.name }))))
  // the list area has two tabs: people with consent or settings records, and everyone seen per server
  let tab = $state<'users' | 'seen'>('users')
  let tabPicked = false
  $effect(() => {
    if (!tabPicked && users.data && (users.data.users ?? []).length === 0 && serverTotal > 0) tab = 'seen'
  })
  const GROUP_ROWS = 12 // rows per server before "show more"
  let expanded = $state<Record<string, boolean>>({})
  type Mark = 'granted' | 'default' | 'stale' | 'withdrawn' | 'none'
  function consentKind(c: Consent, scope: string): Mark {
    if (!c || !c.status) return optOut(scope) ? 'default' : 'none'
    if (c.status === 'withdrawn') return 'withdrawn'
    return c.stale ? 'stale' : 'granted'
  }
  const scopes = $derived(((users.data?.policies ?? []) as any[]).map((p) => p.scope as string))
  const optOut = (scope: string) => !!((users.data?.policies ?? []) as any[]).find((p) => p.scope === scope)?.opt_out

  type Consent = { status: string; allows: boolean; stale: boolean; updated_at: string } | undefined
  function consentState(c: Consent, scope: string): { tone: 'ok' | 'warn' | 'neutral' | 'danger'; label: string } {
    if (!c || !c.status) return optOut(scope) ? { tone: 'ok', label: t('people.consent.default_on') } : { tone: 'neutral', label: t('people.consent.none') }
    if (c.status === 'withdrawn') return { tone: 'neutral', label: t('people.consent.withdrawn') }
    if (c.stale) return { tone: 'warn', label: t('people.consent.stale') }
    return { tone: 'ok', label: t('people.consent.granted') }
  }
  const subscribed = (u: any) => !!u.fortune || (u.notices ?? []).length > 0
  const withdrew = (u: any) => Object.values<any>(u.consents ?? {}).some((c) => c.status === 'withdrawn')
  const shown = $derived(((users.data?.users ?? []) as any[]).filter((u) => {
    if (q && !`${u.name ?? ''} ${u.user_id}`.toLowerCase().includes(q.trim().toLowerCase())) return false
    if (filter === 'subscribed') return subscribed(u)
    if (filter === 'withdrawn') return withdrew(u)
    if (filter === 'blocked') return u.blocked
    return true
  }))

  // the person dialog
  let openId = $state('')
  let detailOpen = $state(false)
  const cur = $derived(((users.data?.users ?? []) as any[]).find((u) => u.user_id === openId))
  function open(u: any) {
    openId = u.user_id
    detailOpen = true
  }
  let serverPersonOpen = $state(false)
  let serverPersonKey = $state('')
  // read from the latest list, not kept from the click: a person who withdraws meanwhile leaves the dialog too
  const serverPerson = $derived(serverPersonKey ? (serverShown.find((p: any) => `${p.guild_id}:${p.user_id}` === serverPersonKey) ?? null) : null)
  $effect(() => {
    if (serverPersonOpen && serverPeopleCurrent && !serverPerson) serverPersonOpen = false
  })
  $effect(() => {
    if (!serverPersonOpen) serverPersonKey = ''
  })
  async function openServerPerson(p: any) {
    if (p.has_user && !users.data) await users.refresh()
    const u = ((users.data?.users ?? []) as any[]).find((x) => x.user_id === p.user_id)
    if (p.has_user && u) return open(u)
    serverPersonKey = `${p.guild_id}:${p.user_id}`
    serverPersonOpen = true
  }
  async function run(act: string, input: Record<string, unknown>, ok: string) {
    const r = await call(act, input)
    if (r.ok) toast('ok', ok)
    else toast('error', t('common.failed'), r.error?.message)
    await users.refresh()
    return r.ok
  }
  const withdraw = (scope: string) => run('privacy.consent', { user_id: openId, scope, decision: 'withdraw' }, t('people.withdrawn_toast', { scope: t(`people.scope.${scope}`) }))
  const fortune = (op: 'unsubscribe' | 'delete') => run('fortune.subscribe', { op, user_id: openId }, t(`people.fortune.${op}_done`))
  const notice = (kind: string, op: 'pause' | 'resume' | 'delete') => run('notices.subscribe', { op, kind, user_id: openId }, t(`people.notice.${op}_done`))
  const resetPrefs = () => run('preferences.set', { user_id: openId, reset: true }, t('people.prefs.reset_done'))
  const access = $derived(users.data?.access ?? { operators: [], blocked_users: [], guild_admins: true })
  async function setListed(field: 'operators' | 'blocked_users', id: string, on: boolean) {
    const list = new Set<string>(access[field] ?? [])
    on ? list.add(id) : list.delete(id)
    await run('access.policy', { [field]: [...list] }, t('people.access.saved'))
  }

  // confirmations
  let memOpen = $state(false)
  let eraseOpen = $state(false)
  let fortuneDelOpen = $state(false)
  async function eraseAll() {
    const u = cur
    if (!u) return
    const steps: Promise<unknown>[] = []
    for (const s of scopes) if (s === 'memory' || u.consents?.[s]?.allows) steps.push(call('privacy.consent', { user_id: u.user_id, scope: s, decision: 'withdraw' }))
    if (u.fortune) steps.push(call('fortune.subscribe', { op: 'delete', user_id: u.user_id }))
    for (const n of u.notices ?? []) steps.push(call('notices.subscribe', { op: 'delete', kind: n.kind, user_id: u.user_id }))
    if (u.prefs) steps.push(call('preferences.set', { user_id: u.user_id, reset: true }))
    const res = (await Promise.all(steps)) as any[]
    const failed = res.filter((r) => !r.ok).length
    failed ? toast('warn', t('people.erase_partial', { n: failed })) : toast('ok', t('people.erase_done'))
    await users.refresh()
  }

  // access policy (config `access`)
  let newOp = $state('')
  let newBlock = $state('')
  const idOk = (v: string) => /^\d{5,25}$/.test(v.trim())
  async function addTo(field: 'operators' | 'blocked_users', v: string) {
    if (!idOk(v)) return toast('warn', t('people.access.bad_id'))
    await setListed(field, v.trim(), true)
    if (field === 'operators') newOp = ''
    else newBlock = ''
  }
  const nameOf = (id: string) => ((users.data?.users ?? []) as any[]).find((u) => u.user_id === id)?.name
</script>

<Confirm bind:open={memOpen} danger title={t('people.memory_title')} body={t('people.memory_body')} confirmLabel={t('people.memory_confirm')} onconfirm={() => withdraw('memory')} />
<Confirm bind:open={fortuneDelOpen} danger title={t('people.fortune.delete_title')} body={t('people.fortune.delete_body')} confirmLabel={t('people.fortune.delete')} onconfirm={() => fortune('delete')} />
<Confirm bind:open={eraseOpen} danger title={t('people.erase_title')} body={t('people.erase_body')} confirmLabel={t('people.erase')} requireText={t('people.erase_word')} onconfirm={eraseAll} />

<PageHeader title={t('nav.people')} description={t('people.desc')} />

<div class="layout">
  <div class="main">
    <div class="bar">
      <div class="search"><Input type="search" bind:value={q} placeholder={t('people.search')} aria-label={t('people.search')} /></div>
    </div>
    <Card padded={false} data-tour="people-table">
      <div class="list-tabs"><Tabs id="people" bind:active={tab} onchange={() => (tabPicked = true)} tabs={[
        { id: 'users', label: t('people.tab.users'), icon: I.ShieldCheck, count: users.data ? (users.data.users ?? []).length : undefined },
        { id: 'seen', label: t('people.tab.seen'), icon: I.Users, count: serverPeople.data && serverPeopleCurrent ? serverTotal : undefined }]} /></div>
      {#if tab === 'users'}
        <div id="people-panel-users" role="tabpanel" aria-labelledby="people-tab-users">
          <div class="tools">
            <p class="xs muted grow">{t('people.tab.users_desc')}</p>
            <Segmented size="sm" label={t('people.filter')} bind:value={filter} options={[
              { value: 'all', label: t('people.f.all') }, { value: 'subscribed', label: t('people.f.subscribed') },
              { value: 'withdrawn', label: t('people.f.withdrawn') }, { value: 'blocked', label: t('people.f.blocked') }]} />
          </div>
          {#if !users.data}
            <div class="pad"><LoadState res={users}><Skeleton lines={6} /></LoadState></div>
          {:else if (users.data.users ?? []).length === 0}
            <EmptyState pose="guide" title={t('people.empty')} description={t('people.empty_body')} />
          {:else if shown.length === 0}
            <p class="small muted pad">{t('people.none_match')}</p>
          {:else}
            <div use:scrollable class="table-scroll">
              <table class="table people">
                <thead><tr><th class="sticky">{t('people.col.person')}</th>{#each scopes as s}<th class="c scope" title={t(`people.scope.${s}`)}>{t(`people.scope.${s}`)}</th>{/each}<th>{t('people.col.subs')}</th><th class="r n">{t('people.col.updated')}</th></tr></thead>
                <tbody>
                  {#each shown as u (u.user_id)}
                    <tr class="clickable" onclick={() => open(u)}>
                      <td class="sticky">
                        <button type="button" class="who" onclick={(e) => { e.stopPropagation(); open(u) }}>
                          <span class="name-line"><strong class="ellipsis">{u.name || t('people.unnamed')}</strong>{#if u.operator}<span class="tag accent">{t('people.operator')}</span>{/if}{#if u.blocked}<span class="tag danger">{t('people.blocked')}</span>{/if}</span>
                          <span class="xs subtle mono">{u.user_id}</span>
                        </button>
                      </td>
                      {#each scopes as s}
                        {@const st = consentState(u.consents?.[s], s)}
                        {@const k = consentKind(u.consents?.[s], s)}
                        <td class="c"><span class="mark {k}" title="{t(`people.scope.${s}`)}: {st.label}" role="img" aria-label="{t(`people.scope.${s}`)}: {st.label}">
                          {#if k === 'granted' || k === 'default'}<I.Check size={15} strokeWidth={2.5} aria-hidden="true" />{:else if k === 'stale'}<I.TriangleAlert size={14} aria-hidden="true" />{:else if k === 'withdrawn'}<I.X size={14} strokeWidth={2.25} aria-hidden="true" />{:else}<span aria-hidden="true">·</span>{/if}
                        </span></td>
                      {/each}
                      <td class="small subs">
                        {#if u.fortune}<span class="chip" class:off={!u.fortune.active}>{t('people.scope.fortune')}{u.fortune.time ? ` ${u.fortune.time}` : ''}</span>{/if}
                        {#each u.notices ?? [] as n}<span class="chip" class:off={!n.active}>{t(`people.notice.kind.${n.kind}`)}</span>{/each}
                        {#if u.reminders}<span class="chip">{t('rem.count', { n: u.reminders })}</span>{/if}
                        {#if !subscribed(u) && !u.reminders}<span class="subtle">–</span>{/if}
                      </td>
                      <td class="r n xs muted">{u.updated_at ? relative(u.updated_at) : '–'}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
            <p class="legend xs muted">
              <span><span class="mark granted"><I.Check size={13} strokeWidth={2.5} aria-hidden="true" /></span>{t('people.consent.granted')}</span>
              <span><span class="mark default"><I.Check size={13} strokeWidth={2.5} aria-hidden="true" /></span>{t('people.consent.default_on')}</span>
              <span><span class="mark stale"><I.TriangleAlert size={12} aria-hidden="true" /></span>{t('people.consent.stale')}</span>
              <span><span class="mark withdrawn"><I.X size={12} strokeWidth={2.25} aria-hidden="true" /></span>{t('people.consent.withdrawn')}</span>
              <span><span class="mark none">·</span>{t('people.consent.none')}</span>
            </p>
          {/if}
        </div>
      {:else}
        <div id="people-panel-seen" role="tabpanel" aria-labelledby="people-tab-seen">
          <div class="tools">
            <p class="xs muted grow">{t('people.servers.desc')}{#if serverPeople.data && serverPeopleCurrent && selectedServerGroups.some((s) => s.has_more)} {t('people.servers.top_limit', { n: serverPeople.data.limit })}{/if}</p>
            <div class="server-select"><Select id="people-server" aria-label={t('people.servers.selector')} bind:value={serverFilter} options={serverOptions} /></div>
          </div>
          {#if !serverPeople.data}
            <div class="pad"><LoadState res={serverPeople}><Skeleton lines={6} /></LoadState></div>
          {:else if !serverPeopleCurrent}
            <div class="pad"><Skeleton lines={6} /></div>
          {:else if serverShown.length === 0}
            <EmptyState compact pose="guide" title={t('people.servers.empty')} description={t('people.servers.empty_body')} />
          {:else}
            <div use:scrollable class="table-scroll">
              <table class="table people">
                <thead><tr><th>{t('people.col.person')}</th><th class="r n">{t('people.servers.col.messages')}</th><th class="r n">{t('people.servers.col.last_seen')}</th><th class="r n">{t('people.servers.col.cards')}</th></tr></thead>
                {#each selectedServerGroups as g (g.guild_id)}
                  {@const all = (g.people ?? []) as any[]}
                  {@const list = expanded[g.guild_id] ? all : all.slice(0, GROUP_ROWS)}
                  <tbody>
                    {#if selectedServerGroups.length > 1 || serverFilter === 'all'}
                      <tr class="group"><th colspan="4" scope="colgroup"><I.Server size={14} aria-hidden="true" />&nbsp; {g.name || g.guild_id}<span class="xs muted num">{t('people.servers.group_count', { n: num(g.total_count ?? all.length) })}</span></th></tr>
                    {/if}
                    {#each list as p (p.user_id)}
                      {@const row = { ...p, guild_id: g.guild_id, guild_name: g.name }}
                      <tr class="clickable" onclick={() => openServerPerson(row)}>
                        <td>
                          <button type="button" class="who" onclick={(e) => { e.stopPropagation(); openServerPerson(row) }} aria-label={t('people.servers.open', { name: p.name, id: p.user_id })}>
                            <span class="name-line"><strong class="ellipsis">{p.name || t('people.unnamed')}</strong>{#if p.has_user}<span class="tag info" title={t('people.servers.users_row_hint')}>{t('people.servers.users_row')}</span>{/if}</span>
                            <span class="xs subtle"><span class="mono">{p.user_id}</span>{#if p.other_names?.length}&nbsp;· {t('people.servers.other_names', { names: p.other_names.join(', ') })}{/if}</span>
                          </button>
                        </td>
                        <td class="r n num small">{num(p.messages)}</td>
                        <td class="r n xs muted">{p.last_seen ? relative(p.last_seen) : '–'}</td>
                        <td class="r n num small">{#if p.card_items}{num(p.card_items)}{:else}<span class="subtle">–</span>{/if}</td>
                      </tr>
                    {/each}
                    {#if all.length > GROUP_ROWS}
                      <tr class="more-row"><td colspan="4"><Button size="sm" variant="ghost" icon={I.ChevronDown} onclick={() => (expanded[g.guild_id] = !expanded[g.guild_id])}>{expanded[g.guild_id] ? t('people.servers.less') : t('people.servers.more', { n: num(all.length - GROUP_ROWS) })}</Button></td></tr>
                    {/if}
                  </tbody>
                {/each}
              </table>
            </div>
          {/if}
        </div>
      {/if}
    </Card>
    <p class="xs muted">{t('people.note')}</p>
  </div>

  <aside class="side">
    <Card title={t('people.access.title')} description={t('people.access.desc')} data-tour="people-access">
      <div class="stack">
        <Switch checked={!!access.guild_admins} onchange={(v) => run('access.policy', { guild_admins: v }, t('people.access.saved'))} label={t('people.access.guild_admins')} hint={t('people.access.guild_admins_hint')} />
        <div>
          <h3 class="small h">{t('people.access.operators')}</h3>
          <p class="xs muted">{t('people.access.operators_hint')}</p>
          <ul class="ids">
            {#each access.operators ?? [] as id (id)}
              <li><span class="small">{nameOf(id) ?? ''}</span><code class="xs">{id}</code><Button size="sm" variant="danger-ghost" icon={I.X} onclick={() => setListed('operators', id, false)} aria-label={t('people.access.remove', { id })}></Button></li>
            {/each}
          </ul>
          <form class="row" onsubmit={(e) => { e.preventDefault(); void addTo('operators', newOp) }}>
            <div class="grow"><Input bind:value={newOp} inputmode="numeric" placeholder={t('people.access.id_ph')} aria-label={t('people.access.operators')} /></div>
            <Button type="submit" size="sm" icon={I.Plus}>{t('people.access.add')}</Button>
          </form>
        </div>
        <div>
          <h3 class="small h">{t('people.access.blocked')}</h3>
          <p class="xs muted">{t('people.access.blocked_hint')}</p>
          <ul class="ids">
            {#each access.blocked_users ?? [] as id (id)}
              <li><span class="small">{nameOf(id) ?? ''}</span><code class="xs">{id}</code><Button size="sm" variant="danger-ghost" icon={I.X} onclick={() => setListed('blocked_users', id, false)} aria-label={t('people.access.remove', { id })}></Button></li>
            {/each}
          </ul>
          <form class="row" onsubmit={(e) => { e.preventDefault(); void addTo('blocked_users', newBlock) }}>
            <div class="grow"><Input bind:value={newBlock} inputmode="numeric" placeholder={t('people.access.id_ph')} aria-label={t('people.access.blocked')} /></div>
            <Button type="submit" size="sm" icon={I.Ban}>{t('people.access.block')}</Button>
          </form>
        </div>
      </div>
    </Card>
    <DMConversation />
    <OperatorAlerts />
    <Card title={t('people.policies.title')} description={t('people.policies.desc')}>
      <ul class="pol">
        {#each users.data?.policies ?? [] as p (p.scope)}
          <li><strong class="small">{t(`people.scope.${p.scope}`)}</strong><span class="xs muted">{p.opt_out ? t('people.policies.opt_out') : t('people.policies.opt_in')} · {p.version}</span></li>
        {/each}
      </ul>
    </Card>
  </aside>
</div>

<Dialog bind:open={detailOpen} size="lg" title={cur ? cur.name || t('people.unnamed') : ''} description={cur ? t('people.detail_desc') : ''}>
  {#if cur}
    <div class="stack">
      <div class="row"><code class="small">{cur.user_id}</code><CopyButton text={cur.user_id} /></div>
      <section>
        <h3 class="small h">{t('people.consents')}</h3>
        <ul class="rows">
          {#each scopes as s}
            {@const c = cur.consents?.[s]}
            {@const st = consentState(c, s)}
            <li>
              <span class="small grow">{t(`people.scope.${s}`)}</span>
              <Status tone={st.tone} label={st.label} size="sm" />
              {#if c?.updated_at}<span class="xs muted nowrap">{datetime(c.updated_at)}</span>{/if}
              {#if (c ? c.allows : optOut(s))}
                <Button size="sm" variant="danger-ghost" onclick={() => (s === 'memory' ? (memOpen = true) : withdraw(s))}>{t('people.withdraw')}</Button>
              {/if}
            </li>
          {/each}
        </ul>
        <p class="xs muted">{t('people.grant_self')}</p>
      </section>
      <section>
        <h3 class="small h">{t('people.subscriptions')}</h3>
        {#if !cur.fortune && !(cur.notices ?? []).length}<p class="small muted">{t('people.no_subs')}</p>{/if}
        <ul class="rows">
          {#if cur.fortune}
            <li>
              <span class="small grow">{t('people.scope.fortune')} · {cur.fortune.time ? t('people.fortune.at', { time: cur.fortune.time }) : t('people.fortune.no_time')}</span>
              <Status tone={cur.fortune.active ? 'ok' : 'neutral'} label={cur.fortune.active ? t('people.sub.active') : t('people.sub.paused')} size="sm" />
              {#if cur.fortune.active}<Button size="sm" variant="ghost" onclick={() => fortune('unsubscribe')}>{t('people.fortune.unsubscribe')}</Button>{/if}
              <Button size="sm" variant="danger-ghost" onclick={() => (fortuneDelOpen = true)}>{t('people.fortune.delete')}</Button>
            </li>
          {/if}
          {#each cur.notices ?? [] as n (n.kind)}
            <li>
              <span class="small grow">{t(`people.notice.kind.${n.kind}`)}{n.time ? ` · ${n.time}` : ''}</span>
              <Status tone={n.active ? 'ok' : 'neutral'} label={n.active ? t('people.sub.active') : t('people.sub.paused')} size="sm" />
              {#if n.active}<Button size="sm" variant="ghost" onclick={() => notice(n.kind, 'pause')}>{t('people.notice.pause')}</Button>
              {:else}<Button size="sm" variant="ghost" onclick={() => notice(n.kind, 'resume')}>{t('people.notice.resume')}</Button>{/if}
              <Button size="sm" variant="danger-ghost" onclick={() => notice(n.kind, 'delete')}>{t('people.notice.delete')}</Button>
            </li>
          {/each}
        </ul>
      </section>
      <section>
        <h3 class="small h">{t('rem.title')}</h3>
        <RemindersList scope={{ user_id: cur.user_id }} empty={t('rem.person_empty')} />
      </section>
      <section>
        <h3 class="small h">{t('pm.title')}</h3>
        <p class="xs muted">{t('pm.desc')}</p>
        <PersonMemory userId={cur.user_id} />
      </section>
      <section>
        <h3 class="small h">{t('people.prefs.title')}</h3>
        {#if cur.prefs}
          <KeyValue items={[
            { k: t('people.prefs.weather_region'), v: cur.prefs.weather_region },
            { k: t('people.prefs.timezone'), v: cur.prefs.timezone },
            { k: t('people.prefs.quiet'), v: cur.prefs.quiet_start ? `${cur.prefs.quiet_start}–${cur.prefs.quiet_end}` : '' },
            { k: t('people.prefs.locale'), v: cur.prefs.locale }]} />
          <div style:margin-top="8px"><Button size="sm" variant="ghost" icon={I.RotateCcw} onclick={resetPrefs}>{t('people.prefs.reset')}</Button></div>
        {:else}<p class="small muted">{t('people.prefs.none')}</p>{/if}
      </section>
      <section>
        <h3 class="small h">{t('people.access.title')}</h3>
        <div class="stack">
          <Switch checked={cur.blocked} onchange={(v) => setListed('blocked_users', cur.user_id, v)} label={t('people.access.block_person')} hint={t('people.access.blocked_hint')} />
          <Switch checked={cur.operator} onchange={(v) => setListed('operators', cur.user_id, v)} label={t('people.access.operator_person')} hint={t('people.access.operators_hint')} />
        </div>
      </section>
      <section class="danger-zone">
        <h3 class="small h">{t('people.erase_title')}</h3>
        <p class="xs muted">{t('people.erase_hint')}</p>
        <Button variant="danger" icon={I.Trash2} onclick={() => (eraseOpen = true)}>{t('people.erase')}</Button>
      </section>
    </div>
  {/if}
</Dialog>

<Dialog bind:open={serverPersonOpen} size="sm" title={serverPerson?.name ?? ''} description={t('people.servers.readonly')}>
  {#if serverPerson}
    <div class="stack">
      <code class="small">{serverPerson.user_id}</code>
      {#if serverPerson.other_names?.length}<p class="small muted">{t('people.servers.other_names', { names: serverPerson.other_names.join(' · ') })}</p>{/if}
      <KeyValue items={[
        { k: t('people.servers.col.server'), v: serverPerson.guild_name || serverPerson.guild_id },
        { k: t('people.servers.col.messages'), v: num(serverPerson.messages) },
        { k: t('people.servers.first_seen'), v: datetime(serverPerson.first_seen) },
        { k: t('people.servers.col.last_seen'), v: datetime(serverPerson.last_seen) },
        { k: t('people.servers.col.cards'), v: num(serverPerson.card_items) },
      ]} />
    </div>
  {/if}
</Dialog>

<style>
  .layout {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr) minmax(0, 340px);
    align-items: start;
  }
  @media (max-width: 1000px) {
    .layout {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .main,
  .side {
    display: grid;
    gap: var(--s-4);
    min-width: 0;
  }
  .bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2);
    align-items: center;
  }
  .search {
    flex: 1;
    min-width: 200px;
  }
  .list-tabs {
    padding: var(--s-2) var(--s-4) 0;
  }
  .tools {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-3);
    padding: var(--s-3) var(--s-4);
  }
  .tools p {
    margin: 0;
    min-width: 200px;
  }
  .pad {
    padding: var(--s-4);
  }
  .people .c {
    text-align: center;
    width: 1%;
  }
  .people th.scope {
    white-space: nowrap;
    font-size: var(--text-xs);
  }
  .people .r {
    text-align: end;
  }
  .people tr.group th {
    position: static;
    height: 32px;
    background: var(--surface-2);
    font-weight: 600;
    text-align: start;
    font-size: var(--text-sm);
  }
  .people tr.group th span {
    margin-inline-start: 8px;
    font-weight: 400;
  }
  .people tr.group :global(svg) {
    vertical-align: -2px;
    color: var(--text-muted);
  }
  .more-row td {
    padding-block: 4px;
  }
  .name-line {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    max-width: 260px;
    min-width: 0;
  }
  .tag {
    flex: none;
    padding: 0 6px;
    border-radius: 999px;
    font-size: var(--text-xs);
    line-height: 18px;
    font-weight: 500;
    background: var(--surface-2);
    color: var(--text-muted);
  }
  .tag.accent {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .tag.info {
    background: var(--info-soft);
    color: var(--info);
  }
  .tag.danger {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .mark {
    display: inline-grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border-radius: 999px;
    color: var(--text-subtle);
    line-height: 1;
  }
  .mark.granted {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .mark.default {
    color: var(--ok);
    opacity: 0.55;
  }
  .mark.stale {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .mark.withdrawn {
    color: var(--text-muted);
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 4px var(--s-3);
    margin: 0;
    padding: var(--s-3) var(--s-4);
    border-top: 1px solid var(--border);
  }
  .legend > span {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .legend .mark {
    width: 18px;
    height: 18px;
  }
  .subs .chip {
    margin-block: 1px;
  }
  .sticky {
    position: sticky;
    inset-inline-start: 0;
    background: var(--surface);
    z-index: 1;
  }
  thead .sticky {
    z-index: 2;
  }
  @media (hover: hover) {
    .people tbody tr:hover .sticky {
      background: var(--surface-2);
    }
  }
  .people td.n,
  .people th.n {
    width: 1%;
    white-space: nowrap;
    padding-inline: var(--s-4);
  }
  @media (max-width: 640px) {
    .name-line {
      max-width: 150px;
    }
  }
  .server-select {
    width: min(100%, 260px);
  }
  .who {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
    max-width: 100%;
    padding: 0;
    border: 0;
    background: none;
    color: var(--text);
    text-align: start;
    cursor: pointer;
  }
  .who:hover strong {
    text-decoration: underline;
  }
  .chip.off {
    opacity: 0.6;
    text-decoration: line-through;
  }
  .chip + .chip {
    margin-inline-start: 4px;
  }
  .h {
    margin: 0 0 6px;
    font-weight: 600;
  }
  .ids,
  .rows,
  .pol {
    list-style: none;
    margin: 8px 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .ids li,
  .rows li {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .ids li code {
    flex: 1;
  }
  .pol li {
    display: flex;
    flex-direction: column;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .danger-zone {
    padding-top: var(--s-3);
    border-top: 1px solid var(--border);
  }
</style>
