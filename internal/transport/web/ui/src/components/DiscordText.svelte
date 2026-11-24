<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { parseDiscord, hasMarkup, type DNode } from '../lib/discordtext'
  import { tail } from '../lib/rooms'

  // Stored messages and answers hold Discord's markup; this draws it (see lib/discordtext.ts) and puts a name
  // on every mention. `mark` (optional) highlights the search words in the plain text. `still` is for text drawn
  // inside a button (a preview you press to select): a spoiler is shown uncovered, marked as a spoiler, and is not a
  // button of its own (the console is the operator's view; the preview is the only place a Lab answer is read).
  let { text, mark, still = false }: { text: string; mark?: (s: string) => { t: string; m: boolean }[]; still?: boolean } = $props()
  const nodes = $derived(hasMarkup(text) ? parseDiscord(text) : ([{ k: 'text', v: text ?? '' }] as DNode[]))
  let shown = $state<Record<string, boolean>>({})
</script>

{#snippet draw(list: DNode[], path: string)}
  {#each list as n, i (path + '.' + i)}
    {#if n.k === 'text'}{#if mark}{#each mark(n.v) as part}{#if part.m}<mark>{part.t}</mark>{:else}{part.t}{/if}{/each}{:else}{n.v}{/if}
    {:else if n.k === 'bold'}<strong>{@render draw(n.c, path + '.' + i)}</strong>
    {:else if n.k === 'italic'}<em>{@render draw(n.c, path + '.' + i)}</em>
    {:else if n.k === 'underline'}<u>{@render draw(n.c, path + '.' + i)}</u>
    {:else if n.k === 'strike'}<s>{@render draw(n.c, path + '.' + i)}</s>
    {:else if n.k === 'quote'}<span class="quote">{@render draw(n.c, path + '.' + i)}</span>
    {:else if n.k === 'spoiler' && still}<span class="spoiler open" title={t('dt.spoiler')}>{@render draw(n.c, path + '.' + i)}</span>
    {:else if n.k === 'spoiler'}<button type="button" class="spoiler" class:open={shown[path + '.' + i]} aria-pressed={!!shown[path + '.' + i]} aria-label={shown[path + '.' + i] ? undefined : t('dt.spoiler')} onclick={() => (shown[path + '.' + i] = !shown[path + '.' + i])}><span aria-hidden={!shown[path + '.' + i]}>{@render draw(n.c, path + '.' + i)}</span></button>
    {:else if n.k === 'code'}{#if n.block}<code class="block">{n.v}</code>{:else}<code>{n.v}</code>{/if}
    {:else if n.k === 'user'}<span class="ref">@{t('dt.user', { tail: tail(n.id) })}</span>
    {:else if n.k === 'role'}<span class="ref">@{t('dt.role', { tail: tail(n.id) })}</span>
    {:else if n.k === 'channel'}<span class="ref">#{t('dt.channel', { tail: tail(n.id) })}</span>
    {:else if n.k === 'emoji'}<span class="ref">:{n.name}:</span>
    {/if}
  {/each}
{/snippet}

<span class="dt">{@render draw(nodes, 'r')}</span>

<style>
  .dt {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  code {
    padding: 0 4px;
    border-radius: 4px;
    background: var(--surface-3);
    font-family: var(--mono);
    font-size: 0.92em;
  }
  code.block {
    display: block;
    margin: 4px 0;
    padding: 6px 8px;
  }
  .quote {
    display: block;
    padding-inline-start: 8px;
    border-inline-start: 2px solid var(--border-strong, var(--border));
    color: var(--text-muted);
  }
  .ref {
    padding: 0 3px;
    border-radius: 4px;
    background: var(--accent-soft);
    color: var(--accent-text);
  }
  .spoiler {
    padding: 0 3px;
    border: 0;
    border-radius: 4px;
    background: var(--text);
    color: transparent;
    font: inherit;
    cursor: pointer;
  }
  .spoiler.open {
    background: var(--surface-3);
    color: inherit;
  }
  mark {
    background: var(--accent-soft-hover);
    color: inherit;
    border-radius: 3px;
    padding: 0 2px;
  }
</style>
