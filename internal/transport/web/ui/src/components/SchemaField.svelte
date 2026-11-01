<script lang="ts">
  import Self from './SchemaField.svelte'
  import Switch from './Switch.svelte'
  import Segmented from './Segmented.svelte'
  import Select from './Select.svelte'
  import TextArea from './TextArea.svelte'
  import Button from './Button.svelte'
  import { t, has, localeName } from '../lib/i18n.svelte'
  import { session } from '../lib/api.svelte'
  import { schemaExample, type Schema } from '../lib/config.svelte'
  import CopyButton from './CopyButton.svelte'
  import { RotateCcw, Plus, X, ArrowRight } from '../lib/icons'

  // One node of the config document, rendered from its schema. Edits are reported as (path, value);
  // undefined means "remove the key" (back to the built-in default). Every kind of value gets a
  // real control; raw JSON is only the fallback for shapes with no form, and for the sections a page
  // of their own manages (behind "edit directly").
  let { schema, value, def, path, onedit, depth = 0, filter = '' }: { schema: Schema; value: any; def: any; path: string[]; onedit: (path: string[], value: any) => void; depth?: number; filter?: string } = $props()
  const key = $derived(path.join('.'))
  const name = $derived(path[path.length - 1] ?? '')
  const labelKey = $derived('cfg.' + path.filter((p) => !/^\d+$/.test(p)).join('.'))
  const label = $derived(has(labelKey) ? t(labelKey) : name.replace(/_/g, ' '))
  const hintKey = $derived(labelKey + '.hint')
  const differs = $derived(JSON.stringify(value ?? null) !== JSON.stringify(def ?? null))
  const uid = $derived('f-' + key.replace(/[^a-z0-9]/gi, '-'))
  const scalar = $derived(['string', 'int', 'float', 'bool', 'any'].includes(schema.t))
  // legacy fields show only while they still hold a value
  const hiddenLegacy = $derived(!!schema.legacy && (value === undefined || value === null || value === false || (Array.isArray(value) && value.length === 0)))
  const visible = $derived(!hiddenLegacy && (!filter || key.toLowerCase().includes(filter.toLowerCase()) || label.toLowerCase().includes(filter.toLowerCase()) || !scalar))
  let jsonErr = $state('')
  let numErr = $state('')

  // sections that have a page of their own: the form links there
  const pages: Record<string, { href: string; nav: string }> = {
    providers: { href: '/connections', nav: 'nav.connections' },
    models: { href: '/connections', nav: 'nav.connections' },
    tools: { href: '/tools', nav: 'nav.tools' },
    guilds: { href: '/rooms', nav: 'nav.rooms' },
    personas: { href: '/persona', nav: 'nav.persona' },
  }
  const page = $derived(schema.t === 'map' && path.length === 1 ? pages[path[0]] : undefined)
  const mapOf = $derived(schema.t === 'map' ? schema.elem : undefined)
  const boolMap = $derived(mapOf?.t === 'bool')
  const listMap = $derived(mapOf?.t === 'array' && ['string', 'int', 'float'].includes(mapOf.elem?.t ?? ''))
  const scalarMap = $derived(!!mapOf && ['string', 'int', 'float'].includes(mapOf.t))
  const scalarList = $derived(schema.t === 'array' && schema.elem && ['string', 'int', 'float'].includes(schema.elem.t))

  const shown = (v: unknown) => (typeof v === 'boolean' ? (v ? t('common.on') : t('common.off')) : String(v))
  const placeholder = $derived(schema.implied !== undefined && schema.implied !== null ? t('settings.default_is', { v: shown(schema.implied) }) : def !== undefined && def !== null && def !== '' ? t('settings.default_is', { v: shown(def) }) : '')
  const enumLabel = (v: string) => (has(`${labelKey}.v.${v}`) ? t(`${labelKey}.v.${v}`) : v)
  const fmtHour = (h: number) => `${String(h).padStart(2, '0')}:00`
  // IANA zones the browser knows, for the time zone fields' suggestions
  const zones: string[] = (() => {
    try {
      return (Intl as any).supportedValuesOf?.('timeZone') ?? []
    } catch {
      return []
    }
  })()
  const localeOptions = $derived([...new Set([...(session.value?.locales ?? ['ko', 'en']), ...(value ? [String(value)] : [])])].map((l) => ({ value: l, label: `${localeName(l)} (${l})` })))
  function usd(e: Event) {
    const raw = (e.currentTarget as HTMLInputElement).value.trim().replace(/^\$/, '')
    numErr = ''
    if (raw === '') return onedit(path, undefined)
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0) return void (numErr = t('settings.usd_invalid'))
    onedit(path, raw)
  }

  function num(e: Event) {
    const raw = (e.currentTarget as HTMLInputElement).value
    numErr = ''
    if (raw === '') return onedit(path, undefined)
    const n = schema.t === 'int' ? parseInt(raw, 10) : parseFloat(raw)
    if (Number.isNaN(n)) return
    if ((schema.min !== undefined && n < schema.min) || (schema.max !== undefined && n > schema.max)) return void (numErr = t('settings.range', { min: schema.min ?? '', max: schema.max ?? '' }))
    onedit(path, n)
  }
  function json(e: Event) {
    const raw = (e.currentTarget as HTMLTextAreaElement).value.trim()
    jsonErr = ''
    if (!raw) return onedit(path, undefined)
    try {
      onedit(path, JSON.parse(raw))
    } catch {
      jsonErr = t('settings.json_invalid')
    }
  }
  // what a JSON value of this field looks like: a real entry from the defaults when there is one,
  // else one built from the schema
  const example = $derived.by(() => {
    if (schema.t === 'map' && def && typeof def === 'object' && Object.keys(def).length) {
      const k = Object.keys(def)[0]
      return JSON.stringify({ [k]: def[k] }, null, 2)
    }
    return JSON.stringify(schemaExample(schema), null, 2)
  })
  const splitList = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean)
  const cast = (x: string, et?: string) => (et === 'int' || et === 'float' ? Number(x) : x)

  // map editors: rows of key → value; a key is renamed or removed in place, a new one is added below
  const entries = $derived(Object.entries((value ?? {}) as Record<string, any>))
  let newKey = $state('')
  let newVal = $state('')
  function putMap(next: Record<string, any>) {
    onedit(path, Object.keys(next).length ? next : undefined)
  }
  function rowValue(v: any) {
    return listMap ? (v ?? []).join(', ') : v ?? ''
  }
  function parseRow(raw: string) {
    return listMap ? splitList(raw).map((x) => cast(x, mapOf?.elem?.t)) : cast(raw.trim(), mapOf?.t)
  }
  function editRow(i: number, k: string, raw: string) {
    const next: Record<string, any> = {}
    entries.forEach(([ek, ev], j) => {
      if (j !== i) next[ek] = ev
      else if (k.trim()) next[k.trim()] = parseRow(raw)
    })
    putMap(next)
  }
  function addRow(e: SubmitEvent) {
    e.preventDefault()
    if (!newKey.trim()) return
    putMap({ ...(value ?? {}), [newKey.trim()]: parseRow(newVal) })
    newKey = ''
    newVal = ''
  }
  // bool maps (feature switches): every known key, from the value and the defaults
  const boolKeys = $derived([...new Set([...Object.keys(def ?? {}), ...Object.keys(value ?? {})])].sort())
  const boolLabel = (k: string) => (has(`${labelKey}.${k}`) ? t(`${labelKey}.${k}`) : key === 'features' && has(`feature.${k}`) ? t(`feature.${k}`) : k.replace(/_/g, ' '))
