// Saving a key or token typed into the console. The value goes to POST /api/secret, which stores it
// (never in the config, never echoed back) and points the provider, the Discord token or a tool at it.
// Four screens used to build this request and read its answer on their own (Connections, the setup
// wizard twice, the new-tool wizard) and read it differently.
import { postJSON } from './api.svelte'

export type SecretTarget = { target: 'discord' } | { target: 'provider'; id: string } | { target: 'file'; id: string }

export type SecretSaved = {
  ok: boolean
  /** the server's reason when it did not save; '' when it gave none (the screen words its own) */
  message: string
  /** where the key now lives (a stored_in value; the screen words it with conn.stored_in.*) */
  storedIn: string
  /** the reference a tool saves with itself (target "file" only) */
  credentialRef: string
}

/** Stores `value` (trimmed) for `target`. Never throws: a network failure is a result with ok false. */
export async function saveSecret(target: SecretTarget, value: string): Promise<SecretSaved> {
  const res = await postJSON<any>('/api/secret', { ...target, value: value.trim() })
  const body = res.body ?? {}
  // the answer is an Action result (provider, Discord: output.stored_in) or a plain object (file)
  const ok = res.status === 200 && body.ok !== false
  return {
    ok,
    message: ok ? '' : String(body.error?.message ?? ''),
    storedIn: String(body.output?.stored_in ?? body.stored_in ?? ''),
    credentialRef: String(body.credential_ref ?? ''),
  }
}

/** The shape a typed key must have as a reference (env:NAME, file:PATH, and keychain:NAME where there is one). */
export function validRef(ref: string, keychain: boolean): boolean {
  return (keychain ? /^(env|file|keychain):.+/ : /^(env|file):.+/).test(ref)
}
