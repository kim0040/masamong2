// The console speaks only Application Actions (POST /api/action) plus a few transport endpoints.
// Session state lives here; a 401 anywhere sends the app to the sign-in screen.

export type ActionError = { code: string; message: string; message_key?: string; args?: Record<string, unknown> }
export type ActionResult<T = any> = { ok: boolean; action: string; revision?: number; output?: T; error?: ActionError }

export type Session = {
  authenticated: boolean
  csrf?: string
  lang: string
  locales: string[]
  ready: boolean
  version: string
  instance?: string
  revision?: number
  data_root?: string
  platform?: string // the system the service runs on: windows | darwin | linux
  bind?: string
  hosts?: string[] // admin.web.hosts: extra names the console answers to (tailscale serve)
  tailnet?: string // this computer's Tailscale name, when Tailscale runs here
  bot_names?: string[]
  timezone?: string
  practice?: { open: boolean }
  signin?: { open: string; token: string; open_cmd?: string; token_cmd?: string }
}

export const session = $state<{ value: Session | null; offline: boolean }>({ value: null, offline: false })

/** Protected dashboard actions only exist after sign-in and setup are both complete. Practice mode is its own
 * instance that is ready from the start, so a tab in practice mode does not wait for the real instance's setup
 * (the first screen offers practice mode before any setup exists). */
export function canLoadProtectedActions(value: Session | null): boolean {
  return !!value?.authenticated && (value.ready || practice.on)
}

// Practice mode sends every request to the separate practice instance (X-Masamong-Practice). The flag
// lives in this tab only, so another tab stays on the real instance.
function readPractice(): boolean {
  try {
    return sessionStorage.getItem('masamong.practice') === '1'
  } catch {
    return false
  }
}
export const practice = $state<{ on: boolean }>({ on: readPractice() })
if (practice.on) document.documentElement.setAttribute('data-practice', '')

/** Switches practice mode for this tab and reloads the app on the other instance. */
export async function setPractice(on: boolean, reset = false): Promise<boolean> {
  if (on) {
    const r = await postJSON('/api/practice', { do: reset ? 'reset' : 'open' })
    if (r.status !== 200) return false
  }
  try {
    if (on) sessionStorage.setItem('masamong.practice', '1')
    else sessionStorage.removeItem('masamong.practice')
  } catch {}
  location.assign('/')
  return true
}

/** can(action): there is one admin (whoever reaches the console through this machine or an SSH
 * tunnel), so every Action is allowed; the helper stays so pages read the same if that changes. */
export function can(_action: string): boolean {
  return true
}

let onUnauthorized: () => void = () => {}
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

let uiLang = 'ko'
export function setApiLang(l: string) {
  uiLang = l
}

async function request(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers)
  headers.set('X-Lang', uiLang)
  if (practice.on) headers.set('X-Masamong-Practice', '1')
  if (init.method && init.method !== 'GET' && session.value?.csrf) headers.set('X-CSRF-Token', session.value.csrf)
  try {
    const res = await fetch(path, { ...init, headers, credentials: 'same-origin' })
    session.offline = false
    if (res.status === 401 && !path.startsWith('/api/login') && !path.startsWith('/api/session')) onUnauthorized()
    return res
  } catch (e) {
    session.offline = true
    throw e
  }
}

export async function loadSession(): Promise<Session> {
  const res = await request('/api/session')
  const s = (await res.json()) as Session
  session.value = s
  return s
}

/** Signs in with the access token (the fallback when the browser was not opened by the launcher). */
export async function login(token: string): Promise<{ ok: boolean; error?: ActionError; status: number }> {
  const res = await request('/api/login', { method: 'POST', body: JSON.stringify({ token }), headers: { 'Content-Type': 'application/json' } })
  const body = await res.json()
  if (res.ok) await loadSession()
  return { ok: res.ok, error: body.error, status: res.status }
}

export async function logout() {
  await request('/api/logout', { method: 'POST' })
  await loadSession()
}

/** Runs an Action. Network failures come back as an error result, never as a thrown exception. */
export async function call<T = any>(action: string, input: Record<string, unknown> = {}, opts: { base_revision?: number } = {}): Promise<ActionResult<T>> {
  try {
    const res = await request('/api/action', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action, input, base_revision: opts.base_revision ?? 0 }),
    })
    const body = (await res.json()) as ActionResult<T>
    if (body.revision && session.value) session.value.revision = body.revision
    if (!body.ok && !body.error) body.error = { code: 'UNKNOWN', message: `HTTP ${res.status}` }
    if (!body.ok && body.error) body.error.message = withHint(body.error)
    return body
  } catch {
    const error = { code: 'OFFLINE', message: '' }
    return { ok: false, action, error: { ...error, message: withHint(error) } }
  }
}

// Server messages are technical and in English; each error code gets a localized sentence that says
// what to do, with the server detail kept after it.
let hintFor: (code: string) => string = () => ''
export function setErrorHints(fn: (code: string) => string) {
  hintFor = fn
}
function withHint(e: ActionError): string {
  const hint = hintFor(e.code)
  if (!hint) return e.message
  return e.message ? `${hint} (${e.message})` : hint
}

export async function get<T = any>(path: string): Promise<T> {
  const res = await request(path)
  return (await res.json()) as T
}

/** POSTs JSON to a transport endpoint. Like call(), it never throws: a network failure comes back as
 * status 0 and an OFFLINE error, a reply that is not JSON as its HTTP status with an UNKNOWN error, so a
 * form can always show a message and stop its spinner. */
export async function postJSON<T = any>(path: string, body: unknown): Promise<{ status: number; body: T }> {
  let res: Response
  try {
    res = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  } catch {
    const error = { code: 'OFFLINE', message: '' }
    return { status: 0, body: { ok: false, error: { ...error, message: withHint(error) } } as T }
  }
  try {
    return { status: res.status, body: (await res.json()) as T }
  } catch {
    return { status: res.status, body: { ok: false, error: { code: 'UNKNOWN', message: `HTTP ${res.status}` } } as T }
  }
}

/** Uploads one file with progress; resolves to the server-side path. */
export function upload(file: File, onProgress?: (pct: number) => void): Promise<{ ok: boolean; path?: string; error?: string }> {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/api/upload')
    if (session.value?.csrf) xhr.setRequestHeader('X-CSRF-Token', session.value.csrf)
    if (practice.on) xhr.setRequestHeader('X-Masamong-Practice', '1')
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress?.(Math.round((100 * e.loaded) / e.total))
    xhr.onload = () => {
      try {
        const b = JSON.parse(xhr.responseText)
        resolve(xhr.status === 200 ? { ok: true, path: b.path } : { ok: false, error: b.error?.message ?? `HTTP ${xhr.status}` })
      } catch {
        resolve({ ok: false, error: `HTTP ${xhr.status}` })
      }
    }
    xhr.onerror = () => resolve({ ok: false, error: 'offline' })
    const fd = new FormData()
    fd.append('file', file)
    xhr.send(fd)
  })
}
