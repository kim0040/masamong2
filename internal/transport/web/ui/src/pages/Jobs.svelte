<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call, can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { useConfig } from '../lib/config.svelte'
  import { relative, datetime, num, usd } from '../lib/format'
  import { jobLine, stopReason, blockHref, etaText } from '../lib/blocks'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Tabs from '../components/Tabs.svelte'
  import Status from '../components/Status.svelte'
  import Progress from '../components/Progress.svelte'
  import Button from '../components/Button.svelte'
  import Confirm from '../components/Confirm.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import * as I from '../lib/icons'

  useConfig()
  const jobs = resource<{ jobs: any[] }>('jobs.list', () => ({}), { live: ['job'], poll: 5000 })
  const blocks = resource<any>('memory.blocks.list', () => ({}), { live: ['memory'] })
  const parse = (s: string) => {
    try {
      return s ? JSON.parse(s) : {}
    } catch {
      return {}
    }
  }
  // what a job is about, in words: the block a preparation works on
  function subject(j: any): { label: string; href: string } | null {
    const room = parse(j.payload).source_room
    if (!room) return null
    const b = (blocks.data?.blocks ?? []).find((x: any) => x.source_room === room)
    return { label: b?.label ?? room, href: blockHref(room) }
  }
  function outcome(j: any): string {
    const r = parse(j.result)
    if (j.status === 'succeeded' && r.leaf_vectors !== undefined) return t('jobs.prepare_done', { n: num(r.leaf_vectors + (r.from_cache ?? 0)), cost: usd(r.spent_usd ?? 0), took: r.elapsed ?? '' })
    if (r.stopped_by) return stopReason(r.stopped_by)
    if (j.error_code === 'INTERRUPTED') return t('jobs.interrupted')
    return r.error ?? (j.error_code ? t(`jobs.code.${j.error_code}`) : '')
  }
  let tab = $state('active')
  let cancelFor = $state<any>(null)
  let confirmOpen = $state(false)
  const groups: Record<string, string[]> = { active: ['pending', 'running', 'leased', 'paused', 'retry_wait'], failed: ['failed'], done: ['succeeded', 'cancelled'], all: [] }
  const list = $derived((jobs.data?.jobs ?? []).filter((j) => tab === 'all' || groups[tab]?.includes(j.status)))
  const count = (k: string) => (jobs.data?.jobs ?? []).filter((j) => k === 'all' || groups[k].includes(j.status)).length
  const tone = (s: string) => (s === 'failed' ? 'danger' : s === 'running' || s === 'leased' ? 'busy' : s === 'pending' || s === 'retry_wait' ? 'info' : s === 'cancelled' ? 'neutral' : 'ok')

  async function retry(id: string) {
    const r = await call('jobs.retry', { id })
    r.ok ? toast('ok', t('jobs.retried')) : toast('error', t('jobs.retry_failed'), r.error?.message)
    void jobs.refresh()
  }
  async function cancel() {
    const r = await call('jobs.cancel', { id: cancelFor.id })
    r.ok ? toast('ok', t('jobs.cancelled')) : toast('error', t('jobs.cancel_failed'), r.error?.message)
    void jobs.refresh()
  }
  const sched = resource<any>('schedules.list', () => ({}), { live: ['audit', 'config'] })
  function every(s: any): string {
    if (s.id === 'index') return t('jobs.sched.every_min', { n: s.every_min })
    if (s.id === 'backup') {
      if (s.schedule === 'off') return t('maint.sched.off')
      const at = `${String(s.hour).padStart(2, '0')}:00`
      return s.schedule === 'daily' ? t('jobs.sched.daily_at', { at }) : t('jobs.sched.weekly_at', { day: t(`weekday.${s.weekday}`), at })
    }
    return t('jobs.sched.daily')
  }
  let running = $state('')
  async function runNow(id: string) {
    running = id
    const r = await call('schedules.run', { id })
    running = ''
    r.ok ? toast('ok', t('jobs.sched.ran')) : toast('error', t('common.failed'), r.error?.message)
    void sched.refresh()
  }
</script>

<PageHeader title={t('jobs.title')} description={t('jobs.desc')} />

<Tabs id="jobs" bind:active={tab} tabs={[
  { id: 'active', label: t('jobs.tab.active'), count: count('active') },
  { id: 'failed', label: t('jobs.tab.failed'), count: count('failed') },
  { id: 'done', label: t('jobs.tab.done'), count: count('done') },
  { id: 'all', label: t('jobs.tab.all'), count: count('all') },
  { id: 'schedule', label: t('jobs.tab.schedule') }]} />

