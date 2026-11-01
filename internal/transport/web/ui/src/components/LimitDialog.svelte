<script lang="ts">
  // The daily call limit of one provider (providers.<id>.daily_limit): one key's calls per day in the
  // instance's time zone. Model providers count chat, embedding, rerank and image calls; when the limit
  // is reached their role's fallback answers.
  import { t } from '../lib/i18n.svelte'
  import { patchConfig } from '../lib/config.svelte'
  import { num } from '../lib/format'
  import Dialog from './Dialog.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Button from './Button.svelte'

  let { open = $bindable(false), id, name, limit = 0, used = 0, model = false, onsaved }: { open?: boolean; id: string; name: string; limit?: number; used?: number; model?: boolean; onsaved?: () => void } = $props()
  let value = $state('')
  let err = $state('')
  let busy = $state(false)
  $effect(() => {
    if (open) {
      value = limit ? String(limit) : ''
      err = ''
    }
  })
  // common steps for a quick pick; the field takes any whole number
  const presets = [100, 1000, 10000]
  async function save() {
    const n = value.trim() === '' ? 0 : Number(value)
    if (!Number.isInteger(n) || n < 0 || n > 10_000_000) return void (err = t('limit.invalid'))
    busy = true
    const path = ['providers', id, 'daily_limit']
    const r = await patchConfig([n ? { path, value: n } : { path, op: 'unset' }], { success: n ? t('limit.saved', { name, n: num(n) }) : t('limit.cleared', { name }) })
    busy = false
    if (r.ok) {
      open = false
      onsaved?.()
    } else err = r.error?.message ?? t('common.failed')
  }
</script>

<Dialog bind:open title={t('limit.title', { name })} description={t(model ? 'limit.desc_model' : 'limit.desc')} size="sm">
  <div class="stack">
    <p class="small num">{limit ? t('limit.now', { used: num(used), limit: num(limit) }) : t('limit.now_none', { used: num(used) })}</p>
    <Field id="limit-n" label={t('limit.field')} hint={t('limit.hint')} error={err || undefined}>{#snippet children({ id: fid, describedby, invalid })}<Input id={fid} aria-describedby={describedby} {invalid} bind:value inputmode="numeric" placeholder={t('limit.none')} />{/snippet}</Field>
    <div class="row presets">
      {#each presets as p (p)}<Button size="sm" variant="ghost" onclick={() => (value = String(p))}>{num(p)}</Button>{/each}
      <Button size="sm" variant="ghost" onclick={() => (value = '')}>{t('limit.none')}</Button>
    </div>
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (open = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" loading={busy} onclick={save}>{t('common.save')}</Button>
  {/snippet}
</Dialog>

<style>
  .presets {
    flex-wrap: wrap;
    gap: 4px;
  }
</style>
