<script lang="ts">
  import { overview, refreshOverview } from '../lib/overview.svelte'
  import { session, call, practice, setPractice } from '../lib/api.svelte'
  import { staleResources, refreshStale } from '../lib/resource.svelte'
  import { live, noteLifecycle } from '../lib/live.svelte'
  import { t } from '../lib/i18n.svelte'
  import { relative } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import Button from './Button.svelte'
  import { Pause, WifiOff, RotateCcw, FlaskConical, TriangleAlert } from '../lib/icons'
  import Confirm from './Confirm.svelte'
  import { slide } from 'svelte/transition'
  import { motionAllowed } from '../lib/prefs.svelte'

  const pause = $derived(overview.data?.pause)
  const restart = $derived((overview.data?.attention ?? []).some((a: any) => a.key === 'restart_pending'))
  const d = $derived(motionAllowed() ? 240 : 0)
  let busy = $state(false)
  async function resume() {
    busy = true
    await call('runtime.pause', { scope: 'all', paused: 'false' })
    await refreshOverview()
    busy = false
    toast('ok', t('pause.resumed'))
  }
  // data on screen that could not be refreshed: the offline and restart banners already say why
  // when the whole service is away, so this one is for a single failing call
  const staleShown = $derived(staleResources.count > 0 && !session.offline && live.status !== 'offline' && !live.lifecycle)
  let refreshing = $state(false)
  async function retryStale() {
    refreshing = true
    await refreshStale()
    refreshing = false
  }
  let resetOpen = $state(false)
  async function resetPractice() {
    await setPractice(true, true)
  }
  async function restartNow() {
    busy = true
    const r = await call('runtime.restart')
    busy = false
    if (r.ok) noteLifecycle('restart')
  }
</script>

{#if practice.on}
  <div class="bn practice" role="note">
    <FlaskConical size={16} aria-hidden="true" />
    <span><strong>{t('practice.banner_title')}</strong> · {t('practice.banner_body')}</span>
    <Button size="sm" variant="ghost" onclick={() => (resetOpen = true)}>{t('practice.reset')}</Button>
    <Button size="sm" variant="secondary" onclick={() => setPractice(false)}>{t('practice.leave')}</Button>
  </div>
{/if}
{#if live.lifecycle === 'restart'}
  <div class="bn info" transition:slide={{ duration: d }} role="status"><RotateCcw size={16} aria-hidden="true" /><span><strong>{t('maint.restarting')}</strong> · {t('maint.restarting_body')}</span></div>
{:else if session.offline || live.status === 'offline'}
  <div class="bn danger" transition:slide={{ duration: d }} role="status"><WifiOff size={16} aria-hidden="true" /><span>{t('banner.offline')}</span></div>
{/if}
{#if staleShown}
  <div class="bn warn" transition:slide={{ duration: d }} role="status">
    <TriangleAlert size={16} aria-hidden="true" />
    <span>{t('load.stale')}</span>
    <Button size="sm" variant="secondary" onclick={retryStale} loading={refreshing}>{t('common.retry')}</Button>
  </div>
{/if}
{#if pause?.all}
  <div class="bn warn" transition:slide={{ duration: d }} role="status">
    <Pause size={16} aria-hidden="true" />
    <span><strong>{t('banner.paused')}</strong> · {relative(pause.at)}{pause.reason ? ` · ${pause.reason}` : ''}</span>
    <Button size="sm" variant="secondary" onclick={resume} loading={busy}>{t('pause.resume')}</Button>
  </div>
{/if}
{#if restart}
  <div class="bn info" transition:slide={{ duration: d }} role="status">
    <RotateCcw size={16} aria-hidden="true" />
    <span>{t('banner.restart_pending')}</span>
    <Button size="sm" variant="secondary" onclick={restartNow} loading={busy}>{t('maint.restart_now')}</Button>
  </div>
{/if}

<Confirm bind:open={resetOpen} title={t('practice.reset_title')} body={t('practice.reset_body')} confirmLabel={t('practice.reset')} onconfirm={resetPractice} />

<style>
  .practice {
    background: var(--practice-soft);
  }
  .practice :global(svg) {
    color: var(--practice);
  }
  .bn {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    padding: 8px clamp(16px, 2.2vw, 40px);
    font-size: var(--text-sm);
  }
  .bn > span {
    flex: 1 1 240px;
    min-width: 0;
  }
  .bn {
    flex-wrap: wrap;
  }
  .warn {
    background: var(--warn-soft);
    color: var(--text);
  }
  .warn :global(svg) {
    color: var(--warn);
  }
  .danger {
    background: var(--danger-soft);
  }
  .danger :global(svg) {
    color: var(--danger);
  }
  .info {
    background: var(--info-soft);
  }
  .info :global(svg) {
    color: var(--info);
  }
</style>