<div class="panel" role="tabpanel" id="jobs-panel-{tab}" aria-labelledby="jobs-tab-{tab}">
  {#if tab === 'schedule'}
    <Card padded={false}>
      <div use:scrollable class="table-scroll"><table class="table">
        <thead><tr><th>{t('jobs.sched.task')}</th><th>{t('jobs.sched.when')}</th><th>{t('jobs.sched.next')}</th><th>{t('jobs.sched.last')}</th><th>{t('jobs.sched.state')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
        <tbody>
          {#each sched.data?.schedules ?? [] as s (s.id)}
            <tr><td>{t(`jobs.sched.${s.id}`)}</td><td class="muted">{every(s)}</td>
              <td class="muted" title={datetime(s.next_run, true)}>{s.next_run ? relative(s.next_run) : '–'}</td>
              <td>{#if s.state?.last_run}<span title={s.state.error ?? ''}><Status tone={s.state.ok ? 'ok' : 'danger'} label={relative(s.state.last_run)} size="sm" /></span>{:else}<span class="muted">–</span>{/if}</td>
              <td><Status tone={s.on ? 'ok' : 'neutral'} label={s.on ? t('common.on') : t('common.off')} size="sm" /></td>
              <td class="r nowrap">
                {#if s.runnable}<Button size="sm" icon={I.Play} loading={running === s.id} disabled={!can('schedules.run')} onclick={() => runNow(s.id)}>{t('jobs.sched.run_now')}</Button>{/if}
                <Button href={s.route} size="sm" variant="ghost" iconEnd={I.ArrowRight}>{t('common.settings')}</Button>
              </td></tr>
          {/each}
        </tbody>
      </table></div>
    </Card>
  {:else if !jobs.data}
    <LoadState res={jobs}><Card><Skeleton lines={5} /></Card></LoadState>
  {:else if list.length === 0}
    <Card><EmptyState pose="sleep" title={t(`jobs.empty.${tab}`)} description={t('jobs.empty_body')} /></Card>
  {:else}
    <div class="jl">
      {#each list as j, i (j.id)}
        <Card class="enter">
          <div class="job">
            <div class="head">
              <strong>{t(`jobs.type.${j.type}`)}</strong>
              {#if subject(j)}{@const s = subject(j)}<a class="subj" href={s?.href}>{s?.label}</a>{/if}
              <Status tone={tone(j.status) as any} label={t(`jobs.status.${j.status}`)} size="sm" />
              <span class="spacer"></span>
              <span class="xs muted num" title={datetime(j.updated_at, true)}>{j.attempt > 1 ? `${t('jobs.attempt', { n: j.attempt })} · ` : ''}{relative(j.updated_at)}</span>
            </div>
            {#if j.progress_label || j.status === 'running'}
              <Progress value={j.progress_pct} label={t('jobs.progress')} />
              <div class="xs muted row">{jobLine(j)}{#if etaText(j)}<span class="spacer"></span>{t('jobs.eta', { eta: etaText(j) })}{/if}</div>
            {/if}
            {#if outcome(j)}<p class="small" class:err={j.status === 'failed'}>{outcome(j)}</p>{/if}
            <div class="row">
              <span class="xs subtle" title={j.id}>{datetime(j.created_at)}</span>
              <span class="spacer"></span>
              {#if ['failed', 'cancelled'].includes(j.status)}<Button size="sm" icon={I.RotateCcw} onclick={() => retry(j.id)}>{t('jobs.retry')}</Button>{/if}
              {#if ['pending', 'running', 'leased', 'paused', 'retry_wait'].includes(j.status)}<Button size="sm" variant="danger-ghost" icon={I.Square} onclick={() => { cancelFor = j; confirmOpen = true }}>{t('jobs.cancel')}</Button>{/if}
            </div>
          </div>
        </Card>
      {/each}
    </div>
  {/if}
</div>

<Confirm bind:open={confirmOpen} title={t('jobs.cancel_title')} body={t('jobs.cancel_body')} confirmLabel={t('jobs.cancel_confirm')} danger onconfirm={cancel} />

<style>
  .panel {
    margin-top: var(--s-4);
  }
  .jl {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 420px), 1fr));
  }
  .job {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .err {
    color: var(--danger);
  }
  .subj {
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 16em;
  }
</style>
