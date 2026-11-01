<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { num } from '../lib/format'
  import Button from './Button.svelte'
  import * as I from '../lib/icons'

  // stats.query: how many polls, jobs and messages are stored in total, whatever the period picked above.
  // The Action counts whole tables on the instance's single database connection, which on a big history
  // and a small server takes long enough to hold up replies, so it runs when asked and never with the
  // page's own charts.
  let totals = $state<{ polls: number; jobs: number; messages: number } | null>(null)
  let busy = $state(false)
  let err = $state('')

  async function count() {
    if (busy) return
    busy = true
    err = ''
    const r = await call('stats.query', {})
    busy = false
    if (r.ok) totals = { polls: r.output.polls ?? 0, jobs: r.output.jobs ?? 0, messages: r.output.messages ?? 0 }
    else err = r.error?.message ?? t('common.failed')
  }
</script>

<div class="stored">
  <strong class="small">{t('stats.stored.title')}</strong>
  {#if totals}
    <span class="num small">{t('stats.stored.messages')} {num(totals.messages)}</span>
    <span class="num small">{t('stats.stored.jobs')} {num(totals.jobs)}</span>
    <span class="num small">{t('stats.stored.polls')} {num(totals.polls)}</span>
  {:else}
    <span class="xs muted">{t('stats.stored.hint')}</span>
  {/if}
  {#if err}<span class="small err" role="alert">{err}</span>{/if}
  <Button size="sm" variant="ghost" icon={I.RefreshCw} loading={busy} onclick={count}>{totals ? t('stats.stored.recount') : t('stats.stored.count')}</Button>
</div>

<style>
  .stored {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-4);
    margin-bottom: var(--s-4);
  }
  .err {
    color: var(--danger);
  }
</style>
