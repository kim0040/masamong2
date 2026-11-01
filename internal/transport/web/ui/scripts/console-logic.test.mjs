// Logic of the console that needs no browser: the helpers behind the audit fixes. Each module is
// transpiled with its imports replaced by small stand-ins, as the other tests here do.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

function load(file, replacements) {
  let source = readFileSync(new URL(`../src/lib/${file}`, import.meta.url), 'utf8')
  for (const [from, to] of replacements) {
    assert.ok(source.includes(from), `${file} no longer contains: ${from}`)
    source = source.replace(from, to)
  }
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
  return import(`data:text/javascript,${encodeURIComponent(js)}#${Math.random()}`)
}

const strings = {
  'platform.tray': 'tray icon',
  'platform.tray.windows': 'notification area icon',
  'platform.tray.darwin': 'menu bar icon',
  'memstate.empty_in_allowed_scope': 'no memories in scope',
}
globalThis.__i18n = {
  has: (k) => k in strings,
  t: (k) => strings[k] ?? k,
  i18n: { lang: 'ko' },
  localeName: (tag) => ({ ko: '한국어', en: 'English', ja: '日本語' })[tag] ?? tag,
}
globalThis.__session = { value: { platform: 'windows' } }

test('persona choices are grouped by language with the room language first', async () => {
  const { personaOptionsFor } = await load('persona.ts', [["import { t, has, localeName } from './i18n.svelte'", 'const { t, has, localeName } = globalThis.__i18n']])
  const list = [
    { id: 'friendly_en', label: 'Friendly English', locale: 'en' },
    { id: 'friendly_ko', label: '친근한 반말', locale: 'ko' },
    { id: 'hidden_ko', label: 'x', locale: 'ko', hidden: true },
    { id: 'mine', label: '내 말투', locale: 'ko' },
  ]
  const options = personaOptionsFor(list, 'ko')
  assert.deepEqual(options.map((o) => [o.value, o.group]), [['friendly_ko', '한국어'], ['mine', '한국어'], ['friendly_en', 'English']])
  assert.ok(options.every((o) => !o.label.includes('·')), 'the group names the language, the label does not repeat it')
  assert.ok(personaOptionsFor(list, 'ko', 'hidden_ko').some((o) => o.value === 'hidden_ko'), 'the current choice stays even when hidden')
})

test('the default persona puts each voice\'s v2 first and a hidden voice takes its v2 with it', async () => {
  const { defaultPersonaId } = await load('persona.ts', [["import { t, has, localeName } from './i18n.svelte'", 'const { t, has, localeName } = globalThis.__i18n']])
  const ids = ['friendly_ko', 'friendly_ko_v2', 'warm_ko', 'warm_ko_v2', 'friendly_en', 'friendly_en_v2', 'friendly_zh_hans', 'friendly_zh_hans_v2', 'friendly_es']
  const list = (hidden = []) => ids.map((id) => ({ id, builtin: true, hidden: hidden.includes(id) }))
  assert.equal(defaultPersonaId(list(), 'ko', false), 'friendly_ko_v2')
  assert.equal(defaultPersonaId(list(), 'ko', true), 'warm_ko_v2')
  assert.equal(defaultPersonaId(list(['warm_ko_v2']), 'ko', true), 'warm_ko', 'hiding the v2 falls to the v1')
  assert.equal(defaultPersonaId(list(['warm_ko']), 'ko', true), 'friendly_ko_v2', 'hiding the voice skips its v2 too')
  assert.equal(defaultPersonaId(list(['friendly_ko', 'warm_ko']), 'ko', true), 'friendly_en_v2', 'then English')
  assert.equal(defaultPersonaId(list(), 'zh-Hans', false), 'friendly_zh_hans_v2', 'a tag with a dash maps to underscores')
  assert.equal(defaultPersonaId(list(), 'es', false), 'friendly_es', 'a language with no v2 uses its v1')
  assert.equal(defaultPersonaId([], 'ko', false), 'friendly_ko', 'no persona list falls back to friendly_ko like the server')
  assert.equal(defaultPersonaId(list(ids), 'ko', true), 'warm_ko_v2', 'every candidate hidden: the first built-in candidate, as the server does')
  assert.equal(defaultPersonaId([{ id: 'friendly_ko_v2', builtin: false }, { id: 'friendly_en', builtin: true }], 'ko', false), 'friendly_en', 'a custom persona with a built-in name is not one')
})

test('a persona ID comes from the name; names without Latin letters get a code, not persona and persona_2', async () => {
  const { personaId } = await load('persona.ts', [["import { t, has, localeName } from './i18n.svelte'", 'const { t, has, localeName } = globalThis.__i18n']])
  assert.equal(personaId('Friendly Bot', new Set()), 'friendly_bot')
  assert.equal(personaId('Friendly Bot', new Set(['friendly_bot'])), 'friendly_bot_2')
  const a = personaId('명랑한 친구', new Set())
  const b = personaId('차분한 선생님', new Set())
  assert.match(a, /^persona_[a-z0-9]{1,6}$/)
  assert.notEqual(a, b, 'two Korean names do not collide')
  assert.equal(personaId('명랑한 친구', new Set()), a, 'the same name gives the same ID')
  assert.equal(personaId('', new Set()), 'persona')
  assert.match(personaId('a', new Set()), /^persona_a$/)
})

test('an empty search names its cause: a room without an attached block, never a raw code', async () => {
  const { emptyScope, stateLabel } = await load('blocks.ts', [
    ["import { t, has } from './i18n.svelte'", 'const { t, has } = globalThis.__i18n'],
    ["import { num, duration } from './format'", 'const num = String, duration = String'],
    ["import { i18n } from './i18n.svelte'", 'const { i18n } = globalThis.__i18n'],
  ])
  const attached = [{ source_room: 'a', channels: [{ guild_id: 'g', channel_id: 'c' }] }, { source_room: 'b', channels: [] }]
  assert.equal(emptyScope(attached, { guild_id: 'g', channel_id: 'c' }), 'no_match')
  assert.equal(emptyScope(attached, { guild_id: 'g', channel_id: 'other' }), 'no_block')
  assert.equal(emptyScope([], { guild_id: 'g', channel_id: 'c' }), 'no_block')
  assert.equal(emptyScope(attached, null), 'no_match', 'explicit rooms: not a question of the room')
  assert.equal(stateLabel('empty_in_allowed_scope'), 'no memories in scope')
  assert.equal(stateLabel('some_new_state'), 'some new state', 'an unknown code is spaced out, not shown raw')
})

test('platform wording picks the system the service runs on', async () => {
  const { tp, platform, examplePath } = await load('platform.ts', [
    ["import { session } from './api.svelte'", 'const session = globalThis.__session'],
    ["import { has, t } from './i18n.svelte'", 'const { has, t } = globalThis.__i18n'],
  ])
  const word = () => tp('platform.tray')
  globalThis.__session.value = { platform: 'windows' }
  assert.equal(platform(), 'windows')
  assert.equal(word(), 'notification area icon')
  assert.equal(examplePath('chat.csv'), 'C:\\Users\\me\\chat.csv')
  globalThis.__session.value = { platform: 'darwin' }
  assert.equal(word(), 'menu bar icon')
  assert.equal(examplePath('chat.csv'), '/Users/me/chat.csv')
  globalThis.__session.value = { platform: 'linux' }
  assert.equal(word(), 'tray icon')
  globalThis.__session.value = null
  assert.equal(platform(), 'linux', 'unknown means the plain wording')
  assert.equal(examplePath('chat.csv'), '/home/me/chat.csv')
})

test('a lazy route failure reaches the retry state; a failed prefetch is ignored', async () => {
  const { loadRoute, prefetchRoute } = await load('route-load.ts', [])
  let failed = false
  let component = null
  let attempts = 0
  const chunk = async () => {
    attempts++
    if (attempts === 1) throw new Error('chunk unavailable')
    return { default: 'RoutePage' }
  }
  const retry = () => {
    failed = false
    return loadRoute(chunk, (loaded) => { component = loaded }, () => { failed = true })
  }
  await retry()
  assert.equal(failed, true, 'failed lazy imports enter the load-error state')
  assert.equal(component, null)
  await retry()
  assert.equal(failed, false, 'retry clears the load-error state')
  assert.equal(component, 'RoutePage')

  await assert.doesNotReject(prefetchRoute(async () => { throw new Error('speculative chunk unavailable') }, () => {
    assert.fail('failed prefetch must not cache a route')
  }))
})

