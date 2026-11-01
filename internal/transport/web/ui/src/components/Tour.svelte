<script lang="ts">
  import { tick } from 'svelte'
  import { t } from '../lib/i18n.svelte'
  import { route } from '../lib/router.svelte'
  import { prefs, setPrefs, motionAllowed } from '../lib/prefs.svelte'
  import { tours, tourKey, autoTours, type TourStep } from '../lib/tours'
  import { tourBus } from '../lib/tourbus.svelte'
  import Button from './Button.svelte'
  import { X } from '../lib/icons'

  // A spotlight around one element plus a small card that explains it. Non-modal: the page stays
  // usable; Escape or "Skip" ends the tour, and focus returns where it was.
  let page = $state('')
  let steps = $state<TourStep[]>([])
  let i = $state(0)
  let rect = $state<DOMRect | null>(null)
  let card: HTMLDivElement | undefined = $state()
  let returnTo: Element | null = null
  let timer: ReturnType<typeof setTimeout> | undefined

  const step = $derived(steps[i])
  const open = $derived(!!step && !!rect)

  // Only the home page tours itself on a first visit; every other page waits for its "?" button, so
  // a new operator is not stopped by a card on each page they open.
  $effect(() => {
    const p = tourKey(route.path)
    clearTimeout(timer)
    rect = null
    steps = []
    if (!tours[p] || !autoTours.includes(p) || prefs.tips?.[p]) return
    // wait for the page to render its data before measuring
    timer = setTimeout(() => void start(p), 900)
    return () => clearTimeout(timer)
  })
  let asked = tourBus.seq
  $effect(() => {
    if (tourBus.seq === asked) return
    asked = tourBus.seq
    const p = tourKey(route.path)
    if (tours[p]) void start(p, true)
  })

  async function start(p: string, onRequest = false) {
    if (tourKey(route.path) !== p || (!onRequest && prefs.tips?.[p])) return
    const found = tours[p].filter((s) => document.querySelector(`[data-tour="${s.target}"]`))
    if (!found.length) return
    page = p
    steps = found
    i = 0
    returnTo = document.activeElement
    await show()
  }
  async function show() {
    const el = document.querySelector(`[data-tour="${step?.target}"]`)
    if (!el) return finish()
    el.scrollIntoView({ block: 'center', behavior: motionAllowed() ? 'smooth' : 'auto' })
    await new Promise((r) => setTimeout(r, motionAllowed() ? 280 : 0))
    rect = el.getBoundingClientRect()
    await tick()
    card?.focus()
  }
  function measure() {
    const el = step && document.querySelector(`[data-tour="${step.target}"]`)
    if (el) rect = el.getBoundingClientRect()
  }
  async function nextStep() {
    if (i + 1 >= steps.length) return finish()
    i++
    await show()
  }
  function finish() {
    setPrefs({ tips: { ...(prefs.tips ?? {}), [page]: true } })
    steps = []
    rect = null
    // a tour that opened by itself has nowhere to return to: keep the keyboard in the page, not on <body>
    const back = returnTo && returnTo !== document.body ? returnTo : document.getElementById('main')
    ;(back as HTMLElement | null)?.focus?.({ preventScroll: true })
  }
  // the card sits below the target when there is room, otherwise above
  const pos = $derived.by(() => {
    if (!rect) return { top: 0, left: 0 }
    const w = Math.min(340, window.innerWidth - 24)
    const below = rect.bottom + 12 + 180 < window.innerHeight
    const top = below ? rect.bottom + 12 : Math.max(12, rect.top - 12 - 190)
    const left = Math.min(Math.max(12, rect.left), window.innerWidth - w - 12)
    return { top, left, w }
  })
</script>

<svelte:window onresize={measure} onscroll={measure} onkeydown={(e) => open && e.key === 'Escape' && finish()} />

{#if open && rect}
  <div class="spot" aria-hidden="true" style:top="{rect.top - 6}px" style:left="{rect.left - 6}px" style:width="{rect.width + 12}px" style:height="{rect.height + 12}px"></div>
  <div class="tour" bind:this={card} role="dialog" aria-modal="false" aria-labelledby="tour-t" aria-describedby="tour-b" tabindex="-1"
    style:top="{pos.top}px" style:left="{pos.left}px" style:width="{pos.w}px">
    <div class="head">
      <span class="xs muted num">{t('tour.step', { i: i + 1, n: steps.length })}</span>
      <button type="button" class="x" aria-label={t('tour.skip')} onclick={finish}><X size={16} aria-hidden="true" /></button>
    </div>
    <h2 id="tour-t">{t(`tour.${step.key}.title`)}</h2>
    <p id="tour-b" class="small">{t(`tour.${step.key}.body`)}</p>
    <div class="row">
      <Button size="sm" variant="ghost" onclick={finish}>{t('tour.skip')}</Button>
      <span class="spacer"></span>
      <Button size="sm" variant="primary" onclick={nextStep}>{i + 1 >= steps.length ? t('tour.done') : t('tour.next')}</Button>
    </div>
  </div>
{/if}

<style>
  .spot {
    position: fixed;
    z-index: 80;
    border-radius: var(--r-lg);
    box-shadow: 0 0 0 9999px var(--scrim), 0 0 0 2px var(--accent);
    pointer-events: none;
    transition-property: top, left, width, height;
    transition-duration: var(--dur-slow);
    transition-timing-function: var(--ease);
  }
  .tour {
    position: fixed;
    z-index: 81;
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 14px 16px;
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-pop);
    outline: none;
    animation: tour-in 260ms var(--ease) both;
  }
  .tour:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  @keyframes tour-in {
    from {
      opacity: 0;
      translate: 0 6px;
    }
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  h2 {
    font-size: var(--text-md);
    margin: 0;
  }
  p {
    margin: 0;
    color: var(--text-muted);
    text-wrap: pretty;
  }
  .x {
    display: grid;
    place-items: center;
    width: 28px;
    height: 28px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .x:hover {
    background: var(--surface-2);
  }
</style>
