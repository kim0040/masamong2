import type { Component } from 'svelte'
import * as I from './icons'

// A page reached through another page's menu item shows as one of its sections (tabs above the page).
export type NavSection = { href: string; key: string }
export type NavItem = { href: string; key: string; icon: Component<any>; sections?: NavSection[] }
export type NavGroup = { key: string; items: NavItem[] }

// The sidebar. Labels are locale keys (nav.<key>, sections nav.section.<key>); routes must also be in
// routes.json. Pages that belong together share one item: statistics and costs, service upkeep and the
// settings file. Display & access sits in the sidebar footer and the top bar.
export const nav: NavGroup[] = [
  { key: 'ops', items: [
    { href: '/', key: 'home', icon: I.House },
    { href: '/turns', key: 'turns', icon: I.MessageSquareText },
    { href: '/jobs', key: 'jobs', icon: I.ListChecks },
  ] },
  { key: 'bot', items: [
    { href: '/rooms', key: 'rooms', icon: I.Hash },
    { href: '/tools', key: 'tools', icon: I.Wrench },
    { href: '/persona', key: 'persona', icon: I.VenetianMask },
    { href: '/people', key: 'people', icon: I.Users },
  ] },
  { key: 'memory', items: [
    { href: '/blocks', key: 'blocks', icon: I.Blocks },
    { href: '/memory', key: 'memory', icon: I.Brain },
    { href: '/lab', key: 'lab', icon: I.FlaskConical },
  ] },
  { key: 'insight', items: [
    { href: '/stats', key: 'stats', icon: I.ChartColumn, sections: [{ href: '/stats', key: 'stats' }, { href: '/usage', key: 'usage' }] },
  ] },
  { key: 'system', items: [
    { href: '/connections', key: 'connections', icon: I.Plug },
    { href: '/logs', key: 'logs', icon: I.ScrollText },
    { href: '/maintenance', key: 'maintenance', icon: I.HardDrive, sections: [{ href: '/maintenance', key: 'maintenance' }, { href: '/settings', key: 'settings' }] },
  ] },
]

/** Pages outside the sidebar list, for the command palette. */
export const extraPages: NavItem[] = [
  { href: '/usage', key: 'usage', icon: I.Gauge },
  { href: '/settings', key: 'settings', icon: I.Settings2 },
  { href: '/display', key: 'display', icon: I.SlidersHorizontal },
]

const under = (path: string, href: string) => href !== '/' && (path === href || path.startsWith(href + '/'))

/** The sidebar item a path belongs to (its own item, or the item it is a section of). */
export function activeFor(path: string): string {
  let best = '/'
  let len = 0
  for (const g of nav)
    for (const it of g.items)
      for (const h of [it.href, ...(it.sections ?? []).map((s) => s.href)])
        if (under(path, h) && h.length > len) {
          best = it.href
          len = h.length
        }
  return best
}

/** The sections of the item a path belongs to, when it has several. */
export function sectionsFor(path: string): NavSection[] {
  const href = activeFor(path)
  for (const g of nav) for (const it of g.items) if (it.href === href && it.sections) return it.sections
  return []
}
