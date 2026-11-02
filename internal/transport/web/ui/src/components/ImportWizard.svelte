<script lang="ts">
  import { brand } from 'virtual:brand'
  import { fly } from 'svelte/transition'
  import { t } from '../lib/i18n.svelte'
  import { examplePath } from '../lib/platform'
  import { call, upload } from '../lib/api.svelte'
  import { cfg } from '../lib/config.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import { num, bytes, usd, duration } from '../lib/format'
  import { period, roomFromFile, blockHref, batchOffer, effectiveMode, type PrepareMode } from '../lib/blocks'
  import { toast } from '../lib/toast.svelte'
  import Dialog from './Dialog.svelte'
  import Button from './Button.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Select from './Select.svelte'
  import Progress from './Progress.svelte'
  import KeyValue from './KeyValue.svelte'
  import BatchChoice from './BatchChoice.svelte'
  import * as I from '../lib/icons'

  // A KakaoTalk export becomes a memory block: file → check → import → prepare for RAG (background
  // job, estimate first) → connect to channels. Every step after the import can be skipped and done
  // later from the block's page.
  let { open = $bindable(false), channels = [], ondone }: { open?: boolean; channels?: any[]; ondone?: (room: string) => void } = $props()

  const d = $derived(motionAllowed() ? 1 : 0)
  const steps = ['file', 'check', 'prepare', 'connect', 'done'] as const
  let step = $state(0)
  let file = $state<File | null>(null)
  let serverPath = $state('')
  let showServerPath = $state(false)
  let dragging = $state(false)
  let pct = $state<number | undefined>()
  let uploaded = $state('')
  let preview = $state<any>(null)
  let checking = $state(false)
  let label = $state('')
  let room = $state('')
  let tz = $state('Asia/Seoul')
  let exportLocale = $state('ko')
  let showAdvanced = $state(false)
  let importing = $state(false)
  let imported = $state<any>(null)
  let estimate = $state<any>(null)
  let estimating = $state(false)
  let budget = $state('')
  let mode = $state<PrepareMode>('live')
  let starting = $state(false)
  let jobStarted = $state(false)
  let batchStarted = $state(false)
  let picked = $state<string[]>([])
  let connecting = $state(false)
  let err = $state('')

  $effect(() => {
    if (open && step === 0 && !file && !serverPath) reset()
  })
  function reset() {
    step = 0
    file = null
    serverPath = ''
    pct = undefined
    uploaded = ''
    preview = null
    label = ''
    room = ''
    tz = cfg.doc?.memory?.naive_timezone || cfg.doc?.i18n?.timezone || 'Asia/Seoul'
    exportLocale = 'ko'
    imported = null
    estimate = null
    budget = cfg.doc?.memory?.index_budget_usd ?? '1.00'
    mode = 'live'
    jobStarted = false
    batchStarted = false
    picked = []
    err = ''
  }
  function choose(f: File) {
    err = ''
    if (!/\.csv$/i.test(f.name) && f.type !== 'text/csv') {
      err = t('wizard.err.not_csv')
      return
    }
    if (f.size === 0) {
      err = t('wizard.err.empty')
      return
    }
    file = f
    uploaded = ''
    preview = null
    label = f.name.replace(/\.[^.]+$/, '').slice(0, 80)
    room = roomFromFile(f.name)
  }
  function pick(e: Event) {
    const f = (e.currentTarget as HTMLInputElement).files?.[0]
    if (f) choose(f)
  }
  function drop(e: DragEvent) {
    e.preventDefault()
    dragging = false
    const f = e.dataTransfer?.files?.[0]
    if (f) choose(f)
  }
  async function toCheck() {
    err = ''
    if (!file && serverPath.trim()) {
      const name = serverPath.trim().split(/[\\/]/).pop() || 'chat.csv'
      label = label || name.replace(/\.[^.]+$/, '')
      room = room || roomFromFile(name)
    }
    step = 1
    await runPreview()
  }
  async function ensurePath(): Promise<string> {
    if (!file) return serverPath.trim()
    if (uploaded) return uploaded
    const up = await upload(file, (p) => (pct = p))
    if (!up.ok) throw new Error(up.error ?? t('common.failed'))
    uploaded = up.path!
    return uploaded
  }
  async function runPreview() {
    checking = true
    err = ''
    try {
      const path = await ensurePath()
      const r = await call('imports.preview', { source: 'kakao_csv', path, room, export_locale: exportLocale })
      if (r.ok) preview = r.output
      else err = r.error?.message ?? t('common.failed')
    } catch (e) {
      err = (e as Error).message
    }
    checking = false
  }
  const existing = $derived(preview?.room?.messages > 0)
  function separate() {
    // a new room key so this export does not merge into the block that already uses the name
    const base = room.replace(/-\d+$/, '')
    let n = 2
    while (n < 100 && base + '-' + n === room) n++
    room = `${base}-${n}`
    void runPreview()
  }
  async function doImport() {
    err = ''
    if (!/^kakao:[^\s]+$/.test(room)) return void (err = t('wizard.err.room'))
    if (!label.trim()) return void (err = t('wizard.err.label'))
    importing = true
    let path = ''
    try {
      path = await ensurePath()
    } catch (e) {
      importing = false
      return void (err = (e as Error).message)
    }
    const r = await call('imports.start', { source: 'kakao_csv', path, room, label: label.trim(), timezone: tz, export_locale: exportLocale })
    importing = false
    if (!r.ok) return void (err = r.error?.message ?? t('common.failed'))
    imported = r.output.result
    toast('ok', t('wizard.imported'), t('wizard.imported_body', { n: num(imported.inserted) }))
    step = 2
    void runEstimate()
  }
  async function runEstimate() {
    estimating = true
    const r = await call('memory.blocks.prepare', { source_room: room, dry_run: true, mode: 'batch' }) // one dry run, both prices
    estimating = false
    if (r.ok) estimate = r.output
    else err = r.error?.message ?? ''
  }
  async function startPrepare() {
    err = ''
    const b = parseFloat(budget)
    if (Number.isNaN(b) || b < 0 || b > 1000) return void (err = t('wizard.err.budget'))
    starting = true
    const r = await call('memory.blocks.prepare', { source_room: room, background: true, budget_usd: String(b), mode: chosen })
    starting = false
    if (!r.ok) return void (err = r.error?.message ?? t('common.failed'))
    if (r.output.batch_resumed) toast('info', t('blocks.batch_resumed'), label)
    jobStarted = true
    batchStarted = r.output.mode === 'batch'
    step = channels.length ? 3 : 4
  }
  function skipPrepare() {
    step = channels.length ? 3 : 4
  }
  async function connect() {
    connecting = true
    err = ''
    const list = await call('memory.blocks.show', { source_room: room })
    const blockID = list.ok ? list.output.block.block_id : ''
    let failed = 0
    for (const key of picked) {
      const [g, c] = key.split('/')
      const r = await call('channels.view.attach', { guild_id: g, channel_id: c, block_id: blockID })
      if (!r.ok) failed++
    }
    connecting = false
    if (failed) toast('warn', t('wizard.connect_partial', { n: failed }))
    else if (picked.length) toast('ok', t('wizard.connected', { n: picked.length }))
    step = 4
  }
  function finish() {
    open = false
    ondone?.(room)
    reset()
  }
  const est = $derived(estimate?.report?.estimate)
  const chosen = $derived(effectiveMode(mode, estimate?.report?.estimate))
  const llmReady = $derived(estimate?.llm_configured !== false)
