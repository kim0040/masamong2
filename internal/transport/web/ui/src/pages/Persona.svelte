<script lang="ts">
  import { t, localeName, languageLabel, i18n, fetchStrings } from '../lib/i18n.svelte'
  import { prefs, setPrefs } from '../lib/prefs.svelte'
  import { resource } from '../lib/resource.svelte'
  import { call } from '../lib/api.svelte'
  import { usd } from '../lib/format'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Select from '../components/Select.svelte'
  import Dialog from '../components/Dialog.svelte'
  import Confirm from '../components/Confirm.svelte'
  import Status from '../components/Status.svelte'
  import Progress from '../components/Progress.svelte'
  import { toast } from '../lib/toast.svelte'
  import { defaultPersonaId, personaId, personaTitle } from '../lib/persona'
  import * as I from '../lib/icons'

  useConfig()
  const personas = resource<any>('personas.list', () => ({}), { live: ['config'] })
  const locales = resource<any>('locales.list')
  const chList = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })

  // Persona language: automatic by default — the console language, which follows the system/browser
  // language — with a manual choice that is remembered per admin (prefs.persona_lang).
  const activeLang = $derived(prefs.persona_lang || i18n.lang)
  const filterLangs = $derived.by(() => {
    const set = new Set<string>((locales.data?.loaded ?? ['ko', 'en']) as string[])
    for (const p of (personas.data?.personas ?? []) as any[]) if (p.locale) set.add(p.locale)
    return [...set].sort()
  })
  const langOptions = $derived([{ value: '', label: t('persona.lang_auto') }, ...filterLangs.map((l) => ({ value: l, label: languageLabel(l) }))])
  let showHidden = $state(false)
  const shown = $derived.by(() =>
    ((personas.data?.personas ?? []) as any[]).filter((p) => p.locale === activeLang && (showHidden || !p.hidden)),
  )
  const hiddenCount = $derived(((personas.data?.personas ?? []) as any[]).filter((p) => p.locale === activeLang && p.hidden).length)
  // the persona an unconfigured room of this language uses (the server's warm_<tag> where the language
  // has one and i18n.polite_default is on, else friendly_<tag>, each with its _v2 first; falling back the same way the server does)
  const defaultPersona = $derived(defaultPersonaId((personas.data?.personas ?? []) as any[], activeLang, !!cfg.doc?.i18n?.polite_default))
  const usage = $derived.by(() => {
    const n: Record<string, number> = {}
    for (const g of Object.values<any>(cfg.doc?.guilds ?? {})) for (const c of Object.values<any>(g?.channels ?? {})) if (c?.persona_ref) n[c.persona_ref] = (n[c.persona_ref] ?? 0) + 1
    return n
  })
  // rooms by persona (every room the console knows, including ones still on the default)
  const roomsOf = $derived.by(() => {
    const m: Record<string, any[]> = {}
    for (const c of (chList.data?.channels ?? []) as any[]) (m[c.persona_ref] ??= []).push(c)
    return m
  })
  const serverName = (id: string) => ((chList.data?.guilds ?? []) as any[]).find((g) => g.id === id)?.name ?? id

  // apply a persona to rooms: one config change for all of them
  let applyFor = $state<any>(null)
  let applyOpen = $state(false)
  let applySel = $state<Set<string>>(new Set())
  let applyBusy = $state(false)
  const rk = (c: any) => `${c.guild_id}:${c.id}`
  const byServer = $derived.by(() => {
    const m = new Map<string, any[]>()
    for (const c of (chList.data?.channels ?? []) as any[]) m.set(c.guild_id, [...(m.get(c.guild_id) ?? []), c])
    return [...m.entries()]
  })
  function openApply(p: any) {
    applyFor = p
    applySel = new Set()
    applyOpen = true
  }
  function pick(c: any) {
    const s = new Set(applySel)
    s.has(rk(c)) ? s.delete(rk(c)) : s.add(rk(c))
    applySel = s
  }
  async function applyRooms() {
    applyBusy = true
    const r = await call('personas.apply', { id: applyFor.id, targets: [...applySel] })
    applyBusy = false
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    toast('ok', t('persona.applied', { name: applyFor.label, n: applySel.size }))
    applyOpen = false
    void chList.refresh()
  }
  const personaName = (id: string) => personaTitle((personas.data?.personas ?? []).find((p: any) => p.id === id), id)

  // new personas get an ID from the name; it stays editable under "more"
  const slug = (label: string) => personaId(label, new Set((personas.data?.personas ?? []).map((p: any) => p.id)))
  let idTouched = $state(false)
  $effect(() => {
    if (edit.isNew && !idTouched) edit.id = slug(edit.label)
  })
  const templates = ['friend', 'polite', 'brief', 'cheer'] as const
  let editOpen = $state(false)
  let edit = $state({ id: '', label: '', locale: 'ko', style: '', isNew: true })
  let editErr = $state('')
  let saving = $state(false)
  let delFor = $state('')
  let delOpen = $state(false)
  // style templates follow the persona's own language, not the console language
  let tplStrings = $state<Record<string, string>>({})
  $effect(() => {
    if (!editOpen) return
    const lang = edit.locale || i18n.lang
    let alive = true
    fetchStrings(lang)
      .then((s) => { if (alive) tplStrings = s })
      .catch(() => { if (alive) tplStrings = {} })
    return () => { alive = false }
  })
  const tplText = (k: string) => tplStrings[`persona.tpl.${k}`] ?? t(`persona.tpl.${k}`)

  function openNew() {
    idTouched = false
    edit = { id: '', label: '', locale: activeLang, style: '', isNew: true }
    editErr = ''
    editOpen = true
  }
  function openEdit(p: any) {
    edit = { id: p.id, label: p.label, locale: p.locale, style: p.style, isNew: false }
    editErr = ''
    editOpen = true
  }
  async function saveEdit() {
    if (!edit.label.trim()) return (editErr = t('persona.err.label'))
    if (!/^[a-z][a-z0-9_]{1,40}$/.test(edit.id)) return (editErr = t('persona.err.id'))
    if (!edit.style.trim()) return (editErr = t('persona.err.style'))
    saving = true
    const r = await patchConfig([{ path: ['personas', edit.id], value: { label: edit.label.trim() || edit.id, locale: edit.locale, style: edit.style.trim() } }], { success: t('persona.saved') })
    saving = false
    if (r.ok) {
      // keep what was saved: refreshing the list would otherwise re-derive a new ID from the name
      const saved = { isNew: edit.isNew, id: edit.id, label: edit.label.trim() || edit.id }
      idTouched = true
      editOpen = false
      await personas.refresh()
      // a new persona is only useful in a room: go straight to choosing rooms
      if (saved.isNew && (chList.data?.channels ?? []).length) openApply(saved)
    } else editErr = r.error?.message ?? ''
  }
  async function remove() {
    await patchConfig([{ path: ['personas', delFor], op: 'unset' }], { success: t('persona.deleted') })
    void personas.refresh()
  }
  // built-ins are hidden instead of deleted: the entry stays in config and can be restored
  async function hide(p: any) {
    await patchConfig([{ path: ['personas', p.id, 'hidden'], value: true }], { success: t('persona.hidden_saved') })
    void personas.refresh()
  }
  async function restore(p: any) {
    await patchConfig([{ path: ['personas', p.id], op: 'unset' }], { success: t('persona.restored') })
    void personas.refresh()
  }

  // try the edited style in a room before saving (a lab turn: nothing goes to Discord)
  const rooms = $derived(((chList.data?.channels ?? []) as any[]).map((c) => ({ value: `${c.guild_id}:${c.id}`, label: `#${c.name || c.id}` })))
  let tryRoom = $state('')
  let tryMsg = $state('')
  let tryOut = $state<any>(null)
  let trying = $state(false)
  $effect(() => {
    if (!tryRoom && rooms.length) tryRoom = rooms[0].value
  })
  async function tryStyle() {
    const [g, c] = tryRoom.split(':')
    trying = true
    const r = await call('chat.respond', { guild_id: g, channel_id: c, message: tryMsg.trim(), speaker: t('lab.default_speaker'), draft_style: edit.style, ...(edit.isNew ? {} : { persona_ref: edit.id }) })
    trying = false
    tryOut = r.ok ? r.output : { error: r.error?.message }
  }

  // bot names and memory languages
  let names = $state('')
  let langs = $state('')
  $effect(() => {
    if (cfg.loaded) {
      names = (cfg.doc?.instance?.bot_names ?? []).join(', ')
      langs = (cfg.doc?.memory?.languages ?? []).join(', ')
    }
  })
  async function saveNames() {
    const list = names.split(',').map((x) => x.trim()).filter(Boolean)
    await patchConfig([{ path: ['instance', 'bot_names'], ...(list.length ? { value: list } : { op: 'unset' as const }) }], { success: t('persona.names_saved') })
  }
  async function saveLangs() {
    const list = langs.split(',').map((x) => x.trim()).filter(Boolean)
    await patchConfig([{ path: ['memory', 'languages'], ...(list.length ? { value: list } : { op: 'unset' as const }) }], { success: t('persona.langs_saved') })
  }
