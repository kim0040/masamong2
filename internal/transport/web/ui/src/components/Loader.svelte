<script lang="ts">
  import { brand } from 'virtual:brand'
  // The mascot wagging its tail (4-frame strip); a still frame under reduced motion.
  let { label, size = 72 }: { label: string; size?: number } = $props()
</script>

<div class="loader" role="status">
  <span class="wag" style:width="{size}px" style:height="{size}px" style:--strip="url({brand('mong-loading.webp')})" aria-hidden="true"></span>
  <span class="lbl">{label}</span>
</div>

<style>
  .loader {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--s-2);
    padding: var(--s-8);
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .wag {
    background: var(--strip) 0 0 / 400% 100% no-repeat;
    animation: wag 0.8s steps(4) infinite;
  }
  @keyframes wag {
    to {
      background-position: 133.333% 0;
    }
  }
</style>