test('protected page resources wait for both sign-in and completed setup', async () => {
  globalThis.$state = (value) => value
  globalThis.document = { documentElement: { setAttribute() {} } }
  globalThis.sessionStorage = { getItem: () => null }
  const api = await load('api.svelte.ts', [])
  assert.equal(api.canLoadProtectedActions(null), false)
  assert.equal(api.canLoadProtectedActions({ authenticated: false, ready: true }), false)
  assert.equal(api.canLoadProtectedActions({ authenticated: true, ready: false }), false)
  assert.equal(api.canLoadProtectedActions({ authenticated: true, ready: true }), true)
  // practice mode runs on its own instance: it does not wait for the real instance's setup, but still needs sign-in
  api.practice.on = true
  assert.equal(api.canLoadProtectedActions({ authenticated: true, ready: false }), true)
  assert.equal(api.canLoadProtectedActions({ authenticated: false, ready: false }), false)
  assert.equal(api.canLoadProtectedActions(null), false)
  api.practice.on = false

  let calls = 0
  globalThis.__firstLoad = {
    session: api.session,
    canLoadProtectedActions: api.canLoadProtectedActions,
    call: async () => { calls++; return { ok: true, output: {} } },
  }
  globalThis.$effect = (fn) => { fn() }
  const resourceModule = await load('resource.svelte.ts', [
    ["import { call, canLoadProtectedActions, session, type ActionError } from './api.svelte'", 'const { call, canLoadProtectedActions, session } = globalThis.__firstLoad'],
    ["import { onLive } from './live.svelte'", 'const onLive = () => () => {}'],
  ])
  const res = resourceModule.resource('stats.series', () => ({ days: 14 }))
  await Promise.resolve()
  assert.equal(calls, 0, 'an unauthenticated setup page must not request dashboard-only data')
  api.session.value = { authenticated: true, ready: true, lang: 'en', locales: [], version: 'test' }
  await res.refresh()
  assert.equal(calls, 1, 'the same resource loads after setup and sign-in')

  const connections = readFileSync(new URL('../src/pages/Connections.svelte', import.meta.url), 'utf8')
  assert.doesNotMatch(connections, /onMount\s*\([^)]*checkBot|\$effect(?:\.root)?\s*\([^)]*checkBot/, 'a saved token is not checked automatically on page load')
  assert.match(connections, /onclick=\{checkBot\}/, 'operators can ask for a token check from the page')
})

test('a busy button and a tall dialog keep the keyboard flow: focus stays on the button, the footer stays on screen', () => {
  const button = readFileSync(new URL('../src/components/Button.svelte', import.meta.url), 'utf8')
  // a disabled button drops the focus, so a dialog it opened could not hand the focus back to it when closed again
  assert.doesNotMatch(button, /disabled=\{disabled \|\| loading\}/, 'loading must not disable the button')
  assert.match(button, /aria-disabled=\{loading \|\| undefined\}/, 'loading is announced as busy and disabled')
  assert.match(button, /loading \? e\.preventDefault\(\) : onclick\?\.\(e\)/, 'a click while loading does nothing')
  // onclick is taken out of rest for the button branch, so the link branch must get it explicitly (Home's attention
  // actions and the import wizard's last step navigate and run their callback)
  assert.match(button, /<a [^>]*\{href\}[^>]*\{onclick\}>/, 'a link button still runs the caller onclick')
  const dialog = readFileSync(new URL('../src/components/Dialog.svelte', import.meta.url), 'utf8')
  // the dialog's own max-height is a percentage of an auto height: a child cannot inherit it, and the footer then
  // sits below a page-tall dialog instead of staying on screen
  const inner = dialog.match(/\.inner \{[^}]*\}/)?.[0] ?? ''
  assert.match(inner, /max-height: min\(100dvh - 48px, 860px\)/)
  assert.doesNotMatch(inner, /max-height: inherit/)
})

test('navigation can wait for an answer: a refused leave stays, an allowed one goes', async () => {
  const calls = []
  globalThis.location = { origin: 'http://x', pathname: '/', search: '' }
  globalThis.history = { pushState: (_s, _t, u) => calls.push(`push ${u.pathname}`), replaceState: (_s, _t, u) => calls.push(`replace ${u.pathname}`) }
  globalThis.window = { scrollTo() {}, addEventListener() {}, removeEventListener() {} }
  globalThis.document = { getElementById: () => ({ focus() {} }), addEventListener() {}, startViewTransition: undefined }
  globalThis.$state = (v) => v
  const router = await load('router.svelte.ts', [["import { motionAllowed } from './prefs.svelte'", 'const motionAllowed = () => false']])
  router.setNavigationGuard(() => Promise.resolve(false))
  router.navigate('/tools')
  await new Promise((r) => setTimeout(r, 5))
  assert.deepEqual(calls, [], 'a refused leave does not navigate')
  router.setNavigationGuard(() => Promise.resolve(true))
  router.navigate('/rooms')
  await new Promise((r) => setTimeout(r, 5))
  assert.deepEqual(calls, ['push /rooms'])
  router.setNavigationGuard(() => false)
  router.navigate('/people')
  assert.deepEqual(calls, ['push /rooms'], 'a synchronous refusal still works')
  router.setNavigationGuard(null)
  router.navigate('/jobs')
  assert.deepEqual(calls, ['push /rooms', 'push /jobs'])
  router.setNavigationGuard(() => Promise.reject(new Error('guard failed')))
  router.navigate('/failed')
  await new Promise((r) => setTimeout(r, 5))
  assert.deepEqual(calls, ['push /rooms', 'push /jobs'], 'a rejected guard keeps the current page')
  assert.equal(router.route.path, '/jobs')
})

test('Back and Forward use the unsaved guard and restore the URL when leaving is refused', async () => {
  const listeners = new Map()
  const entries = [
    { href: 'http://x/rooms', state: { __masamongHistoryIndex: 0 } },
    { href: 'http://x/settings', state: { __masamongHistoryIndex: 1 } },
  ]
  let index = 1
  const setLocation = (href) => {
    const url = new URL(href)
    Object.assign(globalThis.location, { href: url.href, origin: url.origin, pathname: url.pathname, search: url.search })
  }
  globalThis.location = { origin: 'http://x', href: entries[index].href, pathname: '/settings', search: '' }
  globalThis.history = {
    get state() { return entries[index].state },
    pushState(state, _title, href) {
      entries.splice(index + 1)
      entries.push({ href: new URL(href, location.href).href, state })
      index = entries.length - 1
      setLocation(entries[index].href)
    },
    replaceState(state, _title, href) {
      entries[index] = { href: new URL(href, location.href).href, state }
      setLocation(entries[index].href)
    },
    go(delta) {
      setTimeout(() => {
        const next = index + delta
        if (next < 0 || next >= entries.length) return
        index = next
        setLocation(entries[index].href)
        listeners.get('popstate')?.({ state: entries[index].state })
      }, 0)
    },
  }
  globalThis.window = {
    scrollTo() {},
    addEventListener: (name, fn) => listeners.set(name, fn),
    removeEventListener: (name) => listeners.delete(name),
  }
  globalThis.document = {
    getElementById: () => ({ focus() {} }),
    addEventListener() {},
    startViewTransition: undefined,
  }
  globalThis.$state = (v) => v
  const router = await load('router.svelte.ts', [["import { motionAllowed } from './prefs.svelte'", 'const motionAllowed = () => false']])
  router.startRouter()

  const waitFor = async (predicate) => {
    const deadline = Date.now() + 1000
    while (Date.now() < deadline) {
      if (predicate()) return
      await new Promise((resolve) => setTimeout(resolve, 5))
    }
    assert.fail('history traversal did not settle')
  }
  let guardCalls = 0
  router.setNavigationGuard(() => { guardCalls++; return Promise.resolve(false) })
  const before = { returnValue: 'initial', preventDefault() { this.prevented = true } }
  listeners.get('beforeunload')(before)
  assert.equal(before.prevented, true, 'a dirty draft requests the browser unload prompt')
  assert.equal(before.returnValue, '', 'beforeunload sets the browser-required return value')

  history.go(-1)
  await waitFor(() => guardCalls === 1 && location.pathname === '/settings' && router.route.path === '/settings')
  assert.equal(guardCalls, 1, 'Back calls the same unsaved-change guard')
  assert.equal(location.pathname, '/settings', 'canceling Back restores the address bar')
  assert.equal(router.route.path, '/settings', 'canceling Back keeps the current page')

  router.setNavigationGuard(() => { guardCalls++; return Promise.resolve(true) })
  history.go(-1)
  await waitFor(() => location.pathname === '/rooms' && router.route.path === '/rooms')
  assert.equal(location.pathname, '/rooms', 'approving Back applies the destination')
  assert.equal(router.route.path, '/rooms')
  history.go(1)
  await waitFor(() => location.pathname === '/settings' && router.route.path === '/settings')
  assert.equal(location.pathname, '/settings', 'Forward also applies through the guard')
  assert.equal(router.route.path, '/settings')
  assert.equal(guardCalls, 3, 'both accepted history traversals call the guard')

  router.setNavigationGuard(null)
  assert.equal(listeners.has('beforeunload'), false, 'the browser prompt is removed after edits are saved')
})

