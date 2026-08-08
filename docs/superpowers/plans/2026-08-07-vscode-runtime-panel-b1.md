# VS Code Runtime Panel — Increment B1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the memQL VS Code extension an activity-bar presence that connects to a cluster, lists clusters from `~/.memql/clusters.yaml`, and browses every concept's rows and detail — with the rendering done by a new framework-agnostic package the portal will reuse.

**Architecture:** Three layers, each independently testable. A new `sdk/ts-viewkit` package turns rows plus `@displayCard` hints into an HTML string with no DOM dependency. `sdk/ts` gains concept-browsing functions with keyset paging. The extension wires VS Code tree views and webview tabs on top, keeping all logic in modules that never import `vscode` so they can be tested with Node's built-in runner.

**Tech Stack:** TypeScript 5.4, Node 20, `node:test` + `node:assert/strict` (no test framework), `vscode-languageclient` 10, the `yaml` package for comment-preserving config round-trips, VS Code API ^1.91.

## Global Constraints

- **No new runtime dependencies in `sdk/ts` or `sdk/ts-viewkit`.** Both stay zero-dependency. Dev dependencies are limited to `typescript` and `@types/node`.
- **`sdk/ts-viewkit` must not touch the DOM.** No `document`, no `window`, no jsdom. It emits VNode trees and HTML strings.
- **Extension modules containing logic must not import `vscode`.** Only files under `src/views/`, `src/webview/`, and `extension.ts` may. This is what makes the rest testable.
- **Tests use `node:test` and `node:assert/strict`.** No mocha, jest, vitest, or `@vscode/test-electron`.
- **No emojis** in any documentation, code comment, UI string, or commit message. Use `SUCCESS:`, `ERROR:`, `WARNING:`, `INFO:` and `[ ]` / `[x]`.
- **Stage files by explicit path.** Never `git add -A` or `git add .` — the repo owner runs concurrent sessions against this working tree.
- **Node floor:** `>=20`. **VS Code floor:** `^1.91.0`. Any dependency added to `editors/vscode` must declare `engines.node` admitting Node 18 (the extension host Node major for VS Code 1.91) or `cmd/memql-lsp` drift guards fail.
- **This increment adds no engine changes and no new proto messages.** Everything consumed already exists on `MemqlService.Stream`.
- **B1 is read-only against cluster data.** No mutations, no deploy actions, no construct execution. Those are B2 through B4.

---

## File Structure

**New package — `sdk/ts-viewkit/`** (published as `@znasllc-io/memql-view-kit`)

| File | Responsibility |
|---|---|
| `package.json` | Package manifest, zero runtime deps, `node --test` scripts |
| `tsconfig.json` | Build config, mirrors `sdk/ts` |
| `tsconfig.test.json` | Test overlay emitting to `dist-test` |
| `src/vnode.ts` | `VNode` type, `h()`, `text()`, `renderToHtml()`, HTML escaping |
| `src/types.ts` | `DisplayCardHints`, `RowLike`, `ConceptLike` — the data contract |
| `src/rowList.ts` | `renderRowList()` — rows plus display-card hints to a VNode |
| `src/detail.ts` | `renderDetail()` — recursive walk of payload, provenance, intrinsics |
| `src/index.ts` | Package barrel |
| `test/vnode.test.ts` | Serialization and escaping |
| `test/rowList.test.ts` | Display card present, absent, partial |
| `test/detail.test.ts` | Nested payloads, arrays, null handling |

**Modified — `sdk/ts/`**

| File | Responsibility |
|---|---|
| `src/client/conceptBrowser.ts` | **Create.** `browseConceptPage()`, `getRowByConceptAndId()` |
| `src/client/index.ts` | **Modify.** Export the new surface |
| `test/conceptBrowser.test.ts` | **Create.** Mock-dispatcher coverage |

**Modified — `editors/vscode/`**

| File | Responsibility |
|---|---|
| `src/clusters/model.ts` | **Create.** `ClusterConfig`, `ClustersFile`, `needsAuth()` |
| `src/clusters/file.ts` | **Create.** Comment-preserving read / merge / write of `clusters.yaml` |
| `src/connection/endpoint.ts` | **Create.** Cluster config to WebSocket URL |
| `src/connection/manager.ts` | **Create.** Connection lifecycle and state |
| `src/views/clustersTree.ts` | **Create.** Clusters `TreeDataProvider` |
| `src/views/conceptsTree.ts` | **Create.** Concepts `TreeDataProvider` |
| `src/webview/conceptPanel.ts` | **Create.** Concept rows and detail webview tab |
| `src/extension.ts` | **Modify.** Register views, commands, connection manager |
| `icons/memql-activity.svg` | **Create.** 24x24 single-fill activity-bar icon |
| `package.json` | **Modify.** Contributions, `yaml` dependency, trust posture, test script |
| `tsconfig.test.json` | **Create.** Test overlay |
| `test/clustersFile.test.ts` | **Create.** Round-trip and merge coverage |
| `test/endpoint.test.ts` | **Create.** URL derivation coverage |

**Modified — build and CI**

| File | Responsibility |
|---|---|
| `Makefile` | **Modify.** `viewkit-install`, `viewkit-typecheck`, `viewkit-test`, `vscode-test` |
| `.github/workflows/ci.yml` | **Modify.** `viewkit` change bucket, lane, extension test step |

---

## Task 1: view-kit package with VNode core

Creates the package and its serializer. Nothing renders yet — this task delivers the primitive everything else emits into, plus the build and CI wiring so later tasks have a lane that runs them.

**Files:**
- Create: `sdk/ts-viewkit/package.json`
- Create: `sdk/ts-viewkit/tsconfig.json`
- Create: `sdk/ts-viewkit/tsconfig.test.json`
- Create: `sdk/ts-viewkit/.gitignore`
- Create: `sdk/ts-viewkit/src/vnode.ts`
- Create: `sdk/ts-viewkit/src/index.ts`
- Create: `sdk/ts-viewkit/test/vnode.test.ts`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type VNode = { tag: string; attrs: Record<string, string>; children: VNode[] } | { text: string }`
  - `function h(tag: string, attrs: Record<string, string>, children?: VNode[]): VNode`
  - `function text(value: string): VNode`
  - `function renderToHtml(node: VNode): string`
  - `function escapeHtml(value: string): string`

- [ ] **Step 1: Create the package manifest**

Create `sdk/ts-viewkit/package.json`:

```json
{
  "name": "@znasllc-io/memql-view-kit",
  "version": "0.1.0",
  "description": "Framework-agnostic renderer for memQL concept rows: rows plus @displayCard hints in, HTML out. No DOM dependency.",
  "type": "module",
  "main": "./dist/index.js",
  "types": "./dist/index.d.ts",
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "import": "./dist/index.js"
    }
  },
  "files": [
    "dist",
    "src",
    "README.md"
  ],
  "scripts": {
    "build": "tsc",
    "typecheck": "tsc --noEmit",
    "test": "tsc -p tsconfig.test.json && node --test dist-test/test/*.js",
    "clean": "rm -rf dist dist-test"
  },
  "devDependencies": {
    "@types/node": "^20.12.7",
    "typescript": "^5.4.5"
  },
  "engines": {
    "node": ">=20"
  },
  "license": "UNLICENSED",
  "repository": {
    "type": "git",
    "url": "git+https://github.com/znasllc-io/memql.git",
    "directory": "sdk/ts-viewkit"
  },
  "publishConfig": {
    "registry": "https://npm.pkg.github.com"
  }
}
```

- [ ] **Step 2: Create the TypeScript configs**

Create `sdk/ts-viewkit/tsconfig.json`:

```json
{
  "compilerOptions": {
    "module": "Node16",
    "moduleResolution": "Node16",
    "target": "ES2021",
    "lib": ["ES2021"],
    "outDir": "dist",
    "rootDir": "src",
    "declaration": true,
    "declarationMap": true,
    "sourceMap": true,
    "strict": true,
    "esModuleInterop": true,
    "skipLibCheck": true
  },
  "include": ["src"],
  "exclude": ["node_modules", "dist", "dist-test"]
}
```

Create `sdk/ts-viewkit/tsconfig.test.json`:

```json
{
  "extends": "./tsconfig.json",
  "compilerOptions": {
    "rootDir": ".",
    "outDir": "dist-test",
    "declaration": false,
    "declarationMap": false,
    "sourceMap": false,
    "types": ["node"]
  },
  "include": ["src/**/*", "test/**/*"]
}
```

Create `sdk/ts-viewkit/.gitignore`:

```
node_modules/
dist/
dist-test/
```

- [ ] **Step 3: Write the failing test**

Create `sdk/ts-viewkit/test/vnode.test.ts`:

```typescript
// Serializer tests. view-kit emits HTML into a VS Code webview and, later,
// the portal -- every value that reaches renderToHtml is untrusted row data,
// so escaping is a correctness requirement, not a nicety.

import test from "node:test";
import assert from "node:assert/strict";

import { h, text, renderToHtml, escapeHtml } from "../src/vnode.js";

test("renderToHtml serializes a bare element", () => {
  assert.equal(renderToHtml(h("div", {})), "<div></div>");
});

test("renderToHtml serializes attributes", () => {
  assert.equal(
    renderToHtml(h("div", { class: "row", "data-row-id": "abc" })),
    '<div class="row" data-row-id="abc"></div>',
  );
});

test("renderToHtml serializes nested children", () => {
  const node = h("ul", {}, [h("li", {}, [text("one")]), h("li", {}, [text("two")])]);
  assert.equal(renderToHtml(node), "<ul><li>one</li><li>two</li></ul>");
});

test("renderToHtml escapes text content", () => {
  assert.equal(
    renderToHtml(h("p", {}, [text('<script>alert("x")</script>')])),
    "<p>&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;</p>",
  );
});

test("renderToHtml escapes attribute values", () => {
  assert.equal(
    renderToHtml(h("div", { title: 'a" onmouseover="evil()' })),
    '<div title="a&quot; onmouseover=&quot;evil()"></div>',
  );
});

test("renderToHtml emits void elements without a closing tag", () => {
  assert.equal(renderToHtml(h("br", {})), "<br>");
  assert.equal(renderToHtml(h("hr", { class: "sep" })), '<hr class="sep">');
});

test("escapeHtml handles the five significant characters", () => {
  assert.equal(escapeHtml(`&<>"'`), "&amp;&lt;&gt;&quot;&#39;");
});

test("escapeHtml leaves ordinary text untouched", () => {
  assert.equal(escapeHtml("v1:agents:agent"), "v1:agents:agent");
});
```

- [ ] **Step 4: Run the test to verify it fails**

```bash
cd sdk/ts-viewkit && npm install --no-audit --no-fund && npm test
```

Expected: FAIL — `Cannot find module '../src/vnode.js'`.

- [ ] **Step 5: Write the implementation**

Create `sdk/ts-viewkit/src/vnode.ts`:

```typescript
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
```

Create `sdk/ts-viewkit/src/index.ts`:

```typescript
export { h, text, renderToHtml, escapeHtml, type VNode } from "./vnode.js";
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
cd sdk/ts-viewkit && npm test
```

Expected: PASS — 8 tests.

- [ ] **Step 7: Add Makefile targets**

In `Makefile`, immediately after the `sdk-ts-test` target (around line 442), add:

```makefile
## Install view-kit (@znasllc-io/memql-view-kit) dev dependencies. Idempotent.
viewkit-install:
	cd sdk/ts-viewkit && npm install --no-audit --no-fund

## Typecheck view-kit. Runs `tsc --noEmit` against sdk/ts-viewkit.
viewkit-typecheck:
	cd sdk/ts-viewkit && npm run typecheck

## Run the view-kit test suite via node:test. Zero runtime deps.
viewkit-test:
	cd sdk/ts-viewkit && npm test
```

Add the three target names to the `.PHONY` line at line 359 that already lists `sdk-ts-install sdk-ts-typecheck`.

- [ ] **Step 8: Verify the Makefile targets work**

```bash
make viewkit-install && make viewkit-typecheck && make viewkit-test
```

Expected: all three succeed; the test run reports 8 passing tests.

- [ ] **Step 9: Add the CI change bucket and lane**

In `.github/workflows/ci.yml`, in the `changes` job's filter block, directly after the `sdkts:` bucket (around line 294), add:

```yaml
            # The framework-agnostic renderer shared by the VS Code extension
            # and the portal. Its own bucket rather than folded into `sdkts`:
            # it is a separate package with a separate install + test cycle,
            # and a change confined to it must still run its lane.
            viewkit:
              - 'sdk/ts-viewkit/**'
```

In the `changes` job's `outputs:` block, alongside `sdkts:` (around line 63), add:

```yaml
      viewkit: ${{ steps.filter.outputs.viewkit }}
