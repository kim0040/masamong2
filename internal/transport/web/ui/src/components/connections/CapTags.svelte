<script lang="ts">
  // what the model list says a model can do; nothing is shown for what it does not say
  import { t } from '../../lib/i18n.svelte'
  import { reasonKey } from '../../lib/connections'

  let { m, chat, vision = false }: { m: any; chat: boolean; vision?: boolean } = $props()
</script>

{#if m.image_input || m.supports_tools || m.known_family || (chat && m.supports_tools === false) || m.price_source === 'published' || m.price_source === 'reference' || (vision && m.image_input === undefined) || m.reasoning !== undefined}
  <div class="tags">
    {#if m.reasoning !== undefined}<span class="tag" class:dim={!m.reasoning}>{t(reasonKey(m))}</span>{/if}
    {#if m.image_input}<span class="tag ok">{t('conn.tag_image_in')}</span>{:else if vision && m.image_input === undefined}<span class="tag dim">{t('conn.tag_image_unknown')}</span>{/if}
    {#if m.supports_tools}<span class="tag">{t('conn.tag_tools')}</span>{:else if chat && m.supports_tools === false}<span class="tag warn">{t('conn.tag_no_tools')}</span>{/if}
    {#if m.known_family}<span class="tag">{t('conn.tag_known')}</span>{/if}
    {#if m.price_source === 'published' || m.price_source === 'reference'}<span class="tag dim">{t(`conn.tag_price_${m.price_source}`)}</span>{/if}
  </div>
{/if}

<style>
  .tags {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 4px;
  }
  .tag {
    padding: 1px 7px;
    border-radius: 999px;
    background: var(--surface-2);
    color: var(--text-muted);
    font-size: var(--text-xs);
    white-space: nowrap;
  }
  .tag.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tag.dim {
    background: none;
    box-shadow: var(--shadow-border);
  }
  .tag.warn {
    background: var(--warn-soft);
    color: var(--warn);
  }
</style>
