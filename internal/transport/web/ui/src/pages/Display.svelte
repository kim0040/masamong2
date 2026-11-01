<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { session, can, loadSession } from '../lib/api.svelte'
  import { patchConfig } from '../lib/config.svelte'
  import { prefs, setPrefs, densities } from '../lib/prefs.svelte'
  import { navigate } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../components/PageHeader.svelte'
  import Card from '../components/Card.svelte'
  import Button from '../components/Button.svelte'
  import Segmented from '../components/Segmented.svelte'
  import LanguageSelect from '../components/LanguageSelect.svelte'
  import AltCommand from '../components/AltCommand.svelte'
  import CopyButton from '../components/CopyButton.svelte'
  import SigninHelp from '../components/SigninHelp.svelte'
  import LogoutConfirm from '../components/LogoutConfirm.svelte'
  import * as I from '../lib/icons'

  const connect = 'masamong connect you@server'
  // without the helper: forward the console port over SSH by hand
  const port = $derived((session.value?.bind ?? '127.0.0.1:8787').split(':').pop())
  const sshCmd = $derived(`ssh -N -L ${port}:127.0.0.1:${port} <user>@<server>`)
  let logoutOpen = $state(false)

  // Tailscale: tailscale serve forwards the tailnet name to this port, and the console answers to the
  // names listed in admin.web.hosts
  const hosts = $derived<string[]>(session.value?.hosts ?? [])
  const tailnet = $derived(session.value?.tailnet ?? '')
  const serveCmd = $derived(`tailscale serve --bg --http=${port} http://127.0.0.1:${port}`)
  let saving = $state(false)
  // the service looks the name up in the background the first time it is asked: ask again a few times
  let looked = $state(false)
  $effect(() => {
    if (tailnet || looked) return
    let tries = 0
    const timer = setInterval(() => {
      tries++
      void loadSession().catch(() => {})
      if (tries >= 4) {
        clearInterval(timer)
        looked = true
      }
    }, 1500)
    return () => clearInterval(timer)
  })
  async function setHosts(next: string[], success: string) {
    saving = true
    const r = await patchConfig([next.length ? { path: ['admin', 'web', 'hosts'], value: next } : { path: ['admin', 'web', 'hosts'], op: 'unset' }], { success, base: session.value?.revision })
    if (r.ok) await loadSession()
    saving = false
  }
</script>

<PageHeader title={t('display_page.title')} description={t('display_page.desc')} />