```

After the `sdk-ts-typecheck` job (which ends around line 814), add:

```yaml
  # view-kit lane. Gates on `viewkit` (sdk/ts-viewkit/**) so a change confined
  # to the renderer runs its tests without dragging the whole Go suite.
  viewkit-checks:
    name: viewkit-checks
    timeout-minutes: 10
    needs: changes
    if: ${{ github.event_name != 'pull_request' || needs.changes.outputs.ci == 'true' || needs.changes.outputs.viewkit == 'true' }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-node@820762786026740c76f36085b0efc47a31fe5020 # v4
        with:
          node-version: '20'
      - name: install
        run: scripts/ci/retry.sh -- make viewkit-install
      - name: typecheck
        run: make viewkit-typecheck
      - name: test
        run: make viewkit-test
```

In the `ci-required` aggregator's `needs:` list (around line 1056, alongside `sdk-ts-typecheck`), add:

```yaml
      - viewkit-checks
```

- [ ] **Step 10: Verify the workflow parses**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('SUCCESS: workflow parses')"
```

Expected: `SUCCESS: workflow parses`.

- [ ] **Step 11: Commit**

```bash
git add sdk/ts-viewkit/package.json sdk/ts-viewkit/tsconfig.json \
        sdk/ts-viewkit/tsconfig.test.json sdk/ts-viewkit/.gitignore \
        sdk/ts-viewkit/src/vnode.ts sdk/ts-viewkit/src/index.ts \
        sdk/ts-viewkit/test/vnode.test.ts sdk/ts-viewkit/package-lock.json \
        Makefile .github/workflows/ci.yml
git commit -m "feat(viewkit): add the view-kit package with a VNode HTML serializer

Framework-agnostic renderer shared by the VS Code panel and the future
portal. No DOM dependency: it emits VNode trees and HTML strings, so it
tests under node:test with no jsdom.

Escaping covers text and attribute positions with one routine, since row
data reaching the renderer is untrusted."
```

---

## Task 2: view-kit row list rendering

Renders a page of concept rows using whatever `@displayCard` slots the concept declares, degrading to id and intrinsics when it declares none.

**Files:**
- Create: `sdk/ts-viewkit/src/types.ts`
- Create: `sdk/ts-viewkit/src/rowList.ts`
- Modify: `sdk/ts-viewkit/src/index.ts`
- Create: `sdk/ts-viewkit/test/rowList.test.ts`

**Interfaces:**
- Consumes: `h`, `text`, `VNode` from Task 1.
- Produces:
  - `interface DisplayCardHints { primary?: string; secondary?: string; tertiary?: string; status?: string }`
  - `interface ConceptLike { id: string; entity: string; displayCard?: DisplayCardHints }`
  - `type RowLike = Record<string, unknown>`
  - `function renderRowList(rows: RowLike[], concept: ConceptLike, selectedRowId?: string): VNode`
  - `function rowDisplayId(row: RowLike): string`

- [ ] **Step 1: Write the failing test**

Create `sdk/ts-viewkit/test/rowList.test.ts`:

```typescript
// Row-list rendering. The contract that matters: NO concept-specific code,
// ever. A concept declaring @displayCard gets its slots honored; one that
// declares nothing still renders usefully the day it is declared.

import test from "node:test";
import assert from "node:assert/strict";

import { renderRowList, rowDisplayId } from "../src/rowList.js";
import { renderToHtml } from "../src/vnode.js";

const AGENT = {
  id: "v1:agents:agent",
  entity: "agent",
  displayCard: { primary: "name", secondary: "role", status: "active" },
};

const BARE = { id: "v1:cluster:node", entity: "node" };

test("renders one element per row", () => {
  const html = renderToHtml(
    renderRowList(
      [
        { id: "a1", name: "Sofia", role: "hr" },
        { id: "a2", name: "Faye", role: "eng" },
      ],
      AGENT,
    ),
  );
  assert.equal(html.match(/data-row-id=/g)?.length, 2);
});

test("carries the row id as a data attribute for host delegation", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.match(html, /data-row-id="a1"/);
});

test("uses the display card primary slot as the row label", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.match(html, />Sofia</);
});

test("renders secondary and tertiary slots when present", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "Sofia", role: "hr" }], AGENT),
  );
  assert.match(html, /class="vk-row-secondary">hr</);
});

test("omits a slot the row has no value for", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.doesNotMatch(html, /vk-row-secondary/);
});

test("renders a status badge from the status slot", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "Sofia", active: true }], AGENT),
  );
  assert.match(html, /class="vk-row-status" data-status="true"/);
});

test("falls back to the row id when the concept declares no display card", () => {
  const html = renderToHtml(renderRowList([{ id: "bff-local" }], BARE));
  assert.match(html, />bff-local</);
});

test("falls back to the row id when the primary slot is absent from the row", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", role: "hr" }], AGENT));
  assert.match(html, />a1</);
});

test("marks the selected row", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1" }, { id: "a2" }], BARE, "a2"),
  );
  assert.match(html, /data-row-id="a2" data-selected="true"/);
});

test("renders an empty-state element for zero rows", () => {
  const html = renderToHtml(renderRowList([], AGENT));
  assert.match(html, /vk-empty/);
  assert.match(html, /No rows/);
});

test("escapes row values", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "<img src=x onerror=evil()>" }], AGENT),
  );
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});

test("rowDisplayId prefers id and tolerates a missing one", () => {
  assert.equal(rowDisplayId({ id: "a1" }), "a1");
  assert.equal(rowDisplayId({}), "");
});
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd sdk/ts-viewkit && npm test
```

Expected: FAIL — `Cannot find module '../src/rowList.js'`.

- [ ] **Step 3: Write the data contract**

Create `sdk/ts-viewkit/src/types.ts`:

```typescript
// The data contract view-kit renders. Deliberately structural rather than
// imported from the SDK: view-kit must stay consumable by any caller that can
// produce these shapes, including the portal, without taking a dependency on
// the SDK's wire types.

// DisplayCardHints mirrors the per-concept rendering hints memQL publishes on
// ConceptInfo.display_card, declared in the DSL via `@displayCard(...)`. Each
// value NAMES A FIELD on the row, it is not the value itself.
export interface DisplayCardHints {
  primary?: string;
  secondary?: string;
  tertiary?: string;
  status?: string;
}

export interface ConceptLike {
  id: string;
  entity: string;
  displayCard?: DisplayCardHints;
}

export type RowLike = Record<string, unknown>;
```

- [ ] **Step 4: Write the row-list renderer**

Create `sdk/ts-viewkit/src/rowList.ts`:

```typescript
// Row-list rendering.
//
// The rule this file exists to enforce: there is NO concept-specific rendering
// code, anywhere. A row is projected through whatever @displayCard slots its
// concept declares, and degrades to the row id when it declares none or when
// the named field is absent. That is what lets a newly declared concept render
// the day it is declared, with no renderer update.

import { h, text, type VNode } from "./vnode.js";
import type { ConceptLike, RowLike } from "./types.js";

// scalarField reads a row field and renders it as a display string. Non-scalar
// values (objects, arrays) are deliberately NOT stringified here -- a display
// card naming an object field is an authoring mistake, and showing
// "[object Object]" in a row label hides it. Returning empty makes the slot
// fall through to its fallback instead.
function scalarField(row: RowLike, field: string | undefined): string {
  if (!field) return "";
  const v = row[field];
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "";
}

export function rowDisplayId(row: RowLike): string {
  const id = row["id"];
  return typeof id === "string" ? id : "";
}

export function renderRowList(
  rows: RowLike[],
  concept: ConceptLike,
  selectedRowId?: string,
): VNode {
  if (rows.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text(`No rows for ${concept.entity}.`),
    ]);
  }

  const card = concept.displayCard;
  const items = rows.map((row) => {
    const id = rowDisplayId(row);
    const attrs: Record<string, string> = { class: "vk-row", "data-row-id": id };
    if (selectedRowId !== undefined && id === selectedRowId) {
      attrs["data-selected"] = "true";
    }

    const children: VNode[] = [];

    // Primary falls back to the row id so a row is always clickable and always
    // identifiable, even with no display card at all.
    const primary = scalarField(row, card?.primary) || id;
    children.push(h("span", { class: "vk-row-primary" }, [text(primary)]));

    const secondary = scalarField(row, card?.secondary);
    if (secondary) {
      children.push(h("span", { class: "vk-row-secondary" }, [text(secondary)]));
    }

    const tertiary = scalarField(row, card?.tertiary);
    if (tertiary) {
      children.push(h("span", { class: "vk-row-tertiary" }, [text(tertiary)]));
    }

    const status = scalarField(row, card?.status);
    if (status) {
      children.push(
        h("span", { class: "vk-row-status", "data-status": status }, [text(status)]),
      );
    }

    return h("li", attrs, children);
  });

  return h("ul", { class: "vk-rows" }, items);
}
```

- [ ] **Step 5: Export the new surface**

Replace the contents of `sdk/ts-viewkit/src/index.ts`:

```typescript
export { h, text, renderToHtml, escapeHtml, type VNode } from "./vnode.js";
export { renderRowList, rowDisplayId } from "./rowList.js";
export type { ConceptLike, DisplayCardHints, RowLike } from "./types.js";
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
cd sdk/ts-viewkit && npm test
```

Expected: PASS — 12 new tests plus the 8 from Task 1.

- [ ] **Step 7: Commit**

```bash
git add sdk/ts-viewkit/src/types.ts sdk/ts-viewkit/src/rowList.ts \
        sdk/ts-viewkit/src/index.ts sdk/ts-viewkit/test/rowList.test.ts
git commit -m "feat(viewkit): render concept rows from display-card hints

Projects rows through whatever @displayCard slots the concept declares,
falling back to the row id when it declares none or the named field is
absent. No concept-specific rendering code -- a newly declared concept
renders the day it is declared."
```

---

## Task 3: view-kit detail rendering

Renders a single row's full nested shape: payload, provenance, and intrinsics, walked recursively.

**Files:**
- Create: `sdk/ts-viewkit/src/detail.ts`
- Modify: `sdk/ts-viewkit/src/index.ts`
- Create: `sdk/ts-viewkit/test/detail.test.ts`

**Interfaces:**
- Consumes: `h`, `text`, `VNode` from Task 1; `RowLike` from Task 2.
- Produces: `function renderDetail(node: RowLike): VNode`

- [ ] **Step 1: Write the failing test**

Create `sdk/ts-viewkit/test/detail.test.ts`:

```typescript
// Detail rendering. The cockpit calls this shape "Hybrid C": no
// concept-specific rendering, just a recursive walk of the row's payload,
// provenance and intrinsics. Tests pin the walk's handling of every JSON
// shape a payload can hold.

import test from "node:test";
import assert from "node:assert/strict";

import { renderDetail } from "../src/detail.js";
import { renderToHtml } from "../src/vnode.js";

test("renders a scalar field as a key/value pair", () => {
  const html = renderToHtml(renderDetail({ name: "Sofia" }));
  assert.match(html, /vk-key">name</);
  assert.match(html, /vk-value">Sofia</);
});

test("renders numbers and booleans", () => {
  const html = renderToHtml(renderDetail({ count: 3, active: true }));
  assert.match(html, /vk-value">3</);
  assert.match(html, /vk-value">true</);
});

test("renders null as an explicit marker rather than blank", () => {
  const html = renderToHtml(renderDetail({ deletedAt: null }));
  assert.match(html, /vk-value vk-null">null</);
});

test("walks a nested object recursively", () => {
  const html = renderToHtml(
    renderDetail({ provenance: { source: "seed", actor: "system" } }),
  );
  assert.match(html, /vk-key">provenance</);
  assert.match(html, /vk-key">source</);
  assert.match(html, /vk-value">seed</);
});

test("renders arrays with indices", () => {
  const html = renderToHtml(renderDetail({ tags: ["a", "b"] }));
  assert.match(html, /vk-key">\[0\]</);
  assert.match(html, /vk-value">a</);
  assert.match(html, /vk-key">\[1\]</);
});

test("renders an array of objects", () => {
  const html = renderToHtml(renderDetail({ phases: [{ name: "one" }] }));
  assert.match(html, /vk-key">\[0\]</);
  assert.match(html, /vk-key">name</);
  assert.match(html, /vk-value">one</);
});

test("renders an empty object as an explicit marker", () => {
  const html = renderToHtml(renderDetail({ metadata: {} }));
  assert.match(html, /vk-value vk-empty-value">\{\}</);
});

test("renders an empty array as an explicit marker", () => {
  const html = renderToHtml(renderDetail({ tags: [] }));
  assert.match(html, /vk-value vk-empty-value">\[\]</);
});

test("escapes keys and values", () => {
  const html = renderToHtml(renderDetail({ "<k>": "<v>" }));
  assert.doesNotMatch(html, /<k>/);
  assert.match(html, /&lt;k&gt;/);
  assert.match(html, /&lt;v&gt;/);
});

test("renders an empty row without throwing", () => {
  assert.match(renderToHtml(renderDetail({})), /vk-detail/);
});

test("terminates on a self-referential payload instead of recursing forever", () => {
  const cyclic: Record<string, unknown> = { name: "root" };
  cyclic["self"] = cyclic;
  const html = renderToHtml(renderDetail(cyclic));
  assert.match(html, /vk-value vk-cycle">\[circular\]</);
});
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd sdk/ts-viewkit && npm test
```

Expected: FAIL — `Cannot find module '../src/detail.js'`.

- [ ] **Step 3: Write the implementation**

Create `sdk/ts-viewkit/src/detail.ts`:

```typescript
// Detail rendering -- a recursive walk of a row's full nested shape.
//
// This is the cockpit's "Hybrid C" approach: no concept-specific rendering,
// so a concept works the day it is declared. The walk preserves the wire's
// nesting (payload / provenance / intrinsics stay distinct) rather than
// flattening, because flattening drops the intrinsics an operator came here
// to read.

import { h, text, type VNode } from "./vnode.js";
import type { RowLike } from "./types.js";

// The seen-set guards against a self-referential structure. Wire payloads are
// JSON and cannot be cyclic, but renderDetail is a public export and a caller
// can hand it any object -- unbounded recursion would hang the host process,
// which in a webview means a hung editor.
function renderValue(value: unknown, seen: Set<object>): VNode {
  if (value === null || value === undefined) {
    return h("span", { class: "vk-value vk-null" }, [text("null")]);
  }
  if (typeof value === "string") {
    return h("span", { class: "vk-value" }, [text(value)]);
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return h("span", { class: "vk-value" }, [text(String(value))]);
  }
  if (typeof value === "object") {
    if (seen.has(value as object)) {
      return h("span", { class: "vk-value vk-cycle" }, [text("[circular]")]);
    }
    seen.add(value as object);
    const node = Array.isArray(value)
      ? renderEntries(value.map((v, i) => [`[${i}]`, v] as const), seen, "[]")
      : renderEntries(Object.entries(value as Record<string, unknown>), seen, "{}");
    seen.delete(value as object);
    return node;
  }
  // Functions and symbols cannot appear in wire data; render the type rather
  // than crashing if a caller passes one.
  return h("span", { class: "vk-value" }, [text(typeof value)]);
}

function renderEntries(
  entries: readonly (readonly [string, unknown])[],
  seen: Set<object>,
  emptyMarker: string,
): VNode {
  if (entries.length === 0) {
    return h("span", { class: "vk-value vk-empty-value" }, [text(emptyMarker)]);
  }
  return h(
    "div",
    { class: "vk-nested" },
    entries.map(([key, value]) =>
      h("div", { class: "vk-field" }, [
        h("span", { class: "vk-key" }, [text(key)]),
        renderValue(value, seen),
      ]),
    ),
  );
}

export function renderDetail(node: RowLike): VNode {
  const seen = new Set<object>([node]);
  return h("div", { class: "vk-detail" }, [
    renderEntries(Object.entries(node), seen, "{}"),
  ]);
}
```

- [ ] **Step 4: Export the new surface**

Replace the contents of `sdk/ts-viewkit/src/index.ts`:

```typescript
export { h, text, renderToHtml, escapeHtml, type VNode } from "./vnode.js";
export { renderRowList, rowDisplayId } from "./rowList.js";
export { renderDetail } from "./detail.js";
export type { ConceptLike, DisplayCardHints, RowLike } from "./types.js";
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd sdk/ts-viewkit && npm test
```

Expected: PASS — 11 new tests, 31 total.

- [ ] **Step 6: Commit**

```bash
git add sdk/ts-viewkit/src/detail.ts sdk/ts-viewkit/src/index.ts \
        sdk/ts-viewkit/test/detail.test.ts
git commit -m "feat(viewkit): render row detail as a recursive nested walk

Preserves the wire's nesting rather than flattening, since flattening
drops the intrinsics an operator opened the detail pane to read. Guards
against self-referential input so a bad caller cannot hang the host."
```

---

## Task 4: SDK concept browsing

Adds keyset-paginated concept browsing to `sdk/ts`, mirroring the Go SDK's `BrowseConceptPage`.

**Files:**
- Create: `sdk/ts/src/client/conceptBrowser.ts`
- Modify: `sdk/ts/src/client/index.ts`
- Create: `sdk/ts/test/conceptBrowser.test.ts`

**Interfaces:**
- Consumes: `QueryClient` (existing, `sdk/ts/src/client/query.ts`), `Result` and `Row` (existing, `sdk/ts/src/client/types.ts`).
- Produces:
  - `const DEFAULT_CONCEPT_BROWSE_PAGE_SIZE = 200`
  - `interface ConceptPage { rows: Row[]; nextCursor: string }`
  - `function browseConceptPage(query: QueryClient, conceptId: string, opts?: { cursor?: string; pageSize?: number; signal?: AbortSignal }): Promise<ConceptPage>`
  - `function getRowByConceptAndId(query: QueryClient, conceptId: string, rowId: string, opts?: { signal?: AbortSignal }): Promise<Row | null>`

- [ ] **Step 1: Write the failing test**

Create `sdk/ts/test/conceptBrowser.test.ts`:

```typescript
// Concept-browser tests. The query string is the contract: it MUST declare
// sort + paginate, or the engine applies its implicit unmarked-list backstop
// (MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows) and silently truncates with no
// continuation cursor (memql#2008). These tests pin the emitted string.

import test from "node:test";
import assert from "node:assert/strict";

import {
  browseConceptPage,
  getRowByConceptAndId,
  DEFAULT_CONCEPT_BROWSE_PAGE_SIZE,
} from "../src/client/conceptBrowser.js";
import { QueryClient } from "../src/client/query.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private reply: Record<string, unknown> = {};

  setReply(payload: Record<string, unknown>): void {
    this.reply = payload;
  }

  send(msg: ClientMessage): string {
    this.sent.push(msg);
    return "mock-0";
  }

  async sendAndWait(msg: ClientMessage): Promise<ServerMessage> {
    this.sent.push(msg);
    return this.reply as unknown as ServerMessage;
  }

  addEventListener(): () => void {
    return () => {};
  }

  registerStream(): () => void {
    return () => {};
  }

  lastQuery(): string {
    const last = this.sent.at(-1) as { executeQuery?: { query?: string } } | undefined;
    return last?.executeQuery?.query ?? "";
  }

  lastCursor(): string | undefined {
    const last = this.sent.at(-1) as { executeQuery?: { cursor?: string } } | undefined;
    return last?.executeQuery?.cursor;
  }
}

function client(d: MockDispatcher): QueryClient {
  return new QueryClient(d as unknown as Dispatcher);
}

function bundleReply(nodes: unknown[], cursor?: string): Record<string, unknown> {
  return {
    queryResult: {
      requestId: "r",
      result: {
        bundle: { nodes },
        ...(cursor === undefined ? {} : { meta: { cursor } }),
      },
    },
  };
}

test("emits a sort+paginate query so the keyset window applies", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(
    d.lastQuery(),
    `sort(paginate(concept==v1:agents:agent, ${DEFAULT_CONCEPT_BROWSE_PAGE_SIZE}), "createdAt", "asc")`,
  );
});

test("honors an explicit page size", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { pageSize: 25 });
  assert.match(d.lastQuery(), /paginate\(concept==v1:agents:agent, 25\)/);
});

test("falls back to the default page size for a non-positive value", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { pageSize: 0 });
  assert.match(
    d.lastQuery(),
    new RegExp(`paginate\\(concept==v1:agents:agent, ${DEFAULT_CONCEPT_BROWSE_PAGE_SIZE}\\)`),
  );
});

test("forwards the continuation cursor", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { cursor: "opaque-1" });
  assert.equal(d.lastCursor(), "opaque-1");
});

test("omits the cursor on a first page", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(d.lastCursor(), undefined);
});

test("preserves the full nested node shape rather than flattening", async () => {
  const d = new MockDispatcher();
  d.setReply(
    bundleReply([
      { id: "a1", concept: "v1:agents:agent", payload: { name: "Sofia" } },
    ]),
  );
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.rows.length, 1);
  assert.deepEqual(page.rows[0]?.["payload"], { name: "Sofia" });
});

test("returns the next cursor when the engine mints one", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1" }], "next-page"));
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.nextCursor, "next-page");
});

test("returns an empty cursor when the set is exhausted", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1" }]));
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.nextCursor, "");
});

test("rejects an empty concept id", async () => {
  const d = new MockDispatcher();
  await assert.rejects(
    () => browseConceptPage(client(d), ""),
    /conceptId is required/,
  );
});

test("getRowByConceptAndId queries by concept and id", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1", payload: { name: "Sofia" } }]));
  const row = await getRowByConceptAndId(client(d), "v1:agents:agent", "a1");
  assert.equal(d.lastQuery(), "concept==v1:agents:agent && id==a1");
  assert.deepEqual(row?.["payload"], { name: "Sofia" });
});

test("getRowByConceptAndId returns null when nothing matches", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  const row = await getRowByConceptAndId(client(d), "v1:agents:agent", "nope");
  assert.equal(row, null);
});

test("getRowByConceptAndId rejects a missing row id", async () => {
  const d = new MockDispatcher();
  await assert.rejects(
    () => getRowByConceptAndId(client(d), "v1:agents:agent", ""),
    /rowId is required/,
  );
});
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd sdk/ts && npm test
```

Expected: FAIL — `Cannot find module '../src/client/conceptBrowser.js'`.

- [ ] **Step 3: Write the implementation**

Create `sdk/ts/src/client/conceptBrowser.ts`:

```typescript
// Generic concept browsing -- the TypeScript mirror of sdk/go's
// concept_browser.go.
//
// This bypasses the named-primitive surface deliberately, and it is the ONLY
// sanctioned reason to do so. A concept browser iterates the registry from
// listConcepts and lets an operator click into any concept's rows without
// knowing its name at compile time; that use case is concept-name-agnostic by
// definition, so no equivalent named primitive can exist. Every other caller
// uses a generated typed method.

import type { QueryClient, QueryCallOptions } from "./query.js";
import type { Row } from "./types.js";

// A concept registry can hold far more than the engine's implicit
// unmarked-list backstop (MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows), so the
// browse query MUST declare sort + paginate to opt into a keyset window and a
// continuation cursor -- otherwise it silently truncates at the backstop with
// no way to page past it (memql#2008).
export const DEFAULT_CONCEPT_BROWSE_PAGE_SIZE = 200;

export interface ConceptPage {
  // rows preserve the full nested wire shape (payload / metadata / provenance
  // / intrinsics), exactly like Result.rawNodes(). A generic inspector needs
  // the intrinsics that flattening would drop.
  rows: Row[];
  // nextCursor is the opaque continuation token for the following page, or ""
  // when the set is exhausted. It is bound to this query's sort ordering and
  // carries no server session state, so it resolves on any replica.
  nextCursor: string;
}

export interface ConceptBrowseOptions extends QueryCallOptions {
  pageSize?: number;
}

export async function browseConceptPage(
  query: QueryClient,
  conceptId: string,
  opts: ConceptBrowseOptions = {},
): Promise<ConceptPage> {
  if (conceptId === "") {
    throw new Error("browseConceptPage: conceptId is required");
  }
  const pageSize =
    opts.pageSize !== undefined && opts.pageSize > 0
      ? opts.pageSize
      : DEFAULT_CONCEPT_BROWSE_PAGE_SIZE;

  // sort(paginate(concept==X, N), "createdAt", "asc") -- the canonical
  // keyset-eligible directive chain (leading createdAt sort plus a paginate
  // window). The engine appends `id ASC` as the tie-breaker under equal
  // createdAt.
  const call = `sort(paginate(concept==${conceptId}, ${pageSize}), "createdAt", "asc")`;

  const result = await query.executeNamed("conceptBrowse", call, {
    ...(opts.cursor ? { cursor: opts.cursor } : {}),
    ...(opts.signal ? { signal: opts.signal } : {}),
  });

  return {
    rows: result.rawNodes(),
    nextCursor: result.meta()?.cursor ?? "",
  };
}

export async function getRowByConceptAndId(
  query: QueryClient,
  conceptId: string,
  rowId: string,
  opts: QueryCallOptions = {},
): Promise<Row | null> {
  if (conceptId === "") {
    throw new Error("getRowByConceptAndId: conceptId is required");
  }
  if (rowId === "") {
    throw new Error("getRowByConceptAndId: rowId is required");
  }
  const result = await query.executeNamed(
    "conceptRow",
    `concept==${conceptId} && id==${rowId}`,
    opts,
  );
  const nodes = result.rawNodes();
  return nodes.length > 0 ? (nodes[0] ?? null) : null;
}
```

- [ ] **Step 4: Export the new surface**

In `sdk/ts/src/client/index.ts`, after the `QueryClient` export line, add:

```typescript
export {
  browseConceptPage,
  getRowByConceptAndId,
  DEFAULT_CONCEPT_BROWSE_PAGE_SIZE,
  type ConceptPage,
  type ConceptBrowseOptions,
} from "./conceptBrowser.js";
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd sdk/ts && npm test
```

Expected: PASS — 12 new tests. If `executeNamed`'s option object rejects a `cursor` or `signal` key, read `sdk/ts/src/client/query.ts` and match its actual `QueryCallOptions` shape rather than changing the test.

- [ ] **Step 6: Typecheck**

```bash
make sdk-ts-typecheck
```

Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add sdk/ts/src/client/conceptBrowser.ts sdk/ts/src/client/index.ts \
        sdk/ts/test/conceptBrowser.test.ts
git commit -m "feat(sdk-ts): add keyset-paginated concept browsing

Mirrors sdk/go's BrowseConceptPage. The query declares sort + paginate so
it opts into a keyset window and a continuation cursor instead of hitting
the engine's implicit 50-row unmarked-list backstop with no way to page
past it (memql#2008).

Rows preserve the full nested wire shape -- a generic inspector needs the
intrinsics flattening would drop."
```

---

## Task 5: clusters.yaml reader and writer

Reads the cluster registry the cockpit owns, and writes it back without destroying comments, formatting, or fields this version does not know about. Also establishes the extension's test harness, since this is its first tested module.

**Files:**
- Create: `editors/vscode/src/clusters/model.ts`
- Create: `editors/vscode/src/clusters/file.ts`
- Create: `editors/vscode/tsconfig.test.json`
- Create: `editors/vscode/test/clustersFile.test.ts`
- Modify: `editors/vscode/package.json`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `interface ClusterConfig { name: string; displayName?: string; domain?: string; endpoint: string; issuer?: string; clientId?: string; pat?: string }`
  - `interface ClustersFile { clusters: ClusterConfig[]; selectedCluster: string }`
  - `function displayLabel(c: ClusterConfig): string`
  - `function needsAuth(c: ClusterConfig): boolean`
  - `function isOidcOnly(c: ClusterConfig): boolean`
  - `function defaultClustersPath(): string`
  - `function readClustersFile(path: string): Promise<ClustersFile>`
  - `function setSelectedCluster(path: string, name: string): Promise<void>`
  - `function upsertCluster(path: string, cluster: ClusterConfig): Promise<void>`

- [ ] **Step 1: Add the yaml dependency**

```bash
cd editors/vscode && npm install yaml@^2 --no-audit --no-fund
```

The `yaml` package is required rather than `JSON.parse` because this file is **shared with the cockpit**. A naive parse-and-rewrite would strip the operator's comments and reorder keys on every cluster selection. The `Document` API preserves both.

- [ ] **Step 2: Verify the dependency clears the extension manifest drift guards**

```bash
cd /home/znas/memql-projects/memql && go test -count=1 ./cmd/memql-lsp/...
```

Expected: PASS. These guards check that every bundled dependency's `engines.node` admits the Node major VS Code 1.91's extension host runs, and that no dependency's `engines.vscode` floor exceeds the extension's own. If `yaml` fails either, stop and report — do not raise the extension's floor to accommodate it.

- [ ] **Step 3: Write the failing test**

Create `editors/vscode/test/clustersFile.test.ts`:

```typescript
// clusters.yaml round-trip tests.
//
// The file is SHARED with the memQL Cockpit. Two properties matter more than
// anything else here: an unknown key written by a newer cockpit must survive a
// write from this extension, and the operator's comments must not be stripped.
// Both are silent data loss if they regress.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  readClustersFile,
  setSelectedCluster,
  upsertCluster,
} from "../src/clusters/file.js";
import { needsAuth } from "../src/clusters/model.js";

async function tempFile(contents: string): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const file = path.join(dir, "clusters.yaml");
  await fs.writeFile(file, contents, "utf8");
  return file;
}

