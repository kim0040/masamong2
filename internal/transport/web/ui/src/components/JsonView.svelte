<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  let { value, label }: { value: unknown; label?: string } = $props()
</script>

<details class="jv">
  <summary>{label ?? t('common.raw')}</summary>
  <pre>{JSON.stringify(value, null, 2)}</pre>
</details>

<style>
  .jv summary {
    cursor: pointer;
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  pre {
    margin: var(--s-2) 0 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    font-size: var(--text-xs);
    max-height: 360px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