test('room memory budget: off, ok, state paused at 80%, stopped at 100%', async () => {
  const { budgetView } = await load('roommemory.ts', [])
  assert.equal(budgetView(undefined).state, 'unknown')
  assert.equal(budgetView({ limit_usd: 0, spent_usd: 1 }).state, 'off')
  assert.equal(budgetView({ limit_usd: 3, spent_usd: 1 }).state, 'ok')
  const paused = budgetView({ limit_usd: 3, spent_usd: 2.4 })
  assert.equal(paused.state, 'state_paused')
  assert.equal(paused.tone, 'warn')
  assert.ok(Math.abs(paused.remaining - 0.6) < 1e-9)
  const stopped = budgetView({ limit_usd: 3, spent_usd: 3.2 })
  assert.deepEqual([stopped.state, stopped.remaining, stopped.pct], ['stopped', 0, 100])
})

test('room memory sources, room picker values and list input', async () => {
  const { parseSource, roomValue, parseRoomValue, listInput } = await load('roommemory.ts', [])
  assert.deepEqual(parseSource('discord'), { kind: 'discord', block: '' })
  assert.deepEqual(parseSource('kakao:blk-1'), { kind: 'kakao', block: 'blk-1' })
  assert.equal(parseSource('').kind, 'other')
  assert.deepEqual(parseRoomValue(roomValue('discord', 'g1', 'c1')), { kind: 'discord', guild_id: 'g1', channel_id: 'c1' })
  assert.deepEqual(parseRoomValue(roomValue('kakao', 'kakao:club')), { kind: 'kakao', room: 'kakao:club' })
  assert.equal(parseRoomValue(''), null)
  assert.deepEqual(listInput(parseRoomValue(roomValue('kakao', 'kakao:club'))), { room: 'kakao:club', limit: 200 })
  assert.deepEqual(listInput(parseRoomValue(roomValue('discord', 'g1', 'c1'))), { guild_id: 'g1', channel_id: 'c1', limit: 200 })
})

test('room memory episode filters and the forget preview', async () => {
  const { filterEpisodes, distinct, forgetPreview } = await load('roommemory.ts', [])
  const list = [{ kind: 'plan', status: 'open' }, { kind: 'plan', status: 'done' }, { kind: 'joke', status: 'open' }]
  assert.equal(filterEpisodes(list, { kind: '', status: '' }).length, 3)
  assert.equal(filterEpisodes(list, { kind: 'plan', status: '' }).length, 2)
  assert.equal(filterEpisodes(list, { kind: 'plan', status: 'done' }).length, 1)
  assert.deepEqual(distinct(list, 'kind', ['plan', 'decision', 'event', 'joke', 'fact']), ['plan', 'joke'])
  assert.deepEqual(distinct([{ kind: 'zzz' }, { kind: 'plan' }], 'kind', ['plan']), ['plan', 'zzz'])
  assert.deepEqual(forgetPreview({ items: 5, hidden: 2 }), { items: 5, hidden: 2, visible: 3 })
  assert.deepEqual(forgetPreview({ items: 1, hidden: 4 }), { items: 1, hidden: 1, visible: 0 })
  assert.deepEqual(forgetPreview({}), { items: 0, hidden: 0, visible: 0 })
})

test('room memory extraction estimate: budget blocks, over-budget warning, run cap', async () => {
  const { estimateView, validCap, jobOutcome } = await load('roommemory.ts', [])
  const ok = estimateView({ segments: 10, est_cost_usd: 0.4, price_known: true, derived_budget_usd: 3, derived_spent_usd: 1 })
  assert.deepEqual([ok.empty, ok.blocked, ok.over, ok.defaultCap], [false, false, false, '1.00'])
  assert.equal(estimateView({ segments: 10, est_cost_usd: 2.5, price_known: true, derived_budget_usd: 3, derived_spent_usd: 1 }).over, true)
  assert.equal(estimateView({ segments: 10, est_cost_usd: 2.5, price_known: false, derived_budget_usd: 3, derived_spent_usd: 1 }).over, false)
  assert.equal(estimateView({ segments: 10, derived_budget_usd: 0, derived_spent_usd: 0 }).blocked, true)
  assert.equal(estimateView({ segments: 10, derived_budget_usd: 3, derived_spent_usd: 3 }).blocked, true)
  assert.equal(estimateView({ segments: 0, derived_budget_usd: 3, derived_spent_usd: 0 }).empty, true)
  assert.equal(estimateView(null).empty, true)
  assert.equal(estimateView({ segments: 1, est_cost_usd: 5, price_known: true, derived_budget_usd: 9, derived_spent_usd: 0 }).defaultCap, '6.00')
  assert.deepEqual(['1', '0.25', '1000', '0', '1001', '-1', 'abc', '1.23456', ''].map(validCap), [true, true, true, false, false, false, false, false, false])
  assert.equal(jobOutcome({ done: true }), 'done')
  assert.equal(jobOutcome({ stopped: 'budget' }), 'stopped_budget')
  assert.equal(jobOutcome({ stopped: 'error' }), 'stopped_error')
  assert.equal(jobOutcome({ done: false }), 'queued')
  assert.equal(jobOutcome(null), 'queued')
})

