<script lang="ts">
  import { outcomeColor, outcomeTone } from '../lib/outcomes'
  import { t, toolName } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call } from '../lib/api.svelte'
  import { route, setQuery } from '../lib/router.svelte'
  import { onLive } from '../lib/live.svelte'
  import { toast } from '../lib/toast.svelte'
  import { usd, ms, relative } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Status from '../components/Status.svelte'
  import Select from '../components/Select.svelte'
  import Input from '../components/Input.svelte'
  import Button from '../components/Button.svelte'
  import Segmented from '../components/Segmented.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Dialog from '../components/Dialog.svelte'
  import TurnDetail from '../components/TurnDetail.svelte'
  import FeedbackSummary from '../components/FeedbackSummary.svelte'
  import OperatorMarks from '../components/OperatorMarks.svelte'
  import Switch from '../components/Switch.svelte'
  import * as I from '../lib/icons'

  let outcome = $state(route.query.get('outcome') ?? '')
  let tool = $state(route.query.get('tool') ?? '')
  let channel = $state(route.query.get('channel') ?? '')
  let lab = $state<'false' | 'only'>((route.query.get('lab') as any) ?? 'false')
  let toolsOnly = $state(route.query.get('tools') === '1')
  // a random draw of search turns to mark (turns.sample); null while the list is the plain history
  const SAMPLE = 30
  let sample = $state<{ pool: number } | null>(null)
  let turns = $state<any[]>([])
  let next = $state<string | undefined>()
  let loading = $state(true)
  let loadError = $state<{ message: string } | undefined>()
  // the first page failing is a load error (not an empty list); LoadState shows it with a retry button
  const firstPage = {
    get data() {
      return loading || loadError ? undefined : turns
    },
    get error() {
      return loadError
    },
    refresh: () => load(),
  }
  let selected = $state<any>(null)
  // the call ledgers of the selected turn (model calls, tool calls, delivery)
  let ledger = $state<any>(null)
  let wide = $state(true)
  let sheetOpen = $state(false)

  async function load(more = false) {
    loading = !more
    loadError = undefined
    const r = await call('turns.list', { outcome, tool, channel_id: channel, lab, tools_only: toolsOnly, limit: 50, before: more ? next : undefined })
    loading = false
    if (!r.ok) {
      if (more) toast('error', t('load.failed'), r.error?.message)
      else loadError = r.error
      return
    }
    turns = more ? [...turns, ...r.output.turns] : r.output.turns
    next = r.output.next_before
  }
  // the turn being fetched: the effect below calls open() again when the URL changes, and setQuery changes it
  let opening = ''
  async function open(id: string) {
    if (opening === id) return
    opening = id
    setQuery({ id })
    const r = await call('turns.get', { id })
    if (opening !== id) return // a newer open took over
    opening = ''
    if (r.ok) {
      selected = r.output.turn
      ledger = r.output.ledger ?? null
      if (!wide) sheetOpen = true
    }
  }
  async function drawSample() {
    loading = true
    loadError = undefined
    const r = await call('turns.sample', { n: SAMPLE, lab })
    loading = false
    if (!r.ok) {
      toast('error', t('turns.sample.failed'), r.error?.message)
      return
    }
    turns = r.output.turns
    next = undefined
    sample = { pool: r.output.pool }
    selected = null
    setQuery({ id: null })
  }
  function backToList() {
    sample = null
    void load()
  }
  // the operator's verdict was saved: show it in the list and the open turn; in a draw, go on to the next unmarked turn
  function marked(id: string, mark: string) {
    if (selected?.id === id) selected = { ...selected, mark }
    turns = turns.map((x) => (x.id === id ? { ...x, mark } : x))
    const following = sample && mark && selected?.id === id ? turns.find((x) => !x.mark && x.id !== id) : undefined
    if (following) void open(following.id)
  }
  const sampled = $derived(turns.filter((x) => x.mark).length)
  $effect(() => {
    void outcome, tool, channel, lab, toolsOnly
    setQuery({ outcome, tool, channel, lab: lab === 'false' ? null : lab, tools: toolsOnly ? '1' : null })
    sample = null
    void load()
  })
  $effect(() => onLive(['turn'], () => { if (!sample) void load() }))
  $effect(() => {
    const id = route.query.get('id')
    if (id && selected?.id !== id && opening !== id) void open(id)
  })
  $effect(() => {
    const mq = window.matchMedia('(min-width: 1280px)')
    wide = mq.matches
    const f = () => (wide = mq.matches)
    mq.addEventListener('change', f)
    return () => mq.removeEventListener('change', f)
  })
  // long lists render only the rows in view (fixed row height) and load more near the end
  const ROW = 56
  let scroller: HTMLDivElement | undefined = $state()
  let scrollTop = $state(0)
  let viewH = $state(640)
  const virtual = $derived(turns.length > 120)
  const start = $derived(virtual ? Math.max(0, Math.floor(scrollTop / ROW) - 12) : 0)
  const end = $derived(virtual ? Math.min(turns.length, Math.ceil((scrollTop + viewH) / ROW) + 12) : turns.length)
  const rows = $derived(turns.slice(start, end))
  let loadingMore = false
  async function onscroll() {
    if (!scroller) return
    scrollTop = scroller.scrollTop
    viewH = scroller.clientHeight
    if (next && !loadingMore && scroller.scrollTop + scroller.clientHeight > scroller.scrollHeight - ROW * 8) {
      loadingMore = true
      await load(true)
      loadingMore = false
    }
  }
