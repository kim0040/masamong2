// English is the complete fallback; Korean is complete, while additional locales may be partial.
// Run directly for the repository, or import checkCatalogs to validate candidate catalogs in tests.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { basename, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))

function walk(dir) {
  return readdirSync(dir).flatMap((name) => (statSync(join(dir, name)).isDirectory() ? walk(join(dir, name)) : [join(dir, name)]))
}

function braceTokens(value) {
  return [...value.matchAll(/\{[^{}]*\}/g)].map(([braced]) => {
    const inner = braced.slice(1, -1).trim()
    if (/^\w+$/.test(inner)) return `placeholder:${inner}`
    if (!inner) return 'literal:{}'
    try {
      // A JSON example may translate its values, but its keys and value types must survive.
      const object = JSON.parse(braced)
      return `literal:json:${JSON.stringify(Object.fromEntries(Object.entries(object).map(([key, val]) => [key, typeof val])))}`
    } catch {
      return `literal:${inner.replace(/\s+/g, ' ')}`
    }
  }).sort().join('|')
}

const escapeRegex = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
// Existing first-party catalogs deliberately leave this fallback label blank. New locales cannot.
const legacyBlankKeys = new Set(['tool.cond.unknown'])
const pluralSuffix = /^(.*)\.(zero|one|two|few|many|other)$/

export function checkCatalogs(bundles, sources) {
  const en = bundles.get('en')
  const ko = bundles.get('ko')
  const problems = []
  const used = new Set()
  const dynamic = new Set()
  if (!en || typeof en !== 'object' || Array.isArray(en)) problems.push('locales/ui/en.json is required as the UI fallback')
  if (!ko || typeof ko !== 'object' || Array.isArray(ko)) problems.push('locales/ui/ko.json is required')
  if (problems.length) return { problems, used }

  for (const source of sources) {
    for (const match of source.matchAll(/\bt\(\s*(['"])([^'"\n]+)\1/g)) used.add(match[2])
    for (const match of source.matchAll(/\bt\(\s*`([^`]+)`/g)) {
      if (match[1].includes('${')) dynamic.add(match[1])
      else used.add(match[1])
    }
  }
  for (const key of used) if (!Object.hasOwn(en, key)) problems.push(`missing in en: ${key}`)
  for (const template of dynamic) {
    const pattern = new RegExp(`^${template.split(/\$\{[^}]+\}/g).map(escapeRegex).join('.+')}$`)
    if (![...Object.keys(en)].some((key) => pattern.test(key))) problems.push(`missing dynamic key pattern in en: ${template}`)
  }

  const pluralGroups = new Map()
  for (const key of Object.keys(en)) {
    const match = key.match(pluralSuffix)
    if (!match || !Object.hasOwn(en, match[1])) continue
    if (!pluralGroups.has(match[1])) pluralGroups.set(match[1], [match[1]])
    pluralGroups.get(match[1]).push(key)
  }

  for (const [locale, bundle] of bundles) {
    if (!bundle || Array.isArray(bundle) || typeof bundle !== 'object') {
      problems.push(`${locale}: root must be an object`)
      continue
    }
    for (const [key, value] of Object.entries(bundle)) {
      if (typeof value !== 'string' || (!value.trim() && !(value === '' && legacyBlankKeys.has(key) && (locale === 'en' || locale === 'ko')))) {
        problems.push(`${locale}: ${key} must be a nonempty string`)
        continue
      }
      const category = key.match(pluralSuffix)
      const reference = !Object.hasOwn(en, key) && locale !== 'en' && locale !== 'ko' && category && pluralGroups.has(category[1])
        ? category[1] : key
      if (!Object.hasOwn(en, reference)) problems.push(`${locale}: unknown key ${key}`)
      else if (typeof en[reference] === 'string' && braceTokens(value) !== braceTokens(en[reference])) {
        problems.push(`${locale}: placeholders or literal braces differ for ${key}`)
      }
    }
    if (locale !== 'en' && locale !== 'ko') {
      const translatedGroups = new Set()
      for (const key of Object.keys(bundle)) {
        if (pluralGroups.has(key)) translatedGroups.add(key)
        const category = key.match(pluralSuffix)
        if (category && pluralGroups.has(category[1])) translatedGroups.add(category[1])
      }
      for (const base of translatedGroups) {
        for (const key of pluralGroups.get(base)) {
          if (!Object.hasOwn(bundle, key)) problems.push(`${locale}: incomplete plural group ${base}: missing ${key}`)
        }
      }
    }
  }
  for (const key of Object.keys(en)) if (!Object.hasOwn(ko, key)) problems.push(`missing in ko: ${key}`)
  for (const key of Object.keys(ko)) if (!Object.hasOwn(en, key)) problems.push(`only in ko: ${key}`)
  return { problems, used }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const localeDir = join(root, '../../../../locales/ui')
  const files = readdirSync(localeDir).filter((name) => name.endsWith('.json')).sort()
  const bundles = new Map(files.map((name) => [basename(name, '.json'), JSON.parse(readFileSync(join(localeDir, name), 'utf8'))]))
  const sources = walk(join(root, 'src')).filter((file) => /\.(svelte|ts)$/.test(file)).map((file) => readFileSync(file, 'utf8'))
  const { problems, used } = checkCatalogs(bundles, sources)
  if (problems.length) {
    console.error('console strings:\n  ' + problems.join('\n  '))
    process.exitCode = 1
  } else {
    const en = bundles.get('en')
    const coverage = [...bundles].map(([locale, bundle]) => `${locale} ${Object.keys(bundle).length}/${Object.keys(en).length}`).join(', ')
    console.log(`console strings: ${used.size} keys used, ${Object.keys(en).length} defined; ${coverage}`)
  }
}
