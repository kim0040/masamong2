<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '../lib/i18n.svelte'
  import Button from './Button.svelte'
  import Card from './Card.svelte'
  import EmptyState from './EmptyState.svelte'

  // Stands in for the page's content while the data it needs is missing: the skeleton (children) while it
  // loads, with a plain-words note when that takes long, and the reason with a retry button when the
  // first load failed. Nothing once every resource has data. A later refresh that fails is shown by the
  // banner strip (Banners.svelte) over the data already on screen.
  type Source = { data: unknown; error?: { message: string }; refresh: () => Promise<unknown> }
  let { res, children }: { res: Source | Source[]; children?: Snippet } = $props()
  const list = $derived(Array.isArray(res) ? res : [res])
  const failed = $derived(list.find((r) => r.error && r.data == null))
  const ready = $derived(list.every((r) => r.data != null))
  let busy = $state(false)
  async function retry() {
    busy = true
    await Promise.all(list.filter((r) => r.error).map((r) => r.refresh()))
    busy = false
  }

  // a skeleton that stays for a few seconds gets words: grey bars alone look like a hung page
  let slow = $state(false)
  $effect(() => {
    slow = false
    if (ready || failed) return
    const id = setTimeout(() => (slow = true), 3000)
    return () => clearTimeout(id)
  })
</script>

{#if failed}
  <Card>
    <div role="alert">
      <EmptyState compact pose="oops" title={t('load.failed')} description={failed.error?.message}>
        <Button variant="primary" onclick={retry} loading={busy}>{t('common.retry')}</Button>
      </EmptyState>
    </div>
  </Card>
{:else if !ready}
  {@render children?.()}
  {#if slow}<p class="small muted slow" role="status">{t('load.slow')}</p>{/if}
{/if}

<style>
  .slow {
    margin-top: var(--s-3);
    text-align: center;
  }
</style>
