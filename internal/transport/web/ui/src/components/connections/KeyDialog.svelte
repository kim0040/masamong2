<script lang="ts">
  // a key (or reference to one) for a service, or the Discord token: typed here, stored by the server
  import { t } from '../../lib/i18n.svelte'
  import { tp, platform } from '../../lib/platform'
  import { call } from '../../lib/api.svelte'
  import { patchConfig } from '../../lib/config.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { saveSecret, validRef } from '../../lib/secret'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Segmented from '../Segmented.svelte'
  import Button from '../Button.svelte'
  import * as I from '../../lib/icons'

  /** `saved` runs after the key is stored: what to do with a key that is not yet known to work (test it, read the bot). */
  let { conn, saved }: { conn: Connections; saved: (id: string) => Promise<void> } = $props()

  let keyOpen = $state(false)
  let keyFor = $state('')
  let keyMode = $state<'value' | 'ref'>('value')
  let keyValue = $state('')
  let keyRef = $state('env:')
  let keyErr = $state('')
  let keyBusy = $state(false)
  export function openKey(id: string) {
    keyFor = id
    keyMode = 'value'
    keyValue = ''
    keyRef = conn.providers.data?.providers?.find((p: any) => p.id === id)?.credential_ref ?? 'env:'
    keyErr = ''
    keyOpen = true
  }
  async function saveKey() {
    keyErr = ''
    keyBusy = true
    let r: { ok: boolean; message: string; storedIn: string }
    try {
      if (keyMode === 'value') {
        if (!keyValue.trim()) return (keyErr = t('conn.key_required'))
        r = await saveSecret(keyFor === 'discord' ? { target: 'discord' } : { target: 'provider', id: keyFor }, keyValue)
      } else {
        if (!validRef(keyRef, !!conn.providers.data?.keychain)) return (keyErr = t('toolnew.err.cred_ref'))
        const res = keyFor === 'discord' ? await patchConfig([{ path: ['discord', 'token_ref'], value: keyRef }], { quiet: true }) : await call('providers.credentials', { id: keyFor, credential_ref: keyRef })
        r = { ok: !!res?.ok, message: res?.error?.message ?? '', storedIn: res?.output?.stored_in ?? '' }
      }
    } finally {
      keyBusy = false
    }
    // what was typed stays in the field when saving failed, so it can be sent again
    if (!r.ok) return (keyErr = r.message || t('common.failed'))
    const id = keyFor
    keyValue = ''
    keyOpen = false
    const where = r.storedIn ? tp(`conn.stored_in.${r.storedIn}`) : undefined
    toast('ok', t('conn.key_saved', { id: conn.provName(id) }), id === 'discord' ? t('setup.discord.restart_hint') : where, id === 'discord' ? { label: t('maint.restart_now'), run: () => void call('runtime.restart') } : undefined)
    await saved(id)
  }
  // where the provider in the key dialog hands out keys
  const keyHost = $derived(keyFor && keyFor !== 'discord' ? conn.hostOf((conn.providers.data?.providers ?? []).find((p: any) => p.id === keyFor) ?? {}) : null)
</script>

<Dialog bind:open={keyOpen} title={keyFor === 'discord' ? t('conn.set_token') : t('conn.key_for', { id: conn.provName(keyFor) })} description={conn.providers.data?.keychain ? t('conn.key_desc_keychain') : tp('conn.key_desc')} size="sm">
  <div class="stack">
    {#if keyHost?.key_url}<p class="small">{t('conn.key_where', { name: keyHost.name })} <a href={keyHost.key_url} target="_blank" rel="noopener">{t('conn.get_key')}</a></p>{/if}
    <Segmented label={t('conn.key_mode')} bind:value={keyMode} options={[{ value: 'value', label: t('conn.key_mode.value') }, { value: 'ref', label: t('conn.key_mode.ref') }]} />
    {#if keyMode === 'value'}
      <Field id="kv" label={t('conn.key')} hint={t('conn.key_hint')} error={keyErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} type="password" aria-describedby={describedby} {invalid} bind:value={keyValue} autocomplete="off" />{/snippet}</Field>
    {:else}
      <Field id="kr" label={t('tool.credential_ref')} hint={platform() === 'windows' ? `${t('toolnew.cred_hint')} ${t('conn.env_note.windows')}` : t('toolnew.cred_hint')} error={keyErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={keyRef} mono />{/snippet}</Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (keyOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={keyBusy} icon={I.KeyRound} onclick={saveKey}>{t('conn.save_key')}</Button>
  {/snippet}
</Dialog>
