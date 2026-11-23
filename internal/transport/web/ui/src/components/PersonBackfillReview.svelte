<script lang="ts">
  import { has, t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { latest } from '../lib/seqload'
  import { num } from '../lib/format'
  import { groupReview, modelName, type ReviewItem } from '../lib/peoplebackfill'
  import Dialog from './Dialog.svelte'
  import Button from './Button.svelte'
  import Skeleton from './Skeleton.svelte'

  // What the pilot read, for the operator to look over before the real run: facts tied to a person by name,
  // facts the two models saw differently, and a sample of what people said about others. A plain read; the
  // bot has not applied any of it and nothing here can be edited. The text is shown to this operator only.
  let { open = $bindable(false), guild, runId }: { open?: boolean; guild: string; runId: string } = $props()
  let view = $state<{ loading: boolean; error: string; available: boolean; dropped: number; items: ReviewItem[] } | null>(null)
  const loads = latest()
  async function load() {
    const live = loads.start()
    view = { loading: true, error: '', available: true, dropped: 0, items: [] }
    const r = await call('memory.people.backfill.review', runId ? { run_id: runId } : { guild_id: guild })
    if (!live()) return
    view = r.ok
      ? { loading: false, error: '', available: r.output.available !== false, dropped: r.output.dropped ?? 0, items: r.output.items ?? [] }
      : { loading: false, error: r.error?.message ?? t('common.failed'), available: true, dropped: 0, items: [] }
  }
  $effect(() => {
    if (open) void load()
  })
  const groups = $derived(groupReview(view?.items))
  const kind = (k: string | undefined) => (k && has(`pm.kind.${k}`) ? t(`pm.kind.${k}`) : (k ?? ''))
</script>

<Dialog bind:open size="lg" title={t('pbr.title')} description={t('pbr.desc')}>
  {#if !view || view.loading}
    <Skeleton lines={6} />
  {:else if view.error}
    <div role="alert" class="callout danger"><span>{view.error}</span><Button size="sm" variant="ghost" onclick={load}>{t('common.retry')}</Button></div>
  {:else if !view.available}
    <p class="small muted">{t('pbr.gone')}</p>
  {:else if groups.length === 0}
    <p class="small muted">{t('pbr.none')}</p>
  {:else}
    <div class="groups">
      {#each groups as g (g.part)}
        <section aria-labelledby="pbr-{g.part}">
          <h3 id="pbr-{g.part}" class="h">{t(`pbr.part.${g.part}`)} <span class="xs muted">{t('pbr.count', { n: num(g.items.length) })}</span></h3>
          <p class="xs muted">{t(`pbr.part.${g.part}_hint`)}</p>
          <ul class="rows">
            {#each g.items as it, i (`${it.window_id}-${it.model}-${i}`)}
              <li>
                <p class="text break">{it.text}</p>
                <p class="meta xs muted">
                  <span class="chip">{kind(it.kind)}</span>
                  {#if it.subject}<span>{t('pbr.subject', { name: it.subject })}</span>{/if}
                  {#if it.speakers?.length}<span>{t('pbr.speaker', { names: it.speakers.join(', ') })}</span>{/if}
                  <span>{modelName(it.model)}</span>
                </p>
              </li>
            {/each}
          </ul>
        </section>
      {/each}
    </div>
    {#if view.dropped > 0}<p class="xs muted">{t('pbr.dropped', { n: num(view.dropped) })}</p>{/if}
  {/if}
</Dialog>

<style>
  .groups {
    display: grid;
    gap: var(--s-5, var(--s-4));
    min-width: 0;
  }
  .h {
    margin: 0 0 4px;
    font-size: var(--text-md);
  }
  .rows {
    list-style: none;
    margin: var(--s-2) 0 0;
    padding: 0;
    display: grid;
  }
  .rows li {
    padding: var(--s-2) 0;
    box-shadow: inset 0 -1px 0 var(--border);
    min-width: 0;
  }
  .text {
    margin: 0 0 4px;
    font-size: var(--text-sm);
  }
  .meta {
    margin: 0;
    display: flex;
    flex-wrap: wrap;
    gap: 4px 10px;
    align-items: baseline;
  }
</style>
