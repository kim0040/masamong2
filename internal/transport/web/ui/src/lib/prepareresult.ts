// What the server's answer to "start preparing a room's memory" (memory.blocks.prepare with
// background) means for the person: a batch picked up again, a job that was already running, or a new
// job. The same answer is read on the Blocks list, a block's page and the import wizard.

export type PrepareStart = {
  /** the toast: its tone and its message key (a blocks.* string) */
  tone: 'ok' | 'info'
  key: 'blocks.batch_resumed' | 'blocks.already_running' | 'blocks.batch_started' | 'blocks.prepare_started'
  /** a provider batch was started or resumed by this answer (not one that was already running) */
  batch: boolean
}

export function prepareStart(out: { batch_resumed?: boolean; already_running?: boolean; mode?: string } | undefined): PrepareStart {
  if (out?.batch_resumed) return { tone: 'info', key: 'blocks.batch_resumed', batch: true }
  if (out?.already_running) return { tone: 'ok', key: 'blocks.already_running', batch: false }
  if (out?.mode === 'batch') return { tone: 'ok', key: 'blocks.batch_started', batch: true }
  return { tone: 'ok', key: 'blocks.prepare_started', batch: false }
}

/** The answer says something the person may not expect (a provider batch picked up again, a job that was already
 * running); a plain start needs no toast where the screen moves on by itself (the import wizard's next step). */
export const prepareIsNews = (s: PrepareStart): boolean => s.key === 'blocks.batch_resumed' || s.key === 'blocks.already_running'
