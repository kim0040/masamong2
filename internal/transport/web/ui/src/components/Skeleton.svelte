<script lang="ts">
  let { lines = 3, height = 14, block = false }: { lines?: number; height?: number; block?: boolean } = $props()
</script>

<div class="sk" aria-hidden="true">
  {#if block}
    <div class="bar" style:height="{height}px"></div>
  {:else}
    {#each Array(lines) as _, i}<div class="bar" style:height="{height}px" style:width="{i === lines - 1 ? 60 : 100}%"></div>{/each}
  {/if}
</div>

<style>
  .sk {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .bar {
    border-radius: var(--r-sm);
    background: linear-gradient(90deg, var(--surface-2) 25%, var(--surface-3) 50%, var(--surface-2) 75%);
    background-size: 200% 100%;
    animation: shimmer 1.4s linear infinite;
  }
</style>
