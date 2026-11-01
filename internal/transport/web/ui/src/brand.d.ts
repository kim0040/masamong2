declare module 'virtual:brand' {
  /** The address of a file under public/brand, versioned by its content. */
  export function brand(path: string): string
}
