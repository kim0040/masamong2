<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { shortDate } from '../lib/format'
  import Button from './Button.svelte'
  import Input from './Input.svelte'
  import Skeleton from './Skeleton.svelte'
  import * as I from '../lib/icons'

  // events.timeline: what happened when in a memory scope (a channel's, or chosen rooms). Browsing and
  // the lexical fallback make no model call; a question uses the search's own query embedding.
  let { scope, own = '' }: { scope: Record<string, unknown>; own?: string } = $props()
  let input = $state('')
  let query = $state('')
  // the parent renders this only with a scope (a channel, or at least one room)
  const tl = resource<any>('events.timeline', () => ({ ...scope, limit: 40, ...(query ? { query } : {}) }), { live: ['memory'] })
  const clock = (iso: string) => (iso?.length >= 16 ? iso.slice(11, 16) : '')
</script>

<form class="row form" onsubmit={(e) => { e.preventDefault(); query = input.trim() }}>
  <div class="grow"><Input type="search" bind:value={input} placeholder={t('room.timeline_ph')} aria-label={t('room.timeline_ph')} /></div>
  <Button type="submit" icon={I.Search}>{t('room.timeline_find')}</Button>
  {#if query}<Button variant="ghost" onclick={() => { query = ''; input = '' }}>{t('room.timeline_all')}</Button>{/if}
</form>
{#if tl.error && !tl.data}<p class="small muted">{t('room.timeline_unavailable', { error: tl.error.message })}</p>
{:else if !tl.data}<Skeleton lines={4} />
{:else if tl.data.count === 0}<p class="small muted">{query ? t('room.timeline_none_found') : t('room.timeline_empty')}</p>
{:else}
  <ol class="days">
    {#each tl.data.days as d (d.date)}
      <li>
        <h3 class="small">{shortDate(d.date)}</h3>
        <ul>
          {#each d.entries as e (e.doc_id)}
            <li>
              <span class="xs subtle num">{clock(e.time)}</span>
              <div class="ml">
                <span class="small">{e.summary || e.excerpt}</span>
                <span class="xs muted">{[e.room !== own ? e.label : '', (e.speakers ?? []).slice(0, 4).join(', ')].filter(Boolean).join(' · ')}</span>
              </div>
            </li>
          {/each}
        </ul>
      </li>
    {/each}
  </ol>
{/if}

<style>
  .form {
    margin-bottom: var(--s-3);
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .days {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: var(--s-3);
  }
  .days h3 {
    margin: 0 0 6px;
    font-weight: 600;
  }
  .days ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .days ul li {
    display: grid;
    grid-template-columns: 3.2em minmax(0, 1fr);
    gap: var(--s-2);
    align-items: baseline;
  }
  .ml {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
</style>
