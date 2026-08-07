// A minimal virtual-node tree and HTML serializer.
//
// view-kit deliberately does NOT touch the DOM. Its consumers are a VS Code
// webview (which is handed an HTML string) and, later, the memQL portal. A
// pure data-to-string renderer is testable under `node --test` with no jsdom
// and no browser, and it keeps the package dependency-free.
//
// Interactivity is expressed as data attributes; the host attaches a single
// delegated listener and reads them back. view-kit never emits inline
// handlers -- a webview Content-Security-Policy forbids them, and row data is
// untrusted.

export type VNode =
  | { readonly tag: string; readonly attrs: Record<string, string>; readonly children: VNode[] }
  | { readonly text: string };

// HTML void elements: serialized without a closing tag.
const VOID_TAGS = new Set([
  "area", "base", "br", "col", "embed", "hr", "img",
  "input", "link", "meta", "source", "track", "wbr",
]);

export function h(
  tag: string,
  attrs: Record<string, string>,
  children: VNode[] = [],
): VNode {
  return { tag, attrs, children };
}

export function text(value: string): VNode {
  return { text: value };
}

// escapeHtml neutralizes every character that could break out of either a text
// node or a double-quoted attribute value. One function covers both positions:
// escaping the superset is always safe, and a single routine cannot drift out
// of sync with itself the way a text/attribute pair can.
export function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

export function renderToHtml(node: VNode): string {
  if ("text" in node) {
    return escapeHtml(node.text);
  }
  const attrs = Object.entries(node.attrs)
    .map(([k, v]) => ` ${k}="${escapeHtml(v)}"`)
    .join("");
  if (VOID_TAGS.has(node.tag)) {
    return `<${node.tag}${attrs}>`;
  }
  const inner = node.children.map(renderToHtml).join("");
  return `<${node.tag}${attrs}>${inner}</${node.tag}>`;
}