<div class="cols">
  <Card class="a-look" title={t('display_page.look')} description={t('display_page.look_desc')}>
    <div class="stack">
      <div><div class="section-title">{t('display.theme')}</div><div class="mt"><Segmented label={t('display.theme')} value={prefs.theme} onchange={(v) => setPrefs({ theme: v })} options={[{ value: 'light', label: t('display.light'), icon: I.Sun }, { value: 'dark', label: t('display.dark'), icon: I.Moon }, { value: 'system', label: t('display.system'), icon: I.Monitor }]} /></div></div>
      <div>
        <div class="section-title">{t('display.density')}</div>
        <div class="dens mt" role="radiogroup" aria-label={t('display.density')}>
          {#each densities as d}
            <label class="den {d}" class:on={prefs.density === d}>
              <input type="radio" name="density" value={d} checked={prefs.density === d} onchange={() => setPrefs({ density: d })} />
              <span class="sample" aria-hidden="true"><span class="l1"></span><span class="l2"></span><span class="l3"></span></span>
              <span class="dn">{t(`display.density.${d}`)}</span>
              <span class="dd xs muted">{t(`display.density.${d}_desc`)}</span>
            </label>
          {/each}
        </div>
      </div>
      <div><div class="section-title">{t('display.motion')}</div><div class="mt"><Segmented label={t('display.motion')} value={prefs.motion} onchange={(v) => setPrefs({ motion: v })} options={[{ value: 'full', label: t('display.motion_full') }, { value: 'reduced', label: t('display.motion_reduced') }, { value: 'system', label: t('display.system') }]} /></div></div>
    </div>
  </Card>

  <Card class="a-lang" title={t('display.language')} description={t('display_page.lang_desc')}>
    <div class="stack">
      <LanguageSelect id="lang" />
      <p class="xs muted">{t('display_page.lang_add', { path: 'locales/ui/<tag>.json' })}</p>
      <p class="xs muted">{t('display_page.lang_bot')} <a href="/persona">{t('nav.persona')}</a></p>
    </div>
  </Card>

  <Card class="a-access" title={t('display_page.access')} description={t('display_page.access_desc')}>
    <div class="stack">
      <div class="access"><I.Monitor size={16} aria-hidden="true" /><span class="small">{t('display_page.access_local')}</span>{#if session.value?.signin?.open_cmd}<span class="xs muted">{t('signin.ps')}</span>{/if}<span class="cmd"><code class="mono xs">{session.value?.signin?.open ?? 'masamong open'}</code><CopyButton text={session.value?.signin?.open ?? 'masamong open'} /></span>{#if session.value?.signin?.open_cmd}<AltCommand text={session.value.signin.open_cmd} />{/if}</div>
      <div class="access"><I.Terminal size={16} aria-hidden="true" /><span class="small">{t('display_page.access_ssh')}</span><span class="cmd"><code class="mono xs">{connect}</code><CopyButton text={connect} /></span></div>
      <div class="access tn">
        <I.Globe size={16} aria-hidden="true" /><span class="small">{t('display_page.tailnet')}</span>
        {#if tailnet}<span class="xs muted">{t('display_page.tailnet_found', { name: tailnet })}</span>{:else if looked}<span class="xs muted">{t('display_page.tailnet_none')}</span>{/if}
        {#if tailnet && !hosts.includes(tailnet) && can('config.patch')}<span><Button size="sm" variant="primary" icon={I.Globe} disabled={saving} onclick={() => setHosts([...hosts, tailnet], t('display_page.tailnet_on'))}>{t('display_page.tailnet_allow')}</Button></span>{/if}
        {#if hosts.length}
          <span class="xs muted">{t('display_page.tailnet_serve')}</span>
          <span class="cmd"><code class="mono xs">{serveCmd}</code><CopyButton text={serveCmd} /></span>
          <span class="xs muted">{t('display_page.tailnet_links')}</span>
          {#each hosts as h (h)}
            {@const link = `http://${h}:${port}/`}
            <span class="cmd"><code class="mono xs">{link}</code><CopyButton text={link} />{#if can('config.patch')}<Button size="sm" variant="ghost" disabled={saving} onclick={() => setHosts(hosts.filter((x) => x !== h), t('display_page.tailnet_off'))}>{t('display_page.tailnet_remove')}</Button>{/if}</span>
          {/each}
        {/if}
      </div>
      <p class="xs muted">{t('display_page.access_note', { bind: session.value?.bind ?? '127.0.0.1:8787' })}</p>
      <details>
        <summary class="xs muted">{t('conn.remote_manual')}</summary>
        <div class="access"><I.Terminal size={16} aria-hidden="true" /><span class="small">{t('conn.remote_manual_hint', { port })}</span><span class="cmd"><code class="mono xs">{sshCmd}</code><CopyButton text={sshCmd} /></span></div>
      </details>
      <div class="callout"><span class="ic"><I.ShieldCheck size={16} /></span><span class="small">{t('conn.remote_safety')}</span></div>
      <details>
        <summary class="xs muted">{t('display_page.token')}</summary>
        <div class="tk"><SigninHelp only="token" /></div>
      </details>
      <div><Button variant="ghost" icon={I.LogOut} onclick={() => (logoutOpen = true)}>{t('topbar.logout')}</Button></div>
    </div>
  </Card>

  <Card class="a-tut" title={t('admins.tutorial')} description={t('admins.tutorial_desc')}>
    <div class="row">
      {#if can('setup.resume')}<Button icon={I.BookOpen} onclick={() => navigate('/setup?tour=1')}>{t('admins.tutorial_again')}</Button>{/if}
      <Button variant="ghost" icon={I.RotateCcw} onclick={() => { setPrefs({ tutorial: {}, tips: {} }); toast('ok', t('admins.tutorial_reset')) }}>{t('admins.tutorial_reset_btn')}</Button>
    </div>
  </Card>
</div>

<LogoutConfirm bind:open={logoutOpen} />

<style>
  .tk {
    margin-top: var(--s-2);
  }
  .cols {
    display: grid;
    gap: var(--s-4);
    grid-template-columns: minmax(0, 1fr);
  }
  /* the two short cards stack beside the display settings; access instructions get a full row */
  @media (min-width: 1000px) {
    .cols {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      grid-template-areas: 'look lang' 'look tut' 'access access';
    }
    .cols > :global(.a-look) { grid-area: look; }
    .cols > :global(.a-lang) { grid-area: lang; }
    .cols > :global(.a-tut) { grid-area: tut; }
    .cols > :global(.a-access) { grid-area: access; }
  }
  .mt {
    margin-top: 8px;
  }
  .dens {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--s-2);
  }
  .den {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 12px;
    border-radius: var(--r-lg);
    box-shadow: var(--shadow-border);
    cursor: pointer;
    min-width: 0;
  }
  .den.on {
    background: var(--accent-soft);
    box-shadow: 0 0 0 1.5px var(--accent);
  }
  .den input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }
  .den:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .dn {
    font-weight: 650;
  }
  .sample {
    display: flex;
    flex-direction: column;
    height: 44px;
    justify-content: center;
  }
  .sample span {
    display: block;
    height: 5px;
    border-radius: 3px;
    background: var(--border-strong);
  }
  .den.compact .sample {
    gap: 3px;
  }
  .den.compact .sample span {
    height: 4px;
  }
  .den.normal .sample {
    gap: 6px;
  }
  .den.large .sample {
    gap: 9px;
  }
  .den.large .sample span {
    height: 7px;
  }
  .l2 {
    width: 80%;
  }
  .l3 {
    width: 55%;
  }
  .access {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: 4px 10px;
    align-items: center;
  }
  .access code,
  .access .cmd,
  .access :global(.alt) {
    grid-column: 2;
  }
  .tn > :global(:nth-child(n + 3)) {
    grid-column: 2;
  }
  .cmd {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }
  .cmd code {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  details summary {
    cursor: pointer;
  }
</style>
