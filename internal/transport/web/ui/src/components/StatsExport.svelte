<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { call } from '../lib/api.svelte'
  import { useConfig } from '../lib/config.svelte'
  import { toast } from '../lib/toast.svelte'
  import { num } from '../lib/format'
  import { saveFile } from '../lib/download'
  import * as I from '../lib/icons'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Segmented from './Segmented.svelte'
  import Confirm from './Confirm.svelte'

  // stats.export: weekly totals of this install as a file. The complete file is shown first; it is
  // downloaded from this page, never sent by the program (the Action only builds it).
  const cfg = useConfig()
  let weeks = $state<'4' | '12' | '26' | '52'>('12')
  let busy = $state(false)
  let file = $state<any>(null)
  let text = $state('')
  let collecting = $state<boolean | null>(null) // what the last export said; before it, the config
  let resetOpen = $state(false)

  const off = $derived(collecting === null ? cfg.loaded && cfg.doc?.stats?.enabled === false : !collecting)
  const rows = $derived((file?.weekly?.length ?? 0) as number)

  function fromDate(): string {
    return new Date(Date.now() - Number(weeks) * 7 * 86400000).toISOString().slice(0, 10)
  }

  async function build(reset = false) {
    busy = true
    const r = await call('stats.export', { from: fromDate(), ...(reset ? { reset_id: true } : {}) })
    busy = false
    if (r.ok && r.output) {
      file = r.output.file
      text = JSON.stringify(r.output.file, null, 2)
      collecting = r.output.collecting !== false
      if (reset) toast('ok', t('stats.export.reset_done'))
    } else toast('error', t('common.failed'), r.error?.message)
  }

  function download() {
    saveFile(`masamong-stats-${file.period.from || 'none'}-${file.period.to || 'none'}.json`, text + '\n', 'application/json')
  }
</script>

<Card title={t('stats.export.title')} description={t('stats.export.desc')} id="stats-export">
  <div class="stack">
    {#if off}
      <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('stats.export.off')}</span></div>
    {/if}
    <div class="row">
      <Segmented label={t('stats.export.period')} bind:value={weeks} options={['4', '12', '26', '52'].map((v) => ({ value: v as any, label: t('stats.export.weeks', { n: v }) }))} onchange={() => { file = null; text = '' }} />
      <Button variant={file ? 'secondary' : 'primary'} icon={I.FileBarChart} loading={busy} onclick={() => build()}>{t('stats.export.build')}</Button>
    </div>
    <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('stats.export.note_manual')}</span></div>
    <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('stats.export.note_public')}</span></div>
    {#if file}
      <div class="stack enter">
        <p class="small muted">
          {#if rows}{t('stats.export.summary', { from: file.period.from, to: file.period.to, rows: num(rows) })}{:else}{t('stats.export.empty')}{/if}
        </p>
        <textarea class="file mono" readonly rows="16" spellcheck="false" aria-label={t('stats.export.file_label')} value={text}></textarea>
        <p class="xs muted">{t('stats.export.id')}: <span class="mono">{file.install.id}</span></p>
        <div class="row">
          <Button variant="primary" icon={I.Download} onclick={download}>{t('stats.export.download')}</Button>
          <Button variant="ghost" icon={I.KeyRound} onclick={() => (resetOpen = true)}>{t('stats.export.reset')}</Button>
        </div>
      </div>
    {/if}
  </div>
</Card>

<Confirm bind:open={resetOpen} title={t('stats.export.reset_title')} body={t('stats.export.reset_body')} confirmLabel={t('stats.export.reset')} onconfirm={() => build(true)} />

<style>
  .file {
    box-sizing: border-box;
    width: 100%;
    margin: 0;
    padding: var(--s-3);
    border: 0;
    border-radius: var(--r);
    background: var(--surface-2);
    color: var(--text);
    font-size: var(--text-xs);
    line-height: 1.5;
    resize: vertical;
  }
  .file:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
</style>