</script>

<PageHeader title={t('turns.title')} description={t('turns.desc')} />

<FeedbackSummary />
<OperatorMarks />

<div class="filters">
  <Segmented label={t('turns.source')} bind:value={lab} options={[{ value: 'false', label: t('turns.live') }, { value: 'only', label: t('turns.lab') }]} />
  <div class="f"><Select aria-label={t('turns.outcome')} bind:value={outcome} options={[{ value: '', label: t('turns.all_outcomes') }, ...['answered', 'silent', 'not_addressed', 'reacted', 'no_vision', 'limited', 'budget', 'paused', 'disabled', 'canceled', 'error'].map((v) => ({ value: v, label: t(`outcome.${v}`) }))]} /></div>
  <div class="f"><Input aria-label={t('turns.tool')} placeholder={t('turns.tool_ph')} bind:value={tool} /></div>
  <div class="f"><Input aria-label={t('turns.channel')} placeholder={t('turns.channel_ph')} bind:value={channel} inputmode="numeric" /></div>
  <Switch bind:checked={toolsOnly} label={t('turns.tools_only')} />
  <Button variant="secondary" icon={I.Shuffle} onclick={drawSample}>{t('turns.sample.draw', { n: SAMPLE })}</Button>
</div>

<div class="split" class:has-detail={!!selected && wide}>
  <Card padded={false}>
    {#if sample && !loading}
      <div class="sample" role="status">
        <span class="small">{t('turns.sample.banner', { n: turns.length, pool: sample.pool })}</span>
        <span class="small muted num">{t('turns.sample.progress', { done: sampled, n: turns.length })}</span>
        <span class="spacer"></span>
        <Button size="sm" variant="ghost" icon={I.Shuffle} onclick={drawSample}>{t('turns.sample.again')}</Button>
        <Button size="sm" variant="ghost" onclick={backToList}>{t('turns.sample.back')}</Button>
      </div>
    {/if}
    {#if loading || loadError}
      <div style:padding="20px"><LoadState res={firstPage}><Skeleton lines={8} height={18} /></LoadState></div>
    {:else if turns.length === 0}
      {#if sample}
        <EmptyState pose="think" title={t('turns.sample.empty')} />
      {:else}
        <EmptyState pose="think" title={t('turns.empty')} description={t('turns.empty_body')}><Button href="/lab" icon={I.FlaskConical}>{t('home.test_chat')}</Button></EmptyState>
      {/if}
    {:else}
      <div use:scrollable class="table-scroll list" bind:this={scroller} {onscroll} class:virtual>
        <table class="table" aria-rowcount={turns.length + 1}>
          <thead><tr><th>{t('turns.col.time')}</th><th>{t('turns.col.message')}</th><th class="w">{t('turns.col.tools')}</th><th class="r w">{t('turns.col.evidence')}</th><th class="r w">{t('turns.col.latency')}</th><th class="r w">{t('turns.col.cost')}</th><th class="w">{t('turns.col.outcome')}</th></tr></thead>
          <tbody>
            {#if start > 0}<tr aria-hidden="true" style:height="{start * ROW}px"><td colspan="7"></td></tr>{/if}
            {#each rows as tr, i (tr.id)}
              <tr class="clickable" class:selected={selected?.id === tr.id} aria-rowindex={start + i + 2}>
                <td class="nowrap num muted">{relative(tr.at)}</td>
                <td class="msgcell"><a class="row-link ellipsis" href="/turns?id={tr.id}" onclick={(e) => { e.preventDefault(); open(tr.id) }}>{#if tr.body?.message}{tr.body.message}{:else}<span class="no-text">{t(tr.body?.body_free ? 'turns.no_text' : 'turns.no_message')}</span>{/if}</a><div class="xs subtle ellipsis" title={tr.channel_id}>{#if tr.mark}<span class="mk {tr.mark}" title={t('turns.mark.title')}>{t(`turns.mark.${tr.mark}`)}</span>{' · '}{/if}<span class="m-only"><span class="dot" style:background={outcomeColor(tr.outcome)}></span>{`${t(`outcome.${tr.outcome}`)} · ${ms(tr.latency_ms)} · `}</span>#{tr.channel_name || tr.channel_id}{#if tr.body?.speaker}{` · ${tr.body.speaker}`}{/if}{#if tr.body?.name_call?.decision}{` · ${t(`turns.name_call.${tr.body.name_call.decision}`)}`}{/if}</div></td>
                <td class="w">{#each tr.tools as tl}<span class="chip">{toolName(tl)}</span> {/each}</td>
                <td class="r num w">{tr.hits || ''}</td>
                <td class="r num nowrap w">{ms(tr.latency_ms)}</td>
                <td class="r num nowrap w">{usd(tr.cost_usd)}</td>
                <td class="w"><Status tone={outcomeTone(tr.outcome)} label={t(`outcome.${tr.outcome}`)} size="sm" />{#if tr.feedback}<span class="fb xs num" title={t('turns.feedback_title')}>{#if tr.feedback.good}<span>👍 {tr.feedback.good}</span>{/if}{#if tr.feedback.bad}<span class="bad">👎 {tr.feedback.bad}</span>{/if}</span>{/if}</td>
              </tr>
            {/each}
            {#if end < turns.length}<tr aria-hidden="true" style:height="{(turns.length - end) * ROW}px"><td colspan="7"></td></tr>{/if}
          </tbody>
        </table>
      </div>
      {#if virtual}<p class="xs muted count">{t('turns.showing', { n: turns.length })}</p>{/if}
      {#if next}<div class="more"><Button variant="ghost" onclick={() => load(true)}>{t('common.load_more')}</Button></div>{/if}
    {/if}
  </Card>
  {#if selected && wide}
    <div class="detail"><Card title={t('turns.detail')}>{#snippet actions()}<Button size="sm" variant="ghost" icon={I.X} onclick={() => { selected = null; setQuery({ id: null }) }}>{t('common.close')}</Button>{/snippet}<TurnDetail turn={selected} {ledger} onmark={marked} /></Card></div>
  {/if}
</div>
{#if selected && !wide}
  <Dialog bind:open={sheetOpen} title={t('turns.detail')} size="lg" onclose={() => setQuery({ id: null })}><TurnDetail turn={selected} {ledger} onmark={marked} /></Dialog>
{/if}

<style>
  .no-text {
    color: var(--text-muted);
    font-style: italic;
  }
  .fb {
    display: inline-flex;
    gap: 6px;
    margin-inline-start: 6px;
    color: var(--text-muted);
  }
  .fb .bad {
    color: var(--danger);
  }
  .mk {
    font-weight: 600;
    color: var(--ok);
  }
  .mk.wrong {
    color: var(--danger);
  }
  .mk.stale {
    color: var(--warn);
  }
  .sample {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-3);
    padding: var(--s-3) var(--pad-card);
    border-bottom: 1px solid var(--border);
  }
  .filters {
    display: flex;
    gap: var(--s-3);
    flex-wrap: wrap;
    align-items: center;
    margin-bottom: var(--s-4);
  }
  .f {
    flex: 0 1 200px;
  }
  .msgcell {
    max-width: 420px;
  }
  .m-only {
    display: none;
  }
  .dot {
    display: inline-block;
    width: 7px;
    height: 7px;
    margin-inline-end: 4px;
    border-radius: 50%;
    vertical-align: 1px;
  }
  /* a phone shows time and message, with the outcome and time taken under the message */
  @media (max-width: 640px) {
    .list :global(.w) {
      display: none;
    }
    .m-only {
      display: inline;
    }
    .msgcell {
      max-width: calc(100vw - 150px);
    }
    .f {
      flex: 1 1 140px;
    }
  }
  .msgcell .row-link {
    display: block;
  }
  .list {
    max-height: calc(100dvh - 250px);
    overflow-y: auto;
  }
  .list :global(thead th) {
    position: sticky;
    top: 0;
    z-index: 1;
    background: var(--surface);
  }
  .list.virtual :global(tbody tr:not([aria-hidden])) {
    height: 56px;
  }
  .count {
    padding: 8px var(--pad-card) 0;
  }
  .more {
    display: flex;
    justify-content: center;
    padding: var(--s-3);
  }
</style>
