---
title: Constructs View and the Value Viewer
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-14
owner: znas
surface: engine (component/grpc, component/memql) + VS Code extension + view-kit
---

# Constructs View and the Value Viewer

Let a developer ask a running cluster what it has actually loaded, run the
constructs that can be run, and read the answer.

Sub-project **A** of the 2026-08-14 brainstorm. Tracked as epic memql#3747.

---

## 1. Problem

Three gaps, each with evidence.

1. **Nothing enumerates a cluster's constructs.** The pack browser
   (`ListPackDomainsMsg` / `ListPackFilesMsg` / `ReadPackFileMsg`) is
   **file**-grain: domain, then filenames, then raw source text. It cannot
   answer "what has this cluster loaded", and it structurally cannot see a
   **promoted** construct, which lives in the database and in no file at all.

2. **A construct's argument schema is only available from a local file.**
   `src/state/argForm.ts` already builds a complete argument form -- typed
   fields, `@enum` dropdowns, `@autoInjected` marking, no-guess coercion -- but
   every field comes from the language server parsing the `.memql` file in the
   editor. Browsing a cluster, there may be no such file. That is why "the
   input being passed to the construct" is missing from any surface that is not
   an open document.

3. **The value renderer is a recursive dump.** `sdk/ts-viewkit/src/detail.ts`
   is 67 lines that walk a row and emit nested `<div>`s. No collapsing, no
   copy, no type information, no search. It is used by the concept browser and
   by run results alike, so both are equally hard to read.

---

## 2. Constraints discovered in the tree

### 2.1 The runnable set is already decided, and deliberately narrow

`src/constructs/runnable.ts` fixes it at five:

```ts
export const RUNNABLE_KINDS = ["query", "mutate", "logic", "tool", "automation"];
```

