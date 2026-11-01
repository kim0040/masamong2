// A person's memory in one server (memory.mine.show / memory.mine.forget), as the console shows it. Pure
// helpers so the page stays thin and the rules are testable without a browser.

export type MineItem = { id: string; kind: string; text: string; created_at: string; explicit?: boolean }

/** The items the person asked to have remembered, apart from the ones that only name them. */
export function splitMine(items: MineItem[] | null | undefined): { told: MineItem[]; other: MineItem[] } {
  const list = Array.isArray(items) ? items : []
  return { told: list.filter((i) => i.explicit), other: list.filter((i) => !i.explicit) }
}

/** What forgetting would remove, in all: nothing means there is nothing to confirm. */
export function forgetTotal(p: { items?: number; episodes?: number; cards?: number } | null | undefined): number {
  return (p?.items ?? 0) + (p?.episodes ?? 0) + (p?.cards ?? 0)
}