</script>

{#if schema.t === 'object' && schema.fields}
  {#if depth === 0}
    {#each schema.fields as f (f.k)}
      <Self schema={f.s} value={value?.[f.k]} def={def?.[f.k]} path={[...path, f.k]} {onedit} depth={depth + 1} {filter} />
    {/each}
  {:else}
    <fieldset class="obj" class:deep={depth > 1}>
      <legend>{label}</legend>
      {#each schema.fields as f (f.k)}
        <Self schema={f.s} value={value?.[f.k]} def={def?.[f.k]} path={[...path, f.k]} {onedit} depth={depth + 1} {filter} />
      {/each}
    </fieldset>
  {/if}
{:else if page}
  <div class="managed">
    <p class="small">{t('settings.managed_on', { page: t(page.nav), n: entries.length })}</p>
    <div><Button href={page.href} variant="primary" iconEnd={ArrowRight}>{t('settings.open_page', { page: t(page.nav) })}</Button></div>
    <details>
      <summary class="xs muted">{t('settings.edit_direct')}</summary>
      <div class="ctl" style:margin-top="8px">
        <TextArea id={uid} mono rows={Math.min(18, Math.max(4, JSON.stringify(value ?? {}, null, 2).split('\n').length))} value={value === undefined ? '' : JSON.stringify(value, null, 2)} placeholder={example} onchange={json} invalid={!!jsonErr} spellcheck={false} aria-label={label} />
        {#if jsonErr}<span class="xs err">{jsonErr}</span>{:else}<span class="xs muted">{t('settings.json_hint')}</span>{/if}
        <details class="fmt"><summary class="xs muted">{t('settings.json_example')}</summary><div class="ex"><pre>{example}</pre><CopyButton text={example} /></div></details>
      </div>
    </details>
  </div>
{:else if visible}
  <div class="fld" class:changed={differs} class:wide={boolMap || listMap || scalarMap}>
    <div class="lab">
      <label for={boolMap ? undefined : uid}>{label}</label>
      <code class="xs subtle">{key}</code>
      {#if has(hintKey)}<p class="xs muted">{t(hintKey)}</p>{/if}
    </div>
    <div class="ctl">
      {#if schema.readonly}
        <span class="ro small break" id={uid}>{value === undefined || value === '' ? '—' : shown(value)}</span>
        <span class="xs muted">{t('settings.readonly')}</span>
      {:else if schema.t === 'bool' && schema.nullable}
        <Segmented size="sm" label={label} value={value === true ? 'on' : value === false ? 'off' : 'default'} onchange={(v) => onedit(path, v === 'default' ? undefined : v === 'on')} options={[{ value: 'default', label: schema.implied !== undefined ? t('settings.default_with', { v: shown(schema.implied) }) : t('common.default') }, { value: 'on', label: t('common.on') }, { value: 'off', label: t('common.off') }]} />
      {:else if schema.t === 'bool'}
        <Switch checked={!!value} onchange={(v) => onedit(path, v)} label={value ? t('common.on') : t('common.off')} ariaLabel={label} />
      {:else if schema.enum?.length}
        <Select id={uid} value={value ?? ''} onchange={(e) => onedit(path, (e.currentTarget as HTMLSelectElement).value || undefined)} options={[{ value: '', label: schema.implied !== undefined ? t('settings.default_with', { v: enumLabel(String(schema.implied)) }) : t('common.default') }, ...schema.enum.map((v) => ({ value: v, label: enumLabel(v) }))]} />
      {:else if schema.format === 'weekday'}
        <Select id={uid} value={String(value ?? 0)} onchange={(e) => onedit(path, Number((e.currentTarget as HTMLSelectElement).value))} options={[0, 1, 2, 3, 4, 5, 6].map((d) => ({ value: String(d), label: t(`weekday.${d}`) }))} />
      {:else if schema.format === 'hour'}
        <Select id={uid} value={String(value ?? 0)} onchange={(e) => onedit(path, Number((e.currentTarget as HTMLSelectElement).value))} options={Array.from({ length: 24 }, (_, h) => ({ value: String(h), label: fmtHour(h) }))} />
      {:else if schema.format === 'time'}
        <div class="inl">
          <input id={uid} class="inp time" type="time" value={value ?? ''} onchange={(e) => onedit(path, (e.currentTarget as HTMLInputElement).value || undefined)} />
          {#if value}<button type="button" class="reset" onclick={() => onedit(path, undefined)} aria-label={t('settings.clear')}><X size={14} aria-hidden="true" /></button>{/if}
        </div>
        {#if !value}<span class="xs muted">{t('settings.time_empty')}</span>{/if}
      {:else if schema.format === 'timezone'}
        <input id={uid} class="inp" list="{uid}-z" value={value ?? ''} {placeholder} autocomplete="off" spellcheck={false} onchange={(e) => onedit(path, (e.currentTarget as HTMLInputElement).value.trim() || undefined)} />
        <datalist id="{uid}-z">{#each zones as z}<option value={z}></option>{/each}</datalist>
        <span class="xs muted">{t('settings.tz_hint')}</span>
      {:else if schema.format === 'locale'}
        <Select id={uid} value={value ?? ''} onchange={(e) => onedit(path, (e.currentTarget as HTMLSelectElement).value || undefined)} options={localeOptions} />
      {:else if schema.format === 'usd'}
        <span class="money"><span class="cur" aria-hidden="true">US$</span><input id={uid} class="inp num" inputmode="decimal" value={value ?? ''} {placeholder} onchange={usd} aria-invalid={numErr ? true : undefined} /></span>
        {#if numErr}<span class="xs err" role="alert">{numErr}</span>{/if}
      {:else if schema.format === 'multiline'}
        <TextArea id={uid} rows={4} value={value ?? ''} {placeholder} onchange={(e) => onedit(path, (e.currentTarget as HTMLTextAreaElement).value.trim() || undefined)} />
      {:else if schema.t === 'int' || schema.t === 'float'}
        <input id={uid} class="inp num" type="number" step={schema.t === 'int' ? 1 : 'any'} min={schema.min} max={schema.max} value={value ?? ''} {placeholder} onchange={num} aria-invalid={numErr ? true : undefined} />
        {#if numErr}<span class="xs err" role="alert">{numErr}</span>{/if}
      {:else if schema.t === 'string'}
        <input id={uid} class="inp" value={value ?? ''} {placeholder} onchange={(e) => onedit(path, (e.currentTarget as HTMLInputElement).value === '' ? undefined : (e.currentTarget as HTMLInputElement).value)} />
      {:else if scalarList}
        <input id={uid} class="inp" value={(value ?? []).join(', ')} placeholder={placeholder || t('settings.comma_list_ph')} onchange={(e) => { const list = splitList((e.currentTarget as HTMLInputElement).value); onedit(path, list.length ? list.map((x) => cast(x, schema.elem!.t)) : undefined) }} aria-describedby="{uid}-l" />
        <span id="{uid}-l" class="xs muted">{t('settings.comma_list')}</span>
      {:else if boolMap}
        <div class="switches" role="group" aria-label={label}>
          {#each boolKeys as k (k)}
            <Switch checked={(value ?? {})[k] ?? (def ?? {})[k] ?? true} onchange={(v) => putMap({ ...(value ?? {}), [k]: v })} label={boolLabel(k)} />
          {/each}
          {#if boolKeys.length === 0}<span class="small muted">{t('settings.map_empty')}</span>{/if}
        </div>
      {:else if listMap || scalarMap}
        <div class="rows">
          {#each entries as [k, v], i (i)}
            <div class="mrow">
              <input class="inp" value={k} aria-label={t('settings.map_key', { field: label })} onchange={(e) => editRow(i, (e.currentTarget as HTMLInputElement).value, rowValue(v))} />
              <span class="arrow" aria-hidden="true">→</span>
              <input class="inp" value={rowValue(v)} aria-label={t('settings.map_value', { key: k })} onchange={(e) => editRow(i, k, (e.currentTarget as HTMLInputElement).value)} />
              <button type="button" class="reset" onclick={() => editRow(i, '', '')} aria-label={t('settings.map_remove', { key: k })}><X size={14} aria-hidden="true" /></button>
            </div>
          {/each}
          <form class="mrow" onsubmit={addRow}>
            <input id={uid} class="inp" bind:value={newKey} placeholder={has(`${labelKey}.key_ph`) ? t(`${labelKey}.key_ph`) : t('settings.map_key_ph')} aria-label={t('settings.map_key', { field: label })} />
            <span class="arrow" aria-hidden="true">→</span>
            <input class="inp" bind:value={newVal} placeholder={has(`${labelKey}.value_ph`) ? t(`${labelKey}.value_ph`) : listMap ? t('settings.comma_list_ph') : t('settings.map_value_ph')} aria-label={t('settings.map_value', { key: newKey })} />
            <Button type="submit" size="sm" icon={Plus} disabled={!newKey.trim()}>{t('settings.map_add')}</Button>
          </form>
        </div>
      {:else}
        <TextArea id={uid} mono rows={Math.min(14, Math.max(3, JSON.stringify(value ?? {}, null, 2).split('\n').length))} value={value === undefined ? '' : JSON.stringify(value, null, 2)} placeholder={example} onchange={json} invalid={!!jsonErr} spellcheck={false} />
        {#if jsonErr}<span class="xs err">{jsonErr}</span>{:else}<span class="xs muted">{t('settings.json_hint')}</span>{/if}
        <details class="fmt"><summary class="xs muted">{t('settings.json_example')}</summary><div class="ex"><pre>{example}</pre><CopyButton text={example} /></div></details>
      {/if}
    </div>
    <div class="st">
      {#if differs && !schema.readonly}
        <span class="chip accent">{t('settings.changed')}</span>
        <button type="button" class="reset" onclick={() => onedit(path, undefined)} title={t('settings.reset_default')} aria-label="{t('settings.reset_default')}: {label}"><RotateCcw size={14} aria-hidden="true" /></button>
      {/if}
    </div>
  </div>
{/if}

<style>
  .obj {
    margin: 0 0 var(--s-4);
    padding: var(--s-3) var(--s-4) var(--s-2);
    border: 0;
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .obj.deep {
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  legend {
    float: left;
    width: 100%;
    padding: 0 0 var(--s-2);
    font-weight: 650;
    font-size: var(--text-sm);
  }
  .fld {
    clear: both;
    display: grid;
    grid-template-columns: minmax(180px, 280px) minmax(0, 1fr) 110px;
    gap: var(--s-3);
    align-items: start;
    padding: 12px 0;
    box-shadow: inset 0 -1px 0 var(--border);
  }
  .fld.changed .lab label::before {
    content: '';
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-inline-end: 6px;
    border-radius: 999px;
    background: var(--accent);
    vertical-align: middle;
  }
  .lab {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .lab label {
    font-weight: 600;
    font-size: var(--text-sm);
    text-transform: none;
  }
  .lab code {
    overflow-wrap: anywhere;
  }
  /* every control shares one width so the column reads as a form, not a ragged list */
  .ctl {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
    max-width: 520px;
  }
  .fld.wide .ctl {
    max-width: 640px;
  }
  .ctl > :global(.sw),
  .ctl > :global(.seg) {
    align-self: flex-start;
  }
  .inp {
    width: 100%;
    height: var(--control-h);
    padding: 0 10px;
    border: 0;
    border-radius: var(--r);
    background: var(--surface);
    box-shadow: var(--shadow-border);
    color: var(--text);
  }
  .inp::placeholder {
    color: var(--text-subtle);
  }
  .inp.num {
    max-width: 200px;
  }
  .money {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .cur {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .inp.time {
    width: 140px;
  }
  @media (max-width: 640px) {
    .inp {
      font-size: 16px;
    }
  }
  .inp:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
  }
  .ro {
    padding: 8px 10px;
    border-radius: var(--r);
    background: var(--surface-2);
    font-family: var(--font-mono);
  }
  .inl {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .switches {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
    gap: 4px var(--s-4);
  }
  .rows {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .mrow {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto minmax(0, 1.4fr) auto;
    align-items: center;
    gap: 6px;
  }
  .arrow {
    color: var(--text-subtle);
  }
  .managed {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    padding: 4px 0 var(--s-2);
  }
  .managed summary,
  .fmt summary {
    cursor: pointer;
  }
  .ex {
    display: flex;
    gap: var(--s-2);
    align-items: flex-start;
    margin-top: 6px;
  }
  .ex pre {
    flex: 1;
    min-width: 0;
    max-height: 240px;
    margin: 0;
    padding: 8px 10px;
    overflow: auto;
    border-radius: var(--r);
    background: var(--surface-2);
    font-size: var(--text-xs);
  }
  .st {
    display: flex;
    align-items: center;
    gap: 4px;
    justify-content: flex-end;
  }
  .reset {
    display: grid;
    place-items: center;
    flex: none;
    width: 28px;
    height: 28px;
    border: 0;
    border-radius: var(--r-sm);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
  }
  .reset:hover {
    background: var(--surface-3);
    color: var(--text);
  }
  .err {
    color: var(--danger);
  }
  @media (max-width: 760px) {
    .fld {
      grid-template-columns: 1fr;
    }
    .st {
      justify-content: flex-start;
    }
    .mrow {
      grid-template-columns: minmax(0, 1fr) auto;
    }
    .mrow .arrow {
      display: none;
    }
  }
</style>
