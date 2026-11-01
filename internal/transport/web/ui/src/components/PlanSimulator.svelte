<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call, can } from '../lib/api.svelte'
  import { scrollable } from '../lib/scrollable'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Select from './Select.svelte'
  import TextArea from './TextArea.svelte'
  import Status from './Status.svelte'
  import CopyButton from './CopyButton.svelte'
  import * as I from '../lib/icons'

  // agent.simulate: check a tool plan (nodes and what each depends on) against built-in fake outcomes. It
  // never calls a real tool or model, so it is free and safe on a live instance; the Lab hosts it because
  // it is the same "try it without Discord" idea as the chat tab.
  const sample = JSON.stringify({ nodes: [{ id: 'n1', need: 'quote', depends_on: [], cost: 1 }, { id: 'n2', need: 'fx', depends_on: ['n1'], cost: 1 }] }, null, 2)
  const fixtures = ['ok', 'fx_fail', 'timeout', '401', '429']
  let plan = $state(sample)
  let fixture = $state('ok')
  let running = $state(false)
  let parseError = $state('')
  let out = $state<any>(null)
  let failure = $state('')

  async function run() {
    if (running) return
    // the table below always belongs to the plan that is in the box: drop the last result before anything
    // can fail, so a broken edit never sits above an old "would answer" table
    parseError = failure = ''
    out = null
    let parsed: unknown
    try {
      parsed = JSON.parse(plan)
    } catch {
      return void (parseError = t('lab.plan.bad_json'))
    }
    running = true
    const r = await call('agent.simulate', { plan: parsed, fixture })
    running = false
    if (r.ok) out = r.output
    else failure = r.error?.message ?? t('common.failed')
  }
  const tone = (r: string) => (r === 'answer' ? 'ok' : r === 'partial' ? 'warn' : r === 'error' ? 'danger' : 'neutral')
  // the reasons agent.simulate gives for its fixtures; an unknown one is shown as the technical code it is
  const reasonKnown = (r: string) => ['quote_without_fx', 'timeout_unknown_not_retry', '401', '429'].includes(r)
  const label = (group: string, v: string) => (['ok', 'failed', 'unknown', 'degraded', 'retry_wait', 'answer', 'partial', 'silent', 'clarification', 'proposal', 'error'].includes(v) ? t(`lab.plan.${group}.${v}`) : v)
</script>

<div class="stack">
  <Card title={t('lab.plan.title')} description={t('lab.plan.desc')}>
    {#snippet actions()}<Button variant="primary" icon={I.Play} loading={running} disabled={!can('agent.simulate')} onclick={run}>{t('lab.plan.run')}</Button>{/snippet}
    <div class="stack">
      <div>
        <label class="lbl" for="plan-json">{t('lab.plan.input')}</label>
        <TextArea id="plan-json" mono rows={9} bind:value={plan} invalid={!!parseError} aria-describedby="plan-hint" spellcheck="false" />
        <p id="plan-hint" class="xs muted" class:err={!!parseError}>{parseError || t('lab.plan.input_hint')}</p>
      </div>
      <div class="fx">
        <label class="lbl" for="plan-fixture">{t('lab.plan.fixture')}</label>
        <Select id="plan-fixture" bind:value={fixture} options={fixtures.map((v) => ({ value: v, label: t(`lab.plan.fx.${v}`) }))} />
      </div>
      <div><Button size="sm" variant="ghost" onclick={() => (plan = sample)}>{t('lab.plan.sample')}</Button></div>
    </div>
  </Card>

  {#if failure}
    <div class="callout danger" role="alert"><span class="ic"><I.CircleAlert size={16} /></span><span>{t('lab.plan.rejected')} {failure}</span></div>
  {/if}
  {#if out}
    <Card padded={false} title={t('lab.plan.result')} class="enter">
      {#snippet actions()}<Status tone={tone(out.result)} label={label('result', out.result)} size="sm" />{/snippet}
      <div use:scrollable class="table-scroll">
        <table class="table">
          <thead><tr><th>{t('lab.plan.col.node')}</th><th>{t('lab.plan.col.need')}</th><th>{t('lab.plan.col.after')}</th><th>{t('lab.plan.col.status')}</th></tr></thead>
          <tbody>
            {#each out.nodes ?? [] as n (n.id)}
              <tr><td class="mono">{n.id}</td><td>{n.need}</td><td class="small muted">{(n.depends_on ?? []).join(', ') || '–'}</td><td><Status tone={n.status === 'ok' ? 'ok' : n.status === 'unknown' ? 'neutral' : 'warn'} label={label('st', n.status)} size="sm" /></td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="foot small muted">
        {#if out.reason}<span>{t('lab.plan.reason')}: {#if reasonKnown(out.reason)}{t(`lab.plan.rs.${out.reason}`)} {/if}<code class="mono" title={t('lab.plan.code_hint')}>{out.reason}</code></span>{/if}
        <span>{t('lab.plan.evidence', { n: (out.evidence ?? []).length })}</span>
        {#if out.trace_id}<span class="trace">{t('lab.plan.trace')}: <code class="mono">{out.trace_id}</code><CopyButton text={out.trace_id} /></span>{/if}
      </div>
    </Card>
  {/if}
</div>

<style>
  .lbl {
    display: block;
    margin-bottom: 4px;
    font-size: var(--text-sm);
    font-weight: 600;
  }
  .fx {
    max-width: 320px;
  }
  .err {
    color: var(--danger);
  }
  .foot {
    display: flex;
    flex-wrap: wrap;
    gap: var(--s-2) var(--s-4);
    padding: var(--s-3) var(--pad-card);
  }
  .trace {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
</style>
