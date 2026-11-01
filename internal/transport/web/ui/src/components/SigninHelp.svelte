<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { session } from '../lib/api.svelte'
  import CopyButton from './CopyButton.svelte'
  import AltCommand from './AltCommand.svelte'
  import { Monitor, KeyRound } from '../lib/icons'

  // The two ways back in from a terminal on the computer running Masamong. The server fills in this
  // installation's program and data folder, so the copied command reaches this instance.
  let { only }: { only?: 'open' | 'token' } = $props()
  const cmds = $derived<{ open: string; token: string; open_cmd?: string; token_cmd?: string }>(session.value?.signin ?? { open: 'masamong open', token: 'masamong admin token' })
</script>

<ul class="sh">
  {#if only !== 'token'}
    <li>
      <Monitor size={16} aria-hidden="true" />
      <div><span class="small">{t('signin.open')}</span>{#if cmds.open_cmd}<span class="xs muted">{t('signin.ps')}</span>{/if}<span class="cmd"><code class="mono xs">{cmds.open}</code><CopyButton text={cmds.open} /></span>{#if cmds.open_cmd}<AltCommand text={cmds.open_cmd} />{/if}</div>
    </li>
  {/if}
  {#if only !== 'open'}
    <li>
      <KeyRound size={16} aria-hidden="true" />
      <div><span class="small">{t('signin.token')}</span>{#if cmds.token_cmd}<span class="xs muted">{t('signin.ps')}</span>{/if}<span class="cmd"><code class="mono xs">{cmds.token}</code><CopyButton text={cmds.token} /></span>{#if cmds.token_cmd}<AltCommand text={cmds.token_cmd} />{/if}</div>
    </li>
  {/if}
</ul>

<style>
  .sh {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: var(--s-2);
    text-align: start;
  }
  li {
    display: flex;
    gap: var(--s-2);
    align-items: flex-start;
  }
  li > :global(svg) {
    flex: none;
    margin-top: 3px;
    color: var(--accent-text);
  }
  li div {
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
  code {
    overflow-wrap: anywhere;
  }
</style>
