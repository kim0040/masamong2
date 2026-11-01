<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import { route, setQuery, navigate } from '../lib/router.svelte'
  import { num, usd, relative, duration, bytes } from '../lib/format'
  import { messageVectorAsk, messageVectorAskMode, messageVectorsOn, enableMessageVectors, type MessageVectorAsk } from '../lib/messagevectors'
  import { etaText, blockState, stateTone, period, jobLine, blockHref, stopReason, isKakao } from '../lib/blocks'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Button from '../components/Button.svelte'
  import Status from '../components/Status.svelte'
  import Progress from '../components/Progress.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Input from '../components/Input.svelte'
  import Select from '../components/Select.svelte'
  import Switch from '../components/Switch.svelte'
  import Field from '../components/Field.svelte'
  import Confirm from '../components/Confirm.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Popover from '../components/Popover.svelte'
  import ImportWizard from '../components/ImportWizard.svelte'
  import ConnectDialog from '../components/ConnectDialog.svelte'
  import ExtractMemory from '../components/ExtractMemory.svelte'
  import * as I from '../lib/icons'

  useConfig()
  const data = resource<any>('memory.blocks.list', () => ({}), { live: ['config', 'job', 'memory'] })
  let view = $state(route.query.get('view') ?? 'blocks')
  $effect(() => setQuery({ view: view === 'blocks' ? null : view }))

  const all = $derived((data.data?.blocks ?? []) as any[])
  const channels = $derived((data.data?.channels ?? []) as any[])
  const withData = $derived(all.filter((b) => (b.message_count ?? 0) > 0))
  const silent = $derived(all.filter((b) => !b.message_count && b.platform === 'discord'))
  let filter = $state<'all' | 'kakao' | 'discord' | 'todo' | 'ready'>('all')
  let q = $state('')
  const shown = $derived(
    withData
      .filter((b) => filter === 'all' || (filter === 'kakao' && isKakao(b)) || (filter === 'discord' && !isKakao(b)) || (filter === 'todo' && ['needs_prep', 'partial', 'failed'].includes(blockState(b))) || (filter === 'ready' && blockState(b) === 'ready'))
      .filter((b) => !q || `${b.label} ${b.note ?? ''} ${b.source_room}`.toLowerCase().includes(q.toLowerCase()))
      .sort((a, b) => Number(blockState(b) === 'preparing') - Number(blockState(a) === 'preparing') || (b.message_count ?? 0) - (a.message_count ?? 0)),
  )
  const counts = $derived({
    blocks: withData.length,
    ready: withData.filter((b) => blockState(b) === 'ready').length,
    messages: withData.reduce((s, b) => s + (b.message_count ?? 0), 0),
    links: channels.reduce((s, c) => s + (c.attached?.length ?? 0), 0),
    todo: withData.filter((b) => ['needs_prep', 'partial', 'failed'].includes(blockState(b))).length,
  })

  let wizardOpen = $state(route.query.get('import') === '1')
  let connectFor = $state<any>(null)
  let connectOpen = $state(false)
  function openConnect(b: any) {
    connectFor = b
    connectOpen = true
  }

  let extractFor = $state<any>(null)
  let extractOpen = $state(false)
  function openExtract(b: any) {
    extractFor = b
    extractOpen = true
  }

  async function prepare(b: any) {
    const r = await call('memory.blocks.prepare', { source_room: b.source_room, background: true })
    if (r.ok) toast('ok', r.output.already_running ? t('blocks.already_running') : t('blocks.prepare_started'), b.label)
    else toast('error', t('blocks.prepare_failed'), r.error?.message)
    void data.refresh()
  }
  async function cancelJob(b: any) {
    const r = await call('jobs.cancel', { id: b.job.id })
    r.ok ? toast('warn', t('blocks.cancelled')) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }
  async function captureOn(b: any) {
    const oc = b.own_channel
    if (!oc) return
    const ops: any[] = [{ path: ['guilds', oc.guild_id, 'channels', oc.channel_id, 'capture'], value: true }]
    if (!cfg.doc?.memory?.capture) ops.push({ path: ['memory', 'capture'], value: true })
    await patchConfig(ops, { success: t('blocks.capture_on_toast', { name: b.label }) })
    void data.refresh()
  }

  // channel view: attach and detach from the channel's side
  async function attachTo(c: any, b: any, close: () => void) {
    close()
    const r = await call('channels.view.attach', { guild_id: c.guild_id, channel_id: c.channel_id, block_id: b.block_id })
    r.ok ? toast('ok', t('connect.attached', { block: b.label, channel: c.name })) : toast('error', t('connect.failed', { n: 1 }), r.error?.message)
    void data.refresh()
  }
  async function detachFrom(c: any, a: any) {
    const r = await call('channels.view.detach', { guild_id: c.guild_id, channel_id: c.channel_id, block_id: a.block_id })
    r.ok ? toast('ok', t('connect.detached', { block: a.label || t('connect.stale'), channel: c.name })) : toast('error', t('common.failed'), r.error?.message)
    void data.refresh()
  }
  const byGuild = $derived.by(() => {
    const m = new Map<string, any[]>()
    for (const c of channels) m.set(c.guild_name, [...(m.get(c.guild_name) ?? []), c])
    return [...m.entries()]
  })
  const attachable = (c: any) => {
    const have = new Set((c.attached ?? []).map((a: any) => a.source_room))
    return withData.filter((b) => b.source_room !== c.own_room && !have.has(b.source_room))
  }
  const ownBlock = (c: any) => all.find((b) => b.source_room === c.own_room)

  // RAG settings (shared by every block: blocks connected to one channel must share one index recipe)
  const mem = $derived(data.data?.memory ?? {})
  const coverage = $derived(mem.counts?.leaves ? mem.counts.leaves_embedded / mem.counts.leaves : 0)
  async function setMem(key: string, value: unknown) {
    // an empty field returns to the default (room memory settings show it as the placeholder)
    const r = await patchConfig([value === '' ? { path: ['memory', key], op: 'unset' } : { path: ['memory', key], value }], { success: t('index.setting_saved') })
    void data.refresh()
    return r
  }

  // message-level vectors are on while the field is absent. Turning them back on embeds every message of the
  // indexed rooms, so the operator sees the estimate first (the engine prices it while the setting is off); on
  // confirm the setting is saved and every block that can be prepared is prepared again. Turning it off saves
  // an explicit false (an unset field would mean on).
  let mvOpen = $state(false)
  let mvAsk = $state<MessageVectorAsk | null>(null)
  // a switch the operator turned on and then cancelled is drawn again from the saved setting
  let mvKey = $state(0)
  $effect(() => {
    if (!mvOpen) untrack(() => mvKey++)
  })
  async function toggleMessageVectors(on: boolean) {
    if (!on) return setMem('message_vectors', false)
    const r = await call('memory.blocks.list', { message_vectors_estimate: true })
    if (!r.ok) {
      toast('error', t('index.message_vectors_estimate_failed'), r.error?.message)
      mvKey++
      return
    }
    mvAsk = messageVectorAsk(r.output)
    mvOpen = true
  }
  const mvBody = (a: MessageVectorAsk | null) => {
    if (!a) return t('index.message_vectors_unknown')
    const mode = messageVectorAskMode(a)
    // nothing is prepared yet: zeros would read as "free", so no figures are shown at all
    if (mode === 'unpriced') return t('index.message_vectors_body_unpriced', { n: num(a.unprepared) })
    const storage = a.dimensions > 0
      ? t('index.message_vectors_disk', { size: bytes(a.diskBytes) })
      : t('index.message_vectors_disk_unknown')
    return (
      t('index.message_vectors_body', { messages: num(a.messages), tokens: num(a.tokens), usd: usd(a.usd), eta: a.seconds > 0 ? duration(a.seconds) : t('blocks.est.under_minute') }) +
      ` ${storage}` +
      (mode === 'partial' ? ` ${t('index.message_vectors_unprepared', { n: num(a.unprepared) })}` : '') +
      (a.fallbackPrice ? ` ${t('index.message_vectors_fallback_price')}` : '')
    )
  }
  async function confirmMessageVectors() {
    const out = await enableMessageVectors({
      save: () => setMem('message_vectors', true),
      blocks: withData,
      preparing: (b) => blockState(b) === 'preparing',
      prepare: (b) => call('memory.blocks.prepare', { source_room: b.source_room, background: true }),
    })
    if (!out.saved) {
      // the setting was not saved (the save already said why): nothing was started, and the switch is drawn from the saved setting again
      toast('error', t('index.message_vectors_not_saved'), out.saveError)
      mvKey++
      return
    }
    const running = out.running ? ` ${t('index.message_vectors_running', { n: out.running })}` : ''
    if (out.failed) toast(out.started ? 'warn' : 'error', t('index.message_vectors_partial', { started: out.started, failed: out.failed }) + running, out.firstError)
    else if (out.started) toast('ok', t('index.message_vectors_started', { n: out.started }) + running)
    else toast('warn', out.running ? t('index.message_vectors_running', { n: out.running }) : t('index.message_vectors_saved_only'))
    void data.refresh()
  }