const SAMPLE = `# my clusters
clusters:
  - name: local
    display_name: local.znas.io
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    pat: mql_pat_abc
  - name: staging
    domain: staging.example.com
    endpoint: cockpit.staging.example.com:443
    issuer: https://identity.staging.example.com
    client_id: cockpit
selected_cluster: local
`;

test("reads clusters and the selected cluster", async () => {
  const f = await tempFile(SAMPLE);
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.selectedCluster, "local");
  assert.equal(parsed.clusters[0]?.name, "local");
  assert.equal(parsed.clusters[0]?.displayName, "local.znas.io");
  assert.equal(parsed.clusters[0]?.endpoint, "cockpit.local.znas.io:443");
  assert.equal(parsed.clusters[0]?.pat, "mql_pat_abc");
  assert.equal(parsed.clusters[1]?.clientId, "cockpit");
});

test("returns an empty registry when the file does not exist", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const parsed = await readClustersFile(path.join(dir, "absent.yaml"));
  assert.deepEqual(parsed, { clusters: [], selectedCluster: "" });
});

test("returns an empty registry for an empty file", async () => {
  const f = await tempFile("");
  const parsed = await readClustersFile(f);
  assert.deepEqual(parsed, { clusters: [], selectedCluster: "" });
});

test("rejects a malformed file rather than silently returning nothing", async () => {
  const f = await tempFile("clusters: [unclosed\n");
  await assert.rejects(() => readClustersFile(f), /clusters\.yaml/);
});

