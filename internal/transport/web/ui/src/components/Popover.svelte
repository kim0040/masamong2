<script lang="ts">
  import type { Snippet } from 'svelte'
  // A small anchored panel (menus, pickers). Escape and outside clicks close it; focus returns to
  // the trigger.
  let { open = $bindable(false), trigger, children, align = 'end', width = 260 }: { open?: boolean; trigger: Snippet<[{ toggle: () => void; expanded: boolean; id: string }]>; children: Snippet<[{ close: () => void }]>; align?: 'start' | 'end'; width?: number } = $props()
  const id = $props.id()
  let root: HTMLDivElement | undefined = $state()
  let panel: HTMLDivElement | undefined = $state()
  function close(refocus = true) {
    open = false
    if (refocus) (root?.querySelector('[aria-controls]') as HTMLElement | null)?.focus()
  }
  function toggle() {
    open = !open
  }
  $effect(() => {
    if (!open) return
    queueMicrotask(() => (panel?.querySelector('button,[href],input,select,[tabindex="0"]') as HTMLElement | null)?.focus())
    const down = (e: PointerEvent) => {
      if (root && !root.contains(e.target as Node)) close(false)
    }
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        close()
      }
    }
    document.addEventListener('pointerdown', down)
    document.addEventListener('keydown', key)
    return () => {
      document.removeEventListener('pointerdown', down)
      document.removeEventListener('keydown', key)
    }
  })
</script>

<div class="pop" bind:this={root}>
  {@render trigger({ toggle, expanded: open, id })}
  {#if open}
    <div class="panel {align}" id={id} bind:this={panel} style:width="{width}px">
      {@render children({ close: () => close() })}
    </div>
  {/if}
</div>

<style>
  .pop {
    position: relative;
    display: inline-flex;
  }
  .panel {
    position: absolute;
    top: calc(100% + 6px);
    z-index: 60;
    max-width: calc(100vw - 24px);
    max-height: calc(100dvh - var(--topbar-h) - 18px);
    overflow-y: auto;
    overscroll-behavior: contain;
    padding: 6px;
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
    transform-origin: top right;
    animation: pop-in var(--dur) var(--ease) both;
  }
  .end {
    inset-inline-end: 0;
  }
  .start {
    inset-inline-start: 0;
    transform-origin: top left;
  }
  @keyframes pop-in {
    from {
      opacity: 0;
      scale: 0.96;
      translate: 0 -4px;
    }
  }
</style>
