<script lang="ts">
  // the table of roles: which model each one uses, what it costs, what it falls back to, and the buttons that change them
  import { t } from '../../lib/i18n.svelte'
  import { scrollable } from '../../lib/scrollable'
  import { call, can } from '../../lib/api.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { usd, relative } from '../../lib/format'
  import { FALLBACK_ROLES, OPTIONAL_ROLES, ROLES, effortRole, effortOps, reasonKey } from '../../lib/connections'
  import type { EmbedWidthInfo } from '../../lib/embedwidth'
  import type { Connections } from '../../lib/connections.svelte'
  import Card from '../Card.svelte'
  import Button from '../Button.svelte'
  import Confirm from '../Confirm.svelte'
  import Select from '../Select.svelte'
  import * as I from '../../lib/icons'

  let { conn, onbrowse, onprice, onfallbacks, onwidth }: {
    conn: Connections
    onbrowse: (role: string) => void
    onprice: (role: string) => void
    onfallbacks: (role: string) => void
    onwidth: () => void
  } = $props()

  // a published price is a snapshot the release carried; past this age it may no longer be the host's
  const PRICE_STALE_DAYS = 120
  const DAY_MS = 864e5
  const prices = $derived(conn.prices)
  function priceText(role: string): string {
    const p = prices[role]
    if (!p || p.source === 'unknown') return t('conn.price_unknown')
    const out = p.out_per_mtok ? ` / ${usd(p.out_per_mtok)}` : ''
    return `${usd(p.in_per_mtok)}${out}`
  }
  const priceSource = (role: string) => {
    const p = prices[role]
    return p?.source === 'published' ? t('conn.price_src.published', { date: p.checked ?? '' }) : t(`conn.price_src.${p?.source}`)
  }
  const priceStale = (role: string) => {
    const p = prices[role]
    return p?.source === 'published' && !!p.checked && Date.now() - Date.parse(p.checked) > PRICE_STALE_DAYS * DAY_MS
  }
  const widthText = (info: EmbedWidthInfo) =>
    conn.wstate === 'full' ? t('conn.width_full', { n: info.effective }) : conn.wstate === 'narrow' ? t('conn.width_narrow', { n: info.effective, full: info.full_width }) : conn.wstate === 'over' ? t('conn.width_over', { n: info.effective, full: info.full_width }) : conn.wstate === 'custom' ? t('conn.width_custom', { n: info.effective }) : t('conn.width_unknown')

  // the level is changed right in the role's row; the model's own list greys out what it does not take
  async function setEffort(role: string, level: string) {
    if ((cfg.doc?.models?.[role]?.effort ?? '') === level) return
    await patchConfig(effortOps(role, level), { success: t('conn.effort_saved') })
  }

  // roles that may stay empty: the table offers to clear them
  let clearFor = $state('')
  let clearOpen = $state(false)
  async function clearRole() {
    const role = clearFor
    const r = await call('models.select', { role, clear: true })
    if (r.ok) toast('ok', t('conn.role_cleared', { role: t(`role.${role}`) }))
    else toast('error', t('common.failed'), r.error?.message)
    clearFor = ''
    void conn.providers.refresh()
  }
</script>

