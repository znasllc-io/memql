---
title: Dynamic views: bindings you can see, elements that fit, live rows, actions from the catalog
audience: internal
status: draft
area: design
sinceVersion: 0.19.6
owner: znas
---

# Dynamic views: bindings you can see, elements that fit, live rows, actions

> **Status.** This is the design record epic memql#4274 asked for. **Nothing in
> it is implemented.** The epic is large — six workstreams across the portal,
> the view kit and the SDK — and the reason it was filed as a spike is that its
> sixth decision (what a view is allowed to *do*) is a boundary question that
> cannot be settled while writing the first five. This record makes the
> decisions, sequences the work, and names the one that is still open.

## What is wrong, in one sentence each

Every claim below is from the epic and is reproduced here so this record stands
alone.

- **The composer emits no bindings at all.** The reducer supports them —
  `slotBound` / `slotCleared` (`compose/composerState.ts:89-90,169-187`), with
  tests and **zero UI callers**. A saved arrangement is `{element, band}` and
  nothing more.
- **A saved view cannot be deleted.** `archive(viewId)`
  (`compose/useSavedViews.ts:155-165`) has no UI either.
- **Pies lie, three independent ways.** The category is "the text field with
  the fewest distinct values in the loaded 100-row page"; `NON_DISPLAY_FIELDS`
  excludes only id/concept/type/schema/partition, so every foreign key
  qualifies, and `preferFewestDistinct` picks the *least* informative field —
  giving one-slice 100% pies. The measure is "the first numeric field in row
  key order"; `fitness.ts:329` names the failure itself: *"`revocationEpoch
  total` is a true number and a meaningless one."* A category summing to zero
  is dropped from the chart **and the denominator**, silently.
- **The five predefined views defend against this with `bindings: { value: [] }`.
  Composed views cannot write that.**
- **Duplicate elements.** The source says `chart.pie` and `chart.proportion`
  "answer the same question in different room" — identical slots, differing in
  accepted kinds and pixel height.
- **Nothing is live.** `useConceptRows.ts:155-185` *does* subscribe and
  maintains a live band; `useViewRows.ts:26-45` deliberately drops `live`,
  `liveDegraded` and `reload`. **The socket traffic is already paid for and
  discarded.**
- **No actions.** The whole vocabulary is `rowAction: "view"`.
- **The type information the view kit says it lacks is on every row.**
  `fitness.ts:22-31` says ConceptInfo carries no per-field types, so fitness is
  computed from a row sample — but `concepts/schema.ts:20-42` proves each row
  carries its concept's JSON Schema on the `schema` intrinsic, and
  `viewkit/rows.ts:23-43` preserves it, then lists `schema` in
  `NON_DISPLAY_FIELDS` and never opens it.
- **Runtime construct discovery is fully built and unused by the portal.**
  `ListConstructs` returns per construct its `kind`, `bound_concept` and `args`
  with `type` / `required` / `enum_values`. VS Code consumes it end to end
  including a generated run form. The portal's `ClusterClients` has no
  constructs client at all.

## The decisions

### 1. Schema-first fitness, with the row sample as fallback

Profile from the **declared schema** on the `schema` intrinsic; fall back to a
row sample only where the schema is silent.

- A **category** is a declared enum, a boolean, a status-family field, or a
  small-cardinality relationship target. **Never** a foreign key or free text.
- A **measure** is a declared number that is not an epoch, port, duration or
  version. If none qualifies, **the slot is declined and the element counts
  rows** — which is a correct answer, unlike summing `revocationEpoch`.
- Zero-sum and negative categories are **stated, not dropped**. A denominator
  that silently excludes rows is the one failure a reader cannot detect.

The ordering matters: `preferFewestDistinct` must be replaced, not tuned. It
optimises for the wrong thing — the least informative field wins by
construction.

### 2. Bindings visible and editable

