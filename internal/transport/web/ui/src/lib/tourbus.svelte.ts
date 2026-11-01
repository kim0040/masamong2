// Asking for the current page's tour (the "?" in a page header). Tour.svelte watches seq.
export const tourBus = $state({ seq: 0 })

export function askTour() {
  tourBus.seq++
}