</script>

<PageHeader title={t('persona.title')} description={t('persona.desc')}>
  {#snippet actions()}<Button variant="primary" icon={I.Plus} onclick={openNew}>{t('persona.new')}</Button>{/snippet}
</PageHeader>

<div class="row filters">
  <div class="langpick">
    <Select aria-label={t('persona.lang_label')} value={prefs.persona_lang ?? ''} options={langOptions} onchange={(e) => setPrefs({ persona_lang: e.currentTarget.value })} />
  </div>
  {#if hiddenCount > 0}
    <Button size="sm" variant="ghost" icon={showHidden ? I.EyeOff : I.Eye} onclick={() => (showHidden = !showHidden)}>{t('persona.show_hidden', { n: hiddenCount })}</Button>
  {/if}
</div>
<p class="xs muted langhint">{t('persona.lang_hint')}</p>

{#if shown.length === 0}
  <p class="small muted">{t('persona.none_lang')}</p>
{:else}
<div class="grid" style:--min="340px">
  {#each shown as p, i (p.id)}
    <article class="pc enter" class:hidden={p.hidden} style:--i={i}>
      <header>
        <span class="av" aria-hidden="true">{p.locale === 'ko' ? '가' : 'Aa'}</span>
        <div class="nm"><h2>{personaTitle(p)}</h2><code class="xs subtle">{p.id}</code></div>
        {#if p.builtin}<span class="chip">{t('persona.builtin')}</span>{/if}
        {#if p.id === defaultPersona}<span class="chip accent" title={t('persona.default_chip_hint')}>{t('persona.default_chip')}</span>{/if}
        {#if p.hidden}<span class="chip">{t('persona.hidden_chip')}</span>{/if}
        <span class="chip accent">{localeName(p.locale)}</span>
      </header>
      <p class="style small">{p.style}</p>
      {#if !p.hidden}
      <div class="used">
        <span class="xs muted">{t('persona.used_by', { n: (roomsOf[p.id] ?? []).length })}</span>
        {#each (roomsOf[p.id] ?? []).slice(0, 6) as c (rk(c))}<a class="chip" href="/rooms/{c.guild_id}/{c.id}"><I.Hash size={11} aria-hidden="true" />{c.name}</a>{/each}
        {#if (roomsOf[p.id] ?? []).length > 6}<span class="chip">+{(roomsOf[p.id] ?? []).length - 6}</span>{/if}
      </div>
      {/if}
      <footer class="row">
        {#if p.hidden}
          <span class="spacer"></span>
          <Button size="sm" variant="ghost" icon={I.Undo2} onclick={() => restore(p)}>{t('persona.restore')}</Button>
        {:else}
        <Button size="sm" icon={I.Hash} disabled={!(chList.data?.channels ?? []).length} onclick={() => openApply({ id: p.id, label: personaTitle(p) })}>{t('persona.apply')}</Button>
        <span class="spacer"></span>
        <Button size="sm" variant="ghost" icon={I.FlaskConical} href="/lab?persona={encodeURIComponent(p.id)}">{t('persona.preview')}</Button>
        {#if !p.builtin}
          <Button size="sm" variant="ghost" icon={I.SlidersHorizontal} onclick={() => openEdit(p)}>{t('common.edit')}</Button>
          <Button size="sm" variant="danger-ghost" icon={I.Trash2} aria-label={t('common.delete')} title={(usage[p.id] ?? 0) > 0 ? t('persona.delete_in_use') : t('common.delete')} disabled={(usage[p.id] ?? 0) > 0} onclick={() => { delFor = p.id; delOpen = true }}></Button>
        {:else}
          <Button size="sm" variant="ghost" icon={I.Copy} onclick={() => { openNew(); edit.style = p.style; edit.locale = p.locale; edit.label = t('persona.copy_of', { name: personaTitle(p) }) }}>{t('persona.duplicate')}</Button>
          <Button size="sm" variant="ghost" icon={I.EyeOff} aria-label={t('persona.hide')} title={(usage[p.id] ?? 0) > 0 ? t('persona.hide_in_use') : t('persona.hide_hint')} disabled={(usage[p.id] ?? 0) > 0} onclick={() => hide(p)}></Button>
        {/if}
        {/if}
      </footer>
    </article>
  {/each}
</div>
{/if}

<div class="cols">
  <Card title={t('persona.names_title')} description={t('persona.names_desc')}>
    <div class="row">
      <div style:flex="1"><Input bind:value={names} aria-label={t('persona.names_title')} placeholder={t('persona.names_ph')} /></div>
      <Button onclick={saveNames} icon={I.Save}>{t('common.save')}</Button>
    </div>
  </Card>
  <Card title={t('persona.langs_title')} description={t('persona.langs_desc')}>
    <div class="row">
      <div style:flex="1"><Input bind:value={langs} aria-label={t('persona.langs_title')} placeholder="ko, en" /></div>
      <Button onclick={saveLangs} icon={I.Save}>{t('common.save')}</Button>
    </div>
  </Card>
  <Card title={t('persona.bundles_title')} description={t('persona.bundles_desc')}>
    {#each Object.entries(locales.data?.coverage ?? {}) as [l, c]}
      {@const cov = c as any}
      <div class="lc">
        <div class="row small"><strong>{localeName(l)}</strong><span class="spacer"></span><span class="num muted">{cov.missing ?? 0} {t('persona.missing')}</span></div>
        <Progress value={(cov.total ?? 1) - (cov.missing ?? 0)} max={cov.total ?? 1} label={localeName(l)} tone={(cov.missing ?? 0) === 0 ? 'ok' : 'warn'} />
      </div>
    {/each}
  </Card>
</div>

<Dialog bind:open={editOpen} title={edit.isNew ? t('persona.new') : t('persona.edit')} size="lg">
  <div class="stack">
    <div class="form-grid">
      <Field id="plb" label={t('persona.label')} error={editErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={edit.label} placeholder={t('persona.label_ph')} />{/snippet}</Field>
      <Field id="plc" label={t('persona.locale')} hint={t('persona.locale_hint')}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={edit.locale} options={(locales.data?.bundled ?? locales.data?.loaded ?? ['ko', 'en']).map((l: string) => ({ value: l, label: languageLabel(l) }))} />{/snippet}</Field>
    </div>
    {#if edit.isNew && !edit.style.trim()}
      <div class="tpl">
        <span class="xs muted">{t('persona.tpl_title')}</span>
        {#each templates as k}<button type="button" class="chip" onclick={() => { edit.style = tplText(k); if (!edit.label.trim()) edit.label = t(`persona.tpl_name.${k}`) }}>{t(`persona.tpl_name.${k}`)}</button>{/each}
      </div>
    {/if}
    <Field id="pst" label={t('persona.style')} hint={t('persona.style_hint')}>{#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={edit.style} rows={8} />{/snippet}</Field>
    <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('persona.scope_note')}</span></div>
    <details class="try">
      <summary class="small">{t('persona.more')}</summary>
      <div style:margin-top="10px">
        <Field id="pid" label={t('persona.id')} hint={edit.isNew ? t('persona.id_hint') : t('persona.id_fixed')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={edit.id} oninput={() => (idTouched = true)} mono disabled={!edit.isNew} />{/snippet}</Field>
      </div>
    </details>
    <details class="try">
      <summary class="small">{t('persona.try_title')}</summary>
      <div class="stack" style:margin-top="10px">
        <p class="xs muted">{t('persona.try_desc')}</p>
        {#if rooms.length === 0}<p class="small muted">{t('persona.try_no_rooms')} <a href="/rooms">{t('nav.rooms')}</a></p>
        {:else}
          <form class="row" onsubmit={(e) => { e.preventDefault(); if (tryMsg.trim() && edit.style.trim()) void tryStyle() }}>
            <div style:width="180px"><Select aria-label={t('lab.channel')} bind:value={tryRoom} options={rooms} /></div>
            <div style:flex="1" style:min-width="160px"><Input bind:value={tryMsg} placeholder={t('room.try_ph')} aria-label={t('room.try_ph')} /></div>
            <Button type="submit" icon={I.Send} loading={trying} disabled={!tryMsg.trim() || !edit.style.trim()}>{t('room.try_send')}</Button>
          </form>
          {#if tryOut?.error}<p class="small err">{tryOut.error}</p>
          {:else if tryOut}<div class="ans"><p class="small">{tryOut.answer || t('room.try_no_answer')}</p><span class="xs subtle num">{t('room.try_meta', { cost: usd(tryOut.cost_usd ?? 0, 4), n: tryOut.hits ?? 0 })}</span></div>{/if}
        {/if}
      </div>
    </details>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (editOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={saving} onclick={saveEdit} icon={I.Save}>{t('persona.save')}</Button>
  {/snippet}
</Dialog>
<Dialog bind:open={applyOpen} title={t('persona.apply_title', { name: applyFor?.label ?? '' })} description={t('persona.apply_desc')} size="md">
  <div class="stack">
    {#each byServer as [gid, chans] (gid)}
      <fieldset class="srv">
        <legend class="small">{serverName(gid)}</legend>
        {#each chans as c (rk(c))}
          {@const mine = c.persona_ref === applyFor?.id}
          <label class="room" class:mine>
            <input type="checkbox" checked={mine || applySel.has(rk(c))} disabled={mine} onchange={() => pick(c)} />
            <span class="nm"><I.Hash size={13} aria-hidden="true" />{c.name}</span>
            <span class="xs muted">{mine ? t('persona.apply_already') : personaName(c.persona_ref)}</span>
          </label>
        {/each}
      </fieldset>
    {/each}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (applyOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Check} loading={applyBusy} disabled={!applySel.size} onclick={applyRooms}>{t('persona.apply_confirm', { n: applySel.size })}</Button>
  {/snippet}
</Dialog>
<Confirm bind:open={delOpen} title={t('persona.delete_title')} body={t('persona.delete_body', { id: delFor })} confirmLabel={t('persona.delete_confirm')} danger onconfirm={remove} />

<style>
  .filters {
    margin-bottom: var(--s-2);
    flex-wrap: wrap;
  }
  .langpick {
    width: min(240px, 100%);
  }
  .langhint {
    margin-bottom: var(--s-4);
  }
  .pc {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding: var(--pad-card);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .pc.hidden {
    opacity: 0.65;
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
  }
  .av {
    display: grid;
    place-items: center;
    width: 40px;
    height: 40px;
    border-radius: 12px;
    background: linear-gradient(in oklab 135deg, var(--accent-soft), var(--info-soft));
    font-weight: 700;
  }
  .nm {
    flex: 1;
    min-width: 0;
  }
  h2 {
    font-size: var(--text-md);
  }
  .style {
    padding: 12px;
    border-radius: var(--r-lg);
    background: var(--surface-2);
    color: var(--text-muted);
    white-space: pre-wrap;
    max-height: 160px;
    overflow: auto;
  }
  .cols {
    display: grid;
    gap: var(--s-4);
    margin-top: var(--s-6);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 360px), 1fr));
  }
  .try summary {
    cursor: pointer;
    font-weight: 600;
  }
  .ans {
    display: grid;
    gap: 4px;
    padding: 10px 12px;
    border-radius: var(--r);
    background: var(--surface-2);
    white-space: pre-wrap;
  }
  .ans p {
    margin: 0;
  }
  .err {
    color: var(--danger);
  }
  .used {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .used a.chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    color: var(--text);
    text-decoration: none;
  }
  .used a.chip:hover {
    text-decoration: underline;
  }
  .pc footer {
    margin-top: auto;
  }
  .tpl {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .tpl button {
    border: 0;
    cursor: pointer;
  }
  .tpl button:hover {
    background: var(--accent-soft);
  }
  .srv {
    margin: 0;
    padding: 0;
    border: 0;
    display: grid;
    gap: 2px;
  }
  .srv legend {
    padding: 0 0 6px;
    font-weight: 650;
  }
  .room {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: center;
    gap: 10px;
    padding: 8px 10px;
    border-radius: var(--r);
    cursor: pointer;
  }
  .room:hover {
    background: var(--surface-2);
  }
  .room.mine {
    cursor: default;
    opacity: 0.7;
  }
  .room input {
    width: 16px;
    height: 16px;
    accent-color: var(--accent);
  }
  .room .nm {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    font-weight: 600;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .lc {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-bottom: var(--s-3);
  }
</style>
