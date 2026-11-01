<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { tp } from '../lib/platform'
  import { call, session, can, practice } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { toast } from '../lib/toast.svelte'
  import { num, bytes, datetime, relative, duration } from '../lib/format'
  import { overview, startOverview } from '../lib/overview.svelte'
  import { noteLifecycle } from '../lib/live.svelte'
  import { useConfig, patchConfig } from '../lib/config.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Status from '../components/Status.svelte'
  import KeyValue from '../components/KeyValue.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Select from '../components/Select.svelte'
  import Switch from '../components/Switch.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import Confirm from '../components/Confirm.svelte'
  import Dialog from '../components/Dialog.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import * as I from '../lib/icons'

  startOverview()
  const cfg = useConfig()
  const storage = resource<any>('storage.status', () => ({}), { live: ['config'] })
  const backups = resource<any>('backup.list', () => ({}), { live: ['audit'] })
  const autostart = resource<any>('runtime.autostart', () => ({}))
  const updates = resource<any>('updates.status', () => ({}), { live: ['config'] })
  let checkingUpdate = $state(false)
  async function checkUpdate() {
    checkingUpdate = true
    const r = await call('updates.check')
    checkingUpdate = false
    if (!r.ok) toast('error', t('common.failed'), r.error?.message)
    else void updates.refresh()
  }
  // the latest release, in words
  const updateLine = $derived.by(() => {
    const u = updates.data
    if (!u) return '–'
    if (u.dev_build) return t('maint.update.dev')
    if (!u.check) return t('maint.update.off')
    const st = u.state ?? {}
    if (!st.checked_at) return t('maint.update.never')
    if (st.status === 'unavailable') return t('maint.update.unavailable')
    if (st.status === 'error') return t('maint.update.error')
    return u.newer ? t('maint.update.newer', { v: st.latest }) : t('maint.update.current')
  })
  // after a restart that changes what the console should show (a new version, a reset), load the page
  // again once the service answers in its new state
  function reloadWhen(ready: (h: any) => boolean, to: string) {
    noteLifecycle('restart')
    const until = Date.now() + 120000
    const tick = async () => {
      try {
        const r = await fetch('/api/health', { cache: 'no-store' })
        if (r.ok && ready(await r.json())) return void location.assign(to)
      } catch {
        // still restarting
      }
      if (Date.now() < until) setTimeout(tick, 1500)
    }
    setTimeout(tick, 1500)
  }
  let installOpen = $state(false)
  async function installUpdate() {
    const from = updates.data?.current
    const r = await call('updates.apply', { confirm: updates.data?.state?.latest })
    if (!r.ok) return void toast('error', t('common.failed'), r.error?.message)
    toast('ok', r.output?.message ?? '', t('maint.update.restarting'))
    reloadWhen((h) => !!h.version && h.version !== from, location.pathname)
  }
  // a recent update this console installed, shown once the new version runs
  const justUpdated = $derived.by(() => {
    const a = updates.data?.last_applied
    return a && a.to === updates.data?.current && Date.now() - Date.parse(a.at) < 3 * 864e5 ? a : null
  })
  let resetOpen = $state(false)
  let resetBackup = $state(true)
  let resetKeys = $state(false)
  let resetTyped = $state('')
  let resetting = $state(false)
  function openReset() {
    resetBackup = true
    resetKeys = false
    resetTyped = ''
    resetOpen = true
  }
  async function runReset() {
    resetting = true
    const r = await call('instance.reset', { confirm: 'reset', backup: resetBackup, delete_keys: resetKeys })
    resetting = false
    if (!r.ok) return void toast('error', t('common.failed'), r.error?.message)
    resetOpen = false
    toast('ok', r.output?.message ?? '', t('maint.reset.restarting'))
    reloadWhen((h) => h.ready === false, '/')
  }
  let backingUp = $state(false)
  async function backup() {
    backingUp = true
    const r = await call('backup.create')
    backingUp = false
    r.ok ? toast('ok', t('maint.backup_done'), r.output.name) : toast('error', t('maint.backup_failed'), r.error?.message)
    void backups.refresh()
  }
  async function verify(name: string) {
    const r = await call('backup.verify', { name })
    r.ok ? toast('ok', t('maint.verify_ok', { name })) : toast('error', t('maint.verify_fail', { name }), r.error?.message)
  }

  // schedule (config backup.*)
  const pol = $derived(cfg.doc?.backup ?? { schedule: 'weekly', hour: 4, weekday: 0, keep: 4 })
  async function setPolicy(key: string, value: unknown) {
    await patchConfig([{ path: ['backup', key], value }], { success: t('maint.schedule_saved') })
    void backups.refresh()
  }
  const hours = Array.from({ length: 24 }, (_, h) => ({ value: String(h), label: `${String(h).padStart(2, '0')}:00` }))
  const weekdays = Array.from({ length: 7 }, (_, d) => ({ value: String(d), label: t(`weekday.${d}`) }))
  let keepDraft = $state('')
  $effect(() => {
    keepDraft = String(pol.keep ?? 4)
  })

  // restore: preview → type the name → staged → restart
  let restoreFor = $state<any>(null)
  let restoreOpen = $state(false)
  let preview = $state<any>(null)
  let previewErr = $state('')
  let restoreTyped = $state('') // the backup's name, typed in the same dialog as the preview
  let restoring = $state(false)
  async function openRestore(b: any) {
    restoreFor = b
    preview = null
    previewErr = ''
    restoreTyped = ''
    restoreOpen = true
    const r = await call('backup.restoreDryRun', { name: b.name })
    if (r.ok) preview = r.output
    else previewErr = r.error?.message ?? ''
  }
  async function runRestore() {
    restoring = true
    const r = await call('backup.restore', { name: restoreFor.name, confirm: restoreFor.name })
    restoring = false
    restoreOpen = false
    if (r.ok) toast('info', t('maint.restore_started'), t('maint.restore_started_body', { safety: r.output.safety_backup }))
    else toast('error', t('maint.restore_failed'), r.error?.message)
    void backups.refresh()
  }
  // the previous bot's personal feature settings (migration.apply): plan first, then bring over
  let v1Path = $state('')
  let v1Plan = $state<any>(null)
  let v1Err = $state('')
  let v1Busy = $state(false)
  let v1Open = $state(false)
  async function v1(confirm: boolean) {
    v1Busy = true
    v1Err = ''
    const r = await call('migration.apply', { path: v1Path.trim(), ...(confirm ? { confirm: true } : {}) })
    v1Busy = false
    if (r.ok) {
      v1Plan = r.output.plan
      if (confirm) toast('ok', t('maint.v1_done'))
    } else {
      v1Plan = null
      v1Err = r.error?.message ?? ''
    }
  }
  const v1Total = $derived(v1Plan ? v1Plan.consents + v1Plan.fortune_profiles + v1Plan.school_notice_profiles + v1Plan.transfer_subscriptions : 0)
  let delFor = $state('')
  let delOpen = $state(false)
  async function del() {
    const r = await call('backup.delete', { name: delFor })
    r.ok ? toast('ok', t('maint.deleted', { name: delFor })) : toast('error', t('common.failed'), r.error?.message)
    void backups.refresh()
  }

  // start at login
  let autoBusy = $state(false)
  async function setAutostart(on: boolean) {
    autoBusy = true
    const r = await call('runtime.autostart.set', { enabled: on })
    autoBusy = false
    if (r.ok) toast('ok', on ? t('maint.autostart_on') : t('maint.autostart_off'))
    else toast('error', t('common.failed'), r.error?.message)
    void autostart.refresh()
  }

  let restartOpen = $state(false)
  let stopOpen = $state(false)
  async function restart() {
    const r = await call('runtime.restart')
    if (r.ok) noteLifecycle('restart')
    else toast('error', t('common.failed'), r.error?.message)
  }
  async function stop() {
    const r = await call('runtime.stop')
    if (r.ok) noteLifecycle('stop')
    else toast('error', t('common.failed'), r.error?.message)
  }
  const kindTone = (k: string) => (k === 'auto' ? 'info' : k === 'prerestore' || k === 'prereset' ? 'warn' : 'neutral') as 'info' | 'warn' | 'neutral'
  const last = $derived(backups.data?.last_restore)
