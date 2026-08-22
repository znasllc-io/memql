---
audience: internal
status: active
area: design
owner: platform
---

# Artifacts in the portal, and labels you can put on them

**Date:** 2026-08-22
**Brief:** the operator's voice brief of 2026-08-21 (recorded verbatim in the
epic). Sub-project 2 of that brief; sub-project 1 was the portal <-> VS Code
handoff (epic memql#4242, spec `2026-08-21-portal-vscode-handoff-and-locality-design.md`).

## 1. What this is

Two things the operator asked for, in one seam:

1. **The portal has no Artifacts page.** The Library already indexes every
   artifact a cluster produces; nothing in the portal shows them.
2. **Artifacts carry no labels.** The operator wants labels "to help the user
   understand, like, for what project it was" -- editable by the person, and
   editable by the agents they are talking to.

## 2. What already exists (and therefore what this does NOT build)

`v1:library:artifact` (`dsl/library/concepts.memql:40`) is a mature index row:
17 fields covering ownership, lens, kind, provenance (`source`,
`producedByPlanId`, `producedByWorkerId`), format, validation rollup and an
`updatedAt` watermark. Five automations promote rows into it on create
(generated outputs, notes, todos, calendar events, memories). Eleven queries
read it. Two agent tools produce into it (`produceArtifact`, `editDocument`).
A Go integration (`integrations/library/`) owns the read-modify-write paths.

**So this sub-project adds a field, three reads, two writes, and a page.** It
does not build an artifact system; one is already here.

## 3. Decisions

### D1 -- Labels are a `[]string` field on the artifact, not a concept

The tree has exactly one label idiom and it is a bare string array on the
owning concept: `note.tags` (`dsl/notes/concepts.memql:26`), `skill.tags`
(`dsl/agents/concepts.memql:203`). **No label/tag join concept exists for any
concept anywhere in `dsl/`.** Facet reads use a membership predicate --
`when(args.tag) { args.tag in tags }` (`dsl/notes/queries.memql:42`). We copy
that shape exactly.

Rejected: a `v1:library:label` concept plus a join. It buys global rename and
label-level permissions, neither of which the brief asks for, and it would be
the first of its kind in the tree.

**Named `labels`, not `tags`.** The operator says "labels" throughout, the
portal will say "Labels", and the generic concept browser renders raw field
names -- so a field called `tags` would contradict the page above it. The cost
is a name that already exists elsewhere with a different meaning:
`worker.labels` and `cluster.labels` are `object` maps for machine routing
("has-blender=true"). Different namespace, different type, different job; the
field's `@description` says so out loud so nobody conflates them.

### D2 -- Add and remove are Go builtins, not a set-the-whole-array mutation

MemQL has no array append. The DSL-only option is "caller passes the complete
new list", which is what `editDocument` does for content ("the FULL new
content"). That is acceptable for a document body and wrong for labels:

- an agent that forgets to read first **wipes every existing label**, and
- the person and their agent labelling in the same conversation is the
  *expected* case, not a rare race -- last writer clobbers the other.

`integrations/library/` already owns exactly this dance for document versions
(load under a system actor, re-derive, write under a synthetic owner actor).
Two capabilities join it:

    integration.library.addArtifactLabel     (artifactId, label)
    integration.library.removeArtifactLabel  (artifactId, label)

Each loads the row, merges the single label in or out, and writes back. Adding
a label already present, or removing one absent, succeeds and changes nothing
-- idempotent, because both an agent retry and a double-click will do it.

One implementation serves both callers: `generated_builtins.ts` already exists
in the TS SDK, so the portal calls the same builtin the agent tool wraps.

### D3 -- `touchArtifact` must carry labels forward, or edits eat them

`integrations/library/library.go:437-460` re-versions an artifact row by
re-calling `createArtifact` with a **fixed argument list**. MemQL is
insert-versioning: the new version has only the fields that call names. So the
moment `labels` exists, every document edit silently drops the artifact's
labels, and nothing would fail.

**This is the one place this feature can lose user data**, so it is a named
requirement with its own regression test, not a note: `touchArtifact` reads the
current row's labels and passes them through, and a test labels an artifact,
edits its document, and asserts the labels survived.

The five `index*OnCreate` automations do **not** need this -- they fire on
`node.created` for a source row that has never been labelled.

### D4 -- The page is a bespoke feature directory, modelled on Sites

The portal has three list-page archetypes. The five predefined views
(`src/views/`) are mechanically forbidden from hand-rendering a row -- they may
only compose view-kit elements (`portal_view_composition_test.go`) -- which
rules them out, because a label editor is a custom interaction widget. The
generic concept browser already renders artifacts but cannot host one either.

So: `clients/portal/src/artifacts/`, following `src/sites/` -- a routes splat,
a list page, a detail page, `urls.ts`, and hooks over `useConceptRows`. Row
layout still comes from `@displayCard` via `RowList`; only the label editor is
new UI.

Every destination is a URL, including the drill-in (`/artifacts/:artifactId`)
and the active label filter (`?label=`). The portal states this rule in three
places and this page does not get to be the exception.

### D5 -- "Create an artifact" means recording one, not authoring a file

The brief says users "can create new artifacts if they want". Artifacts index
*backing rows*; `artifact.sourceConceptRef` is the idempotency key. A portal
"New artifact" that wrote a bare index row with nothing behind it would create
a row every drill-in renders as broken.

So the portal's create affordance mints a `generatedOutput` (title, summary,
markdown body) -- which the existing `indexGeneratedOutputOnCreate` automation
promotes into an artifact on its own. The person gets "I made an artifact"; the
graph stays consistent. **Uploading bytes is out of scope** -- that is the
existing attachment path and the brief does not ask to move it.

