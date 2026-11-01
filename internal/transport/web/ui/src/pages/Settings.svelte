<script lang="ts">
  import { t, has, i18n } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { tp } from '../lib/platform'
  import { call, can } from '../lib/api.svelte'
  import Field from '../components/Field.svelte'
  import Dialog from '../components/Dialog.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, configSource, useConfig, patchConfig, loadConfig, type Op } from '../lib/config.svelte'
  import { route, setQuery } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import { datetime, relative } from '../lib/format'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Status from '../components/Status.svelte'
  import SaveBar from '../components/SaveBar.svelte'
  import SchemaField from '../components/SchemaField.svelte'
  import Confirm from '../components/Confirm.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Segmented from '../components/Segmented.svelte'
  import ChangeList from '../components/ChangeList.svelte'
  import * as I from '../lib/icons'

  useConfig()
  let tab = $state(route.query.get('tab') ?? 'form')
  let section = $state(route.query.get('section') ?? 'instance')
  // on a narrow screen the sections are a sideways row: keep the chosen one in sight
  let secsEl = $state<HTMLElement>()
  $effect(() => {
    void section
    const on = secsEl?.querySelector<HTMLElement>('button.on')
    const ul = on?.closest('ul')
    if (!on || !ul || ul.scrollWidth <= ul.clientWidth) return
    const a = on.getBoundingClientRect(), b = ul.getBoundingClientRect()
    ul.scrollTo({ left: ul.scrollLeft + a.left - b.left - (b.width - a.width) / 2 })
  })
  let filter = $state('')
  let draft = $state<any>(null)
  let edits = $state<Record<string, Op>>({})
  let saving = $state(false)
  let seenRev = 0
  $effect(() => {
    if (cfg.loaded && (draft === null || (cfg.revision !== seenRev && Object.keys(edits).length === 0))) {
      draft = JSON.parse(JSON.stringify(cfg.doc))
      seenRev = cfg.revision
    }
  })
  // admin.* (console transport and address) is not edited here: the console always listens on
  // loopback and remote access is an SSH tunnel.
  const hiddenSections = new Set(['schema_version', 'admin'])
  // Settings that have their own page are changed there (with checks, previews and plain words); here
  // they point at it, and their raw fields stay one click away for whoever wants them.
  const owner: Record<string, { href: string; page: string }> = {
    discord: { href: '/connections', page: 'connections' },
    providers: { href: '/connections', page: 'connections' },
    models: { href: '/connections', page: 'connections' },
    limits: { href: '/usage', page: 'usage' },
    memory: { href: '/blocks?view=settings', page: 'blocks' },
    backup: { href: '/maintenance', page: 'maintenance' },
    tools: { href: '/tools', page: 'tools' },
    chat: { href: '/tools', page: 'tools' },
    web_search: { href: '/tools/search.web', page: 'tools' },
    disaster_alerts: { href: '/tools/disasters.query?tab=ops', page: 'tools' },
    guilds: { href: '/rooms', page: 'rooms' },
    personas: { href: '/persona', page: 'persona' },
    access: { href: '/people', page: 'people' },
    alerts: { href: '/people', page: 'people' },
    dm: { href: '/people', page: 'people' },
  }
  let rawOpen = $state<Record<string, boolean>>({})
  const sections = $derived((cfg.schema?.fields ?? []).map((f) => f.k).filter((k) => !hiddenSections.has(k)))
  const sectionSchema = $derived(cfg.schema?.fields?.find((f) => f.k === section)?.s)
  function setIn(obj: any, path: string[], v: any) {
    let cur = obj
    for (let i = 0; i < path.length - 1; i++) {
      if (cur[path[i]] === undefined || cur[path[i]] === null) cur[path[i]] = {}
      cur = cur[path[i]]
    }
    if (v === undefined) delete cur[path[path.length - 1]]
    else cur[path[path.length - 1]] = v
  }
  function onedit(path: string[], v: any) {
    setIn(draft, path, v)
    edits[path.join('\u0000')] = v === undefined ? { path, op: 'unset' } : { path, value: v }
  }
  async function save() {
    saving = true
    // saved against the revision the draft started from: if the configuration changed elsewhere since,
    // the server refuses (a conflict) instead of this form silently overwriting that change
    let r
    try {
      r = await patchConfig(Object.values(edits), { base: seenRev })
    } finally {
      saving = false
    }
    if (r.ok) {
      edits = {}
      draft = JSON.parse(JSON.stringify(cfg.doc))
      seenRev = cfg.revision
    } else if (r.error?.code === 'CONFIG_CONFLICT') {
      // the newest configuration with these edits on top: look, then save again on purpose
      draft = JSON.parse(JSON.stringify(cfg.doc))
      for (const e of Object.values(edits)) setIn(draft, e.path, e.op === 'unset' ? undefined : e.value)
      seenRev = cfg.revision
    }
  }
  function reset() {
    edits = {}
    draft = JSON.parse(JSON.stringify(cfg.doc))
  }
  const label = (k: string) => (has(`cfg.${k}`) ? t(`cfg.${k}`) : k)

  // YAML
  const yaml = resource<any>('config.show', () => ({}), { live: ['config'] })
  let importText = $state('')
  let diff = $state<any>(null)
  let importOpen = $state(false)
  async function checkImport() {
    const r = await call('config.diff', { yaml: importText })
    diff = r.ok ? r.output : { valid: false, error: r.error?.message }
  }
  async function applyImport() {
    const r = await call('config.apply', { yaml: importText }, { base_revision: cfg.revision })
    if (r.ok) {
      toast('ok', t('settings.imported'))
      importText = ''
      diff = null
      await loadConfig(true)
    } else toast('error', t('config.save_failed'), r.error?.message)
  }
  async function readFile(e: Event) {
    const f = (e.currentTarget as HTMLInputElement).files?.[0]
    if (f) importText = await f.text()
  }

  // history
  // change log: filtered on the server (where from, result), newest first, older pages on demand
  let hVia = $state<'all' | 'gui' | 'discord' | 'cli'>('all')
  const viaFilter = { all: '', gui: 'gui', discord: 'discord,command,chat', cli: 'cli,api' } as const
  const audit = resource<any>('audit.list', () => ({ limit: 200, via: viaFilter[hVia] }), { live: ['audit'] })
  let older = $state<any[]>([])
  let olderNext = $state<number | null>(null)
  let olderBusy = $state(false)
  $effect(() => {
    void audit.data
    older = []
    olderNext = null
  })
  const nextBefore = $derived(olderNext ?? audit.data?.next_before_id ?? null)
  async function loadOlder() {
    if (!nextBefore) return
    olderBusy = true
    const r = await call('audit.list', { limit: 200, via: viaFilter[hVia], before_id: nextBefore })
    olderBusy = false
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    older = [...older, ...(r.output.entries ?? [])]
    olderNext = r.output.next_before_id ?? 0
  }
  function exportCSV() {
    const cell = (v: unknown) => `"${String(v ?? '').replaceAll('"', '""')}"`
    const head = ['at', 'action', 'via', 'ok', 'code', 'revision_before', 'revision_after', 'actor', 'actor_id', 'guild_id', 'summary']
    const lines = [head.join(','), ...history.map((e) => head.map((k) => cell(e[k])).join(','))]
    const url = URL.createObjectURL(new Blob(['\ufeff' + lines.join('\n')], { type: 'text/csv;charset=utf-8' }))
    const a = document.createElement('a')
    a.href = url
    a.download = `masamong-changes-${new Date().toISOString().slice(0, 10)}.csv`
    a.click()
    URL.revokeObjectURL(url)
  }
  let rollbackTo = $state(0)
  let rbOpen = $state(false)
  let hResult = $state<'all' | 'ok' | 'failed'>('all')
  let hKind = $state<'config' | 'all'>('config')
  let hQuery = $state('')
  const actionLabel = (id: string) => (has(`audit.a.${id}`) ? t(`audit.a.${id}`) : id)
  const history = $derived(
    ([...(audit.data?.entries ?? []), ...older] as any[])
      .filter((e) => hResult === 'all' || (hResult === 'ok') === !!e.ok)
      .filter((e) => hKind === 'all' || e.revision_after !== e.revision_before)
      .filter((e) => !hQuery || `${e.action} ${actionLabel(e.action)} ${e.summary} ${e.actor} ${e.actor_id} ${e.guild_id}`.toLowerCase().includes(hQuery.toLowerCase())),
  )
  const byDay = $derived.by(() => {
    const m = new Map<string, any[]>()
    for (const e of history) {
      const d = new Date(e.at)
      const key = Number.isNaN(d.getTime()) ? e.at.slice(0, 10) : new Intl.DateTimeFormat(i18n.lang || 'en', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'short' }).format(d)
      m.set(key, [...(m.get(key) ?? []), e])
    }
    return [...m.entries()]
  })
  let changesFor = $state<any>(null)
  let changesOut = $state<any>(null)
  let changesOpen = $state(false)
  async function showChanges(e: any) {
    changesFor = e
    changesOut = null
    changesOpen = true
    const r = await call('config.changes', { from: e.revision_before, to: e.revision_after })
    changesOut = r.ok ? r.output : { error: r.error?.message }
  }
  async function rollback() {
    const r = await call('config.rollback', { revision: rollbackTo })
    r.ok ? toast('ok', t('settings.rolled_back', { rev: rollbackTo })) : toast('error', r.error?.message ?? '')
    await loadConfig(true)
  }
  $effect(() => {
    setQuery({ tab: tab === 'form' ? null : tab, section: tab === 'form' ? section : null })
  })

  // snapshots: named copies of the whole config
  const snaps = resource<any>('config.snapshots', () => ({}), { live: ['audit', 'config'] })
  let snapName = $state('')
  let snapNote = $state('')
  let snapBusy = $state(false)
  async function saveSnap(e: SubmitEvent) {
    e.preventDefault()
    if (!snapName.trim()) return
    snapBusy = true
    const r = await call('config.snapshot', { name: snapName.trim(), note: snapNote.trim() })
    snapBusy = false
    if (r.ok) {
      toast('ok', t('settings.snap_saved', { name: snapName.trim() }))
      snapName = ''
      snapNote = ''
      void snaps.refresh()
    } else toast('error', t('common.failed'), r.error?.message)
  }
  let snapFor = $state<any>(null)
  let snapDiff = $state<any>(null)
  let snapOpen = $state(false)
  let snapConfirm = $state(false)
  async function previewSnap(sn: any) {
    snapFor = sn
    snapDiff = null
    snapOpen = true
    const r = await call('config.snapshot.restore', { id: sn.id, dry_run: true })
    snapDiff = r.ok ? r.output : { error: r.error?.message }
  }
  async function restoreSnap() {
    const r = await call('config.snapshot.restore', { id: snapFor.id }, { base_revision: cfg.revision })
    if (r.ok) {
      toast('ok', t('settings.snap_restored', { name: snapFor.name }))
      snapOpen = false
      await loadConfig(true)
    } else toast('error', t('config.save_failed'), r.error?.message)
  }
  async function deleteSnap(id: string) {
    const r = await call('config.snapshot.delete', { id })
    if (r.ok) void snaps.refresh()
    else toast('error', t('common.failed'), r.error?.message)
  }
