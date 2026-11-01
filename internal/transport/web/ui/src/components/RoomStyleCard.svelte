<script lang="ts">
  import { t, has, localeName } from '../lib/i18n.svelte'
  import { resource } from '../lib/resource.svelte'
  import { call } from '../lib/api.svelte'
  import { cfg, patchConfig } from '../lib/config.svelte'
  import { num, pct, usd, datetime } from '../lib/format'
  import { toast } from '../lib/toast.svelte'
  import { pickVersions, statsView, groupItems, dryRunView, previewMessage, PREVIEW_MAX } from '../lib/roomstyle'
  import Card from './Card.svelte'
  import Field from './Field.svelte'
  import Select from './Select.svelte'
  import TextArea from './TextArea.svelte'
  import Button from './Button.svelte'
  import Status from './Status.svelte'
  import Skeleton from './Skeleton.svelte'
  import LoadState from './LoadState.svelte'
  import * as I from '../lib/icons'

  // Room style (experimental, plan §5.6): a short note on how this room talks, added after the persona once
  // an operator has approved it. The numbers are counted in code, only slang, nicknames and jokes come from
  // a model, and every new version waits here as a proposal: analyze (a free count first, then one model
  // call), preview the replies with and without the note, approve or reject. Off is the default and is
  // stored as "unset".
  let { guild, channel }: { guild: string; channel: string } = $props()
  const pol = $derived(cfg.doc?.guilds?.[guild]?.channels?.[channel] ?? {})
  const setting = $derived<string>(pol.conversation?.room_style || 'off')
  const path = $derived(['guilds', guild, 'channels', channel, 'conversation', 'room_style'])
  const input = () => ({ guild_id: guild, channel_id: channel })

  async function choose(v: string) {
    await patchConfig(v === 'off' ? [{ path, op: 'unset' }] : [{ path, value: v }], { success: t('conv.saved') })
  }

  const show = resource<any>('room.style.show', input, { live: ['config'] })
  const view = $derived(pickVersions(show.data?.versions))
  const latest = $derived<any>(show.data?.versions?.[0])

  // 'dry' = counting, 'build' = analyzing with a model, 'preview', 'approve', 'reject'
  let busy = $state('')
  let dry = $state<ReturnType<typeof dryRunView>>(null)
  let message = $state('')
  let preview = $state<{ version: number; plain: string; styled: string } | null>(null)

  const stats = $derived(dry?.stats ?? statsView(latest?.stats))
  const statsFresh = $derived(!!dry?.stats)
  const approvedGroups = $derived(groupItems(view.approved?.items))
  const pendingGroups = $derived(groupItems(view.pending?.items))
  const canPreview = $derived(previewMessage(message) !== '')

  const fail = (r: { error?: { code: string; message: string } }) =>
    toast('error', t('common.failed'), r.error?.code === 'BUDGET_EXCEEDED' ? t('rstyle.budget_spent') : r.error?.message)

  async function analyze() {
    busy = 'dry'
    const r = await call('room.style.build', { ...input(), dry_run: true })
    busy = ''
    if (!r.ok) return fail(r)
    dry = dryRunView(r.output)
  }
  async function build() {
    busy = 'build'
    const r = await call('room.style.build', input())
    busy = ''
    if (!r.ok) return fail(r)
    if (r.output?.enough === false) {
      dry = dryRunView(r.output)
      return toast('warn', t('rstyle.not_enough_toast'))
    }
    dry = null
    preview = null
    await show.refresh()
    toast('ok', t('rstyle.built'), t('rstyle.built_body'))
  }
  async function runPreview() {
    const v = view.pending
    const msg = previewMessage(message)
    if (!v || !msg) return
    busy = 'preview'
    const r = await call('room.style.preview', { ...input(), version: v.version, message: msg })
    busy = ''
    if (!r.ok) return fail(r)
    preview = { version: v.version, plain: String(r.output?.plain ?? ''), styled: String(r.output?.styled ?? '') }
  }
  async function decide(decision: 'approve' | 'reject') {
    const v = view.pending
    if (!v) return
    busy = decision
    const r = await call('room.style.decide', { ...input(), version: v.version, decision })
    busy = ''
    if (!r.ok) return fail(r)
    preview = null
    dry = null
    await show.refresh()
    if (decision === 'reject') toast('info', t('rstyle.rejected'))
    else toast('ok', t('rstyle.approved'), setting === 'on' ? t('rstyle.approved_on') : t('rstyle.approved_off'))
  }

  const FACTS = ['numbers', 'people', 'approval', 'delete', 'weekly']
  const pctOf = (n: number) => pct(n)
  const langName = (tag: string) => localeName(tag)
  const stateLabel = (s: string) => (has(`rstyle.state.${s}`) ? t(`rstyle.state.${s}`) : s)
</script>

