<script lang="ts">
  import { shownIn } from '../lib/evidence'
  import { scrollable } from '../lib/scrollable'
  import { stateLabel } from '../lib/blocks'
  import { fly } from 'svelte/transition'
  import { has, t } from '../lib/i18n.svelte'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import { route, setQuery } from '../lib/router.svelte'
  import { relative, num } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Select from '../components/Select.svelte'
  import Field from '../components/Field.svelte'
  import Dialog from '../components/Dialog.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Status from '../components/Status.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Timeline from '../components/Timeline.svelte'
  import RoomMemory from '../components/RoomMemory.svelte'
  import * as I from '../lib/icons'

  // Search what the bot remembers, with the evidence spans; exclude a wrong or private chunk, attach a
  // correction the answer model will see, and link names that belong to one person. Source messages are
  // never edited: reviews and links are separate records.
  const blocks = resource<any>('memory.blocks.list')
  const d = $derived(motionAllowed() ? 1 : 0)
  let tab = $state(route.query.get('tab') ?? 'search')
  let q = $state(route.query.get('q') ?? '')
  let searchMode = $state<'memory' | 'messages'>('memory')
  let rooms = $state<string[]>([])
  let ranking = $state('auto')
  let topK = $state('8')
  let hits = $state<any[] | null>(null)
  let corrected = $state<Record<string, any>>({})
  let ragState = $state('')
  let busy = $state(false)
  let err = $state('')
  function codeLabel(namespace: string, code: string) {
    const key = `${namespace}.${code}`
    return has(key) ? t(key) : code.replaceAll('_', ' ')
  }
  // the blocks searched are attached to no room: the bot cannot use what is found here in any chat yet
  const unattached = $derived(rooms.length > 0 && ((blocks.data?.blocks ?? []) as any[]).filter((b) => rooms.includes(b.source_room)).every((b) => !(b.channels ?? []).length))
  const allRooms = $derived(((blocks.data?.blocks ?? []) as any[]).map((b) => ({ id: b.source_room as string, label: (b.label || b.source_room) as string, n: b.message_count ?? 0 })))
  $effect(() => {
    if (rooms.length || !allRooms.length) return
    // only when something is found: an empty result is a new array every run and would loop this effect
    const withMessages = allRooms.filter((r) => r.n > 0).map((r) => r.id)
    if (withMessages.length) rooms = withMessages
  })
  async function search(e?: SubmitEvent) {
    e?.preventDefault()
    if (!q.trim()) return (err = t('memory.q_required'))
    if (!rooms.length) return (err = t('memory.rooms_required'))
    err = ''
    busy = true
    setQuery({ q })
    const action = searchMode === 'messages' ? 'messages.search' : 'memory.search'
    const r = await call(action, { query: q.trim(), rooms, ranking, top_k: Number(topK) })
    busy = false
    if (!r.ok) return (err = r.error?.message ?? '')
    hits = r.output.hits ?? []
    corrected = r.output.corrected ?? {}
    ragState = r.output.rag_state
  }
  // mark highlights the query's words in a hit. A word in Latin letters is matched whole and needs four
  // letters ("is" and "the" are in every English line, and "is" inside "Spanish" is no match); other
  // scripts match inside words, since Korean glues particles on ("캠핑" in "캠핑장에").
  function mark(text: string, query: string) {
    const latin = /^[a-z]+$/i
    const terms = query
      .split(/\s+/)
      .map((w) => w.replace(/^[^\p{L}\p{N}]+|[^\p{L}\p{N}]+$/gu, ''))
      .filter((w) => (latin.test(w) ? w.length >= 4 : w.length >= 2))
      .map((w) => {
        const esc = w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
        return latin.test(w) ? `\\b${esc}\\b` : esc
      })
    if (!terms.length) return [{ t: text, m: false }]
    const re = new RegExp(`(${terms.join('|')})`, 'gi')
    return text.split(re).map((part, i) => ({ t: part, m: i % 2 === 1 }))
  }

  // reviews
  const reviews = resource<any>('memory.reviews', () => ({}), { live: ['memory'] })
  let revFor = $state<any>(null)
  let revKind = $state<'exclude' | 'correct'>('correct')
  let revNote = $state('')
  let revOpen = $state(false)
  let revBusy = $state(false)
  function openReview(h: any, kind: 'exclude' | 'correct') {
    revFor = h
    revKind = kind
    revNote = corrected[h.doc_id]?.note ?? ''
    revOpen = true
  }
  async function saveReview() {
    revBusy = true
    const r = await call('memory.review', { leaf_id: revFor.doc_id, kind: revKind, note: revNote.trim() })
    revBusy = false
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    revOpen = false
    toast('ok', revKind === 'exclude' ? t('memory.excluded') : t('memory.corrected'))
    void reviews.refresh()
    if (hits) void search()
  }
  async function clearReview(leaf: string) {
    const r = await call('memory.review', { leaf_id: leaf, kind: 'clear' })
    if (r.ok) {
      toast('ok', t('memory.review_cleared'))
      void reviews.refresh()
      if (hits) void search()
    } else toast('error', t('common.failed'), r.error?.message)
  }

  // people
  const people = resource<any>('people.list', () => ({}), { live: ['config'] })
  let picked = $state<string[]>([])
  let linkOpen = $state(false)
  let linkName = $state('')
  let linkAliases = $state('')
  let peopleFilter = $state('')
  const speakers = $derived(((people.data?.speakers ?? []) as any[]).filter((sp) => !peopleFilter || sp.name.toLowerCase().includes(peopleFilter.toLowerCase())))
  const linkedTo = $derived.by(() => {
    const m = new Map<string, string>()
    for (const p of (people.data?.people ?? []) as any[]) {
      m.set(p.name, p.name)
      for (const a of p.aliases) m.set(a, p.name)
    }
    return m
  })
  function openLink(existing?: any) {
    if (existing) {
      linkName = existing.name
      linkAliases = existing.aliases.join('\n')
    } else {
      linkName = picked[0] ?? ''
      linkAliases = picked.slice(1).join('\n')
    }
    linkOpen = true
  }
  async function saveLink() {
    const aliases = linkAliases.split(/[\n,]/).map((x) => x.trim()).filter(Boolean)
    const r = await call('people.merge', { name: linkName.trim(), aliases })
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    linkOpen = false
    picked = []
    toast('ok', t('memory.people_linked', { name: linkName.trim() }))
    void people.refresh()
  }
  async function unlink(name: string) {
    const r = await call('people.merge', { name, remove: true })
    if (r.ok) {
      toast('ok', t('memory.people_unlinked', { name }))
      void people.refresh()
    }
  }
