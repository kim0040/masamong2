<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { toast } from '../lib/toast.svelte'
  import Dialog from './Dialog.svelte'
  import Button from './Button.svelte'
  import Input from './Input.svelte'
  import * as I from '../lib/icons'

  // Which channels search one block: tick or untick channels and save; each change is one attach or
  // detach Action (the block itself is never copied or re-embedded).
  let { open = $bindable(false), block, channels = [], onsaved }: { open?: boolean; block: any; channels: any[]; onsaved?: () => void } = $props()

  const keyOf = (c: any) => c.guild_id + '/' + c.channel_id
  const own = $derived(block?.own_channel ? block.own_channel.guild_id + '/' + block.own_channel.channel_id : '')
  const initial = $derived(new Set<string>((block?.channels ?? []).map((c: any) => c.guild_id + '/' + c.channel_id)))
  let chosen = $state<Set<string>>(new Set())
  let q = $state('')
  let saving = $state(false)
  $effect(() => {
    if (open) chosen = new Set(initial)
  })
  const visible = $derived(channels.filter((c) => keyOf(c) !== own && (!q || `${c.name} ${c.guild_name}`.toLowerCase().includes(q.toLowerCase()))))
  const groups = $derived.by(() => {
    const m = new Map<string, any[]>()
    for (const c of visible) m.set(c.guild_name, [...(m.get(c.guild_name) ?? []), c])
    return [...m.entries()]
  })
  const changes = $derived([...chosen].filter((k) => !initial.has(k)).length + [...initial].filter((k) => !chosen.has(k)).length)
  function toggle(k: string, v: boolean) {
    const next = new Set(chosen)
    if (v) next.add(k)
    else next.delete(k)
    chosen = next
  }
  async function save() {
    saving = true
    let failed = 0
    for (const k of chosen) {
      if (initial.has(k)) continue
      const [g, c] = k.split('/')
      const r = await call('channels.view.attach', { guild_id: g, channel_id: c, block_id: block.block_id })
      if (!r.ok) failed++
    }
    for (const k of initial) {
      if (chosen.has(k)) continue
      const [g, c] = k.split('/')
      // detach by the id the channel holds (it may be an older id of the same room)
      const held = (block.channels ?? []).find((x: any) => x.guild_id + '/' + x.channel_id === k)?.block_id ?? block.block_id
      const r = await call('channels.view.detach', { guild_id: g, channel_id: c, block_id: held })
      if (!r.ok) failed++
    }
    saving = false
    if (failed) toast('error', t('connect.failed', { n: failed }))
    else toast('ok', t('connect.saved'))
    open = false
    onsaved?.()
  }
</script>

<Dialog bind:open title={t('connect.title', { name: block?.label ?? '' })} description={t('connect.desc')}>
  {#if channels.length === 0}
    <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('connect.no_channels')} <a href="/rooms">{t('nav.rooms')}</a></span></div>
  {:else}
    <div class="stack">
      {#if channels.length > 8}<Input type="search" bind:value={q} placeholder={t('connect.search')} aria-label={t('connect.search')} />{/if}
      <div class="list">
        {#each groups as [guild, list] (guild)}
          <fieldset>
            <legend class="xs muted">{guild}</legend>
            {#each list as c (keyOf(c))}
              <label class:on={chosen.has(keyOf(c))}>
                <input type="checkbox" checked={chosen.has(keyOf(c))} onchange={(e) => toggle(keyOf(c), (e.currentTarget as HTMLInputElement).checked)} />
                <I.Hash size={15} aria-hidden="true" /><span class="nm">{c.name}</span>
                {#if c.memory_use === 'off'}<span class="chip xs" title={t('connect.memory_off_hint')}>{t('connect.memory_off')}</span>{/if}
              </label>
            {/each}
          </fieldset>
        {/each}
      </div>
      {#if own}<p class="xs muted">{t('connect.own_note')}</p>{/if}
    </div>
  {/if}
  {#snippet footer()}
    <Button variant="ghost" onclick={() => (open = false)}>{t('common.cancel')}</Button>
    <Button variant="primary" icon={I.Link2} loading={saving} disabled={!changes} onclick={save}>{changes ? t('connect.save_n', { n: changes }) : t('common.save')}</Button>
  {/snippet}
</Dialog>

<style>
  .list {
    display: grid;
    gap: var(--s-3);
    max-height: 50vh;
    overflow: auto;
  }
  fieldset {
    margin: 0;
    padding: 0;
    border: 0;
    display: grid;
    gap: 2px;
  }
  legend {
    padding: 0 0 4px;
  }
  label {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: var(--control-h);
    padding: 0 10px;
    border-radius: var(--r);
    cursor: pointer;
  }
  label:hover {
    background: var(--surface-2);
  }
  label.on {
    background: var(--accent-soft);
  }
  label input {
    accent-color: var(--accent);
    width: 16px;
    height: 16px;
    flex: none;
  }
  .nm {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
