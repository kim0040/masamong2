<script lang="ts">
  // the page: the shared lists live in one Connections object (lib/connections.svelte.ts), each part is a component
  // in components/connections/, and this file only wires them together
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { useConfig } from '../lib/config.svelte'
  import { toast } from '../lib/toast.svelte'
  import { Connections } from '../lib/connections.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Confirm from '../components/Confirm.svelte'
  import LoadState from '../components/LoadState.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LimitDialog from '../components/LimitDialog.svelte'
  import Overview from '../components/connections/Overview.svelte'
  import ProviderGroup from '../components/connections/ProviderGroup.svelte'
  import ModelRoles from '../components/connections/ModelRoles.svelte'
  import DiscordCard from '../components/connections/DiscordCard.svelte'
  import KeyDialog from '../components/connections/KeyDialog.svelte'
  import AddDialog from '../components/connections/AddDialog.svelte'
  import PriceDialog from '../components/connections/PriceDialog.svelte'
  import FallbackDialog from '../components/connections/FallbackDialog.svelte'
  import ModelPicker from '../components/connections/ModelPicker.svelte'
  import WidthDialog from '../components/connections/WidthDialog.svelte'
  import Recommend from '../components/connections/Recommend.svelte'
  import * as I from '../lib/icons'

  useConfig()
  const conn = new Connections()
  const { providers, channels, provName } = conn

  let keyDialog: KeyDialog
  let addDialog: AddDialog
  let priceDialog: PriceDialog
  let fallbackDialog: FallbackDialog
  let picker: ModelPicker
  let widthDialog: WidthDialog
  let recommend: Recommend
  let discord = $state<DiscordCard>()

  const openKey = (id: string) => keyDialog.openKey(id)
  // a saved key is not yet a working one: try it right away (the provider's cheapest free request)
  async function keySaved(id: string) {
    if (id === 'discord') void discord?.checkBot()
    else {
      const ok = await conn.test(id)
      // a new model provider no role uses yet: offer to fill the roles it can serve
      if (ok && conn.modelCaps[id] && !conn.usedByRole(id).length) void recommend.openRecommend(id)
    }
    void providers.refresh()
  }

  let removeFor = $state('')
  let removeOpen = $state(false)
  async function removeProvider() {
    const r = await call('providers.remove', { id: removeFor })
    if (r.ok) toast('ok', t('conn.removed', { id: provName(removeFor) }))
    else toast('error', t('common.failed'), r.error?.message)
    removeFor = ''
    void providers.refresh()
  }
  const removeBody = (id: string) => (conn.usedByRole(id).length ? t('conn.remove_in_use', { roles: conn.roleNames(conn.usedByRole(id)) }) : t('conn.remove_body'))

  let limitP = $state<any>(null)
  let limitOpen = $state(false)
</script>

<PageHeader title={t('conn.title')} description={t('conn.desc')}>
  {#snippet actions()}<Button variant="primary" icon={I.Plus} onclick={() => addDialog.openAdd()}>{t('conn.add_provider')}</Button>{/snippet}
</PageHeader>

<!-- until both lists are here the page shows placeholders, not "Discord: Off" or "price unknown", which
     look like real answers while a slow call is still on its way -->
{#if !providers.data || !channels.data}
  <LoadState res={[providers, channels]}>
    <div class="grid" style:--min="240px">{#each Array(3) as _}<Card><Skeleton lines={2} /></Card>{/each}</div>
    <div style:margin-top="var(--s-5)"><Card><Skeleton lines={6} /></Card></div>
  </LoadState>
{:else}
  <Overview {conn} onadd={() => addDialog.openAdd()} onkey={openKey} />

  <section class="block" id="models" aria-labelledby="models-h">
    <div class="sec-head"><h2 id="models-h" class="sec-title">{t('conn.group.model')}</h2><span class="xs muted">{t('conn.group.model_hint')}</span></div>
    <ProviderGroup {conn} g="model" list={conn.groups.model.cards} waiting={conn.groups.model.waiting} onkey={openKey} onrecommend={(id) => recommend.openRecommend(id)} onremove={(id) => ((removeFor = id), (removeOpen = true))} onlimit={(p) => ((limitP = p), (limitOpen = true))} />
    <ModelRoles {conn} onbrowse={(role) => picker.browse(role)} onprice={(role) => priceDialog.openPrice(role)} onfallbacks={(role) => fallbackDialog.openFallbacks(role)} onwidth={() => widthDialog.openWidth()} />
  </section>

  <div class="block" id="discord"><DiscordCard bind:this={discord} {conn} onkey={openKey} /></div>

  {#if conn.serviceGroups.length}
    <section class="block" id="services" aria-labelledby="services-h">
      <div class="sec-head"><h2 id="services-h" class="sec-title">{t('conn.services')}</h2></div>
      {#each conn.serviceGroups as [g, { cards: list, waiting }] (g)}
        <h3 class="group-title">{t(`conn.group.${g}`)}<span class="xs muted"> · {t(`conn.group.${g}_hint`)}</span></h3>
        <ProviderGroup {conn} {g} {list} {waiting} onkey={openKey} onrecommend={(id) => recommend.openRecommend(id)} onremove={(id) => ((removeFor = id), (removeOpen = true))} onlimit={(p) => ((limitP = p), (limitOpen = true))} />
      {/each}
    </section>
  {/if}
{/if}

<KeyDialog bind:this={keyDialog} {conn} saved={keySaved} />
<AddDialog bind:this={addDialog} {conn} needskey={openKey} />
<PriceDialog bind:this={priceDialog} {conn} />
<FallbackDialog bind:this={fallbackDialog} {conn} />
<ModelPicker bind:this={picker} {conn} />
<WidthDialog bind:this={widthDialog} {conn} />
<Recommend bind:this={recommend} {conn} />

{#if limitP}<LimitDialog bind:open={limitOpen} id={limitP.id} name={provName(limitP.id)} limit={limitP.daily_limit ?? 0} used={limitP.used_today ?? 0} model={!!conn.modelCaps[limitP.id]} onsaved={() => void providers.refresh()} />{/if}

<Confirm bind:open={removeOpen} title={t('conn.remove', { id: provName(removeFor) })} body={removeBody(removeFor)} confirmLabel={t('conn.remove_confirm')} danger onconfirm={removeProvider} />

<style>
  .sec-head {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 4px var(--s-3);
    margin-bottom: var(--s-3);
  }
  .sec-title {
    font-size: var(--text-lg);
    font-weight: 650;
  }
  .group-title {
    margin: var(--s-4) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .block {
    margin-bottom: var(--s-8);
    scroll-margin-top: calc(var(--topbar-offset, 0px) + var(--s-4));
  }
</style>
