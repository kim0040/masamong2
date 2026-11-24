<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { relative, num } from '../lib/format'
  import Card from './Card.svelte'
  import Select from './Select.svelte'
  import Button from './Button.svelte'
  import EmptyState from './EmptyState.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // The people the bot has seen, per server, each with a button that opens that person's memory (the person
  // dialog of the People page, at its memory). The same view every other path to a person reaches.
  const servers = resource<any>('people.servers', () => ({}), { live: ['config', 'audit', 'memory', 'discord'] })
  const groups = $derived((servers.data?.servers ?? []) as any[])
  let pick = $state('')
  $effect(() => {
    if (!groups.some((g) => g.guild_id === pick)) pick = groups[0]?.guild_id ?? ''
  })
  const group = $derived(groups.find((g) => g.guild_id === pick))
  // people with something remembered first, then the latest seen
  const people = $derived(
    [...((group?.people ?? []) as any[])].sort((a, b) => (b.card_items ?? 0) - (a.card_items ?? 0) || Date.parse(b.last_seen) - Date.parse(a.last_seen)).slice(0, 30),
  )
</script>

<Card title={t('pml.title')} description={t('pml.desc')}>
  {#snippet actions()}<Button size="sm" variant="ghost" href="/people" icon={I.Users}>{t('pml.all')}</Button>{/snippet}
  {#if !servers.data}
    <LoadState res={servers}><Skeleton lines={4} /></LoadState>
  {:else if groups.length === 0}
    <EmptyState compact pose="guide" title={t('people.servers.empty')} description={t('people.servers.empty_body')} />
  {:else}
    <div class="pick"><Select aria-label={t('people.servers.selector')} bind:value={pick} options={groups.map((g) => ({ value: g.guild_id, label: g.name || g.guild_id }))} /></div>
    <ul class="list">
      {#each people as p (p.user_id)}
        <li>
          <div class="who">
            <strong class="ellipsis">{p.name || t('people.unnamed')}</strong>
            <span class="xs muted">{t('pml.row', { n: num(p.messages), when: p.last_seen ? relative(p.last_seen) : '–' })}</span>
          </div>
          <span class="xs muted num">{p.card_items ? t('pml.items', { n: num(p.card_items) }) : t('pml.none')}</span>
          <Button size="sm" variant="ghost" icon={I.Brain} href="/people?person={encodeURIComponent(p.user_id)}&guild={encodeURIComponent(pick)}" aria-label={t('pml.open_one', { name: p.name || p.user_id })}>{t('pml.open')}</Button>
        </li>
      {/each}
    </ul>
    {#if (group?.total_count ?? 0) > people.length}<p class="xs muted">{t('pml.more', { n: num(group.total_count - people.length) })}</p>{/if}
  {/if}
</Card>

<style>
  .pick {
    width: min(100%, 260px);
    margin-bottom: var(--s-3);
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
  }
  .list li {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    padding: var(--s-2) 0;
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .who {
    flex: 1;
    min-width: 0;
    display: grid;
  }
</style>
