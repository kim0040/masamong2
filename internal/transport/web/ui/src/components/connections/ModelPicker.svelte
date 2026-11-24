<script lang="ts">
  // picking a role's model from its provider's list (or typing one, for a host that lists none)
  import { tick } from 'svelte'
  import { t } from '../../lib/i18n.svelte'
  import { scrollable } from '../../lib/scrollable'
  import { call } from '../../lib/api.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { navigate } from '../../lib/router.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { num, usd } from '../../lib/format'
  import { embedReindexAsk, type EmbedAsk } from '../../lib/embedswitch'
  import { chatRole, filterModels, effortRole, effortOps, effortFits, reasonKey, type ModelSort } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Select from '../Select.svelte'
  import Button from '../Button.svelte'
  import Confirm from '../Confirm.svelte'
  import CapTags from './CapTags.svelte'
  import * as I from '../../lib/icons'

  let { conn }: { conn: Connections } = $props()

  let modelsFor = $state('')
  let modelProv = $state('')
  let modelList = $state<any[]>([])
  let modelFilter = $state('')
  let modelSort = $state<ModelSort>('list')
  let loadingModels = $state(false)
  // the role's provider when it is a model provider, else the first model provider there is
  const roleProvider = (role: string) => {
    const cur = cfg.doc?.models?.[role]?.provider
    return cur && conn.modelCaps[cur] ? cur : Object.keys(conn.modelCaps)[0] ?? ''
  }
  export async function browse(role: string, provider = roleProvider(role)) {
    modelsFor = role
    modelProv = provider
    modelFilter = ''
    modelSort = 'list'
    modelList = []
    noList = false
    typedModel = ''
    if (!provider) return
    loadingModels = true
    const r = await call('models.list', { provider, role: role === 'embed' ? 'embedding' : role === 'image' ? 'image' : '' })
    loadingModels = false
    modelList = r.ok ? r.output.models ?? [] : []
    noList = r.ok && r.output?.status === 'no_list'
    if (!r.ok) toast('error', t('conn.models_failed'), r.error?.message)
  }
  // an Anthropic-format host that answers /messages only has no list: the model ID is typed
  let noList = $state(false)
  let typedModel = $state('')
  // an indexed embedding model is only switched after the operator confirms the re-index it implies:
  // the server refuses (with the estimate) until the request carries include_embed
  let embedAsk = $state<(EmbedAsk & { provider: string; model: string }) | null>(null)
  let embedOpen = $state(false)
  // after a chat-style role gets its model, the same dialog asks for the reasoning level of that model
  // (the level the role had stays until it is changed here: a new model may not take it)
  let done = $state<{ role: string; provider: string; model: string; effort: string; reasoning: { reasoning?: boolean; efforts?: string[] } } | null>(null)
  let effortPick = $state('')
  async function saveEffort() {
    if (!done) return
    const r = await patchConfig(effortOps(done.role, effortPick), { success: t('conn.effort_saved') })
    if (r.ok) closePicker()
  }
  async function closePicker() {
    const role = modelsFor
    modelsFor = ''
    done = null
    await tick()
    document.querySelector<HTMLElement>(`[data-change-model="${role}"]`)?.focus()
  }
  async function pickModel(role: string, model: string, includeEmbed = false) {
    const provider = modelProv
    const before = cfg.doc?.models?.[role]?.effort ?? ''
    const listed = modelList.find((m) => m.id === model)
    const r = await call('models.select', includeEmbed ? { role, provider, model, include_embed: true } : { role, provider, model })
    if (r.ok) {
      toast('ok', t('conn.model_set', { role: t(`role.${role}`), model }), includeEmbed ? t('conn.rec_reindex') : undefined, includeEmbed ? { label: t('conn.rec_reindex_go'), run: () => navigate('/blocks') } : undefined)
      if (effortRole(role)) {
        effortPick = before
        done = { role, provider, model, effort: before, reasoning: listed && (listed.reasoning !== undefined || listed.efforts) ? { reasoning: listed.reasoning, efforts: listed.efforts } : conn.reasoningOf(provider, model) }
        return
      }
      // the picker (and a confirm that was on top of it) are gone: keyboard focus goes back to the role's button
      await closePicker()
      return
    }
    const ask = role === 'embed' && !includeEmbed ? embedReindexAsk(r.error) : null
    if (ask) {
      embedAsk = { ...ask, provider, model }
      embedOpen = true
    } else toast('error', r.error?.message ?? '')
  }
  // the estimate the refusal carried; without it the generic note, never "0 pieces in 0 rooms"
  const embedBody = (a: EmbedAsk | null) =>
    !a ? '' : !a.known ? t('conn.embed_change_note') : t('conn.embed_switch_body', { leaves: num(a.leaves), rooms: num(a.rooms) }) + (a.tokens > 0 ? ` ${t('conn.embed_switch_tokens', { tokens: num(a.tokens) })}` : '')
  async function confirmEmbedSwitch() {
    if (!embedAsk) return
    const { provider, model } = embedAsk
    modelProv = provider
    // close the confirm first: closing the picker under an open modal would leave focus on <body>
    embedOpen = false
    await tick()
    await pickModel('embed', model, true)
  }
  // a list that says nothing about picture input (name-based lists) cannot narrow the vision role
  const listKnowsImages = $derived(modelList.some((m) => m.image_input !== undefined))
  const imageUnknown = $derived(modelsFor === 'vision' && modelList.length > 0 && !listKnowsImages)
  const filteredModels = $derived(filterModels(modelList, modelsFor, modelFilter, modelSort))
  const perM = (v: number | undefined) => (typeof v === 'number' ? usd(v) : t('conn.price_none'))
  const isCurrent = (m: any) => cfg.doc?.models?.[modelsFor]?.model === m.id && cfg.doc?.models?.[modelsFor]?.provider === modelProv
