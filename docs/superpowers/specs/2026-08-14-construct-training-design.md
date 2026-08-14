---
title: Training Constructs Into a Running Cluster
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-14
owner: znas
surface: engine (component/memql) + memql-lsp + VS Code extension + sdk/ts
---

# Training Constructs Into a Running Cluster

Make the two-tier reality visible and operable: constructs that live on disk
and load at boot, and constructs that live in the database and go live without
one.

Sub-project **B** of the 2026-08-14 brainstorm. Tracked as epic memql#3745.

---

## 1. Problem

A memQL cluster can already be **taught** a construct at runtime.
`DurablePromoteBundleMsg` validates in the Gate-1 sandbox, persists a
`v1:authoring:bundle` + `v1:authoring:construct` row pair, registers into the
shared registry, broadcasts a cross-node propagation event so the construct is
callable on **every** node within seconds -- no restart -- and
`RehydratePromotedConstructs` replays it at boot.

Almost nothing surfaces it:

- the extension never calls it;
- `sdk/ts` wraps `validateBundle` and `sessionDefineBundle` and **not** the
  durable promote;
- an author editing a `.memql` file gets no signal that what they wrote is
  unknown to the cluster they are pointed at;
- and **`concept` cannot be promoted at all**, which is the kind that matters
  most because a customer's domain arrives as nouns and every other kind binds
  to one.

The result is two tiers, both real, one of them invisible:

| | Where it lives | To change it |
|---|---|---|
| **Seeded** | the embedded `dsl/` tree, or a bundle at `MEMQL_DSL_PATH` | image or bundle rollout |
| **Trained** | `v1:authoring:construct` rows | promote; live in seconds |

---

## 2. Vocabulary

Fixed here because the rest of the design leans on it.

- **Untrained** -- a construct in a file that the connected cluster has no
  record of.
- **Drifted** -- a construct the cluster knows, whose local source no longer
  matches what was promoted.
- **Trained** -- promoted, persisted, live.
- **Seeded** -- loaded from disk at boot. Not trained, and not trainable
  without a rollout.

**Saving is not promoting.** A file may hold trained, untrained and drifted
constructs at once, and writing it to disk changes none of their states.

---

## 3. Drift detection

