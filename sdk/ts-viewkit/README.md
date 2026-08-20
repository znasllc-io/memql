# @znasllc-io/memql-view-kit

Framework-agnostic renderer for MemQL concept rows: rows plus `@displayCard`
hints in, HTML out. No DOM dependency, no runtime dependencies.

## Why it exists

Two surfaces render the same thing: the VS Code extension's concept browser
(`editors/vscode/src/webview/conceptPanel.ts`) and the MemQL portal. Any
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
  renderValueView,
  renderToHtml,
  escapeHtml,
  viewKitStyles,
} from "@znasllc-io/memql-view-kit";

const listHtml = renderToHtml(renderRowList(rows, concept, selectedRowId));
const detailHtml = renderToHtml(renderValueView(row));
```

`concept` is a `ConceptLike` -- `{ id, entity, displayCard? }`, mirroring
MemQL's `ConceptInfo`. `rows` are `RowLike` (`Record<string, unknown>`). The
types are declared structurally rather than imported from the SDK so any
caller that can produce these shapes can use the package without taking on the
SDK's wire types.

Note the asymmetry between the two renderers, which is deliberate: the row LIST
expects a flattened row (payload merged up, because a display card names
payload fields directly), while the VALUE VIEW expects the raw nested wire
shape -- flattening there would drop the intrinsics an operator came to read.
The VS Code consumer does that flattening in
`editors/vscode/src/state/rowProjection.ts`.

## The value viewer

`renderValueView` is **the** value surface. Not one of several: a row's detail,
a run's result, an automation trace and its per-step output all render through
it, and a consumer that reaches for `JSON.stringify` into a `<pre>` has
reintroduced the problem it exists to solve.

`renderDetail` was the earlier one and is **gone** (memql#3751). It walked a
row and emitted every node, always expanded, with no types, no filter and no
ceiling -- so a large payload produced a page nobody could read and a deeply
nested one produced a page the webview struggled to draw. It has no deprecation
shim and no alias; reaching for the name gets a compile error, which is the
intent.

What the replacement adds, and why each one is not a nicety:

- **Collapsing.** Subtrees below `expandDepth` start closed, so the shape is
  legible before the contents are.
- **Type badges.** `"12"` and `12` render identically as text and mean
  different things to the engine.
- **A filter.** Case-insensitive, matched against keys *and* scalar values, so
  finding a field in a wide row does not mean scrolling it.
- **Bounds.** `maxStringLength` truncates a long scalar, `pageSize` pages a long
  array, and `nodeBudget` is a hard ceiling on nodes emitted. A value large
  enough to hang the host renders as much as the budget allows and says it
  stopped -- rather than being drawn, or silently dropped.
- **Cycle safety.** The ANCESTOR chain is tracked, not every object ever seen,
  so a value that legitimately appears twice renders twice and only a real
  cycle is cut.

Copy affordances are **off** unless `copy: true`, because they do nothing until
the host attaches the delegated listener described by `VALUE_VIEW_ATTRS` -- and
a button that does nothing is worse than no button. Every node carries a
copyable path (`joinPath`), rooted at whatever `path` the caller passes.

The root's entries render directly rather than inside a disclosure of their
own: the root IS the thing being looked at, and putting it behind a collapsed
node would hide the whole view.

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

The core rendering surface. The package also exports the element library
(table, calendar, timeline, kanban, charts, ...), the fitness and arrangement
layers, and the display-card and value-formatting helpers -- `src/index.ts` is
the full list.

| Export | Purpose |
| --- | --- |
| `renderRowList(rows, concept, selectedRowId?)` | The row list, projected through the concept's display card. |
| `renderValueView(value, options?)` | **The** value surface: recursive, collapsing, type-badged, filterable and bounded. Keeps payload / provenance / intrinsics distinct. |
| `valueTypeName(value)` | The type name a badge shows. |
| `joinPath(segments)` | A node's copyable path. |
| `VALUE_VIEW_ATTRS` | The data attributes a host's delegated listener reads. |
| `DEFAULT_EXPAND_DEPTH` / `_PAGE_SIZE` / `_MAX_STRING_LENGTH` / `_NODE_BUDGET` | The defaults `ValueViewOptions` overrides. |
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