</script>

{#snippet stepFooter()}
  {#if done}
    <Button variant="ghost" onclick={closePicker}>{effortPick === done.effort ? t('common.done') : t('conn.effort_keep')}</Button>
    <Button variant="primary" disabled={effortPick === done.effort} onclick={saveEffort}>{t('conn.effort_save')}</Button>
  {/if}
{/snippet}

<Dialog open={!!modelsFor} footer={done ? stepFooter : undefined} onclose={() => ((modelsFor = ''), (done = null))} title={t('conn.pick_model', { role: t(`role.${modelsFor}`) })} description={done ? t('conn.effort_step_desc') : t('conn.pick_model_desc')} size="lg">
  {#if done}
    {@const step = done}
    <div class="stack">
      <div class="callout"><span class="ic"><I.Check size={16} /></span><span class="small">{t('conn.model_set', { role: t(`role.${step.role}`), model: step.model })}</span></div>
      <Field id="mneffort" label={t('conn.effort')} hint={`${t(step.role === 'summary' ? 'conn.effort_hint_summary' : 'conn.effort_hint')} ${t(`${reasonKey(step.reasoning)}_hint`)}`}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={effortPick} options={conn.effortOptions(step.reasoning, effortPick)} />{/snippet}</Field>
      {#if !effortFits(step.reasoning, step.effort)}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.effort_unfit', { level: t(`conn.effort.${step.effort}`) })}</span></div>{/if}
    </div>
  {:else}
  <div class="stack">
    {#if Object.keys(conn.modelCaps).length > 1}
      <Field id="mprov" label={t('conn.model_provider')} hint={t('conn.model_provider_hint')}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={modelProv} onchange={(e) => browse(modelsFor, (e.currentTarget as HTMLSelectElement).value)} options={Object.keys(conn.modelCaps).map((pid) => ({ value: pid, label: conn.provName(pid) }))} />{/snippet}</Field>
    {:else if modelProv}<p class="small muted">{t('conn.model_provider_one', { name: conn.provName(modelProv) })}</p>{/if}
    {#if imageUnknown}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.vision_unknown_note')}</span></div>{/if}
    {#if chatRole(modelsFor) && modelList.some((m) => m.supports_tools === false)}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.no_tools_note')}</span></div>{/if}
    {#if modelsFor === 'embed'}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.embed_change_note')}</span></div>{/if}
    {#if modelsFor === 'rerank' && conn.modelCaps[modelProv] && !conn.modelCaps[modelProv].rerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.rerank_unsupported')}</span></div>{/if}
    <div class="browse-bar">
      <div class="grow"><Input type="search" bind:value={modelFilter} placeholder={t('conn.model_search')} aria-label={t('conn.model_search')} /></div>
      <Select aria-label={t('conn.sort')} bind:value={modelSort} options={[{ value: 'list', label: t('conn.sort_list') }, { value: 'price_asc', label: t('conn.sort_price_asc') }, { value: 'price_desc', label: t('conn.sort_price_desc') }, { value: 'ctx', label: t('conn.sort_ctx') }]} />
    </div>
    {#if noList}
      <div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.no_list_note')}</span></div>
      <div class="browse-bar">
        <div class="grow"><Field id="typedm" label={t('conn.typed_model')} hint={t('conn.fb_model_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={typedModel} mono autocomplete="off" />{/snippet}</Field></div>
        <Button variant="primary" disabled={!typedModel.trim()} onclick={() => pickModel(modelsFor, typedModel.trim())}>{t('conn.use')}</Button>
      </div>
    {:else if loadingModels}<p class="small muted">{t('common.loading')}</p>{:else if !filteredModels.length}<p class="small muted">{t('conn.no_models')}</p>{:else}
      <div use:scrollable class="table-scroll models">
        <table class="table">
          <thead><tr><th>{t('conn.model')}</th><th class="r">{t('conn.ctx')}</th><th class="r">{t('conn.col_in')}</th><th class="r">{t('conn.col_out')}</th><th class="r">{t('conn.col_cached')}</th><th><span class="sr-only">{t('common.actions')}</span></th></tr></thead>
          <tbody>
            {#each filteredModels as m (m.id)}
              <tr><td><div class="small">{m.name}</div><div class="xs mono subtle">{m.id}</div><CapTags {m} chat={chatRole(modelsFor)} vision={modelsFor === 'vision'} /></td>
                <td class="r num xs">{m.context_length ? num(m.context_length) : '–'}</td>
                <td class="r num xs" class:muted={m.in_per_mtok === undefined}>{perM(m.in_per_mtok)}</td>
                <td class="r num xs">{m.in_per_mtok === undefined ? '–' : perM(m.out_per_mtok)}</td>
                <td class="r num xs">{m.cached_per_mtok === undefined ? '–' : perM(m.cached_per_mtok)}</td>
                <td class="r"><Button size="sm" variant={isCurrent(m) ? 'secondary' : 'ghost'} disabled={isCurrent(m)} onclick={() => pickModel(modelsFor, m.id)}>{isCurrent(m) ? t('conn.current') : t('conn.use')}</Button></td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
  {/if}
</Dialog>

<Confirm bind:open={embedOpen} title={t('conn.embed_switch_title')} body={embedBody(embedAsk)} confirmLabel={t('conn.embed_switch_confirm')} onconfirm={confirmEmbedSwitch} />

<style>
  .grow {
    flex: 1 1 240px;
    min-width: 0;
  }
  .models {
    max-height: 56vh;
  }
  .browse-bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2);
    align-items: center;
  }
  .browse-bar > :global(.wrap) {
    flex: 0 1 200px;
  }
</style>
