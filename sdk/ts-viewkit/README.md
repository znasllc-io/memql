# @znasllc-io/memql-view-kit

Framework-agnostic renderer for memQL concept rows: rows plus `@displayCard`
hints in, HTML out. No DOM dependency, no runtime dependencies.

## Why it exists

Two surfaces render the same thing: the VS Code extension's concept browser
(`editors/vscode/src/webview/conceptPanel.ts`) and the memQL portal. Any
concept-aware markup written inside either one is markup the other has to
rebuild, and the two answers diverge on day one rather than in some future
refactor. view-kit is the shared answer.

It carries one hard rule: **there is no concept-specific rendering code,
anywhere.** A row is projected through whatever `@displayCard` slots its
concept declares and degrades to the row id when it declares none, or when the
named field is absent. That is what lets a newly declared concept render the
day it is declared, with no renderer update and no release.

## Design constraints

- **No DOM.** `renderToHtml` returns a string. There is no `document`, no
  `<style>` injection, no `adoptedStyleSheets`. A VS Code webview is handed an
  HTML string, so a DOM-based renderer could not serve it at all -- and a
  pure data-to-string renderer is testable under bare `node --test` with no
  jsdom and no browser.
- **No inline event handlers.** Interactivity is expressed as data attributes
  (`data-row-id`, `data-selected`, `data-status`); the host attaches one
  delegated listener and reads them back. Row data is untrusted, so the
  consuming webview runs under a Content-Security-Policy that forbids inline
  handlers outright.
- **Zero dependencies.** Both consumers bundle it.
- **Escaping is not optional.** `escapeHtml` neutralises every character that
  could break out of either a text node or a double-quoted attribute value,
  and every value that reaches the output passes through it.

## Usage

```ts
import {
  renderRowList,
  renderDetail,
  renderToHtml,
  escapeHtml,
  viewKitStyles,
} from "@znasllc-io/memql-view-kit";

const listHtml = renderToHtml(renderRowList(rows, concept, selectedRowId));
const detailHtml = renderToHtml(renderDetail(row));
```

`concept` is a `ConceptLike` -- `{ id, entity, displayCard? }`, mirroring
memQL's `ConceptInfo`. `rows` are `RowLike` (`Record<string, unknown>`). The
types are declared structurally rather than imported from the SDK so any
caller that can produce these shapes can use the package without taking on the
SDK's wire types.

Note the asymmetry between the two renderers, which is deliberate: the row LIST
expects a flattened row (payload merged up, because a display card names
payload fields directly), while the DETAIL view expects the raw nested wire
shape -- flattening there would drop the intrinsics an operator came to read.
The VS Code consumer does that flattening in
`editors/vscode/src/state/rowProjection.ts`.

## Styling

`viewKitStyles` is the stylesheet for view-kit's own class contract
(`vk-rows`, `vk-row`, `vk-row-primary`, `vk-key`, `vk-value`, `vk-cycle`, ...).
It ships with the markup it styles because view-kit owns what those classes
MEAN, and leaving each consumer to re-derive that from class names alone is
how the two surfaces drift.

Every colour is `var(--vk-*, <fallback>)`. A host themes view-kit by defining
the tokens listed in `VIEW_KIT_CSS_VARIABLES`; a host that defines nothing
still gets a legible, self-contained rendering. view-kit never names a
host-specific variable itself -- the VS Code panel maps `--vk-*` onto
`--vscode-*` in one small block, and the portal will map them onto its own
tokens in another.

Scope is view-kit-owned classes ONLY. Page chrome -- toolbars, panes, grid
layout, buttons, error banners -- belongs to the consumer, which knows its own
layout. Putting it here would make view-kit dictate page structure it cannot
see.

```ts
// In a webview, under a nonce-carrying CSP:
`<style nonce="${nonce}">
  :root { --vk-fg: var(--vscode-foreground); /* ... */ }
${viewKitStyles}
</style>`
```

## API

| Export | Purpose |
| --- | --- |
| `renderRowList(rows, concept, selectedRowId?)` | The row list, projected through the concept's display card. |
| `renderDetail(row)` | Recursive walk of a row's full nested shape (payload / provenance / intrinsics stay distinct). |
| `rowDisplayId(row)` | The row's id as a display string; `""` when absent or not a string. |
| `renderToHtml(node)` | Serialise a `VNode` tree to an HTML string. |
| `h(tag, attrs, children?)` / `text(value)` | `VNode` constructors. |
| `escapeHtml(value)` | Text- and attribute-safe escaping. |
| `viewKitStyles` | The stylesheet, as a string. |
| `VIEW_KIT_CSS_VARIABLES` | The `--vk-*` tokens a host may define. |

## Development

```bash
npm ci
npm run build      # tsc -> dist/
npm test           # tsc -p tsconfig.test.json && node --test dist-test/test/*.js
npm run typecheck
```
