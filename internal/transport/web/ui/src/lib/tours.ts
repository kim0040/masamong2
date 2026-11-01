// Feature tours: 2–4 steps per page. A step points at an element marked data-tour="…"; text lives in
// locales (tour.<page>.<n>.title / .body). Pages in autoTours show theirs on the first visit (once per
// admin, prefs.tips[page]; "Reset tutorial" on the Display page shows it again); every page with a
// tour has a "?" in its header that shows it on request.
export type TourStep = { target: string; key: string }

/** The tour of a path: a room page is /rooms/:room whatever its server and channel. */
export function tourKey(path: string): string {
  return /^\/rooms\/[^/]+\/[^/]+$/.test(path) ? '/rooms/:room' : path
}

export const autoTours = ['/']

export const tours: Record<string, TourStep[]> = {
  '/': [
    { target: 'status', key: 'home.1' },
    { target: 'attention', key: 'home.2' },
    { target: 'edit', key: 'home.3' },
  ],
  // every room page shares one tour (and one "seen" flag): see tourKey
  '/rooms/:room': [
    { target: 'room-tabs', key: 'room.1' },
    { target: 'room-try', key: 'room.2' },
    { target: 'room-copy', key: 'room.3' },
  ],
  '/people': [
    { target: 'people-table', key: 'people.1' },
    { target: 'people-access', key: 'people.2' },
  ],
  '/rooms': [
    { target: 'rooms-table', key: 'rooms.1' },
    { target: 'rooms-add', key: 'rooms.2' },
  ],
  '/tools': [
    { target: 'tools-grid', key: 'tools.1' },
    { target: 'tools-new', key: 'tools.2' },
  ],
  '/blocks': [
    { target: 'blocks-import', key: 'blocks.1' },
    { target: 'blocks-tabs', key: 'blocks.2' },
  ],
  '/lab': [
    { target: 'lab-setup', key: 'lab.1' },
    { target: 'lab-composer', key: 'lab.2' },
    { target: 'lab-tabs', key: 'lab.3' },
  ],
  '/memory': [
    { target: 'memory-search', key: 'memory.1' },
    { target: 'memory-tabs', key: 'memory.2' },
  ],
  '/settings': [{ target: 'settings-tabs', key: 'settings.1' }],
  '/maintenance': [
    { target: 'maint-schedule', key: 'maint.1' },
    { target: 'maint-backups', key: 'maint.2' },
  ],
}