test('room style: which version matters, counted stats, item groups, dry run, preview text', async () => {
  const { pickVersions, statsView, groupItems, dryRunView, previewMessage, PREVIEW_MAX, HISTORY_MAX } = await load('roomstyle.ts', [])
  const v = (version, state) => ({ version, state })
  // newest first, as the server sends them
  const mix = pickVersions([v(5, 'pending'), v(4, 'approved'), v(3, 'superseded'), v(2, 'rejected'), v(1, 'stale')])
  assert.equal(mix.pending.version, 5)
  assert.equal(mix.approved.version, 4)
  assert.deepEqual(mix.history.map((h) => h.version), [3, 1])
  assert.equal(mix.stale, false)
  const stale = pickVersions([v(2, 'stale'), v(1, 'superseded')])
  assert.deepEqual([stale.stale, stale.pending, stale.approved], [true, undefined, undefined])
  assert.equal(pickVersions([v(3, 'pending'), v(2, 'stale')]).stale, false)
  // a stale proposal next to the approved version in use is not 'analyze again'
  assert.equal(pickVersions([v(3, 'stale'), v(2, 'approved')]).stale, false)
  // the approved version the server appends after the newest five is still found
  assert.equal(pickVersions([v(9, 'pending'), v(8, 'rejected'), v(7, 'rejected'), v(6, 'superseded'), v(5, 'superseded'), v(2, 'approved')]).approved.version, 2)
  assert.deepEqual(pickVersions(null), { pending: undefined, approved: undefined, history: [], stale: false })
  const many = pickVersions(Array.from({ length: 9 }, (_, i) => v(9 - i, 'stale')))
  assert.equal(many.history.length, HISTORY_MAX)

  assert.equal(statsView(null), null)
  assert.equal(statsView({ messages: 0 }), null)
  const s = statsView({ messages: 120.4, people: 5, median_runes: 14, p90_runes: 62, laughs: ['ㅋㅋ', '', 'ㅎㅎ', 'www', 'lol', 'xd'], laugh_share: 0.35, emoji_share: 1.7, question_share: -1, languages: { en: 20, ko: 75, ja: 5, fr: 0 } })
  assert.deepEqual([s.messages, s.people, s.median, s.long], [120, 5, 14, 62])
  assert.deepEqual(s.laughs, ['ㅋㅋ', 'ㅎㅎ', 'www', 'lol'])
  assert.deepEqual([s.laughShare, s.emojiShare, s.questionShare], [0.35, 1, 0])
  assert.deepEqual(s.languages, [{ tag: 'ko', pct: 75 }, { tag: 'en', pct: 20 }, { tag: 'ja', pct: 5 }])
  assert.deepEqual(statsView({ messages: 3 }).languages, [])

  const g = groupItems([
    { kind: 'joke', text: 'bug again', evidence: 3 },
    { kind: 'slang', text: 'ngl', evidence: 4.2 },
    { kind: 'other', text: 'x', evidence: 1 },
    { kind: 'avoid', text: '', evidence: 2 },
    { kind: 'slang', text: 'fr', evidence: -1 },
  ])
  assert.deepEqual(g.map((x) => x.kind), ['slang', 'joke'])
  assert.deepEqual(g[0].items, [{ text: 'ngl', evidence: 4 }, { text: 'fr', evidence: 0 }])
  assert.deepEqual(groupItems(undefined), [])

  assert.equal(dryRunView(null), null)
  const few = dryRunView({ stats: { messages: 12 }, enough: false, min_messages: 50 })
  assert.deepEqual([few.enough, few.min, few.messages], [false, 50, 12])
  assert.equal(few.stats.messages, 12)
  assert.equal(dryRunView({ stats: {}, enough: true, min_messages: 50 }).stats, null)

  assert.equal(previewMessage('  hi there \n'), 'hi there')
  assert.equal(previewMessage('   '), '')
  assert.equal(previewMessage(undefined), '')
  assert.equal(previewMessage('a'.repeat(PREVIEW_MAX + 50)).length, PREVIEW_MAX)
})

test("a person's memory: told items apart from the rest, and what forgetting removes", async () => {
  const { splitMine, forgetTotal } = await load('personmemory.ts', [])
  const { told, other } = splitMine([{ id: 'a', explicit: true }, { id: 'b', explicit: false }, { id: 'c' }])
  assert.deepEqual([told.map((i) => i.id), other.map((i) => i.id)], [['a'], ['b', 'c']])
  assert.deepEqual(splitMine(null), { told: [], other: [] })
  assert.equal(forgetTotal({ items: 2, episodes: 1, cards: 3 }), 6)
  assert.equal(forgetTotal({ items: 0, episodes: 0, cards: 0 }), 0)
  assert.equal(forgetTotal(null), 0)
})

test('the embedding switch refusal is recognised by its message key, not its English text', async () => {
  const { embedReindexAsk, EMBED_REINDEX_KEY } = await load('embedswitch.ts', [])
  assert.deepEqual(embedReindexAsk({ message_key: EMBED_REINDEX_KEY, args: { reindex: { rooms: 3, leaves: 120, tokens: 9000 } } }), { known: true, rooms: 3, leaves: 120, tokens: 9000 })
  // no estimate in the refusal: still a confirmation, but without numbers (the console shows the generic note)
  assert.deepEqual(embedReindexAsk({ message_key: EMBED_REINDEX_KEY }), { known: false, rooms: 0, leaves: 0, tokens: 0 })
  assert.deepEqual(embedReindexAsk({ message_key: EMBED_REINDEX_KEY, args: {} }), { known: false, rooms: 0, leaves: 0, tokens: 0 })
  assert.deepEqual(embedReindexAsk({ message_key: EMBED_REINDEX_KEY, args: { reindex: null } }), { known: false, rooms: 0, leaves: 0, tokens: 0 })
  // an estimate without a token count keeps the rest
  assert.deepEqual(embedReindexAsk({ message_key: EMBED_REINDEX_KEY, args: { reindex: { rooms: 2, leaves: 5 } } }), { known: true, rooms: 2, leaves: 5, tokens: 0 })
  assert.equal(embedReindexAsk({ message_key: 'generic.error', args: { reindex: { rooms: 1, leaves: 1 } } }), null)
  assert.equal(embedReindexAsk(undefined), null)
})

test('the embedding width choices follow what the server says the model can be cut to', async () => {
  const { widthChoices, widthDirection, widthState, narrowWithoutRerank } = await load('embedwidth.ts', [])
  const info = (o) => ({ dimensions: 1024, effective: 1024, full_width: 4096, truncatable: true, recommended: 1024, ...o })
  // a cuttable model offers its full width and the narrower widths the server allows, widest first
  assert.deepEqual(widthChoices(info()).map((c) => [c.value, c.width, c.current, c.recommended]), [[0, 4096, false, false], [2048, 2048, false, false], [1024, 1024, true, true]])
  assert.equal(widthChoices(info({ dimensions: 0, effective: 4096 })).find((c) => c.current).full, true)
  // a width at or above the full width is not offered (the server would refuse it)
  assert.deepEqual(widthChoices(info({ full_width: 2048, effective: 2048, dimensions: 0 })).map((c) => c.value), [0, 1024])
  // a model whose full width is already 1024 has no narrower choice, so nothing is offered
  assert.deepEqual(widthChoices(info({ full_width: 1024, effective: 1024, dimensions: 0 })), [])
  // when the server lists the widths it accepts, only those are offered (0 = the full width), widest first
  assert.deepEqual(widthChoices(info({ widths: [0, 1024] })).map((c) => [c.value, c.width]), [[0, 4096], [1024, 1024]])
  assert.deepEqual(widthChoices(info({ widths: [1024, 0, 2048, 1024] })).map((c) => c.value), [0, 2048, 1024])
  assert.deepEqual(widthChoices(info({ widths: [0] })), [])
  assert.deepEqual(widthChoices(info({ widths: [] })), [])
  // a listed width the model cannot have (not below the full width) is dropped, never offered
  assert.deepEqual(widthChoices(info({ full_width: 2048, widths: [0, 4096, 1024] })).map((c) => c.value), [0, 1024])
  // an unknown model, or no data at all, offers nothing
  assert.deepEqual(widthChoices(info({ truncatable: false, full_width: 0, effective: 0, dimensions: 0 })), [])
  assert.deepEqual(widthChoices(undefined), [])
  const [full, mid, narrow] = widthChoices(info())
  assert.equal(widthDirection(info(), full), 'raise')
  assert.equal(widthDirection(info(), mid), 'raise')
  assert.equal(widthDirection(info(), narrow), 'same')
  assert.equal(widthDirection(info({ dimensions: 0, effective: 4096 }), narrow), 'lower')
  assert.equal(widthState(info()), 'narrow')
  assert.equal(widthState(info({ effective: 4096 })), 'full')
  assert.equal(widthState(info({ full_width: 0, effective: 1536, truncatable: false })), 'custom')
  assert.equal(widthState(info({ full_width: 0, effective: 0, truncatable: false })), 'unknown')
  assert.equal(widthState(undefined), 'unknown')
  // the rerank note is for a narrow width the server says has no usable rerank model; it is the server's judgment,
  // and a server that does not say counts as usable (no claim it was never told)
  assert.equal(narrowWithoutRerank(info({ rerank_usable: false })), true)
  assert.equal(narrowWithoutRerank(info({ rerank_usable: true })), false)
  assert.equal(narrowWithoutRerank(info()), false)
  assert.equal(narrowWithoutRerank(info({ rerank_usable: false, effective: 4096 })), false)
  assert.equal(narrowWithoutRerank(info({ rerank_usable: false, truncatable: false, full_width: 0, effective: 0 })), false)
})

