<script lang="ts">
  import { fly } from 'svelte/transition'
  import { scrollable } from '../lib/scrollable'
  import { t } from '../lib/i18n.svelte'
  import { call, postJSON } from '../lib/api.svelte'
  import { cfg, useConfig, patchConfig } from '../lib/config.svelte'
  import { navigate } from '../lib/router.svelte'
  import { motionAllowed } from '../lib/prefs.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Field from '../components/Field.svelte'
  import Input from '../components/Input.svelte'
  import TextArea from '../components/TextArea.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Switch from '../components/Switch.svelte'
  import Status from '../components/Status.svelte'
  import * as I from '../lib/icons'

  // Adds an operator HTTP tool without code: basics → request → result → save and test.
  // The same rules as config validation are checked here so errors show next to the field.
  useConfig()
  const d = $derived(motionAllowed() ? 1 : 0)
  let step = $state(0)
  let dir = $state(1)
  let f = $state({ id: '', description: '', route: '', url: 'https://', credMode: 'none' as 'none' | 'env' | 'value', credRef: 'env:', credValue: '', header: '', headerValue: '', pick: '', maxChars: 3000, chat: true, commands: true, alias: '' })
  let params = $state<{ name: string; required: boolean; description: string }[]>([])
  let errs = $state<Record<string, string>>({})
  let saving = $state(false)
  let result = $state<any>(null)
  let testInput = $state<Record<string, string>>({})

  const idRE = /^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$/
  const paramRE = /^[a-z][a-z0-9_]{0,31}$/
  const placeholders = $derived([...new Set([...f.url.matchAll(/\{([a-z][a-z0-9_]*)\}/g)].map((m) => m[1]).filter((n) => n !== 'credential'))])
  $effect(() => {
    // the declared params are exactly the URL's {placeholders}: a new one gets a row, one that left the
    // URL loses it (a stale required parameter would have the bot ask for a value the URL never uses)
    const keep = params.filter((p) => placeholders.includes(p.name))
    const added = placeholders.filter((n) => !keep.some((p) => p.name === n))
    if (keep.length !== params.length || added.length) params = [...keep, ...added.map((name) => ({ name, required: true, description: '' }))]
  })

  function validate(s: number): boolean {
    const e: Record<string, string> = {}
    if (s === 0) {
      if (!idRE.test(f.id)) e.id = t('toolnew.err.id')
      else if (cfg.doc?.tools?.[f.id]) e.id = t('toolnew.err.id_taken')
      if (!f.description.trim()) e.description = t('toolnew.err.description')
      else if (f.description.length > 600) e.description = t('toolnew.err.too_long', { n: 600 })
    }
    if (s === 1) {
      try {
        const u = new URL(f.url.replace(/[{}]/g, ''))
        const host = f.url.replace(/^https?:\/\//, '').split('/')[0]
        if (u.protocol !== 'https:' && !(u.protocol === 'http:' && ['127.0.0.1', 'localhost'].includes(u.hostname))) e.url = t('toolnew.err.https')
        else if (host.includes('{')) e.url = t('toolnew.err.host_placeholder')
      } catch {
        e.url = t('toolnew.err.url')
      }
      for (const p of params) if (!paramRE.test(p.name) || p.name === 'credential') e['p_' + p.name] = t('toolnew.err.param')
      const usesCred = f.url.includes('{credential}') || f.headerValue.includes('{credential}')
      if (usesCred && f.credMode === 'none') e.cred = t('toolnew.err.cred_needed')
      if (f.credMode === 'env' && !/^(env|file):.+/.test(f.credRef)) e.cred = t('toolnew.err.cred_ref')
      if (f.credMode === 'value' && !f.credValue.trim()) e.cred = t('toolnew.err.cred_value')
    }
    errs = e
    return Object.keys(e).length === 0
  }
  function go(to: number) {
    if (to > step && !validate(step)) return
    dir = to > step ? 1 : -1
    step = to
  }
  async function saveAndTest() {
    if (!validate(0) || !validate(1)) return
    saving = true
    try {
      await save()
    } finally {
      saving = false
    }
  }
  async function save() {
    let credential_ref = ''
    if (f.credMode === 'env') credential_ref = f.credRef.trim()
    if (f.credMode === 'value') {
      const r = await postJSON('/api/secret', { target: 'file', id: f.id, value: f.credValue.trim() })
      // the typed key stays in the field until the tool is saved, so a failure can be retried
      if (r.status !== 200) {
        errs = { cred: r.body?.error?.message ?? t('common.failed') }
        step = 1
        return
      }
      credential_ref = r.body.credential_ref
    }
    const http: any = { url: f.url.trim() }
    if (credential_ref) http.credential_ref = credential_ref
    if (f.header.trim()) http.headers = { [f.header.trim()]: f.headerValue }
    const pick = f.pick.split(',').map((x) => x.trim()).filter(Boolean)
    if (pick.length) http.pick = pick
    if (f.maxChars && f.maxChars !== 3000) http.max_chars = Number(f.maxChars)
    const settings: any = { description: f.description.trim(), http, chat: f.chat, commands: f.commands }
    if (f.route.trim()) settings.route = f.route.trim()
    if (params.length) settings.params = params.map((p) => ({ name: p.name, required: p.required || undefined, description: p.description || undefined }))
    if (f.alias.trim()) settings.aliases = f.alias.split(',').map((x) => x.trim()).filter(Boolean)
    const r = await patchConfig([{ path: ['tools', f.id], value: settings }], { quiet: true })
    if (!r.ok) {
      errs = { save: r.error?.message ?? '' }
      return
    }
    f.credValue = ''
    const test = await call('tools.test', { id: f.id, input: JSON.stringify(Object.fromEntries(params.map((p) => [p.name, testInput[p.name] ?? '']))) })
    result = test.ok ? test.output : { ok: false, error: test.error }
    dir = 1
    step = 3
  }
  const steps = ['basics', 'request', 'result', 'done']
</script>

<PageHeader title={t('toolnew.title')} description={t('toolnew.desc')} crumbs={[{ href: '/tools', label: t('nav.tools') }]} />

<div class="wiz">
  <ol class="st" aria-label={t('toolnew.steps')}>
    {#each steps as s, i}
      <li class:cur={i === step} class:done={i < step} aria-current={i === step ? 'step' : undefined}><span class="n">{#if i < step}<I.Check size={12} strokeWidth={3} aria-hidden="true" />{:else}{i + 1}{/if}</span>{t(`toolnew.step.${s}`)}</li>
    {/each}
  </ol>

  {#key step}
    <div in:fly={{ x: 20 * dir * d, duration: 280 * d, opacity: 0 }}>
      <Card>
        {#if step === 0}
          <div class="stack">
            <Field id="tid" label={t('toolnew.id')} hint={t('toolnew.id_hint')} error={errs.id} required>
              {#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={f.id} mono placeholder="weather.airkorea" autocomplete="off" spellcheck={false} />{/snippet}
            </Field>
            <Field id="tds" label={t('tool.description')} hint={t('toolnew.description_hint')} error={errs.description} required>
              {#snippet children({ id, describedby, invalid })}<TextArea {id} aria-describedby={describedby} {invalid} bind:value={f.description} rows={3} maxlength={600} placeholder={t('toolnew.description_ph')} />{/snippet}
            </Field>
            <Field id="trt" label={t('tool.route')} hint={t('tool.route_hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={f.route} maxlength={200} placeholder={t('toolnew.route_ph')} />{/snippet}
            </Field>
            <div class="form-grid">
              <Switch bind:checked={f.chat} label={t('tools.sw.chat')} hint={t('tool.sw_hint.chat')} />
              <Switch bind:checked={f.commands} label={t('tools.sw.commands')} hint={t('tool.sw_hint.commands')} />
            </div>
            <Field id="tal" label={t('tool.aliases')} hint={t('tool.aliases_hint')}>
              {#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={f.alias} placeholder={t('toolnew.alias_ph')} />{/snippet}
            </Field>
          </div>
        {:else if step === 1}
          <div class="stack">
            <Field id="url" label="URL" hint={t('toolnew.url_hint', { name: '{name}', credential: '{credential}' })} error={errs.url} required>
              {#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={f.url} mono type="url" spellcheck={false} placeholder="https://api.example.com/v1/items?q={'{query}'}&key={'{credential}'}" />{/snippet}
            </Field>
            {#if params.length}
              <div>
                <div class="section-title">{t('toolnew.params')}</div>
                <div use:scrollable class="table-scroll"><table class="table">
                  <thead><tr><th>{t('toolnew.param_name')}</th><th>{t('toolnew.param_required')}</th><th>{t('toolnew.param_desc')}</th></tr></thead>
                  <tbody>
                    {#each params as p (p.name)}
                      <tr>
                        <td><code>{'{'}{p.name}{'}'}</code>{#if errs['p_' + p.name]}<div class="xs err">{errs['p_' + p.name]}</div>{/if}</td>
                        <td><input type="checkbox" bind:checked={p.required} aria-label={t('toolnew.param_required')} /></td>
                        <td><Input bind:value={p.description} aria-label={t('toolnew.param_desc')} placeholder={t('toolnew.param_desc_ph')} /></td>
                      </tr>
                    {/each}
                  </tbody>
                </table></div>
              </div>
            {/if}
            <div>
              <div class="section-title">{t('toolnew.credential')}</div>
              <div style:margin-top="8px"><Segmented label={t('toolnew.credential')} bind:value={f.credMode} options={[{ value: 'none', label: t('toolnew.cred.none') }, { value: 'value', label: t('toolnew.cred.value') }, { value: 'env', label: t('toolnew.cred.env') }]} /></div>
              {#if f.credMode === 'value'}
                <Field id="cv" label={t('toolnew.cred_value')} hint={t('toolnew.cred_value_hint')} error={errs.cred}>{#snippet children({ id, describedby, invalid })}<Input {id} type="password" aria-describedby={describedby} {invalid} bind:value={f.credValue} autocomplete="off" />{/snippet}</Field>
              {:else if f.credMode === 'env'}
                <Field id="ce" label={t('tool.credential_ref')} hint={t('toolnew.cred_hint')} error={errs.cred}>{#snippet children({ id, describedby, invalid })}<Input {id} aria-describedby={describedby} {invalid} bind:value={f.credRef} mono />{/snippet}</Field>
              {:else if errs.cred}<p class="small err">{errs.cred}</p>{/if}
            </div>
            <div class="form-grid">
              <Field id="hn" label={t('toolnew.header')} hint={t('toolnew.header_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={f.header} mono placeholder="Authorization" />{/snippet}</Field>
              <Field id="hv" label={t('toolnew.header_value')}>{#snippet children({ id })}<Input {id} bind:value={f.headerValue} mono placeholder={'Bearer {credential}'} />{/snippet}</Field>
            </div>
          </div>
        {:else if step === 2}
          <div class="stack">
            <Field id="pk" label={t('tool.pick')} hint={t('toolnew.pick_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} bind:value={f.pick} mono placeholder="response.body.items" />{/snippet}</Field>
            <Field id="mx" label={t('tool.max_chars')} hint={t('toolnew.max_hint')}>{#snippet children({ id, describedby })}<Input {id} aria-describedby={describedby} type="number" min="200" max="20000" bind:value={f.maxChars} />{/snippet}</Field>
            {#if params.length}
              <div class="section-title">{t('toolnew.test_values')}</div>
              <div class="form-grid">
                {#each params as p (p.name)}
                  <Field id="tv-{p.name}" label={p.name}>{#snippet children({ id })}<Input {id} bind:value={testInput[p.name]} />{/snippet}</Field>
                {/each}
              </div>
            {/if}
            <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('toolnew.save_note')}</span></div>
            {#if errs.save}<p class="small err break" role="alert">{errs.save}</p>{/if}
          </div>
        {:else}
          <div class="stack">
            <div class="row"><Status tone={result?.ok ? 'ok' : 'warn'} label={result?.ok ? t('toolnew.test_ok') : t('toolnew.test_fail')} /><span class="small muted">{t('toolnew.saved_as', { id: f.id })}</span></div>
            {#if result?.error}<p class="small err break">{result.error.message}</p>{/if}
            <pre class="out">{JSON.stringify(result?.output ?? result?.error ?? {}, null, 2)}</pre>
          </div>
        {/if}
        {#snippet footer()}
          <div class="row">
            {#if step > 0 && step < 3}<Button variant="ghost" icon={I.ChevronLeft} onclick={() => go(step - 1)}>{t('setup.back')}</Button>{/if}
            <span class="spacer"></span>
            {#if step < 2}<Button variant="primary" iconEnd={I.ArrowRight} onclick={() => go(step + 1)}>{t('setup.next')}</Button>
            {:else if step === 2}<Button variant="primary" icon={I.Play} loading={saving} onclick={saveAndTest}>{t('toolnew.save_test')}</Button>
            {:else}<Button href="/tools/{encodeURIComponent(f.id)}" variant="primary" iconEnd={I.ArrowRight}>{t('toolnew.open')}</Button>{/if}
          </div>
        {/snippet}
      </Card>
    </div>
  {/key}
</div>

<style>
  .wiz {
    max-width: 880px;
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
  }
  .st {
    list-style: none;
    display: flex;
    gap: var(--s-2);
    margin: 0;
    padding: 0;
    flex-wrap: wrap;
  }
  .st li {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 12px 6px 6px;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-sm);
    font-weight: 550;
    transition: background-color var(--dur) var(--ease);
  }
  .st li.cur {
    background: var(--accent-soft);
    color: var(--text);
  }
  .n {
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border-radius: 999px;
    background: var(--surface-3);
    font-size: var(--text-xs);
  }
  .cur .n {
    background: var(--accent);
    color: var(--on-accent);
  }
  .done .n {
    background: var(--ok);
    color: #fff;
  }
  .out {
    margin: 0;
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
    font-size: var(--text-xs);
    max-height: 360px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .err {
    color: var(--danger);
  }
</style>
