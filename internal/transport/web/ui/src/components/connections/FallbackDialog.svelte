<script lang="ts">
  // fallbacks (models.<role>.fallbacks): asked in order when the role's model fails; the server's
  // config.FallbackRoles and MaxFallbacks
  import { t } from '../../lib/i18n.svelte'
  import { call } from '../../lib/api.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { MAX_FALLBACKS, buildFallbacks, fallbackOps, fallbackRows, reasonKey, type FB } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Select from '../Select.svelte'
  import Button from '../Button.svelte'
  import IconButton from '../IconButton.svelte'
  import * as I from '../../lib/icons'

  let { conn }: { conn: Connections } = $props()

  let fbFor = $state('')
  let fbRows = $state<FB[]>([])
  let fbErr = $state('')
  let fbBusy = $state(false)
  let fbModels = $state<Record<string, any[]>>({})
  // a fallback's reasoning: what its provider's list says, else what the server knows of a saved one
  const fbReasoning = (r: FB) => {
    const m = (fbModels[r.provider] ?? []).find((x) => x.id === r.model.trim())
    return m && (m.reasoning !== undefined || m.efforts) ? m : conn.reasoningOf(r.provider, r.model.trim())
  }
  export function openFallbacks(role: string) {
    fbFor = role
    fbErr = ''
    fbRows = fallbackRows(cfg.doc?.models?.[role]?.fallbacks)
    for (const r of fbRows) void loadFbModels(r.provider)
  }
  function addFallback() {
    const used = new Set([cfg.doc?.models?.[fbFor]?.provider, ...fbRows.map((r) => r.provider)])
    const provider = conn.modelProviderIds.find((id) => !used.has(id)) ?? conn.modelProviderIds[0] ?? ''
    fbRows = [...fbRows, { provider, model: cfg.doc?.models?.[fbFor]?.model ?? '', price_in: '', price_out: '', timeout: '', effort: '' }]
    void loadFbModels(provider)
  }
  function moveFallback(i: number, d: number) {
    const rows = [...fbRows]
    ;[rows[i], rows[i + d]] = [rows[i + d], rows[i]]
    fbRows = rows
  }
  async function loadFbModels(provider: string) {
    if (!provider || fbModels[provider]) return
    const r = await call('models.list', { provider, role: '' })
    if (r.ok) fbModels = { ...fbModels, [provider]: ((r.output.models ?? []) as any[]).filter((m) => m.id) }
  }
  async function saveFallbacks() {
    const built = buildFallbacks(fbRows, cfg.doc?.models?.[fbFor])
    if (!built.ok) return void (fbErr = t(built.problem.key, 'model' in built.problem ? { model: built.problem.model } : undefined))
    fbBusy = true
    const res = await patchConfig(fallbackOps(fbFor, built.list), { success: t('conn.fb_saved') })
    fbBusy = false
    if (res.ok) {
      fbFor = ''
      void conn.providers.refresh()
    } else fbErr = res.error?.message ?? t('common.failed')
  }
</script>

<Dialog open={!!fbFor} onclose={() => (fbFor = '')} title={t('conn.fb_title', { role: fbFor ? t(`role.${fbFor}`) : '' })} description={t('conn.fb_desc')} size="md">
  <div class="stack">
    <p class="small">{t('conn.fb_primary')} <span class="mono">{cfg.doc?.models?.[fbFor]?.model ?? '–'}</span> <span class="muted">· {conn.provName(cfg.doc?.models?.[fbFor]?.provider ?? '')}</span></p>
    {#if !fbRows.length}<p class="small muted">{t('conn.fb_none')}</p>{/if}
    <ol class="fb-rows">
      {#each fbRows as r, i (i)}
        <li class="fb-row">
          <div class="fb-head"><strong class="small">{t('conn.fb_line', { n: i + 1 })}</strong><span class="spacer"></span>
            <IconButton icon={I.ArrowUp} size={16} label={t('conn.fb_up')} disabled={i === 0} onclick={() => moveFallback(i, -1)} />
            <IconButton icon={I.ArrowDown} size={16} label={t('conn.fb_down')} disabled={i === fbRows.length - 1} onclick={() => moveFallback(i, 1)} />
            <IconButton icon={I.Trash2} size={16} tone="danger" label={t('conn.fb_remove')} onclick={() => (fbRows = fbRows.filter((_, j) => j !== i))} />
          </div>
          <div class="fb-grid">
            <Field id={`fbp${i}`} label={t('conn.fb_provider')}>{#snippet children({ id })}<Select {id} bind:value={r.provider} onchange={() => loadFbModels(r.provider)} options={conn.modelProviderIds.map((p) => ({ value: p, label: conn.provName(p) }))} />{/snippet}</Field>
            <Field id={`fbm${i}`} label={t('conn.fb_model')} hint={t('conn.fb_model_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={r.model} mono list={`fbl${i}`} autocomplete="off" />{/snippet}</Field>
            <datalist id={`fbl${i}`}>{#each fbModels[r.provider] ?? [] as fm (fm.id)}<option value={fm.id}></option>{/each}</datalist>
            <Field id={`fbi${i}`} label={t('conn.price_in')}>{#snippet children({ id })}<Input {id} bind:value={r.price_in} inputmode="decimal" placeholder={t('conn.fb_price_auto')} />{/snippet}</Field>
            <Field id={`fbo${i}`} label={t('conn.price_out')}>{#snippet children({ id })}<Input {id} bind:value={r.price_out} inputmode="decimal" placeholder={t('conn.fb_price_auto')} />{/snippet}</Field>
            <Field id={`fbt${i}`} label={t('conn.timeout')}>{#snippet children({ id })}<Input {id} bind:value={r.timeout} inputmode="numeric" placeholder={fbFor === 'response' ? '90' : fbFor === 'summary' ? '60' : '30'} />{/snippet}</Field>
            <Field id={`fbe${i}`} label={t('conn.effort')} hint={t(reasonKey(fbReasoning(r)))}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={r.effort} options={[{ value: '', label: t('conn.fb_effort_role') }, ...conn.effortOptions(fbReasoning(r), r.effort).slice(1)]} />{/snippet}</Field>
          </div>
        </li>
      {/each}
    </ol>
    {#if fbRows.length < MAX_FALLBACKS}<div><Button size="sm" variant="secondary" icon={I.Plus} disabled={!conn.modelProviderIds.length} onclick={addFallback}>{t('conn.fb_add')}</Button></div>{:else}<p class="xs muted">{t('conn.fb_max', { n: MAX_FALLBACKS })}</p>{/if}
    <p class="xs muted">{t('conn.fb_price_note')}</p>
    {#if fbErr}<div class="callout danger tight" role="alert"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{fbErr}</span></div>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (fbFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={fbBusy} onclick={saveFallbacks}>{t('common.save')}</Button>
  {/snippet}
</Dialog>

<style>
  .fb-rows {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: var(--s-3);
  }
  .fb-row {
    padding: var(--s-3);
    border-radius: var(--r);
    box-shadow: var(--shadow-border);
  }
  .fb-head {
    display: flex;
    align-items: center;
    gap: 2px;
    margin-bottom: var(--s-2);
  }
  .fb-grid {
    display: grid;
    gap: var(--s-2) var(--s-3);
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }
  @media (max-width: 640px) {
    .fb-grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .tight {
    padding: 6px 10px;
  }
</style>
