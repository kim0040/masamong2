// The browser tab, history and screen readers name a page by its document title ("Tools · Masamong"),
// so each page sets it while it is on screen; leaving it restores the plain app name.
import { t } from './i18n.svelte'

export function useTitle(get: () => string | undefined) {
  $effect(() => {
    const page = get()?.trim()
    const name = t('app.name')
    document.title = page ? `${page} · ${name}` : name
    return () => {
      document.title = name
    }
  })
}
