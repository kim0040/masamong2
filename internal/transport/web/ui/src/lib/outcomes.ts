// One color per reply outcome, shared by every chart of turns: a new outcome must not borrow
// "answered"'s orange and read as replies.
const colors: Record<string, string> = {
  answered: 'var(--chart-1)', silent: 'var(--chart-2)', limited: 'var(--chart-6)', budget: 'var(--chart-4)', paused: 'var(--chart-3)',
  disabled: 'var(--text-subtle)', error: 'var(--chart-5)', not_addressed: 'var(--border-strong)',
  reacted: 'color-mix(in srgb, var(--chart-1) 45%, var(--surface))', canceled: 'color-mix(in srgb, var(--text-subtle) 60%, var(--surface))',
  no_vision: 'color-mix(in srgb, var(--chart-4) 55%, var(--surface))',
}
const spare = ['var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)', 'var(--chart-6)']

export function outcomeColor(outcome: string, i = 0): string {
  return colors[outcome] ?? spare[i % spare.length]
}

// The status tone of an outcome: decisions to stay out (not said to the bot, an emoji only, silence,
// a stop) are neutral; only limits and failures warn.
export function outcomeTone(outcome: string): 'ok' | 'warn' | 'danger' | 'neutral' {
  if (outcome === 'answered') return 'ok'
  if (outcome === 'error') return 'danger'
  if (outcome === 'silent' || outcome === 'not_addressed' || outcome === 'reacted' || outcome === 'canceled') return 'neutral'
  return 'warn'
}
