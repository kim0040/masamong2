<script lang="ts">
  import EmptyState from '../components/EmptyState.svelte'
  import { useTitle } from '../lib/title.svelte'
  import Button from '../components/Button.svelte'
  import { t } from '../lib/i18n.svelte'
  useTitle(() => t('notfound.title'))
</script>

<EmptyState level={1} pose="lost" title={t('notfound.title')} description={t('notfound.body')}>
  <Button href="/" variant="primary">{t('notfound.home')}</Button>
</EmptyState>