</script>

<Dialog bind:open title={t('wizard.title')} description={t('wizard.desc')} size="lg" onclose={() => step === 4 && finish()}>
  <ol class="steps" aria-label={t('wizard.progress')}>
    {#each steps.slice(0, 4) as s, i}
      <li class:on={i === step} class:done={i < step} aria-current={i === step ? 'step' : undefined}><span class="n num">{i < step ? '✓' : i + 1}</span><span class="l">{t(`wizard.step.${s}`)}</span></li>
    {/each}
  </ol>
  {#key step}
    <div class="stack body" in:fly={{ x: 16 * d, duration: 220 * d, opacity: 0 }}>
      {#if step === 0}
        <label class="drop" class:dragging ondragover={(e) => { e.preventDefault(); dragging = true }} ondragleave={() => (dragging = false)} ondrop={drop}>
          <input type="file" accept=".csv,text/csv" onchange={pick} />
          <I.FileUp size={28} aria-hidden="true" />
          {#if file}<strong class="break">{file.name}</strong><span class="small muted">{bytes(file.size)}</span>
          {:else}<strong>{t('wizard.drop')}</strong><span class="small muted">{t('wizard.drop_hint')}</span>{/if}
        </label>
        <details class="how">
          <summary class="small">{t('wizard.how_title')}</summary>
          <ol class="small muted"><li>{t('wizard.how.1')}</li><li>{t('wizard.how.2')}</li><li>{t('wizard.how.3')}</li></ol>
        </details>
        <button type="button" class="linkish small" onclick={() => (showServerPath = !showServerPath)} aria-expanded={showServerPath}>{t('wizard.server_path_toggle')}</button>
        {#if showServerPath}
          <Field id="wz-sp" label={t('sources.server_path')} hint={t('sources.server_path_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={serverPath} mono placeholder={examplePath('chat.csv')} disabled={!!file} />{/snippet}</Field>
        {/if}
      {:else if step === 1}
        {#if pct !== undefined && pct < 100}<Progress value={pct} label={t('sources.uploading')} /><p class="xs muted num">{t('sources.uploading')} {pct}%</p>{/if}
        {#if checking}<p class="small muted">{t('wizard.checking')}</p>{/if}
        {#if preview}
          {#if preview.duplicate_file}
            <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('sources.pv.duplicate')}</span></div>
          {:else if existing}
            <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('wizard.merge_into', { n: num(preview.room.messages) })} <button type="button" class="linkish" onclick={separate}>{t('wizard.separate')}</button></span></div>
          {/if}
          <div class="pv-grid">
            <div><span class="xs muted">{t('sources.pv.rows')}</span><strong class="num">{num(preview.rows)}</strong></div>
            <div><span class="xs muted">{t('sources.pv.new')}</span><strong class="num">{num(preview.new)}</strong></div>
            <div><span class="xs muted">{t('sources.pv.speakers')}</span><strong class="num">{num(preview.speakers)}</strong></div>
            <div><span class="xs muted">{t('sources.pv.period')}</span><strong>{period(preview.first, preview.last) || '–'}</strong></div>
          </div>
          {#if preview.rows > preview.eligible}<p class="xs muted">{t('wizard.skipped_rows', { n: num(preview.rows - preview.eligible) })}</p>{/if}
          {#if preview.top_speakers?.length}<div class="chips">{#each preview.top_speakers as sp}<span class="chip">{sp.name} <span class="num subtle">{num(sp.n)}</span></span>{/each}</div>{/if}
        {/if}
        <Field id="wz-label" label={t('wizard.label')} hint={t('wizard.label_hint')} error={err === t('wizard.err.label') ? err : undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={label} maxlength={80} />{/snippet}</Field>
        <button type="button" class="linkish small" onclick={() => (showAdvanced = !showAdvanced)} aria-expanded={showAdvanced}>{t('wizard.advanced')}</button>
        {#if showAdvanced}
          <div class="form-grid">
            <Field id="wz-tz" label={t('sources.timezone')} hint={t('sources.timezone_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={tz} mono />{/snippet}</Field>
            <Field id="wz-el" label={t('sources.export_locale')} hint={t('sources.export_locale_hint')}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={exportLocale} onchange={() => void runPreview()} options={[{ value: 'ko', label: '한국어' }, { value: 'en', label: 'English' }]} />{/snippet}</Field>
            <Field id="wz-room" label={t('wizard.room')} hint={t('wizard.room_hint')} error={err === t('wizard.err.room') ? err : undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={room} mono onchange={() => void runPreview()} />{/snippet}</Field>
          </div>
        {/if}
      {:else if step === 2}
        <div class="callout ok"><span class="ic"><I.CircleCheck size={16} /></span><span>{t('wizard.imported_line', { n: num(imported?.inserted ?? 0), dup: num(imported?.duplicate ?? 0) })}</span></div>
        <p class="small">{t('wizard.prepare_lead')}</p>
        {#if estimating}<p class="small muted">{t('wizard.estimating')}</p>{/if}
        {#if est}
          <div class="pv-grid">
            <div><span class="xs muted">{t('blocks.est.pieces')}</span><strong class="num">{num(est.leaf_embeds + est.cached_leaves)}</strong></div>
            <div><span class="xs muted">{t('blocks.est.time')}</span><strong>{chosen === 'batch' ? t('blocks.est.batch_time') : est.seconds < 60 ? t('blocks.est.under_minute') : t('blocks.est.approx', { d: duration(est.seconds) })}</strong></div>
            <div><span class="xs muted">{t('blocks.est.cost')}</span><strong class="num">{usd(chosen === 'batch' ? (batchOffer(est)?.batch ?? est.total_usd) : est.total_usd)}</strong></div>
            <div><span class="xs muted">{t('blocks.est.summaries')}</span><strong class="num">{num(est.summaries)}</strong></div>
          </div>
          <p class="xs muted">{est.price_basis === 'fallback' ? t('blocks.est.basis_fallback') : t('blocks.est.basis_prices')}</p>
        {/if}
        {#if !llmReady}
          <div class="callout warn"><span class="ic"><I.KeyRound size={16} /></span><span>{t('blocks.no_llm')} <a href="/connections">{t('nav.connections')}</a></span></div>
        {/if}
        {#if est}<BatchChoice id="wz-mode" bind:mode {est} />{/if}
        <Field id="wz-budget" label={t('blocks.budget')} hint={t('blocks.budget_hint')} error={err === t('wizard.err.budget') ? err : undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={budget} inputmode="decimal" />{/snippet}</Field>
      {:else if step === 3}
        <p class="small">{jobStarted ? t('wizard.connect_lead_running') : t('wizard.connect_lead')}</p>
        <ul class="pick">
          {#each channels as c (c.guild_id + '/' + c.channel_id)}
            {@const key = c.guild_id + '/' + c.channel_id}
            <li><label><input type="checkbox" checked={picked.includes(key)} onchange={(e) => (picked = (e.currentTarget as HTMLInputElement).checked ? [...picked, key] : picked.filter((x) => x !== key))} />
              <span><strong>#{c.name}</strong> <span class="xs muted">{c.guild_name}</span></span></label></li>
          {/each}
        </ul>
      {:else}
        <div class="done"><img src={brand('mascot/mong-party.webp')} alt="" width="96" height="96" /><div class="stack">
          <h3>{t('wizard.done_title')}</h3>
          <p class="small muted">{batchStarted ? t('wizard.done_batch') : jobStarted ? t('wizard.done_running') : t('wizard.done_later')}</p>
        </div></div>
      {/if}
      {#if err && ![t('wizard.err.label'), t('wizard.err.room'), t('wizard.err.budget')].includes(err)}<p class="small err" role="alert">{err}</p>{/if}
    </div>
  {/key}
  {#snippet footer()}
    {#if step === 0}
      <Button variant="ghost" onclick={() => (open = false)}>{t('common.cancel')}</Button>
      <Button variant="primary" iconEnd={I.ArrowRight} disabled={!file && !serverPath.trim()} onclick={toCheck}>{t('setup.next')}</Button>
    {:else if step === 1}
      <Button variant="ghost" icon={I.ChevronLeft} onclick={() => (step = 0)}>{t('setup.back')}</Button>
      <Button variant="primary" icon={I.Download} loading={importing} disabled={checking || !preview || !!preview?.duplicate_file} onclick={doImport}>{t('wizard.import')}</Button>
    {:else if step === 2}
      <Button variant="ghost" onclick={skipPrepare}>{t('wizard.later')}</Button>
      <Button variant="primary" icon={I.Sparkles} loading={starting} disabled={estimating || !llmReady} onclick={startPrepare}>{t('blocks.prepare_start')}</Button>
    {:else if step === 3}
      <Button variant="ghost" onclick={() => (step = 4)}>{t('wizard.skip')}</Button>
      <Button variant="primary" icon={I.Link2} loading={connecting} disabled={!picked.length} onclick={connect}>{t('wizard.connect', { n: picked.length })}</Button>
    {:else}
      <Button variant="ghost" href={blockHref(room)} onclick={finish}>{t('wizard.open_block')}</Button>
      <Button variant="primary" onclick={finish}>{t('common.done')}</Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  .steps {
    list-style: none;
    margin: 0 0 var(--s-4);
    padding: 0;
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--s-2);
  }
  .steps li {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    color: var(--text-subtle);
    font-size: var(--text-sm);
  }
  .steps .n {
    flex: none;
    display: grid;
    place-items: center;
    width: 1.6em;
    height: 1.6em;
    border-radius: 999px;
    background: var(--surface-2);
    font-size: var(--text-xs);
    font-weight: 650;
  }
  .steps .l {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .steps li.on {
    color: var(--text);
    font-weight: 600;
  }
  .steps li.on .n {
    background: var(--accent);
    color: var(--on-accent);
  }
  .steps li.done .n {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .body {
    min-height: 220px;
  }
  .drop {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 6px;
    padding: var(--s-8) var(--s-4);
    border-radius: var(--r-xl);
    background: var(--surface-2);
    box-shadow: inset 0 0 0 2px var(--border);
    color: var(--text-muted);
    text-align: center;
    cursor: pointer;
    transition-property: box-shadow, background-color;
    transition-duration: var(--dur);
  }
  .drop.dragging,
  .drop:hover {
    box-shadow: inset 0 0 0 2px var(--accent);
    background: var(--accent-soft);
  }
  .drop input {
    position: absolute;
    inset: 0;
    opacity: 0;
    cursor: pointer;
  }
  .drop:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .how summary {
    cursor: pointer;
    color: var(--text-muted);
  }
  .how ol {
    margin: 6px 0 0;
    padding-inline-start: 1.4em;
  }
  .linkish {
    align-self: flex-start;
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-text);
    cursor: pointer;
    text-decoration: underline;
    text-underline-offset: 2px;
  }
  .pv-grid {
    display: grid;
    gap: var(--s-2);
    grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .pv-grid div {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .pv-grid strong {
    font-size: var(--text-lg);
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .pick {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
    max-height: 320px;
    overflow: auto;
  }
  .pick label {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 10px;
    border-radius: var(--r);
    cursor: pointer;
  }
  .pick label:hover {
    background: var(--surface-2);
  }
  .pick input {
    accent-color: var(--accent);
    width: 16px;
    height: 16px;
  }
  .done {
    display: flex;
    gap: var(--s-4);
    align-items: center;
  }
  .err {
    color: var(--danger);
  }
</style>
