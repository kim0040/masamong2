<script lang="ts">
  // a role's price (for hosts that do not list prices, USD per million tokens), call time limit and reasoning level
  import { t } from '../../lib/i18n.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { effortRole, timeoutRole, parseUsd, parseSeconds, validTimeout, priceOps, reasonKey } from '../../lib/connections'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Select from '../Select.svelte'
  import Button from '../Button.svelte'

  let { conn }: { conn: Connections } = $props()

  let priceFor = $state('')
  let priceIn = $state('')
  let priceOut = $state('')
  let priceCached = $state('')
  let priceErr = $state('')
  // how long a chat model may take before the other chat model answers (planner and response only)
  let timeoutSecs = $state('')
  let timeoutErr = $state('')
  // how much a reasoning model thinks on the role's calls (models.<role>.effort; '' = low); every chat
  // role takes one (the server's config.FallbackRoles), a fallback may name its own
  let effort = $state('')
  // which calls the level covers: the summary role's job is thinking-free unless set, the others low
  const effortHintKey = (role: string) => (role === 'summary' ? 'conn.effort_hint_summary' : 'conn.effort_hint')
  export function openPrice(role: string) {
    priceFor = role
    priceIn = String(cfg.doc?.models?.[role]?.price_in_per_mtok ?? '')
    priceOut = String(cfg.doc?.models?.[role]?.price_out_per_mtok ?? '')
    priceCached = String(cfg.doc?.models?.[role]?.price_cached_in_per_mtok ?? '')
    timeoutSecs = String(cfg.doc?.models?.[role]?.timeout_seconds ?? '')
    effort = cfg.doc?.models?.[role]?.effort ?? ''
    priceErr = ''
    timeoutErr = ''
  }
  async function savePrice() {
    const a = parseUsd(priceIn)
    const b = parseUsd(priceOut)
    const c = parseUsd(priceCached)
    if (a === null || b === null || c === null) return void (priceErr = t('conn.price_invalid'))
    const secs = parseSeconds(timeoutSecs)
    if (timeoutRole(priceFor) && !validTimeout(secs)) return void (timeoutErr = t('conn.timeout_invalid'))
    const r = await patchConfig(priceOps(priceFor, { in: a, out: b, cached: c, secs, effort }), { success: t('conn.price_saved') })
    if (r.ok) {
      priceFor = ''
      void conn.providers.refresh()
    }
  }
</script>

<Dialog open={!!priceFor} onclose={() => (priceFor = '')} title={t('conn.price_title', { role: priceFor ? t(`role.${priceFor}`) : '' })} description={t('conn.price_desc')} size="sm">
  <div class="stack">
    <Field id="pin" label={t('conn.price_in')}>{#snippet children({ id })}<Input {id} bind:value={priceIn} inputmode="decimal" placeholder="0.14" />{/snippet}</Field>
    <Field id="pout" label={t('conn.price_out')}>{#snippet children({ id })}<Input {id} bind:value={priceOut} inputmode="decimal" placeholder="0.42" />{/snippet}</Field>
    <Field id="pcached" label={t('conn.price_cached')} hint={t('conn.price_cached_hint')} error={priceErr || undefined}>{#snippet children({ id, describedby, invalid })}<Input {id} {invalid} aria-describedby={describedby} bind:value={priceCached} inputmode="decimal" placeholder={conn.prices[priceFor]?.cached_per_mtok ? String(conn.prices[priceFor].cached_per_mtok) : '0.014'} />{/snippet}</Field>
    <p class="xs muted">{t('conn.price_note')}</p>
    {#if timeoutRole(priceFor)}
      <Field id="ptimeout" label={t('conn.timeout')} hint={t('conn.timeout_hint')} error={timeoutErr || undefined}>{#snippet children({ id, invalid })}<Input {id} {invalid} bind:value={timeoutSecs} inputmode="numeric" placeholder={priceFor === 'planner' || priceFor === 'vision' ? '30' : '90'} />{/snippet}</Field>
    {/if}
    {#if effortRole(priceFor)}
      {@const pm = cfg.doc?.models?.[priceFor]}
      {@const pr = conn.reasoningOf(pm?.provider ?? '', pm?.model ?? '')}
      <Field id="peffort" label={t('conn.effort')} hint={`${t(effortHintKey(priceFor))} ${t(`${reasonKey(pr)}_hint`)}`}>{#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} bind:value={effort} options={conn.effortOptions(pr, effort)} />{/snippet}</Field>
    {/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (priceFor = '')}>{t('common.cancel')}</Button>
    <Button variant="primary" onclick={savePrice}>{t('common.save')}</Button>
  {/snippet}
</Dialog>
