<script lang="ts">
  // the embedding width: what is stored (cut to the leading part of the model's vector, or all of it) and the
  // widths the model can be cut to
  import { tick } from 'svelte'
  import { t } from '../../lib/i18n.svelte'
  import { call } from '../../lib/api.svelte'
  import { onLive } from '../../lib/live.svelte'
  import { navigate } from '../../lib/router.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { num } from '../../lib/format'
  import { latest } from '../../lib/seqload'
  import { embedReindexAsk, type EmbedAsk } from '../../lib/embedswitch'
  import { widthChoices, widthDirection, widthState, narrowWithoutRerank, widthBasis, sameWidthBasis, widthRead, widthApplied, widthApproval, widthRequest, widthReplyFits, type EmbedWidthInfo, type WidthChoice, type WidthApproval } from '../../lib/embedwidth'
  import type { Connections } from '../../lib/connections.svelte'
  import Dialog from '../Dialog.svelte'
  import Button from '../Button.svelte'
  import Confirm from '../Confirm.svelte'
  import * as I from '../../lib/icons'

  let { conn }: { conn: Connections } = $props()

  // a second width change within this time of the last one does not repeat the "stale" warning
  const WIDTH_STALE_TOAST_GAP_MS = 2000
  // the tags beside a width: which one is the full width, the recommended one, the one in use
  const widthTags = (c: WidthChoice) => [c.full && t('conn.width_tag_full'), c.recommended && t('conn.width_tag_recommended'), c.current && t('conn.current')].filter(Boolean).join(' · ')
  let widthOpen = $state(false)
  let widthPick = $state(-1)
  let widthBusy = $state(false)
  let widthLowerOpen = $state(false)
  let widthRaiseOpen = $state(false)
  // What the operator approved on the screen (the basis and the direction it announced) is kept with the
  // confirmation, so the include_embed request that follows goes out with the same revision and direction rather
  // than the current ones.
  let widthAsk = $state<(EmbedAsk & { value: number; approval: WidthApproval }) | null>(null)
  let widthLowerApproval: WidthApproval | null = null
  let widthLoading = $state(false)
  // The picker works on its own read of providers.list's embed_width, and on nothing else. That object is made from
  // one configuration snapshot on the server: the revision it names, the model and provider, the stored width
  // and the choices all belong together. They are never mixed with config.document or the page's cached
  // providers data, which may be older or newer; a read that fails leaves nothing to apply (null), never the
  // previous answer. The revision goes out with the request as base_revision, and a change made elsewhere
  // meanwhile (another tab, a live event) voids the answers instead of applying them to a setting the operator
  // never saw.
  let wi = $state<EmbedWidthInfo | null>(null)
  const widthBase = $derived(widthBasis(wi))
  const pickState = $derived(widthState(wi))
  const pickOptions = $derived(widthChoices(wi))
  const pickNoRerank = $derived(narrowWithoutRerank(wi))
  const widthChoice = $derived(pickOptions.find((c) => c.value === widthPick))
  const widthDir = $derived(widthChoice ? widthDirection(wi, widthChoice) : 'same')
  let widthVoidedAt = 0
  const widthReads = latest()
  const currentWidthPick = () => pickOptions.find((c) => c.current)?.value ?? -1
  // Read the width again. 'stale' means a newer read took over (this answer is dropped). A different basis closes
  // every confirmation that was asked on the old one.
  async function loadWidth(notify: boolean): Promise<'ok' | 'failed' | 'stale'> {
    const live = widthReads.start()
    widthLoading = true
    const info = widthRead(await call('providers.list', {}))
    if (!live()) return 'stale'
    widthLoading = false
    const before = widthBase
    wi = info
    if (sameWidthBasis(before, widthBase) && info) return 'ok'
    widthLowerOpen = false
    widthRaiseOpen = false
    widthAsk = null
    widthLowerApproval = null
    await tick()
    widthPick = currentWidthPick()
    if (notify && before && Date.now() - widthVoidedAt > WIDTH_STALE_TOAST_GAP_MS) toast('warn', t('conn.width_stale_title'), t('conn.width_stale_body'))
    widthVoidedAt = Date.now()
    return info ? 'ok' : 'failed'
  }
  export async function openWidth() {
    wi = null
    const st = await loadWidth(false)
    if (st === 'stale') return
    if (st === 'failed') return toast('error', t('conn.width_read_failed'), t('conn.width_read_failed_body'))
    widthVoidedAt = 0
    widthOpen = true
  }
  $effect(() => {
    if (!widthOpen) return
    return onLive(['config'], () => {
      if (!widthBusy) void loadWidth(true)
    })
  })
  // lowering only cuts what is stored (no API call), so it is confirmed with that promise; raising embeds again
  function applyWidth() {
    if (!widthChoice || widthLoading) return
    const approval = widthApproval(widthBase, widthDir)
    if (!approval) return
    if (approval.direction === 'lower') {
      widthLowerApproval = approval
      widthLowerOpen = true
    } else void sendWidth(widthChoice, approval)
  }
  async function sendWidth(choice: WidthChoice, approval: WidthApproval, includeEmbed = false) {
    widthBusy = true
    const req = widthRequest(approval, choice.value, includeEmbed)
    const r = await call('models.embed_width', req.input, { base_revision: req.baseRevision })
    widthBusy = false
    if (r.ok) {
      widthOpen = false
      void conn.providers.refresh()
      await tick()
      document.querySelector<HTMLElement>('[data-change-width]')?.focus()
      // the width was already what was asked for (changed elsewhere meanwhile): nothing was cut or embedded
      const done = widthApplied(r.output)
      if (done === 'unchanged') return toast('info', t('conn.width_unchanged', { n: choice.width }))
      const again = done === 'reembed'
      toast('ok', t('conn.width_set', { n: choice.width }), again ? t('conn.width_reindex') : t('conn.width_cut_done'), again ? { label: t('conn.rec_reindex_go'), run: () => navigate('/blocks') } : undefined)
      return
    }
    // changed elsewhere since the picker opened: read it again and ask again, as every config save does
    if (r.error?.code === 'CONFIG_CONFLICT') {
      toast('warn', t('config.conflict_title'), t('config.conflict_body'))
      void conn.providers.refresh()
      // only providers.list is read again, and the confirmation that was asked on the old basis is dropped
      widthLowerOpen = false
      widthRaiseOpen = false
      widthAsk = null
      widthLowerApproval = null
      wi = null
      widthVoidedAt = Date.now()
      await loadWidth(false)
      return
    }
    // memory is indexed: the server refuses the raise with the estimate until the request carries include_embed
    const ask = !includeEmbed ? embedReindexAsk(r.error) : null
    if (ask) {
      // the estimate was made for the width the request was sent on: when the picker has moved on meanwhile, it is
      // dropped and read again instead of being asked about on the new basis
      if (!widthReplyFits(approval, widthBase)) {
        widthAsk = null
        widthRaiseOpen = false
        toast('warn', t('conn.width_stale_title'), t('conn.width_stale_body'))
        await loadWidth(false)
        return
      }
      widthAsk = { ...ask, value: choice.value, approval }
      widthRaiseOpen = true
    } else toast('error', r.error?.message ?? t('common.failed'))
  }
  async function confirmWidthLower() {
    const choice = widthChoice
    const approval = widthLowerApproval
    widthLowerOpen = false
    widthLowerApproval = null
    await tick()
    if (choice && approval) await sendWidth(choice, approval)
  }
  const widthRaiseBody = (a: EmbedAsk | null) =>
    !a ? '' : !a.known ? t('conn.width_raise_note') : t('conn.width_raise_body', { leaves: num(a.leaves), rooms: num(a.rooms) }) + (a.tokens > 0 ? ` ${t('conn.embed_switch_tokens', { tokens: num(a.tokens) })}` : '')
  async function confirmWidthRaise() {
    const choice = pickOptions.find((c) => c.value === widthAsk?.value)
    const approval = widthAsk?.approval
    widthRaiseOpen = false
    await tick()
    if (choice && approval) await sendWidth(choice, approval, true)
  }
