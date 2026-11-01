<script lang="ts">
  import { brand } from 'virtual:brand'
  let { pose = 'hello', size = 160, alt = '' }: { pose?: string; size?: number; alt?: string } = $props()
</script>

<img class="mascot" src={brand(`mascot/mong-${pose}.webp`)} {alt} width={size} height={size} loading="lazy" decoding="async" />

<style>
  .mascot {
    display: block;
    object-fit: contain;
    user-select: none;
    -webkit-user-drag: none;
  }
</style>