test("setSelectedCluster updates the selection", async () => {
  const f = await tempFile(SAMPLE);
  await setSelectedCluster(f, "staging");
  assert.equal((await readClustersFile(f)).selectedCluster, "staging");
});

test("setSelectedCluster preserves comments", async () => {
  const f = await tempFile(SAMPLE);
  await setSelectedCluster(f, "staging");
  assert.match(await fs.readFile(f, "utf8"), /# my clusters/);
});

test("setSelectedCluster preserves unknown keys written by a newer cockpit", async () => {
  const f = await tempFile(
    SAMPLE.replace("    pat: mql_pat_abc\n", "    pat: mql_pat_abc\n    future_field: keep-me\n"),
  );
  await setSelectedCluster(f, "staging");
  assert.match(await fs.readFile(f, "utf8"), /future_field: keep-me/);
});

test("upsertCluster adds a new cluster", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "prod",
    endpoint: "cockpit.prod.example.com:443",
    domain: "prod.example.com",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 3);
  assert.equal(parsed.clusters[2]?.name, "prod");
});

test("upsertCluster updates an existing cluster in place", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "staging",
    endpoint: "cockpit.new.example.com:443",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.clusters[1]?.endpoint, "cockpit.new.example.com:443");
});

test("upsertCluster preserves unknown keys on the updated cluster", async () => {
  const f = await tempFile(
    SAMPLE.replace("    client_id: cockpit\n", "    client_id: cockpit\n    future_field: keep-me\n"),
  );
  await upsertCluster(f, { name: "staging", endpoint: "cockpit.new.example.com:443" });
  assert.match(await fs.readFile(f, "utf8"), /future_field: keep-me/);
});

test("writes create the file and its parent directory when absent", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const f = path.join(dir, "nested", "clusters.yaml");
  await upsertCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" });
  assert.equal((await readClustersFile(f)).clusters[0]?.name, "local");
});

test("needsAuth is false for a cluster with an endpoint and a PAT", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "h:443", pat: "mql_pat_x" }), false);
});

test("needsAuth is false for a cluster with an endpoint, issuer and client id", () => {
  assert.equal(
    needsAuth({ name: "s", endpoint: "h:443", issuer: "https://i", clientId: "cockpit" }),
    false,
  );
});

test("needsAuth is true without an endpoint", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "", pat: "mql_pat_x" }), true);
});

test("needsAuth is true with an issuer but no client id", () => {
  assert.equal(needsAuth({ name: "s", endpoint: "h:443", issuer: "https://i" }), true);
});
```

- [ ] **Step 4: Add the test harness**

Create `editors/vscode/tsconfig.test.json`:

```json
{
  "extends": "./tsconfig.json",
  "compilerOptions": {
    "rootDir": ".",
    "outDir": "dist-test",
    "sourceMap": false,
    "types": ["node"]
  },
  "include": ["src/**/*", "test/**/*"],
  "exclude": ["node_modules", ".vscode-test", "out", "dist-test"]
}
```

The test overlay compiles `src` and `test` together. Modules containing logic must not import `vscode`, so they compile and run under plain Node — that constraint is what makes this harness possible without `@vscode/test-electron`.

In `editors/vscode/package.json`, add to `"scripts"`:

```json
    "test": "tsc -p tsconfig.test.json && node --test dist-test/test/*.js"
```

Add `dist-test/` to `editors/vscode/.gitignore`.

- [ ] **Step 5: Run the test to verify it fails**

```bash
cd editors/vscode && npm test
```

Expected: FAIL — `Cannot find module '../src/clusters/file.js'`.

- [ ] **Step 6: Write the model**

Create `editors/vscode/src/clusters/model.ts`:

```typescript
// The cluster registry's data model, mirroring the cockpit's
// cli/config.ClusterConfig so the two tools read and write the same file.
//
// Field names here are camelCase; the YAML uses snake_case. file.ts owns the
// mapping. Keeping the boundary in one place means the rest of the extension
// never sees a wire spelling.

export interface ClusterConfig {
  // name is the slot key used for lookups (e.g. "local", "staging").
  name: string;
  // displayName is the human-friendly label; falls back to name when empty.
  displayName?: string;
  // domain is the single value the add/edit flow collects (e.g.
  // "staging.example.com"). endpoint / issuer / clientId are composed from it
  // by convention. Stored so an edit can round-trip the domain instead of
  // reverse-engineering it from the endpoint.
  domain?: string;
  // endpoint is the gRPC address (host:port).
  endpoint: string;
  issuer?: string;
  clientId?: string;
  // pat is an optional Personal Access Token (mql_pat_<...>) sent as
  // `Authorization: Bearer <pat>`. When set it short-circuits the OIDC flow --
  // the token IS the credential.
  pat?: string;
}

export interface ClustersFile {
  clusters: ClusterConfig[];
  selectedCluster: string;
}

// displayLabel returns the label any UI surface should show.
export function displayLabel(c: ClusterConfig): string {
  return c.displayName && c.displayName !== "" ? c.displayName : c.name;
}

// needsAuth reports whether a cluster lacks enough credentials to dial.
// Configured means an endpoint AND either a PAT or an issuer/clientId pair.
// An empty endpoint counts as not-configured: even with auth fields set there
// is nowhere to dial.
export function needsAuth(c: ClusterConfig): boolean {
  if (c.endpoint === "") return true;
  if (c.pat !== undefined && c.pat !== "") return false;
  return (
    c.issuer === undefined || c.issuer === "" ||
    c.clientId === undefined || c.clientId === ""
  );
}

// isOidcOnly reports a cluster this extension cannot authenticate itself:
// OIDC is configured but no PAT is present. B1 supports PAT auth only, so
// these clusters must be authenticated in the cockpit first.
export function isOidcOnly(c: ClusterConfig): boolean {
  const noPat = c.pat === undefined || c.pat === "";
  const hasOidc =
    c.issuer !== undefined && c.issuer !== "" &&
    c.clientId !== undefined && c.clientId !== "";
  return noPat && hasOidc;
}
```

- [ ] **Step 7: Write the file module**

Create `editors/vscode/src/clusters/file.ts`:

```typescript
// Reading and writing ~/.memql/clusters.yaml.
//
// This file is SHARED with the memQL Cockpit, which is the reason every write
// goes through the yaml Document API rather than a parse-and-serialize round
// trip. A naive rewrite would strip the operator's comments and drop any key a
// newer cockpit writes that this version does not model -- silent data loss on
// something as routine as selecting a cluster.
//
// Writes are read-modify-write against the file as it is on disk at write
// time, never against a cached parse, so a concurrent cockpit edit is merged
// rather than clobbered.

import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { Document, parseDocument, type YAMLMap, type YAMLSeq } from "yaml";

import type { ClusterConfig, ClustersFile } from "./model.js";

export function defaultClustersPath(): string {
  return path.join(os.homedir(), ".memql", "clusters.yaml");
}

// Wire spellings. The YAML uses snake_case; the model uses camelCase.
const FIELD_MAP: ReadonlyArray<readonly [keyof ClusterConfig, string]> = [
  ["name", "name"],
  ["displayName", "display_name"],
  ["domain", "domain"],
  ["endpoint", "endpoint"],
  ["issuer", "issuer"],
  ["clientId", "client_id"],
  ["pat", "pat"],
];

async function loadDocument(file: string): Promise<Document> {
  let raw: string;
  try {
    raw = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return new Document({ clusters: [] });
    }
    throw err;
  }
  if (raw.trim() === "") {
    return new Document({ clusters: [] });
  }
  const doc = parseDocument(raw);
  if (doc.errors.length > 0) {
    throw new Error(
      `clusters.yaml at ${file} is malformed: ${doc.errors[0]?.message ?? "parse error"}`,
    );
  }
  return doc;
}

function stringAt(map: YAMLMap, key: string): string | undefined {
  const v = map.get(key, false);
  return typeof v === "string" ? v : undefined;
}

function clusterFromNode(node: unknown): ClusterConfig | null {
  const map = node as YAMLMap;
  if (typeof map?.get !== "function") return null;
  const name = stringAt(map, "name");
  if (name === undefined) return null;
  const out: ClusterConfig = { name, endpoint: stringAt(map, "endpoint") ?? "" };
  for (const [modelKey, wireKey] of FIELD_MAP) {
    if (modelKey === "name" || modelKey === "endpoint") continue;
    const v = stringAt(map, wireKey);
    if (v !== undefined) {
      (out as Record<string, unknown>)[modelKey] = v;
    }
  }
  return out;
}

export async function readClustersFile(file: string): Promise<ClustersFile> {
  const doc = await loadDocument(file);
  const seq = doc.get("clusters", true) as YAMLSeq | undefined;
  const items = Array.isArray(seq?.items) ? seq.items : [];
  const clusters: ClusterConfig[] = [];
  for (const item of items) {
    const c = clusterFromNode(item);
    if (c !== null) clusters.push(c);
  }
  const selected = doc.get("selected_cluster", false);
  return {
    clusters,
    selectedCluster: typeof selected === "string" ? selected : "",
  };
}

async function saveDocument(file: string, doc: Document): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.writeFile(file, doc.toString(), "utf8");
}

export async function setSelectedCluster(file: string, name: string): Promise<void> {
  const doc = await loadDocument(file);
  doc.set("selected_cluster", name);
  await saveDocument(file, doc);
}

export async function upsertCluster(file: string, cluster: ClusterConfig): Promise<void> {
  const doc = await loadDocument(file);
  let seq = doc.get("clusters", true) as YAMLSeq | undefined;
  if (seq === undefined || !Array.isArray(seq.items)) {
    doc.set("clusters", []);
    seq = doc.get("clusters", true) as YAMLSeq;
  }

  const existing = seq.items.find((item) => {
    const map = item as YAMLMap;
    return typeof map?.get === "function" && map.get("name", false) === cluster.name;
  }) as YAMLMap | undefined;

  if (existing !== undefined) {
    // Set only the fields we were given. Every other key on the node -- including
    // one a newer cockpit wrote and this version does not model -- is left alone.
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      if (v !== undefined) existing.set(wireKey, v);
    }
  } else {
    const fresh: Record<string, string> = {};
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      if (v !== undefined) fresh[wireKey] = v;
    }
    seq.add(fresh);
  }

  await saveDocument(file, doc);
}
```

- [ ] **Step 8: Run the test to verify it passes**

```bash
cd editors/vscode && npm test
```

Expected: PASS — 15 tests.

- [ ] **Step 9: Wire the extension test into make and CI**

In `Makefile`, after the `viewkit-test` target added in Task 1, add:

```makefile
## Run the VS Code extension's unit tests. Covers only modules that do not
## import `vscode`; the API layer is exercised by packaging, not unit tests.
vscode-test:
	cd editors/vscode && npm ci --no-audit --no-fund && npm test
```

Add `vscode-test` to the same `.PHONY` line.

In `.github/workflows/ci.yml`, in the `vscode-extension` job, insert this step immediately BEFORE the `package extension` step:

```yaml
      - name: extension unit tests
        run: make vscode-test
```

- [ ] **Step 10: Verify the make target and workflow**

```bash
cd /home/znas/memql-projects/memql && make vscode-test
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('SUCCESS: workflow parses')"
```

Expected: tests pass; workflow parses.

- [ ] **Step 11: Commit**

```bash
git add editors/vscode/src/clusters/model.ts editors/vscode/src/clusters/file.ts \
        editors/vscode/tsconfig.test.json editors/vscode/test/clustersFile.test.ts \
        editors/vscode/package.json editors/vscode/package-lock.json \
        editors/vscode/.gitignore Makefile .github/workflows/ci.yml
git commit -m "feat(vscode): read and write the shared clusters.yaml registry

The file is shared with the memQL Cockpit, so every write goes through the
yaml Document API rather than a parse-and-serialize round trip: a naive
rewrite would strip operator comments and drop any key a newer cockpit
writes that this version does not model.

Writes are read-modify-write against the file on disk at write time, so a
concurrent cockpit edit merges instead of being clobbered.

