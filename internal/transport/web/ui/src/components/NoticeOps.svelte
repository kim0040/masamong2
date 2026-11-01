<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { scrollable } from '../lib/scrollable'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { toast } from '../lib/toast.svelte'
  import { num, relative } from '../lib/format'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Segmented from './Segmented.svelte'
  import KeyValue from './KeyValue.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // School and transfer notice boards: what each board last returned, who needs it, and a manual run.
  const st = resource<any>('notices.status', () => ({}), { live: ['config'] })
  let kind = $state<'all' | 'school' | 'transfer'>('all')
  let onlyNeeded = $state(true)
  let collecting = $state(false)
  const boards = $derived(((st.data?.boards ?? []) as any[]).filter((b) => (kind === 'all' || b.kind === kind) && (!onlyNeeded || b.needed || b.state)))
  const failures = $derived(Object.entries<string>(st.data?.last_collect?.failures ?? {}))
  async function collect() {
    collecting = true
    const r = await call('notices.subscribe', { op: 'collect' })
    collecting = false
    if (r.ok) toast('ok', t('notices.collected', { n: num(r.output.report?.new ?? 0), sources: num(r.output.report?.sources ?? 0) }))
    else toast('error', t('common.failed'), r.error?.message)
    void st.refresh()
  }
  function tone(b: any): 'ok' | 'danger' | 'neutral' | 'info' {
    if (b.manual) return 'info'
    if (!b.state) return 'neutral'
    return b.state.ok ? 'ok' : 'danger'
  }
  function label(b: any) {
    if (b.manual) return t('notices.manual')
    if (!b.state) return t('notices.never_read')
    return b.state.ok ? t('notices.read_ok', { n: num(b.state.items) }) : t('notices.read_failed')
  }
</script>

<div class="cols">
  <Card title={t('notices.overview')} description={t('notices.overview_desc')}>
    {#snippet actions()}<Button size="sm" variant="primary" icon={I.RefreshCw} loading={collecting} onclick={collect}>{t('notices.collect')}</Button>{/snippet}
    {#if !st.data}<LoadState res={st}><Skeleton lines={4} /></LoadState>{:else}
      <KeyValue items={[
        { k: t('notices.subs.school'), v: num(st.data.subscribers?.school ?? 0) },
        { k: t('notices.subs.transfer'), v: num(st.data.subscribers?.transfer ?? 0) },
        { k: t('notices.last_collect'), v: st.data.last_collect?.at ? `${relative(st.data.last_collect.at)} · ${t('notices.new_n', { n: num(st.data.last_collect.new ?? 0) })}` : t('notices.never') },
        { k: t('notices.catalog'), v: st.data.catalog === 'built-in' ? t('notices.catalog_builtin') : st.data.catalog, mono: st.data.catalog !== 'built-in' },
      ]} />
      <p class="xs muted" style:margin-top="10px">{t('notices.catalog_hint', { path: st.data.catalog_path })}</p>
      {#if failures.length}
        <details style:margin-top="10px"><summary class="small">{t('notices.failures', { n: failures.length })}</summary>
          <ul class="fails">{#each failures as [id, e]}<li class="xs"><code>{id}</code> <span class="muted break">{e}</span></li>{/each}</ul>
        </details>
      {/if}
    {/if}
  </Card>
</div>
<div style:margin-top="var(--s-4)">
  <Card padded={false} title={t('notices.boards')} description={t('notices.boards_desc')}>
    {#snippet actions()}
      <Segmented size="sm" label={t('notices.kind')} bind:value={kind} options={[{ value: 'all', label: t('notices.kind.all') }, { value: 'school', label: t('notices.kind.school') }, { value: 'transfer', label: t('notices.kind.transfer') }]} />
      <Segmented size="sm" label={t('notices.show')} value={onlyNeeded ? 'needed' : 'all'} onchange={(v) => (onlyNeeded = v === 'needed')} options={[{ value: 'needed', label: t('notices.show.needed') }, { value: 'all', label: t('notices.show.all') }]} />
    {/snippet}
    {#if !st.data}<div style:padding="20px"><LoadState res={st}><Skeleton lines={6} /></LoadState></div>
    {:else if boards.length === 0}<p class="small muted" style:padding="20px">{onlyNeeded ? t('notices.none_needed') : t('notices.none')}</p>
    {:else}
      <div use:scrollable class="table-scroll"><table class="table">
        <thead><tr><th>{t('notices.col.board')}</th><th>{t('notices.col.state')}</th><th>{t('notices.col.last')}</th></tr></thead>
        <tbody>
          {#each boards as b (b.id)}
            <tr>
              <td><a href={b.url} target="_blank" rel="noopener noreferrer">{b.university}{b.board ? ` · ${b.board}` : ''}</a>
                <div class="xs subtle">{t(`notices.kind.${b.kind}`)}{b.needed ? ` · ${t('notices.needed')}` : ''}{b.note ? ` · ${b.note}` : ''}</div></td>
              <td><Status tone={tone(b)} label={label(b)} size="sm" />{#if b.state && !b.state.ok && b.state.error}<div class="xs muted break">{b.state.error}</div>{/if}</td>
              <td class="xs muted nowrap">{b.state?.last_run ? relative(b.state.last_run) : '–'}</td>
            </tr>
          {/each}
        </tbody>
      </table></div>
    {/if}
  </Card>
</div>

<style>
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr));
  }
  .fails {
    list-style: none;
    margin: 8px 0 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
</style>
