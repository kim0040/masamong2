<script lang="ts">
  // the Discord connection: switch, token, who the bot is, gateway health, the invite link and the command settings
  import { t } from '../../lib/i18n.svelte'
  import { cfg, patchConfig } from '../../lib/config.svelte'
  import { toast } from '../../lib/toast.svelte'
  import { num, ms, duration } from '../../lib/format'
  import { checkBot as askBot, makeInvite as fetchInvite } from '../../lib/discord'
  import type { Connections } from '../../lib/connections.svelte'
  import Card from '../Card.svelte'
  import Button from '../Button.svelte'
  import Status from '../Status.svelte'
  import Field from '../Field.svelte'
  import Input from '../Input.svelte'
  import Switch from '../Switch.svelte'
  import CopyButton from '../CopyButton.svelte'
  import * as I from '../../lib/icons'

  let { conn, onkey }: { conn: Connections; onkey: (id: string) => void } = $props()
  const channels = $derived(conn.channels)

  async function setDiscord(key: string, value: unknown) {
    await patchConfig([{ path: ['discord', key], value }], { success: t('conn.discord_saved') })
  }

  // gateway health (read-only): the heartbeat decides the state; event age is only a hint, a quiet server is normal
  const gw = $derived(channels.data?.live ? channels.data.gateway : undefined)
  const gwTone = $derived(gw?.state === 'ok' ? 'ok' : gw?.state === 'check' ? 'warn' : 'neutral')
  const gwAge = (sec: number | undefined) => (sec === undefined || sec < 0 ? t('conn.gw.none') : t('conn.gw.ago', { when: sec < 60 ? t('conn.gw.secs', { n: num(sec) }) : duration(sec) }))

  let invite = $state('')
  async function makeInvite() {
    const r = await fetchInvite()
    if (r.url) invite = r.url
    else toast('warn', t('conn.invite_failed'), r.message || undefined)
  }
  // who the stored token belongs to, asked from Discord (the bot id is stored for the invite link)
  let bot = $state<any>(null)
  let botBusy = $state(false)
  let botErr = $state('')
  export async function checkBot() {
    botBusy = true
    botErr = ''
    const checked = await askBot()
    botBusy = false
    bot = checked.bot
    if (checked.message !== undefined) botErr = checked.message || t('common.failed')
  }
</script>

