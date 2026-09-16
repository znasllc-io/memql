/** Read only explicitly declared page metadata, never form values or page text.
 * Retained panes remain mounted but must not become another page's context. */
export function visiblePageContext(root: HTMLElement | null, base: string): string {
  if (!root) return base;
  const pages = Array.from(root.querySelectorAll<HTMLElement>('[data-os-page-context]'))
    .filter(node => !node.closest('[hidden], [inert], [aria-hidden="true"]'))
    .flatMap(node => {
      try { return [JSON.parse(node.dataset.osPageContext || '{}')]; }
      catch { return []; }
    });
  return pages.length ? `${base}\nVisible page data: ${JSON.stringify(pages)}` : base;
}

/** A short human label for the Ask sheet; identifiers stay in transport context. */
export function visiblePageLabel(root: HTMLElement | null, fallback: string): string {
  if (!root) return fallback;
  const labels: string[] = [];
  for (const node of root.querySelectorAll<HTMLElement>('[data-os-page-context]')) {
    if (node.closest('[hidden], [inert], [aria-hidden="true"]')) continue;
    try {
      const page = JSON.parse(node.dataset.osPageContext || '{}');
      const name = page.name || page.machine || page.source || page.page;
      for (const label of [name, page.view && !["Overview", "Equipment"].includes(page.view) ? page.view : undefined, page.selectedModel]) {
        if (typeof label === 'string' && label && !labels.includes(label)) labels.push(label);
      }
    } catch { /* A malformed declaration cannot prevent opening Ask. */ }
  }
  return labels.length ? labels.join(' / ') : fallback;
}