</script>

<PageHeader title={t('maint.title')} description={t('maint.desc')} />

{#if practice.on}
  <div class="callout practice mb"><span class="ic"><I.FlaskConical size={16} /></span><span>{t('practice.maint_note')}</span></div>
{/if}
{#if last}
  <div class="callout {last.ok ? 'ok' : 'danger'} mb enter">
    <span class="ic">{#if last.ok}<I.CircleCheck size={16} />{:else}<I.CircleAlert size={16} />{/if}</span>
    <span class="break">{last.ok ? t('maint.last_restore_ok', { name: last.name, when: relative(last.at) }) : t('maint.last_restore_fail', { name: last.name, error: last.error ?? '', safety: last.safety_backup ?? '' })}</span>
  </div>
{/if}
{#if backups.data?.restore_pending}
  <div class="callout warn mb"><span class="ic"><I.RotateCcw size={16} /></span><span>{t('maint.restore_pending')}</span></div>
{/if}

<div class="cols">
  <Card class="a-service" title={t('maint.service')} description={t('maint.service_desc')}>
    <div class="stack">
      <KeyValue items={[
        { k: t('maint.instance'), v: session.value?.instance },
        { k: t('maint.version'), v: session.value?.version },
        { k: t('maint.update.label'), v: updateLine },
        { k: t('maint.uptime'), v: overview.data ? duration(overview.data.uptime_sec) : '–' },
        { k: t('maint.data_root'), v: session.value?.data_root, mono: true },
        { k: t('conn.bind'), v: session.value?.bind, mono: true },
      ]} />
      <div class="row">
        <Button icon={I.RotateCcw} disabled={!can('runtime.restart') || practice.on} onclick={() => (restartOpen = true)}>{t('maint.restart')}</Button>
        <Button variant="danger-ghost" icon={I.Power} disabled={!can('runtime.stop') || practice.on} onclick={() => (stopOpen = true)}>{t('maint.stop')}</Button>
        <Button href="/lite" data-native variant="ghost" icon={I.ExternalLink}>{t('nav.lite')}</Button>
      </div>
      {#if justUpdated}
        <div class="callout ok"><span class="ic"><I.CircleCheck size={16} /></span><span class="break">{t('maint.update.done', { to: justUpdated.to, backup: justUpdated.backup })}</span></div>
      {/if}
      {#if updates.data?.newer}
        <div class="callout update"><span class="ic"><I.Sparkles size={16} /></span>
          <div class="stack tight">
            <span><strong>{t('maint.update.newer', { v: updates.data.state.latest })}</strong>{#if updates.data.state?.url} · <a href={updates.data.state.url} target="_blank" rel="noopener noreferrer">{t('maint.update.notes')}</a>{/if}</span>
            {#if updates.data.can_apply}
              <div class="row"><Button size="sm" variant="primary" icon={I.Download} disabled={!can('updates.apply') || practice.on} onclick={() => (installOpen = true)}>{t('maint.update.install')}</Button></div>
            {:else}
              <span class="small">{updates.data.apply_blocked && updates.data.apply_blocked !== 'practice' ? tp(`maint.update.blocked.${updates.data.apply_blocked}`) : t('maint.update.how', { v: updates.data.state.latest })}</span>
            {/if}
          </div>
        </div>
      {/if}
      {#if updates.data && !updates.data.dev_build && updates.data.check}
        <div class="row"><Button size="sm" variant="ghost" icon={I.RefreshCw} loading={checkingUpdate} disabled={!can('updates.check') || practice.on} onclick={checkUpdate}>{t('maint.update.check')}</Button>
          {#if updates.data.state?.checked_at}<span class="xs muted">{t('maint.update.checked', { when: relative(updates.data.state.checked_at) })}</span>{/if}</div>
      {/if}
    </div>
  </Card>

  <Card class="a-autostart" title={t('maint.autostart')} description={t('maint.autostart_desc', { tray: tp('platform.tray') })}>
    {#if practice.on}
      <p class="small muted">{t('maint.practice_real_only')}</p>
    {:else if autostart.data?.autostart}
      {@const a = autostart.data.autostart}
      <div class="stack">
        <Switch checked={a.enabled} disabled={autoBusy || !can('runtime.autostart.set') || practice.on} label={t('maint.autostart_switch')} hint={t(`maint.autostart_method.${a.method}`)} onchange={setAutostart} />
        {#if a.note}<p class="xs warn-t break">{a.note}</p>{/if}
        <details><summary class="small muted">{t('maint.autostart_detail')}</summary>
          <KeyValue items={[{ k: t('maint.autostart_where'), v: a.path, mono: true }, { k: t('maint.autostart_cmd'), v: (a.command ?? []).join(' '), mono: true }]} />
        </details>
        <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('maint.tray_note', { tray: tp('platform.tray') })}</span></div>
      </div>
    {:else if autostart.error}
      <p class="small err">{autostart.error.message}</p>
    {/if}
  </Card>

  <Card class="a-storage" title={t('maint.storage')} description={t('maint.storage_desc')}>
    {#if storage.data}
      <div class="stack">
        <div class="row"><Status tone="ok" label="SQLite" />{#if storage.data.restart_pending}<Status tone="warn" label={t('maint.restart_pending')} />{/if}</div>
        <KeyValue items={[
          { k: t('maint.location'), v: storage.data.location, mono: true },
          { k: t('maint.size'), v: storage.data.size_bytes !== undefined ? bytes(storage.data.size_bytes) : '–' },
          { k: t('maint.rows'), v: num(storage.data.rows) },
        ]} />
        <details><summary class="small muted">{t('maint.tables')}</summary>
          <div use:scrollable class="table-scroll"><table class="table"><tbody>{#each Object.entries(storage.data.tables ?? {}).sort((a, b) => (b[1] as number) - (a[1] as number)) as [k, v]}<tr><td class="mono small">{k}</td><td class="r num">{num(v as number)}</td></tr>{/each}</tbody></table></div>
        </details>
      </div>
    {/if}
  </Card>

  <Card class="a-v1" title={t('maint.v1_title')} description={t('maint.v1_desc')}>
    <div class="stack">
      <form class="row" onsubmit={(e) => { e.preventDefault(); if (v1Path.trim()) void v1(false) }}>
        <div class="grow"><Input bind:value={v1Path} mono placeholder={t('maint.v1_path_ph')} aria-label={t('maint.v1_path')} oninput={() => (v1Plan = null)} /></div>
        <Button type="submit" loading={v1Busy && !v1Open} disabled={!v1Path.trim()}>{t('maint.v1_preview')}</Button>
      </form>
      {#if v1Err}<p class="small err break">{v1Err}</p>{/if}
      {#if v1Plan}
        <KeyValue items={[
          { k: t('maint.v1_consents'), v: num(v1Plan.consents) },
          { k: t('maint.v1_fortune'), v: num(v1Plan.fortune_profiles) },
          { k: t('maint.v1_school'), v: num(v1Plan.school_notice_profiles) },
          { k: t('maint.v1_transfer'), v: num(v1Plan.transfer_subscriptions) },
        ]} />
        {#if Object.keys(v1Plan.skipped ?? {}).length}
          <details><summary class="small muted">{t('maint.v1_skipped', { n: num(Object.values<number>(v1Plan.skipped).reduce((a, b) => a + b, 0)) })}</summary>
            <ul class="skips">{#each Object.entries<number>(v1Plan.skipped) as [k, n]}<li class="small"><span>{t(`maint.v1_skip.${k}`)}</span><span class="num muted">{num(n)}</span></li>{/each}</ul>
          </details>
        {/if}
        {#if v1Plan.applied}<Status tone="ok" label={t('maint.v1_applied')} />
        {:else}<div class="row"><Button variant="primary" icon={I.FolderInput} disabled={!v1Total} onclick={() => (v1Open = true)}>{t('maint.v1_apply', { n: num(v1Total) })}</Button></div>{/if}
      {/if}
      <p class="xs muted">{t('maint.v1_note')}</p>
    </div>
  </Card>

  <Card class="a-schedule" title={t('maint.schedule')} description={t('maint.schedule_desc')} data-tour="maint-schedule">
    <div class="stack">
      <Segmented label={t('maint.schedule')} value={pol.schedule} onchange={(v) => setPolicy('schedule', v)} options={[{ value: 'off', label: t('maint.sched.off') }, { value: 'daily', label: t('maint.sched.daily') }, { value: 'weekly', label: t('maint.sched.weekly') }]} />
      {#if pol.schedule !== 'off'}
        <div class="grid3">
          {#if pol.schedule === 'weekly'}
            <Field id="bk-wd" label={t('maint.weekday')}>{#snippet children({ id })}<Select {id} value={String(pol.weekday)} options={weekdays} onchange={(e) => setPolicy('weekday', Number((e.currentTarget as HTMLSelectElement).value))} />{/snippet}</Field>
          {/if}
          <Field id="bk-h" label={t('maint.hour')}>{#snippet children({ id })}<Select {id} value={String(pol.hour)} options={hours} onchange={(e) => setPolicy('hour', Number((e.currentTarget as HTMLSelectElement).value))} />{/snippet}</Field>
          <Field id="bk-k" label={t('maint.keep')} hint={t('maint.keep_hint')}>{#snippet children({ id, describedby })}<Input {id} type="number" min="1" max="100" inputmode="numeric" bind:value={keepDraft} aria-describedby={describedby} onchange={() => { const n = Number(keepDraft); if (n >= 1 && n <= 100 && n !== pol.keep) void setPolicy('keep', n) }} />{/snippet}</Field>
        </div>
      {/if}
      <KeyValue items={[
        { k: t('maint.next_backup'), v: backups.data?.next_backup_at ? `${datetime(backups.data.next_backup_at)} (${relative(backups.data.next_backup_at)})` : '–' },
        { k: t('maint.last_backup'), v: backups.data?.last_backup_at ? relative(backups.data.last_backup_at) : t('maint.never') },
        { k: t('maint.last_auto'), v: backups.data?.schedule_state?.last_run ? `${relative(backups.data.schedule_state.last_run)} · ${backups.data.schedule_state.ok ? t('common.ok') : t('common.failed')}` : '–' },
      ]} />
      {#if backups.data?.schedule_state && !backups.data.schedule_state.ok && backups.data.schedule_state.error}<p class="small err break">{backups.data.schedule_state.error}</p>{/if}
    </div>
  </Card>
</div>

<Card padded={false} title={t('maint.backups')} description={t('maint.backups_desc')} class="bk" data-tour="maint-backups">
  {#snippet actions()}<Button variant="primary" icon={I.Save} loading={backingUp} disabled={!can('backup.create')} onclick={backup}>{t('maint.backup_now')}</Button>{/snippet}
  {#if (backups.data?.backups ?? []).length === 0}
    <EmptyState compact pose="box" title={t('maint.no_backups')} description={t('maint.no_backups_body')} />
  {:else}
    <div use:scrollable class="table-scroll"><table class="table">
      <thead><tr><th>{t('maint.b.name')}</th><th>{t('maint.b.kind')}</th><th>{t('maint.b.created')}</th><th>{t('maint.b.backend')}</th><th class="r">{t('maint.size')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
      <tbody>
        {#each backups.data.backups as b (b.name)}
          <tr><td class="mono small">{b.name}</td><td><Status tone={kindTone(b.kind)} label={t(`maint.kind.${b.kind}`)} size="sm" /></td>
            <td class="muted" title={datetime(b.created_at, true)}>{relative(b.created_at)}</td><td>{b.backend}</td><td class="r num">{bytes(b.size_bytes)}</td>
            <td class="r nowrap">
              <Button size="sm" variant="ghost" icon={I.ShieldCheck} onclick={() => verify(b.name)}>{t('maint.verify')}</Button>
              <Button size="sm" variant="ghost" icon={I.History} disabled={!can('backup.restore') || practice.on} onclick={() => openRestore(b)}>{t('maint.restore')}</Button>
              <Button size="sm" variant="danger-ghost" icon={I.Trash2} aria-label={t('maint.delete_named', { name: b.name })} disabled={!can('backup.delete')} onclick={() => { delFor = b.name; delOpen = true }}></Button>
            </td></tr>
        {/each}
      </tbody>
    </table></div>
  {/if}
  <p class="xs muted foot">{t('maint.backup_note2')}</p>
</Card>

<Card title={t('maint.reset.title')} description={t('maint.reset.desc')} class="reset">
  <div class="row">
    <p class="small muted reset-text">{t('maint.reset.body')}</p>
    <Button variant="danger-ghost" icon={I.RotateCcw} disabled={!can('instance.reset') || practice.on} onclick={openReset}>{t('maint.reset.open')}</Button>
  </div>
</Card>

<Dialog bind:open={resetOpen} title={t('maint.reset.dialog_title')} size="sm">
  <div class="stack">
    <div class="callout danger"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('maint.reset.warn')}</span></div>
    <ul class="small reset-list">
      <li>{t('maint.reset.gone')}</li>
      <li class="muted">{t('maint.reset.kept')}</li>
    </ul>
    <Switch bind:checked={resetBackup} label={t('maint.reset.backup')} hint={t('maint.reset.backup_hint')} />
    {#if !resetBackup}<p class="xs err" role="alert">{t('maint.reset.no_backup')}</p>{/if}
    <Switch bind:checked={resetKeys} label={t('maint.reset.keys')} hint={t('maint.reset.keys_hint')} />
    <Field id="reset-word" label={t('common.type_to_confirm', { word: t('maint.reset.word') })}>{#snippet children({ id })}<Input {id} bind:value={resetTyped} autocomplete="off" spellcheck={false} />{/snippet}</Field>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (resetOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="danger" icon={I.RotateCcw} loading={resetting} disabled={resetTyped.trim().toLowerCase() !== t('maint.reset.word').toLowerCase()} onclick={runReset}>{t('maint.reset.confirm')}</Button>
  {/snippet}
</Dialog>
<Confirm bind:open={installOpen} title={t('maint.update.install_title', { v: updates.data?.state?.latest ?? '' })} body={t('maint.update.install_body')} confirmLabel={t('maint.update.install_confirm')} onconfirm={installUpdate} />

<Dialog bind:open={restoreOpen} title={t('maint.restore_title', { name: restoreFor?.name ?? '' })} description={t('maint.restore_desc')} size="lg">
  <div class="stack">
    {#if previewErr}
      <div class="callout danger"><span class="ic"><I.CircleAlert size={16} /></span><span class="break">{previewErr}</span></div>
    {:else if !preview}
      <p class="small muted">{t('common.loading')}</p>
    {:else}
      <div class="callout ok"><span class="ic"><I.ShieldCheck size={16} /></span><span>{t('maint.restore_verified')}</span></div>
      <KeyValue items={[
        { k: t('maint.b.created'), v: datetime(preview.manifest?.created_at, true) },
        { k: t('maint.restore_rev'), v: t('maint.restore_rev_v', { from: preview.current_revision, to: preview.backup_revision }) },
        { k: t('maint.restore_method'), v: t(`maint.method.${preview.method}`) },
        { k: t('maint.b.backend'), v: preview.manifest?.backend },
        { k: t('maint.version'), v: preview.manifest?.version },
      ]} />
      <ol class="steps small">
        <li>{t('maint.restore_step1')}</li><li>{t('maint.restore_step2')}</li><li>{t('maint.restore_step3')}</li>
      </ol>
      <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('maint.restore_warn')}</span></div>
      <Field id="restore-name" label={t('common.type_to_confirm', { word: restoreFor?.name ?? '' })}>
        {#snippet children({ id })}
          <div class="row"><div style:flex="1"><Input {id} bind:value={restoreTyped} autocomplete="off" spellcheck={false} mono /></div><CopyButton text={restoreFor?.name ?? ''} /></div>
        {/snippet}
      </Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (restoreOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="danger" icon={I.History} disabled={!preview || restoreTyped.trim() !== restoreFor?.name} loading={restoring} onclick={runRestore}>{t('maint.restore_run')}</Button>
  {/snippet}
</Dialog>
<Confirm bind:open={v1Open} title={t('maint.v1_confirm_title')} body={t('maint.v1_confirm_body')} confirmLabel={t('maint.v1_apply', { n: num(v1Total) })} onconfirm={() => v1(true)} />
<Confirm bind:open={delOpen} danger title={t('maint.delete_title')} body={t('maint.delete_body', { name: delFor })} confirmLabel={t('common.delete')} onconfirm={del} />

<Confirm bind:open={restartOpen} title={t('maint.restart_title')} body={t('maint.restart_body')} confirmLabel={t('maint.restart_confirm')} onconfirm={restart} />
<Confirm bind:open={stopOpen} danger title={t('maint.stop_title')} body={t('maint.stop_body', { tray: tp('platform.tray') })} confirmLabel={t('maint.stop_confirm')} onconfirm={stop} />

<style>
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
    margin-bottom: var(--s-4);
  }
  /* pairs of similar height share a row; the short import form takes a row of its own */
  @media (min-width: 1000px) {
    .cols {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      grid-template-areas: 'service autostart' 'storage schedule' 'v1 v1';
    }
    .cols > :global(.a-service) { grid-area: service; }
    .cols > :global(.a-autostart) { grid-area: autostart; }
    .cols > :global(.a-storage) { grid-area: storage; }
    .cols > :global(.a-schedule) { grid-area: schedule; }
    .cols > :global(.a-v1) { grid-area: v1; }
  }
  .mb {
    margin-bottom: var(--s-4);
  }
  .grid3 {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 130px), 1fr));
  }
  .steps {
    margin: 0;
    padding-inline-start: 1.2em;
    display: grid;
    gap: 4px;
    color: var(--text-muted);
  }
  .foot {
    padding: var(--s-3) var(--pad-card);
  }
  .err {
    color: var(--danger);
  }
  .warn-t {
    color: var(--warn);
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .skips {
    list-style: none;
    margin: 8px 0 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .skips li {
    display: flex;
    justify-content: space-between;
    gap: var(--s-3);
  }
  .callout.update {
    background: var(--accent-soft);
  }
  .callout.update .ic {
    color: var(--accent-text);
  }
  .tight {
    gap: var(--s-2);
  }
  /* the last thing on the page, apart from everything else */
  :global(.card.reset) {
    margin-top: var(--s-8);
    box-shadow: var(--shadow-border), inset 3px 0 0 var(--danger);
  }
  .reset-list {
    margin: 0;
    padding-inline-start: 1.2em;
    display: grid;
    gap: 6px;
  }
  .reset-text {
    flex: 1 1 260px;
    margin: 0;
  }
</style>
