<script lang="ts">
  // filling every role a model provider can serve with its newest suitable models. Each changed role
  // can be left out; the embedding model starts left out while memory is indexed (a new one empties
  // memory search until every room is embedded again), and choosing it shows what that costs.
  import { tick } from 'svelte'
  import { t, has } from '../../lib/i18n.svelte'
  import { scrollable } from '../../lib/scrollable'
  import { call } from '../../lib/api.svelte'
  import { onLive } from '../../lib/live.svelte'
  import { navigate } from '../../lib/router.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { num, usd } from '../../lib/format'
  import { latest } from '../../lib/seqload'
  import { embedReindexAsk, type EmbedAsk } from '../../lib/embedswitch'
  import { recommendedWidth, recTicks, recAfterRefresh, recLiveStart, recLiveChange, recLiveLoaded, type RecWidth, type RecLive } from '../../lib/embedwidth'
  import { ROLES } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Button from '../Button.svelte'
  import Switch from '../Switch.svelte'
  import Confirm from '../Confirm.svelte'
  import CapTags from './CapTags.svelte'
  import * as I from '../../lib/icons'

  let { conn }: { conn: Connections } = $props()

  let recFor = $state('')
  let rec = $state<any>(null)
  let recBusy = $state(false)
  let recOn = $state<Record<string, boolean>>({})
  let recReindex = $state(true)
  // A settings change announced while the first preview is still being read is remembered, and the preview is read
  // again as soon as the first answer is in (Apply and the width proposals wait for that).
  let recLive: RecLive = { loading: false, dirty: false }
  export async function openRecommend(id: string) {
    recFor = id
    rec = null
    recBusy = true
    recLive = recLiveStart()
    const r = await call('models.recommend', { provider: id })
    recBusy = false
    const loaded = recLiveLoaded(recLive, r.ok)
    recLive = loaded.state
    if (!r.ok) {
      recFor = ''
      return toast('error', t('conn.rec_failed'), r.error?.message)
    }
    refreshes.cancel()
    recRefreshing = false
    recApplied = {}
    rec = r.output
    recOn = recTicks(rec)
    recReindex = true
    if (loaded.refresh) void refreshRecommend()
  }
  // The width proposals (and the picks themselves) are true for the saved settings the preview was made on. A role
  // saved from this very dialog (the cheaper-model buttons) or changed in another tab moves those settings, so the
  // preview is read again: the ticks the operator set stay where the new preview still proposes the role, and
  // Apply waits for the new answer.
  let recRefreshing = $state(false)
  const refreshes = latest()
  let recApplied: Record<string, { provider: string; model: string }> = {}
  async function refreshRecommend() {
    const id = recFor
    if (!id || !rec) return
    const live = refreshes.start()
    recRefreshing = true
    const r = await call('models.recommend', { provider: id })
    if (!live()) return
    recRefreshing = false
    if (recFor !== id) return
    if (!r.ok) {
      // the preview on screen is for settings that have moved on, and a new one cannot be had: do not apply it
      recFor = ''
      return toast('error', t('conn.rec_failed'), r.error?.message)
    }
    // a confirmation asked about the old preview's picks is void
    embedOpen = false
    rec = recAfterRefresh(r.output, recApplied)
    recOn = recTicks(rec, recOn)
  }
  $effect(() => {
    if (!recFor) return
    return onLive(['config'], () => {
      const c = recLiveChange(recLive, recBusy)
      recLive = c.state
      if (c.refresh) void refreshRecommend()
    })
  })
  // cost-effective alternatives per role (the server ranks them); one click sets the role like the picker does
  let budgetBusy = $state('')
  async function useBudget(role: string, model: string) {
    budgetBusy = `${role}|${model}`
    const r = await call('models.select', { role, provider: recFor, model })
    budgetBusy = ''
    if (!r.ok) return toast('error', t('common.failed'), r.error?.message)
    toast('ok', t('conn.model_set', { role: t(`role.${role}`), model }))
    rec.budget[role] = rec.budget[role].map((b: any) => ({ ...b, current: b.model === model }))
    // the curated pick for this role no longer applies: "Apply" must not send it and put the old model back
    recOn[role] = false
    recApplied[role] = { provider: recFor, model }
    for (const p of rec.picks ?? []) if (p.role === role) Object.assign(p, { current_provider: recFor, current_model: model, replaced: p.model !== model, changed: false })
    void conn.providers.refresh()
    // the saved settings moved: the width proposals made on the old ones are read again
    void refreshRecommend()
  }
  const budgetRoles = $derived(ROLES.filter((r) => rec?.budget?.[r]?.length))
  const recEmbed = $derived(!!rec?.reindex && !!recOn.embed)
  // the width the server would store for the picks as ticked now: with or without the rerank pick
  const recW = $derived(recRefreshing ? null : recommendedWidth(rec, recOn))
  const recWidthText = (w: RecWidth) => (w.dimensions === 0 ? (w.full > 0 ? t('conn.rec_width_whole', { n: w.full }) : t('conn.width_unknown')) : t('conn.rec_width', { n: w.dimensions }))
  // memory was indexed after the preview: the server refuses with the estimate, so ask like the model picker does
  let embedAsk = $state<(EmbedAsk & { provider: string; model: string }) | null>(null)
  let embedOpen = $state(false)
  async function applyRecommend(includeEmbed = false) {
    if (recRefreshing) return
    recBusy = true
    // exactly what the preview showed and the operator kept: no second model list
    const picks = Object.fromEntries((rec?.picks ?? []).filter((x: any) => x.changed && recOn[x.role]).map((x: any) => [x.role, x.model]))
    const r = await call('models.recommend', { provider: recFor, apply: true, picks, include_embed: recEmbed || includeEmbed, reindex: recEmbed && recReindex })
    recBusy = false
    if (!r.ok) {
      const ask = picks.embed && !includeEmbed ? embedReindexAsk(r.error) : null
      if (!ask) return toast('error', t('common.failed'), r.error?.message)
      embedAsk = { ...ask, provider: recFor, model: picks.embed }
      embedOpen = true
      return
    }
    const jobs = (r.output?.reindex_jobs ?? []).length
    if (jobs) toast('ok', t('conn.rec_applied', { id: conn.provName(recFor) }), t('conn.rec_reindex_started', { n: jobs }), { label: t('conn.rec_jobs_go'), run: () => navigate('/jobs') })
    else toast('ok', t('conn.rec_applied', { id: conn.provName(recFor) }), r.output?.embed_changes ? t('conn.rec_reindex') : undefined, r.output?.embed_changes ? { label: t('conn.rec_reindex_go'), run: () => navigate('/blocks') } : undefined)
    recFor = ''
    void conn.providers.refresh()
  }
  // the estimate the refusal carried; without it the generic note, never "0 pieces in 0 rooms"
  const embedBody = (a: EmbedAsk | null) =>
    !a ? '' : !a.known ? t('conn.embed_change_note') : t('conn.embed_switch_body', { leaves: num(a.leaves), rooms: num(a.rooms) }) + (a.tokens > 0 ? ` ${t('conn.embed_switch_tokens', { tokens: num(a.tokens) })}` : '')
  async function confirmEmbedSwitch() {
    if (!embedAsk) return
    embedOpen = false
    await tick()
    await applyRecommend(true)
  }
