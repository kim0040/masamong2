<script lang="ts">
  import { login, session } from '../lib/api.svelte'
  import { useTitle } from '../lib/title.svelte'
  import { t } from '../lib/i18n.svelte'
  import { route, navigate } from '../lib/router.svelte'
  import Button from '../components/Button.svelte'
  import Input from '../components/Input.svelte'
  import Field from '../components/Field.svelte'
  import Mascot from '../components/Mascot.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import SigninHelp from '../components/SigninHelp.svelte'
  import AltCommand from '../components/AltCommand.svelte'
  import { KeyRound, Monitor, Terminal } from '../lib/icons'

  // The console is opened by the launcher: `masamong open` on this computer, or `masamong connect`
  // over SSH for a server. Both sign the browser in with a one-time link. Pasting the token from the
  // data folder is the fallback for a browser opened by hand.
  let { afterLogin }: { afterLogin: () => Promise<void> } = $props()
  let token = $state('')
  const signedOut = route.query.get('signed_out') === '1'
  const openCmd = $derived(session.value?.signin?.open ?? 'masamong open')
  let showToken = $state(false)
  let error = $state(route.query.get('error') === 'token' ? t('login.link_expired') : route.query.get('error') === 'rate' ? t('login.rate') : '')
  let busy = $state(false)
  let field: HTMLInputElement | undefined = $state()

  $effect(() => {
    if (session.value?.authenticated) void go()
  })
  $effect(() => {
    if (showToken) field?.focus()
  })
  async function go() {
    await afterLogin()
    const next = route.query.get('next')
    navigate(next && next.startsWith('/') && !next.startsWith('//') ? next : '/', { replace: true })
  }
  async function submit(e: SubmitEvent) {
    e.preventDefault()
    error = ''
    if (!token.trim()) return void ((error = t('login.token_required')), field?.focus())
    busy = true
    const r = await login(token.trim())
    busy = false
    if (r.ok) await go()
    else {
      error = r.status === 429 ? t('login.rate') : t('login.wrong')
      field?.focus()
    }
  }
  useTitle(() => t('login.title'))
</script>

<main class="wrap" id="main">
  <div class="card">
    <div class="art enter"><Mascot pose="hello" size={120} /></div>
    <h1 class="enter" style:--i="1">{t('login.title')}</h1>
    <p class="lead enter" style:--i="2">{t('login.lead')}</p>
    {#if error && !showToken}<p class="err small" role="alert">{error}</p>
    {:else if signedOut}<p class="bye small" role="status">{t('signin.signed_out')}</p>{/if}
    <ol class="ways enter" style:--i="3">
      <li><Monitor size={18} aria-hidden="true" /><div><strong>{t('login.here')}</strong><span class="small muted">{t('login.here_how')}</span>{#if session.value?.signin?.open_cmd}<span class="xs muted">{t('signin.ps')}</span>{/if}<span class="cmd"><code class="mono small">{openCmd}</code><CopyButton text={openCmd} /></span>{#if session.value?.signin?.open_cmd}<AltCommand text={session.value.signin.open_cmd} />{/if}</div></li>
      <li><Terminal size={18} aria-hidden="true" /><div><strong>{t('login.remote')}</strong><span class="small muted">{t('login.remote_how')}</span><span class="cmd"><code class="mono small">masamong connect you@server</code><CopyButton text="masamong connect you@server" /></span></div></li>
    </ol>
    {#if showToken}
      <form onsubmit={submit} class="enter" novalidate>
        <Field id="token" label={t('login.token')} error={error || undefined}>
          {#snippet children({ id, describedby, invalid })}
            <Input {id} bind:el={field} type="password" bind:value={token} autocomplete="current-password" aria-describedby={describedby} {invalid} />
          {/snippet}
        </Field>
        <SigninHelp only="token" />
        <Button type="submit" variant="primary" size="lg" full loading={busy} icon={KeyRound}>{t('login.submit')}</Button>
      </form>
    {:else}
      <button type="button" class="alt-link small enter" style:--i="4" onclick={() => (showToken = true)}>{t('login.use_token')}</button>
    {/if}
  </div>
</main>

<style>
  .wrap {
    min-height: 100dvh;
    display: grid;
    place-items: center;
    padding: var(--s-6) var(--s-4);
    background: radial-gradient(1200px 600px at 50% -10%, var(--accent-soft), transparent 60%), var(--bg);
  }
  .card {
    width: min(460px, 100%);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--s-3);
    text-align: center;
    padding: var(--s-8) var(--s-6);
    border-radius: 24px;
    background: var(--surface);
    box-shadow: var(--shadow-pop);
  }
  .lead {
    color: var(--text-muted);
    font-size: var(--text-sm);
    text-wrap: pretty;
  }
  .ways {
    list-style: none;
    margin: var(--s-2) 0 0;
    padding: 0;
    width: 100%;
    display: grid;
    gap: var(--s-2);
    text-align: start;
  }
  .ways li {
    display: flex;
    gap: var(--s-3);
    padding: var(--s-3);
    border-radius: var(--r-lg);
    background: var(--surface-2);
  }
  .ways li :global(svg) {
    flex: none;
    margin-top: 2px;
    color: var(--accent-text);
  }
  .ways li div {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
  }
  .cmd {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }
  form {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: var(--s-4);
    margin-top: var(--s-2);
    text-align: start;
  }
  .alt-link {
    margin-top: var(--s-2);
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-muted);
    text-decoration: underline;
    text-underline-offset: 2px;
    cursor: pointer;
  }
  .err {
    color: var(--danger);
  }
  .bye {
    padding: 8px 12px;
    border-radius: var(--r);
    background: var(--ok-soft);
    color: var(--ok-text, var(--text));
  }
  .cmd code {
    overflow-wrap: anywhere;
  }
</style>