test('a hand-set width above the full width is told apart, and going back to the full width from it embeds again', async () => {
  const { widthChoices, widthDirection, widthState } = await load('embedwidth.ts', [])
  // qwen3-embedding-4b written as 3000 in the settings file: effective 3000, full 2560
  const over = { dimensions: 3000, effective: 3000, full_width: 2560, truncatable: true, recommended: 1024, widths: [0, 1024, 2048] }
  assert.equal(widthState(over), 'over')
  assert.equal(widthState({ ...over, dimensions: 0, effective: 2560 }), 'full')
  assert.equal(widthState({ ...over, dimensions: 2560, effective: 2560 }), 'full')
  const choices = widthChoices(over)
  assert.deepEqual(choices.map((c) => [c.value, c.width, c.current]), [[0, 2560, false], [2048, 2048, false], [1024, 1024, false]], 'none of them is the width in use')
  const [full, mid] = choices
  // 0 is the full width, a smaller number than 3000, yet the server embeds again (the space cannot be cut to its native one)
  assert.equal(widthDirection(over, full), 'raise')
  assert.equal(widthDirection(over, mid), 'lower', 'a narrower width still only cuts')
  // a width equal to or below the full width keeps the numeric comparison
  assert.equal(widthDirection({ ...over, dimensions: 0, effective: 2560 }, full), 'same')
  assert.equal(widthDirection({ ...over, dimensions: 2048, effective: 2048 }, full), 'raise')
  // the legacy 4096 is the native width of a 4096 model, not an oversize one
  assert.equal(widthState({ dimensions: 4096, effective: 4096, full_width: 4096, truncatable: true, recommended: 1024 }), 'full')
  assert.equal(widthState({ dimensions: 4096, effective: 3072, full_width: 3072, truncatable: true, recommended: 1024 }), 'full')
})

test('a width answer is void once the model, the stored width or the revision it was asked on changes', async () => {
  const { widthBasis, sameWidthBasis, widthApplied } = await load('embedwidth.ts', [])
  // the basis is made from one embed_width alone: revision, provider and model are its own fields
  const info = { revision: 7, provider: 'openrouter', model: 'qwen/qwen3-embedding-8b', dimensions: 1024, effective: 1024, full_width: 4096, truncatable: true, recommended: 1024 }
  const base = widthBasis(info)
  assert.deepEqual(base, { revision: 7, provider: 'openrouter', model: 'qwen/qwen3-embedding-8b', dimensions: 1024, effective: 1024, full_width: 4096 })
  assert.equal(widthBasis.length, 1, 'no second source (config.document, a revision argument) can be mixed in')
  assert.equal(sameWidthBasis(base, widthBasis({ ...info })), true)
  assert.equal(sameWidthBasis(base, widthBasis({ ...info, revision: 8 })), false, 'another tab saved something')
  assert.equal(sameWidthBasis(base, widthBasis({ ...info, dimensions: 0, effective: 4096 })), false, 'the width changed')
  assert.equal(sameWidthBasis(base, widthBasis({ ...info, model: 'openai/text-embedding-3-large', full_width: 3072, effective: 3072 })), false, 'a different model')
  assert.equal(sameWidthBasis(base, widthBasis({ ...info, provider: 'cometapi' })), false, 'another provider')
  assert.equal(sameWidthBasis(null, base), false)
  assert.equal(sameWidthBasis(base, undefined), false)
  // nothing read, or an answer that does not name its revision, gives no basis: the page cannot apply on it
  assert.equal(widthBasis(undefined), null)
  assert.equal(widthBasis(null), null)
  const { revision, ...noRevision } = info
  assert.equal(widthBasis(noRevision), null)
  assert.equal(widthBasis({ ...info, revision: 0 }), null)
  assert.equal(sameWidthBasis(base, widthBasis(noRevision)), false)
  // the server's changed:false is "no change", not a cut or a re-embedding
  assert.equal(widthApplied({ changed: false, reembed: false }), 'unchanged')
  assert.equal(widthApplied({ dimensions: 1024, previous: 0, adapted: true, reembed: false }), 'cut')
  assert.equal(widthApplied({ dimensions: 2048, adapted: false, reembed: true }), 'reembed')
  assert.equal(widthApplied(undefined), 'cut')
})

test('the width picker reads providers.list once and never falls back to an older or other answer', async () => {
  const { widthRead, widthBasis, sameWidthBasis, widthChoices, widthDirection } = await load('embedwidth.ts', [])
  const ew = (o) => ({ revision: 7, provider: 'openrouter', model: 'qwen/qwen3-embedding-8b', dimensions: 1024, effective: 1024, full_width: 4096, truncatable: true, recommended: 1024, widths: [0, 1024, 2048], ...o })
  // a failed read, no embed_width, or one without a revision: nothing to apply on (the page asks to read again)
  assert.equal(widthRead(undefined), null)
  assert.equal(widthRead({ ok: false, error: { code: 'INTERNAL' } }), null)
  assert.equal(widthRead({ ok: true, output: {} }), null)
  assert.equal(widthRead({ ok: true, output: { embed_width: ew({ revision: undefined }) } }), null)
  const read = widthRead({ ok: true, output: { embed_width: ew() } })
  assert.equal(read.revision, 7)
  // The review's scenario: the old read said width 1024 at revision 7; width 0 (4096) was saved as revision 8.
  // A second read of providers.list at revision 8 is a different basis, so the answer asked at revision 7 is void,
  // and its choices and direction come from the new read: going to 2048 is a cut (lower), asked as such.
  const old = widthBasis(read)
  const now = widthRead({ ok: true, output: { embed_width: ew({ revision: 8, dimensions: 0, effective: 4096 }) } })
  assert.equal(sameWidthBasis(old, widthBasis(now)), false)
  const mid = widthChoices(now).find((c) => c.value === 2048)
  assert.equal(widthDirection(now, mid), 'lower', 'the new read says 4096, so 2048 is the narrowing that needs the cut confirmation')
  assert.equal(widthDirection(read, widthChoices(read).find((c) => c.value === 2048)), 'raise', 'the old read said 1024, where 2048 would have looked like a raise')
  // the same content at the same revision is the same basis (a live event that changed nothing here keeps answers)
  assert.equal(sameWidthBasis(old, widthBasis(widthRead({ ok: true, output: { embed_width: ew({ rerank_usable: false }) } }))), true)
})

test('the width picker builds its basis from providers.list alone and sends that revision', () => {
  const src = readFileSync(new URL('../src/pages/Connections.svelte', import.meta.url), 'utf8')
  const calls = src.match(/widthBasis\([^)]*\)/g) ?? []
  assert.deepEqual([...new Set(calls)], ['widthBasis(wi)'], "one source for the basis: the picker's own providers.list read")
  const widthPart = src.slice(src.indexOf('async function loadWidth'), src.indexOf('async function confirmWidthLower'))
  assert.ok(!/cfg\.revision|cfg\.doc|loadConfig/.test(widthPart), 'config.document is never mixed into the width basis')
  assert.ok(widthPart.includes("call('providers.list'"), 'the picker reads providers.list itself')
  assert.equal((widthPart.match(/base_revision: req\.baseRevision/g) ?? []).length, 1, 'both the first request and the include_embed repeat go through one send')
  assert.ok(!/base_revision: widthBase|widthBase\.revision/.test(widthPart), 'a request never reads the current revision again; it uses the approval it was asked on')
  assert.ok(/disabled=\{widthDir === 'same' \|\| !widthBase \|\| widthLoading\}/.test(src), 'no basis, or a read in flight: no apply')
})