## 4. The surface

### Concept change

    // dsl/library/concepts.memql, on concept artifact
    labels []string @description("Free-text labels the owner or their agents put on
      this artifact to say what it was for. Rendered as chips in the portal's
      Artifacts page and filterable one label at a time. Distinct from
      v1:worker:registration.labels and v1:cluster:node.labels, which are key=value
      OBJECT maps for machine routing -- these are display labels on a []string,
      matching note.tags and skill.tags.")

`createArtifact` gains a matching optional `labels []string` arg.

### Reads

    query artifact libraryArtifactsByLabel { args { label string! } ... }

filtering `ownerUserId==actor.userId && when(args.label) { args.label in labels }`.

**The owner conjunct is top-level and unguarded, deliberately.**
`TestPerRowAuthzClassification` flags a construct whose only caller-scope term
sits inside a `when()` -- the guard drops when its arg is absent, so the term
does not hold on every path. `FLAG` must be exactly 0 or the build fails.

`artifactFull` gains `labels` so the list and detail render them. The label set
the filter bar offers is derived client-side from the rows already walked -- no
DISTINCT aggregation, which MemQL does not have.

### Writes

    builtin libraryAddArtifactLabel    { artifactId string! ; label string! }
    builtin libraryRemoveArtifactLabel { artifactId string! ; label string! }

`@executor("integration.library.addArtifactLabel" | ".removeArtifactLabel")`.

### Agent tools

    tool artifactAddLabel     { artifactId! ; label! }
    tool artifactRemoveLabel  { artifactId! ; label! }

`@handler(type="function", name="libraryAddArtifactLabel")` -- a builtin is
reached through the `function` handler type; there is no `"builtin"` type. The
handler target resolves at load, so a typo refuses boot rather than failing
mid-turn.

Authorization needs no new mechanism: the builtin resolves the artifact under
the caller's own actor, so an agent acting in a user's session can only label
what that user can already read.

### Portal

`clients/portal/src/artifacts/` -- `ArtifactsRoutes`, `ArtifactsPage`
(list + label filter bar + create form), `ArtifactDetailPage` (row detail +
label editor), `urls.ts`, `concepts.ts`, `useArtifacts.ts`.
`/artifacts` and `/artifacts/:artifactId`, plus a nav-rail entry.

New shared primitive: `src/ui/LabelChips.tsx` -- renders labels as `Badge`
chips, each with a remove control, plus an add input. It is the only new
design-system piece; a raw input or button class outside `src/ui/` is a defect
the portal sweeps for.

## 5. Testing

- **DSL conformance** -- `TestPerRowAuthzClassification` FLAG=0 for every new
  construct; the filter follows the canonical clause syntax.
- **The label-loss regression (D3)** -- label an artifact, run the document edit
  path, assert labels survive. This test fails against today's `touchArtifact`.
- **Go** -- add/remove idempotency both directions; a label on someone else's
  artifact is refused.
- **Portal** -- vitest in `clients/portal/test/artifacts.test.tsx` against a fake
  connection: list renders, label filter narrows and lives in the URL, add and
  remove call the builtin, empty and error states.

## 6. Delivery

Three PRs, matching the operator's standing limit:

1. **Backend** -- the field, the query, the shape, the two builtins, the Go
   capabilities, the `touchArtifact` fix + its regression test, the agent tools.
2. **Portal** -- the feature directory, both pages, `LabelChips`, the nav entry,
   the vitest suite.
3. **Docs** -- the Library page gains labels; the portal doc gains the page.

PR 2 depends on PR 1's generated SDK surface, so it branches off PR 1 and
targets `main` (a dependent PR gets CI when its BASE is `main`).

## 7. Out of scope

Uploading bytes from the portal; label rename-everywhere; per-label
permissions; sharing an artifact with another user; a label taxonomy or
autocomplete across users. None is asked for, and each would outgrow a
`[]string`.
