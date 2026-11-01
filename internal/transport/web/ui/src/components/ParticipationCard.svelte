<script lang="ts">
  import { t, has } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { patchConfig } from '../lib/config.svelte'
  import { toast } from '../lib/toast.svelte'
  import { relative, num, usd } from '../lib/format'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Segmented from './Segmented.svelte'
  import Status from './Status.svelte'
  import Confirm from './Confirm.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Joining the conversation unasked (participation.mode): the channel's mode under the instance cap,
  // what the code filters dropped, and the log of candidates to judge before going conservative.
  let { guild, channel }: { guild: string; channel: string } = $props()
  const st = resource<any>('participation.mode', () => ({ guild_id: guild, channel_id: channel, limit: 20 }), { live: ['config', 'participation'] })
  let confirmOpen = $state(false)
  const modes = ['off', 'collect', 'shadow', 'conservative']
  async function setMode(mode: string) {
    if (mode === 'conservative' && st.data?.mode !== 'conservative') return (confirmOpen = true)
    await apply(mode)
  }
  async function apply(mode: string) {
    const r = await call('participation.mode', { guild_id: guild, channel_id: channel, mode })
    r.ok ? toast('ok', t('part.saved')) : toast('error', t('common.failed'), r.error?.message)
    void st.refresh()
  }
  // the instance switch: the least that lets this channel's choice apply
  async function raiseInstance() {
    const want = ['collect', 'shadow', 'conservative'].includes(st.data?.mode) ? st.data.mode : 'shadow'
    await patchConfig([{ path: ['participation', 'enabled'], value: true }, { path: ['participation', 'mode'], value: want }], { success: t('part.instance_on') })
    void st.refresh()
  }
  async function review(id: string, verdict: string) {
    const r = await call('participation.mode', { review: id, verdict })
    if (!r.ok) toast('error', t('common.failed'), r.error?.message)
    void st.refresh()
  }
  const capped = $derived(st.data && st.data.mode !== 'off' && st.data.effective !== st.data.mode)
  const tone = (s: string) => (s === 'spoken' ? 'ok' : s === 'shadow' || s === 'collected' ? 'info' : s === 'error' ? 'danger' : 'neutral') as any
  const good = $derived(st.data?.reviewed?.good ?? 0)
  const bad = $derived(st.data?.reviewed?.bad ?? 0)
  // labels on collected moments (no judge looked at them) are shown apart and never feed the confirm text
  const cGood = $derived(st.data?.reviewed?.collected_good ?? 0)
  const cBad = $derived(st.data?.reviewed?.collected_bad ?? 0)
  const textless = $derived((st.data?.log ?? []).some((e: any) => !e.message && e.mode === 'collect'))
</script>

<Confirm bind:open={confirmOpen} title={t('part.confirm_title')} body={t('part.confirm_body', { good, bad })} confirmLabel={t('part.confirm')} onconfirm={() => apply('conservative')} />

<Card title={t('part.title')} description={t('part.desc')}>
  {#snippet actions()}<Status tone="warn" label={t('conv.experimental')} size="sm" />{/snippet}
  {#if !st.data}<LoadState res={st}><Skeleton lines={4} /></LoadState>{:else}
    <div class="stack">
      <Segmented label={t('part.title')} value={st.data.mode} onchange={setMode} options={modes.map((m) => ({ value: m, label: t(`part.mode.${m}`) }))} />
      <p class="small muted">{t(`part.mode_hint.${st.data.mode}`)}</p>
      {#if capped}
        <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span>
          <span>{t('part.capped', { mode: t(`part.mode.${st.data.effective}`) })} <Button size="sm" variant="ghost" onclick={raiseInstance}>{t('part.raise')}</Button></span></div>
      {/if}
      <div class="row small">
        {#each ['collected', 'shadow', 'spoken', 'silent', 'cancelled'] as k}
          <span class="chip">{t(`part.state.${k}`)} {num(st.data.stats_7d?.[k] ?? 0)}</span>
        {/each}
        {#if good + bad}<span class="chip accent">{t('part.reviewed', { good, bad })}</span>{/if}
      </div>
      {#if cGood + cBad}<p class="xs muted">{t('part.reviewed_collected', { good: cGood, bad: cBad })}</p>{/if}
      {#if Object.keys(st.data.filtered ?? {}).length}
        <p class="xs muted">{t('part.filtered')}: {Object.entries(st.data.filtered).map(([k, v]) => `${has(`part.why.${k}`) ? t(`part.why.${k}`) : k} ${v}`).join(' · ')}</p>
      {/if}
      {#if (st.data.log ?? []).length === 0}
        <p class="small muted">{st.data.mode === 'off' ? t('part.empty_off') : t('part.empty')}</p>
      {:else}
        {#if textless}<p class="xs muted">{t('part.no_text')}</p>{/if}
        <ul class="log">
          {#each st.data.log as e (e.id)}
            <li>
              <div class="head">
                <Status tone={tone(e.state)} label={t(`part.state.${e.state}`)} size="sm" />
                <span class="xs muted">{t(`part.kind.${e.kind || 'question'}`)}{e.score ? ` · ${e.score.toFixed(2)}` : ''}{e.cost_usd ? ` · ${usd(e.cost_usd, 4)}` : ''} · {relative(e.at)}</span>
              </div>
              <p class="small q">{e.message || `${t(`part.kind.${e.kind || 'question'}`)} · ${relative(e.at)}`}</p>
              {#if e.draft}<p class="small draft">{e.draft}</p>{/if}
              {#if e.reason}<p class="xs muted">{has(`part.why.${e.reason}`) ? t(`part.why.${e.reason}`) : e.reason}</p>{/if}
              {#if e.next}<p class="xs muted">{t('part.next_label')}: {has(`part.next.${e.next}`) ? t(`part.next.${e.next}`) : e.next}</p>{/if}
              {#if e.state === 'shadow' || e.state === 'spoken' || e.state === 'collected'}
                {#if e.state === 'collected'}<p class="xs muted">{t('part.ask_collected')}</p>{/if}
                <div class="row">
                  <Button size="sm" variant={e.verdict === 'good' ? 'secondary' : 'ghost'} icon={I.ThumbsUp} aria-pressed={e.verdict === 'good'} onclick={() => review(e.id, e.verdict === 'good' ? '' : 'good')}>{t('part.good')}</Button>
                  <Button size="sm" variant={e.verdict === 'bad' ? 'secondary' : 'ghost'} icon={I.ThumbsDown} aria-pressed={e.verdict === 'bad'} onclick={() => review(e.id, e.verdict === 'bad' ? '' : 'bad')}>{t('part.bad')}</Button>
                </div>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</Card>

<style>
  .log {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: var(--s-3);
    max-height: 520px;
    overflow: auto;
  }
  .log li {
    display: grid;
    gap: 4px;
    padding: 10px 12px;
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .log p {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .q {
    color: var(--text-muted);
  }
  .draft {
    white-space: pre-wrap;
    border-inline-start: 3px solid var(--accent);
    padding-inline-start: 8px;
  }
</style>