test('the recommendation dialog keeps its ticks across a newer preview and drops what it no longer proposes', async () => {
  const { recTicks, recAfterRefresh } = await load('embedwidth.ts', [])
  const pick = (role, o) => ({ role, model: `${role}-pick`, current_provider: 'p1', current_model: `${role}-now`, changed: true, ...o })
  // first preview: roles proposed start ticked, except the embedding while memory is indexed
  assert.deepEqual(recTicks({ picks: [pick('embed'), pick('rerank'), pick('response', { changed: false })], reindex: { rooms: 2 } }), { embed: false, rerank: true })
  assert.deepEqual(recTicks({ picks: [pick('embed'), pick('rerank')] }), { embed: true, rerank: true })
  // a newer preview: the operator's choices stay, a role that is no longer a change disappears, a new one starts by the rule
  const before = { embed: true, rerank: false, response: true }
  assert.deepEqual(recTicks({ picks: [pick('embed'), pick('rerank'), pick('summary')], reindex: { rooms: 2 } }, before), { embed: true, rerank: false, summary: true })
  assert.deepEqual(recTicks({ picks: [pick('rerank')] }, before), { rerank: false })
  assert.deepEqual(recTicks(null), {})
  // a role saved from the dialog (cheaper model) still shows as replaced by hand, and is not sent
  const fresh = { picks: [pick('embed'), pick('rerank', { current_model: 'cheap-rerank' })] }
  const done = recAfterRefresh(fresh, { rerank: { provider: 'p1', model: 'cheap-rerank' } })
  assert.equal(done.picks[1].changed, false)
  assert.equal(done.picks[1].replaced, true)
  assert.equal(done.picks[0].changed, true)
  // another tab moved the role to something else since: the server's own answer stands
  const other = recAfterRefresh({ picks: [pick('rerank', { current_model: 'tab-b-rerank' })] }, { rerank: { provider: 'p1', model: 'cheap-rerank' } })
  assert.equal(other.picks[0].changed, true)
  // the same model on another provider is another setting: another tab moved it, the server's answer stands
  const moved = recAfterRefresh({ picks: [pick('rerank', { current_provider: 'p2', current_model: 'cheap-rerank' })] }, { rerank: { provider: 'p1', model: 'cheap-rerank' } })
  assert.equal(moved.picks[0].changed, true)
  assert.equal(moved.picks[0].replaced, undefined)
  // the model chosen here is the curated pick itself: the server already says unchanged, nothing replaced
  const same = recAfterRefresh({ picks: [pick('rerank', { current_model: 'rerank-pick', changed: false })] }, { rerank: { provider: 'p1', model: 'rerank-pick' } })
  assert.equal(same.picks[0].replaced, false)
})

test('preview, apply a cheaper rerank model at once, then the width follows the new preview', async () => {
  const { recommendedWidth, recTicks, recAfterRefresh } = await load('embedwidth.ts', [])
  // 1) The rerank provider has no key: the preview of the new provider proposes embed + rerank. With both ticked
  //    the width is 1024 (the rerank pick makes reranking usable); without the rerank pick it is the full width.
  const first = {
    picks: [{ role: 'embed', model: 'e', current_model: 'old-e', changed: true }, { role: 'rerank', model: 'r', current_model: 'dead-r', changed: true }],
    embed_dimensions: 1024, embed_dimensions_reason: 'default',
    embed_width_without_rerank_pick: { dimensions: 0, reason: 'no_rerank', full_width: 4096 },
  }
  let on = recTicks(first)
  assert.deepEqual(recommendedWidth(first, on), { dimensions: 1024, reason: 'default', full: 0 })
  // 2) "Use this model" for rerank saves the cheaper model on the new provider: the dialog marks it done (unticked,
  //    not sent) and the old preview would still offer the stale 'no_rerank' alternative. That is the bug the review found.
  on = { ...on, rerank: false }
  const marked = { ...first, picks: first.picks.map((p) => (p.role === 'rerank' ? { ...p, current_model: 'cheap-r', changed: false, replaced: true } : p)) }
  assert.deepEqual(recommendedWidth(marked, on), { dimensions: 0, reason: 'no_rerank', full: 4096 }, 'the old preview shows the full width the server would not store')
  // 3) The preview is read again on the saved settings: the kept rerank is usable now, so both proposals are 1024.
  const fresh = {
    picks: [{ role: 'embed', model: 'e', current_model: 'old-e', changed: true }, { role: 'rerank', model: 'r', current_provider: 'p1', current_model: 'cheap-r', changed: true }],
    embed_dimensions: 1024, embed_dimensions_reason: 'default',
    embed_width_without_rerank_pick: { dimensions: 1024, reason: 'default' },
  }
  const rec = recAfterRefresh(fresh, { rerank: { provider: 'p1', model: 'cheap-r' } })
  on = recTicks(rec, on)
  assert.deepEqual(on, { embed: true }, 'the embedding tick survives; the replaced rerank is no change and has no tick')
  assert.deepEqual(recommendedWidth(rec, on), { dimensions: 1024, reason: 'default', full: 0 }, 'embed alone now applies 1024: what the server stores with the kept usable rerank')
  assert.equal(rec.picks[1].changed, false, 'the replaced rerank is not a change to send')
})

test('a settings change during the first recommendation load is remembered and the preview is read again afterwards', async () => {
  const { recLiveStart, recLiveChange, recLiveLoaded } = await load('embedwidth.ts', [])
  // load starts -> a live change arrives (applying is true during the load, as recBusy is) -> the first answer comes
  let st = recLiveStart()
  assert.deepEqual(st, { loading: true, dirty: false })
  const live = recLiveChange(st, true)
  assert.equal(live.refresh, false, 'there is no preview to refresh yet')
  assert.deepEqual(live.state, { loading: true, dirty: true })
  const done = recLiveLoaded(live.state)
  assert.equal(done.refresh, true, 'the first answer was made before the change: read the preview again at once')
  assert.deepEqual(done.state, { loading: false, dirty: false })
  // nothing changed while loading: no extra read
  assert.equal(recLiveLoaded(recLiveStart()).refresh, false)
  // a failed first load never asks for a refresh
  assert.equal(recLiveLoaded({ loading: true, dirty: true }, false).refresh, false)
  // afterwards a change refreshes the idle preview, and waits while an apply is in flight
  assert.equal(recLiveChange(done.state, false).refresh, true)
  assert.equal(recLiveChange(done.state, true).refresh, false)
  // the page wires them in this order: start before the request, change through the live callback, loaded after the answer
  const src = readFileSync(new URL('../src/pages/Connections.svelte', import.meta.url), 'utf8')
  const open = src.slice(src.indexOf('async function openRecommend'), src.indexOf('// The width proposals (and the picks themselves)'))
  assert.ok(open.indexOf('recLiveStart()') < open.indexOf("call('models.recommend'") && open.indexOf("call('models.recommend'") < open.indexOf('recLiveLoaded(') && open.indexOf('recLiveLoaded(') < open.indexOf('void refreshRecommend()'))
  assert.ok(src.includes('recLiveChange(recLive, recBusy)'), 'the live callback goes through the same state')
})

test('a width request carries the approval it was asked on, and a late refusal on another basis is dropped', async () => {
  const { widthBasis, widthApproval, widthRequest, widthReplyFits } = await load('embedwidth.ts', [])
  const ew = (o) => ({ revision: 7, provider: 'openrouter', model: 'qwen/qwen3-embedding-8b', dimensions: 1024, effective: 1024, full_width: 4096, truncatable: true, recommended: 1024, ...o })
  // the review's order: A asks for 2048 at revision 7 (width 1024: a raise), B stores the full width as revision 8,
  // the read finishes, then the refusal of the first request arrives and the operator confirms.
  const approval = widthApproval(widthBasis(ew()), 'raise')
  assert.deepEqual(approval.basis.revision, 7)
  const first = widthRequest(approval, 2048)
  assert.deepEqual(first, { input: { dimensions: 2048, direction: 'raise' }, baseRevision: 7 })
  const now = widthBasis(ew({ revision: 8, dimensions: 0, effective: 4096 }))
  assert.equal(widthReplyFits(approval, now), false, 'the refusal came back for revision 7 while the picker shows 8: dropped, read again')
  assert.equal(widthReplyFits(approval, widthBasis(ew())), true)
  assert.equal(widthReplyFits(approval, null), false)
  assert.equal(widthReplyFits(null, now), false)
  // the include_embed repeat is made from the stored approval: the revision and direction of the first ask,
  // whatever the picker shows by now
  assert.deepEqual(widthRequest(approval, 2048, true), { input: { dimensions: 2048, direction: 'raise', include_embed: true }, baseRevision: 7 })
  assert.deepEqual(widthRequest(widthApproval(now, 'lower'), 1024), { input: { dimensions: 1024, direction: 'lower' }, baseRevision: 8 })
  // nothing to approve when there is no basis or no change
  assert.equal(widthApproval(null, 'raise'), null)
  assert.equal(widthApproval(now, 'same'), null)
  // the page keeps the approval in the confirmation and sends it back, never rebuilding it from the current basis
  const src = readFileSync(new URL('../src/pages/Connections.svelte', import.meta.url), 'utf8')
  assert.ok(src.includes('widthAsk = { ...ask, value: choice.value, approval }'))
  assert.ok(src.includes('sendWidth(choice, approval, true)'))
  assert.ok(src.includes('widthReplyFits(approval, widthBase)'))
})

