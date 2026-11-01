<script lang="ts">
  import type { HTMLInputAttributes } from 'svelte/elements'
  let { value = $bindable(), el = $bindable(), invalid = false, mono = false, ...rest }: HTMLInputAttributes & { invalid?: boolean; mono?: boolean; el?: HTMLInputElement } = $props()
</script>

<input class="input" class:mono bind:this={el} bind:value aria-invalid={invalid || undefined} {...rest} />

<style>
  .input {
    width: 100%;
    min-height: var(--control-h);
    padding: 0 12px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text);
    font-size: var(--text-base);
    transition-property: box-shadow, background-color;
    transition-duration: var(--dur-fast);
    transition-timing-function: var(--ease-out);
  }
  .input::placeholder {
    color: var(--text-subtle);
  }
  .input:hover:not(:disabled) {
    box-shadow: var(--shadow-border-hover);
  }
  .input:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
  .input[aria-invalid='true'] {
    box-shadow: 0 0 0 1px var(--danger);
  }
  .input:disabled {
    opacity: 0.6;
  }
  .mono {
    font-family: var(--mono);
    font-size: var(--text-sm);
  }
  @media (max-width: 640px) {
    .input {
      font-size: 16px;
    }
  }
</style>
