<script lang="ts">
  import { CircleCheck, CircleAlert, TriangleAlert, CircleDot, Info, LoaderCircle, Minus } from '../lib/icons'
  // Status is never color alone: an icon and a word always come with it.
  export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'neutral' | 'busy'
  let { tone = 'neutral', label, size = 'md' }: { tone?: Tone; label: string; size?: 'sm' | 'md' } = $props()
  const icons = { ok: CircleCheck, warn: TriangleAlert, danger: CircleAlert, info: Info, neutral: Minus, busy: LoaderCircle }
  const Icon = $derived(icons[tone] ?? CircleDot)
</script>

<span class="st {tone} {size}"><span class="ic" class:spin={tone === 'busy'}><Icon size={size === 'sm' ? 12 : 14} strokeWidth={2.25} aria-hidden="true" /></span>{label}</span>

<style>
  .st {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 8px 2px 6px;
    border-radius: 999px;
    font-size: var(--text-xs);
    font-weight: 600;
    line-height: 18px;
    /* never squeezed by its neighbours; only a label longer than the whole box (a long translation)
       wraps, between words, instead of spilling out */
    flex-shrink: 0;
    max-width: 100%;
    overflow-wrap: break-word;
  }
  .md {
    font-size: var(--text-sm);
    padding: 3px 10px 3px 7px;
  }
  .ic {
    display: inline-flex;
  }
  .spin {
    animation: spin 1s linear infinite;
  }
  .ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .warn {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .danger {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .info {
    background: var(--info-soft);
    color: var(--info);
  }
  .neutral,
  .busy {
    background: var(--surface-2);
    color: var(--text-muted);
  }
</style>