Adds the extension's first unit-test harness (node:test, no
@vscode/test-electron) and runs it in the vscode-extension CI lane."
```

---

## Task 6: Endpoint derivation and connection manager

Turns a cluster config into a dialable WebSocket URL and owns the single live connection.

**Files:**
- Create: `editors/vscode/src/connection/endpoint.ts`
- Create: `editors/vscode/src/connection/manager.ts`
- Create: `editors/vscode/test/endpoint.test.ts`
- Modify: `editors/vscode/package.json`

**Interfaces:**
- Consumes: `ClusterConfig`, `needsAuth`, `isOidcOnly` from Task 5.
- Produces:
  - `function webSocketUrlFor(cluster: ClusterConfig): string`
  - `type ConnectionState = { status: "disconnected" } | { status: "connecting"; clusterName: string } | { status: "connected"; clusterName: string; nodeId: string } | { status: "error"; clusterName: string; message: string }`
  - `class ConnectionManager` with `connect(cluster)`, `disconnect()`, `get state()`, `get query()`, `get subscriptions()`, `onDidChangeState(listener)`

- [ ] **Step 1: Add the SDK dependency**

The extension consumes the SDK from the workspace. In `editors/vscode/package.json`, add to `"dependencies"`:

```json
    "@znasllc-io/memql-sdk-core": "file:../../sdk/ts",
    "@znasllc-io/memql-view-kit": "file:../../sdk/ts-viewkit"
```

Then:

```bash
cd sdk/ts && npm install --no-audit --no-fund && npm run build
cd ../ts-viewkit && npm install --no-audit --no-fund && npm run build
cd ../../editors/vscode && npm install --no-audit --no-fund
```

Both packages must be built before the extension installs them — `file:` dependencies resolve `main`/`types` into `dist/`.

Note: `sdk/ts` has no committed `package-lock.json`, which is why every target that installs it uses `npm install` rather than `npm ci`. Do not commit that lockfile as part of this task; it is a pre-existing repo-wide decision, out of scope for B1.

- [ ] **Step 2: Re-run the extension manifest drift guards**

```bash
cd /home/znas/memql-projects/memql && go test -count=1 ./cmd/memql-lsp/...
```

Expected: PASS. Both new dependencies declare `engines.node: >=20`, which admits the VS Code 1.91 extension host.

- [ ] **Step 3: Write the failing test**

Create `editors/vscode/test/endpoint.test.ts`:

```typescript
// Endpoint derivation tests.
//
// clusters.yaml stores a gRPC address (host:port) because the cockpit dials
// native gRPC. This extension speaks the /memql/ws bridge, so the address must
// be lifted to a wss:// URL. Getting this wrong is the difference between
// "connects" and "hangs until the handshake times out".

import test from "node:test";
import assert from "node:assert/strict";

import { webSocketUrlFor } from "../src/connection/endpoint.js";

test("derives a wss URL from a host:port endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io:443" }),
    "wss://cockpit.local.znas.io/memql/ws",
  );
});

test("preserves a non-standard port", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io:8443" }),
    "wss://cockpit.local.znas.io:8443/memql/ws",
  );
});

test("handles an endpoint with no port", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io" }),
    "wss://cockpit.local.znas.io/memql/ws",
  );
});

test("uses ws for a plain-http localhost endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "localhost:50051" }),
    "ws://localhost:50051/memql/ws",
  );
});

test("uses ws for a 127.0.0.1 endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "127.0.0.1:50051" }),
    "ws://127.0.0.1:50051/memql/ws",
  );
});

test("passes an explicit wss URL through unchanged", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "wss://cockpit.example.com/memql/ws" }),
    "wss://cockpit.example.com/memql/ws",
  );
});

test("appends the bridge path to an explicit scheme without one", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "wss://cockpit.example.com" }),
    "wss://cockpit.example.com/memql/ws",
  );
});

test("throws on an empty endpoint rather than dialing nowhere", () => {
  assert.throws(() => webSocketUrlFor({ name: "l", endpoint: "" }), /endpoint is empty/);
});
```

- [ ] **Step 4: Run the test to verify it fails**

```bash
cd editors/vscode && npm test
```

Expected: FAIL — `Cannot find module '../src/connection/endpoint.js'`.

- [ ] **Step 5: Write the endpoint module**

Create `editors/vscode/src/connection/endpoint.ts`:

```typescript
// Deriving a WebSocket URL from a cluster's stored gRPC endpoint.
//
// clusters.yaml records a gRPC address (host:port) because the cockpit dials
// native gRPC. This extension speaks the /memql/ws bridge instead, so the
// address is lifted to a URL here -- one place, so the rest of the extension
// never reasons about transport.

import type { ClusterConfig } from "../clusters/model.js";

const BRIDGE_PATH = "/memql/ws";

// Loopback hosts are served over plain HTTP by a raw port-forward, which is a
// debugging path rather than the front door. Everything else is TLS.
function isLoopback(host: string): boolean {
  return host === "localhost" || host === "127.0.0.1" || host === "::1";
}

export function webSocketUrlFor(cluster: ClusterConfig): string {
  const raw = cluster.endpoint.trim();
  if (raw === "") {
    throw new Error(`cluster "${cluster.name}": endpoint is empty`);
  }

  // An operator may store a full URL. Honor it, adding the bridge path when
  // it carries none.
  if (raw.startsWith("wss://") || raw.startsWith("ws://")) {
    const url = new URL(raw);
    if (url.pathname === "" || url.pathname === "/") {
      url.pathname = BRIDGE_PATH;
    }
    return url.toString();
  }

  const lastColon = raw.lastIndexOf(":");
  const hasPort = lastColon > 0 && /^\d+$/.test(raw.slice(lastColon + 1));
  const host = hasPort ? raw.slice(0, lastColon) : raw;
  const port = hasPort ? raw.slice(lastColon + 1) : "";

  const scheme = isLoopback(host) ? "ws" : "wss";
  // 443 is the front door's implicit port; emitting it produces a URL that
  // works but reads as a misconfiguration in logs and error messages.
  const authority = port === "" || port === "443" ? host : `${host}:${port}`;
  return `${scheme}://${authority}${BRIDGE_PATH}`;
}
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
cd editors/vscode && npm test
```

Expected: PASS — 8 new tests.

- [ ] **Step 7: Write the connection manager**

Create `editors/vscode/src/connection/manager.ts`:

```typescript
// The extension's single live cluster connection.
//
// Exactly one connection exists at a time: the working cluster's. Selecting a
// different cluster tears the old one down first. This mirrors the cockpit's
// single-connection invariant -- concurrent connections to different clusters
// make "which cluster did that row come from" unanswerable.
//
// Deliberately free of `vscode` imports so it is unit-testable. State changes
// are published through a plain listener set; the views adapt it to VS Code's
// event model.

import { Connection } from "@znasllc-io/memql-sdk-core/client";
import type { QueryClient, SubscriptionManager } from "@znasllc-io/memql-sdk-core/client";

import type { ClusterConfig } from "../clusters/model.js";
import { isOidcOnly, needsAuth } from "../clusters/model.js";
import { webSocketUrlFor } from "./endpoint.js";

export type ConnectionState =
  | { status: "disconnected" }
  | { status: "connecting"; clusterName: string }
  | { status: "connected"; clusterName: string; nodeId: string }
  | { status: "error"; clusterName: string; message: string };

export type StateListener = (state: ConnectionState) => void;

export class ConnectionManager {
  private conn: Connection | undefined;
  private current: ConnectionState = { status: "disconnected" };
  private readonly listeners = new Set<StateListener>();
  // Guards against an out-of-order settle: if the user selects cluster A then
  // cluster B before A's handshake finishes, A's completion must not overwrite
  // B's state.
  private generation = 0;

  get state(): ConnectionState {
    return this.current;
  }

  get query(): QueryClient | undefined {
    return this.conn?.query;
  }

  get subscriptions(): SubscriptionManager | undefined {
    return this.conn?.subscriptions;
  }

