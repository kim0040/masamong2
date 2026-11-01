<script lang="ts">
  import { t, has } from '../lib/i18n.svelte'
  import { inputLabel, optionLabel, inputExample, type ToolInput, type InputValues } from '../lib/toolmeta'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Select from './Select.svelte'
  import Segmented from './Segmented.svelte'

  // A form for a tool's inputs as the server describes them (tools.list inputs). The same form edits a
  // test run's input and the tool's saved defaults: with defaults, nothing is required and every field
  // can be left to the tool's own default.
  let { tool, inputs, values = $bindable(), providers = [], idPrefix, defaults = false }: { tool: string; inputs: ToolInput[]; values: InputValues; providers?: { value: string; label: string }[]; idPrefix: string; defaults?: boolean } = $props()

  const tri = (v: unknown) => (v === true ? 'on' : v === false ? 'off' : 'default')
  function setTri(name: string, v: string) {
    values = { ...values, [name]: v === 'on' ? true : v === 'off' ? false : undefined }
  }
  function set(name: string, v: string) {
    values = { ...values, [name]: v }
  }
</script>

<div class="grid">
  {#each inputs.filter((i) => defaults || !i.operator) as s (s.name)}
    {@const fid = `${idPrefix}-${s.name}`}
    <Field id={fid} label={inputLabel(tool, s.name)} required={!defaults && s.required} hint={has(`toolp.hint.${tool}.${s.name}`) ? t(`toolp.hint.${tool}.${s.name}`) : undefined}>
      {#snippet children({ id })}
        {#if s.type === 'bool'}
          <Segmented size="sm" label={inputLabel(tool, s.name)} value={tri(values[s.name])} onchange={(v) => setTri(s.name, v)} options={[{ value: 'default', label: t('common.default') }, { value: 'on', label: t('common.on') }, { value: 'off', label: t('common.off') }]} />
        {:else if s.type === 'select' || s.type === 'provider'}
          <Select {id} value={(values[s.name] as string) ?? ''} onchange={(e) => set(s.name, (e.currentTarget as HTMLSelectElement).value)}
            options={[{ value: '', label: s.type === 'provider' ? t('toolp.provider_auto') : t('common.default') }, ...(s.type === 'provider' ? providers : (s.options ?? []).map((o) => ({ value: o, label: optionLabel(tool, s.name, o) })))]} />
        {:else}
          <Input {id} value={(values[s.name] as string) ?? ''} oninput={(e) => set(s.name, e.currentTarget.value)} type={s.type === 'number' ? 'number' : s.type === 'url' ? 'url' : 'text'} min={s.min || undefined} max={s.max || undefined} placeholder={inputExample(tool, s)} />
        {/if}
      {/snippet}
    </Field>
  {/each}
</div>

<style>
  .grid {
    display: grid;
    gap: var(--s-3);
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 220px), 1fr));
  }
</style>
