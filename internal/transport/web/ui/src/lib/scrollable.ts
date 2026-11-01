// A region that scrolls must be reachable by keyboard (WCAG 2.1.1): once its content is wider or taller
// than the box it gets tabindex=0, so arrow keys can scroll it. Removed again when it fits.
export function scrollable(node: HTMLElement) {
  const sync = () => {
    if (node.scrollWidth > node.clientWidth + 1 || node.scrollHeight > node.clientHeight + 1) node.tabIndex = 0
    else node.removeAttribute('tabindex')
  }
  const resize = new ResizeObserver(sync)
  resize.observe(node)
  const mutate = new MutationObserver(sync)
  mutate.observe(node, { childList: true, subtree: true, characterData: true })
  sync()
  return {
    destroy() {
      resize.disconnect()
      mutate.disconnect()
    },
  }
}
