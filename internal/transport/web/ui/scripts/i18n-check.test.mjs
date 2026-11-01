import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkCatalogs } from './i18n-check.mjs'

const en = {
  'common.label': 'Hello {name} and {name}',
  'common.example': 'JSON: {"region": "Busan"}; empty {}',
  'nav.home': 'Home',
  'tour.first.title': 'Welcome',
  'items.count': '{n} items',
  'items.count.one': '{n} item',
  'items.count.other': '{n} items',
}
const check = (fr = {}, source = "t('common.label'); t(`nav.${item.key}`); t(`tour.${step.key}.title`)", english = en) =>
  checkCatalogs(new Map([['en', english], ['ko', { ...english }], ['fr', fr]]), [source]).problems

test('a partial candidate locale falls back to English without shipping a catalog', () => {
  assert.deepEqual(check({ 'common.label': 'Salut {name} et {name}', 'common.example': 'JSON : {"region": "Paris"} ; vide {}' }), [])
})

test('plural groups are atomic in additional locales, while unrelated partial keys remain valid', () => {
  assert.deepEqual(check({ 'nav.home': 'Accueil' }), [])
  assert.match(check({ 'items.count': '{n} articles' }).join('\n'), /fr: incomplete plural group items.count: missing items.count.one/)
  assert.match(check({ 'items.count': '{n} articles' }).join('\n'), /fr: incomplete plural group items.count: missing items.count.other/)
  assert.match(check({ 'items.count.one': '{n} article' }).join('\n'), /fr: incomplete plural group items.count: missing items.count/)
  assert.deepEqual(check({ 'nav.home': 'Accueil', 'items.count': '{n} articles', 'items.count.one': '{n} article', 'items.count.other': '{n} articles' }), [])
  const englishWithOneOnly = { ...en, 'notifications.count': '{n} alerts', 'notifications.count.one': '{n} alert' }
  assert.deepEqual(check({ 'notifications.count': '{n} alertes', 'notifications.count.one': '{n} alerte' }, undefined, englishWithOneOnly), [])
  assert.deepEqual(check({ 'notifications.count': '{n} alertes', 'notifications.count.one': '{n} alerte', 'notifications.count.other': '{n} alertes' }, undefined, englishWithOneOnly), [])
})

test('Arabic may add CLDR categories only for known count groups', () => {
  const base = { 'items.count': '{n} عناصر', 'items.count.one': '{n} عنصر', 'items.count.other': '{n} عناصر' }
  const extras = Object.fromEntries(['zero', 'two', 'few', 'many'].map((category) => [`items.count.${category}`, `{n} ${category}`]))
  const arabic = (bundle) => checkCatalogs(new Map([['en', en], ['ko', { ...en }], ['ar', bundle]]), []).problems
  assert.deepEqual(arabic({ 'nav.home': 'الرئيسية', ...base, ...extras }), [])
  const incomplete = arabic({ 'items.count.few': '{n} قليل' }).join('\n')
  for (const key of ['items.count', 'items.count.one', 'items.count.other']) {
    assert.ok(incomplete.includes(`ar: incomplete plural group items.count: missing ${key}`))
  }
  assert.match(arabic({ ...base, 'items.count.few': 'قليل' }).join('\n'), /ar: placeholders or literal braces differ for items.count.few/)
  assert.match(arabic({ ...base, 'items.count.few': '{n} {n} قليل' }).join('\n'), /ar: placeholders or literal braces differ for items.count.few/)
  assert.match(arabic({ ...base, 'nav.home.zero': '{n} خطأ' }).join('\n'), /ar: unknown key nav.home.zero/)
  assert.match(arabic({ ...base, 'missing.count.zero': '{n} خطأ' }).join('\n'), /ar: unknown key missing.count.zero/)
  assert.match(arabic({ ...base, 'items.count.ordinal': '{n} خطأ' }).join('\n'), /ar: unknown key items.count.ordinal/)
})

test('rejects missing, extra and repeated placeholder changes', () => {
  assert.match(check({ 'common.label': 'Salut {name}' }).join('\n'), /placeholders or literal braces differ for common.label/)
  assert.match(check({ 'common.label': 'Salut {name} {other}' }).join('\n'), /placeholders or literal braces differ for common.label/)
})

test('checks literal braces and JSON examples while allowing localized JSON values', () => {
  assert.match(check({ 'common.example': 'JSON : {"region": "Paris"}' }).join('\n'), /placeholders or literal braces differ for common.example/)
  assert.match(check({ 'common.example': 'JSON : {"city": "Paris"} ; vide {}' }).join('\n'), /placeholders or literal braces differ for common.example/)
})

test('rejects blank values, unknown keys, missing static keys and unmatched dynamic patterns', () => {
  assert.match(check({ 'common.label': '  ' }).join('\n'), /nonempty string/)
  assert.match(check({ 'tool.cond.unknown': '' }, '', { ...en, 'tool.cond.unknown': '' }).join('\n'), /fr: tool.cond.unknown must be a nonempty string/)
  assert.match(check({}, '', { ...en, 'tool.cond.unknown': ' ' }).join('\n'), /en: tool.cond.unknown must be a nonempty string/)
  assert.match(check({ 'made.up': 'Bonjour' }).join('\n'), /unknown key made.up/)
  assert.match(check({}, "t('missing.key')").join('\n'), /missing in en: missing.key/)
  assert.match(check({}, 't(`missing.${item.key}`)').join('\n'), /missing dynamic key pattern in en/)
  assert.match(check({}, 't(`tour.${step.key}.body`)').join('\n'), /missing dynamic key pattern in en/)
  assert.match(check({}, '', { ...en, 'common.label': '' }).join('\n'), /en: common.label must be a nonempty string/)
})

test('Korean remains complete', () => {
  const bundles = new Map([['en', en], ['ko', { 'common.label': en['common.label'] }]])
  assert.match(checkCatalogs(bundles, []).problems.join('\n'), /missing in ko: nav.home/)
})