{#each conn.blocked as b (b.id)}
  <div class="callout danger blocked" role="status"><span class="ic"><I.CircleAlert size={16} /></span>
    <span class="small">{t(b.rejected ? 'conn.rejected_keys' : 'conn.missing_keys', { id: conn.provName(b.id), roles: conn.roleNames(b.roles) })}</span>
  </div>
{/each}
<h3 class="sub-title">{t('conn.models')}</h3>
<Card padded={false}>
  <div use:scrollable class="table-scroll"><table class="table roles">
    <thead><tr><th>{t('conn.role')}</th><th>{t('conn.model')}</th><th>{t('conn.effort')}</th><th>{t('conn.price')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
    <tbody>
      {#each ROLES as role}
        {@const st = conn.roleState(role)}
        {@const m = cfg.doc?.models?.[role]}
        <tr>
          <td class="c-role"><strong>{t(`role.${role}`)}</strong><div class="xs muted">{t(`role.${role}_hint`)}</div></td>
          <td class="c-model"><span class="mono small break">{m?.model ?? '–'}</span>
            {#if m?.provider}<div class="xs muted">{conn.provName(m.provider)}{#if st === 'no_key' || st === 'rejected'} · <span class="warn-text">{st === 'rejected' ? t('conn.badge_rejected') : t('tools.state.no_key')}</span>{/if}</div>{/if}
            {#if m?.provider && conn.resting[`${m.provider}/${m.model}`]}<div class="xs warn-text">{t('conn.fb_resting', { when: relative(conn.resting[`${m.provider}/${m.model}`].until) })}</div>{/if}
            {#if FALLBACK_ROLES.includes(role) && m?.fallbacks?.length}
              <details class="fb">
                <summary class="xs muted">{t('conn.fb_summary', { n: m.fallbacks.length })}</summary>
                <ol class="fb-list xs">
                  {#each m.fallbacks as f, i (i)}
                    <li><span class="muted">{t('conn.fb_line', { n: i + 1 })}</span> <span class="mono break">{f.model}</span> <span class="muted">· {conn.provName(f.provider)} · {t(reasonKey(conn.reasoningOf(f.provider, f.model)))}{#if f.effort} · {t('conn.effort_now', { level: t(`conn.effort.${f.effort}`) })}{/if}</span>{#if conn.resting[`${f.provider}/${f.model}`]} <span class="warn-text">· {t('conn.fb_resting', { when: relative(conn.resting[`${f.provider}/${f.model}`].until) })}</span>{/if}</li>
                  {/each}
                </ol>
              </details>
            {/if}
            {#if role === 'rerank' && conn.modelCaps[m?.provider ?? ''] && !conn.modelCaps[m.provider].rerank}<div class="xs warn-text">{t('conn.rerank_unsupported')}</div>{/if}
            {#if role === 'embed' && conn.embedShowsWidth && conn.ew}
              <div class="width-line"><span class="xs muted">{widthText(conn.ew)}</span>{#if conn.widthOptions.length}<Button size="sm" variant="ghost" icon={I.SlidersHorizontal} data-change-width="embed" disabled={!can('models.embed_width')} onclick={onwidth}>{t('conn.width_change')}</Button>{/if}</div>
              {#if !conn.ew.truncatable}<div class="xs muted">{conn.wstate === 'custom' ? t('conn.width_custom_note') : t('conn.width_unsupported')}</div>{/if}
              {#if conn.widthNoRerank}<div class="xs muted">{t('conn.width_norerank')}</div>{/if}
            {/if}</td>
          <td class="c-effort small">{#if effortRole(role) && m?.model}<div class="effort-select"><Select aria-label={t('conn.effort_row', { role: t(`role.${role}`) })} value={m.effort ?? ''} disabled={!can('config.patch')} onchange={(e) => setEffort(role, (e.currentTarget as HTMLSelectElement).value)} options={conn.effortOptions(conn.reasoningOf(m.provider, m.model), m.effort ?? '')} /></div><div class="xs muted">{t(reasonKey(conn.reasoningOf(m.provider, m.model)))}</div>{:else}<span class="subtle" title={t('conn.effort_none_role')}>–</span>{/if}</td>
          <td class="c-price small"><button type="button" class="price-edit" title={t('conn.set_price')} onclick={() => onprice(role)}><span class="num" class:muted={(prices[role]?.source ?? 'unknown') === 'unknown'}>{priceText(role)}</span><I.Pencil size={13} aria-hidden="true" /><span class="sr-only">{t('conn.set_price')}</span></button>{#if prices[role]?.cached_per_mtok}<div class="xs muted">{t('conn.price_cached_short', { usd: usd(prices[role].cached_per_mtok) })}</div>{/if}{#if prices[role]?.source && prices[role].source !== 'unknown'}<div class="xs muted">{priceSource(role)}</div>{/if}{#if priceStale(role)}<div class="xs warn-text">{t('conn.price_stale')}</div>{/if}</td>
          <td class="c-acts r nowrap"><Button size="sm" variant="ghost" icon={I.Search} data-change-model={role} onclick={() => onbrowse(role)}>{t('conn.change_model')}</Button>{#if FALLBACK_ROLES.includes(role) && m?.model}<Button size="sm" variant="ghost" icon={I.Layers} disabled={!can('config.patch')} onclick={() => onfallbacks(role)}>{m?.fallbacks?.length ? t('conn.fb_button_n', { n: m.fallbacks.length }) : t('conn.fb_button')}</Button>{/if}{#if OPTIONAL_ROLES.includes(role) && m?.model}<Button size="sm" variant="ghost" icon={I.X} disabled={!can('models.select')} onclick={() => ((clearFor = role), (clearOpen = true))}>{t('conn.clear_role')}</Button>{/if}</td>
        </tr>
      {/each}
    </tbody>
  </table></div>
  {#if ROLES.some((r) => (prices[r]?.source ?? 'unknown') === 'unknown')}<p class="xs muted pnote">{t('conn.price_src.unknown')}</p>{/if}
</Card>

<Confirm bind:open={clearOpen} title={t('conn.clear_title', { role: clearFor ? t(`role.${clearFor}`) : '' })} body={clearFor ? t(`conn.clear_body.${clearFor}`) : ''} confirmLabel={t('conn.clear_role')} onconfirm={clearRole} />

<style>
  .sub-title {
    margin: var(--s-5) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .blocked {
    margin-top: var(--s-3);
  }
  .price-edit {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin: 0 -6px;
    padding: 4px 6px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
  .price-edit :global(svg) {
    color: var(--text-subtle);
  }
  @media (hover: hover) {
    .price-edit:hover {
      background: var(--surface-2);
    }
    .price-edit:hover :global(svg) {
      color: var(--text);
    }
  }
  .price-edit:focus-visible {
    outline: 2px solid var(--accent);
  }
  .warn-text {
    color: var(--warn);
  }
  .pnote {
    padding: var(--s-3) var(--pad-card);
    margin: 0;
  }
  .fb-list {
    margin: 4px 0 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 2px;
  }
  .roles .c-role {
    width: 30%;
  }
  .roles .c-model {
    min-width: 220px;
  }
  .roles .c-effort {
    min-width: 160px;
  }
  .effort-select {
    width: 150px;
    margin-bottom: 2px;
  }
  .fb {
    margin-top: 4px;
  }
  .fb summary {
    cursor: pointer;
    width: fit-content;
  }
  .width-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--s-2);
  }
  .width-line :global(.btn) {
    margin-inline-start: -6px;
  }
  /* on a phone each role is a small card: name, then model, then price and the two buttons */
  @media (max-width: 640px) {
    .roles thead {
      display: none;
    }
    .roles tr {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 4px var(--s-3);
      padding: var(--s-3) var(--pad-card);
      box-shadow: inset 0 -1px 0 var(--border);
    }
    .roles tbody tr:last-child {
      box-shadow: none;
    }
    .roles td {
      display: block;
      height: auto;
      padding: 0;
      box-shadow: none;
    }
    .roles .c-role,
    .roles .c-model,
    .roles .c-effort {
      grid-column: 1 / -1;
    }
    .roles .c-acts {
      align-self: center;
    }
  }
</style>
