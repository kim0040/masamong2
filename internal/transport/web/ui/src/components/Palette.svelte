<script lang="ts">
  import type { Component } from 'svelte'
  import { nav, extraPages } from '../lib/nav'
  import { t, toolName } from '../lib/i18n.svelte'
  import { navigate } from '../lib/router.svelte'
  import { call, practice, setPractice, can } from '../lib/api.svelte'
  import { setPrefs } from '../lib/prefs.svelte'
  import { toast } from '../lib/toast.svelte'
  import { refreshOverview, overview } from '../lib/overview.svelte'
  import { cfg } from '../lib/config.svelte'
  import * as I from '../lib/icons'

  // ⌘K: jump to any page, tool, room or setting, or run a safe command. A combobox over a listbox.
  let { open = $bindable(false) }: { open?: boolean } = $props()
  type Item = { id: string; group: string; label: string; hint?: string; icon: Component<any>; keywords?: string; run: () => void | Promise<void> }
  let q = $state('')
  let idx = $state(0)
  let el: HTMLDialogElement | undefined = $state()
  let input: HTMLInputElement | undefined = $state()
  let tools = $state<{ id: string }[]>([])

  $effect(() => {
    if (!el) return
    if (open && !el.open) {
      el.showModal()
      q = ''
      idx = 0
      queueMicrotask(() => input?.focus())
      if (!tools.length) void call('tools.list').then((r) => r.ok && (tools = r.output.tools))
    } else if (!open && el.open) el.close()
  })

  const items = $derived.by<Item[]>(() => {
    const out: Item[] = []
    for (const g of nav) for (const it of g.items) out.push({ id: 'p' + it.href, group: t('palette.pages'), label: t(`nav.${it.key}`), hint: t(`nav.group.${g.key}`), icon: it.icon, keywords: it.key, run: () => navigate(it.href) })
    for (const it of extraPages) out.push({ id: 'p' + it.href, group: t('palette.pages'), label: t(`nav.${it.key}`), icon: it.icon, keywords: it.key, run: () => navigate(it.href) })
    const paused = !!overview.data?.pause?.all
    out.push({ id: 'c-pause', group: t('palette.commands'), label: paused ? t('pause.resume') : t('pause.pause_all'), icon: paused ? I.Play : I.Pause, keywords: 'pause resume stop', run: async () => { await call('runtime.pause', { scope: 'all', paused: paused ? 'false' : 'true' }); await refreshOverview(); toast('ok', paused ? t('pause.resumed') : t('pause.paused')) } })
    out.push({ id: 'c-backup', group: t('palette.commands'), label: t('palette.cmd_backup'), icon: I.Save, keywords: 'backup', run: async () => { const r = await call('backup.create'); r.ok ? toast('ok', t('maint.backup_done'), r.output?.name) : toast('error', t('maint.backup_failed'), r.error?.message) } })
    out.push({ id: 'c-dark', group: t('palette.commands'), label: t('palette.cmd_dark'), icon: I.Moon, keywords: 'theme dark', run: () => setPrefs({ theme: 'dark' }) })
    out.push({ id: 'c-light', group: t('palette.commands'), label: t('palette.cmd_light'), icon: I.Sun, keywords: 'theme light', run: () => setPrefs({ theme: 'light' }) })
    out.push({ id: 'c-sys', group: t('palette.commands'), label: t('palette.cmd_system'), icon: I.Monitor, keywords: 'theme system', run: () => setPrefs({ theme: 'system' }) })
    out.push({ id: 'c-lab', group: t('palette.commands'), label: t('palette.cmd_lab'), icon: I.FlaskConical, keywords: 'test chat', run: () => navigate('/lab') })
    out.push({ id: 'c-practice', group: t('palette.commands'), label: practice.on ? t('practice.leave') : t('practice.enter'), icon: I.FlaskConical, keywords: 'practice sandbox demo 연습', run: () => void setPractice(!practice.on) })
    out.push({ id: 'c-report', group: t('palette.commands'), label: t('stats.report.make'), icon: I.FileBarChart, keywords: 'report 보고서', run: () => navigate('/stats') })
    out.push({ id: 'c-import', group: t('palette.commands'), label: t('blocks.import'), icon: I.FileUp, keywords: 'kakao csv import 카카오 가져오기 block 블록', run: () => navigate('/blocks?import=1') })
    out.push({ id: 'c-uptime', group: t('palette.commands'), label: t('logs.tab.uptime'), icon: I.Activity, keywords: 'uptime crash 가동', run: () => navigate('/logs?tab=uptime') })
    out.push({ id: 'c-newtool', group: t('palette.commands'), label: t('tools.new'), icon: I.Plus, keywords: 'http tool add', run: () => navigate('/tools/new') })
    for (const tl of tools) out.push({ id: 't' + tl.id, group: t('palette.tools'), label: toolName(tl.id), hint: tl.id, icon: I.Wrench, keywords: tl.id, run: () => navigate(`/tools/${encodeURIComponent(tl.id)}`) })
    for (const [g, gv] of Object.entries<any>(cfg.doc?.guilds ?? {})) for (const c of Object.keys(gv?.channels ?? {})) out.push({ id: `r${g}:${c}`, group: t('palette.rooms'), label: `#${c}`, hint: g, icon: I.Hash, keywords: `${g} ${c}`, run: () => navigate(`/rooms/${g}/${c}`) })
    for (const sec of Object.keys(cfg.doc ?? {})) out.push({ id: 's' + sec, group: t('palette.settings'), label: sec, icon: I.Settings2, keywords: 'config ' + sec, run: () => navigate(`/settings?section=${encodeURIComponent(sec)}`) })
    return out
  })
  const filtered = $derived.by(() => {
    const s = q.trim().toLowerCase()
    const list = s ? items.filter((i) => (i.label + ' ' + (i.hint ?? '') + ' ' + (i.keywords ?? '')).toLowerCase().includes(s)) : items.filter((i) => i.group !== t('palette.settings'))
    return list.slice(0, 60)
  })
  $effect(() => {
    void q
    idx = 0
  })

  async function run(i: Item) {
    open = false
    await i.run()
  }
  function key(e: KeyboardEvent) {
    if (e.key === 'ArrowDown') idx = Math.min(filtered.length - 1, idx + 1)
    else if (e.key === 'ArrowUp') idx = Math.max(0, idx - 1)
    else if (e.key === 'Enter' && filtered[idx]) run(filtered[idx])
    else return
    e.preventDefault()
    el?.querySelector(`#pal-${idx}`)?.scrollIntoView({ block: 'nearest' })
  }
