<script lang="ts">
  let { items }: { items: { k: string; v: string | number | undefined | null; mono?: boolean }[] } = $props()
</script>

<dl class="kv">
  {#each items as it (it.k)}
    <dt>{it.k}</dt>
    <dd class:mono={it.mono} class="break">{it.v === undefined || it.v === null || it.v === '' ? '–' : it.v}</dd>
  {/each}
</dl>

<style>
  .kv {
    display: grid;
    grid-template-columns: minmax(96px, max-content) 1fr;
    gap: 8px var(--s-4);
    margin: 0;
    font-size: var(--text-sm);
  }
  dt {
    color: var(--text-muted);
  }
  dd {
    margin: 0;
    min-width: 0;
  }
</style>
