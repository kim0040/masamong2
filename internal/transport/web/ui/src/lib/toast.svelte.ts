// Toasts: polite status messages. Errors and toasts with an action stay until dismissed.
export type Toast = { id: number; kind: 'ok' | 'error' | 'info' | 'warn'; title: string; body?: string; action?: { label: string; run: () => void }; sticky: boolean }

export const toasts = $state<Toast[]>([])
let seq = 0

export function toast(kind: Toast['kind'], title: string, body?: string, action?: Toast['action']) {
  const id = ++seq
  const sticky = kind === 'error' || !!action
  toasts.push({ id, kind, title, body, action, sticky })
  if (!sticky) setTimeout(() => dismiss(id), 4200)
  if (toasts.length > 5) toasts.splice(0, toasts.length - 5)
  return id
}

export function dismiss(id: number) {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
}
