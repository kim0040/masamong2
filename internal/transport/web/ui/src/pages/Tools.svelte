<script lang="ts">
  import { t, has, toolName, toolDesc } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, patchConfig, useConfig } from '../lib/config.svelte'
  import { num } from '../lib/format'
  import { toolState, toolNeedsAttention, toolGroup, groupOrder, type Group } from '../lib/toolmeta'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Status from '../components/Status.svelte'
  import Button from '../components/Button.svelte'
  import Switch from '../components/Switch.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Sparkline from '../components/Sparkline.svelte'
  import Skeleton from '../components/Skeleton.svelte'
  import LoadState from '../components/LoadState.svelte'
  import EmptyState from '../components/EmptyState.svelte'
  import Input from '../components/Input.svelte'
  import Select from '../components/Select.svelte'
  import Confirm from '../components/Confirm.svelte'
  import * as I from '../lib/icons'

  useConfig()
  const tools = resource<any>('tools.list', () => ({}), { live: ['config', 'turn'] })
  const providers = resource<any>('providers.list', () => ({}), { live: ['config'] })
  const series = resource<any>('stats.series', () => ({ days: 14 }), { live: ['turn'] })
  let filter = $state<'all' | 'on' | 'off' | 'attention'>('all')
  let q = $state('')
  const plist = $derived((providers.data?.providers ?? []) as any[])
  function stateOf(tl: any): { tone: any; label: string; hint?: string } {
    const st = toolState(tl)
    // working, but some of its providers still lack a key (weather without KMA: worldwide source only)
    const partial = tl.ready && has(`tools.partial.${tl.id}`) && plist.some((p) => (tl.providers ?? []).includes(p.id) && !p.configured && p.status !== 'disabled')
    return { ...st, hint: partial ? t(`tools.partial.${tl.id}`) : undefined }
  }
  const needsAttention = toolNeedsAttention
  const visible = $derived(
    ((tools.data?.tools ?? []) as any[])
      .filter((tl) => !q || `${tl.id} ${toolName(tl.id)} ${toolDesc(tl.id)}`.toLowerCase().includes(q.toLowerCase()))
      .filter((tl) => filter === 'all' || (filter === 'on' && tl.enabled) || (filter === 'off' && !tl.enabled) || (filter === 'attention' && needsAttention(tl))),
  )
  const grouped = $derived.by(() => {
    const m = new Map<Group, any[]>()
    for (const tl of visible) m.set(toolGroup(tl), [...(m.get(toolGroup(tl)) ?? []), tl])
    return groupOrder.filter((g) => m.has(g)).map((g) => [g, m.get(g)!] as const)
  })
  const attention = $derived(((tools.data?.tools ?? []) as any[]).filter(needsAttention).length)
  // a tool that hands what people post to an outside service is turned on only after saying so
  let optIn = $state<any>(null)
  let optInOpen = $state(false)
  let redraw = $state(0)
  async function toggle(tl: any, v: boolean) {
    if (v && tl.opt_in && !tl.enabled) {
      optIn = tl
      optInOpen = true
      redraw++ // the switch goes back to off until the dialog is confirmed
      return
    }
    await quick(tl.id, 'enabled', v)
  }
  async function quick(id: string, field: 'enabled' | 'chat' | 'commands', v: boolean) {
    await patchConfig([{ path: ['tools', id, field], value: v }], { success: t('tools.saved', { tool: toolName(id) }) })
    void tools.refresh()
  }
  const cap = (tl: any) => tl.limits?.per_day || 0
  // how a reply picks its tools (config chat.tool_mode); "loop" is the default and is stored as unset
  type Mode = 'planner' | 'shadow' | 'loop'
  const toolMode = $derived<Mode>((cfg.doc?.chat?.tool_mode as Mode) || 'loop')
  async function setMode(v: Mode) {
    await patchConfig([v === 'loop' ? { path: ['chat', 'tool_mode'], op: 'unset' } : { path: ['chat', 'tool_mode'], value: v }], { success: t('tools.mode.saved') })
  }
  // which model the tool loop asks (config chat.tool_model): "planner" is the default and is stored as unset; only the loop uses it
  type LoopModel = 'planner' | 'response'
  const toolModel = $derived<LoopModel>((cfg.doc?.chat?.tool_model as LoopModel) || 'planner')
  async function setModel(v: LoopModel) {
    await patchConfig([v === 'planner' ? { path: ['chat', 'tool_model'], op: 'unset' } : { path: ['chat', 'tool_model'], value: v }], { success: t('tools.model.saved') })
  }
  // whether a reply looks at the pictures the bot is called with (config chat.vision); "auto" is the default and is stored as unset
  const visionOn = $derived((cfg.doc?.chat?.vision ?? 'auto') !== 'off')
  async function setVision(v: boolean) {
    await patchConfig([v ? { path: ['chat', 'vision'], op: 'unset' } : { path: ['chat', 'vision'], value: 'off' }], { success: t('tools.vision.saved') })
  }
