<script lang="ts">
  import { t, i18n, languageLabel, switchLanguage } from '../lib/i18n.svelte'
  import { session } from '../lib/api.svelte'
  import { setPrefs } from '../lib/prefs.svelte'
  import { toast } from '../lib/toast.svelte'
  import Select from './Select.svelte'

  // Every bundle in locales/ui/ is offered; a new <tag>.json appears here without code changes.
  let { id }: { id?: string } = $props()
  const options = $derived(
    (session.value?.locales?.length ? session.value.locales : ['ko', 'en'])
      .map((l) => ({ value: l, label: languageLabel(l) }))
      .sort((a, b) => a.label.localeCompare(b.label, i18n.lang)),
  )
  let busy = $state(false)
  async function change(e: Event) {
    const select = e.currentTarget as HTMLSelectElement
    const v = select.value
    busy = true
    try {
      await switchLanguage(v, (lang) => setPrefs({ lang }))
    } catch {
      select.value = i18n.lang
      toast('error', t('common.failed'))
    } finally {
      busy = false
    }
  }
</script>

<Select {id} aria-label={t('display.language')} value={i18n.lang} {options} onchange={change} disabled={busy} />