</script>

<dialog bind:this={el} class="pal" aria-label={t('palette.title')} onclose={() => (open = false)} onclick={(e) => e.target === el && (open = false)}>
  <div class="box">
    <div class="in">
      <I.Search size={18} aria-hidden="true" />
      <input bind:this={input} bind:value={q} onkeydown={key} placeholder={t('palette.placeholder')} aria-label={t('palette.title')} role="combobox" aria-expanded="true" aria-controls="pal-list" aria-activedescendant={filtered[idx] ? `pal-${idx}` : undefined} aria-autocomplete="list" autocomplete="off" spellcheck={false} />
      <kbd>Esc</kbd>
    </div>
    <ul id="pal-list" role="listbox" aria-label={t('palette.results')}>
      {#each filtered as it, i (it.id)}
        {#if i === 0 || filtered[i - 1].group !== it.group}<li class="gh" role="presentation">{it.group}</li>{/if}
        <li id="pal-{i}" role="option" aria-selected={i === idx} class:sel={i === idx} onclick={() => run(it)} onpointermove={() => (idx = i)} onkeydown={() => {}}>
          <it.icon size={16} strokeWidth={1.75} aria-hidden="true" />
          <span class="l">{it.label}</span>
          {#if it.hint}<span class="h">{it.hint}</span>{/if}
          {#if i === idx}<I.ArrowRight size={14} aria-hidden="true" />{/if}
        </li>
      {:else}
        <li class="empty" role="presentation">{t('palette.none', { q })}</li>
      {/each}
    </ul>
  </div>
</dialog>

<style>
  .pal {
    width: min(100% - 32px, 620px);
    margin-top: 12vh;
    padding: 0;
    border: 0;
    border-radius: var(--r-xl);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-pop);
    opacity: 0;
    scale: 0.96;
    transition-property: opacity, scale, overlay, display;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
    transition-behavior: allow-discrete;
  }
  .pal[open] {
    opacity: 1;
    scale: 1;
  }
  @starting-style {
    .pal[open] {
      opacity: 0;
      scale: 0.96;
    }
  }
  .pal::backdrop {
    background: var(--scrim);
  }
  .in:focus-within {
    box-shadow: inset 0 -2px 0 var(--focus);
  }
  .in {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 16px;
    height: 56px;
    color: var(--text-muted);
    box-shadow: inset 0 -1px 0 var(--border);
  }
  input {
    flex: 1;
    height: 100%;
    border: 0;
    background: none;
    font-size: var(--text-md);
    outline: none;
    color: var(--text);
  }
  kbd {
    padding: 1px 6px;
    border-radius: 4px;
    background: var(--surface-2);
    font-size: 11px;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 6px;
    max-height: min(56vh, 480px);
    overflow: auto;
  }
  .gh {
    padding: 10px 10px 4px;
    font-size: var(--text-xs);
    font-weight: 650;
    color: var(--text-subtle);
  }
  li[role='option'] {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 40px;
    padding: 0 10px;
    border-radius: var(--r);
    cursor: pointer;
    color: var(--text-muted);
  }
  li.sel {
    background: var(--accent-soft);
    color: var(--text);
  }
  .l {
    color: var(--text);
    font-weight: 550;
  }
  .h {
    flex: 1;
    font-size: var(--text-xs);
    color: var(--text-subtle);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .empty {
    padding: 24px;
    text-align: center;
    color: var(--text-muted);
  }
</style>