</script>

<PageHeader title={t('tools.title')} description={t('tools.desc')}>
  {#snippet actions()}
    <Button data-tour="tools-new" variant="primary" href="/tools/new" icon={I.Plus}>{t('tools.new')}</Button>
    <Button href="/usage" variant="ghost" icon={I.Gauge}>{t('tools.limits_overview')}</Button>
  {/snippet}
</PageHeader>

<div class="bar">
  <Segmented label={t('tools.filter')} bind:value={filter} options={[
    { value: 'all', label: t('tools.f.all') }, { value: 'on', label: t('tools.f.on') }, { value: 'off', label: t('tools.f.off') },
    { value: 'attention', label: attention ? t('tools.f.attention_n', { n: attention }) : t('tools.f.attention') }]} />
  <div class="search"><Input type="search" bind:value={q} placeholder={t('tools.search')} aria-label={t('tools.search')} /></div>
</div>

{#if cfg.loaded}
  <section class="mode" aria-labelledby="tool-mode-title">
    <div class="mode-text">
      <h2 id="tool-mode-title" class="gt">{t('tools.mode.title')}</h2>
      <p class="xs muted">{t(`tools.mode.hint.${toolMode}`)}</p>
    </div>
    <Segmented label={t('tools.mode.title')} value={toolMode} onchange={setMode} size="sm" options={[
      { value: 'loop', label: t('tools.mode.v.loop') }, { value: 'planner', label: t('tools.mode.v.planner') }, { value: 'shadow', label: t('tools.mode.v.shadow') }]} />
    {#if toolMode === 'loop'}
      <div class="model">
        <div class="mode-text">
          <h3 id="tool-model-title" class="gt">{t('tools.model.title')}</h3>
          <p class="xs muted">{t(`tools.model.hint.${toolModel}`)}</p>
        </div>
        <div class="model-select"><Select aria-label={t('tools.model.title')} value={toolModel} onchange={(e) => setModel(e.currentTarget.value as LoopModel)} options={[
          { value: 'planner', label: t('tools.model.v.planner') }, { value: 'response', label: t('tools.model.v.response') }]} /></div>
      </div>
    {/if}
    <div class="model">
      <div class="mode-text">
        <h3 id="tool-vision-title" class="gt">{t('tools.vision.title')}</h3>
        <p class="xs muted">{t(`tools.vision.hint.${visionOn ? 'on' : 'off'}`)}</p>
      </div>
      <Switch checked={visionOn} onchange={setVision} label={t('tools.vision.switch')} />
    </div>
  </section>
{/if}

{#if !tools.data}
  <LoadState res={tools}><div class="grid" style:--min="340px">{#each Array(6) as _}<Card><Skeleton lines={4} /></Card>{/each}</div></LoadState>
{:else if visible.length === 0}
  <Card><EmptyState pose="tools" title={t('tools.empty')} description={t('tools.empty_body')}><Button href="/tools/new" icon={I.Plus}>{t('tools.new')}</Button></EmptyState></Card>
{:else}
  <div data-tour="tools-grid">
    {#each grouped as [group, list] (group)}
      {#if group === 'later' || group === 'internal'}
        <details class="more">
          <summary><span class="gt">{t(`tools.group.${group}`)}</span> <span class="xs muted">{t(`tools.group.${group}_desc`, { n: list.length })}</span></summary>
          <ul class="mini">
            {#each list as tl (tl.id)}
              <li><a href="/tools/{encodeURIComponent(tl.id)}">{toolName(tl.id)}</a><span class="xs muted">{tl.kind === 'http' ? tl.description : toolDesc(tl.id) || tl.description}</span></li>
            {/each}
          </ul>
        </details>
      {:else}
        <section class="group">
          <h2 class="gt">{t(`tools.group.${group}`)}</h2>
          <div class="grid" style:--min="320px">
            {#each list as tl, i (tl.id)}
              {@const st = stateOf(tl)}
              <article class="tool enter" style:--i={Math.min(i, 8)}>
                <a class="cover" href="/tools/{encodeURIComponent(tl.id)}" aria-label={t('tools.open', { tool: toolName(tl.id) })}></a>
                <header>
                  <span class="ic" class:off={!tl.enabled} aria-hidden="true"><I.Wrench size={18} strokeWidth={1.75} /></span>
                  <div class="nm">
                    <h3>{toolName(tl.id)}</h3>
                    {#if tl.kind === 'http'}<code class="xs subtle">{tl.id}</code>{/if}
                  </div>
                  <Status tone={st.tone} label={st.label} size="sm" />
                </header>
                <p class="desc small muted">{tl.kind === 'http' ? tl.description : toolDesc(tl.id) || tl.description}</p>
                {#if st.hint}<p class="xs hint"><I.Info size={12} aria-hidden="true" /> {st.hint}</p>{/if}
                <div class="usage">
                  <div class="u-top small"><span class="muted">{t('tools.today')}</span><span class="num"><strong>{num(tl.used_today)}</strong>{#if cap(tl)}<span class="muted"> / {num(cap(tl))}</span>{/if}</span></div>
                  {#if cap(tl)}<div class="cap" aria-hidden="true"><span style:width="{Math.min(100, (100 * tl.used_today) / cap(tl))}%" class:hot={tl.used_today >= cap(tl)}></span></div>{/if}
                  <Sparkline data={series.data?.tools?.[tl.id] ?? []} label={t('tools.trend')} height={28} />
                </div>
                <div class="switches">
                  {#key redraw}<Switch checked={tl.enabled} disabled={!tl.implemented} onchange={(v) => toggle(tl, v)} label={t('tools.sw.enabled')} ariaLabel="{toolName(tl.id)} {t('tools.sw.enabled')}" />{/key}
                  {#if tl.id !== 'chat.respond'}
                    {#if tl.can_chat === false}<span class="na" title={t('tools.na.chat')}><span class="na-v xs muted">{t('tools.na.label')}</span><span class="na-l">{t('tools.sw.chat')}</span></span>
                    {:else}<Switch checked={tl.chat} disabled={!tl.enabled || !tl.implemented} onchange={(v) => quick(tl.id, 'chat', v)} label={t('tools.sw.chat')} ariaLabel="{toolName(tl.id)} {t('tools.sw.chat')}" />{/if}
                    {#if tl.can_command === false}<span class="na" title={t('tools.na.commands')}><span class="na-v xs muted">{t('tools.na.label')}</span><span class="na-l">{t('tools.sw.commands')}</span></span>
                    {:else}<Switch checked={tl.commands} disabled={!tl.enabled || !tl.implemented} onchange={(v) => quick(tl.id, 'commands', v)} label={t('tools.sw.commands')} ariaLabel="{toolName(tl.id)} {t('tools.sw.commands')}" />{/if}
                  {/if}
                </div>
              </article>
            {/each}
          </div>
        </section>
      {/if}
    {/each}
  </div>
{/if}

{#if optIn}
  <Confirm bind:open={optInOpen} title={t('tools.optin_title', { tool: toolName(optIn.id) })} body={t(`tool.optin.${optIn.id}`)} confirmLabel={t('tools.optin_on')} onconfirm={() => quick(optIn.id, 'enabled', true)} />
{/if}

<style>
  .bar {
    display: flex;
    gap: var(--s-3);
    flex-wrap: wrap;
    align-items: center;
    margin-bottom: var(--s-4);
  }
  .search {
    flex: 0 1 280px;
  }
  .mode {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--s-3);
    margin-bottom: var(--s-5);
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .model {
    flex: 1 0 100%;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--s-3);
    padding-top: var(--s-3);
    border-top: 1px solid var(--border);
  }
  .model-select {
    flex: 0 1 280px;
  }
  .mode-text {
    flex: 1 1 260px;
    min-width: 0;
  }
  .mode .gt {
    margin-bottom: 2px;
  }
  .tool {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding: var(--pad-card);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    transition-property: box-shadow, translate;
    transition-duration: var(--dur);
    transition-timing-function: var(--ease);
  }
  @media (hover: hover) {
    .tool:hover {
      box-shadow: var(--shadow-border-hover);
      translate: 0 -2px;
    }
  }
  .cover {
    position: absolute;
    inset: 0;
    border-radius: inherit;
  }
  .cover:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .group {
    margin-bottom: var(--s-6);
  }
  .gt {
    font-size: var(--text-sm);
    font-weight: 650;
    color: var(--text-muted);
    margin: 0 0 var(--s-3);
  }
  .hint {
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--text-muted);
  }
  .more {
    margin-bottom: var(--s-3);
    padding: var(--s-3) var(--s-4);
    border-radius: var(--r-lg);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .more summary {
    cursor: pointer;
  }
  .more .gt {
    display: inline;
  }
  .mini {
    list-style: none;
    margin: var(--s-3) 0 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .mini li {
    display: flex;
    flex-direction: column;
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--s-3);
    flex-wrap: wrap;
  }
  .ic {
    display: grid;
    place-items: center;
    width: 38px;
    height: 38px;
    border-radius: 12px;
    background: var(--accent-soft);
    color: var(--accent-text);
    transition: background-color var(--dur) var(--ease);
  }
  .ic.off {
    background: var(--surface-2);
    color: var(--text-subtle);
  }
  .nm {
    flex: 1;
    min-width: 0;
  }
  .nm h3 {
    font-size: var(--text-md);
  }
  .desc {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
    min-height: 2.8em;
  }
  .usage {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .u-top {
    display: flex;
    justify-content: space-between;
  }
  .cap {
    height: 6px;
    border-radius: 999px;
    background: var(--surface-2);
    overflow: hidden;
  }
  .cap span {
    display: block;
    height: 100%;
    background: var(--accent);
    border-radius: inherit;
    transition: width 600ms var(--ease);
  }
  .cap span.hot {
    background: var(--danger);
  }
  .switches {
    position: relative;
    z-index: 1;
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--s-2);
    padding-top: var(--s-3);
    box-shadow: inset 0 1px 0 var(--border);
    font-size: var(--text-sm);
  }
  .switches :global(.sw) {
    flex-direction: column-reverse;
    align-items: flex-start;
    gap: 6px;
  }
  /* laid out like a Switch (control above, label below) so the three columns line up */
  .na {
    display: inline-flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
    font-size: var(--text-sm);
  }
  .na-v {
    display: flex;
    align-items: center;
    min-height: 22px;
  }
  .na-l {
    color: var(--text-muted);
    font-weight: 550;
  }
</style>