`ListConstructs` (sub-project A, memql#3747) returns `sourceHash` and `origin`
per construct. The language server computes the same hash for each construct in
the open document. Equal hash means trained; construct absent from the catalog
means untrained; present with a different hash means drifted.

**The hash must be computed identically on both sides**, which is the one real
contract risk here. It is defined once -- normalize the construct's source
(strip comments outside `///` doc blocks, collapse insignificant whitespace),
then SHA-256 -- and pinned by a parity test running both implementations over
the same corpus. Divergence would silently mark every construct drifted, which
reads as "everything is broken" rather than as a bug in the hash.

---

## 4. Editor presentation

The requirement is that state is visible **without fighting syntax colouring**.
So: not semantic tokens, and not Problems-panel diagnostics -- an untrained
construct is a state, not an error.

**Gutter icon plus a subtle signature-range decoration**, via
`TextEditorDecorationType`. Three states, three icons, distinguishable without
colour alone.

**A CodeLens above each construct's signature**, beside the existing Run lens:

```
untrained · Dry-run · Try in session · Promote
drifted   · Dry-run · Try in session · Promote (updates the trained version)
trained   · Demote
seeded    ·                                        (no action; needs a rollout)
```

**A status-bar item** for the active document: `3 untrained · 1 drifted`,
clicking through to a list. This is what makes "I saved but did not promote"
impossible to miss.

**Nothing runs automatically.** No promote-on-save, no train-on-save. The
extension already holds this line for runs (`capabilities.untrustedWorkspaces`
records it: *"nothing ever runs automatically -- a CodeLens renders an
affordance and there is no run-on-save"*), and promotion is a strictly larger
commitment than a run.

---

## 5. Read-only rules

| File | Cluster | Editable |
|---|---|---|
| core engine `dsl/` | any | **no** |
| product bundle `dsl/` | local | yes |
| product bundle `dsl/` | remote | **no** |
| a new file | any | yes -- this is the training path |

The rule underneath: **a file is read-only exactly when editing it cannot
change what the cluster runs.** Core constructs are sealed by the engine's
core-first invariant. A remote cluster's bundle loads from its own image, so
editing a local checkout of it changes nothing there.

Mechanism: the extension manages `files.readonlyInclude` in **workspace**
settings from the selected cluster, and pairs it with a `FileDecorationProvider`
badge so the reason is visible rather than merely felt.

**And the marking is a courtesy, not the control** -- the same doctrine
`src/deploy/actions.ts` states for role tiers. A user can override the setting;
what actually refuses is the engine, because `PromoteAuthoredConstruct` will not
let a promoted construct shadow a core name. The editor explains; the engine
enforces.

---

## 6. The four actions

Each maps to an engine message that exists, except where noted.

| Action | Message | Effect |
|---|---|---|
| **Dry-run** | `AuthoringValidateBundleMsg` | Gate-1 compile+bind in the sandbox; per-construct diagnostics; **zero** engine mutation |
| **Try in session** | `AuthoringSessionDefineBundleMsg` | callable by name for this stream only; dropped at disconnect |
| **Promote** | `DurablePromoteBundleMsg` | persisted, shared, cross-node live, restart-durable |
| **Demote** | `DurableDemoteBundleMsg` | the inverse |

The SDK gains `durablePromoteBundle` and `durableDemoteBundle` to sit beside
the two it already wraps.

Promote is **owner-only** and demote matches it -- stricter than define, which
is owner-or-developer. That asymmetry is deliberate and inherited, not
introduced here.

---

## 7. Concepts become promotable

The anchor. `PromoteAuthoredConstruct` handles the function family and the spec
family and refuses everything else; `isDurablePromotableKind` mirrors the list;
`v1:authoring:construct.kind` has no `concept` value.

### 7.1 Why it is tractable

- **No migration, ever.** Every row lives in one generic hypertable --
  `MemoryNodes(id, "createdAt", "createdBy", schema JSONB, payload JSONB,
  metadata JSONB, "type", concept TEXT)`. A new concept is a new *string value*
  in the `concept` column. No DDL.
- **The registry is already runtime-mutable** -- `memoryNodes.MergeAll`, with
  `CloneDefaultRegistry` giving the sandbox its isolated copy.
- **Gate 1 already compiles candidate concepts**, and
  `TestSandboxCompileBundle_CandidateConceptCompiles` already proves they do not
  leak into the global registry.
- **Concepts with no directory on disk are an existing category** --
  `concept_ids.go` carries a block of "MemQL runtime concepts ... registered
  dynamically (no concept directory on disk)", and `ValidateConceptConstants`
  walks only `AllFilesystemConcepts()`.
- **Persistence, re-hydration and cross-node propagation are kind-agnostic.**

### 7.2 Demote: retire always, remove only when empty

Withdrawing a function makes it uncallable and that is the whole story.
Withdrawing a **concept** would strand rows already written under it, so:

- **Rows exist** -> the concept is **retired**. It stays in the registry marked
  retired, its name stays claimed, existing rows stay readable, new writes are
  refused, and re-promoting un-retires it.
- **Zero rows** -> it is **removed** outright and the name is free again, so a
  concept promoted by typo can be cleanly withdrawn.

Data is never made unreachable by an operation whose name suggests it affects
only a definition.

### 7.3 Re-promote: classify the diff, refuse breaking

Re-promoting a changed concept is a migration wearing a different hat. It is
helped considerably -- not solved -- by `schema` being stored **per row**, so
prior rows carry the schema they were written under and stay valid.

The promote path diffs the candidate against the promoted version and
classifies:

**Additive, lands:** a new optional field; a new `@relationship`; an edited
`@description`; a widened `@enum`.

**Breaking, refused:** a removed field; a changed field type; a **new required
field** (existing mutations do not supply one, and `@default` is never applied
on insert -- memql#2960 -- so `??` in the mutation body is the only mechanism
that would fill it); a narrowed `@enum`.

A refusal names the field, the row count affected, and the constructs that
reference it. `--allow-breaking` is the explicit override for when it is meant.

This follows the house rule memql#3625 set for tools: a silent degrade is not
permissive, it is confidently wrong.

### 7.4 The invariants it still satisfies

- **Core-first, never-shadow**, applied to the concept registry.
- **Reserved namespaces and reserved payload fields** --
  `ensureReservedFieldsNotDeclared`, `concept_reserved_namespace_test.go`.
- **`@rowAuthz` classification.** `test/dslconformance/conformance_test.go`
  hard-fails on an unclassified construct, and the blast radius of a tier is
  measured by declaring it and running the gates, not by reasoning about it.
- **`@relationship` to a core concept.** `type` is a closed set the engine
  owns; what a promoted concept may declare, and in which direction, is stated
  rather than discovered.
- **Node-type invariants** on the `type` column.

---

## 8. Module layout

**Engine**

```
component/memql/authoring_promote_concept.go   the promote branch + registry merge
component/memql/authoring_concept_diff.go      additive vs breaking classification
component/memql/authoring_concept_retire.go    retire / remove, row-count gated
dsl/authoring/concepts.memql                   kind enum gains "concept"; retired state
```

**LSP**

```
cmd/memql-lsp/training.go       per-construct hash + state against the catalog
component/memql/sense/...       the hash, shared with the engine
```

**SDK**

```
sdk/ts/src/authoring/authoring.ts   durablePromoteBundle + durableDemoteBundle
```

**Extension**

```
src/state/training.ts           construct state model                pure
src/constructs/trainingLens.ts  the CodeLens
src/constructs/decorations.ts   gutter + range decoration
src/constructs/readonly.ts      the readonly rules
src/training/actions.ts         dry-run / define / promote / demote
```

---

## 9. Errors

| Condition | Renders as |
|---|---|
| Not connected | state is *unknown*, not *untrained* -- an absent cluster must never read as "the cluster does not have this" |
| Promote refused (not owner) | the engine's refusal naming the role |
| Promote refused (shadows core) | the engine's message naming the core construct |
| Breaking schema change | the classified diff of §7.3, with the override named |
| Demote of a concept with rows | the retire outcome, stating the row count |
| Hash parity failure | a loud, distinct message -- never "everything is drifted" |

---

## 10. Relationship to the other sub-projects

**Depends on A** (memql#3747) for `ListConstructs`, `sourceHash` and `origin`.
Without them there is no way to ask what the cluster knows, and the whole of
§3 is unimplementable.

Inherits **C**'s boundary rule (memql#3733): authoring is a machine-side act,
so it lives in the editor rather than the portal.

Independent of **D**. A promoted construct is per-database, so under D's
two-schema model staging and production are trained separately -- which is the
correct behaviour and needs no coordination between the two designs.
