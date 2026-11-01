<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { live } from '../lib/live.svelte'
  import { tp } from '../lib/platform'
  import Mascot from './Mascot.svelte'
  import SigninHelp from './SigninHelp.svelte'

  // Shown instead of the console once the service stops: nothing here can start it again, so the page says
  // how to (this installation's command) and reloads by itself when the service answers.
  const done = $derived(live.lifecycle === 'stopped')
</script>

<main class="stopped" aria-live="polite">
  <Mascot pose="sleep" size={140} />
  {#if done}
    <h1>{t('stopped.title')}</h1>
    <p class="muted">{t('stopped.body', { tray: tp('platform.tray') })}</p>
    <div class="how"><SigninHelp only="open" /></div>
    <p class="small muted wait"><span class="dot" aria-hidden="true"></span>{t('stopped.waiting')}</p>
  {:else}
    <h1>{t('maint.stopping')}</h1>
    <p class="muted">{t('stopped.stopping_body')}</p>
  {/if}
</main>

<style>
  .stopped {
    min-height: 100dvh;
    display: grid;
    place-content: center;
    justify-items: center;
    text-align: center;
    gap: var(--s-3);
    padding: var(--s-6) var(--s-4);
    max-width: 34rem;
    margin: 0 auto;
  }
  h1 {
    margin: 0;
    font-size: var(--text-xl);
  }
  p {
    margin: 0;
  }
  .how {
    width: 100%;
    padding: var(--s-3) var(--s-4);
    border: 1px solid var(--border);
    border-radius: var(--r-lg);
    background: var(--surface);
  }
  .wait {
    display: inline-flex;
    align-items: center;
    gap: var(--s-2);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--warn, currentColor);
    animation: pulse 1.6s ease-in-out infinite;
  }
  @keyframes pulse {
    50% {
      opacity: 0.3;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .dot {
      animation: none;
    }
  }
</style>
