<script lang="ts">
  // adding a provider: a listed model host, a web service, or any OpenAI-compatible address
  import { t, has } from '../../lib/i18n.svelte'
  import { call } from '../../lib/api.svelte'
  import { toast } from '../../lib/toast.svelte'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Segmented from '../Segmented.svelte'
  import Button from '../Button.svelte'
  import * as I from '../../lib/icons'

  /** `needskey` runs when the new service needs a key (a keyless one only says it was added). */
  let { conn, needskey }: { conn: Connections; needskey: (id: string) => void } = $props()

  let addOpen = $state(false)
  let addSel = $state('')
  let addId = $state('')
  let addURL = $state('')
  // the request format of a custom address: OpenAI chat completions, or Anthropic's Messages API
  let addAPI = $state<'compatible' | 'anthropic'>('compatible')
  let addErr = $state('')
  let addBusy = $state(false)
  const present = (sel: string) => {
    const [kind, v] = sel.split(':')
    // a built-in (keyless Jina) does not count: adding it for real lets it take a key
    return (conn.providers.data?.providers ?? []).some((p: any) => !p.builtin && (kind === 'host' ? p.id === v : p.kind === v))
  }
  export function openAdd() {
    addSel = ''
    addId = ''
    addURL = ''
    addAPI = 'compatible'
    addErr = ''
    addOpen = true
  }
  async function addProvider() {
    addErr = ''
    const [kind, v] = addSel.split(':')
    let input: Record<string, unknown>
    if (kind === 'host') input = { host: v }
    else if (kind === 'kind') input = { kind: v }
    else {
      if (!addId.trim() || !/^https:\/\//.test(addURL.trim())) return void (addErr = t('conn.add_custom_invalid'))
      input = { api: addAPI, id: addId.trim(), base_url: addURL.trim() }
    }
    addBusy = true
    const r = await call('providers.add', input)
    addBusy = false
    if (!r.ok) return void (addErr = r.error?.message ?? t('common.failed'))
    addOpen = false
    await conn.providers.refresh()
    const id = String(r.output.id)
    const host = (conn.providers.data?.web_hosts ?? []).find((h: any) => h.kind === v)
    if (host?.keyless) toast('ok', t('conn.added', { id: conn.provName(id) }))
    else needskey(id)
  }
</script>

<Dialog bind:open={addOpen} title={t('conn.add_provider')} description={t('conn.add_desc')} size="lg">
  <div class="stack">
    {#each [['model', conn.providers.data?.hosts ?? []], ['web', conn.providers.data?.web_hosts ?? []]] as [g, hosts] (g)}
      <div class="section-title">{t(`conn.group.${g}`)}</div>
      <div class="hosts">
        {#each hosts as h ((h.api ?? h.kind) + h.id)}
          {@const sel = g === 'model' ? `host:${h.id}` : `kind:${h.kind}`}
          <label class="host" class:on={addSel === sel} class:dim={present(sel)}>
            <input type="radio" name="host" value={sel} bind:group={addSel} disabled={present(sel)} />
            <span><strong class="small">{h.name}</strong>{#if present(sel)}<span class="xs muted"> · {t('conn.added_already')}</span>{/if}<br /><span class="xs muted">{g === 'model' ? t(has(`conn.host_note.${h.id}`) ? `conn.host_note.${h.id}` : `conn.host_note.${h.api}`) : t(`tool.prov_note.${h.kind}`)}</span></span>
          </label>
        {/each}
        {#if g === 'model'}
          <label class="host" class:on={addSel === 'custom'}>
            <input type="radio" name="host" value="custom" bind:group={addSel} />
            <span><strong class="small">{t('conn.custom')}</strong><br /><span class="xs muted">{t('conn.host_note.compatible')}</span></span>
          </label>
        {/if}
      </div>
    {/each}
    {#if addSel === 'custom'}
      <Segmented label={t('conn.custom_api')} bind:value={addAPI} options={[{ value: 'compatible', label: t('conn.api.compatible') }, { value: 'anthropic', label: t('conn.api.anthropic') }]} />
      <p class="xs muted">{t(`conn.custom_api_hint.${addAPI}`)}</p>
      <div class="form-grid">
        <Field id="aid" label={t('conn.custom_id')} hint={t('conn.custom_id_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={addId} placeholder="deepseek" mono />{/snippet}</Field>
        <Field id="aurl" label={t('conn.custom_url')} hint={t(addAPI === 'anthropic' ? 'conn.custom_url_hint_anthropic' : 'conn.custom_url_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={addURL} placeholder={addAPI === 'anthropic' ? 'https://api.example.com/anthropic' : 'https://api.deepseek.com/v1'} mono type="url" />{/snippet}</Field>
      </div>
    {/if}
    {#if addErr}<p class="small err" role="alert">{addErr}</p>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (addOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Plus} loading={addBusy} disabled={!addSel} onclick={addProvider}>{t('conn.add')}</Button>
  {/snippet}
</Dialog>

<style>
  .hosts {
    display: grid;
    gap: var(--s-2);
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 240px), 1fr));
  }
  .host {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    padding: 10px 12px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    cursor: pointer;
  }
  .host.on {
    box-shadow: 0 0 0 2px var(--accent);
  }
  .host.dim {
    opacity: 0.6;
    cursor: default;
  }
  .host input {
    margin-top: 3px;
    accent-color: var(--accent);
  }
  .err {
    color: var(--danger);
  }
</style>
