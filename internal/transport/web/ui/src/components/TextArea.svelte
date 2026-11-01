<script lang="ts">
  import type { HTMLTextareaAttributes } from 'svelte/elements'
  let { value = $bindable(), invalid = false, mono = false, ...rest }: HTMLTextareaAttributes & { invalid?: boolean; mono?: boolean } = $props()
</script>

<textarea class="ta" class:mono bind:value aria-invalid={invalid || undefined} {...rest}></textarea>

<style>
  .ta {
    width: 100%;
    min-height: 88px;
    padding: 10px 12px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text);
    line-height: 1.5;
    resize: vertical;
    font-size: var(--text-base);
  }
  .ta:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
  .ta[aria-invalid='true'] {
    box-shadow: 0 0 0 1px var(--danger);
  }
  .mono {
    font-family: var(--mono);
    font-size: var(--text-sm);
  }
  @media (max-width: 640px) {
    .ta {
      font-size: 16px;
    }
  }
</style>
