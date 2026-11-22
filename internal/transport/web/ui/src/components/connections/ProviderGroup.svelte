<script lang="ts">
  // one group of services as cards (working ones) and one line each (optional ones still waiting for a key)
  import { t, has } from '../../lib/i18n.svelte'
  import { can } from '../../lib/api.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { num, relative, datetime } from '../../lib/format'
  import type { Group } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Button from '../Button.svelte'
  import Status from '../Status.svelte'
  import Switch from '../Switch.svelte'
  import IconButton from '../IconButton.svelte'
  import * as I from '../../lib/icons'

  let { conn, g, list, waiting, onkey, onrecommend, onremove, onlimit }: {
    conn: Connections
    g: Group
    list: any[]
    waiting: any[]
    onkey: (id: string) => void
    onrecommend: (id: string) => void
    onremove: (id: string) => void
    onlimit: (p: any) => void
  } = $props()

  const KEY_OLD_DAYS = 180
  const DAY_MS = 864e5
  const groupIcon = { model: I.Sparkles, web: I.Globe, data: I.CloudSun }
  const GIcon = $derived(groupIcon[g])
  const { provName, isPractice, keyless, hostOf, refKind, pstate, removable, usedByRole, roleNames } = $derived(conn)

  // Claude on OpenRouter only on the operator's own Anthropic key (providers.<id>.byok_only)
  const byokClaude = (id: string) => cfg.doc?.providers?.[id]?.byok_only?.anthropic === 'anthropic'
  async function setByokClaude(id: string, on: boolean) {
    const path = ['providers', id, 'byok_only', 'anthropic']
    await patchConfig([on ? { path, value: 'anthropic' } : { path, op: 'unset' }], { success: t('conn.byok_saved') })
  }
</script>