</script>

<PageHeader title={t('blocks.title')} description={t('blocks.desc')}>
  {#snippet actions()}
    <Button data-tour="blocks-import" variant="primary" icon={I.FileUp} onclick={() => (wizardOpen = true)}>{t('blocks.import')}</Button>
  {/snippet}
</PageHeader>

<!-- while the list loads the numbers are not known yet: a dash, not a 0 that reads as "no memory" -->
<div class="summary">
  <div><span class="muted small">{t('blocks.sum.blocks')}</span><strong class="num">{data.data ? num(counts.blocks) : '–'}</strong>{#if data.data}<span class="xs muted">{t('blocks.sum.ready', { n: counts.ready })}</span>{/if}</div>
  <div><span class="muted small">{t('blocks.sum.messages')}</span><strong class="num">{data.data ? num(counts.messages) : '–'}</strong></div>
  <div><span class="muted small">{t('blocks.sum.links')}</span><strong class="num">{data.data ? num(counts.links) : '–'}</strong>{#if data.data}<span class="xs muted">{t('blocks.sum.channels', { n: channels.length })}</span>{/if}</div>
  <div class:warn={counts.todo > 0}><span class="muted small">{t('blocks.sum.todo')}</span><strong class="num">{data.data ? num(counts.todo) : '–'}</strong></div>
</div>

<div data-tour="blocks-tabs"><Tabs id="blocks" bind:active={view} tabs={[
  { id: 'blocks', label: t('blocks.tab.blocks'), icon: I.Blocks, count: counts.blocks },
  { id: 'channels', label: t('blocks.tab.channels'), icon: I.Link2, count: channels.length },
  { id: 'settings', label: t('blocks.tab.settings'), icon: I.SlidersHorizontal }]} /></div>

<div class="panel" role="tabpanel" id="blocks-panel-{view}" aria-labelledby="blocks-tab-{view}">
  {#if !data.data}
    <LoadState res={data}><div class="grid" style:--min="320px">{#each Array(4) as _}<Card><Skeleton lines={5} /></Card>{/each}</div></LoadState>
  {:else if view === 'blocks'}
    {#if withData.length === 0}
      <Card>
        <EmptyState pose="box" title={t('blocks.empty')} description={t('blocks.empty_body')}>
          <Button variant="primary" icon={I.FileUp} onclick={() => (wizardOpen = true)}>{t('blocks.import')}</Button>
          {#if silent.length}<Button variant="ghost" onclick={() => (view = 'channels')}>{t('blocks.empty_discord')}</Button>{/if}
        </EmptyState>
      </Card>
    {:else}
      <div class="bar">
        <Segmented label={t('blocks.filter')} bind:value={filter} options={[
          { value: 'all', label: t('blocks.f.all') }, { value: 'kakao', label: t('blocks.f.kakao') }, { value: 'discord', label: t('blocks.f.discord') },
          { value: 'todo', label: t('blocks.f.todo') }, { value: 'ready', label: t('blocks.f.ready') }]} />
        <div class="search"><Input type="search" bind:value={q} placeholder={t('blocks.search')} aria-label={t('blocks.search')} /></div>
      </div>
      {#if shown.length === 0}
        <Card><EmptyState compact pose="search" title={t('blocks.none_match')} /></Card>
      {:else}
        <div class="grid" style:--min="330px">
          {#each shown as b, i (b.source_room)}
            {@const st = blockState(b)}
            <article class="block enter" style:--i={Math.min(i, 8)}>
              <header>
                <span class="ic {b.platform}" aria-hidden="true">{#if isKakao(b)}<I.MessagesSquare size={18} />{:else}<I.Hash size={18} />{/if}</span>
                <div class="nm">
                  <h2><a href={blockHref(b.source_room)}>{b.label}</a></h2>
                  <span class="xs muted">{t(`blocks.platform.${b.platform}`)}{b.own_channel ? ` · ${b.own_channel.guild_name}` : ''}</span>
                </div>
                <Status tone={stateTone[st]} label={t(`blocks.state.${st}`)} size="sm" />
              </header>
              {#if b.note}<p class="note small muted">{b.note}</p>{/if}
              <p class="meta small"><span class="num">{t('blocks.meta.messages', { n: num(b.message_count) })}</span>{#if b.speakers}<span>· {t('blocks.meta.people', { n: num(b.speakers) })}</span>{/if}{#if b.first}<span>· {period(b.first, b.last)}</span>{/if}{#if b.memory_estimate_bytes}<span class="mem" title={t('blocks.meta.memory_title')}>· {t('blocks.meta.memory', { size: bytes(b.memory_estimate_bytes) })}{b.resident === false ? ` (${t('blocks.meta.memory_not_loaded')})` : ''}</span>{/if}</p>

              {#if st === 'preparing'}
                <div class="job">
                  <Progress value={b.job.progress_pct ?? 0} label={t('blocks.preparing')} />
                  <div class="row small"><span class="muted">{jobLine(b.job)}</span><span class="num">{Math.round(b.job.progress_pct ?? 0)}%{etaText(b.job) ? ` · ${t('blocks.eta', { eta: etaText(b.job) })}` : ''}</span></div>
                  <div><Button size="sm" variant="ghost" icon={I.X} onclick={() => cancelJob(b)}>{t('common.cancel')}</Button></div>
                </div>
              {:else if st === 'needs_prep'}
                <div class="next"><span class="small">{t('blocks.next.needs_prep')}</span><Button size="sm" variant="primary" icon={I.Sparkles} onclick={() => navigate(blockHref(b.source_room) + '?prepare=1')}>{t('blocks.prepare')}</Button></div>
              {:else if st === 'partial'}
                <div class="next"><span class="small">{t('blocks.next.partial', { n: num(b.pending_leaves) })}</span><Button size="sm" icon={I.RefreshCw} onclick={() => prepare(b)}>{t('blocks.update')}</Button></div>
              {:else if st === 'failed'}
                <div class="next bad"><span class="small">{stopReason(b.error) || t('blocks.state.failed')}</span><Button size="sm" icon={I.RotateCcw} onclick={() => navigate(blockHref(b.source_room) + '?prepare=1')}>{t('blocks.retry')}</Button></div>
              {:else}
                <p class="small ok-line"><I.CircleCheck size={14} aria-hidden="true" /> {t('blocks.next.ready', { n: num(b.embedded) })}{b.prepared_at ? ` · ${relative(b.prepared_at)}` : ''}</p>
              {/if}

              {#if isKakao(b)}
                <div class="memrow"><span class="xs muted">{t('rm.ex.hint')}</span><Button size="sm" variant="ghost" icon={I.Layers} onclick={() => openExtract(b)}>{t('rm.ex.action')}</Button></div>
              {/if}

              <footer>
                <div class="links">
                  {#each b.channels ?? [] as c (c.guild_id + c.channel_id)}<span class="chip"><I.Hash size={12} aria-hidden="true" />{c.name}</span>{:else}<span class="xs muted">{t('blocks.no_links')}</span>{/each}
                </div>
                <Button size="sm" variant="ghost" icon={I.Link2} onclick={() => openConnect(b)} disabled={!channels.length}>{t('blocks.connect')}</Button>
              </footer>
              {#if b.own_channel && !b.own_channel.capture_on}<p class="xs warn-line"><I.TriangleAlert size={12} aria-hidden="true" /> {t('blocks.capture_off')}</p>{/if}
            </article>
          {/each}
        </div>
      {/if}
      {#if silent.length}
        <details class="silent">
          <summary class="small">{t('blocks.silent', { n: silent.length })}</summary>
          <ul>
            {#each silent as b (b.source_room)}
              <li><span class="small"><I.Hash size={13} aria-hidden="true" /> {b.label} <span class="xs muted">{b.own_channel?.guild_name ?? ''}</span></span>
                {#if b.own_channel?.capture_on}<span class="xs muted">{t('blocks.capture_waiting')}</span>{:else}<Button size="sm" variant="ghost" icon={I.Download} onclick={() => captureOn(b)}>{t('blocks.capture_on')}</Button>{/if}</li>
            {/each}
          </ul>
          <p class="xs muted">{t('blocks.silent_note')}</p>
        </details>
      {/if}
    {/if}
  {:else if view === 'channels'}
    {#if channels.length === 0}
      <Card><EmptyState pose="guide" title={t('connect.no_channels_title')} description={t('connect.no_channels')}><Button href="/rooms" icon={I.Hash}>{t('nav.rooms')}</Button><Button href="/connections" variant="ghost">{t('nav.connections')}</Button></EmptyState></Card>
    {:else}
      <p class="small muted lead">{t('connect.lead')}</p>
      {#each byGuild as [guild, list] (guild)}
        <Card padded={false} title={guild} class="guild">
          <ul class="chans">
            {#each list as c (c.guild_id + c.channel_id)}
              {@const own = ownBlock(c)}
              <li>
                <div class="ch">
                  <a href="/rooms/{c.guild_id}/{c.channel_id}" class="chn"><I.Hash size={15} aria-hidden="true" />{c.name}</a>
                  {#if c.memory_use === 'off'}<Status tone="neutral" label={t('connect.memory_off')} size="sm" />{/if}
                </div>
                <div class="attached">
                  <span class="chip own" title={t('connect.own_title')}>{t('connect.own')}{#if !(own?.message_count)} <span class="subtle">· {c.capture ? t('connect.own_waiting') : t('connect.own_off')}</span>{/if}</span>
                  {#each c.attached ?? [] as a (a.block_id)}
                    <span class="chip accent" class:stale={a.stale}>{#if a.stale}{t('connect.stale')}{:else}<a href={blockHref(a.source_room)}>{a.label}</a>{/if}
                      <button type="button" class="x" aria-label={t('connect.detach', { block: a.label || t('connect.stale') })} onclick={() => detachFrom(c, a)}><I.X size={12} /></button></span>
                  {/each}
                  <Popover width={300} align="start">
                    {#snippet trigger({ toggle, expanded, id })}
                      <button type="button" class="add" onclick={toggle} aria-expanded={expanded} aria-controls={id} disabled={!attachable(c).length}><I.Plus size={13} aria-hidden="true" />{t('connect.add')}</button>
                    {/snippet}
                    {#snippet children({ close })}
                      <div class="menu-h">{t('connect.add_to', { channel: c.name })}</div>
                      <ul class="menu">
                        {#each attachable(c) as b (b.source_room)}
                          {@const st = blockState(b)}
                          <li><button type="button" onclick={() => attachTo(c, b, close)}><span class="mi">{#if isKakao(b)}<I.MessagesSquare size={14} />{:else}<I.Hash size={14} />{/if}</span><span class="ml">{b.label}<span class="xs muted"> · {t('blocks.meta.messages', { n: num(b.message_count) })}</span></span>{#if st !== 'ready'}<Status tone={stateTone[st]} label={t(`blocks.state.${st}`)} size="sm" />{/if}</button></li>
                        {/each}
                      </ul>
                    {/snippet}
                  </Popover>
                </div>
              </li>
            {/each}
          </ul>
        </Card>
      {/each}
    {/if}
  {:else}
    <div class="cols">
      <Card title={t('index.state')}>
        <div class="stack">
          <div class="big"><span class="num">{Math.round(coverage * 100)}%</span><span class="small muted">{t('index.coverage')}</span></div>
          <Progress value={mem.counts?.leaves_embedded ?? 0} max={mem.counts?.leaves || 1} label={t('index.coverage')} tone={coverage >= 0.99 ? 'ok' : 'accent'} />
          <KeyValue items={[
            { k: t('sources.count.leaves'), v: num(mem.counts?.leaves ?? 0) },
            { k: t('sources.count.leaves_embedded'), v: num(mem.counts?.leaves_embedded ?? 0) },
            { k: t('sources.count.summaries'), v: num(mem.counts?.summaries ?? 0) },
            ...(mem.message_vectors?.enabled ? [
              { k: t('index.mv.width'), v: mem.message_vectors.dimensions ? `${num(mem.message_vectors.dimensions)} ${t('index.mv.dims')}` : t('index.mv.full_width') },
              { k: t('index.mv.stored'), v: num(mem.message_vectors.stored ?? 0) },
              { k: t('index.mv.pending'), v: num(mem.message_vectors.pending ?? 0) },
            ] : []),
            { k: t('index.loaded'), v: mem.loaded ? t('index.loaded_at', { n: num(mem.loaded.leaves), at: relative(mem.loaded.at) }) : t('index.not_loaded') },
          ]} />
        </div>
      </Card>
      {#if cfg.loaded}
        <Card title={t('blocks.set.prepare')} description={t('blocks.set.prepare_desc')}>
          <div class="stack">
            <Switch checked={!!cfg.doc.memory?.summaries} onchange={(v) => setMem('summaries', v)} label={t('index.summaries')} hint={t('index.summaries_hint')} />
            {#key mvKey}<Switch checked={messageVectorsOn(cfg.doc.memory)} onchange={toggleMessageVectors} label={t('index.message_vectors')} hint={t('index.message_vectors_hint')} />{/key}
            <Field id="ls" label={t('index.leaf_split')} hint={t('index.leaf_split_hint')}>
              {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={cfg.doc.memory?.leaf_split || 'time_gap'} onchange={(e) => setMem('leaf_split', (e.currentTarget as HTMLSelectElement).value)} options={[{ value: 'time_gap', label: t('index.split.time_gap') }, { value: 'fixed', label: t('index.split.fixed') }]} />{/snippet}
            </Field>
            <Field id="bd" label={t('blocks.set.budget')} hint={t('blocks.set.budget_hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} inputmode="decimal" value={cfg.doc.memory?.index_budget_usd ?? '1.00'} onchange={(e) => setMem('index_budget_usd', (e.currentTarget as HTMLInputElement).value.trim())} />{/snippet}
            </Field>
            <Field id="iw" label={t('cfg.memory.index_workers')} hint={t('cfg.memory.index_workers.hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" min="1" max="256" value={cfg.doc.memory?.index_workers || 8} onchange={(e) => setMem('index_workers', Math.min(32, Math.max(1, Math.round(Number((e.currentTarget as HTMLInputElement).value) || 8))))} />{/snippet}
            </Field>
          </div>
        </Card>
        <Card title={t('blocks.set.live')} description={t('blocks.set.live_desc')}>
          <div class="stack">
            <Switch checked={!!cfg.doc.memory?.capture} onchange={(v) => setMem('capture', v)} label={t('index.capture')} hint={t('index.capture_hint')} />
            <Switch checked={!!cfg.doc.memory?.embed} onchange={(v) => setMem('embed', v)} label={t('index.embed')} hint={t('index.embed_hint')} />
            <Field id="iv" label={t('index.interval')} hint={t('index.interval_hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" min="1" max="1440" value={cfg.doc.memory?.index_interval_min ?? 10} onchange={(e) => setMem('index_interval_min', Math.max(1, Math.round(Number((e.currentTarget as HTMLInputElement).value) || 10)))} />{/snippet}
            </Field>
          </div>
        </Card>
        <Card title={t('rm.set.title')} description={t('rm.set.desc')}>
          <div class="stack">
            <Field id="db" label={t('cfg.memory.derived_budget_usd')} hint={t('cfg.memory.derived_budget_usd.hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} inputmode="decimal" placeholder="3.00" value={cfg.doc.memory?.derived_budget_usd ?? ''} onchange={(e) => setMem('derived_budget_usd', (e.currentTarget as HTMLInputElement).value.trim())} />{/snippet}
            </Field>
            <Field id="dbd" label={t('cfg.memory.day_boundary')} hint={t('cfg.memory.day_boundary.hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="time" value={cfg.doc.memory?.day_boundary ?? '05:00'} onchange={(e) => setMem('day_boundary', (e.currentTarget as HTMLInputElement).value)} />{/snippet}
            </Field>
            <Switch checked={cfg.doc.memory?.episodes !== 'off'} onchange={(v) => setMem('episodes', v ? '' : 'off')} label={t('cfg.memory.episodes')} hint={t('cfg.memory.episodes.hint')} />
            <Switch checked={cfg.doc.memory?.state !== 'off'} onchange={(v) => setMem('state', v ? '' : 'off')} label={t('cfg.memory.state')} hint={t('cfg.memory.state.hint')} />
            <Switch checked={cfg.doc.memory?.cards === 'on'} onchange={(v) => setMem('cards', v ? 'on' : '')} label={t('cfg.memory.cards')} hint={t('cfg.memory.cards.hint')}>
              <Status tone="warn" label={t('conv.experimental')} size="sm" />
            </Switch>
            <p class="xs muted">{t('rm.set.where')} <a href="/memory?tab=room">{t('memory.tab.room')}</a></p>
          </div>
        </Card>
        <Card title={t('blocks.set.search')} description={t('blocks.set.search_desc')}>
          <div class="stack">
            <Field id="rt" label={t('index.retrieve')} hint={t(`index.retrieve_hint.${cfg.doc.memory?.retrieve ?? 'auto'}`)}>
              {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={cfg.doc.memory?.retrieve ?? 'auto'} onchange={(e) => setMem('retrieve', (e.currentTarget as HTMLSelectElement).value)} options={['auto', 'hybrid', 'rerank', 'off'].map((v) => ({ value: v, label: t(`index.retrieve.${v}`) }))} />{/snippet}
            </Field>
            <Field id="tb" label={t('blocks.set.turn_budget')} hint={t('blocks.set.turn_budget_hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} inputmode="decimal" value={cfg.doc.memory?.turn_budget_usd ?? '0.05'} onchange={(e) => setMem('turn_budget_usd', (e.currentTarget as HTMLInputElement).value.trim())} />{/snippet}
            </Field>
            <p class="xs muted">{t('blocks.set.lab_hint')} <a href="/lab">{t('nav.lab')}</a></p>
          </div>
        </Card>
      {/if}
    </div>
  {/if}
</div>

<Confirm bind:open={mvOpen} title={t('index.message_vectors_title')} body={mvBody(mvAsk)} confirmLabel={t('index.message_vectors_confirm')} onconfirm={confirmMessageVectors} />
<ImportWizard bind:open={wizardOpen} {channels} ondone={() => void data.refresh()} />
{#if extractFor}<ExtractMemory bind:open={extractOpen} block={extractFor} />{/if}
{#if connectFor}<ConnectDialog bind:open={connectOpen} block={connectFor} {channels} onsaved={() => void data.refresh()} />{/if}

<style>
  .summary {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    margin-bottom: var(--s-4);
  }
  .summary > div {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .summary strong {
    font-size: var(--text-xl);
    letter-spacing: -0.02em;
  }
  .summary .warn strong {
    color: var(--warn);
  }
  .panel {
    margin-top: var(--s-4);
  }
  .bar {
    display: flex;
    gap: var(--s-3);
    flex-wrap: wrap;
    align-items: center;
    margin-bottom: var(--s-4);
  }
  .search {
    flex: 0 1 280px;
  }
  .block {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding: var(--pad-card);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    min-width: 0;
  }
  .block header {
    display: flex;
    align-items: center;
    gap: var(--s-3);
  }
  .ic {
    flex: none;
    display: grid;
    place-items: center;
    width: 2.25em;
    height: 2.25em;
    border-radius: var(--r-lg);
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .ic.discord {
    background: var(--info-soft);
    color: var(--info);
  }
  .nm {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .nm h2 {
    font-size: var(--text-md);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .nm h2 a {
    color: var(--text);
    text-decoration: none;
  }
  .nm h2 a:hover {
    text-decoration: underline;
  }
  .note {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    color: var(--text-muted);
  }
  .job,
  .next {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .job .row {
    justify-content: space-between;
  }
  .next {
    flex-direction: row;
    align-items: center;
    justify-content: space-between;
    gap: var(--s-3);
    background: var(--warn-soft);
  }
  .next.bad {
    background: var(--danger-soft);
  }
  .memrow {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .ok-line {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--ok);
  }
  .warn-line {
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--warn);
  }
  .block footer {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    justify-content: space-between;
    margin-top: auto;
    padding-top: var(--s-2);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .links {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    min-width: 0;
  }
  .links .chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
  }
  .silent {
    margin-top: var(--s-4);
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .silent summary {
    cursor: pointer;
    color: var(--text-muted);
  }
  .silent ul {
    list-style: none;
    margin: var(--s-2) 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .silent li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s-2);
  }
  .lead {
    margin-bottom: var(--s-3);
  }
  :global(.guild) {
    margin-bottom: var(--s-4);
  }
  .chans {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .chans li {
    display: grid;
    grid-template-columns: minmax(140px, 220px) 1fr;
    gap: var(--s-3);
    align-items: center;
    padding: var(--s-3) var(--pad-card);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .chans li:last-child {
    box-shadow: none;
  }
  @media (max-width: 640px) {
    .chans li {
      grid-template-columns: 1fr;
    }
  }
  .ch {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    min-width: 0;
  }
  .chn {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-weight: 600;
    color: var(--text);
    text-decoration: none;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .attached {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .attached .chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .attached .chip a {
    color: inherit;
    text-decoration: none;
  }
  .chip.own {
    background: var(--surface-2);
    color: var(--text-muted);
  }
  .chip.stale {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .x {
    display: inline-grid;
    place-items: center;
    padding: 0;
    width: 16px;
    height: 16px;
    border: 0;
    border-radius: 999px;
    background: none;
    color: inherit;
    cursor: pointer;
  }
  .x:hover {
    background: color-mix(in oklab, currentColor 18%, transparent);
  }
  .add {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 26px;
    padding: 0 10px;
    border: 1px dashed var(--border-strong);
    border-radius: 999px;
    background: none;
    color: var(--text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .add:hover:not(:disabled) {
    color: var(--accent-text);
    border-color: var(--accent);
  }
  .add:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .menu-h {
    padding: 4px 10px 2px;
    color: var(--text-subtle);
    font-size: var(--text-xs);
    font-weight: 600;
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
  .mi {
    flex: none;
    color: var(--text-muted);
    display: inline-flex;
  }
  .ml {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 380px), 1fr));
  }
  .big {
    display: flex;
    align-items: baseline;
    gap: var(--s-2);
  }
  .big .num {
    font-size: var(--text-3xl);
    font-weight: 700;
    letter-spacing: -0.03em;
  }
</style>
