<script lang="ts">
  import { untrack } from 'svelte'
  import { has, t } from '../lib/i18n.svelte'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { relative, datetime, num, usd } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { budgetView, parseSource, roomValue, parseRoomValue, listInput, filterEpisodes, distinct, forgetPreview, statusTone, EPISODE_KINDS, EPISODE_STATUSES } from '../lib/roommemory'
  import Card from './Card.svelte'
  import Field from './Field.svelte'
  import Select from './Select.svelte'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Progress from './Progress.svelte'
  import Confirm from './Confirm.svelte'
  import EmptyState from './EmptyState.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Room memory (ADR-17): the compressed state of a room (L1) and the episodes made from its
  // conversations (L2). Both are short derived sentences; each carries the messages it came from, which
  // are read from the store when shown, so a deleted message disappears from here as well.
  const channels = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const blocks = resource<any>('memory.blocks.list', () => ({}), { live: ['memory'] })

  const rooms = $derived.by(() => {
    const guildName = new Map<string, string>(((channels.data?.guilds ?? []) as any[]).map((g) => [g.id as string, (g.name || g.id) as string]))
    const out: { value: string; label: string; group: string }[] = []
    for (const c of (channels.data?.channels ?? []) as any[]) out.push({ value: roomValue('discord', c.guild_id, c.id), label: `# ${c.name || c.id}`, group: guildName.get(c.guild_id) ?? c.guild_id })
    for (const b of (blocks.data?.blocks ?? []) as any[]) if (b.platform === 'kakao' && (b.message_count ?? 0) > 0) out.push({ value: roomValue('kakao', b.source_room), label: b.label || b.source_room, group: t('rm.group_kakao') })
    return out
  })
  let picked = $state('')
  $effect(() => {
    if (!picked && rooms.length) picked = rooms[0].value
  })
  const pick = $derived(parseRoomValue(picked))
  const blockName = (id: string) => ((blocks.data?.blocks ?? []) as any[]).find((b) => b.block_id === id)?.label as string | undefined

  const eps = resource<any>('memory.episodes.list', () => listInput(pick), { live: ['memory'] })
  const budget = $derived(budgetView(eps.data?.budget))
  const all = $derived((eps.data?.episodes ?? []) as any[])
  let kind = $state('')
  let status = $state('')
  const shown = $derived(filterEpisodes(all, { kind, status }))
  const kindOptions = $derived(distinct(all, 'kind', EPISODE_KINDS))
  const statusOptions = $derived(distinct(all, 'status', EPISODE_STATUSES))
  const label = (ns: string, v: string) => (has(`${ns}.${v}`) ? t(`${ns}.${v}`) : v)

  // L1 state: Discord channels only
  let st = $state<{ loading: boolean; error: string; state: any; at: string } | null>(null)
  let stSeq = 0
  async function loadState() {
    const p = pick
    if (p?.kind !== 'discord') return void (st = null)
    const my = ++stSeq
    st = { loading: true, error: '', state: st?.state ?? null, at: st?.at ?? '' }
    const r = await call('memory.state.show', { guild_id: p.guild_id, channel_id: p.channel_id })
    if (my !== stSeq) return
    st = r.ok ? { loading: false, error: '', state: r.output.state ?? null, at: r.output.updated_at ?? '' } : { loading: false, error: r.error?.message ?? t('common.failed'), state: null, at: '' }
  }
  $effect(() => {
    void picked
    // loadState reads and writes st: only the picked room is a dependency
    untrack(() => void loadState())
  })
  $effect(() => {
    // the room moved on: the open episode belongs to the old list
    void picked
    open = ''
  })
  const stateEmpty = $derived(!st?.state || ((st.state.threads ?? []).length + (st.state.open_questions ?? []).length === 0 && !st.state.mood && !st.state.banter))

  // one episode open at a time, its items and evidence read live
  let open = $state('')
  let detail = $state<{ loading: boolean; error: string; items: any[] } | null>(null)
  let dSeq = 0
  async function toggle(id: string) {
    if (open === id) return void (open = '')
    open = id
    detail = { loading: true, error: '', items: [] }
    const my = ++dSeq
    const r = await call('memory.episodes.show', { id })
    if (my !== dSeq) return
    detail = r.ok ? { loading: false, error: '', items: r.output.items ?? [] } : { loading: false, error: r.error?.message ?? t('common.failed'), items: [] }
  }

  // forget one episode
  let forgetFor = $state<any>(null)
  let forgetOpen = $state(false)
  const preview = $derived(forgetPreview(forgetFor ?? {}))
  async function forget() {
    const r = await call('memory.episodes.forget', { id: forgetFor.id })
    if (!r.ok) {
      toast('error', t('common.failed'), r.error?.message)
      return
    }
    toast('ok', t('rm.forgot'))
    open = ''
    void eps.refresh()
  }
  const sourceText = (s: string) => {
    const p = parseSource(s)
    if (p.kind === 'kakao') return blockName(p.block) ? t('rm.source.kakao', { block: blockName(p.block) as string }) : t('rm.source.kakao_plain')
    return p.kind === 'discord' ? t('rm.source.discord') : s
  }
  const budgetNote: Record<string, string> = { ok: 'rm.budget.ok_hint', state_paused: 'rm.budget.paused_hint', stopped: 'rm.budget.stopped_hint', off: 'rm.budget.off_hint' }
  const budgetTone = { ok: 'ok', warn: 'warn', danger: 'danger', neutral: 'neutral' } as const