Every placed element shows its slots and the field feeding each ("Share by
status · counting rows"), with a picker per slot, **"count rows" as an explicit
choice**, a title, and a band override. The rendered view shows the same
caption, so what a reader sees and what an author chose are the same sentence.
Saved views become deletable — `archive` already exists.

This is the smallest workstream and unblocks the most: with bindings writable,
a composed view can defend itself the way the five predefined ones already do.

### 3. Catalog by question, format as an option

Five questions — how many, how divided, over time, which ones, where — each
with format options. **Pie and proportion fold into one "share" element**;
`table`/`rowList` and `timeline`/`calendar` likewise. Choosing a *question* and
then a *shape* is the ordering that stops an author picking a pie because it is
in the list.

### 4. Live by policy, never by default

`useViewRows` exposes `live` / `liveDegraded` / `reload`; each view chooses off,
notice-and-refresh, or auto-apply. **Shape elements never shift under a reader
without a notice** — a pie that re-slices mid-read is worse than a stale one,
because the reader cannot tell it happened.

### 5. Actions from the construct catalog

A view's actions are the runnable mutations whose `bound_concept` is the view's
concept, **read at runtime** — so a pack mounted at `MEMQL_DSL_PATH`, or
promoted from VS Code, becomes a button with no portal release. Forms generated
from `args` exactly as the VS Code run form already does. Destructive verbs
confirm.

**Nothing in the UI authors MemQL text.** Composed views write only through the
constructs client.

### 6. The open decision: engine verbs that are not DSL mutations

"Invite a person", "deploy" and their kin are gRPC messages, not mutations, so
they have no `bound_concept` and cannot be discovered. Two candidate answers,
and this record does **not** pick one:

- **A closed adapter list** in the portal, mapping a concept to the engine
  verbs that act on it. Simple, reviewable, and it reintroduces exactly the
  per-release coupling item 5 exists to remove.
- **A `tool` construct fronting each one.** Tools already have a declared arg
  schema and are already discoverable, so the catalog read stays uniform and a
  new verb needs no portal change. Costs a `tool` declaration per verb and a
  handler that dispatches to the gRPC path.

The second is more consistent with the rest of the epic. It is left open
because it commits `tool` to a second job — being the declarative face of a
non-DSL verb — and that is a language decision rather than a portal one.

## Sequencing

Ordered so each step is shippable and the risky one comes last.

1. **Bindings + delete** (decision 2). No new data sources; unblocks authors
   defending their own views immediately.
2. **Schema-first fitness** (decision 1). Behind the bindings, so a bad
   automatic choice is now correctable rather than only wrong.
3. **Catalog consolidation** (decision 3). Mostly deletion.
4. **Live by policy** (decision 4). The subscription already exists; this is
   plumbing plus a policy field.
5. **Constructs client + generated action forms** (decision 5), after decision
   6 is settled.

## Constraints any implementation must respect

These are enforced, not advisory:

- ~~`portal_view_composition_test.go` — no row markup or iteration in the five
  view bodies.~~ **Deleted with the portal (epic memql#4984), along with the
  view bodies it scanned. The constraint still describes what a replacement
  should hold; nothing enforces it now.**
- ~~`portal_render_path_test.go` — no concept-id literal in the generic render
  path.~~ **Deleted with the portal (epic memql#4984). Same standing: a real
  constraint with no enforcer, listed here so a reader of this draft does not
  assume the check exists.**
- `sdk/ts-viewkit/test/guards.test.ts` — no DOM, no dependencies, no branching
  on a concept id.
- The view kit README's rule: **no inline event handlers** — data attributes,
  one delegated listener.

## Related

memql#3320 (composer), #3319 (predefined views), #3749/#3750 (construct
catalog), #3309 (run form), #2460 (graph subscriptions), #4182 (live tiles),
#4142/#4138 (packs render through composed views). Full evidence:
`.claude/prds/portal-polish.md` item 6. Sibling epic: memql#4261.
