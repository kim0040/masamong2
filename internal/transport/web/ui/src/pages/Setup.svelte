<script lang="ts">
  import { brand } from 'virtual:brand'
  import { useTitle } from '../lib/title.svelte'
  import { fly } from 'svelte/transition'
  import { t, i18n, languageLabel } from '../lib/i18n.svelte'
  import { call, postJSON, session, loadSession, setPractice, canLoadProtectedActions } from '../lib/api.svelte'
  import { navigate, route } from '../lib/router.svelte'
  import { tp } from '../lib/platform'
  import { prefs, setPrefs, loadPrefs, motionAllowed } from '../lib/prefs.svelte'
  import { cfg, loadConfig, patchConfig } from '../lib/config.svelte'
  import { startOverview } from '../lib/overview.svelte'
  import { toast } from '../lib/toast.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import Field from '../components/Field.svelte'
  import Segmented from '../components/Segmented.svelte'
  import Select from '../components/Select.svelte'
  import Switch from '../components/Switch.svelte'
  import Status from '../components/Status.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import * as I from '../lib/icons'

  // First run: data folder (and how the console is protected) → model key → Discord → first room → a
  // test chat. Every step uses the real Actions, can be skipped, and resumes where it stopped
  // (prefs.tutorial.step).
  const steps = ['welcome', 'model', 'discord', 'room', 'test'] as const
  const art: Record<string, string> = { welcome: 'setup-data-folder', model: 'setup-model-key', discord: 'setup-discord', room: 'setup-first-room', test: 'setup-test-chat' }
  let step = $state(Math.min(steps.length - 1, Math.max(0, route.query.get('tour') ? 0 : (prefs.tutorial?.step ?? 0))))
  let dir = $state(1)
  const d = $derived(motionAllowed() ? 1 : 0)
  const cur = $derived(steps[step])

  let instance = $state('masamong')
  // the bot's language: new rooms answer in it with its built-in persona. It starts as the console's
  // language, so a console used in Korean does not create an English bot.
  let botLang = $state(i18n.lang)
  const botLangs = $derived((session.value?.locales?.length ? session.value.locales : ['ko', 'en']).map((l) => ({ value: l, label: languageLabel(l) })))
  let apiKey = $state('')
  let discordToken = $state('')
  // who the token belongs to, read from Discord after it is saved (discord.check stores the bot id)
  let bot = $state<any>(null)
  let botErr = $state('')
  let botReason = $state('')
  // a bot token can be read now (a saved one counts: the field may stay empty)
  let tokenSaved = $state(false)
  let guildId = $state('')
  let channelId = $state('')
  let idErr = $state('')
  let respond = $state('mention')
  let testMsg = $state('')
  let busy = $state(false)
  let err = $state('')
  // ready only after a real check of the key (a saved but wrong key is 'invalid', not connected)
  let modelStatus = $state<'unknown' | 'checking' | 'ready' | 'invalid' | 'missing'>('unknown')
  let keyProblem = $state('')
  // which model host the key is for (the server lists them: OpenRouter, Gemini, OpenAI)
  let hosts = $state<any[]>([])
  let hostId = $state('openrouter')
  const host = $derived(hosts.find((h) => h.id === hostId) ?? hosts[0])
  // the step is done when the provider the answer model uses has a working key
  const modelProvider = () => cfg.doc?.models?.response?.provider || host?.id || 'openrouter'
  let discordEnabled = $state(false)
  let answer = $state<{ text: string; cost: number; ms: number } | null>(null)
  let invite = $state('')
  let liveChannels = $state<{ id: string; name: string; guild_id: string }[]>([])

  const done = $derived<Record<string, boolean>>({
    welcome: !!session.value?.ready,
    model: modelStatus === 'ready',
    discord: discordEnabled && !!bot, // on, and Discord answered for the token
    room: !!(guildId && channelId && cfg.doc?.guilds?.[guildId]?.channels?.[channelId]),
    test: !!answer,
  })

  async function refresh() {
    if (!canLoadProtectedActions(session.value)) return
    await loadConfig(true)
    const r = await call('providers.list')
    hosts = r.output?.hosts ?? []
    const pid = modelProvider()
    const cap = r.output?.model_providers?.[pid]
    const known = hosts.find((h) => (cap?.host ? h.id === cap.host : h.api === cap?.api && h.api !== 'compatible'))
    if (known) hostId = known.id
    const or = r.output?.providers?.find((p: any) => p.id === pid)
    if (!or?.configured) modelStatus = 'missing'
    else if (modelStatus !== 'ready' && modelStatus !== 'invalid') void checkKey()
    discordEnabled = !!cfg.doc?.discord?.enabled
    const ch = await call('channels.list')
    tokenSaved = !!ch.output?.discord_token
    if (discordEnabled && tokenSaved && !bot) void checkBot()
    liveChannels = (ch.output?.channels ?? []).filter((c: any) => c.type === 'text' || !c.type)
    if (!guildId) {
      const g = Object.keys(cfg.doc?.guilds ?? {})[0]
      if (g) {
        guildId = g
        channelId = Object.keys(cfg.doc.guilds[g].channels ?? {})[0] ?? ''
      }
    }
  }
  $effect(() => {
    void refresh()
  })

  function go(to: number) {
    err = ''
    dir = to > step ? 1 : -1
    step = Math.max(0, Math.min(steps.length - 1, to))
    setPrefs({ tutorial: { ...prefs.tutorial, step } })
  }
  async function finish() {
    setPrefs({ tutorial: { ...prefs.tutorial, done: true, step: 0 } })
    startOverview()
    navigate('/')
  }

  async function doWelcome() {
    if (session.value?.ready) return go(1)
    busy = true
    try {
      let timezone = ''
      try {
        timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || ''
      } catch {}
      const r = await call('setup.resume', { instance: instance.trim() || 'masamong', locale: botLang, timezone })
      if (!r.ok) return (err = r.error?.message ?? '')
      await loadSession()
      await loadPrefs()
      await refresh()
      go(1)
    } finally {
      busy = false
    }
  }
  async function checkKey(id = modelProvider()) {
    modelStatus = 'checking'
    const r = await call('providers.test', { id })
    modelStatus = r.ok ? 'ready' : 'invalid'
    keyProblem = r.ok ? '' : (r.error?.message ?? '')
  }
  async function saveKey() {
    if (!apiKey.trim()) return (err = t('setup.model.key_required'))
    if (!host) return
    busy = true
    err = ''
    try {
      // a host the instance does not list yet is added first (a new instance lists only OpenRouter)
      if (!cfg.doc?.providers?.[host.id]) {
        const a = await call('providers.add', { host: host.id })
        if (!a.ok) return (err = a.error?.message ?? t('common.failed'))
      }
      const r = await postJSON('/api/secret', { target: 'provider', id: host.id, value: apiKey.trim() })
      // the typed key stays in the field when saving failed, so it can be sent again
      if (r.status !== 200) return (err = r.body?.error?.message ?? t('common.failed'))
      apiKey = ''
      await checkKey(host.id)
      // the key works: point the roles this host can serve at its newest models
      if (modelStatus === 'ready' && modelProvider() !== host.id) {
        const rec = await call('models.recommend', { provider: host.id, apply: true })
        if (!rec.ok) err = rec.error?.message ?? ''
        await loadConfig(true)
      }
      if (modelStatus === 'ready') toast('ok', t('setup.model.saved'))
    } finally {
      busy = false
    }
  }
  async function makeInvite() {
    const r = await call('channels.invite')
    if (r.ok) invite = r.output.url
    else err = r.error?.message ?? ''
  }
  // The step needs a token: the one typed here, or one already saved. The switch is turned on only
  // when Discord has not refused the token, and the step counts as done once Discord has answered for it.
  async function saveDiscord() {
    err = ''
    const typed = discordToken.trim()
    if (!typed && !tokenSaved) return (err = t('setup.discord.token_required'))
    busy = true
    try {
      if (typed) {
        const r = await postJSON('/api/secret', { target: 'discord', value: typed })
        if (r.status !== 200) return (err = r.body?.error?.message ?? t('common.failed'))
        discordToken = ''
        tokenSaved = true
      }
      await checkBot()
      if (botReason === 'token_rejected') return
      const r = await patchConfig([{ path: ['discord', 'enabled'], value: true }], { quiet: true })
      if (!r.ok) return (err = r.error?.message ?? '')
      await refresh()
      const inv = await call('channels.invite')
      invite = inv.ok ? inv.output.url : ''
      toast('info', t('setup.discord.saved'), t('setup.discord.restart_hint'), { label: t('maint.restart_now'), run: () => void call('runtime.restart') })
    } finally {
      busy = false
    }
  }
  async function checkBot() {
    botErr = ''
    botReason = ''
    const r = await call('discord.check', { apply: true })
    bot = r.ok ? r.output : null
    if (!r.ok) {
      botReason = String(r.error?.args?.reason ?? '')
      if (botReason !== 'no_token') botErr = r.error?.message ?? ''
    }
  }
  async function saveRoom() {
    if (!/^\d{5,25}$/.test(guildId.trim()) || !/^\d{5,25}$/.test(channelId.trim())) return (idErr = t('setup.room.ids_invalid'))
    busy = true
    try {
      const r = await call('channels.configure', { guild_id: guildId.trim(), channel_id: channelId.trim(), respond, memory_use: 'on' })
      if (!r.ok) return (err = r.error?.message ?? '')
      await refresh()
      toast('ok', t('setup.room.saved'))
      go(steps.indexOf('test'))
    } finally {
      busy = false
    }
  }
  async function runTest() {
    if (!testMsg.trim()) return (err = t('lab.message_required'))
    busy = true
    err = ''
    try {
      const r = await call('chat.respond', { guild_id: guildId || '0', channel_id: channelId || 'setup', message: testMsg.trim(), speaker: t('lab.default_speaker') })
      if (!r.ok) return (err = r.error?.message ?? '')
      answer = { text: r.output.answer, cost: r.output.cost_usd, ms: Object.values<number>(r.output.timing_ms ?? {}).reduce((a, b) => a + b, 0) }
    } finally {
      busy = false
    }
  }
  // Enter in any field of a step does what the step's main button does
  function submitStep(e: SubmitEvent) {
    e.preventDefault()
    if (busy) return
    if (cur === 'welcome') void doWelcome()
    else if (cur === 'model') void saveKey()
    else if (cur === 'discord') void saveDiscord()
    else if (cur === 'room') void saveRoom()
    else if (cur === 'test') void runTest()
  }
  useTitle(() => t(`setup.${cur}.title`))