test('the recommended width follows the picks ticked in the dialog', async () => {
  const { recommendedWidth } = await load('embedwidth.ts', [])
  const rec = (o) => ({
    picks: [{ role: 'embed', changed: true }, { role: 'rerank', changed: true }, { role: 'response', changed: false }],
    embed_dimensions: 1024,
    embed_dimensions_reason: 'default',
    embed_width_without_rerank_pick: { dimensions: 0, reason: 'no_rerank', full_width: 4096 },
    ...o,
  })
  // every pick kept: the width of the whole set
  assert.deepEqual(recommendedWidth(rec(), { embed: true, rerank: true }), { dimensions: 1024, reason: 'default', full: 0 })
  // the rerank pick unticked: the width the server would store without it, as the full width with its size
  assert.deepEqual(recommendedWidth(rec(), { embed: true, rerank: false }), { dimensions: 0, reason: 'no_rerank', full: 4096 })
  assert.deepEqual(recommendedWidth(rec(), { embed: true }), { dimensions: 0, reason: 'no_rerank', full: 4096 })
  // the embedding row unticked: no width changes, nothing is shown
  assert.equal(recommendedWidth(rec(), { embed: false, rerank: true }), null)
  assert.equal(recommendedWidth(rec(), {}), null)
  // a rerank pick the operator already replaced by hand (not changed any more) is not sent either
  assert.deepEqual(recommendedWidth(rec({ picks: [{ role: 'embed', changed: true }, { role: 'rerank', changed: false }] }), { embed: true, rerank: true }), { dimensions: 0, reason: 'no_rerank', full: 4096 })
  // no rerank pick in the preview: one proposal, whatever rerank says
  const single = rec({ picks: [{ role: 'embed', changed: true }], embed_width_without_rerank_pick: undefined, embed_dimensions: 0, embed_dimensions_reason: 'no_rerank', embed_full_width: 4096 })
  assert.deepEqual(recommendedWidth(single, { embed: true, rerank: false }), { dimensions: 0, reason: 'no_rerank', full: 4096 })
  // the full width of a model that is 1024 wide anyway still carries its size
  assert.deepEqual(recommendedWidth(rec({ embed_dimensions: 0, embed_dimensions_reason: 'default', embed_full_width: 1024, embed_width_without_rerank_pick: undefined }), { embed: true, rerank: true }), { dimensions: 0, reason: 'default', full: 1024 })
  // no width in the preview (embedding kept, or a model that cannot be cut): nothing
  assert.equal(recommendedWidth(rec({ embed_dimensions: undefined, embed_width_without_rerank_pick: undefined }), { embed: true, rerank: true }), null)
  assert.equal(recommendedWidth(null, { embed: true }), null)
})

test('the message-vector estimate is read from the blocks list, never as zeros it did not carry', async () => {
  const { messageVectorAsk } = await load('messagevectors.ts', [])
  assert.deepEqual(
    messageVectorAsk({ memory: { message_vectors: { enabled: false, estimate: { messages: 120, tokens: 5400, usd: 0.0012, seconds: 8, dimensions: 512, disk_bytes: 61920, price_basis: 'prices' } } } }),
    { messages: 120, tokens: 5400, usd: 0.0012, seconds: 8, dimensions: 512, diskBytes: 61920, fallbackPrice: false, unprepared: 0 },
  )
  assert.equal(messageVectorAsk({ memory: { message_vectors: { estimate: { dimensions: 0, disk_bytes: 0 } } } }).dimensions, 0)
  assert.equal(messageVectorAsk({ memory: { message_vectors: { estimate: { messages: 1, price_basis: 'fallback' } } } }).fallbackPrice, true)
  // junk numbers become 0 instead of NaN in the confirm text
  assert.deepEqual(messageVectorAsk({ memory: { message_vectors: { estimate: { messages: 'x', tokens: -4, usd: NaN } } } }), { messages: 0, tokens: 0, usd: 0, seconds: 0, dimensions: 0, diskBytes: 0, fallbackPrice: false, unprepared: 0 })
  // no estimate (the engine could not price it, or the setting is on and nobody asked): the generic note
  assert.equal(messageVectorAsk({ memory: { message_vectors: { enabled: true, stored: 3 } } }), null)
  assert.equal(messageVectorAsk({ memory: {} }), null)
  assert.equal(messageVectorAsk(undefined), null)
})

test('the message-vector switch shows on while the field is absent and off only for an explicit false', async () => {
  const { messageVectorsOn } = await load('messagevectors.ts', [])
  assert.equal(messageVectorsOn(undefined), true)
  assert.equal(messageVectorsOn(null), true)
  assert.equal(messageVectorsOn({}), true)
  assert.equal(messageVectorsOn({ message_vectors: null }), true)
  assert.equal(messageVectorsOn({ message_vectors: true }), true)
  assert.equal(messageVectorsOn({ message_vectors: false }), false)
})

test('messages outside any prepared leaf are reported, never priced as zero', async () => {
  const { messageVectorAsk, messageVectorAskMode } = await load('messagevectors.ts', [])
  const ask = (e) => messageVectorAsk({ memory: { message_vectors: { estimate: e } } })
  assert.equal(ask({ messages: 0, unprepared: 480 }).unprepared, 480)
  assert.equal(ask({ messages: 3, unprepared: -2 }).unprepared, 0)
  // exact when everything is in leaves, a note when some are not, no figures at all when none are
  assert.equal(messageVectorAskMode(ask({ messages: 10, unprepared: 0 })), 'exact')
  assert.equal(messageVectorAskMode(ask({ messages: 10, unprepared: 5 })), 'partial')
  assert.equal(messageVectorAskMode(ask({ messages: 0, unprepared: 5 })), 'unpriced')
  assert.equal(messageVectorAskMode(ask({ messages: 0 })), 'exact')
})

test('turning message vectors on prepares nothing when the setting is not saved', async () => {
  const { enableMessageVectors } = await load('messagevectors.ts', [])
  const blocks = [{ id: 'a' }, { id: 'b' }]
  for (const error of [{ message: 'revision changed' }, { message: 'disk full' }, null]) {
    const prepared = []
    const out = await enableMessageVectors({
      save: async () => ({ ok: false, error }),
      blocks,
      preparing: () => false,
      prepare: async (b) => (prepared.push(b.id), { ok: true, output: {} }),
    })
    assert.deepEqual(prepared, [], 'no paid job may start for a setting that is still off')
    assert.deepEqual(out, { saved: false, saveError: error?.message, started: 0, running: 0, failed: 0 })
  }
})

test('turning message vectors on counts started, already running and failed blocks apart', async () => {
  const { enableMessageVectors } = await load('messagevectors.ts', [])
  const blocks = [{ id: 'new' }, { id: 'busy' }, { id: 'lock' }, { id: 'bad' }, { id: 'throws' }, { id: 'bad2' }]
  const order = []
  const out = await enableMessageVectors({
    save: async () => ({ ok: true, output: { revision: 5 } }),
    blocks,
    preparing: (b) => b.id === 'busy',
    prepare: async (b) => {
      order.push(b.id)
      if (b.id === 'lock') return { ok: true, output: { already_running: true } }
      if (b.id === 'bad') return { ok: false, error: { message: 'no key' } }
      if (b.id === 'bad2') return { ok: false, error: { message: 'later' } }
      if (b.id === 'throws') throw new Error('network')
      return { ok: true, output: {} }
    },
  })
  assert.deepEqual(order, ['new', 'lock', 'bad', 'throws', 'bad2'], 'a block that is preparing is not asked again')
  assert.deepEqual(out, { saved: true, started: 1, running: 2, failed: 3, firstError: 'no key' })
  // all started: nothing failed or running
  const ok = await enableMessageVectors({ save: async () => ({ ok: true }), blocks: blocks.slice(0, 2), preparing: () => false, prepare: async () => ({ ok: true }) })
  assert.deepEqual(ok, { saved: true, started: 2, running: 0, failed: 0 })
  // no blocks: saved only
  assert.deepEqual(await enableMessageVectors({ save: async () => ({ ok: true }), blocks: [], preparing: () => false, prepare: async () => ({ ok: true }) }), { saved: true, started: 0, running: 0, failed: 0 })
})

