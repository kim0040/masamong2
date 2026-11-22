<script lang="ts">
  // the summary row: the two connections the bot cannot work without, then the optional services
  import { t } from '../../lib/i18n.svelte'
  import { can } from '../../lib/api.svelte'
  import { cfg } from '../../lib/config.svelte'
  import { ROLES } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Button from '../Button.svelte'
  import Status from '../Status.svelte'
  import * as I from '../../lib/icons'

  let { conn, onadd, onkey }: { conn: Connections; onadd: () => void; onkey: (id: string) => void } = $props()

  type Tile = { id: string; icon: any; label: string; tone: any; text: string; action?: { label: string; icon: any; run: () => void } }
  const tiles = $derived.by((): Tile[] => {
    const { providers, groups, blocked, provName } = conn
    const modelProviders = groups.model.cards
    const stopped = blocked.reduce((n, b) => n + b.roles.length, 0)
    const working = ROLES.filter((r) => conn.roleState(r) === 'ok').length
    const model: Tile = !modelProviders.length
      ? { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: 'danger', text: t('conn.ov.model_none'), action: { label: t('conn.add_provider'), icon: I.Plus, run: onadd } }
      : blocked.length
        ? { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: 'danger', text: blocked[0].rejected ? t('conn.ov.model_rejected', { id: provName(blocked[0].id) }) : t('conn.ov.model_missing', { n: stopped }),
            action: can('providers.credentials') ? { label: blocked[0].rejected ? t('conn.replace_key') : t('conn.add_key'), icon: I.KeyRound, run: () => onkey(blocked[0].id) } : undefined }
        : { id: 'models', icon: I.Sparkles, label: t('conn.group.model'), tone: providers.data ? 'ok' : 'neutral', text: providers.data ? t('conn.ov.model_ok', { n: working }) : t('common.loading') }
    const discord: Tile = { id: 'discord', icon: I.Bot, label: t('conn.discord'), tone: conn.discordStatus.tone, text: conn.discordStatus.text }
    const svc = (['web', 'data'] as const).flatMap((g) => groups[g].cards).filter((p) => conn.pstate(p).tone === 'ok').length
    const wait = (['web', 'data'] as const).reduce((n, g) => n + groups[g].waiting.length, 0)
    const services: Tile = { id: 'services', icon: I.Globe, label: t('conn.ov.services'), tone: svc ? 'ok' : 'neutral', text: [t('conn.ov.services_on', { n: svc }), wait ? t('conn.ov.services_wait', { n: wait }) : ''].filter(Boolean).join(' · ') }
    // the practice instance has no search or data services: no tile for an empty section
    return conn.serviceGroups.length ? [model, discord, services] : [model, discord]
  })
  function jump(id: string) {
    const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches
    document.getElementById(id)?.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'start' })
  }
</script>

<ul class="overview" aria-label={t('conn.overview')}>
  {#each tiles as tl (tl.id)}
    {@const TIcon = tl.icon}
    <li class="tile {tl.tone}">
      <button type="button" class="tile-go" onclick={() => jump(tl.id)} title={t('conn.open_section')}>
        <span class="ti" aria-hidden="true"><TIcon size={18} /></span>
        <span class="tt"><span class="tl small">{tl.label}</span><Status tone={tl.tone} label={tl.text} size="sm" /></span>
      </button>
      {#if tl.action}<Button size="sm" variant="primary" icon={tl.action.icon} onclick={tl.action.run}>{tl.action.label}</Button>{/if}
    </li>
  {/each}
</ul>

<style>
  /* summary row: what works, what is missing, and the one button that fixes it */
  .overview {
    list-style: none;
    margin: 0 0 var(--s-6);
    padding: 0;
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr));
  }
  .tile {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2);
    padding: var(--s-2) var(--s-3) var(--s-2) var(--s-2);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .tile.danger {
    box-shadow: var(--shadow-border), inset 3px 0 0 var(--danger);
  }
  .tile.warn {
    box-shadow: var(--shadow-border), inset 3px 0 0 var(--warn);
  }
  .tile-go {
    flex: 1 1 180px;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: var(--s-3);
    padding: var(--s-2);
    border: 0;
    border-radius: var(--r-lg);
    background: none;
    color: inherit;
    font: inherit;
    text-align: start;
    cursor: pointer;
  }
  @media (hover: hover) {
    .tile-go:hover {
      background: var(--surface-2);
    }
  }
  .tile-go:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .ti {
    flex: none;
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: var(--surface-2);
    color: var(--text-muted);
  }
  .tile.ok .ti {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tile.warn .ti {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .tile.danger .ti {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .tt {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 4px;
    min-width: 0;
  }
  .tl {
    font-weight: 650;
  }
</style>
