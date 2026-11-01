export type LazyModule<T> = { default: T }

// Route chunks fail independently from the initial console boot. Keep both paths settled so callers
// can show their existing error state or simply ignore a speculative prefetch failure.
export function loadRoute<T>(
  load: () => Promise<LazyModule<T>>,
  loaded: (component: T) => void,
  failed: (error: unknown) => void,
): Promise<void> {
  return load().then((module) => loaded(module.default)).catch(failed)
}

export function prefetchRoute<T>(load: () => Promise<LazyModule<T>>, loaded: (component: T) => void): Promise<void> {
  return load().then((module) => loaded(module.default)).catch(() => {})
}
