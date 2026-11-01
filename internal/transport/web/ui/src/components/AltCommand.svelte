<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import CopyButton from './CopyButton.svelte'

  // The same command for Command Prompt, shown under the PowerShell one on Windows when the two differ
  // (a quoted program path needs & in PowerShell and has none in cmd).
  let { text }: { text: string } = $props()
</script>

<span class="alt"><span class="xs muted">{t('signin.cmd')}</span><span class="cmd"><code class="mono xs">{text}</code><CopyButton text={text} /></span></span>

<style>
  .alt {
    display: flex;
    flex-direction: column;
    gap: 2px;
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