  onDidChangeState(listener: StateListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  private publish(state: ConnectionState): void {
    this.current = state;
    for (const l of this.listeners) l(state);
  }

  async connect(cluster: ClusterConfig): Promise<void> {
    const gen = ++this.generation;
    await this.closeCurrent();

    if (needsAuth(cluster)) {
      const message = isOidcOnly(cluster)
        ? "This cluster is configured for OIDC. Authenticate it in the memQL Cockpit first, or add a PAT to clusters.yaml."
        : "This cluster is not configured. Set an endpoint and a PAT.";
      this.publish({ status: "error", clusterName: cluster.name, message });
      return;
    }

    this.publish({ status: "connecting", clusterName: cluster.name });
    try {
      const conn = await Connection.dial({
        endpoint: webSocketUrlFor(cluster),
        auth: { bearer: cluster.pat ?? "" },
        clientId: "memql-vscode",
        sdkName: "memql-vscode",
      });
      if (gen !== this.generation) {
        // Superseded while dialing; drop this connection on the floor.
        conn.close();
        return;
      }
      this.conn = conn;
      this.publish({ status: "connected", clusterName: cluster.name, nodeId: conn.nodeId });
    } catch (err) {
      if (gen !== this.generation) return;
      this.publish({
        status: "error",
        clusterName: cluster.name,
        message: err instanceof Error ? err.message : String(err),
      });
    }
  }

  async disconnect(): Promise<void> {
    this.generation++;
    await this.closeCurrent();
    this.publish({ status: "disconnected" });
  }

  private async closeCurrent(): Promise<void> {
    if (this.conn === undefined) return;
    try {
      this.conn.close();
    } catch {
      // A close on an already-dead socket is not worth surfacing.
    }
    this.conn = undefined;
  }
}
```

- [ ] **Step 8: Typecheck and test**

```bash
cd editors/vscode && npm run compile && npm test
```

Expected: compile clean, all tests pass. If `Connection.close()` is not public in the SDK, read `sdk/ts/src/client/connection.ts` and use the actual teardown method.

- [ ] **Step 9: Make the workspace dependencies buildable from a clean checkout**

The `file:` dependencies resolve `main` and `types` into each package's `dist/`, which does not exist in a fresh clone. Both `make vscode-test` and the CI lane's `npm ci` would fail on a clean checkout unless the dependencies are built first.

In `Makefile`, replace the `vscode-test` target added in Task 5 with:

```makefile
## Build the workspace packages the extension depends on via `file:`. Their
## dist/ must exist before `npm ci` in editors/vscode can resolve them, so a
## clean checkout needs this first.
##
## sdk/ts uses `npm install`, not `npm ci`: its package-lock.json is not
## committed, and `npm ci` fails without one. This matches the existing
## sdk-ts-install target. sdk/ts-viewkit does commit its lockfile, so it gets
## the reproducible `npm ci`.
vscode-deps:
	cd sdk/ts && npm install --no-audit --no-fund && npm run build
	cd sdk/ts-viewkit && npm ci --no-audit --no-fund && npm run build

## Run the VS Code extension's unit tests. Covers only modules that do not
## import `vscode`; the API layer is exercised by packaging, not unit tests.
vscode-test: vscode-deps
	cd editors/vscode && npm ci --no-audit --no-fund && npm test
```

Add `vscode-deps` to the same `.PHONY` line.

In `.github/workflows/ci.yml`, in the `vscode-extension` job, insert this step immediately BEFORE the `extension unit tests` step added in Task 5:

```yaml
      - name: build workspace dependencies (sdk/ts, sdk/ts-viewkit)
        run: scripts/ci/retry.sh -- make vscode-deps
```

`scripts/vscode/package.sh` runs its own `npm ci` inside `editors/vscode`, so with `vscode-deps` already built earlier in the lane the packaging step resolves both dependencies.

- [ ] **Step 10: Verify from a clean dependency state**

```bash
cd /home/znas/memql-projects/memql
rm -rf editors/vscode/node_modules sdk/ts/dist sdk/ts-viewkit/dist
make vscode-test
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('SUCCESS: workflow parses')"
```

Expected: the target rebuilds both dependencies and the extension tests pass; the workflow parses. This is the exact sequence CI runs from a fresh checkout.

- [ ] **Step 11: Commit**

```bash
git add editors/vscode/src/connection/endpoint.ts editors/vscode/src/connection/manager.ts \
        editors/vscode/test/endpoint.test.ts editors/vscode/package.json \
        editors/vscode/package-lock.json Makefile .github/workflows/ci.yml
git commit -m "feat(vscode): derive the bridge URL and own a single cluster connection

clusters.yaml stores a gRPC address because the cockpit dials native gRPC;
this extension speaks /memql/ws, so the address is lifted to a URL in one
place.

Exactly one connection exists at a time, mirroring the cockpit's
single-connection invariant. A generation counter stops a slow handshake
for a previously selected cluster from overwriting the current one."
```

---

## Task 7: Activity bar, Clusters view, and trust posture

The extension becomes visible: an activity-bar container, a Clusters tree, commands to select, add and edit, and the workspace-trust change that executing any of this requires.

**Files:**
- Create: `editors/vscode/icons/memql-activity.svg`
- Create: `editors/vscode/src/views/clustersTree.ts`
- Modify: `editors/vscode/src/extension.ts`
- Modify: `editors/vscode/package.json`

**Interfaces:**
- Consumes: `readClustersFile`, `setSelectedCluster`, `upsertCluster`, `defaultClustersPath` (Task 5); `displayLabel`, `needsAuth`, `isOidcOnly`, `ClusterConfig` (Task 5); `ConnectionManager` (Task 6).
- Produces:
  - `class ClustersTreeProvider implements vscode.TreeDataProvider<ClusterNode>` with `refresh()`
  - `interface ClusterNode { cluster: ClusterConfig; selected: boolean }`
  - Commands: `memql.clusters.refresh`, `memql.clusters.select`, `memql.clusters.add`, `memql.clusters.edit`, `memql.clusters.disconnect`
  - Exported from `extension.ts`: `getConnectionManager(): ConnectionManager`

- [ ] **Step 1: Create the activity-bar icon**

VS Code masks an activity-bar icon and repaints it with the theme foreground color, so the supplied color is discarded. The constraint is legibility at 24x24 under that mask — which is why this uses the simplified four-node glyph from `assets/logo.svg`, not the dense twelve-node mark in `icons/memql.png`.

Create `editors/vscode/icons/memql-activity.svg`:

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24">
  <g fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round">
    <line x1="4" y1="14" x2="11" y2="6"/>
    <line x1="11" y1="6" x2="20" y2="15"/>
    <line x1="4" y1="14" x2="11" y2="20"/>
    <line x1="11" y1="20" x2="20" y2="15"/>
    <line x1="11" y1="6" x2="11" y2="20"/>
  </g>
  <g fill="currentColor">
    <circle cx="4" cy="14" r="2.4"/>
    <circle cx="11" cy="6" r="2.4"/>
    <circle cx="11" cy="20" r="2.4"/>
    <circle cx="20" cy="15" r="2.4"/>
  </g>
</svg>
```

- [ ] **Step 2: Declare the contributions and change the trust posture**

In `editors/vscode/package.json`:

Replace the `capabilities` block:

```json
  "capabilities": {
    "untrustedWorkspaces": {
      "supported": "limited",
      "description": "Language features (highlighting, diagnostics, completion, hover, signature help) work in untrusted workspaces. Connecting to a cluster and browsing its data require trust: those read credentials from ~/.memql/clusters.yaml and open a network connection, which a malicious workspace must not be able to trigger.",
      "restrictedConfigurations": ["memql.lsp.serverPath"]
    }
  },
```

Add `"onView:memqlClusters"` to `activationEvents`.

Add to `contributes`:

```json
    "viewsContainers": {
      "activitybar": [
        {
          "id": "memql",
          "title": "memQL",
          "icon": "icons/memql-activity.svg"
        }
      ]
    },
    "views": {
      "memql": [
        {
          "id": "memqlClusters",
          "name": "Clusters"
        }
      ]
    },
    "commands": [
      { "command": "memql.clusters.refresh", "title": "memQL: Refresh Clusters", "icon": "$(refresh)" },
      { "command": "memql.clusters.select", "title": "memQL: Select Cluster" },
      { "command": "memql.clusters.add", "title": "memQL: Add Cluster", "icon": "$(add)" },
      { "command": "memql.clusters.edit", "title": "memQL: Edit Cluster", "icon": "$(edit)" },
      { "command": "memql.clusters.disconnect", "title": "memQL: Disconnect" }
    ],
    "menus": {
      "view/title": [
        { "command": "memql.clusters.add", "when": "view == memqlClusters", "group": "navigation@1" },
        { "command": "memql.clusters.refresh", "when": "view == memqlClusters", "group": "navigation@2" }
      ],
      "view/item/context": [
        { "command": "memql.clusters.edit", "when": "view == memqlClusters && viewItem == memqlCluster", "group": "inline" }
      ]
    },
```

- [ ] **Step 3: Write the clusters tree provider**

Create `editors/vscode/src/views/clustersTree.ts`:

```typescript
// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.

import * as vscode from "vscode";

import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel, isOidcOnly, needsAuth } from "../clusters/model.js";
import { readClustersFile } from "../clusters/file.js";
import type { ConnectionManager, ConnectionState } from "../connection/manager.js";

export interface ClusterNode {
  cluster: ClusterConfig;
  selected: boolean;
}

export class ClustersTreeProvider implements vscode.TreeDataProvider<ClusterNode> {
  private readonly changed = new vscode.EventEmitter<ClusterNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  constructor(
    private readonly clustersPath: string,
    private readonly connections: ConnectionManager,
  ) {
    this.connections.onDidChangeState(() => this.changed.fire(undefined));
  }

  refresh(): void {
    this.changed.fire(undefined);
  }

  async getChildren(element?: ClusterNode): Promise<ClusterNode[]> {
    // Flat list: clusters have no children.
    if (element !== undefined) return [];
    const file = await readClustersFile(this.clustersPath);
    return file.clusters.map((cluster) => ({
      cluster,
      selected: cluster.name === file.selectedCluster,
    }));
  }

  getTreeItem(node: ClusterNode): vscode.TreeItem {
    const item = new vscode.TreeItem(
      displayLabel(node.cluster),
      vscode.TreeItemCollapsibleState.None,
    );
    item.contextValue = "memqlCluster";
    item.description = node.cluster.endpoint;
    item.command = {
      command: "memql.clusters.select",
      title: "Select Cluster",
      arguments: [node],
    };
    item.iconPath = this.iconFor(node);
    item.tooltip = this.tooltipFor(node);
    return item;
  }

  private iconFor(node: ClusterNode): vscode.ThemeIcon {
    const state = this.connections.state;
    const isActive =
      state.status !== "disconnected" && state.clusterName === node.cluster.name;

    if (isActive && state.status === "connected") {
      return new vscode.ThemeIcon(
        "circle-filled",
        new vscode.ThemeColor("charts.green"),
      );
    }
    if (isActive && state.status === "connecting") {
      return new vscode.ThemeIcon("loading~spin");
    }
    if (isActive && state.status === "error") {
      return new vscode.ThemeIcon(
        "error",
        new vscode.ThemeColor("charts.red"),
      );
    }
    if (needsAuth(node.cluster)) {
      return new vscode.ThemeIcon(
        "warning",
        new vscode.ThemeColor("charts.yellow"),
      );
    }
    return new vscode.ThemeIcon("circle-outline");
  }

  private tooltipFor(node: ClusterNode): string {
    const state = this.connections.state;
    if (state.status !== "disconnected" && state.clusterName === node.cluster.name) {
      if (state.status === "connected") return `Connected (node ${state.nodeId})`;
      if (state.status === "connecting") return "Connecting...";
      if (state.status === "error") return `ERROR: ${state.message}`;
    }
    if (isOidcOnly(node.cluster)) {
      return "Configured for OIDC. Authenticate in the memQL Cockpit, or add a PAT.";
    }
    if (needsAuth(node.cluster)) {
      return "Not configured. Set an endpoint and a PAT.";
    }
    return node.cluster.endpoint;
  }
}
```

- [ ] **Step 4: Wire it into the extension**

In `editors/vscode/src/extension.ts`, add these imports at the top:

```typescript
import { defaultClustersPath, readClustersFile, setSelectedCluster, upsertCluster } from './clusters/file.js';
import type { ClusterConfig } from './clusters/model.js';
import { ConnectionManager } from './connection/manager.js';
import { ClustersTreeProvider, type ClusterNode } from './views/clustersTree.js';
```

Add a module-level manager alongside the existing `client` declaration:

```typescript
let connections: ConnectionManager | undefined;

// getConnectionManager exposes the live manager to later-registered views.
export function getConnectionManager(): ConnectionManager {
  if (connections === undefined) {
    throw new Error('memQL: connection manager accessed before activation');
  }
  return connections;
}
```

At the end of `activate()`, after the LanguageClient start block, add:

```typescript
  // The runtime surface reads credentials from the home directory and opens a
  // network connection, so it is gated on workspace trust. Language features
  // above are not.
  if (!workspace.isTrusted) {
    return;
  }
  registerRuntimeSurface(context);
}

function registerRuntimeSurface(context: ExtensionContext): void {
  const clustersPath = defaultClustersPath();
  connections = new ConnectionManager();

  const clustersTree = new ClustersTreeProvider(clustersPath, connections);
  context.subscriptions.push(
    window.registerTreeDataProvider('memqlClusters', clustersTree)
  );

  // The cockpit writes this file too; watch it so the tree stays truthful.
  //
  // MUST be a RelativePattern with a base Uri, never the bare path string.
  // Given a plain `string` glob, createFileSystemWatcher only watches paths
  // INSIDE the workspace folders, and ~/.memql/clusters.yaml is outside any
  // workspace -- the string form never fires and all three handlers below
  // become dead code. (This plan originally prescribed the string form; it
  // was implemented faithfully and caught in final review. Do not reintroduce
  // it in B2.)
  const watcher = workspace.createFileSystemWatcher(
    new RelativePattern(Uri.file(path.dirname(clustersPath)), path.basename(clustersPath))
  );
  watcher.onDidChange(() => clustersTree.refresh());
  watcher.onDidCreate(() => clustersTree.refresh());
  watcher.onDidDelete(() => clustersTree.refresh());
  context.subscriptions.push(watcher);

  context.subscriptions.push(
    commands.registerCommand('memql.clusters.refresh', () => clustersTree.refresh()),
    commands.registerCommand('memql.clusters.disconnect', async () => {
      await connections?.disconnect();
      clustersTree.refresh();
    }),
    commands.registerCommand('memql.clusters.select', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      await setSelectedCluster(clustersPath, target.cluster.name);
      clustersTree.refresh();
      await connections?.connect(target.cluster);
      const state = connections?.state;
      if (state?.status === 'error') {
        window.showErrorMessage(`memQL: ${state.message}`);
      }
    }),
    commands.registerCommand('memql.clusters.add', async () => {
      const created = await promptForCluster();
      if (created === undefined) {
        return;
      }
      await upsertCluster(clustersPath, created);
      clustersTree.refresh();
    }),
    commands.registerCommand('memql.clusters.edit', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      const edited = await promptForCluster(target.cluster);
      if (edited === undefined) {
        return;
      }
      await upsertCluster(clustersPath, edited);
      clustersTree.refresh();
    })
  );
}

async function pickCluster(clustersPath: string): Promise<ClusterNode | undefined> {
  const file = await readClustersFile(clustersPath);
  const picked = await window.showQuickPick(
    file.clusters.map((cluster) => ({
      label: cluster.name,
      description: cluster.endpoint,
      cluster,
    })),
    { placeHolder: 'Select a memQL cluster' }
  );
  if (picked === undefined) {
    return undefined;
  }
  return { cluster: picked.cluster, selected: picked.cluster.name === file.selectedCluster };
}

// promptForCluster collects a cluster with native inputs rather than a webview:
// it is four fields, and a QuickInput sequence is both less code and more
// idiomatic than a custom form.
async function promptForCluster(existing?: ClusterConfig): Promise<ClusterConfig | undefined> {
  const name = await window.showInputBox({
    prompt: 'Cluster name (the key used in clusters.yaml)',
    value: existing?.name ?? '',
    ignoreFocusOut: true,
    validateInput: (v) => (v.trim() === '' ? 'A name is required' : undefined),
  });
  if (name === undefined) {
    return undefined;
  }

  const domain = await window.showInputBox({
    prompt: 'Domain (e.g. local.znas.io). The endpoint is composed as cockpit.<domain>:443.',
    value: existing?.domain ?? '',
    ignoreFocusOut: true,
  });
  if (domain === undefined) {
    return undefined;
  }

  const endpoint = await window.showInputBox({
    prompt: 'gRPC endpoint (host:port)',
    value: existing?.endpoint ?? (domain.trim() === '' ? '' : `cockpit.${domain.trim()}:443`),
    ignoreFocusOut: true,
    validateInput: (v) => (v.trim() === '' ? 'An endpoint is required' : undefined),
  });
  if (endpoint === undefined) {
    return undefined;
  }

  const pat = await window.showInputBox({
    prompt: 'Personal Access Token (mql_pat_...). Leave empty to authenticate in the memQL Cockpit instead.',
    value: existing?.pat ?? '',
    ignoreFocusOut: true,
    password: true,
  });
  if (pat === undefined) {
    return undefined;
  }

  const out: ClusterConfig = { name: name.trim(), endpoint: endpoint.trim() };
  if (domain.trim() !== '') {
    out.domain = domain.trim();
  }
  if (pat.trim() !== '') {
    out.pat = pat.trim();
  }
  return out;
}
```

Extend the existing `import { ExtensionContext, window, workspace } from 'vscode';` line to also import `commands`.

- [ ] **Step 5: Compile and test**

```bash
cd editors/vscode && npm run compile && npm test
```

Expected: compile clean; the 23 existing tests still pass.

- [ ] **Step 6: Verify the manifest guards accept the new contributions**

```bash
cd /home/znas/memql-projects/memql && go test -count=1 ./cmd/memql-lsp/...
```

Expected: PASS.

- [ ] **Step 7: Verify the extension packages**

```bash
bash scripts/vscode/package.sh --out=/tmp/claude-1000/-home-znas-memql-projects-memql/72dc4c20-4d55-4a3c-a02e-eff35982ed0f/scratchpad/memql.vsix
```

Expected: `SUCCESS: VSIX packaged`.

- [ ] **Step 8: Commit**

```bash
git add editors/vscode/icons/memql-activity.svg editors/vscode/src/views/clustersTree.ts \
        editors/vscode/src/extension.ts editors/vscode/package.json
git commit -m "feat(vscode): add the activity bar container and Clusters view

Lists clusters from the shared registry with live connection state, and
selects, adds and edits them. Add/edit uses native QuickInput rather than a
webview -- four fields does not justify a custom form.

The activity-bar icon is a 24x24 single-fill SVG derived from the
simplified four-node glyph: VS Code masks this slot and repaints it with
the theme color, so the dense marketplace PNG would render as mush.

Workspace trust drops from supported to limited. The runtime surface reads
credentials and opens a network connection, so a malicious workspace must
not be able to trigger it; language features stay available untrusted."
```

---

## Task 8: Concepts tree

Lists every registered concept, grouped by domain, for the connected cluster.

**Files:**
- Create: `editors/vscode/src/views/conceptsTree.ts`
- Modify: `editors/vscode/src/extension.ts`
- Modify: `editors/vscode/package.json`

**Interfaces:**
- Consumes: `ConnectionManager` (Task 6); `Concept` from `@znasllc-io/memql-sdk-core/client` (existing SDK type).
- Produces:
  - `class ConceptsTreeProvider implements vscode.TreeDataProvider<ConceptTreeNode>` with `refresh()`
  - `type ConceptTreeNode = { kind: "domain"; domain: string } | { kind: "concept"; concept: Concept }`
  - Command: `memql.concepts.refresh`

- [ ] **Step 1: Declare the view and command**

In `editors/vscode/package.json`, add to `contributes.views.memql` (after the Clusters entry):

```json
        {
          "id": "memqlConcepts",
          "name": "Concepts"
        }
```

Add to `contributes.commands`:

```json
      { "command": "memql.concepts.refresh", "title": "memQL: Refresh Concepts", "icon": "$(refresh)" }
```

Add to `contributes.menus["view/title"]`:

```json
        { "command": "memql.concepts.refresh", "when": "view == memqlConcepts", "group": "navigation@1" }
```

- [ ] **Step 2: Write the concepts tree provider**

Create `editors/vscode/src/views/conceptsTree.ts`:

```typescript
// The Concepts tree: every registered concept on the connected cluster,
// grouped by domain.
//
// The list comes from ConceptsListMsg via the SDK's listConcepts, which the
// engine answers from its own registry -- so a concept added to the DSL shows
// up here with no client change. That is the whole point of a generic browser.

import * as vscode from "vscode";

import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionManager } from "../connection/manager.js";

export type ConceptTreeNode =
  | { kind: "domain"; domain: string }
  | { kind: "concept"; concept: Concept };

export class ConceptsTreeProvider implements vscode.TreeDataProvider<ConceptTreeNode> {
  private readonly changed = new vscode.EventEmitter<ConceptTreeNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  // Cached so expanding a domain does not re-issue the list. Cleared whenever
  // the connection changes, since concepts are per-cluster.
  private cache: Concept[] | undefined;

  constructor(private readonly connections: ConnectionManager) {
    this.connections.onDidChangeState(() => {
      this.cache = undefined;
      this.changed.fire(undefined);
    });
  }

  refresh(): void {
    this.cache = undefined;
    this.changed.fire(undefined);
  }

  private async load(): Promise<Concept[]> {
    if (this.cache !== undefined) return this.cache;
    const query = this.connections.query;
    if (query === undefined) return [];
    const concepts = await query.listConcepts();
    this.cache = concepts;
    return concepts;
  }

  async getChildren(element?: ConceptTreeNode): Promise<ConceptTreeNode[]> {
    const concepts = await this.load();

    if (element === undefined) {
      const domains = [...new Set(concepts.map((c) => c.domain))].sort();
      return domains.map((domain) => ({ kind: "domain", domain }));
    }

    if (element.kind === "domain") {
      return concepts
        .filter((c) => c.domain === element.domain)
        .sort((a, b) => a.entity.localeCompare(b.entity))
        .map((concept) => ({ kind: "concept", concept }));
    }

    return [];
  }

  getTreeItem(node: ConceptTreeNode): vscode.TreeItem {
    if (node.kind === "domain") {
      const item = new vscode.TreeItem(
        node.domain,
        vscode.TreeItemCollapsibleState.Collapsed,
      );
      item.contextValue = "memqlConceptDomain";
      item.iconPath = new vscode.ThemeIcon("folder");
      return item;
    }

    const item = new vscode.TreeItem(
      node.concept.entity,
      vscode.TreeItemCollapsibleState.None,
    );
    item.contextValue = "memqlConcept";
    item.description = node.concept.id;
    item.tooltip = node.concept.description !== "" ? node.concept.description : node.concept.id;
    item.iconPath = new vscode.ThemeIcon("symbol-class");
    item.command = {
      command: "memql.concepts.open",
      title: "Open Concept",
      arguments: [node.concept],
    };
    return item;
  }
}
```

The `memql.concepts.open` command is registered in Task 9. Until then, clicking a concept is a no-op with a "command not found" notice — expected at this checkpoint.

- [ ] **Step 3: Register the tree**

In `editors/vscode/src/extension.ts`, add the import:

```typescript
import { ConceptsTreeProvider } from './views/conceptsTree.js';
```

Inside `registerRuntimeSurface`, after the clusters watcher registration, add:

```typescript
  const conceptsTree = new ConceptsTreeProvider(connections);
  context.subscriptions.push(
    window.registerTreeDataProvider('memqlConcepts', conceptsTree),
    commands.registerCommand('memql.concepts.refresh', () => conceptsTree.refresh())
  );
```

- [ ] **Step 4: Compile and test**

```bash
cd editors/vscode && npm run compile && npm test
```

Expected: compile clean; existing tests pass.

- [ ] **Step 5: Commit**

```bash
git add editors/vscode/src/views/conceptsTree.ts editors/vscode/src/extension.ts \
        editors/vscode/package.json
git commit -m "feat(vscode): add the Concepts tree grouped by domain

Reads the registry via ConceptsListMsg, so a concept added to the DSL
appears with no client change -- the point of a generic browser. The list
is cached per connection and cleared whenever the connection changes,
since concepts are per-cluster."
```

---

## Task 9: Concept webview tab with rows, paging, and detail

Clicking a concept opens an editor tab listing its rows, paging through the keyset cursor, and showing a selected row's full detail.

**Files:**
- Create: `editors/vscode/src/webview/conceptPanel.ts`
- Modify: `editors/vscode/src/extension.ts`
- Modify: `editors/vscode/package.json`

**Interfaces:**
- Consumes: `browseConceptPage`, `getRowByConceptAndId` (Task 4); `renderRowList`, `renderDetail`, `renderToHtml`, `escapeHtml` (Tasks 1–3); `ConnectionManager` (Task 6).
- Produces:
  - `class ConceptPanel` with `static open(context, connections, concept): void`
  - Command: `memql.concepts.open`

- [ ] **Step 1: Declare the command**

In `editors/vscode/package.json`, add to `contributes.commands`:

```json
      { "command": "memql.concepts.open", "title": "memQL: Open Concept" }
```

- [ ] **Step 2: Write the panel**

Create `editors/vscode/src/webview/conceptPanel.ts`:

```typescript
// The concept browser tab: row list, keyset paging, and row detail.
//
// All rendering is delegated to view-kit, which emits an HTML string and knows
// nothing about VS Code. That is deliberate -- the same renderer serves the
// portal, so any VS Code-specific markup here would be markup the portal has
// to rebuild.
//
// The webview runs under a strict CSP with a per-load nonce: row data is
// untrusted, and view-kit escapes it, but a CSP means an escaping bug cannot
// become script execution.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";
import { browseConceptPage, getRowByConceptAndId } from "@znasllc-io/memql-sdk-core/client";
import { renderDetail, renderRowList, renderToHtml, escapeHtml } from "@znasllc-io/memql-view-kit";

import type { ConnectionManager } from "../connection/manager.js";

const PAGE_SIZE = 200;

// flattenForList projects a wire node into the flat shape a display card names
// its fields on. The wire keeps payload nested; the display card names payload
// fields directly, so the list needs the merge. Detail rendering deliberately
// does NOT flatten -- it shows the nesting.
function flattenForList(node: Row): Row {
  const out: Row = {};
  for (const [k, v] of Object.entries(node)) {
    if (k === "payload" && v !== null && typeof v === "object" && !Array.isArray(v)) {
      for (const [pk, pv] of Object.entries(v as Record<string, unknown>)) {
        out[pk] = pv;
      }
      continue;
    }
    out[k] = v;
  }
  return out;
}

export class ConceptPanel {
  private static readonly open_ = new Map<string, ConceptPanel>();

  private readonly panel: vscode.WebviewPanel;
  private nodes: Row[] = [];
  private nextCursor = "";
  private selectedRowId: string | undefined;
  private detail: Row | null = null;
  private error = "";

  static open(
    context: vscode.ExtensionContext,
    connections: ConnectionManager,
    concept: Concept,
  ): void {
    const existing = ConceptPanel.open_.get(concept.id);
    if (existing !== undefined) {
      existing.panel.reveal();
      return;
    }
    const panel = new ConceptPanel(context, connections, concept);
    ConceptPanel.open_.set(concept.id, panel);
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly connections: ConnectionManager,
    private readonly concept: Concept,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlConcept",
      `Concept: ${concept.entity}`,
      vscode.ViewColumn.Active,
      { enableScripts: true, retainContextWhenHidden: true },
    );

    this.panel.onDidDispose(
      () => ConceptPanel.open_.delete(concept.id),
      null,
      context.subscriptions,
    );

    this.panel.webview.onDidReceiveMessage(
      (msg: { type: string; rowId?: string }) => {
        if (msg.type === "selectRow" && msg.rowId !== undefined) {
          void this.selectRow(msg.rowId);
        } else if (msg.type === "loadMore") {
          void this.loadPage();
        } else if (msg.type === "reload") {
          this.nodes = [];
          this.nextCursor = "";
          this.selectedRowId = undefined;
          this.detail = null;
          void this.loadPage();
        }
      },
      null,
      context.subscriptions,
    );

    this.render();
    void this.loadPage();
  }

  private async loadPage(): Promise<void> {
    const query = this.connections.query;
    if (query === undefined) {
      this.error = "Not connected. Select a cluster in the Clusters view.";
      this.render();
      return;
    }
    try {
      const page = await browseConceptPage(query, this.concept.id, {
        pageSize: PAGE_SIZE,
        ...(this.nextCursor === "" ? {} : { cursor: this.nextCursor }),
      });
      this.nodes = this.nodes.concat(page.rows);
      this.nextCursor = page.nextCursor;
      this.error = "";
    } catch (err) {
      this.error = err instanceof Error ? err.message : String(err);
    }
    this.render();
  }

  private async selectRow(rowId: string): Promise<void> {
    this.selectedRowId = rowId;
    const query = this.connections.query;
    if (query === undefined) {
      this.error = "Not connected. Select a cluster in the Clusters view.";
      this.render();
      return;
    }
    try {
      this.detail = await getRowByConceptAndId(query, this.concept.id, rowId);
      this.error = "";
    } catch (err) {
      this.error = err instanceof Error ? err.message : String(err);
      this.detail = null;
    }
    this.render();
  }

  private render(): void {
    const nonce = nonceValue();
    const listHtml = renderToHtml(
      renderRowList(
        this.nodes.map(flattenForList),
        this.concept,
        this.selectedRowId,
      ),
    );
    const detailHtml =
      this.detail === null
        ? '<div class="vk-empty">Select a row.</div>'
        : renderToHtml(renderDetail(this.detail));

    const errorHtml =
      this.error === ""
        ? ""
        : `<div class="error">ERROR: ${escapeHtml(this.error)}</div>`;

    const moreHtml =
      this.nextCursor === ""
        ? ""
        : '<button id="more" type="button">Load more</button>';

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.concept.entity)}</title>
<style nonce="${nonce}">
  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0; padding: 0; }
  .toolbar { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             display: flex; gap: 12px; align-items: center; }
  .layout { display: grid; grid-template-columns: minmax(240px, 40%) 1fr; height: calc(100vh - 42px); }
  .pane { overflow: auto; padding: 8px 12px; }
  .pane + .pane { border-left: 1px solid var(--vscode-panel-border); }
  .vk-rows { list-style: none; margin: 0; padding: 0; }
  .vk-row { padding: 4px 6px; cursor: pointer; border-radius: 3px;
            display: flex; gap: 8px; align-items: baseline; }
  .vk-row:hover { background: var(--vscode-list-hoverBackground); }
  .vk-row[data-selected="true"] { background: var(--vscode-list-activeSelectionBackground);
                                  color: var(--vscode-list-activeSelectionForeground); }
  .vk-row-secondary, .vk-row-tertiary { opacity: 0.7; font-size: 0.9em; }
  .vk-row-status { margin-left: auto; font-size: 0.8em; opacity: 0.8;
                   border: 1px solid var(--vscode-panel-border); border-radius: 8px; padding: 0 6px; }
  .vk-field { display: flex; gap: 8px; padding: 1px 0; }
  .vk-key { opacity: 0.7; min-width: 8em; }
  .vk-nested { padding-left: 12px; border-left: 1px solid var(--vscode-panel-border); }
  .vk-null, .vk-empty-value, .vk-cycle { opacity: 0.5; font-style: italic; }
  .vk-empty { opacity: 0.6; padding: 8px 0; }
  .error { color: var(--vscode-errorForeground); padding: 8px 12px; }
  button { background: var(--vscode-button-background); color: var(--vscode-button-foreground);
           border: none; padding: 4px 10px; cursor: pointer; border-radius: 2px; }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(this.concept.id)}</strong>
  <span>${this.nodes.length} loaded</span>
  <button id="reload" type="button">Reload</button>
  ${moreHtml}
</div>
${errorHtml}
<div class="layout">
  <div class="pane" id="rows">${listHtml}</div>
  <div class="pane" id="detail">${detailHtml}</div>
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  // One delegated listener: view-kit emits data attributes, never inline
  // handlers, which is what lets the CSP forbid them outright.
  document.getElementById('rows').addEventListener('click', (e) => {
    const row = e.target.closest('[data-row-id]');
    if (row) vscode.postMessage({ type: 'selectRow', rowId: row.dataset.rowId });
  });
  document.getElementById('reload').addEventListener('click', () =>
    vscode.postMessage({ type: 'reload' }));
  const more = document.getElementById('more');
  if (more) more.addEventListener('click', () => vscode.postMessage({ type: 'loadMore' }));
</script>
</body>
</html>`;
  }
}

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one -- its output is predictable from prior draws, which defeats the
// nonce's purpose. node:crypto is built in, so this costs no dependency.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
```

- [ ] **Step 3: Register the open command**

In `editors/vscode/src/extension.ts`, add the imports:

```typescript
import type { Concept } from '@znasllc-io/memql-sdk-core/client';
import { ConceptPanel } from './webview/conceptPanel.js';
```

Inside `registerRuntimeSurface`, after the concepts-tree registration, add:

```typescript
  context.subscriptions.push(
    commands.registerCommand('memql.concepts.open', (concept: Concept) => {
      if (connections === undefined) {
        return;
      }
      ConceptPanel.open(context, connections, concept);
    })
  );