{#if list.length}<div class="providers">
  {#each list as p, i (p.id)}
    {@const ps = pstate(p)}
    {@const host = hostOf(p)}
    <article class="prov enter" style:--i={i}>
      <header><span class="pi {g}" aria-hidden="true"><GIcon size={18} /></span><div class="pn"><h3>{provName(p.id)}</h3>{#if provName(p.id) !== p.id}<span class="xs subtle">{p.id}{#if conn.modelCaps[p.id] && conn.modelCaps[p.id].api !== p.id} · {t(`conn.api.${conn.modelCaps[p.id].api}`)}{/if}</span>{/if}</div><span class="spacer"></span><Status tone={ps.tone} label={ps.label} size="sm" /></header>
      <div class="small muted">{isPractice(p) ? t('conn.practice_note') : p.builtin ? t('conn.builtin') : keyless(p) && !p.key_present ? t('conn.keyless_note') : refKind(p.credential_ref)}</div>
      {#if has(`tool.prov_note.${p.kind}`)}<div class="xs muted">{t(`tool.prov_note.${p.kind}`)}</div>{/if}
      {#if conn.modelCaps[p.id] && !conn.modelCaps[p.id].rerank}<div class="xs muted">{t('conn.no_rerank')}</div>{/if}
      {#if conn.modelCaps[p.id]?.api === 'openrouter' && !isPractice(p) && cfg.doc}<Switch checked={byokClaude(p.id)} disabled={!can('config.patch')} onchange={(v) => setByokClaude(p.id, v)} label={t('conn.byok_claude')} hint={t('conn.byok_claude_hint')} />{/if}
      {#if !p.builtin && !isPractice(p)}
        <div class="quota xs"><span class="muted num">{p.daily_limit ? t('tool.provider_quota_n', { used: num(p.used_today ?? 0), limit: num(p.daily_limit) }) : t('limit.used_n', { used: num(p.used_today ?? 0) })}</span>
          <button type="button" class="quota-edit" disabled={!can('config.patch')} onclick={() => onlimit(p)}><I.Pencil size={12} aria-hidden="true" />{p.daily_limit ? t('limit.edit') : t('limit.set')}</button></div>
      {:else if p.daily_limit}<div class="xs muted num">{t('tool.provider_quota_n', { used: num(p.used_today), limit: num(p.daily_limit) })}</div>{/if}
      {#if p.error}<div class="xs subtle">{p.error}</div>{/if}
      {#if conn.providers.data?.health?.[p.id]?.rejected}
        {@const h = conn.providers.data.health[p.id]}
        <div class="callout danger tight"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{t('conn.key_rejected', { when: relative(h.last_error_at) })}</span></div>
      {/if}
      {#if conn.providers.data?.health?.[p.id] && (conn.providers.data.health[p.id].last_ok || conn.providers.data.health[p.id].last_error_at || conn.providers.data.health[p.id].key_updated_at)}
        {@const h = conn.providers.data.health[p.id]}
        <dl class="health xs">
          {#if h.last_ok}<dt>{t('conn.last_ok')}</dt><dd title={datetime(h.last_ok, true)}>{relative(h.last_ok)}</dd>{/if}
          {#if h.last_error_at}<dt>{t('conn.last_error')}</dt><dd title={h.last_error}>{relative(h.last_error_at)}</dd>{/if}
          {#if h.key_updated_at}<dt>{t('conn.key_age')}</dt><dd title={datetime(h.key_updated_at, true)} class:old={Date.now() - Date.parse(h.key_updated_at) > KEY_OLD_DAYS * DAY_MS}>{relative(h.key_updated_at)}</dd>{/if}
        </dl>
      {/if}
      <div class="row acts">
        {#if !p.builtin && !isPractice(p)}<Button size="sm" variant={!p.configured && !keyless(p) ? 'primary' : 'secondary'} icon={I.KeyRound} disabled={!can('providers.credentials')} onclick={() => onkey(p.id)}>{p.key_present ? t('conn.replace_key') : keyless(p) ? t('conn.add_key_optional') : t('conn.add_key')}</Button>{/if}
        <Button size="sm" variant="ghost" icon={I.Activity} loading={conn.testing[p.id]} disabled={!p.configured} onclick={() => conn.test(p.id)}>{t('conn.test')}</Button>
        {#if conn.modelCaps[p.id] && p.configured}<Button size="sm" variant="ghost" icon={I.Sparkles} onclick={() => onrecommend(p.id)}>{t('conn.rec_open')}</Button>{/if}
        {#if host?.key_url && !p.key_present && !p.builtin}<a class="xs" href={host.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a>{/if}
        <span class="spacer"></span>
        {#if removable(p)}
          {@const inUse = usedByRole(p.id)}
          <IconButton icon={I.Trash2} size={16} tone="danger" disabled={inUse.length > 0} label={inUse.length ? t('conn.remove_in_use', { roles: roleNames(inUse) }) : t('conn.remove', { id: provName(p.id) })} onclick={() => onremove(p.id)} />
        {/if}
      </div>
    </article>
  {/each}
</div>{/if}
{#if waiting.length}
  <div class="waiting">
    <p class="xs muted">{list.length ? t('conn.waiting_more') : t('conn.waiting')}</p>
    <ul>
      {#each waiting as p (p.id)}
        {@const host = hostOf(p)}
        <li>
          <span class="grow"><strong class="small">{provName(p.id)}</strong>{#if has(`tool.prov_note.${p.kind}`)}<span class="xs muted"> · {t(`tool.prov_note.${p.kind}`)}</span>{/if}</span>
          {#if host?.key_url}<a class="xs" href={host.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a>{/if}
          <Button size="sm" icon={I.KeyRound} disabled={!can('providers.credentials')} onclick={() => onkey(p.id)}>{t('conn.add_key')}</Button>
          {#if removable(p)}<IconButton icon={I.Trash2} size={16} tone="danger" label={t('conn.remove', { id: provName(p.id) })} onclick={() => onremove(p.id)} />{/if}
        </li>
      {/each}
    </ul>
  </div>
{/if}

<style>
  /* providers three (or two) to a row, so six make two full rows */
  .providers {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
  }
  @media (min-width: 760px) {
    .providers {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (min-width: 1240px) {
    .providers {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
  }
  .prov {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding: var(--pad-card);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .prov header {
    display: flex;
    align-items: center;
    gap: var(--s-3);
  }
  .pi {
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: var(--info-soft);
    color: var(--info);
  }
  .pi.model {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .pn {
    min-width: 0;
  }
  .pn h3 {
    font-size: var(--text-md);
    font-weight: 650;
    overflow-wrap: anywhere;
  }
  .acts {
    margin-top: auto;
    flex-wrap: wrap;
  }
  .quota {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--s-2);
  }
  .quota-edit {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 2px 6px;
    border: 0;
    border-radius: var(--r-sm, 6px);
    background: none;
    color: var(--accent-text, var(--accent));
    font: inherit;
    cursor: pointer;
  }
  .quota-edit:hover:not(:disabled) {
    background: var(--surface-hover, var(--surface-2));
  }
  .quota-edit:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .health {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 2px 10px;
    margin: 0;
    color: var(--text-muted);
  }
  .health dd {
    margin: 0;
    color: var(--text);
  }
  .health dd.old {
    color: var(--warn);
    font-weight: 600;
  }
  .tight {
    padding: 6px 10px;
  }
  .waiting {
    margin-top: var(--s-3);
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .waiting ul {
    list-style: none;
    margin: var(--s-2) 0 0;
    padding: 0;
    display: grid;
    gap: var(--s-2);
  }
  .waiting li {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
    padding-top: var(--s-2);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .waiting li:first-child {
    box-shadow: none;
    padding-top: 0;
  }
  .grow {
    flex: 1 1 240px;
    min-width: 0;
  }
</style>