</script>

<Dialog open={!!recFor} onclose={() => (recFor = '')} title={t('conn.rec_title', { id: conn.provName(recFor) })} description={t('conn.rec_desc')} size="lg">
  {#if recBusy && !rec}<p class="small muted">{t('common.loading')}</p>
  {:else if rec}
    <div class="stack">
      {#if !rec.picks?.length}<p class="small muted">{t('conn.rec_none')}</p>{:else}
        <div use:scrollable class="table-scroll"><table class="table picks">
          <thead><tr><th><span class="sr-only">{t('conn.rec_use')}</span></th><th>{t('conn.role')}</th><th>{t('conn.rec_now')}</th><th>{t('conn.rec_new')}</th><th class="r">{t('conn.price')}</th></tr></thead>
          <tbody>
            {#each rec.picks as r (r.role)}
              <tr class:same={!r.changed || !recOn[r.role]}>
                <td>{#if r.changed}<input type="checkbox" bind:checked={recOn[r.role]} aria-label={t('conn.rec_use_role', { role: t(`role.${r.role}`) })} />{/if}</td>
                <td><strong class="small">{t(`role.${r.role}`)}</strong>{#if r.replaced}<div class="xs muted">{t('conn.rec_replaced')}</div>{:else if !r.changed}<div class="xs muted">{t('conn.rec_same')}</div>{/if}</td><td class="xs mono muted break">{r.current_model || '–'}<div>{conn.provName(r.current_provider ?? '')}</div></td><td class="small mono break">{r.model}{#if r.role === 'embed' && recW}<div class="xs rec-width"><strong>{recWidthText(recW)}</strong>{#if has(`conn.rec_width_why.${recW.reason}`)}<div class="muted">{t(`conn.rec_width_why.${recW.reason}`)}</div>{/if}</div>{/if}</td>
                <td class="r xs num">{r.in_per_mtok !== undefined ? `${usd(r.in_per_mtok)}${r.out_per_mtok ? ' / ' + usd(r.out_per_mtok) : ''}` : t('conn.price_unknown')}</td></tr>
            {/each}
          </tbody>
        </table></div>
      {/if}
      <h3 class="sub-title">{t('conn.rec_budget_title')}</h3>
      {#if !budgetRoles.length}<p class="small muted">{t('conn.rec_budget_none')}</p>{:else}
        <p class="xs muted">{t('conn.rec_budget_desc')}</p>
        <div use:scrollable class="table-scroll"><table class="table budget">
          <thead><tr><th>{t('conn.role')}</th><th>{t('conn.model')}</th><th class="r">{t('conn.ctx')}</th><th class="r">{t('conn.col_in')}</th><th class="r">{t('conn.col_out')}</th><th class="r">{t('conn.rec_blended')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {#each budgetRoles as role (role)}
              {#each rec.budget[role] as b, i (b.model)}
                <tr class:first={i === 0}>
                  <td>{#if i === 0}<strong class="small">{t(`role.${role}`)}</strong>{/if}</td>
                  <td><div class="small">{b.name || b.model}</div><div class="xs mono subtle break">{b.model}</div><CapTags m={b} chat={role === 'response' || role === 'planner'} /></td>
                  <td class="r num xs">{b.context_length ? num(b.context_length) : '–'}</td>
                  <td class="r num xs">{usd(b.in_per_mtok)}</td><td class="r num xs">{usd(b.out_per_mtok)}</td><td class="r num xs">{usd(b.blended_per_mtok)}</td>
                  <td class="r"><Button size="sm" variant={b.current ? 'secondary' : 'ghost'} disabled={b.current || (role === 'embed' && !!rec.embed_indexed)} loading={budgetBusy === `${role}|${b.model}`} aria-label={`${t('conn.rec_use_this')}: ${t(`role.${role}`)} · ${b.name || b.model}`} onclick={() => useBudget(role, b.model)}>{b.current ? t('conn.current') : t('conn.rec_use_this')}</Button></td>
                </tr>
              {/each}
            {/each}
          </tbody>
        </table></div>
        {#if rec.embed_indexed && rec.budget?.embed}<p class="xs muted">{t('conn.rec_budget_embed_locked')}</p>{/if}
      {/if}
      {#if !rec.rerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.rec_no_rerank')}</span></div>{/if}
      {#if rec.reindex}
        <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span>
          <div class="stack tight">
            <span class="small">{recEmbed ? t('conn.rec_embed_on', { leaves: num(rec.reindex.leaves), rooms: num(rec.reindex.rooms), tokens: num(rec.reindex.tokens), usd: rec.reindex.usd !== undefined ? usd(rec.reindex.usd) : t('conn.price_unknown') }) : t('conn.rec_embed_off', { leaves: num(rec.reindex.leaves) })}</span>
            {#if recEmbed}<Switch bind:checked={recReindex} label={t('conn.rec_reindex_now')} />{/if}
          </div>
        </div>
      {:else if rec.embed_changes && recOn.embed}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.embed_change_note')}</span></div>{/if}
    </div>
  {/if}
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (recFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Check} loading={(recBusy || recRefreshing) && !!rec} disabled={recRefreshing || !rec?.picks?.some((r: any) => r.changed && recOn[r.role])} onclick={() => applyRecommend()}>{t('conn.rec_apply')}</Button>
  {/snippet}
</Dialog>

<Confirm bind:open={embedOpen} title={t('conn.embed_switch_title')} body={embedBody(embedAsk)} confirmLabel={t('conn.embed_switch_confirm')} onconfirm={confirmEmbedSwitch} />

<style>
  .sub-title {
    margin: var(--s-5) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .stack > .sub-title {
    margin: 0;
  }
  .tight {
    padding: 6px 10px;
  }
  tr.same td {
    color: var(--text-muted);
  }
  .rec-width {
    margin-top: 4px;
    font-family: var(--font);
  }
  .budget tr.first td {
    box-shadow: inset 0 1px 0 var(--border);
  }
  /* model names break anywhere, so without a floor a phone squeezes them to a few letters per line: the table keeps
     a readable width and scrolls sideways inside its frame instead */
  .picks {
    min-width: 44rem;
  }
  /* the role name column: a long one (Japanese "プランナー") must not break inside the word */
  .picks td:nth-child(2) {
    min-width: 7rem;
  }
  .budget {
    min-width: 44rem;
  }
</style>