```

- [ ] **Step 4: Compile and test**

```bash
cd editors/vscode && npm run compile && npm test
```

Expected: compile clean; existing tests pass.

- [ ] **Step 5: Verify end to end against the local cluster**

```bash
cd /home/znas/memql-projects/memql && make up && make status
```

Then press F5 in VS Code with `editors/vscode` open to launch the Extension Development Host. In the new window:

1. Open the memQL activity-bar container.
2. Confirm the Clusters view lists the local cluster.
3. Click it and confirm the icon turns to a filled green circle.
4. Expand a domain in the Concepts view and click a concept.
5. Confirm the tab opens, rows render, and clicking a row populates the detail pane.

Expected: all five. If the Clusters view is empty, confirm `~/.memql/clusters.yaml` exists; create the local entry with **memQL: Add Cluster** and a PAT minted at the identity binary's `/me/tokens`.

- [ ] **Step 6: Commit**

```bash
git add editors/vscode/src/webview/conceptPanel.ts editors/vscode/src/extension.ts \
        editors/vscode/package.json
git commit -m "feat(vscode): add the concept browser tab with paging and detail

Rows page through the keyset cursor; a selected row renders its full nested
shape. All rendering goes through view-kit, which emits an HTML string and
knows nothing about VS Code -- the same renderer serves the portal.

