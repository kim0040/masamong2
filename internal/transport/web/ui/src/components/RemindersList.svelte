<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { toast } from '../lib/toast.svelte'
  import { datetime, dateIn } from '../lib/format'
  import { repeatText } from '../lib/reminder'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Reminders people asked for (reminders.update list): in a channel or of one person. The console can
  // cancel one (a repeating one: the whole series) and pause or resume a series; only the person who
  // asked can set or change it, in Discord. A scheduled task (kind task) also shows the request the bot
  // runs when it is due.
  let { scope, empty }: { scope: Record<string, string>; empty: string } = $props()
  const list = resource<any>('reminders.update', () => ({ op: 'list', limit: 50, ...scope }), { live: ['reminders'] })
  // still to come: asked, set, paused, or a series whose occurrence is going out
  const upcoming = (r: any) => r.status === 'proposed' || r.status === 'scheduled' || r.status === 'paused' || (r.status === 'sending' && !!r.repeat)
  const tone = (s: string) => (s === 'scheduled' ? 'info' : s === 'sent' ? 'ok' : s === 'failed' || s === 'unknown' ? 'warn' : 'neutral') as any
  async function cancel(id: string) {
    const r = await call('reminders.update', { op: 'cancel', id })
    r.ok && r.output.cancelled ? toast('ok', t('rem.cancelled')) : toast('error', t('common.failed'), r.error?.message)
    void list.refresh()
  }
  async function pause(id: string, resume: boolean) {
    const r = await call('reminders.update', { op: resume ? 'resume' : 'pause', id })
    r.ok && (resume ? r.output.resumed : r.output.paused) ? toast('ok', t(resume ? 'rem.resumed' : 'rem.paused')) : toast('error', t('common.failed'), r.error?.message)
    void list.refresh()
  }
  const rule = (r: any) => repeatText(r, t, (iso, tz) => dateIn(iso, tz))
  /** The second line: for a series its rule and next occurrence, else the time and where it goes. */
  const detail = (r: any) => {
    const where = r.destination ? t(`rem.dest.${r.destination}`) : t('rem.dest.none')
    const owner = r.owner_name ? ` · ${r.owner_name}` : ''
    const err = r.error ? ` · ${r.error}` : ''
    if (!r.repeat) return `${r.due_at ? datetime(r.due_at) : t('rem.no_time_yet')} · ${where}${owner}${err}`
    const next = r.due_at && r.status !== 'paused' && r.status !== 'proposed' ? t('rem.next', { when: datetime(r.due_at) }) : ''
    const sent = r.occurrences ? t('rem.occurrences', { n: r.occurrences }) : ''
    return [rule(r), next, sent, where].filter(Boolean).join(' · ') + owner + err
  }
  const sorted = $derived(((list.data?.reminders ?? []) as any[]).slice().sort((a, b) => Number(upcoming(b)) - Number(upcoming(a))))
</script>

{#if !list.data}<LoadState res={list}><Skeleton lines={3} /></LoadState>
{:else if sorted.length === 0}<p class="small muted">{empty}</p>
{:else}
  <ul class="rem">
    {#each sorted as r (r.id)}
      <li>
        <Status tone={tone(r.status)} label={t(`rem.state.${r.status}`)} size="sm" />
        <div class="ml">
          <span class="small">{#if r.kind === 'task'}<span class="kind">{t('rem.kind.task')}</span>{/if}{r.content}</span>
          {#if r.kind === 'task' && r.task}<span class="xs muted task" title={r.task}>{t('rem.task_line', { task: r.task })}</span>{/if}
          <span class="xs muted">{detail(r)}</span>
        </div>
        {#if r.repeat && (r.status === 'scheduled' || r.status === 'paused')}
          <Button size="sm" variant="ghost" onclick={() => pause(r.id, r.status === 'paused')}>{t(r.status === 'paused' ? 'rem.resume' : 'rem.pause')}</Button>
        {/if}
        {#if upcoming(r)}<Button size="sm" variant="danger-ghost" icon={I.X} onclick={() => cancel(r.id)}>{t(r.repeat ? 'rem.cancel_series' : 'rem.cancel')}</Button>{/if}
      </li>
    {/each}
  </ul>
{/if}

<style>
  .rem {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .rem li {
    display: flex;
    align-items: center;
    gap: var(--s-2);
  }
  .kind {
    display: inline-block;
    margin-right: 6px;
    padding: 0 6px;
    border-radius: var(--r-sm);
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: 11px;
    line-height: 18px;
    vertical-align: 1px;
  }
  .task {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ml {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
</style>
