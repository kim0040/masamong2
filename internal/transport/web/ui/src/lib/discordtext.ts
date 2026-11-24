// Discord's own markup as it appears in stored messages and in answers: __underline__, **bold**, *italic*,
// ~~strike~~, `code`, ||spoiler||, > quotes, and the tokens <@id>, <#id>, <@&id>, <:name:id>. Shown raw they
// read like internal codes, so the console draws them. Pure, so it is tested without a browser; nothing here
// builds HTML: the component turns each node into plain elements.

export type DNode =
  | { k: 'text'; v: string }
  | { k: 'bold' | 'italic' | 'underline' | 'strike' | 'spoiler' | 'quote'; c: DNode[] }
  | { k: 'code'; v: string; block?: boolean }
  | { k: 'user' | 'channel' | 'role'; id: string }
  | { k: 'emoji'; name: string }

const token = /<(@!?|@&|#)(\d{5,25})>|<a?:([A-Za-z0-9_]{2,32}):\d{5,25}>/y

/** A closing marker that is not escaped and has some text before it. */
function closeAt(s: string, from: number, marker: string): number {
  for (let i = from; i < s.length; i++) {
    if (s[i] === '\\') {
      i++
      continue
    }
    if (s.startsWith(marker, i) && i > from) return i
  }
  return -1
}

function inline(s: string): DNode[] {
  const out: DNode[] = []
  let buf = ''
  const flush = () => {
    if (buf) out.push({ k: 'text', v: buf })
    buf = ''
  }
  for (let i = 0; i < s.length; ) {
    const ch = s[i]
    if (ch === '\\' && i + 1 < s.length && /[*_~`|\\<>]/.test(s[i + 1])) {
      buf += s[i + 1]
      i += 2
      continue
    }
    if (ch === '`') {
      const run = /^`+/.exec(s.slice(i))![0]
      const end = s.indexOf(run, i + run.length)
      if (end > i + run.length) {
        flush()
        out.push({ k: 'code', v: s.slice(i + run.length, end).replace(/^ | $/g, '') })
        i = end + run.length
        continue
      }
    }
    if (ch === '<') {
      token.lastIndex = i
      const m = token.exec(s)
      if (m) {
        flush()
        if (m[3]) out.push({ k: 'emoji', name: m[3] })
        else out.push({ k: m[1] === '#' ? 'channel' : m[1] === '@&' ? 'role' : 'user', id: m[2] })
        i += m[0].length
        continue
      }
    }
    const marks: [string, DNode['k']][] = [['||', 'spoiler'], ['**', 'bold'], ['__', 'underline'], ['~~', 'strike']]
    let hit = false
    for (const [mk, kind] of marks) {
      if (!s.startsWith(mk, i)) continue
      const end = closeAt(s, i + mk.length, mk)
      if (end < 0) continue
      flush()
      out.push({ k: kind as 'bold', c: inline(s.slice(i + mk.length, end)) })
      i = end + mk.length
      hit = true
      break
    }
    if (hit) continue
    if (ch === '*' || (ch === '_' && (i === 0 || /[\s([{>]/.test(s[i - 1])))) {
      const end = closeAt(s, i + 1, ch)
      // a single marker must close on the same line and not sit next to a space ("2 * 3 * 4" is not italic)
      if (end > 0 && !/\n/.test(s.slice(i, end)) && !/\s/.test(s[i + 1]) && !/\s/.test(s[end - 1]) && (ch === '*' || end + 1 >= s.length || /[\s.,!?:;)\]}]/.test(s[end + 1]))) {
        flush()
        out.push({ k: 'italic', c: inline(s.slice(i + 1, end)) })
        i = end + 1
        continue
      }
    }
    buf += ch
    i++
  }
  flush()
  return out
}

/** The nodes of one message or answer. */
export function parseDiscord(text: string): DNode[] {
  const src = (text ?? '').replace(/\r\n/g, '\n')
  const out: DNode[] = []
  // fenced code first: ```lang\n…```
  const fence = /```(?:[A-Za-z0-9_+-]*\n)?([\s\S]*?)```/g
  let last = 0
  const lines = (chunk: string) => {
    // block quotes: consecutive lines that start with "> "
    const rows = chunk.split('\n')
    let i = 0
    let pending: string[] = []
    const flushText = () => {
      if (pending.length) out.push(...inline(pending.join('\n')))
      pending = []
    }
    while (i < rows.length) {
      if (/^> /.test(rows[i])) {
        flushText()
        const q: string[] = []
        while (i < rows.length && /^> /.test(rows[i])) q.push(rows[i++].replace(/^> /, ''))
        out.push({ k: 'quote', c: inline(q.join('\n')) })
        if (i < rows.length) pending.push('')
        continue
      }
      pending.push(rows[i++])
    }
    flushText()
  }
  for (const m of src.matchAll(fence)) {
    lines(src.slice(last, m.index))
    out.push({ k: 'code', v: m[1].replace(/\n$/, ''), block: true })
    last = m.index! + m[0].length
  }
  lines(src.slice(last))
  return out
}

/** Whether the text has any markup worth drawing (plain text can be shown as it is). */
export const hasMarkup = (text: string): boolean => /[*_~`|<>\\]/.test(text ?? '')

/** What the generated tag line starts with: a Discord room label, or a room key. */
const ROOM_LABEL = /^(discord guild=|discord DM channel=|discord:|kakao:)/

/** A generated excerpt starts with a tag line "[room · date · speakers]"; the page already shows those three,
 * so the line is taken off the text. Only a line that looks generated goes (a room label, or the " · " between
 * its parts): a bracketed first line of a message ("[TODO]") is text and stays, and `raw` (the excerpt is a stored
 * message, not a generated one) keeps every line. */
export function splitExcerpt(excerpt: string | undefined, opts: { raw?: boolean } = {}): { head: { room: string; date: string; speakers: string } | null; body: string } {
  const s = excerpt ?? ''
  const m = /^\[([^\]\n]*)\]\n/.exec(s)
  if (!m || opts.raw || !(ROOM_LABEL.test(m[1]) || m[1].includes(' · '))) return { head: null, body: s }
  const [room = '', date = '', ...rest] = m[1].split(' · ')
  return { head: { room, date, speakers: rest.join(' · ') }, body: s.slice(m[0].length) }
}