</script>

<PageHeader title={t('memory.title')} description={t('memory.desc')} />

<div data-tour="memory-tabs"><Tabs id="memory" bind:active={tab} onchange={(v) => setQuery({ tab: v === 'search' ? null : v })} tabs={[
  { id: 'search', label: t('memory.tab.search'), icon: I.Search },
  { id: 'room', label: t('memory.tab.room'), icon: I.Layers },
  { id: 'timeline', label: t('memory.tab.timeline'), icon: I.CalendarRange },
  { id: 'reviews', label: t('memory.tab.reviews'), icon: I.PencilLine, count: reviews.data?.reviews?.length },
  { id: 'people', label: t('memory.tab.people'), icon: I.Users, count: people.data?.people?.length }]} /></div>

<div class="panel" role="tabpanel" id="memory-panel-{tab}" aria-labelledby="memory-tab-{tab}">
    {#if tab === 'search'}
    <div class="mode"><Segmented label={t('memory.search_mode')} bind:value={searchMode} options={[{ value: 'memory', label: t('memory.mode.semantic') }, { value: 'messages', label: t('memory.mode.messages') }]} /></div>
    <form class="searchbar" onsubmit={search} data-tour="memory-search">
      <div class="q"><I.Search size={18} aria-hidden="true" /><input bind:value={q} placeholder={t('memory.placeholder')} aria-label={t('memory.placeholder')} /></div>
      {#if searchMode === 'memory'}<div style:width="150px"><Select aria-label={t('lab.ranking')} bind:value={ranking} options={['auto', 'hybrid', 'rerank', 'heuristic'].map((v) => ({ value: v, label: t(`lab.rank.${v}`) }))} /></div>{/if}
      <div style:width="100px"><Select aria-label={t('memory.top_k')} bind:value={topK} options={['5', '8', '12', '20'].map((v) => ({ value: v, label: t('memory.top_n', { n: v }) }))} /></div>
      <Button type="submit" variant="primary" loading={busy} icon={I.Search}>{t('memory.search')}</Button>
    </form>
    <div class="rooms">
      {#each allRooms as r (r.id)}
        <button type="button" class="room" title={r.id} class:on={rooms.includes(r.id)} aria-pressed={rooms.includes(r.id)} onclick={() => (rooms = rooms.includes(r.id) ? rooms.filter((x) => x !== r.id) : [...rooms, r.id])}>{r.label}<span class="num">{r.n}</span></button>
      {/each}
    </div>
    {#if err}<p class="err small" role="alert">{err}</p>{/if}
    {#if busy}
      <Card><Skeleton lines={8} /></Card>
    {:else if hits === null}
      <Card><EmptyState pose="search" title={t('memory.start')} description={t('memory.start_body')} /></Card>
    {:else if hits.length === 0}
      <Card><EmptyState pose="search" title={t('memory.none', { q })} description={t('memory.none_body')}>
        {#if ragState}<span class="chip">{stateLabel(ragState)}</span>{/if}
        {#if unattached}<p class="small muted">{t('memory.none_unattached')}</p><Button size="sm" href="/blocks" icon={I.Link2}>{t('lab.empty.attach')}</Button>{/if}
      </EmptyState></Card>
    {:else}
      <p class="small muted" style:margin-bottom="8px">{t('memory.found', { n: hits.length })} · <span class="chip">{stateLabel(ragState)}</span></p>
      <div class="hits">
        {#each hits as h, i (h.doc_id ?? i)}
          <article class="hit" in:fly={{ y: 10, duration: 260 * d, delay: i * 40 * d }}>
            <header class="row xs muted">
              <span class="chip accent">#{h.rank}</span>
              <span class="ellipsis">{h.source_room_id}</span>
              <span>· {h.date}</span>
              {#if h.speakers?.length}<span>· {h.speakers.join(', ')}</span>{/if}
              <span class="spacer"></span>
              {#if h.rerank_score !== undefined && h.rerank_score !== null}<span class="num">{t('memory.rerank_score')} {Number(h.rerank_score).toFixed(3)}</span>{/if}
              {#if h.dense_cos !== undefined && h.dense_cos !== null}<span class="num">{t('memory.cosine_score')} {Number(h.dense_cos).toFixed(3)}</span>{/if}
              {#if h.lexical_score !== undefined}<span class="num">{t('memory.lexical_score')} {h.lexical_score}</span>{/if}
            </header>
            {#if corrected[h.doc_id]}<Status tone="info" label={t('memory.has_correction')} size="sm" />{/if}
            {#if h.summary}<p class="sum small">{h.summary}</p>{/if}
            {#if shownIn(hits, i)}<p class="xs muted">{t('evidence.shown_in', { n: shownIn(hits, i) })}</p>{/if}
            <pre>{#each mark(h.excerpt ?? '', q) as part}{#if part.m}<mark>{part.t}</mark>{:else}{part.t}{/if}{/each}</pre>
            <footer class="row">
              {#if h.packing_status}<span class="xs subtle">{codeLabel('memory.packing', h.packing_status)}</span>{/if}
              <span class="spacer"></span>
              {#if searchMode === 'memory' && can('memory.review')}
                <Button size="sm" variant="ghost" icon={I.PencilLine} onclick={() => openReview(h, 'correct')}>{t('memory.correct')}</Button>
                <Button size="sm" variant="danger-ghost" icon={I.EyeOff} onclick={() => openReview(h, 'exclude')}>{t('memory.exclude')}</Button>
              {/if}
            </footer>
          </article>
        {/each}
      </div>
    {/if}
  {:else if tab === 'room'}
    <RoomMemory />
  {:else if tab === 'timeline'}
    <div class="rooms">
      {#each allRooms as r (r.id)}
        <button type="button" class="room" title={r.id} class:on={rooms.includes(r.id)} aria-pressed={rooms.includes(r.id)} onclick={() => (rooms = rooms.includes(r.id) ? rooms.filter((x) => x !== r.id) : [...rooms, r.id])}>{r.label}<span class="num">{r.n}</span></button>
      {/each}
    </div>
    <Card title={t('room.timeline_title')} description={t('memory.timeline_desc')}>
      {#if rooms.length}<Timeline scope={{ rooms }} />{:else}<p class="small muted">{t('memory.rooms_required')}</p>{/if}
    </Card>
  {:else if tab === 'reviews'}
    <Card padded={false} title={t('memory.reviews_title')} description={t('memory.reviews_desc')}>
      {#if !(reviews.data?.reviews ?? []).length}
        <EmptyState compact pose="search" title={t('memory.reviews_empty')} description={t('memory.reviews_empty_body')} />
      {:else}
        <div use:scrollable class="table-scroll"><table class="table">
          <thead><tr><th>{t('memory.col.kind')}</th><th>{t('memory.col.memory')}</th><th>{t('memory.col.note')}</th><th>{t('memory.col.when')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {#each reviews.data.reviews as rv (rv.leaf_id)}
              <tr><td><Status tone={rv.kind === 'exclude' ? 'warn' : 'info'} label={t(`memory.kind.${rv.kind}`)} size="sm" /></td>
                <td class="q2"><div class="xs subtle mono">{rv.room}</div><div class="small clamp">{rv.excerpt}</div></td>
                <td class="small break">{rv.note || '–'}</td>
                <td class="muted nowrap">{relative(rv.at)}{#if rv.actor}<div class="xs subtle">{rv.actor}</div>{/if}</td>
                <td class="r"><Button size="sm" variant="ghost" icon={I.Undo2} disabled={!can('memory.review')} onclick={() => clearReview(rv.leaf_id)}>{t('memory.undo_review')}</Button></td></tr>
            {/each}
          </tbody>
        </table></div>
      {/if}
    </Card>
  {:else}
    <div class="people">
      <Card padded={false} title={t('memory.speakers')} description={t('memory.speakers_desc')}>
        {#snippet actions()}<Button variant="primary" size="sm" icon={I.Link2} disabled={picked.length < 1 || !can('people.merge')} onclick={() => openLink()}>{t('memory.link_selected', { n: picked.length })}</Button>{/snippet}
        <div class="filter"><Input type="search" bind:value={peopleFilter} placeholder={t('memory.speaker_filter')} aria-label={t('memory.speaker_filter')} /></div>
        {#if !people.data}
          <div class="pad"><LoadState res={people}><Skeleton lines={6} /></LoadState></div>
        {:else if speakers.length === 0}
          <EmptyState compact pose="search" title={t('memory.no_speakers')} description={t('memory.no_speakers_body')} />
        {:else}
          <div use:scrollable class="table-scroll"><table class="table">
            <thead><tr><th><span class="sr-only">{t('memory.pick')}</span></th><th>{t('memory.col.name')}</th><th>{t('memory.col.platform')}</th><th class="r">{t('memory.col.messages')}</th><th class="r">{t('memory.col.rooms')}</th><th>{t('memory.col.linked')}</th></tr></thead>
            <tbody>
              {#each speakers as sp (sp.name + sp.platform)}
                <tr><td><input type="checkbox" aria-label={t('memory.pick_name', { name: sp.name })} checked={picked.includes(sp.name)} onchange={() => (picked = picked.includes(sp.name) ? picked.filter((x) => x !== sp.name) : [...picked, sp.name])} /></td>
                  <td><strong class="break">{sp.name}</strong></td><td><span class="chip">{sp.platform}</span></td>
                  <td class="r num">{num(sp.messages)}</td><td class="r num">{sp.rooms}</td>
                  <td>{#if linkedTo.get(sp.name)}<span class="chip accent">{linkedTo.get(sp.name)}</span>{:else if sp.accounts > 1}<span class="xs muted" title={t('memory.accounts_hint')}>{t('memory.accounts_n', { n: sp.accounts })}</span>{/if}</td></tr>
              {/each}
            </tbody>
          </table></div>
        {/if}
      </Card>
      <Card title={t('memory.links')} description={t('memory.links_desc')}>
        {#if !(people.data?.people ?? []).length}
          <p class="small muted">{t('memory.links_empty')}</p>
        {:else}
          <ul class="links">
            {#each people.data.people as p (p.name)}
              <li><div><strong>{p.name}</strong><div class="chips">{#each p.aliases as a}<span class="chip">{a}</span>{/each}</div></div>
                <div class="row"><Button size="sm" variant="ghost" icon={I.PencilLine} disabled={!can('people.merge')} onclick={() => openLink(p)}>{t('common.edit')}</Button><Button size="sm" variant="danger-ghost" icon={I.Unlink} disabled={!can('people.merge')} onclick={() => unlink(p.name)}>{t('memory.unlink')}</Button></div></li>
            {/each}
          </ul>
        {/if}
        <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('memory.links_note')}</span></div>
      </Card>
    </div>
  {/if}
</div>

<Dialog bind:open={revOpen} title={revKind === 'exclude' ? t('memory.exclude_title') : t('memory.correct_title')} description={revKind === 'exclude' ? t('memory.exclude_desc') : t('memory.correct_desc')}>
  <div class="stack">
    {#if revFor}<pre class="quote small">{(revFor.excerpt ?? '').slice(0, 600)}</pre>{/if}
    {#if revKind === 'correct'}
      <Field id="rv-note" label={t('memory.correct_note')} hint={t('memory.correct_note_hint')}>{#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={revNote} rows={3} maxlength={1000} />{/snippet}</Field>
    {:else}
      <Field id="rv-why" label={t('memory.exclude_note')}>{#snippet children({ id })}<Input {id} bind:value={revNote} maxlength={300} />{/snippet}</Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (revOpen = false)}>{t('common.cancel')}</Button>
    <Button variant={revKind === 'exclude' ? 'danger' : 'primary'} loading={revBusy} disabled={revKind === 'correct' && !revNote.trim()} onclick={saveReview}>{revKind === 'exclude' ? t('memory.exclude') : t('memory.correct')}</Button>
  {/snippet}
</Dialog>

<Dialog bind:open={linkOpen} title={t('memory.link_title')} description={t('memory.link_desc')}>
  <div class="stack">
    <Field id="ln-name" label={t('memory.link_name')} hint={t('memory.link_name_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={linkName} />{/snippet}</Field>
    <Field id="ln-al" label={t('memory.link_aliases')} hint={t('memory.link_aliases_hint')}>{#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={linkAliases} rows={4} />{/snippet}</Field>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (linkOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Link2} disabled={!linkName.trim() || !linkAliases.trim()} onclick={saveLink}>{t('memory.link_save')}</Button>
  {/snippet}
</Dialog>

<style>
  .mode {
    margin-bottom: var(--s-3);
  }
  .panel {
    margin-top: var(--s-4);
  }
  .searchbar {
    display: flex;
    gap: var(--s-2);
    flex-wrap: wrap;
    align-items: center;
  }
  .q {
    flex: 1 1 320px;
    display: flex;
    align-items: center;
    gap: 10px;
    height: 44px;
    padding: 0 14px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text-muted);
  }
  .q:focus-within {
    outline: 2px solid var(--focus);
  }
  .q input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: none;
    outline: none;
    font-size: var(--text-md);
    color: var(--text);
  }
  .rooms {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: var(--s-3) 0 var(--s-4);
  }
  .room {
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    display: inline-flex;
    gap: 6px;
    align-items: center;
    height: 30px;
    padding: 0 12px;
    border: 0;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .room .num {
    font-size: var(--text-xs);
  }
  .room.on {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .hits {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 460px), 1fr));
  }
  .hit {
    padding: var(--s-4);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }
  .hit pre,
  .quote {
    margin: 0;
    font-family: var(--font);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 260px;
    overflow: auto;
  }
  .quote {
    padding: 10px 12px;
    border-radius: var(--r);
    background: var(--surface-2);
    max-height: 180px;
  }
  mark {
    background: var(--accent-soft-hover);
    color: inherit;
    border-radius: 3px;
    padding: 0 2px;
  }
  .sum {
    color: var(--text-muted);
    padding: 8px 10px;
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .clamp {
    display: -webkit-box;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .people {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
    align-items: start;
  }
  @media (min-width: 1100px) {
    .people {
      grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr);
    }
  }
  .filter {
    padding: 0 var(--pad-card) var(--s-3);
  }
  .pad {
    padding: var(--s-4) var(--pad-card);
  }
  .links {
    list-style: none;
    margin: 0 0 var(--s-3);
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
  }
  .links li {
    display: flex;
    justify-content: space-between;
    gap: var(--s-3);
    flex-wrap: wrap;
    padding-bottom: var(--s-3);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 4px;
  }
  .err {
    color: var(--danger);
    margin-bottom: 8px;
  }
</style>