<Card title={t('conn.discord')} description={t('conn.discord_desc')}>
  {#snippet actions()}<Status tone={conn.discordStatus.tone} label={conn.discordStatus.text} />{/snippet}
  {#if cfg.loaded}
    <div class="dgrid">
      <div class="stack">
        <h3 class="sub-title">{t('conn.discord_conn')}</h3>
        <Switch checked={!!cfg.doc.discord?.enabled} onchange={(v) => setDiscord('enabled', v)} label={t('conn.discord_enabled')} hint={t('conn.restart_needed')} />
        <div class="row"><span class="small grow">{t('conn.token')}: <span class="muted">{conn.refKind(cfg.doc.discord?.token_ref)}</span></span><Button size="sm" icon={I.KeyRound} onclick={() => onkey('discord')}>{t('conn.set_token')}</Button></div>
        <div class="bot">
          <div class="row"><span class="small grow">{bot ? t('conn.bot_found', { name: bot.bot_name, id: bot.bot_id }) : botBusy ? t('conn.bot_checking') : t('conn.bot_none')}</span><Button size="sm" variant="ghost" icon={I.RefreshCw} loading={botBusy} disabled={!channels.data?.discord_token} onclick={checkBot}>{t('conn.bot_check')}</Button></div>
          {#if botErr}<p class="xs err" role="alert">{botErr}</p>{/if}
          {#if bot}
            {#if bot.message_content_blocked}<div class="callout danger tight"><span class="ic"><I.CircleAlert size={14} /></span><span class="xs">{t('conn.intent_blocked')} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a></span></div>
            {:else}<p class="xs muted">{bot.message_content_portal ? t('conn.intent_on') : t('conn.intent_off')}{#if !bot.message_content_portal} <a href={bot.portal_url} target="_blank" rel="noopener">{t('conn.intent_portal')}</a>{/if}</p>{/if}
          {/if}
        </div>
        {#if gw}
          <div class="gw">
            <div class="row"><span class="small grow">{t('conn.gw.title')}</span><Status tone={gwTone} size="sm" label={t(`conn.gw.${gw.state}`)} /><Button size="sm" variant="ghost" icon={I.RefreshCw} loading={channels.loading} onclick={() => void channels.refresh()}>{t('conn.gw.refresh')}</Button></div>
            {#if gw.state === 'check'}<div class="callout warn tight" role="status"><span class="ic"><I.TriangleAlert size={14} /></span><span class="xs">{t('conn.gw.check_hint')}</span></div>
            {:else if gw.state === 'unknown'}<p class="xs muted">{t('conn.gw.unknown_hint')}</p>{/if}
            <dl class="health xs">
              <dt>{t('conn.gw.latency')}</dt><dd class="num">{gw.latency_ms >= 0 ? ms(gw.latency_ms) : t('conn.gw.none')}</dd>
              <dt>{t('conn.gw.ack')}</dt><dd class="num" class:old={gw.state === 'check'}>{gwAge(gw.ack_age_sec)}</dd>
              <dt>{t('conn.gw.event')}</dt><dd class="num" title={t('conn.gw.event_hint')}>{gwAge(gw.event_age_sec)}</dd>
              <dt>{t('conn.gw.sessions')}</dt><dd class="num">{t('conn.gw.sessions_v', { ready: num(gw.ready_count), resumed: num(gw.resumed_count), down: num(gw.disconnect_count) })}</dd>
            </dl>
          </div>
        {/if}
        {#if invite}<div class="row"><a href={invite} target="_blank" rel="noopener" class="small break grow">{invite}</a><CopyButton text={invite} /></div>{:else}<div class="row"><Button size="sm" icon={I.Link} onclick={makeInvite}>{t('room.invite_make')}</Button></div>{/if}
      </div>
      <div class="stack">
        <h3 class="sub-title">{t('conn.discord_cmds')}</h3>
        <Field id="pfx" label={t('conn.prefix')} hint={t('conn.prefix_hint')}>{#snippet children({ id, describedby })}<div class="narrow"><Input {id} aria-describedby={describedby} value={cfg.doc.discord?.command_prefix ?? '!'} maxlength={3} onchange={(e) => setDiscord('command_prefix', (e.currentTarget as HTMLInputElement).value)} /></div>{/snippet}</Field>
        <Switch checked={!!cfg.doc.discord?.register_commands} onchange={(v) => setDiscord('register_commands', v)} label={t('conn.slash')} hint={t('conn.slash_hint')} />
        <Switch checked={!!cfg.doc.discord?.message_content} onchange={(v) => setDiscord('message_content', v)} label={t('conn.content_intent')} hint={t('conn.content_intent_hint')} />
      </div>
    </div>
  {/if}
</Card>

<style>
  .sub-title {
    margin: var(--s-5) 0 var(--s-2);
    font-size: var(--text-sm);
    font-weight: 650;
  }
  .stack > .sub-title {
    margin: 0;
  }
  .dgrid {
    display: grid;
    gap: var(--s-5) var(--s-8);
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 340px), 1fr));
    align-items: start;
  }
  .narrow {
    max-width: 8rem;
  }
  .err {
    color: var(--danger);
  }
  .grow {
    flex: 1 1 240px;
    min-width: 0;
  }
  .bot {
    display: flex;
    flex-direction: column;
    gap: var(--s-2);
    padding: var(--s-3);
    border-radius: var(--r);
    background: var(--surface-2);
  }
  .gw {
    display: grid;
    gap: var(--s-2);
  }
  .health {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 2px 10px;
    margin: 0;
    color: var(--text-muted);
  }
  .health dd {
    margin: 0;
    color: var(--text);
  }
  .health dd.old {
    color: var(--warn);
    font-weight: 600;
  }
  .tight {
    padding: 6px 10px;
  }
</style>
