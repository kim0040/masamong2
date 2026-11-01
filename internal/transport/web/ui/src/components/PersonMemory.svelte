<script lang="ts">
  import { untrack } from 'svelte'
  import { has, t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { datetime, num } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { splitMine, forgetTotal } from '../lib/personmemory'
  import Field from './Field.svelte'
  import Select from './Select.svelte'
  import Button from './Button.svelte'
  import Confirm from './Confirm.svelte'
  import Skeleton from './Skeleton.svelte'
  import * as I from '../lib/icons'

  // What the bot keeps about one person in one server (memory.mine.show), and forgetting it: a preview
  // first, then a confirmation with the preview's token (memory.mine.forget). Each server keeps its own,
  // so the operator picks the server. The person is named by the dialog this sits in; the ID is not shown.
  let { userId }: { userId: string } = $props()

  const channels = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const servers = $derived(((channels.data?.guilds ?? []) as any[]).map((g) => ({ value: g.id as string, label: (g.name || g.id) as string })))
  let guild = $state('')
  $effect(() => {
    if ((!guild || !servers.some((s) => s.value === guild)) && servers.length) guild = servers[0].value
  })

  let view = $state<{ loading: boolean; error: string; data: any } | null>(null)
  let seq = 0
  async function load() {
    const my = ++seq
    if (!guild || !userId) return void (view = null)
    view = { loading: true, error: '', data: view?.data ?? null }
    const r = await call('memory.mine.show', { guild_id: guild, user_id: userId })
    if (my !== seq) return
    view = r.ok ? { loading: false, error: '', data: r.output } : { loading: false, error: r.error?.message ?? t('common.failed'), data: null }
  }
  $effect(() => {
    void guild
    void userId
    untrack(() => void load())
  })

  const parts = $derived(splitMine(view?.data?.items))
  const card = $derived((view?.data?.person_card ?? []) as string[])
  const episodes = $derived<number>(view?.data?.episodes ?? 0)
  const empty = $derived(!!view?.data && parts.told.length + parts.other.length + card.length + episodes === 0)
  const kindLabel = (k: string) => (has(`rm.item.${k}`) ? t(`rm.item.${k}`) : k)

  // forgetting: the preview opens the confirmation, the token it returns confirms exactly that
  let plan = $state<{ items: number; episodes: number; cards: number; token: string } | null>(null)
  let confirmOpen = $state(false)
  let previewing = $state(false)
  async function preview() {
    previewing = true
    const r = await call('memory.mine.forget', { guild_id: guild, user_id: userId, scope: 'all' })
    previewing = false
    if (!r.ok) return void toast('error', t('common.failed'), r.error?.message)
    plan = r.output
    if (forgetTotal(plan) === 0) return void toast('info', t('pm.forget_nothing'))
    confirmOpen = true
  }
  async function forget() {
    if (!plan) return
    const r = await call('memory.mine.forget', { guild_id: guild, user_id: userId, scope: 'all', confirm: plan.token })
    if (!r.ok) toast('error', t('common.failed'), r.error?.message)
    else if (r.output.applied) toast('ok', t('pm.forgot', { n: num(r.output.deleted ?? 0) }))
    else toast('warn', t('pm.stale'))
    await load()
  }
</script>

<div class="pm">
  {#if channels.data && servers.length === 0}
    <p class="small muted">{t('pm.no_servers')}</p>
  {:else}
    <Field id="pm-server" label={t('pm.server')}>
      {#snippet children({ id })}<Select {id} bind:value={guild} options={servers} disabled={servers.length === 0} />{/snippet}
    </Field>
    <div aria-live="polite" class="out">
      {#if !view || (view.loading && !view.data)}
        <Skeleton lines={3} />
      {:else if view.error}
        <div role="alert" class="callout danger"><span class="ic"><I.CircleAlert size={16} aria-hidden="true" /></span><span>{view.error}</span><Button size="sm" variant="ghost" onclick={() => load()}>{t('common.retry')}</Button></div>
      {:else if empty}
        <p class="small muted">{t('pm.empty')}</p>
      {:else}
        {#if parts.told.length}
          <section aria-labelledby="pm-told">
            <h4 id="pm-told" class="sub">{t('pm.told')}</h4>
            <ul class="list">
              {#each parts.told as it (it.id)}<li><span class="break">{it.text}</span> <span class="xs muted">{datetime(it.created_at)}</span></li>{/each}
            </ul>
          </section>
        {/if}
        {#if card.length}
          <section aria-labelledby="pm-card">
            <h4 id="pm-card" class="sub">{t('pm.card')}</h4>
            <ul class="list">{#each card as c}<li class="break">{c}</li>{/each}</ul>
          </section>
        {/if}
        {#if parts.other.length}
          <section aria-labelledby="pm-items">
            <h4 id="pm-items" class="sub">{t('pm.items')}</h4>
            <ul class="list">
              {#each parts.other as it (it.id)}
                <li><span class="chip">{kindLabel(it.kind)}</span> <span class="break">{it.text}</span> <span class="xs muted">{datetime(it.created_at)}</span></li>
              {/each}
            </ul>
          </section>
        {/if}
        <p class="xs muted">{t('pm.episodes', { n: num(episodes) })}</p>
      {/if}
    </div>
    <div class="act">
      <Button size="sm" variant="danger-ghost" icon={I.Trash2} loading={previewing} disabled={!guild || !!view?.loading || !!view?.error || empty} onclick={preview}>{t('pm.forget')}</Button>
    </div>
  {/if}
</div>

<Confirm bind:open={confirmOpen} danger title={t('pm.forget_title')} body={t('pm.forget_body')} confirmLabel={t('pm.forget_confirm')} onconfirm={forget}>
  {#snippet details()}
    {#if plan}<p class="fp small">{t('pm.forget_preview', { items: num(plan.items), episodes: num(plan.episodes), cards: num(plan.cards) })}</p>{/if}
  {/snippet}
</Confirm>

<style>
  .pm {
    display: grid;
    gap: var(--s-3);
    min-width: 0;
  }
  .out {
    display: grid;
    gap: var(--s-3);
    min-width: 0;
  }
  .sub {
    margin: 0 0 6px;
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text-muted);
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .list li {
    font-size: var(--text-sm);
    min-width: 0;
  }
  .fp {
    margin: var(--s-3) 0 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
  }
</style>