</script>

<PageHeader title={t('settings.title')} description={t('settings.desc', { rev: cfg.revision })}>
  {#snippet actions()}
    <Button href="/api/export/config" data-native icon={I.Download}>{t('settings.export')}</Button>
  {/snippet}
</PageHeader>

<div data-tour="settings-tabs"><Tabs id="settings" bind:active={tab} tabs={[{ id: 'form', label: t('settings.tab.form'), icon: I.SlidersHorizontal }, { id: 'yaml', label: t('settings.tab.yaml'), icon: I.FileText }, { id: 'snapshots', label: t('settings.tab.snapshots'), icon: I.Camera }, { id: 'history', label: t('settings.tab.history'), icon: I.History }]} /></div>

<div class="panel" role="tabpanel" id="settings-panel-{tab}" aria-labelledby="settings-tab-{tab}">
  {#if !cfg.loaded || !draft}
    <LoadState res={configSource}><Card><Skeleton lines={10} /></Card></LoadState>
  {:else if tab === 'form'}
    <div class="layout">
      <nav class="secs" aria-label={t('settings.sections')} bind:this={secsEl}>
        <Input type="search" bind:value={filter} placeholder={t('settings.filter')} aria-label={t('settings.filter')} />
        <ul>
          {#each sections as s}
            <li><button type="button" class:on={s === section} aria-current={s === section ? 'true' : undefined} onclick={() => (section = s)}>{label(s)}{#if Object.keys(edits).some((k) => k.startsWith(s + '\u0000'))}<span class="dot" aria-label={t('settings.has_edits')}></span>{/if}</button></li>
          {/each}
        </ul>
      </nav>
      <Card title={label(section)} description={has(`cfg.${section}.desc`) ? t(`cfg.${section}.desc`) : undefined}>
        {#if owner[section]}
          <div class="callout owner"><span class="ic"><I.ArrowUpRight size={16} /></span>
            <span class="small grow">{t('settings.owner', { page: t(`nav.${owner[section].page}`) })}</span>
            <Button size="sm" variant="primary" href={owner[section].href}>{t('settings.owner_open', { page: t(`nav.${owner[section].page}`) })}</Button>
          </div>
          {#if !rawOpen[section] && !filter}<Button size="sm" variant="ghost" icon={I.PencilLine} onclick={() => (rawOpen[section] = true)}>{t('settings.owner_raw')}</Button>{/if}
        {/if}
        {#if sectionSchema && (!owner[section] || rawOpen[section] || filter)}
          {#key section}
            <div class="enter">
              {#if sectionSchema.t === 'object'}
                <SchemaField schema={sectionSchema} value={draft[section]} def={cfg.defaults?.[section]} path={[section]} {onedit} {filter} />
              {:else}
                <SchemaField schema={sectionSchema} value={draft[section]} def={cfg.defaults?.[section]} path={[section]} {onedit} depth={1} {filter} />
              {/if}
            </div>
          {/key}
        {/if}
      </Card>
    </div>
  {:else if tab === 'yaml'}
    <div class="cols">
      <Card title={t('settings.current_yaml')} description={t('settings.current_yaml_desc')}>
        {#if yaml.data}<pre class="yaml">{yaml.data.yaml}</pre>{:else}<LoadState res={yaml}><Skeleton lines={10} /></LoadState>{/if}
      </Card>
      <Card title={t('settings.import')} description={t('settings.import_desc')}>
        <div class="stack">
          <input type="file" accept=".yaml,.yml,text/yaml" onchange={readFile} aria-label={t('settings.import_file')} />
          <TextArea bind:value={importText} rows={12} mono spellcheck={false} placeholder="schema_version: 1" aria-label={t('settings.import')} />
          <div class="row"><Button icon={I.GitCompare} onclick={checkImport} disabled={!importText.trim()}>{t('settings.check')}</Button><Button variant="primary" icon={I.Upload} disabled={!diff?.valid || !diff?.changed} onclick={() => (importOpen = true)}>{t('settings.apply')}</Button></div>
          {#if diff}
            {#if diff.valid === false}<div class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span class="break">{diff.error}</span></div>
            {:else if !diff.changed}<div class="callout ok"><span class="ic"><I.CircleCheck size={16} /></span><span>{t('settings.no_change')}</span></div>
            {:else}<ChangeList changes={diff.changes ?? []} />{/if}
          {/if}
        </div>
      </Card>
    </div>
  {:else if tab === 'snapshots'}
    <div class="snap-grid">
      <Card title={t('settings.snap_new')} description={t('settings.snap_new_desc')}>
        <form class="stack" onsubmit={saveSnap}>
          <Field id="sn-name" label={t('settings.snap_name')}>{#snippet children({ id })}<Input {id} bind:value={snapName} maxlength={80} placeholder={t('settings.snap_name_ph')} />{/snippet}</Field>
          <Field id="sn-note" label={t('settings.snap_note')}>{#snippet children({ id })}<Input {id} bind:value={snapNote} maxlength={400} />{/snippet}</Field>
          <div><Button type="submit" variant="primary" icon={I.Camera} loading={snapBusy} disabled={!snapName.trim() || !can('config.snapshot')}>{t('settings.snap_save')}</Button></div>
        </form>
      </Card>
      <Card padded={false} title={t('settings.snap_list')} description={t('settings.snap_list_desc')}>
        {#if (snaps.data?.snapshots ?? []).length === 0}
          <EmptyState compact pose="box" title={t('settings.snap_empty')} description={t('settings.snap_empty_body')} />
        {:else}
          <div use:scrollable class="table-scroll"><table class="table">
            <thead><tr><th>{t('settings.snap_name')}</th><th>{t('settings.h.when')}</th><th>{t('settings.h.revision')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {#each snaps.data.snapshots as sn (sn.id)}
                <tr><td><strong class="break">{sn.name}</strong>{#if sn.same_as_current} <Status tone="ok" label={t('settings.snap_current')} size="sm" />{/if}{#if sn.note}<div class="xs muted break">{sn.note}</div>{/if}{#if sn.actor}<div class="xs subtle">{sn.actor}</div>{/if}</td>
                  <td class="muted nowrap" title={datetime(sn.at, true)}>{relative(sn.at)}</td><td class="num">r{sn.revision}</td>
                  <td class="r nowrap"><Button size="sm" variant="ghost" icon={I.History} disabled={sn.same_as_current || !can('config.snapshot.restore')} onclick={() => previewSnap(sn)}>{t('settings.snap_restore')}</Button>
                    <Button size="sm" variant="danger-ghost" icon={I.Trash2} aria-label={t('common.delete')} disabled={!can('config.snapshot.delete')} onclick={() => deleteSnap(sn.id)}></Button></td></tr>
              {/each}
            </tbody>
          </table></div>
        {/if}
      </Card>
    </div>
  {:else}
    <Card padded={false} title={t('settings.history_title')} description={t('settings.history_desc')}>
      <div class="hbar">
        <Segmented size="sm" label={t('settings.h.kind')} bind:value={hKind} options={[{ value: 'config', label: t('settings.h.kind_config') }, { value: 'all', label: t('settings.h.kind_all') }]} />
        <Segmented size="sm" label={t('settings.h.via')} bind:value={hVia} options={[{ value: 'all', label: t('blocks.f.all') }, { value: 'gui', label: t('via.gui') }, { value: 'discord', label: t('via.discord') }, { value: 'cli', label: t('via.cli') }]} />
        <Segmented size="sm" label={t('settings.h.result')} bind:value={hResult} options={[{ value: 'all', label: t('blocks.f.all') }, { value: 'ok', label: t('settings.h.ok') }, { value: 'failed', label: t('settings.h.failed') }]} />
        <div class="hsearch"><Input type="search" bind:value={hQuery} placeholder={t('settings.h.search')} aria-label={t('settings.h.search')} /></div>
        <Button size="sm" variant="ghost" icon={I.Download} disabled={!history.length} onclick={exportCSV}>{t('settings.h.export')}</Button>
      </div>
      {#if !audit.data}<div style:padding="20px"><LoadState res={audit}><Skeleton lines={8} /></LoadState></div>
      {:else if history.length === 0}<EmptyState compact pose="search" title={t('settings.h.empty')} />
      {:else}
        {#each byDay as [day, list] (day)}
          <h3 class="day xs muted">{day}</h3>
          <ul class="hist">
            {#each list as e (e.id)}
              <li class:bad={!e.ok}>
                <span class="when num xs muted" title={datetime(e.at, true)}>{new Date(e.at).toLocaleTimeString(i18n.lang || 'en', { hour: '2-digit', minute: '2-digit' })}</span>
                <span class="what">
                  <strong class="small">{actionLabel(e.action)}</strong>
                  <span class="xs muted break">{e.revision_after !== e.revision_before ? `r${e.revision_before} → r${e.revision_after} · ` : ''}{has(`via.${e.via}`) ? tp(`via.${e.via}`) : e.via}{e.actor ? ` · ${e.actor}` : ''}{e.guild_id ? ` · ${t('settings.h.server', { id: e.guild_id })}` : ''}{!e.ok ? ` · ${e.code || t('common.failed')}` : ''}</span>
                  {#if e.summary}<span class="xs subtle break sum">{e.summary}</span>{/if}
                </span>
                <span class="acts">
                  {#if e.ok && e.revision_after > e.revision_before && e.revision_before > 0}<Button size="sm" variant="ghost" icon={I.GitCompare} onclick={() => showChanges(e)}>{t('settings.h.changes')}</Button>{/if}
                  {#if e.ok && e.revision_after > e.revision_before && e.revision_before > 0 && e.revision_after === cfg.revision}<Button size="sm" variant="ghost" icon={I.Undo2} onclick={() => { rollbackTo = e.revision_before; rbOpen = true }}>{t('settings.undo')}</Button>{/if}
                </span>
              </li>
            {/each}
          </ul>
        {/each}
        {#if nextBefore}<div class="more"><Button size="sm" variant="ghost" loading={olderBusy} onclick={loadOlder}>{t('settings.h.more')}</Button></div>{/if}
      {/if}
    </Card>
  {/if}
</div>

<SaveBar count={Object.keys(edits).length} {saving} onsave={save} onreset={reset} note={t('settings.savebar_note')} />
<Dialog bind:open={snapOpen} title={t('settings.snap_restore_title', { name: snapFor?.name ?? '' })} description={t('settings.snap_restore_desc')} size="lg">
  {#if !snapDiff}<Skeleton lines={6} />{:else if snapDiff.error}<div class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span class="break">{snapDiff.error}</span></div>
  {:else}<ChangeList changes={snapDiff.changes ?? []} />{/if}
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (snapOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.History} disabled={!snapDiff?.changed} onclick={() => (snapConfirm = true)}>{t('settings.snap_restore')}</Button>
  {/snippet}
</Dialog>
<Confirm bind:open={snapConfirm} title={t('settings.snap_confirm_title')} body={t('settings.snap_confirm_body')} confirmLabel={t('settings.snap_restore')} onconfirm={restoreSnap} />
<Confirm bind:open={importOpen} title={t('settings.apply_title')} body={t('settings.apply_body')} confirmLabel={t('settings.apply_confirm')} onconfirm={applyImport} />
<Dialog bind:open={changesOpen} title={t('settings.h.changes_title', { from: changesFor?.revision_before ?? '', to: changesFor?.revision_after ?? '' })} description={changesFor ? `${actionLabel(changesFor.action)} · ${datetime(changesFor.at, true)}` : ''} size="lg">
  {#if !changesOut}<Skeleton lines={5} />{:else if changesOut.error}<div class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span class="break">{changesOut.error}</span></div>
  {:else}<ChangeList changes={changesOut.changes ?? []} />{/if}
</Dialog>
<Confirm bind:open={rbOpen} title={t('settings.rollback_title')} body={t('settings.rollback_body', { rev: rollbackTo })} confirmLabel={t('settings.rollback_confirm', { rev: rollbackTo })} onconfirm={rollback} />

<style>
  .panel {
    margin-top: var(--s-4);
  }
  .snap-grid {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 380px), 1fr));
    align-items: start;
  }
  .layout {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
    align-items: start;
  }
  @media (min-width: 1000px) {
    .layout {
      grid-template-columns: 240px minmax(0, 1fr);
    }
    .secs {
      position: sticky;
      top: calc(var(--topbar-h) + var(--s-4));
    }
  }
  .secs ul {
    list-style: none;
    margin: var(--s-3) 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .secs button {
    position: relative;
    width: 100%;
    height: var(--control-h);
    padding: 0 12px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    text-align: start;
    font-weight: 550;
    cursor: pointer;
  }
  .secs button:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .secs button.on {
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-border);
  }
  .dot {
    position: absolute;
    inset-inline-end: 10px;
    top: 50%;
    translate: 0 -50%;
    width: 7px;
    height: 7px;
    border-radius: 999px;
    background: var(--accent);
  }
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 480px), 1fr));
  }
  .yaml {
    margin: 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    font-size: var(--text-xs);
    max-height: 64vh;
    overflow: auto;
  }
  .more {
    display: flex;
    justify-content: center;
    padding: var(--s-3);
  }
  .hbar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2);
    align-items: center;
    padding: 0 var(--pad-card) var(--s-3);
  }
  .hsearch {
    flex: 0 1 260px;
    margin-inline-start: auto;
  }
  .day {
    margin: 0;
    padding: var(--s-3) var(--pad-card) 4px;
    font-weight: 600;
    position: sticky;
    top: var(--topbar-h);
    background: var(--surface);
  }
  .hist {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .hist li {
    display: grid;
    grid-template-columns: 3.5em minmax(0, 1fr) auto;
    gap: var(--s-3);
    align-items: start;
    padding: 8px var(--pad-card);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .hist li.bad {
    background: color-mix(in oklab, var(--danger-soft) 45%, transparent);
  }
  .when {
    padding-top: 2px;
  }
  .what {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .sum {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .acts {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  /* narrower: the sections are one scrollable row of chips above the form, not a screen-long list */
  @media (max-width: 999px) {
    .secs ul {
      flex-direction: row;
      overflow-x: auto;
      scrollbar-width: thin;
      gap: 6px;
      padding-bottom: 4px;
    }
    .secs button {
      width: auto;
      white-space: nowrap;
      padding-inline: 12px 22px;
      box-shadow: inset 0 0 0 1px var(--border);
    }
  }
  .owner {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
    margin-bottom: var(--s-3);
  }
  .owner .grow {
    flex: 1 1 220px;
  }
</style>