The webview runs under a strict CSP with a per-load nonce. view-kit escapes
row data, but row data is untrusted, and a CSP means an escaping bug cannot
become script execution."
```

---

## Task 10: Live row refresh and documentation

Rows update as the graph changes, and the feature is documented.

**Files:**
- Modify: `editors/vscode/src/webview/conceptPanel.ts`
- Create: `docs/public/language/vscode-runtime-panel.md`
- Modify: `docs/public/language/vscode.md`
- Modify: `editors/vscode/README.md`
- Modify: `GLOSSARY.md`

**Interfaces:**
- Consumes: everything from Tasks 1–9; `SubscriptionManager` from the SDK.
- Produces: no new exported surface.

- [ ] **Step 1: Subscribe to graph changes**

In `editors/vscode/src/webview/conceptPanel.ts`, add a field to the class:

```typescript
  private unsubscribe: (() => void) | undefined;
```

In the constructor, after the `onDidReceiveMessage` registration, add:

```typescript
    this.subscribeToChanges();
```

Extend the dispose handler so the subscription is released with the panel:

```typescript
    this.panel.onDidDispose(
      () => {
        this.unsubscribe?.();
        this.unsubscribe = undefined;
        ConceptPanel.open_.delete(concept.id);
      },
      null,
      context.subscriptions,
    );
```

Add the method:

```typescript
  // Live refresh. A CDC subscription on this concept means a row written by
  // anything -- another operator, an automation, or a mutation run from the
  // editor in a later increment -- appears without a manual reload. That is
  // the loop this panel exists to close.
  //
  // The whole page set is reloaded rather than patched: a CDC event carries
  // the change, not the row's position in this query's sort order, so
  // splicing it in would put rows in the wrong place. Reloading is correct and
  // a concept browser is not hot enough for the cost to matter.
  private subscribeToChanges(): void {
    const subs = this.connections.subscriptions;
    if (subs === undefined) return;
    try {
      this.unsubscribe = subs.subscribeGraph(
        { concept: this.concept.id, actions: ["created", "updated", "deleted"] },
        () => {
          this.nodes = [];
          this.nextCursor = "";
          void this.loadPage();
        },
      );
    } catch (err) {
      // A subscription failure degrades to manual reload; it must never take
      // the panel down with it.
      this.error = `live updates unavailable: ${err instanceof Error ? err.message : String(err)}`;
      this.render();
    }
  }
```

Read `sdk/ts/src/client/subscriptions.ts` and match `subscribeGraph`'s actual signature and unsubscribe contract — adjust the call, not the intent, if it differs.

- [ ] **Step 2: Re-subscribe when the connection changes**

Still in the constructor, after `this.subscribeToChanges();`, add:

```typescript
    // A reconnect invalidates the old subscription; re-establish against the
    // new stream or the panel silently stops updating.
    const offState = this.connections.onDidChangeState((state) => {
      this.unsubscribe?.();
      this.unsubscribe = undefined;
      if (state.status === "connected") {
        this.subscribeToChanges();
        this.nodes = [];
        this.nextCursor = "";
        void this.loadPage();
      }
    });
    context.subscriptions.push({ dispose: offState });
```

- [ ] **Step 3: Compile and verify live refresh**

```bash
cd editors/vscode && npm run compile && npm test
```

Then in the Extension Development Host, open a concept with a small row count and insert a row through any other surface (the cockpit, or `psql`). Confirm the list updates with no manual reload.

Expected: compile clean, tests pass, list updates within a second or two.

- [ ] **Step 4: Write the feature documentation**

Create `docs/public/language/vscode-runtime-panel.md`:

```markdown
---
title: VS Code Runtime Panel
audience: public
status: stable
area: language
sinceVersion: 0.4.0
owner: znas
---

# VS Code Runtime Panel

The memQL extension's activity-bar panel connects VS Code to a running
cluster: pick a cluster, browse every registered concept, and inspect
rows without leaving the editor.

## Requirements

- A **trusted** workspace. Language features (highlighting, diagnostics,
  completion, hover, signature help) work in an untrusted workspace; the
  runtime panel does not. It reads credentials and opens a network
  connection, which a malicious workspace must not be able to trigger.
- A cluster in `~/.memql/clusters.yaml` with an endpoint and a Personal
  Access Token.

## Clusters

The panel reads the same `~/.memql/clusters.yaml` the memQL Cockpit uses,
so a cluster added in either tool appears in both. The file is watched:
an external edit refreshes the view.

Click a cluster to make it the working cluster. The selection persists to
`selected_cluster`, so the cockpit resumes on the same cluster.

| Icon | Meaning |
|---|---|
| Filled green circle | Connected |
| Spinner | Connecting |
| Red error icon | Connection failed; hover for the message |
| Yellow warning | Not configured -- no endpoint, or no credential |
| Hollow circle | Configured, not connected |

**Add Cluster** and **Edit Cluster** collect a name, domain, endpoint and
PAT. Writes preserve comments and any field a newer cockpit wrote,
because the file is shared.

### Authentication

This panel authenticates with the `pat` field. Mint a token at the
identity binary's `/me/tokens`.

A cluster configured for OIDC with no PAT reports that it must be
authenticated in the memQL Cockpit first -- the panel cannot yet read the
cockpit's keyring credential store.

## Concepts

The Concepts view lists every registered concept on the connected
cluster, grouped by domain, read from the engine's own registry. A
concept added to the DSL appears with no extension update.

Click a concept to open its browser tab: rows on the left, detail on the
right.

- Rows are labelled using whatever `@displayCard` slots the concept
  declares, falling back to the row id when it declares none.
- **Load more** pages through the keyset cursor; a concept larger than one
  page is fully walkable.
- Selecting a row shows its full nested shape -- payload, provenance and
  intrinsics -- with no flattening, so the intrinsics stay visible.
- The list updates live as rows are created, updated or deleted.

There is no concept-specific rendering anywhere in the panel. That is
deliberate: it is what makes a newly declared concept work the day it is
declared.

## What this panel does not do yet

Executing constructs, running automations, and driving deployments are
later increments. See
[the design spec](../../superpowers/specs/2026-08-07-vscode-runtime-panel-design.md).
```

- [ ] **Step 5: Cross-link the documentation**

In `docs/public/language/vscode.md`, add near the top, after the intro:

```markdown
> The extension also ships a runtime panel -- cluster selection and a
> generic concept browser against a live cluster. See
> [VS Code Runtime Panel](vscode-runtime-panel.md).
```

In `editors/vscode/README.md`, add a `## Runtime panel` section summarizing the same in three or four sentences and linking to the doc.

In `GLOSSARY.md`, add an entry pointing at `docs/public/language/vscode-runtime-panel.md`, matching the surrounding format exactly.

- [ ] **Step 6: Verify the docs conformance tests pass**

```bash
cd /home/znas/memql-projects/memql && go test -count=1 ./... -run 'TestDocs|TestGlossary|TestLifecycle'
```

Expected: PASS. The repo has several docs conformance guards (`docs_construct_names_test.go`, `lifecycle_docs_conformance_test.go`); if one rejects the new file, follow its message rather than weakening the guard.

- [ ] **Step 7: Full verification**

```bash
cd /home/znas/memql-projects/memql
make viewkit-typecheck && make viewkit-test
make sdk-ts-typecheck && make sdk-ts-test
make vscode-deps && make vscode-test
go test -count=1 ./cmd/memql-lsp/... ./scripts/dev/...
bash scripts/vscode/package.sh --out=/tmp/claude-1000/-home-znas-memql-projects-memql/72dc4c20-4d55-4a3c-a02e-eff35982ed0f/scratchpad/memql.vsix
```

Expected: every command exits zero and the VSIX packages.

- [ ] **Step 8: Commit**

```bash
git add editors/vscode/src/webview/conceptPanel.ts \
        docs/public/language/vscode-runtime-panel.md docs/public/language/vscode.md \
        editors/vscode/README.md GLOSSARY.md
git commit -m "feat(vscode): live row refresh, and document the runtime panel

A CDC subscription on the open concept means a row written by anything --
another operator, an automation, or a mutation run from the editor in a
later increment -- appears without a manual reload.

The page set is reloaded rather than patched: a CDC event carries the
change but not the row's position in this query's sort order, so splicing
would misplace rows.

Re-subscribes on reconnect; without that the panel silently stops updating
after a dropped stream."
```

---

## Structural amendments during execution

Two things B1 now depends on structurally were not in this plan when it was
written. B2 builds on both, so they are recorded here rather than left to be
rediscovered from the diff.

### 1. The `ws` dependency + `webSocketFactory` (how the extension connects)

The extension host has **no global `WebSocket`**. It runs Node 20 on the
declared `"vscode": "^1.91.0"` floor, and the global only arrives in Node 22.
The SDK's default socket factory throws a clear error in that case, so
`editors/vscode` takes a dependency on the `ws` package and every dial passes a
`webSocketFactory`:

```ts
Connection.dial({
  ...opts,
  webSocketFactory: (url, protocols) =>
    new NodeWebSocket(url, protocols) as unknown as WebSocket,
});
```

A factory that does not forward `protocols` breaks authenticated dials --
bearer/guest credentials ride the WebSocket subprotocol channel (memql#2511),
not the URL.

Related, and the reason this is called out rather than left implicit: the SDK
itself must not dereference the bare global either. `socket.readyState ===
WebSocket.OPEN` evaluates both operands, so it threw `ReferenceError` before
readyState was consulted, defeating the factory entirely. `sdk/ts` now compares
against numeric constants (`sdk/ts/src/client/wsReadyState.ts`). **Any new SDK
code on the socket path must use those constants**, and
`sdk/ts/test/connection-no-global-websocket.test.ts` is the regression guard --
it dials with no global installed, unlike every other connection test.

### 2. esbuild bundling (how the extension builds)

`editors/vscode` bundles with esbuild (`esbuild.js` for the extension,
`esbuild.test.js` for `node --test`) rather than shipping raw `tsc` output.
Two independent forcing functions:

- **`file:` symlinked workspace deps broke `vsce package`.** `sdk/ts` and
  `sdk/ts-viewkit` are consumed as `file:` dependencies; `vsce` does not
  follow those symlinks into a packaged VSIX, so the installed extension
  could not resolve them at runtime. Bundling inlines them.
- **Both packages are pure ESM, and the extension host is CommonJS.** esbuild
  inlines them and emits CJS, which is what lets `manager.ts` use a plain
  static `import` of `@znasllc-io/memql-sdk-core/client` instead of a
  `require()` that would fail on an ESM-only package.

Consequences B2 inherits: `npm run compile` is `tsc -p ./ && node esbuild.js`
(typecheck AND bundle -- neither alone is sufficient); a new runtime dependency
must be bundleable or explicitly externalized; and `make vscode-deps` must run
before `npm ci` in `editors/vscode` on a clean checkout, because the `file:`
targets' `main`/`types` point into `dist/` directories that do not exist yet.

## Verification

After Task 10, B1 is complete when all of the following hold:

- [ ] `make viewkit-typecheck && make viewkit-test` passes (40 tests)
- [ ] `make sdk-ts-typecheck && make sdk-ts-test` passes (110 tests), including 12 new concept-browser tests
- [ ] `make vscode-test` passes (85 tests) from a clean checkout with no prebuilt `dist/`
- [ ] `go test ./cmd/memql-lsp/... ./scripts/dev/...` passes — extension manifest and CI lane-scope guards
- [ ] `bash scripts/vscode/package.sh` produces a VSIX
- [ ] In the Extension Development Host against `make up`: the activity bar shows the memQL icon; Clusters lists the local cluster and connects; Concepts lists domains and concepts; a concept tab renders rows, pages, shows detail, and updates live
- [ ] Opening an **untrusted** workspace leaves language features working and the runtime views absent

## Out of Scope for B1 — do not build these

Reject any of these if they appear in a task; they belong to later increments in the spec:

- Running constructs, CodeLens run affordances, arg forms, run configurations (B2)
- Automation invocation and step traces (B3)
- Topology rendering, deployment history, deploy actions (B4)
- The `local` field in `clusters.yaml` and the non-local write confirmation — B1 performs no writes to cluster data, so the field has no consumer yet (B2)
- OIDC authentication and keyring credstore access (deferred; PAT only)
- Any engine, proto, or Go SDK change

## Follow-ups to file

- **memql-cockpit:** add the `local` field to `cli/config.ClusterConfig` when B2 introduces it, so a round-trip through either tool preserves it. Not needed until B2.
- **Portal:** `@znasllc-io/memql-view-kit` is published to the GitHub registry from this repo; the portal spec should consume it rather than vendoring the renderer.