test('the quick reindex counts message vectors and cached or summary-only leaves as work', async () => {
  const { quickVerdict, indexHasWork } = await load('quickreindex.ts', [])
  // only message vectors are missing (the speaker label changed, or message vectors were just switched on)
  const messagesOnly = { pending: 0, estimate: { leaf_embeds: 0, summaries: 0, message_embeds: 20, seconds: 5 }, rooms: [{ new: 0, changed: 0, removed: 0 }] }
  assert.equal(indexHasWork(messagesOnly), true)
  assert.equal(quickVerdict(messagesOnly), 'ok')
  // a leaf that only lacks a cached vector or a summary vector is pending but not in leaf_embeds
  assert.equal(quickVerdict({ pending: 3, estimate: { leaf_embeds: 0, summaries: 0, seconds: 1 } }), 'ok')
  assert.equal(quickVerdict({ pending: 0, estimate: { leaf_embeds: 0, summaries: 0, message_embeds: 0 } }), 'nothing')
  assert.equal(quickVerdict(null), 'nothing')
})

test('the quick reindex steers a big or failed job away from the request', async () => {
  const { quickVerdict, QUICK_LIMIT, QUICK_MAX_SECONDS } = await load('quickreindex.ts', [])
  const small = { pending: 10, estimate: { leaf_embeds: 10, seconds: 20 } }
  assert.equal(quickVerdict(small), 'ok')
  // the engine cuts leaf and message work at the limit, so a count that reaches it may hide more
  assert.equal(quickVerdict({ pending: QUICK_LIMIT, estimate: { leaf_embeds: 1, seconds: 1 } }), 'big')
  assert.equal(quickVerdict({ pending: 1, estimate: { message_embeds: QUICK_LIMIT, seconds: 1 } }), 'big')
  assert.equal(quickVerdict({ pending: 1, estimate: { leaf_embeds: 1, seconds: QUICK_MAX_SECONDS } }), 'ok')
  assert.equal(quickVerdict({ pending: 1, estimate: { leaf_embeds: 1, seconds: QUICK_MAX_SECONDS + 1 } }), 'big')
  // a large unsynced import is resynced before any limit applies
  assert.equal(quickVerdict({ pending: 5, estimate: { leaf_embeds: 5, seconds: 5 }, rooms: [{ new: QUICK_LIMIT, changed: 0, removed: 0 }] }), 'big')
  // a room that failed to resync makes every number incomplete, so it is neither "nothing to do" nor runnable
  assert.equal(quickVerdict({ pending: 0, estimate: {}, failed_rooms: { 'kakao:x': 'io error' } }), 'failed')
  assert.equal(quickVerdict({ pending: 4, estimate: { leaf_embeds: 4, seconds: 1 }, failed_rooms: { a: 'x' } }), 'failed')
})

test('a stopped or partly failed quick pass is not reported as complete', async () => {
  const { quickOutcome } = await load('quickreindex.ts', [])
  assert.deepEqual(quickOutcome({ leaf_vectors: 3 }), { failedRooms: 0, stoppedBy: '', complete: true })
  assert.deepEqual(quickOutcome({ stopped_by: 'provider_error' }), { failedRooms: 0, stoppedBy: 'provider_error', complete: false })
  assert.deepEqual(quickOutcome({ failed_rooms: { a: 'secret detail' } }), { failedRooms: 1, stoppedBy: '', complete: false })
})

test('DM conversation form: stored values become choices and back into only the edits that changed', async () => {
  const dm = await load('dmsettings.ts', [])
  // nothing stored: members, both limits at their defaults
  const empty = dm.dmDraft(undefined)
  assert.deepEqual(empty, { allow: 'members', user: { mode: 'default', n: '' }, day: { mode: 'default', n: '' } })
  assert.deepEqual(dm.dmOps(empty, undefined), [])
  // 0 is the default, -1 is no limit, anything else is a number
  const stored = { allow: 'operators', per_user_per_day: -1, per_day: 250 }
  const d = dm.dmDraft(stored)
  assert.deepEqual(d, { allow: 'operators', user: { mode: 'none', n: '' }, day: { mode: 'custom', n: '250' } })
  assert.deepEqual(dm.dmOps(d, stored), [])
  // an unknown stored mode shows as the default one
  assert.equal(dm.dmDraft({ allow: 'everyone' }).allow, 'members')
  // back to the default removes the key; a number and "no limit" write -1 or the number; unchanged fields write nothing
  d.user = { mode: 'default', n: '' }
  d.day = { mode: 'custom', n: '300' }
  d.allow = 'off'
  assert.deepEqual(dm.dmOps(d, stored), [
    { path: ['dm', 'allow'], value: 'off' },
    { path: ['dm', 'per_user_per_day'], op: 'unset' },
    { path: ['dm', 'per_day'], value: 300 },
  ])
  const none = dm.dmDraft(undefined)
  none.day = { mode: 'none', n: '' }
  assert.deepEqual(dm.dmOps(none, undefined), [{ path: ['dm', 'per_day'], value: -1 }])
  // a typed number that equals the stored one is no change, even with spaces
  const same = dm.dmDraft({ per_day: 40 })
  same.day.n = ' 40 '
  assert.deepEqual(dm.dmOps(same, { per_day: 40 }), [])
})

test('DM conversation form: typed limits are checked against the server ranges', async () => {
  const dm = await load('dmsettings.ts', [])
  const draft = (user, day) => ({ allow: 'members', user: { mode: 'custom', n: user }, day: { mode: 'custom', n: day } })
  assert.deepEqual(dm.dmErrors(draft('1', '1')), { user: undefined, day: undefined })
  assert.deepEqual(dm.dmErrors(draft('10000', '100000')), { user: undefined, day: undefined })
  // a number box bound by the form holds a number (or null when emptied), not a string
  assert.deepEqual(dm.dmErrors(draft(80, 5)), { user: undefined, day: undefined })
  assert.equal(dm.dmErrors(draft(0, 5)).user, 'range')
  assert.equal(dm.dmErrors(draft(null, 5)).user, 'range')
  assert.equal(dm.dmErrors(draft(2.5, 5)).user, 'range')
  assert.equal(dm.limitValue({ mode: 'custom', n: 80 }), 80)
  assert.equal(dm.dmErrors(draft('10001', '5')).user, 'range')
  assert.equal(dm.dmErrors(draft('5', '100001')).day, 'range')
  for (const bad of ['', '0', '-1', '2.5', '1e3', 'abc', ' ']) assert.equal(dm.dmErrors(draft(bad, '5')).user, 'range', `"${bad}"`)
  // default and "no limit" never need a number
  const open = { allow: 'members', user: { mode: 'none', n: 'abc' }, day: { mode: 'default', n: '' } }
  assert.deepEqual(dm.dmErrors(open), { user: undefined, day: undefined })
})

test('DM conversation form: warns when one person may use more than everybody together', async () => {
  const dm = await load('dmsettings.ts', [])
  const mk = (user, day) => ({ allow: 'members', user, day })
  const custom = (n) => ({ mode: 'custom', n })
  const dflt = { mode: 'default', n: '' }
  const none = { mode: 'none', n: '' }
  assert.equal(dm.dmOrderWarning(mk(dflt, dflt)), false) // 30 of 100
  assert.equal(dm.dmOrderWarning(mk(custom('500'), dflt)), true) // 500 > the default 100
  assert.equal(dm.dmOrderWarning(mk(custom('500'), none)), false) // no overall limit
  assert.equal(dm.dmOrderWarning(mk(none, custom('50'))), false) // no per-person limit
  assert.equal(dm.dmOrderWarning(mk(custom('x'), custom('50'))), false) // not a number yet
})