with the reason recorded in the same file: *"spec / trait / prompt / seed /
concept / shape / provider / builtin each need an execution semantic decided
(which row does a spec evaluate against; who pays for a prompt's provider call)
that the runtime-panel design explicitly defers."*

That deferral stands. Everything else is **view-only**, and this spec does not
reopen it.

### 2.2 The engine already knows where a construct came from

`Function` (`component/memql/function_types.go:70`) carries `Origin` -- "where
this function was loaded from (file path)" -- plus `Description`, `DocComment`,
`UsageDoc`, `ExprSource`, `BoundConcept`, `FunctionKind`. Jump-to-file is a
read, not a new index.

### 2.3 One parser, and it is the server's

`src/constructs/runnable.ts` states the rule: *"THE LANGUAGE SERVER OWNS ALL
.memql PARSING. Nothing in this file, or anywhere else in the extension, looks
at construct syntax."* Two parsers would let the generated form disagree with
the compiler about what a construct accepts.

The new RPC must therefore report the **same shape** the LSP reports, so one
form model serves both paths. It does not get its own dialect.

### 2.4 view-kit has no DOM and no inline handlers

`renderToHtml` returns a string; interactivity is data attributes plus one
delegated listener, because the webview CSP forbids inline script. A collapsible
viewer must express collapse as markup plus delegation, not as an event
handler.

---

## 3. Naming: Constructs browses definitions, Data browses rows

A concept **is** a construct, so a "Constructs" view beside a "Concepts" view
overlaps by name. The resolution:

| View | Shows | Was |
|---|---|---|
| **Constructs** | definitions -- every kind, every origin | new |
| **Data** | rows | "Concepts" |

The old Concepts view never showed a concept's *definition*; it showed rows.
The rename says what it always did.

```
memQL
├── Deployments
├── Clusters
├── Constructs      NEW · definitions
│   ├── concepts (23)      view only
│   ├── queries (118)      ▶ run
│   ├── mutations (94)     ▶ run
│   ├── logic (31)         ▶ run
│   ├── tools (517)        ▶ run
│   ├── automations (42)   ▶ run
│   └── shapes / specs / traits / prompts / providers / builtins   view only
├── Data            renamed · rows
└── Runs
```

---

## 4. `ListConstructsMsg` -- the registry-grain read

A new message pair on `MemqlService.Stream`, read-only, alongside the existing
pack browser rather than replacing it (the pack browser answers "show me this
file"; this answers "what do you have").

It reads the **live registries** -- `e.functions`, `e.specs`, the concept
registry, the tool/prompt/provider registries -- so a promoted construct
appears the moment it is promoted, and a demoted one disappears.

Per construct:

| Field | Notes |
|---|---|
| `name` | |
| `kind` | concept, query, mutation, logic, tool, automation, spec, trait, shape, prompt, provider, builtin, policy |
| `namespace` | |
| `origin` | `core` \| `bundle` \| `promoted` |
| `originPath` | `Function.Origin` when it came from a file; empty for promoted |
| `description` | already resolved server-side (`///` doc comment, or `@description` for tool fields) |
| `runnable` | whether it is one of the five |
| `args[]` | the SAME shape the LSP reports (§2.3) |
| `boundConcept` | for query / mutation / shape / spec |
| `sourceHash` | content hash of the construct's source |

`sourceHash` is included here rather than in sub-project B because it costs
nothing to compute alongside the rest, and B's drift detection is exactly
"does the local file's construct hash match the cluster's".

**`origin` is derived, not stored.** `core` when the origin path resolves
inside the embedded tree, `bundle` when it resolves under `MEMQL_DSL_PATH`,
`promoted` when there is a `v1:authoring:construct` row and no file. That
derivation lives in ONE place server-side; no client re-derives it.

Authorization: the same tier as the pack browser. A construct's *definition*
is not data, and gating it more tightly than the source file it came from
would be incoherent.

---

## 5. The Constructs surface

**Tree** grouped by kind, then namespace. A runnable kind carries a run
affordance; a view-only kind does not, and does not render a disabled one --
the absence is the statement.

**Detail page** per construct: kind, origin badge (`core` / `bundle` /
`promoted`), bound concept, description, the argument table, and the source.

**Two actions:**

- **Run** / **Run with arguments** -- for the five runnable kinds. Reuses
  `src/run/orchestrator.ts`, `src/state/argForm.ts` and the Result view
  unchanged, with the argument fields sourced from `ListConstructs` instead of
  from the LSP. This is the whole point of matching shapes in §2.3.
- **Open the .memql file** -- `originPath` plus the signature range. A
  **promoted** construct has no file: its source opens in a read-only untitled
  document, labelled as living in the cluster's database. That is the honest
  rendering, and it is also the first place a developer meets the
  seeded-vs-trained distinction sub-project B builds on.

**The view is read-only.** Editing happens in a `.memql` file. Nothing in this
view mutates a construct.

---

## 6. The value viewer

A new view-kit module, `valueView.ts`, replacing `renderDetail`'s job
everywhere: the Data browser, run results, and the Constructs detail page.

It is a view-kit module rather than an extension component so the portal
inherits it -- the same reason view-kit exists at all.

What it does that `detail.ts` does not:

- **Collapse and expand** at every level, with objects and arrays collapsed
  beyond a depth threshold on first render, and a count on the collapsed node
  (`{...} 14 keys`, `[...] 1,284 items`).
- **Copy** at every node: copy this value, copy this subtree as JSON, copy the
  path.
- **Type badges** -- string / number / boolean / null / object / array -- so
  `"42"` and `42` are distinguishable, which today they are not.
- **Path breadcrumb** on the focused node (`payload.lineage.originatingPlanId`).
- **Filter** by key or value, matching nodes revealed with their ancestors.
- **Large-value handling**: long strings truncate with an expand affordance;
  long arrays page. A 4MB payload must not hang the webview.

What it keeps unchanged from `detail.ts`, because both are load-bearing:

- **The wire's nesting is preserved, not flattened.** Payload, provenance and
  intrinsics stay distinct; flattening drops the intrinsics an operator came to
  read.
- **The cycle guard.** `renderDetail` is a public export and a caller can hand
  it any object; unbounded recursion in a webview means a hung editor.
- **No concept-specific rendering, anywhere.** A row projects through whatever
  `@displayCard` slots its concept declares and degrades to its id when it
  declares none. That is what lets a concept declared five minutes ago render
  with no client change -- and it is exactly what makes a *promoted* concept
  render for free.

---

## 7. Module layout

**Engine**

```
component/grpc/constructs_handlers.go     ListConstructsMsg handler
component/memql/construct_catalog.go      registry walk + origin derivation
component/grpc/memql.proto                the message pair
```

**SDK**

```
sdk/ts/src/constructs/constructs.ts       ListConstructs client
```

**view-kit**

```
sdk/ts-viewkit/src/valueView.ts           the viewer
sdk/ts-viewkit/src/detail.ts              deleted; callers move to valueView
```

**Extension**

```
src/state/constructCatalog.ts   catalog model + grouping     pure
src/views/constructsTree.ts     the tree
src/webview/constructPanel.ts   the detail page
src/views/conceptsTree.ts       renamed to dataTree.ts
```

`src/state/argForm.ts`, `src/run/*` and `src/state/runResult.ts` are reused
**unchanged**. If any of them needs editing, the shapes in §2.3 have diverged
and that is the bug.

---

## 8. Errors

| Condition | Renders as |
|---|---|
| Not connected | the tree says so and offers connect; it does not render an empty list |
| Cluster predates `ListConstructs` | a stated version mismatch naming the message, not a blank view |
| `originPath` set but the file is absent locally | offer the source read-only, saying the file is not in this workspace |
| A run refused by role | the engine's refusal, naming the role, per the existing doctrine |
| A payload too large to render | the truncation affordance, never a hang |

---

## 9. Testing

- `component/memql/construct_catalog_test.go`: every kind enumerated; origin
  derivation across core / bundle / promoted; a promoted construct appears
  after promote and disappears after demote.
- A **shape-parity test** pinning `ListConstructs`'s arg shape to the LSP's
  `RunnableArg`, in the spirit of `cmd/memql-lsp/runnable.go`'s existing fixed
  contract. If they drift, the generated form disagrees with the compiler and
  the developer finds out by running the wrong thing.
- `valueView` under bare `node --test`: collapse state, filter, the cycle
  guard, large-array paging, and that escaping still neutralises every value.
- Extension state modules under `node --test`; panels under `test:host`.

---

## 10. Relationship to the other sub-projects

Depends on nothing. **B depends on this**: drift detection is "does this file's
construct hash match the cluster's", and `sourceHash` plus `origin` are what
make that question askable.

Inherits C's boundary rule (memql#3733): the plugin owns what is on your
machine and what you can reach. A construct browser is an authoring surface and
belongs here; a pod grid does not.
