<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { patchConfig } from '../lib/config.svelte'
  import { relative } from '../lib/format'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Switch from './Switch.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import KeyValue from './KeyValue.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Earthquakes and weather warnings the bot announces on its own (disaster_alerts): the switches,
  // what is announced, and what the last check found. A room turns them off on its own page.
  const st = resource<any>('disasters.alerts', () => ({}), { live: ['config'] })
  let regions = $state('')
  let minDom = $state('')
  let minOver = $state('')
  let busy = $state(false)
  let seen = ''
  $effect(() => {
    const d = st.data
    if (!d) return
    const k = JSON.stringify([d.warning_regions, d.earthquake_min_domestic, d.earthquake_min_overseas])
    if (k === seen) return
    seen = k
    regions = (d.warning_regions ?? []).join(', ')
    minDom = String(d.earthquake_min_domestic)
    minOver = String(d.earthquake_min_overseas)
  })
  async function set(path: string[], value: unknown) {
    busy = true
    await patchConfig([{ path: ['disaster_alerts', ...path], value }], { success: t('disaster.saved') })
    busy = false
    void st.refresh()
  }
  async function saveDetails() {
    const dom = Number(minDom)
    const over = Number(minOver)
    if (!(dom > 0 && dom <= 10 && over > 0 && over <= 10)) return
    const list = regions.split(',').map((r) => r.trim()).filter(Boolean)
    busy = true
    await patchConfig(
      [
        { path: ['disaster_alerts', 'earthquake_min_domestic'], value: dom },
        { path: ['disaster_alerts', 'earthquake_min_overseas'], value: over },
        list.length ? { path: ['disaster_alerts', 'warning_regions'], value: list } : { path: ['disaster_alerts', 'warning_regions'], op: 'unset' },
      ],
      { success: t('disaster.saved') },
    )
    busy = false
    void st.refresh()
  }
  const status = $derived.by(() => {
    const d = st.data
    if (!d) return { tone: 'neutral', label: '' }
    if (!d.enabled) return { tone: 'neutral', label: t('disaster.state.off') }
    if (d.waiting) return { tone: 'warn', label: t(`disaster.waiting.${d.waiting}`) }
    return { tone: 'ok', label: t('disaster.state.on', { n: d.channels }) }
  })
</script>

<Card title={t('disaster.title')} description={t('disaster.desc')}>
  {#if !st.data}<LoadState res={st}><Skeleton lines={5} /></LoadState>
  {:else}
    {@const d = st.data}
    <div class="stack">
      <div class="row"><Status tone={status.tone as any} label={status.label} /></div>
      <Switch checked={d.enabled} disabled={busy} onchange={(v) => set(['enabled'], v)} label={t('disaster.enabled')} hint={t('disaster.enabled_hint')} />
      <Switch checked={d.warnings} disabled={busy || !d.enabled} onchange={(v) => set(['warnings'], v)} label={t('disaster.warnings')} hint={t('disaster.warnings_hint')} />
      <Switch checked={d.marine} disabled={busy || !d.enabled || !d.warnings} onchange={(v) => set(['marine'], v)} label={t('disaster.marine')} hint={t('disaster.marine_hint')} />
      <div class="form-grid">
        <Field id="dmin" label={t('disaster.min_domestic')}>{#snippet children({ id })}<Input {id} bind:value={minDom} type="number" min="1" max="10" step="0.1" inputmode="decimal" />{/snippet}</Field>
        <Field id="omin" label={t('disaster.min_overseas')}>{#snippet children({ id })}<Input {id} bind:value={minOver} type="number" min="1" max="10" step="0.1" inputmode="decimal" />{/snippet}</Field>
      </div>
      <Field id="dreg" label={t('disaster.regions')} hint={t('disaster.regions_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={regions} placeholder={t('disaster.regions_ph')} />{/snippet}</Field>
      <div class="row"><Button size="sm" icon={I.Save} loading={busy} onclick={saveDetails}>{t('common.save')}</Button></div>
      <KeyValue items={[
        { k: t('disaster.last_check'), v: d.state?.checked_at ? relative(d.state.checked_at) : t('disaster.never') },
        { k: t('disaster.last_sent'), v: d.state?.last_sent_at ? `${relative(d.state.last_sent_at)} · ${t(`disaster.kind.${d.state.last_sent_kind}`)} · ${t('disaster.sent_channels', { n: d.state.last_sent_channels ?? 0 })}` : t('disaster.never') },
        ...(d.state?.last_error ? [{ k: t('disaster.last_error'), v: `${relative(d.state.last_error_at)} · ${d.state.last_error}` }] : []),
      ]} />
    </div>
  {/if}
</Card>
