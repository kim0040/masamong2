<script lang="ts">
  import { untrack } from 'svelte'
  import { has, t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { latest } from '../lib/seqload'
  import { resource } from '../lib/resource.svelte'
  import { datetime, num } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { splitMine, forgetTotal, personTotal, memberServers, pickServer, claimSummary, sortClaims, isDirect, type Claim } from '../lib/personmemory'
  import Field from './Field.svelte'
  import Select from './Select.svelte'
  import Button from './Button.svelte'
  import Confirm from './Confirm.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import { useRooms } from '../lib/rooms.svelte'
  import * as I from '../lib/icons'

  // What the bot keeps about one person in one server (memory.mine.show), hiding one statement
  // (memory.person.hide) and forgetting it all: a preview first, then a confirmation with the preview's
  // token (memory.mine.forget). Each server keeps its own, so the operator picks the server, among the ones
  // the person is in. The person is named by the dialog this sits in; the ID is not shown.
  const names = useRooms()
  let { userId, preferredGuild = '' }: { userId: string; preferredGuild?: string } = $props()

  // Exact membership is filtered before the per-server list cap.
  type Group = { guild_id: string; name: string; people: { user_id: string; last_seen: string }[] }
  const membership = resource<{ servers: Group[] }>('people.servers', () => ({ user_id: userId }), { live: ['config', 'audit', 'memory', 'discord'] })
  const servers = $derived(memberServers(membership.data?.servers, userId))
  let guild = $state('')
  $effect(() => {
    guild = pickServer(servers, guild, preferredGuild)
  })

  let view = $state<{ loading: boolean; error: string; data: any } | null>(null)
  const loads = latest()
  async function load() {
    const live = loads.start()
    if (!guild || !userId) return void (view = null)
    view = { loading: true, error: '', data: null }
    const r = await call('memory.mine.show', { guild_id: guild, user_id: userId })
    if (!live()) return
    view = r.ok ? { loading: false, error: '', data: r.output } : { loading: false, error: r.error?.message ?? t('common.failed'), data: null }
  }
  $effect(() => {
    void guild
    void userId
    untrack(() => void load())
  })

  const parts = $derived(splitMine(view?.data?.items))
  const claims = $derived(sortClaims((view?.data?.person ?? []) as Claim[]))
  const sum = $derived(claimSummary(claims))
  const directClaims = $derived(claims.filter((c) => isDirect(c)))
  const heardClaims = $derived(claims.filter((c) => !isDirect(c)))
  const episodes = $derived<number>(view?.data?.episodes ?? 0)
  const empty = $derived(!!view?.data && parts.told.length + parts.other.length + claims.length + episodes === 0)
  const itemKind = (k: string) => (has(`rm.item.${k}`) ? t(`rm.item.${k}`) : k)
  const claimKind = (k: string) => (has(`pm.kind.${k}`) ? t(`pm.kind.${k}`) : k)
  function source(c: Claim): string {
    const name = c.said_by?.[0] ?? ''
    if (c.origin === 'said') return name ? t('pm.src.said', { name }) : t('pm.src.said_unknown')
    if (c.origin === 'told') return name ? t('pm.src.told', { name }) : t('pm.src.told_unknown')
    return c.origin === 'explicit' ? t('pm.src.explicit') : t('pm.src.self')
  }

  // hiding one statement: it stays out of every answer and is not made again from the same lines
  let hideFor = $state<Claim | null>(null)
  let hideOpen = $state(false)
  function askHide(c: Claim) {
    hideFor = c
    hideOpen = true
  }
  async function hide() {
    if (!hideFor) return
    const r = await call('memory.person.hide', { guild_id: guild, user_id: userId, claim_id: hideFor.id })
    if (!r.ok) toast('error', t('common.failed'), r.error?.message)
    else toast('ok', t('pm.hid'))
    await load()
  }

  // forgetting: the preview opens the confirmation, the token it returns confirms exactly that
  let plan = $state<{ items: number; episodes: number; cards: number; person_items?: number; token: string } | null>(null)
  let confirmOpen = $state(false)
  let previewing = $state(false)
  async function preview() {
    previewing = true
    const r = await call('memory.mine.forget', { guild_id: guild, user_id: userId, scope: 'all' })
    previewing = false
    if (!r.ok) return void toast('error', t('common.failed'), r.error?.message)
    plan = r.output
    if (forgetTotal(plan) === 0) return void toast('info', t('pm.forget_nothing'))
    confirmOpen = true
  }
  async function forget() {
    if (!plan) return
    const r = await call('memory.mine.forget', { guild_id: guild, user_id: userId, scope: 'all', confirm: plan.token })
    if (!r.ok) toast('error', t('common.failed'), r.error?.message)
    else if (r.output.applied) toast('ok', t('pm.forgot', { n: num(r.output.deleted ?? 0) }))
    else toast('warn', t('pm.stale'))
    await load()
  }
</script>

<div class="pm">
  {#if !membership.data}
    <LoadState res={membership}><Skeleton lines={3} /></LoadState>
  {:else if servers.length === 0}
    <p class="small muted">{t('pm.no_servers')}</p>
  {:else}
    <Field id="pm-server" label={t('pm.server')}>
      {#snippet children({ id })}<Select {id} bind:value={guild} options={servers} disabled={servers.length === 0} />{/snippet}
    </Field>
    <div aria-live="polite" class="out">
      {#if !view || (view.loading && !view.data)}
        <Skeleton lines={3} />
      {:else if view.error}
        <div role="alert" class="callout danger"><span class="ic"><I.CircleAlert size={16} aria-hidden="true" /></span><span>{view.error}</span><Button size="sm" variant="ghost" onclick={() => load()}>{t('common.retry')}</Button></div>
      {:else if empty}
        <div class="empty small muted">
          <p>{t('pm.empty')}</p>
          <ul>
            <li>{t('pm.empty_off')}</li>
            <li>{t('pm.empty_quiet')}</li>
          </ul>
        </div>
      {:else}
        {#if claims.length}
          <section aria-labelledby="pm-claims">
            <h4 id="pm-claims" class="sub">{t('pm.claims')}</h4>
            <p class="small sum">{t('pm.summary', { own: num(sum.own), others: num(sum.others), told: num(sum.told) })} · {t('pm.last', { when: datetime(sum.last) })}</p>
            <p class="xs muted sum">{t('pm.hidden_note')}</p>
            {#each [{ id: 'direct', list: directClaims }, { id: 'heard', list: heardClaims }] as grp (grp.id)}
              {#if grp.list.length}
                <h5 class="grp">{t(`pm.group.${grp.id}`)}<span class="xs muted num"> · {num(grp.list.length)}</span></h5>
                <ul class="claims">
                  {#each grp.list as c (c.id)}
                    <li>
                      <div class="main">
                        <span class="chip">{claimKind(c.kind)}</span>
                        {#if c.state === 'pending'}<span class="tier" title={t('pm.pending_hint')}>{t('pm.pending')}</span>{/if}
                        <span class="break">{c.text}</span>
                      </div>
                      <div class="meta xs muted"><span>{source(c)}</span>{#if c.room_id}<span>·</span><span title={names.name(c.room_id).server}>{names.name(c.room_id).label}</span>{/if}<span>·</span><span>{datetime(c.observed_at)}</span></div>
                      <Button size="sm" variant="ghost" icon={I.EyeOff} aria-label={t('pm.hide_one', { text: c.text })} onclick={() => askHide(c)}>{t('pm.hide')}</Button>
                    </li>
                  {/each}
                </ul>
              {/if}
            {/each}
          </section>
        {/if}
        {#if parts.told.length + parts.other.length}
          <details class="more">
            <summary class="small">{t('pm.items_more', { n: num(parts.told.length + parts.other.length) })}</summary>
            {#if parts.told.length}
              <section aria-labelledby="pm-told">
                <h4 id="pm-told" class="sub">{t('pm.told')}</h4>
                <ul class="list">
                  {#each parts.told as it (it.id)}<li><span class="break">{it.text}</span> <span class="xs muted">{datetime(it.created_at)}</span></li>{/each}
                </ul>
              </section>
            {/if}
            {#if parts.other.length}
              <section aria-labelledby="pm-items">
                <h4 id="pm-items" class="sub">{t('pm.items')}</h4>
                <ul class="list">
                  {#each parts.other as it (it.id)}
                    <li><span class="chip">{itemKind(it.kind)}</span> <span class="break">{it.text}</span> <span class="xs muted">{datetime(it.created_at)}</span></li>
                  {/each}
                </ul>
              </section>
            {/if}
          </details>
        {/if}
        <p class="xs muted">{t('pm.episodes', { n: num(episodes) })}</p>
      {/if}
    </div>
    <div class="act">
      <Button size="sm" variant="danger-ghost" icon={I.Trash2} loading={previewing} disabled={!guild || !!view?.loading || !!view?.error || empty} onclick={preview}>{t('pm.forget')}</Button>
    </div>
  {/if}
</div>

<Confirm bind:open={hideOpen} title={t('pm.hide_title')} body={t('pm.hide_body')} confirmLabel={t('pm.hide')} onconfirm={hide}>
  {#snippet details()}
    {#if hideFor}<p class="fp small break">{hideFor.text}</p>{/if}
  {/snippet}
</Confirm>

<Confirm bind:open={confirmOpen} danger title={t('pm.forget_title')} body={t('pm.forget_body')} confirmLabel={t('pm.forget_confirm')} onconfirm={forget}>
  {#snippet details()}
    {#if plan}<p class="fp small">{t('pm.forget_preview', { items: num(plan.items), episodes: num(plan.episodes), cards: num(personTotal(plan)) })}</p>{/if}
  {/snippet}
</Confirm>

<style>
  .pm {
    display: grid;
    gap: var(--s-3);
    min-width: 0;
  }
  .out {
    display: grid;
    gap: var(--s-3);
    min-width: 0;
  }
  .sub {
    margin: 0 0 6px;
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text-muted);
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .list li {
    font-size: var(--text-sm);
    min-width: 0;
  }
  .sum {
    margin: 0 0 var(--s-2);
    color: var(--text-muted);
  }
  .claims {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
  }
  .claims li {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    column-gap: var(--s-3);
    align-items: start;
    padding: var(--s-2) 0;
    box-shadow: inset 0 -1px 0 var(--border);
    font-size: var(--text-sm);
    min-width: 0;
  }
  .claims .main {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 6px;
    min-width: 0;
  }
  .claims .meta {
    grid-column: 1;
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .claims li > :global(button) {
    grid-column: 2;
    grid-row: 1 / span 2;
  }
  .grp {
    margin: var(--s-3) 0 var(--s-1);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--text-muted);
  }
  .tier {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .more summary {
    cursor: pointer;
    color: var(--text-muted);
    margin-bottom: var(--s-2);
  }
  .empty p {
    margin: 0 0 6px;
  }
  .empty ul {
    margin: 0;
    padding-left: 1.2em;
    display: grid;
    gap: 4px;
  }
  .fp {
    margin: var(--s-3) 0 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
  }
</style>
