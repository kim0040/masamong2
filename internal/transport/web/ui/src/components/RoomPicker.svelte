<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { groupRooms, type RoomItem } from '../lib/rooms'
  import { num } from '../lib/format'
  import Input from './Input.svelte'
  import Switch from './Switch.svelte'
  import Button from './Button.svelte'

  // Pick rooms to search: one group per server, rooms with no messages out of the way until asked for, a
  // filter for long lists. The KakaoTalk imports are a group of their own, last.
  let { items, value = $bindable([]) }: { items: RoomItem[]; value: string[] } = $props()

  let filter = $state('')
  let showEmpty = $state(false)
  const groups = $derived(groupRooms(items, { showEmpty, filter }))
  const emptyTotal = $derived(items.filter((r) => r.count === 0).length)
  const many = $derived(items.length > 12)
  const toggle = (k: string) => (value = value.includes(k) ? value.filter((x) => x !== k) : [...value, k])
  const setGroup = (g: { items: RoomItem[] }, on: boolean) => {
    const keys = new Set(g.items.map((r) => r.key))
    value = on ? [...new Set([...value, ...keys])] : value.filter((k) => !keys.has(k))
  }
  const selectedIn = (g: { items: RoomItem[] }) => g.items.filter((r) => value.includes(r.key)).length
  // a group stays folded when it is long and none of its rooms is picked
  const openAtStart = (g: { items: RoomItem[] }) => groups.length === 1 || selectedIn(g) > 0 || !!filter
</script>

<div class="rp">
  {#if many || emptyTotal > 0}
    <div class="bar">
      {#if many}<div class="find"><Input type="search" bind:value={filter} placeholder={t('rp.filter')} aria-label={t('rp.filter')} /></div>{/if}
      {#if emptyTotal > 0}<Switch bind:checked={showEmpty} label={t('rp.show_empty', { n: emptyTotal })} />{/if}
    </div>
  {/if}
  {#each groups as g (g.id)}
    <details class="grp" open={openAtStart(g)}>
      <summary><span class="srv">{g.server}</span><span class="xs muted num">{t('rp.picked', { n: selectedIn(g), total: g.items.length })}{#if g.emptyHidden} · {t('rp.empty_hidden', { n: g.emptyHidden })}{/if}</span></summary>
      <div class="acts"><Button size="sm" variant="ghost" onclick={() => setGroup(g, true)}>{t('rp.all')}</Button><Button size="sm" variant="ghost" onclick={() => setGroup(g, false)}>{t('rp.none')}</Button></div>
      <div class="chips">
        {#each g.items as r (r.key)}
          <button type="button" class="room" class:on={value.includes(r.key)} class:dim={r.hidden} aria-pressed={value.includes(r.key)} title={r.key} onclick={() => toggle(r.key)}>{r.label}<span class="num">{num(r.count)}</span></button>
        {/each}
      </div>
    </details>
  {:else}
    <p class="small muted">{filter ? t('rp.no_match') : t('index.no_rooms')}</p>
  {/each}
  <p class="xs muted">{t('rp.legend')}</p>
</div>

<style>
  .rp {
    display: grid;
    gap: var(--s-2);
  }
  .bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--s-2) var(--s-4);
  }
  .find {
    flex: 1 1 200px;
    min-width: 0;
  }
  .grp {
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    padding: 0 var(--s-3);
  }
  summary {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 2px var(--s-3);
    cursor: pointer;
    padding: var(--s-2) 0;
  }
  .srv {
    font-weight: 600;
    font-size: var(--text-sm);
  }
  .acts {
    display: flex;
    gap: var(--s-1);
    margin-bottom: 4px;
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding-bottom: var(--s-3);
  }
  .room {
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    display: inline-flex;
    gap: 6px;
    align-items: center;
    height: 30px;
    padding: 0 12px;
    border: 0;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .room.dim {
    font-style: italic;
  }
  .room .num {
    font-size: var(--text-xs);
  }
  .room.on {
    background: var(--accent-soft);
    color: var(--accent-text);
  }
</style>