</script>

<Dialog bind:open={widthOpen} title={t('conn.width_title')} description={t('conn.width_desc')} size="sm">
  <div class="stack">
    <div class="widths" role="radiogroup" aria-label={t('conn.width_title')}>
      {#each pickOptions as c (c.value)}
        <label class="host" class:on={widthPick === c.value}>
          <input type="radio" name="embed-width" value={c.value} bind:group={widthPick} />
          <span><strong class="small num">{c.width}</strong>{#if widthTags(c)}<span class="xs muted wtags">· {widthTags(c)}</span>{/if}<br />
            <span class="xs muted">{c.full ? t('conn.width_opt_full') : c.recommended ? t('conn.width_opt_recommended') : t('conn.width_opt_middle')}</span></span>
        </label>
      {/each}
    </div>
    {#if !wi}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.width_read_failed_body')}</span><Button size="sm" variant="ghost" onclick={() => void loadWidth(false)}>{t('conn.width_reread')}</Button></div>{/if}
    {#if pickState === 'over'}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.width_over_note', { full: wi?.full_width ?? 0 })}</span></div>{/if}
    {#if pickNoRerank}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.width_norerank')}</span></div>{/if}
    {#if widthDir === 'raise'}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.width_raise_note')}</span></div>{/if}
  </div>
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (widthOpen = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Check} loading={widthBusy} disabled={widthDir === 'same' || !widthBase || widthLoading} onclick={applyWidth}>{t('conn.width_apply')}</Button>
  {/snippet}
</Dialog>

<Confirm bind:open={widthLowerOpen} title={t('conn.width_lower_title', { n: widthChoice?.width ?? 0 })} body={t('conn.width_lower_body')} confirmLabel={t('conn.width_lower_confirm')} onconfirm={confirmWidthLower} />

<Confirm bind:open={widthRaiseOpen} title={t('conn.width_raise_title', { n: pickOptions.find((c) => c.value === widthAsk?.value)?.width ?? 0 })} body={widthRaiseBody(widthAsk)} confirmLabel={t('conn.width_raise_confirm')} onconfirm={confirmWidthRaise} />

<style>
  .widths {
    display: grid;
    gap: var(--s-2);
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
  .host input {
    margin-top: 3px;
    accent-color: var(--accent);
  }
  .wtags {
    margin-inline-start: 0.4em;
  }
</style>