</script>

<div class="rm">
  <Card title={t('rm.budget_title')} description={t('rm.budget_desc')}>
    {#if !eps.data}
      <LoadState res={eps}><Skeleton lines={2} /></LoadState>
    {:else}
      <div class="stack">
        <div class="row between">
          <span class="num spend">{budget.state === 'unknown' ? '–' : t('rm.budget_of', { spent: usd(budget.spent), limit: usd(budget.limit) })}</span>
          {#if budget.state !== 'unknown'}<Status tone={budgetTone[budget.tone]} label={t(`rm.budget.${budget.state}`)} size="sm" />{/if}
        </div>
        {#if budget.limit > 0}<Progress value={budget.spent} max={budget.limit} label={t('rm.budget_title')} tone={budget.tone === 'neutral' ? 'accent' : budget.tone} />{/if}
        {#if budgetNote[budget.state]}<p class="small muted">{t(budgetNote[budget.state])}</p>{/if}
        <div><Button size="sm" variant="ghost" icon={I.SlidersHorizontal} href="/blocks?view=settings">{t('rm.budget_edit')}</Button></div>
      </div>
    {/if}
  </Card>

  {#if !channels.data && !blocks.data}
    <LoadState res={[channels, blocks]}><Card><Skeleton lines={4} /></Card></LoadState>
  {:else if rooms.length === 0}
    <Card><EmptyState pose="box" title={t('rm.no_rooms')} description={t('rm.no_rooms_body')}><Button href="/rooms" icon={I.Hash}>{t('nav.rooms')}</Button><Button href="/blocks" variant="ghost" icon={I.Blocks}>{t('nav.blocks')}</Button></EmptyState></Card>
  {:else}
    <div class="picker">
      <Field id="rm-room" label={t('rm.room')}>
        {#snippet children({ id })}<Select {id} bind:value={picked} options={rooms} />{/snippet}
      </Field>
    </div>

    <Card title={t('rm.state_title')} description={t('rm.state_desc')}>
      {#snippet actions()}{#if st?.at}<span class="xs muted" title={datetime(st.at)}>{t('rm.state_age', { when: relative(st.at) })}</span>{/if}{/snippet}
      {#if pick?.kind !== 'discord'}
        <p class="small muted">{t('rm.state_kakao')}</p>
      {:else if !st || (st.loading && !st.state)}
        <Skeleton lines={3} />
      {:else if st.error}
        <div role="alert" class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span>{st.error}</span><Button size="sm" variant="ghost" onclick={() => loadState()}>{t('common.retry')}</Button></div>
      {:else if stateEmpty}
        <EmptyState compact pose="search" title={t('rm.state_empty')} description={t('rm.state_empty_body')} />
      {:else}
        <div class="stack">
          <p class="xs muted refnote"><I.Info size={13} aria-hidden="true" /> {t('rm.state_ref')}</p>
          {#if (st.state.threads ?? []).length}
            <section aria-labelledby="rm-topics">
              <h3 id="rm-topics" class="sub">{t('rm.state_topics')}</h3>
              <ul class="plain">
                {#each st.state.threads as th}
                  <li><div class="row"><strong class="break">{th.label}</strong><Status tone={th.status === 'active' ? 'ok' : 'neutral'} label={t(`rm.topic.${th.status === 'active' ? 'active' : 'cooling'}`)} size="sm" />{#if th.last_at}<span class="xs muted">{relative(th.last_at)}</span>{/if}</div>
                    {#if th.gist}<p class="small muted break">{th.gist}</p>{/if}</li>
                {/each}
              </ul>
            </section>
          {/if}
          {#if (st.state.open_questions ?? []).length}
            <section aria-labelledby="rm-questions">
              <h3 id="rm-questions" class="sub">{t('rm.state_questions')}</h3>
              <ul class="plain">{#each st.state.open_questions as q}<li><span class="small break">{q.q}</span> {#if q.last_at}<span class="xs muted">{relative(q.last_at)}</span>{/if}</li>{/each}</ul>
            </section>
          {/if}
          {#if st.state.mood}<section aria-labelledby="rm-mood"><h3 id="rm-mood" class="sub">{t('rm.state_mood')}</h3><p class="small break">{st.state.mood.text}</p></section>{/if}
          {#if st.state.banter}<section aria-labelledby="rm-jokes"><h3 id="rm-jokes" class="sub">{t('rm.state_jokes')}</h3><p class="small break">{st.state.banter.text}</p></section>{/if}
        </div>
      {/if}
    </Card>

    <Card title={t('rm.episodes_title')} description={t('rm.episodes_desc')}>
      {#if !eps.data}
        <LoadState res={eps}><Skeleton lines={5} /></LoadState>
      {:else if all.length === 0}
        <EmptyState compact pose="box" title={t('rm.empty')} description={t('rm.empty_body')} />
      {:else}
        <div class="filters">
          <Field id="rm-kind" label={t('rm.filter_kind')}>
            {#snippet children({ id })}<Select {id} bind:value={kind} options={[{ value: '', label: t('rm.filter_all') }, ...kindOptions.map((v) => ({ value: v, label: label('rm.kind', v) }))]} />{/snippet}
          </Field>
          <Field id="rm-status" label={t('rm.filter_status')}>
            {#snippet children({ id })}<Select {id} bind:value={status} options={[{ value: '', label: t('rm.filter_all') }, ...statusOptions.map((v) => ({ value: v, label: label('rm.status', v) }))]} />{/snippet}
          </Field>
        </div>
        {#if shown.length === 0}
          <EmptyState compact pose="search" title={t('rm.none_match')} />
        {:else}
          <ul class="eps" aria-label={t('rm.episodes_title')}>
            {#each shown as ep (ep.id)}
              <li class:on={open === ep.id}>
                <button type="button" class="head" aria-expanded={open === ep.id} aria-controls="rm-ep-{ep.id}" onclick={() => toggle(ep.id)}>
                  <span class="title break">{ep.title}</span>
                  <span class="chips">
                    <span class="chip">{label('rm.kind', ep.kind)}</span>
                    <Status tone={statusTone[ep.status] ?? 'neutral'} label={label('rm.status', ep.status)} size="sm" />
                    {#if ep.regen}<Status tone="warn" label={t('rm.regen')} size="sm" />{/if}
                  </span>
                  <span class="meta xs muted">
                    <span>{sourceText(ep.source)}</span>
                    {#if ep.due_at || ep.when_text}<span>· {t('rm.due', { when: ep.due_at ? datetime(ep.due_at) : ep.when_text })}</span>{/if}
                    <span>· {t('rm.people', { n: num(ep.participants) })}</span>
                    <span>· {t('rm.items_n', { n: num(ep.items) })}</span>
                    <span>· {t('rm.last_seen', { when: relative(ep.last_seen) })}</span>
                  </span>
                </button>
                {#if open === ep.id}
                  <div class="detail" id="rm-ep-{ep.id}">
                    {#if !detail || detail.loading}
                      <Skeleton lines={3} />
                    {:else if detail.error}
                      <div role="alert" class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span>{detail.error}</span></div>
                    {:else}
                      {#if ep.regen}<p class="small muted">{t('rm.regen_hint')}</p>{/if}
                      <p class="xs muted">{t('rm.evidence_hint')}</p>
                      <ul class="items">
                        {#each detail.items as it (it.id)}
                          <li>
                            <div class="row"><span class="chip">{label('rm.item', it.kind)}</span>{#if it.state && it.state !== 'active'}<Status tone="neutral" label={label('rm.item_state', it.state)} size="sm" />{/if}</div>
                            <p class="break">{it.text}</p>
                            {#if (it.evidence ?? []).length}
                              <details>
                                <summary class="small">{t('rm.evidence_n', { n: it.evidence.length })}</summary>
                                <ul class="ev">
                                  {#each it.evidence as ev (ev.id)}
                                    {#if ev.gone}
                                      <li class="gone"><I.EyeOff size={13} aria-hidden="true" /> <span class="small">{t('rm.evidence_gone')}</span></li>
                                    {:else}
                                      <li><span class="xs muted">{ev.author}{ev.at ? ` · ${datetime(ev.at)}` : ''}</span><p class="small break">{ev.text}</p></li>
                                    {/if}
                                  {/each}
                                </ul>
                              </details>
                            {/if}
                          </li>
                        {:else}
                          <li class="small muted">{t('rm.no_items')}</li>
                        {/each}
                      </ul>
                      <div><Button size="sm" variant="danger-ghost" icon={I.Trash2} disabled={!can('memory.episodes.forget')} onclick={() => { forgetFor = ep; forgetOpen = true }}>{t('rm.forget')}</Button></div>
                    {/if}
                  </div>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
      {/if}
    </Card>
  {/if}
</div>

<Confirm bind:open={forgetOpen} danger title={t('rm.forget_title')} body={t('rm.forget_body')} confirmLabel={t('rm.forget_confirm')} onconfirm={forget}>
  {#snippet details()}
    {#if forgetFor}
      <div class="fp">
        <strong class="break">{forgetFor.title}</strong>
        <ul>
          <li>{t('rm.forget_items', { n: num(preview.items) })}{#if preview.hidden}<span class="muted"> {t('rm.forget_hidden', { n: num(preview.hidden) })}</span>{/if}</li>
          <li>{t('rm.forget_keeps')}</li>
        </ul>
      </div>
    {/if}
  {/snippet}
</Confirm>

<style>
  .rm {
    display: grid;
    gap: var(--s-4);
    min-width: 0;
  }
  .rm > :global(*) {
    min-width: 0;
  }
  .picker {
    max-width: 420px;
  }
  .spend {
    font-size: var(--text-xl);
    letter-spacing: -0.02em;
  }
  .sub {
    margin: 0 0 6px;
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text-muted);
  }
  .refnote {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .plain,
  .items,
  .ev,
  .eps {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .plain {
    display: grid;
    gap: var(--s-2);
  }
  .filters {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 180px), 1fr));
    margin-bottom: var(--s-3);
  }
  .eps {
    display: grid;
    gap: var(--s-2);
  }
  .eps > li {
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    min-width: 0;
  }
  .eps > li.on {
    box-shadow: var(--shadow-border-hover);
  }
  .head {
    display: grid;
    gap: 6px;
    width: 100%;
    padding: var(--s-3) var(--s-4);
    border: 0;
    border-radius: inherit;
    background: none;
    color: inherit;
    font: inherit;
    text-align: start;
    cursor: pointer;
    min-height: 44px;
  }
  .head:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: -2px;
  }
  .title {
    font-weight: 600;
  }
  .chips,
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 8px;
    align-items: center;
  }
  .detail {
    display: grid;
    gap: var(--s-3);
    padding: 0 var(--s-4) var(--s-4);
  }
  .items {
    display: grid;
    gap: var(--s-3);
  }
  .items > li {
    display: grid;
    gap: 4px;
    padding-bottom: var(--s-3);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .items p {
    margin: 0;
  }
  .ev {
    display: grid;
    gap: var(--s-2);
    margin-top: var(--s-2);
    padding-inline-start: var(--s-3);
    border-inline-start: 2px solid var(--border);
  }
  .ev p {
    margin: 0;
  }
  .gone {
    display: flex;
    gap: 6px;
    align-items: center;
    color: var(--text-muted);
    font-style: italic;
  }
  summary {
    cursor: pointer;
    min-height: 24px;
  }
  .fp {
    display: grid;
    gap: 6px;
    margin-top: var(--s-3);
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .fp ul {
    margin: 0;
    padding-inline-start: 18px;
    font-size: var(--text-sm);
  }
  .between {
    justify-content: space-between;
  }
</style>
