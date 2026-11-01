<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call, can, practice } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { toast } from '../lib/toast.svelte'
  import { relative } from '../lib/format'
  import { patchConfig } from '../lib/config.svelte'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import KeyValue from './KeyValue.svelte'
  import Segmented from './Segmented.svelte'
  import Switch from './Switch.svelte'
  import * as I from '../lib/icons'

  // Discord DMs to the operators when something needs them; it sits next to the operator list.
  const alerts = resource<any>('alerts.status', () => ({}), { live: ['config', 'audit'] })
  let alertBusy = $state(false)
  let alertTesting = $state(false)
  async function setAlerts(key: 'discord' | 'min_severity', value: unknown) {
    alertBusy = true
    await patchConfig([{ path: ['alerts', key], value }])
    alertBusy = false
    void alerts.refresh()
  }
  async function testAlert() {
    alertTesting = true
    const r = await call('alerts.test')
    alertTesting = false
    if (r.ok) toast('ok', t('maint.alerts.test_ok', { n: r.output?.last?.sent ?? 0 }))
    else {
      const why = r.error?.args?.reason as string | undefined
      toast('error', t('maint.alerts.test_failed'), why === 'discord_offline' ? t('maint.alerts.discord_offline') : why && why !== 'send_failed' ? t(`maint.alerts.reason.${why}`) : r.error?.message)
    }
    void alerts.refresh()
  }
</script>

<Card title={t('maint.alerts.title')} description={t('maint.alerts.desc')}>
  {#if practice.on}
    <p class="small muted">{t('maint.practice_real_only')}</p>
  {:else if alerts.data}
    {@const al = alerts.data}
    <div class="stack">
      <Switch checked={al.discord} disabled={alertBusy || !can('config.patch') || practice.on} label={t('maint.alerts.switch')} hint={t('maint.alerts.switch_hint')} onchange={(v) => setAlerts('discord', v)} />
      {#if al.discord}
        <Segmented size="sm" label={t('maint.alerts.severity')} value={al.min_severity} onchange={(v) => setAlerts('min_severity', v)} options={[{ value: 'warn', label: t('maint.alerts.severity.warn') }, { value: 'danger', label: t('maint.alerts.severity.danger') }]} />
      {/if}
      {#if al.operators === 0}
        <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('maint.alerts.no_operators')}</span></div>
      {:else}
        <KeyValue items={[
          { k: t('maint.alerts.last'), v: al.last ? `${relative(al.last.at)} · ${t('maint.alerts.last_result', { items: al.last.items, sent: al.last.sent })}` : t('maint.alerts.last_none') },
        ]} />
        <p class="small muted">{t('maint.alerts.operators', { n: al.operators })}</p>
        {#if al.reason === 'discord_offline'}<p class="xs warn-t">{t('maint.alerts.discord_offline')}</p>{/if}
        {#if al.last?.reason}<p class="xs warn-t">{t(`maint.alerts.reason.${al.last.reason}`)}</p>{/if}
      {/if}
      <div class="row"><Button icon={I.Send} loading={alertTesting} disabled={!can('alerts.test') || practice.on || al.operators === 0} onclick={testAlert}>{t('maint.alerts.test')}</Button></div>
    </div>
  {:else if alerts.error}
    <p class="small err">{alerts.error.message}</p>
  {/if}
</Card>

<style>
  .warn-t {
    color: var(--warn);
  }
</style>
