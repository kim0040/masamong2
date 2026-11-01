<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { can } from '../lib/api.svelte'
  import { resource } from '../lib/resource.svelte'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import { num } from '../lib/format'
  import { DM_ALLOW, DM_USER, DM_DAY, dmDraft, dmErrors, dmOps, dmOrderWarning, type DMDraft, type LimitMode } from '../lib/dmsettings'
  import Card from './Card.svelte'
  import Button from './Button.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import KeyValue from './KeyValue.svelte'
  import Segmented from './Segmented.svelte'
  import * as I from '../lib/icons'

  // Conversation in Discord DMs (config `dm`): who may chat there and how many chats a day. It sits with
  // the access lists. Commands keep working in DMs whatever is chosen here.
  useConfig()
  const info = resource<any>('tools.list', () => ({}), { live: ['config', 'turn'] })
  const raw = $derived(cfg.doc?.dm)
  const rawKey = $derived(JSON.stringify(raw ?? null))

  let draft = $state<DMDraft>(dmDraft(undefined))
  let base = $state('')
  let baseRev = $state(0)
  $effect(() => {
    if (!cfg.loaded || rawKey === base) return
    draft = dmDraft(raw)
    base = rawKey
    baseRev = cfg.revision
  })

  const errors = $derived(dmErrors(draft))
  const ops = $derived(cfg.loaded ? dmOps(draft, raw) : [])
  const dirty = $derived(ops.length > 0)
  const invalid = $derived(!!errors.user || !!errors.day)
  let saving = $state(false)
  async function save() {
    if (invalid || !dirty) return
    saving = true
    try {
      const r = await patchConfig(ops, { success: t('dm.saved'), base: baseRev })
      // a conflict reloaded the newest settings; the form follows them and the admin chooses again
      if (!r.ok) base = ''
    } finally {
      saving = false
    }
    void info.refresh()
  }
  function revert() {
    draft = dmDraft(raw)
  }

  const limits = [
    { key: 'user', def: DM_USER, label: 'dm.user', hint: 'dm.user_hint' },
    { key: 'day', def: DM_DAY, label: 'dm.day', hint: 'dm.day_hint' },
  ] as const
  const modeOptions = (def: number) => [
    { value: 'default' as LimitMode, label: t('dm.mode.default', { n: num(def) }) },
    { value: 'custom' as LimitMode, label: t('dm.mode.custom') },
    { value: 'none' as LimitMode, label: t('dm.mode.none') },
  ]

  // what the server counts today (effective values, so the numbers are what Discord enforces)
  const live = $derived(info.data?.dm)
  const usedText = $derived(live ? (live.per_day === -1 ? t('dm.used_nolimit', { used: num(live.used_today ?? 0) }) : t('dm.used_of', { used: num(live.used_today ?? 0), cap: num(live.per_day) })) : '')
</script>

<Card title={t('dm.title')} description={t('dm.desc')}>
  {#if !cfg.loaded}
    <p class="small muted">{cfg.error ? cfg.error : t('common.loading')}</p>
  {:else}
    <form class="stack" onsubmit={(e) => { e.preventDefault(); void save() }}>
      <fieldset class="group">
        <legend class="small h">{t('dm.allow')}</legend>
        {#each DM_ALLOW as v (v)}
          <label class="opt" class:on={draft.allow === v}>
            <input type="radio" name="dm-allow" value={v} checked={draft.allow === v} disabled={!can('config.patch')} onchange={() => (draft.allow = v)} />
            <span class="txt"><span class="small name">{t(`dm.allow.${v}`)}</span><span class="xs muted">{t(`dm.allow.${v}_hint`)}</span></span>
          </label>
        {/each}
      </fieldset>

      {#each limits as l (l.key)}
        {@const lim = draft[l.key]}
        <div class="group" role="group" aria-labelledby="dm-{l.key}-t">
          <div class="small h" id="dm-{l.key}-t">{t(l.label)}</div>
          <p class="xs muted">{t(l.hint)}</p>
          <Segmented size="sm" label={t(l.label)} value={lim.mode} onchange={(v) => (lim.mode = v)} options={modeOptions(l.def.default)} />
          {#if lim.mode === 'custom'}
            <Field id="dm-{l.key}-n" label={t('dm.count')} hint={t('dm.count_hint', { max: num(l.def.max) })} error={errors[l.key] ? t('dm.err.range', { max: num(l.def.max) }) : undefined}>
              {#snippet children({ id, describedby, invalid })}
                <div class="num"><Input {id} aria-describedby={describedby} {invalid} type="number" min="1" max={l.def.max} step="1" inputmode="numeric" bind:value={lim.n} /><span class="muted small">{t('dm.unit')}</span></div>
              {/snippet}
            </Field>
          {/if}
        </div>
      {/each}

      {#if dmOrderWarning(draft)}
        <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('dm.order')}</span></div>
      {/if}

      <div class="row">
        <Button type="submit" variant="primary" icon={I.Check} loading={saving} disabled={!dirty || invalid || !can('config.patch')}>{t('common.save')}</Button>
        <Button variant="ghost" icon={I.RotateCcw} disabled={!dirty || saving} onclick={revert}>{t('dm.revert')}</Button>
      </div>
    </form>

    {#if live}
      <div class="usage">
        <KeyValue items={[{ k: t('dm.used'), v: usedText }, ...(info.data?.day ? [{ k: t('dm.used_day'), v: info.data.day }] : [])]} />
      </div>
    {/if}
    <ul class="notes xs muted">
      <li>{t('dm.note.reset')}</li>
      <li>{t('dm.note.stack')}</li>
      <li>{t('dm.note.commands')}</li>
      <li>{t('dm.note.memory')}</li>
      <li>{t('dm.note.refused')}</li>
    </ul>
  {/if}
</Card>

<style>
  .group {
    display: grid;
    gap: 6px;
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .group p {
    margin: 0;
  }
  .h {
    margin: 0 0 2px;
    padding: 0;
    font-weight: 600;
  }
  .opt {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    padding: 10px 12px;
    border-radius: var(--r);
    background: var(--surface-2);
    box-shadow: inset 0 0 0 1px var(--border);
    cursor: pointer;
  }
  .opt.on {
    box-shadow: inset 0 0 0 2px var(--accent);
  }
  .opt:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .opt input {
    margin: 3px 0 0;
    flex: none;
    accent-color: var(--accent);
  }
  .txt {
    display: grid;
    gap: 2px;
    min-width: 0;
  }
  .name {
    font-weight: 550;
  }
  .num {
    display: flex;
    align-items: center;
    gap: var(--s-2);
    max-width: 220px;
  }
  .usage {
    margin-top: var(--s-3);
    padding-top: var(--s-3);
    border-top: 1px solid var(--border);
    display: grid;
    gap: 6px;
  }
  .notes {
    margin: var(--s-3) 0 0;
    padding-inline-start: 18px;
    display: grid;
    gap: 4px;
  }
</style>