</script>

<div class="setup">
  <aside class="rail">
    <a class="brand" href="/"><img src="/brand/logo-mark.svg" alt="" width="32" height="32" /><strong>{t('app.name')}</strong></a>
    <p class="muted small">{t('setup.rail_lead')}</p>
    <ol class="steps">
      {#each steps as s, i}
        <li class:cur={i === step} class:done={done[s]}>
          <button type="button" onclick={() => go(i)} disabled={i > 0 && !session.value?.ready} aria-current={i === step ? 'step' : undefined}>
            <span class="n" aria-hidden="true">{#if done[s]}<I.Check size={14} />{:else}{i + 1}{/if}</span>
            <span>{t(`setup.${s}.nav`)}</span>
            {#if done[s]}<span class="sr-only">{t('common.done')}</span>{/if}
          </button>
        </li>
      {/each}
    </ol>
    {#if session.value?.ready}<Button variant="ghost" size="sm" onclick={finish}>{t('setup.skip_all')}</Button>{/if}
    <div class="practice-cta">
      <p class="xs muted">{t('setup.practice_hint')}</p>
      <Button size="sm" variant="secondary" icon={I.FlaskConical} onclick={() => setPractice(true)}>{t('practice.enter')}</Button>
    </div>
  </aside>

  <main class="main" id="main" tabindex="-1">
    {#key step}
      <section class="panel" in:fly={{ x: 24 * dir * d, duration: 320 * d, opacity: 0 }} aria-labelledby="step-title">
        <header class="head">
          <img class="art" src={brand(`tutorial/${art[cur]}.webp`)} alt="" width="640" height="400" />
          <p class="kicker">{t('setup.step_of', { n: step + 1, total: steps.length })}</p>
          <h1 id="step-title">{t(`setup.${cur}.title`)}</h1>
          <p class="lead">{t(`setup.${cur}.body`)}</p>
        </header>

        <form class="form" onsubmit={submitStep} novalidate>
          {#if cur === 'welcome'}
            <div class="kv"><span class="muted">{t('setup.welcome.data_root')}</span><code class="break">{session.value?.data_root}</code></div>
            {#if !session.value?.ready}
              <Field id="inst" label={t('setup.welcome.instance')} hint={t('setup.welcome.instance_hint')}>
                {#snippet children({ id, describedby })}<Input {id} bind:value={instance} aria-describedby={describedby} autocomplete="off" />{/snippet}
              </Field>
              <Field id="botlang" label={t('setup.welcome.bot_lang')} hint={t('setup.welcome.bot_lang_hint')}>
                {#snippet children({ id, describedby })}<Select {id} bind:value={botLang} options={botLangs} aria-describedby={describedby} />{/snippet}
              </Field>
            {:else}
              <Status tone="ok" label={t('setup.welcome.ready', { instance: session.value?.instance })} />
            {/if}
            <p class="small"><strong>{t('setup.protect.title')}</strong></p>
            <ul class="facts">
              <li><I.ShieldCheck size={18} aria-hidden="true" /><span>{t('setup.protect.fact_local', { bind: session.value?.bind })}</span></li>
              <li><I.KeyRound size={18} aria-hidden="true" /><span>{t('setup.protect.fact_token', { tray: tp('platform.tray') })}</span></li>
              <li><I.Terminal size={18} aria-hidden="true" /><span>{t('setup.protect.fact_ssh')}</span></li>
            </ul>
          {:else if cur === 'model'}
            <div class="row"><Status tone={modelStatus === 'ready' ? 'ok' : modelStatus === 'invalid' ? 'danger' : modelStatus === 'checking' ? 'info' : 'warn'} label={modelStatus === 'ready' ? t('setup.model.connected') : modelStatus === 'invalid' ? t('setup.model.invalid') : modelStatus === 'checking' ? t('setup.model.checking') : t('setup.model.missing')} /></div>
            {#if modelStatus === 'invalid'}<p class="err" role="alert">{t('setup.model.invalid_body')}{#if keyProblem}<br /><span class="xs muted">{keyProblem}</span>{/if}</p>{/if}
            {#if hosts.length > 1}
              <Segmented label={t('setup.model.host')} bind:value={hostId} options={hosts.map((h) => ({ value: h.id, label: h.name }))} />
              <p class="xs muted">{t(`setup.model.host_note.${hostId}`)}</p>
            {/if}
            <Field id="key" label={t('setup.model.key_for', { name: host?.name ?? '' })} hint={t('setup.model.key_hint')}>
              {#snippet children({ id, describedby })}<Input {id} type="password" bind:value={apiKey} autocomplete="off" aria-describedby={describedby} placeholder={hostId === 'openrouter' ? 'sk-or-…' : hostId === 'anthropic' ? 'sk-ant-…' : hostId === 'openai' || hostId === 'cometapi' ? 'sk-…' : ''} />{/snippet}
            </Field>
            <div class="row"><Button type="submit" variant="primary" loading={busy} icon={I.KeyRound}>{t('setup.model.save')}</Button>{#if host?.key_url}<Button variant="ghost" href={host.key_url} target="_blank" rel="noopener" data-native icon={I.ExternalLink}>{t('setup.model.get_key')}</Button>{/if}</div>
          {:else if cur === 'discord'}
            <div class="row"><Status tone={discordEnabled && bot ? 'ok' : discordEnabled ? 'warn' : 'neutral'} label={discordEnabled && bot ? t('setup.discord.enabled') : discordEnabled ? t('setup.discord.unverified') : t('setup.discord.not_enabled')} /></div>
            <Field id="dt" label={t('setup.discord.token')} hint={t('setup.discord.token_hint')}>
              {#snippet children({ id, describedby })}<Input {id} type="password" bind:value={discordToken} autocomplete="off" aria-describedby={describedby} />{/snippet}
            </Field>
            <div class="row"><Button type="submit" variant="primary" loading={busy} icon={I.Bot}>{t('setup.discord.save')}</Button></div>
            {#if botErr}<p class="err" role="alert">{botErr}</p>{/if}
            {#if bot}
              <Status tone="ok" label={t('conn.bot_found', { name: bot.bot_name, id: bot.bot_id })} />
              {#if bot.message_content_blocked}<div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span class="small">{t('conn.intent_blocked')} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a></span></div>
              {:else if !bot.message_content_portal}<div class="callout"><span class="ic"><I.Info size={16} /></span><span class="small">{t('conn.intent_off')} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a></span></div>{/if}
            {/if}
            <div class="invite">
              <p class="small"><strong>{t('setup.discord.invite')}</strong></p>
              <p class="xs muted">{t('setup.discord.invite_hint')}</p>
              {#if invite}
                <div class="row"><Button href={invite} target="_blank" rel="noopener" data-native icon={I.ExternalLink}>{t('setup.discord.invite_open')}</Button><CopyButton text={invite} /></div>
              {:else}
                <div class="row"><Button icon={I.Link} disabled={!bot && !discordEnabled} onclick={makeInvite}>{t('room.invite_make')}</Button></div>
              {/if}
            </div>
          {:else if cur === 'room'}
            {#if liveChannels.length}
              <Field id="pick" label={t('setup.room.pick')}>
                {#snippet children({ id })}<Select {id} value={channelId} onchange={(e) => { const c = liveChannels.find((x) => x.id === (e.currentTarget as HTMLSelectElement).value); if (c) { channelId = c.id; guildId = c.guild_id } }} options={[{ value: '', label: '—' }, ...liveChannels.map((c) => ({ value: c.id, label: '#' + c.name }))]} />{/snippet}
              </Field>
            {/if}
            <div class="form-grid">
              <Field id="gid" label={t('setup.room.guild')} hint={t('setup.room.id_hint')} error={idErr || undefined}>
                {#snippet children({ id, describedby, invalid })}<Input {id} bind:value={guildId} inputmode="numeric" mono {invalid} oninput={() => (idErr = '')} aria-describedby={describedby} />{/snippet}
              </Field>
              <Field id="cid" label={t('setup.room.channel')}>
                {#snippet children({ id })}<Input {id} bind:value={channelId} inputmode="numeric" mono invalid={!!idErr} oninput={() => (idErr = '')} />{/snippet}
              </Field>
            </div>
            <Field id="resp" label={t('rooms.respond')} hint={t(`rooms.respond_hint.${respond}`)}>
              {#snippet children({ id })}<Select {id} bind:value={respond} options={['always', 'mention', 'command', 'off'].map((v) => ({ value: v, label: t(`rooms.respond.${v}`) }))} />{/snippet}
            </Field>
            <div class="row"><Button type="submit" variant="primary" loading={busy} icon={I.Hash}>{t('setup.room.save')}</Button></div>
          {:else if cur === 'test'}
            {@const noModel = modelStatus === 'missing' || modelStatus === 'invalid'}
            {#if noModel}
              <div class="callout warn"><span class="ic"><I.TriangleAlert size={16} /></span><span>{t('setup.test.needs_key')}</span><Button size="sm" variant="secondary" onclick={() => go(steps.indexOf('model'))}>{t('setup.test.to_key')}</Button></div>
            {:else}
              <div class="callout"><span class="ic"><I.Info size={16} /></span><span>{t('setup.test.cost_note')}</span></div>
            {/if}
            <Field id="msg" label={t('lab.message')}>
              {#snippet children({ id })}<Input {id} bind:value={testMsg} placeholder={t('setup.test.example')} disabled={noModel} />{/snippet}
            </Field>
            <div class="row"><Button type="submit" variant="primary" loading={busy} disabled={noModel} icon={I.Send}>{t('setup.test.send')}</Button></div>
            {#if answer}
              <div class="bubble" in:fly={{ y: 8, duration: 260 * d }}>
                <img src="/brand/logo-mark.svg" alt="" width="28" height="28" />
                <div><p>{answer.text || t('lab.silent')}</p><p class="xs muted num">{t('lab.cost_time', { cost: answer.cost.toFixed(4), ms: answer.ms })}</p></div>
              </div>
            {/if}
          {/if}
          {#if err}<p class="err" role="alert">{err}</p>{/if}
        </form>

        <div class="nav">
          {#if step > 0}<Button variant="ghost" onclick={() => go(step - 1)} icon={I.ChevronLeft}>{t('setup.back')}</Button>{/if}
          <span class="spacer"></span>
          {#if cur === 'welcome'}
            <Button variant="primary" loading={busy} onclick={doWelcome} iconEnd={I.ArrowRight}>{session.value?.ready ? t('setup.next') : t('setup.welcome.start')}</Button>
          {:else if step < steps.length - 1}
            {#if !done[cur]}<Button variant="ghost" onclick={() => go(step + 1)}>{t('setup.later')}</Button>{/if}
            <Button variant={done[cur] ? 'primary' : 'secondary'} onclick={() => go(step + 1)} iconEnd={I.ArrowRight}>{t('setup.next')}</Button>
          {:else}
            <Button variant="primary" onclick={finish} iconEnd={I.Sparkles}>{t('setup.finish')}</Button>
          {/if}
        </div>
      </section>
    {/key}
  </main>
</div>

<style>
  .practice-cta {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: var(--s-4);
    padding: 12px;
    border-radius: var(--r-lg);
    background: var(--practice-soft);
  }
  .setup {
    min-height: 100dvh;
    display: grid;
    grid-template-columns: 300px 1fr;
    background: var(--bg);
  }
  .rail {
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
    padding: var(--s-8) var(--s-6);
    background: var(--surface);
    box-shadow: inset -1px 0 0 var(--border);
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
    color: var(--text);
    text-decoration: none;
    font-size: var(--text-lg);
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .steps button {
    display: flex;
    align-items: center;
    gap: 12px;
    width: 100%;
    height: 44px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--r);
    background: none;
    color: var(--text-muted);
    font-weight: 550;
    text-align: start;
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-out);
  }
  .steps button:disabled {
    cursor: not-allowed;
    opacity: 0.5;
  }
  .steps button:hover:not(:disabled) {
    background: var(--surface-2);
  }
  .cur button {
    background: var(--accent-soft);
    color: var(--text);
  }
  .n {
    display: grid;
    place-items: center;
    width: 24px;
    height: 24px;
    border-radius: 999px;
    background: var(--surface-3);
    font-size: var(--text-xs);
    font-weight: 700;
    transition: background-color var(--dur) var(--ease);
  }
  .done .n {
    background: var(--ok);
    color: #fff;
  }
  .cur .n {
    background: var(--accent);
    color: var(--on-accent);
  }
  /* Every step has the same frame: a header of fixed height, the step's card filling the space left
     (scrolling inside when it holds more), and the buttons always on the same line at the bottom. */
  .main {
    display: flex;
    justify-content: center;
    padding: var(--s-8) var(--s-6);
    outline: none;
    overflow: clip; /* hides the step slide-in without becoming a scroll box (the sticky buttons need that) */
  }
  .panel {
    width: min(640px, 100%);
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
  }
  .head {
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
    flex: none;
  }
  .art {
    display: block;
    width: auto;
    max-width: 100%;
    height: clamp(96px, 22dvh, 200px);
    object-fit: contain;
    margin: 0 auto var(--s-2);
  }
  .kicker {
    color: var(--text-muted);
    font-weight: 650;
    font-size: var(--text-sm);
  }
  h1 {
    font-size: var(--text-3xl);
    letter-spacing: -0.03em;
  }
  .lead {
    color: var(--text-muted);
    font-size: var(--text-md);
    line-height: 1.55;
    min-height: calc(2 * 1.55em); /* one- and two-line leads take the same room */
  }
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
    margin-top: var(--s-4);
    padding: var(--s-5);
    border-radius: var(--r-xl);
    background: var(--surface);
    box-shadow: var(--shadow-border);
  }
  .kv {
    display: flex;
    gap: var(--s-2);
    align-items: center;
    flex-wrap: wrap;
    font-size: var(--text-sm);
  }
  .invite {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding-top: var(--s-4);
    box-shadow: inset 0 1px 0 var(--border);
  }
  .facts {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--s-3);
  }
  .facts li {
    display: flex;
    gap: var(--s-3);
    align-items: flex-start;
  }
  .facts :global(svg) {
    flex: none;
    color: var(--accent-text);
    margin-top: 2px;
  }
  .bubble {
    display: flex;
    gap: var(--s-3);
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .bubble img {
    border-radius: 999px;
    flex: none;
  }
  .err {
    color: var(--danger);
    font-weight: 550;
    font-size: var(--text-sm);
  }
  .nav {
    display: flex;
    flex: none;
    gap: var(--s-2);
    align-items: center;
    min-height: 40px;
    margin-top: var(--s-2);
  }
  @media (min-width: 901px) {
    .setup {
      height: 100dvh;
    }
    .rail {
      overflow-y: auto;
    }
    main {
      min-height: 0; /* the step area is the window's height, not its content's, so the card can scroll */
    }
    .panel {
      height: 100%;
      max-height: 920px;
    }
    /* the card is as tall as its step (scrolling when that is more than fits); the buttons stay at the
       bottom so they do not move from step to step */
    .form {
      flex: 0 1 auto;
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
    }
    .nav {
      margin-top: auto;
    }
  }
  @media (max-width: 900px) {
    .setup {
      grid-template-columns: minmax(0, 1fr); /* the scrolling step row must not widen the page */
    }
    .rail {
      min-width: 0;
      padding: var(--s-4);
      box-shadow: inset 0 -1px 0 var(--border);
    }
    .steps {
      position: relative; /* keeps the buttons' screen-reader text inside the scrolling row */
      flex-direction: row;
      overflow-x: auto;
    }
    .steps button {
      width: auto;
      white-space: nowrap;
    }
    .main {
      padding: var(--s-6) var(--s-4) 0;
    }
    /* on a phone the buttons stay at the bottom of the screen while the step scrolls */
    .nav {
      position: sticky;
      bottom: 0;
      margin: 0 calc(-1 * var(--s-4));
      padding: var(--s-3) var(--s-4) calc(var(--s-3) + env(safe-area-inset-bottom));
      background: var(--bg);
      box-shadow: 0 -1px 0 var(--border);
    }
  }
</style>