{#snippet items(groups: ReturnType<typeof groupItems>)}
  {#if groups.length}
    <div class="groups">
      {#each groups as g (g.kind)}
        <div class="grp">
          <h4 class="xs muted">{t(`rstyle.kind.${g.kind}`)}</h4>
          <ul class="its">
            {#each g.items as it (it.text)}
              <li class="small"><span class="txt">{it.text}</span><span class="chip" title={t('rstyle.evidence_hint')}>{t('rstyle.evidence', { count: num(it.evidence) })}</span></li>
            {/each}
          </ul>
        </div>
      {/each}
    </div>
  {/if}
{/snippet}

<Card title={t('rstyle.title')} description={t('rstyle.desc')}>
  <div class="stack">
    <div class="gh"><Status tone="warn" label={t('conv.experimental')} size="sm" /></div>
    <Field id="rstyle-setting" label={t('rstyle.setting')} hint={t('rstyle.setting.hint')}>
      {#snippet children({ id, describedby })}<Select {id} aria-describedby={describedby} value={setting} onchange={(e) => choose(e.currentTarget.value)} options={['off', 'on'].map((v) => ({ value: v, label: t(`rstyle.setting.${v}`) }))} />{/snippet}
    </Field>
    <ul class="facts small muted">
      {#each FACTS as f (f)}<li>{t(`rstyle.fact.${f}`)}</li>{/each}
    </ul>
    {#if setting === 'on' && show.data && !view.approved}<p class="xs muted">{t('rstyle.on_nothing')}</p>{/if}

    {#if !show.data}
      <LoadState res={show}><Skeleton lines={3} /></LoadState>
    {:else}
      {#if stats}
        <section class="block" aria-labelledby="rstyle-stats-h">
          <h3 class="small" id="rstyle-stats-h">{t('rstyle.stats_title')}</h3>
          <p class="xs muted">{statsFresh ? t('rstyle.stats_fresh', { messages: num(stats.messages), people: num(stats.people) }) : t('rstyle.stats_saved', { messages: num(stats.messages), people: num(stats.people) })}</p>
          <dl class="grid">
            <div><dt class="xs muted">{t('rstyle.stat.median')}</dt><dd class="num">{t('rstyle.chars', { count: num(stats.median) })}</dd></div>
            <div><dt class="xs muted">{t('rstyle.stat.long')}</dt><dd class="num">{t('rstyle.chars', { count: num(stats.long) })}</dd></div>
            <div>
              <dt class="xs muted">{t('rstyle.stat.laughs')}</dt>
              <dd><span class="num">{pctOf(stats.laughShare)}</span>{#if stats.laughs.length}<span class="marks">{#each stats.laughs as l (l)}<span class="chip">{l}</span>{/each}</span>{/if}</dd>
            </div>
            <div><dt class="xs muted">{t('rstyle.stat.emoji')}</dt><dd class="num">{pctOf(stats.emojiShare)}</dd></div>
            <div><dt class="xs muted">{t('rstyle.stat.questions')}</dt><dd class="num">{pctOf(stats.questionShare)}</dd></div>
            <div>
              <dt class="xs muted">{t('rstyle.stat.languages')}</dt>
              <dd>{#if stats.languages.length}<ul class="langs">{#each stats.languages as l (l.tag)}<li><span>{langName(l.tag)}</span> <span class="num muted">{l.pct}%</span></li>{/each}</ul>{:else}<span class="muted">–</span>{/if}</dd>
            </div>
          </dl>
        </section>
      {:else if !view.pending && !view.approved}
        <p class="small muted">{t('rstyle.none')}</p>
      {/if}

      {#if view.stale && !view.pending}
        <div class="callout warn" role="status"><span class="ic"><I.TriangleAlert size={16} aria-hidden="true" /></span><span>{t('rstyle.stale')}</span></div>
      {/if}

      <div class="actions">
        <Button variant={view.stale || (!view.pending && !view.approved) ? 'primary' : 'secondary'} icon={I.Sparkles} loading={busy === 'dry'} disabled={busy !== '' && busy !== 'dry'} onclick={analyze}>{view.stale ? t('rstyle.analyze_again') : t('rstyle.analyze')}</Button>
      </div>

      {#if dry}
        <div class="callout {dry.enough ? 'ok' : 'warn'}" role="status">
          <span class="ic">{#if dry.enough}<I.CircleCheck size={16} aria-hidden="true" />{:else}<I.TriangleAlert size={16} aria-hidden="true" />{/if}</span>
          <span>{dry.enough ? t('rstyle.enough', { count: num(dry.messages) }) : t('rstyle.too_few', { count: num(dry.messages), min: num(dry.min) })}</span>
        </div>
        {#if dry.enough}
          <div class="confirm">
            <p class="xs muted">{t('rstyle.cost_note')}</p>
            <Button variant="primary" icon={I.Sparkles} loading={busy === 'build'} disabled={busy !== '' && busy !== 'build'} onclick={build}>{t('rstyle.build')}</Button>
          </div>
        {/if}
      {/if}

      {#if view.pending}
        {@const p = view.pending}
        <section class="block" aria-labelledby="rstyle-pending-h">
          <div class="gh"><h3 class="small" id="rstyle-pending-h">{t('rstyle.pending_title')}</h3><Status tone="warn" label={stateLabel('pending')} size="sm" /></div>
          <p class="xs muted">{t('rstyle.pending_meta', { messages: num(p.messages), cost: usd(p.cost_usd ?? 0, 4), date: datetime(p.created_at) })}</p>
          {#if p.note}<blockquote class="note small">{p.note}</blockquote>{/if}
          {@render items(pendingGroups)}

          <div class="try">
            <Field id="rstyle-msg" label={t('rstyle.try')} hint={t('rstyle.try_hint')}>
              {#snippet children({ id, describedby })}<TextArea {id} aria-describedby={describedby} bind:value={message} rows={2} maxlength={PREVIEW_MAX} placeholder={t('rstyle.try_ph')} />{/snippet}
            </Field>
            <div class="actions"><Button variant="secondary" icon={I.Play} loading={busy === 'preview'} disabled={!canPreview || (busy !== '' && busy !== 'preview')} onclick={runPreview}>{t('rstyle.preview')}</Button></div>
            <div aria-live="polite">
              {#if busy === 'preview'}<Skeleton lines={3} />
              {:else if preview && preview.version === p.version}
                <div class="cmp">
                  <div class="ans"><h4 class="xs muted">{t('rstyle.without')}</h4><p class="small pre">{preview.plain || t('rstyle.no_reply')}</p></div>
                  <div class="ans styled"><h4 class="xs muted">{t('rstyle.with')}</h4><p class="small pre">{preview.styled || t('rstyle.no_reply')}</p></div>
                </div>
              {/if}
            </div>
          </div>

          <div class="actions">
            <Button variant="primary" icon={I.Check} loading={busy === 'approve'} disabled={busy !== '' && busy !== 'approve'} onclick={() => decide('approve')}>{t('rstyle.approve')}</Button>
            <Button variant="ghost" icon={I.X} loading={busy === 'reject'} disabled={busy !== '' && busy !== 'reject'} onclick={() => decide('reject')}>{t('rstyle.reject')}</Button>
          </div>
        </section>
      {/if}

      {#if view.approved}
        {@const a = view.approved}
        <section class="block" aria-labelledby="rstyle-approved-h">
          <div class="gh"><h3 class="small" id="rstyle-approved-h">{t('rstyle.approved_title')}</h3><Status tone="ok" label={stateLabel('approved')} size="sm" /></div>
          <p class="xs muted">{t('rstyle.approved_meta', { date: datetime(a.decided_at) })}{#if setting !== 'on'}{' '}{t('rstyle.approved_unused')}{/if}</p>
          {#if a.note}<blockquote class="note small">{a.note}</blockquote>{/if}
          {@render items(approvedGroups)}
        </section>
      {/if}

      {#if view.history.length}
        <details class="hist">
          <summary class="small">{t('rstyle.history', { count: num(view.history.length) })}</summary>
          <ul class="hl">
            {#each view.history as h (h.version)}
              <li class="xs"><span>{t('rstyle.version', { count: h.version })}</span> <span class="muted">· {stateLabel(h.state)}{#if h.created_at} · {datetime(h.created_at)}{/if}</span></li>
            {/each}
          </ul>
        </details>
      {/if}
    {/if}
  </div>
</Card>

<style>
  .gh {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .gh h3,
  .block h3 {
    margin: 0;
  }
  .block {
    display: grid;
    gap: var(--s-3);
    padding-block-start: var(--s-3);
    border-block-start: 1px solid var(--border);
    min-width: 0;
  }
  .block p,
  .confirm p {
    margin: 0;
  }
  .facts {
    margin: 0;
    padding-inline-start: 1.2em;
    display: grid;
    gap: 2px;
  }
  .grid {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 140px), 1fr));
    margin: 0;
  }
  .grid > div {
    display: grid;
    gap: 2px;
    align-content: start;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    min-width: 0;
  }
  .grid dd {
    margin: 0;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .marks {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-inline-start: var(--s-2);
    vertical-align: middle;
  }
  .langs {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 2px;
  }
  .actions {
    display: flex;
    gap: var(--s-2);
    flex-wrap: wrap;
  }
  .confirm {
    display: grid;
    gap: var(--s-2);
    justify-items: start;
  }
  .note {
    margin: 0;
    padding: var(--s-3);
    border-inline-start: 3px solid var(--border-strong, var(--border));
    border-radius: var(--r);
    background: var(--surface-2);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .groups {
    display: grid;
    gap: var(--s-3);
  }
  .grp h4,
  .ans h4 {
    margin: 0 0 4px;
    font-weight: 600;
  }
  .its {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .its li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--s-2);
  }
  .txt {
    overflow-wrap: anywhere;
    min-width: 0;
  }
  .try {
    display: grid;
    gap: var(--s-2);
  }
  /* side by side where there is room, stacked on a phone */
  .cmp {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 240px), 1fr));
  }
  .ans {
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    min-width: 0;
  }
  .ans.styled {
    box-shadow: inset 0 0 0 1.5px var(--accent);
  }
  .ans p {
    margin: 0;
  }
  .pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .hist summary {
    cursor: pointer;
  }
  .hl {
    list-style: none;
    margin: var(--s-2) 0 0;
    padding: 0;
    display: grid;
    gap: 2px;
  }
</style>
