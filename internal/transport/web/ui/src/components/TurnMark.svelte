<script lang="ts">
  import { call } from '../lib/api.svelte'
  import { t } from '../lib/i18n.svelte'
  import { toast } from '../lib/toast.svelte'
  import Button from './Button.svelte'
  import * as I from '../lib/icons'
  // The operator's verdict on one reply (turns.mark): right, wrong or out of date. Stored apart from what
  // people rated, with no text and no reason; it does not change the bot. A turn whose content is gone
  // cannot be marked, and the control says why instead of going missing.
  let { turn, onmark }: { turn: any; onmark?: (id: string, mark: string) => void } = $props()
  let busy = $state('')
  const mark = $derived<string>(turn.mark ?? '')
  const blocked = $derived<string>(turn.mark_blocked ?? '')
  const verdicts = [
    { v: 'right', icon: I.Check },
    { v: 'wrong', icon: I.X },
    { v: 'stale', icon: I.Hourglass },
  ] as const
  async function set(verdict: string) {
    busy = verdict
    const r = await call('turns.mark', { id: turn.id, verdict })
    busy = ''
    if (!r.ok) {
      toast('error', t('turns.mark.failed'), r.error?.args?.reason === 'gone' ? t('turns.mark.gone') : r.error?.message)
      return
    }
    onmark?.(turn.id, verdict === 'clear' ? '' : verdict)
  }
</script>

<section class="mark" aria-label={t('turns.mark.title')}>
  <h4 class="small">{t('turns.mark.question')}</h4>
  <div class="row" role="group" aria-label={t('turns.mark.question')}>
    {#each verdicts as x (x.v)}
      <Button size="sm" variant={mark === x.v ? 'primary' : 'secondary'} icon={x.icon} aria-pressed={mark === x.v} disabled={!!blocked} loading={busy === x.v} onclick={() => set(x.v)}>{t(`turns.mark.${x.v}`)}</Button>
    {/each}
    {#if mark}<Button size="sm" variant="ghost" loading={busy === 'clear'} onclick={() => set('clear')}>{t('turns.mark.clear')}</Button>{/if}
  </div>
  {#if blocked}<p class="xs muted">{t(`turns.mark.blocked.${blocked}`)}{#if mark} · {t('turns.mark.kept')}{/if}</p>{/if}
  <p class="xs muted">{t('turns.mark.hint')}</p>
</section>

<style>
  .mark {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2);
  }
</style>
