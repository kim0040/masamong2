<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { num, relative } from '../lib/format'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import KeyValue from './KeyValue.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Morning briefings at a glance; people and their data are managed on the People page.
  const fp = resource<any>('fortune.subscribe', () => ({ op: 'status' }), { live: ['config', 'audit'] })
  const list = $derived((fp.data?.profiles ?? []) as any[])
  const active = $derived(list.filter((p) => p.active && p.time))
  const failed = $derived(list.filter((p) => p.job_state === 'failed'))
  const lastSent = $derived(list.map((p) => p.last_sent).filter(Boolean).sort().at(-1))
</script>

<Card title={t('fortune_ops.title')} description={t('fortune_ops.desc')}>
  {#snippet actions()}<Button size="sm" variant="ghost" href="/people" iconEnd={I.ArrowRight}>{t('nav.people')}</Button>{/snippet}
  {#if !fp.data}<LoadState res={fp}><Skeleton lines={4} /></LoadState>{:else}
    <KeyValue items={[
      { k: t('fortune_ops.registered'), v: num(list.length) },
      { k: t('fortune_ops.subscribed'), v: num(active.length) },
      { k: t('fortune_ops.no_consent'), v: num(list.filter((p) => !p.consent).length) },
      { k: t('fortune_ops.last_sent'), v: lastSent ? lastSent : '–' },
    ]} />
    {#if failed.length}
      <div class="callout warn" style:margin-top="12px"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('fortune_ops.failed', { n: failed.length })}</span></div>
      <ul class="fails">{#each failed as p (p.user_id)}<li class="xs"><code>{p.user_id}</code> <span class="muted break">{p.job_error}</span> <span class="subtle">{relative(p.updated_at)}</span></li>{/each}</ul>
    {/if}
  {/if}
</Card>

<style>
  .fails {
    list-style: none;
    margin: 8px 0 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
</style>
