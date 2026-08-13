# Concept relationships: a concrete, gated, product-safe edge model for v1

**Date:** 2026-08-13
**Status:** Approved design, pending implementation
**Scope:** `@relationship` declarations, the relationship load path, and relationship traversal

---

## Why now

memQL is being cut as v1. The `@relationship` construct was an initial
implementation that was never revisited, and it is the last major DSL surface the
`memql#3631` hardening sweep did not cover.

It carries the same defect shape that sweep keeps finding -- **declaration
theatre**: something that parses, is then discarded, and leaves the author
believing it is in effect. Relationships have three instances of it, two of which
silently corrupt data rather than merely doing nothing.

The stakes are set by platform consolidation (`memql#2472`). This repo ships a
**product-agnostic engine**; client repos mount their own DSL at
`MEMQL_DSL_PATH`. The DSL surface is therefore a **public API for downstream
repos**, and the absence of a construct's user in this tree is not evidence of
absence -- it is the expected state. Two consequences run through this entire
design:

1. **A gate that is a Go test over `dsl/` protects this repo and nobody else.**
   That is `memql#3629`. Relationship validation must live in the engine's load
   path, which is the path a mounted bundle also takes.
2. **Anything frozen at v1 is frozen for every downstream repo.** A closed
   vocabulary the engine owns cannot be extended by a client without an engine
   release from us.

---

## The problem

### Finding 1 -- `type` is a closed enum that drives no runtime behavior

`canonicalRelationshipType` (`component/memql/relations.go:51`) hard-rejects
anything outside nine values: `parent`, `alias`, `equals`, `interactsWith`,
`contains`, `owns`, `createdBy`, `dependsOn`, `formedFrom`. It is not the dynamic
string it is often assumed to be -- it is the opposite.

Yet only two runtime consumers read relationships at all:

- `canonicalizeRelationshipFields` (`component/memql/partition_context.go:160`) --
  canonicalizes the foreign key to `{concept}:{shortId}` **on write**.
- `canonicalizeRelationshipFieldValue` (`component/memql/executor_filter.go:761`)
  -- the same **on read/filter**.

Both select on `direction == "outgoing"` plus `field` plus `target`. **Neither
reads `type`.** All 143 live declarations behave identically whether they say
`parent` or `interactsWith`.

`type` is otherwise consulted in exactly two places, both in
`component/memql/engine_bootstrap.go:121-125`: a `collection` concept must declare
`contains`, and a `reference` concept must declare `alias` or `equals`. There are
zero `collection` and zero `reference` concepts in this tree, so neither check
fires here.

Net: `type` is validated at load, published in the concepts API
(`component/memql/engine.go:1099`), consumed by traversal, and inert for every
live declaration.

### Finding 2 -- the closed enum has a boot-refusing failure mode, and it has fired twice

Two commits exist solely to add a word to that switch statement:

```
d45efaa2 fix(engine): register `formedFrom` relationship type -- harness semanticMemory load
42aeff3b fix(engine): register `dependsOn` relationship type -- unblocks harness concept load
```

`component/memql/relations_test.go:5` records what happened: *"the engine rejected
it at bootstrap before, taking the whole cluster down -- identity-first."*

An unrecognized verb is not a soft failure. It is a **boot refusal that takes the
mesh down**, and the only remedy is editing Go and cutting an engine release. A
client repo that invents its own verb is blocked on our release cycle, with their
cluster down. This is the single strongest argument in the design.

### Finding 3 -- an unresolvable target silently drops the edge

`component/memql/engine_bootstrap.go:80-90` skips a relationship whose target is
not in the local registry, emitting only a `Debug` log. Its justification is a
comment about `@visibility` filtering -- **a feature that no longer exists** (zero
uses in `dsl/`; `component/memql/unified_loader.go:68` confirms every binary loads
every concept).

The upstream half is equally soft: `resolveRelationshipTargets`
(`component/memql/unified_loader.go:265-290`) emits `logger.Warn` for a bare name
it cannot resolve and leaves the unresolved string in place.

So a typo or a missing `use` import produces two soft logs and a relationship that
does not exist. Because the relationship is what drives id canonicalization, ids
then persist in non-canonical form, and `(concept, id)` lookups quietly return
nothing. **This is the highest-severity finding: it silently corrupts data.**

This tree is protected by `TestRelationshipTargetsResolveToKnownConcepts`
(`component/memql/relationship_target_resolution_test.go:20`). A mounted product
bundle is protected by nothing.

### Finding 4 -- `field` is never validated against the concept

Nothing checks that a relationship's `field` is a field the concept actually
declares. `deriveRelationshipFieldSource` (`component/memql/engine.go:1385`) only
asks whether the first segment is a *reserved* name, in order to choose
table-vs-payload source.

So `field="agenId"` loads clean. The write-path canonicalizer then does an exact
`payload[field]` lookup (`component/memql/partition_context.go:170`), misses, and
continues. Same blast radius as Finding 3, and it is the likeliest authoring
mistake there is.

### Finding 5 -- write and read disagree on case

Write path: exact map lookup (`payload[field]`).
Read path: `strings.EqualFold(rel.Field, fieldName)`
(`component/memql/executor_filter.go:766`).

`field="AgentId"` against a payload key `agentId` therefore canonicalizes on
filter but not on write -- an edge that half-works, which is worse than one that
does not work at all.

### Finding 6 -- `interactsWith` is a junk drawer

46 of 143 live declarations (32 percent); `parent` is 90 (63 percent). Together 95
percent. `interactsWith` carries no information beyond "this row points at that
row", so `participant.agentId -> agent` and `participant.forUserId -> user` are
two identical shrugs with no way to express how they differ.

### Finding 7 -- three vocabularies describe one thing, and two are wrong

Runtime emits these `GraphEdge.type` values
(`component/memql/executor.go:1046-1178`, `component/memql/graph_bundle_builder.go:10`):

`child` `alias` `equals` `interactsWith` `createdBy` `contains` `owns`

Against that:

- **`docs/public/language/memql.md:101` is wrong.** It documents `aliases`
  (actual: `alias`) and `interactions` (actual: `interactsWith`), and omits
  `equals`. A client parsing `bundle.edges` per our public docs looks for
  `interactions` and never matches. This is a shipped defect in a v1 client
  contract.
- **`component/language/ast/ast.go:1771`'s comment is wrong.** It lists `parent`,
  which is never emitted; only `child` is.

The drift is possible only because the values are string literals at seven call
sites with no single source of truth.

### Finding 8 -- the traversal surface is live, recently buggy, and barely covered

`component/memql/executor_relationship.go` is 826 lines implementing nine
traversal functions (`component/language/ast/ast.go:92-105`). There are **zero**
uses across `dsl/`, `clients/`, and `editors/`. It is reachable only via runtime
query strings over gRPC / WebSocket / MCP -- and by client repos, which is the
point.

It is not dormant. `memql#3432` -- *"Incoming relationship lookups cap results at
`len(sourceIds)`, so one parent can return at most one child"* -- was a severe
correctness bug fixed on 2026-08-09. Coverage is two db tests, both regressions
(`memql#3397`, `memql#3432`).

`direction` is `outgoing` for all 143 live declarations. `incoming` has one test.
`bidirectional` is accepted by the normalizer and is used and tested **nowhere**,
yet would freeze at v1.

### Finding 9 -- the authoring reference teaches an invalid type

`dsl/_reference/_concept.memql:659` uses `type="references"`, which
`component/memql/relations_test.go:37` explicitly pins as **invalid**. It also uses
the retired quoted `target="v1:identity:user"` form instead of the live
bare-identifier plus `use` import form (`memql#1067`).

The root cause is structural: `_reference/` is walker-skipped
(`core/dslfs/walker.go:40`), so nothing ever parses it. Fixing the file without
fixing that guarantees it rots again.

---

## The design

### The model: two independent axes

Every `@relationship` carries two axes that the current `type` field conflates.

**Axis 1 -- `type`: what the engine does with the edge.** Closed, engine-defined,
frozen at v1.

| type | Engine behavior | Live count |
|---|---|---|
| `parent` | hierarchy / lifecycle; traversable both directions (`parentOf`, `childOf`) | 90 |
| `owns` | ownership; `owns` traversal | 3 |
| `createdBy` | provenance; the only type permitted a metadata field source (`engine.go:1306`) | 2 |
| `alias` / `equals` | identity equivalence; `reference` node type | 0 here |
| `contains` | containment; `collection` node type | 0 here |
| `interactsWith` | **default** -- plain foreign-key edge. Id canonicalization only | 46 |

Zero live count is not a reason to cut: client repos author their own concepts
against this same closed set, and a type removed at v1 is a type they cannot use
without an engine release.

**Axis 2 -- `as`: what the edge means.** Open, author-defined, never frozen.

```memql
@relationship(type="interactsWith", as="respondsAs", field="agentId",   target=agent, direction="outgoing")
@relationship(type="interactsWith", as="actsFor",    field="forUserId", target=user,  direction="outgoing")
```

- Validated for **form only** -- identifier shape (`^[a-z][a-zA-Z0-9]*$`) and a
  length cap -- and **never** for membership. That non-check is the entire point:
  the moment `as` acquires a list, the treadmill of Finding 2 is rebuilt.
- **Optional on every type**, including structural ones
  (`type="parent" as="belongsToSpace"` is legal and useful).
- Absent `as` means "unlabeled", which is what all 143 current declarations
  become. **Nothing in this tree or any client tree must change to keep working.**

Where a label matches multiple edges on a concept, traversal returns their
**union**. This is deliberate: the existing per-field duplicate rule
(`engine.go:1336`, keyed on concept plus field plus fieldSource) already prevents
the genuinely ambiguous case, and union is the useful behavior for "every edge
meaning *assignedTo*".

An unknown `type` still refuses boot -- consistent with the strict-boot convention
-- but the error becomes an instruction rather than a dead end:

```
concept "v1:acme:ticket" relationship[0]: unknown structural type "assignedTo".
  Structural types are: parent, owns, createdBy, alias, equals, contains, interactsWith.
  For a domain verb, use: type="interactsWith", as="assignedTo"
```

That message is what makes the cluster-down class of failure unreachable.

### Breaking change: `dependsOn` and `formedFrom` become labels

They were never structural. Their own registration comments
(`component/memql/relations.go:26-39`) state that graph-expansion traversal is
deliberately unwired and the field is read directly. They exist as structural
types only because, at the time, there was no other way to say the word.

They become `type="interactsWith" as="dependsOn"` and
`type="interactsWith" as="formedFrom"`. Two declarations change in this tree.

**A client repo using `type="dependsOn"` gets a boot refusal after this change.**
It is loud, it names the fix, and it is a one-line edit. Pre-release is the only
cheap moment for it, and the alternative -- silently normalizing the old spelling
at load -- is precisely the quiet compatibility layer CLAUDE.md forbids.

### The load-time gate

Every invariant is enforced by the **engine, at load**, in the path that also
mounts `MEMQL_DSL_PATH`. Not by a Go test over `dsl/`.

**Retained** (already in `normalizeRelationshipDefinition`, `engine.go:1284`):
type membership, non-empty field, `fieldSource` rejection, non-empty target,
direction validity, `contains` plus `incoming` rejection, per-field duplicate
detection, reverse-direction consistency, node-type invariants.

**Three silent failures become hard errors:**

1. **Unresolvable target fails the load.** Remove the `@visibility`-justified skip
   at `engine_bootstrap.go:80-90` and promote the `unified_loader.go` warning to
   an error.
2. **`field` must be a declared field on the concept.** New check, consulting the
   concept's own field set.
3. **Case handling is unified** on exact match against the declared field,
   enforced by (2), with the read path brought into agreement.

**New:** `as` form validation.

Error messages must carry the fix, because a client repo's author cannot read our
Go:

```
concept "v1:acme:ticket" relationship[1]: target "usr" is not a registered concept
  Did you mean "user"? Add a file-top import: use identity.concepts.{ user }
```

**Pinned, not changed:** `checkRelationshipDirection` (`engine.go:1358`) keys on
concept-pair plus type plus fieldSource, so two edges linking the same pair
through different fields overwrite each other in the tracker and only the last is
direction-checked. Narrow, but exactly the shape that hides a bug. Cover it.

### Traversal

**Label-scoped traversal.** `as` must be readable from a query or it is
write-only metadata -- the original mistake in a new field.

```
interactsWith(<expr>)                    // all interactsWith edges (unchanged)
interactsWith("assignedTo", <expr>)      // only edges labeled assignedTo
```

Arg-count discrimination is already the established pattern here
(`component/language/dslspec/builtins.go:293` notes `contains` is discriminated
exactly this way), so this introduces no new grammar concept. The label-scoped
form applies uniformly to every traversal function, including structural ones
(`parentOf("belongsToSpace", <expr>)`).

**`as` rides the wire.** `GraphEdge` (`component/grpc/memql.proto:751`) carries
`type`, `from_id`, `to_id`, `depth`. Add `as` as field 5 so a client can
distinguish labeled edges in a result bundle. Additive and backward compatible,
but it is a wire-contract change and needs a frontend-team callout in the commit
body per the CLAUDE.md convention.

**One source of truth for edge labels.** Export a single constant set that the
executor emits from, the docs generate from, and a test pins. Fix
`memql.md:101` and the `ast.go:1771` comment against it.

**`bidirectional` gets a decision.** It is a v1-frozen value with zero
declarations and zero tests. Gating it means writing its semantics down for the
first time. If coverage shows it does not cohere, cutting it before v1 is far
cheaper than after.

### Migration and docs

- `dependsOn` / `formedFrom` migrate to labels (2 declarations).
- The 46 `interactsWith` declarations are labeled **selectively, not wholesale**.
  Unlabeled remains legal, so this is opt-in value. The payoff concentrates where
  a concept carries multiple `interactsWith` edges that are currently
  indistinguishable: `cognition/participant` (2), `campaigns/campaign` (3),
  `library/artifact` (4).
- `dsl/_reference/_concept.memql` is corrected, **and** a test is added that
  parses the reference skeletons despite the walker skip. Authoring reference
  material no test reads has a half-life.
- `authoring-rules.md`, the CLAUDE.md relationship section, the `dslspec` lexicon,
  and `sense` (LSP completion and hover) all learn `as=` and the label-scoped
  traversal signature. Without this the feature exists but nothing helps an author
  discover it.

### Testing

**The load gate is tested through a mounted bundle.** A test that loads a
synthetic tree from a temp dir via `MEMQL_DSL_PATH` and asserts each invariant
*fails the load* is the only thing that proves a client repo is protected.
Testing against `dsl/` proves only that our own tree is clean, which is the exact
gap in `memql#3629`.

**Traversal coverage matrix:**

| Axis | Values |
|---|---|
| Function | `parentOf`, `childOf`, `aliasOf`, `equals`, `interactsWith`, `contains`, `owns`, `createdBy`, `ids` |
| Direction | `outgoing`, `incoming`, `bidirectional` (currently untested) |
| Label | unlabeled, labeled-hit, labeled-miss |
| Field shape | scalar, `[]string` array |
| Cardinality | one-to-one, one-to-many (the `memql#3432` regression), many-to-one |
| Ids | plain, clustered / versioned (the `memql#3397` regression) |

Plus the read/write asymmetry as an explicit case: an array-valued relationship
field canonicalizes on write (`partition_context.go:185`, `case []any`) but the
filter path early-returns on non-strings (`executor_filter.go:753`). Fix it or
document it, but stop leaving it accidental.

**Round-trip:** one test carrying a labeled relationship the whole way --
declaration, load, write-path canonicalization, filter-path canonicalization,
traversal, `GraphEdge` on the wire. Every silent failure found here lived in a
*gap between* two stages, so the seams are what need coverage, not the stages.

---

## Decisions log

| Decision | Chosen | Rejected, and why |
|---|---|---|
| Vocabulary model | Closed structural `type` plus open `as` label | **Bigger closed enum** -- freezes a guess and keeps the treadmill. **Fully open `type`** -- the engine loses the ability to reason about edges structurally, which per-row authz inheritance and cascade will need. |
| `interactsWith` name | **Rename to `references`** (memql#3663) | Initially recorded as "keep it", on the grounds that renaming touched 46 sites for no semantic gain once `as` carried the meaning. Reversed on the counter-argument that was already on record: pre-release is the cheapest moment this will ever be, and every downstream bundle that adopts `interactsWith` before v1 raises the price. `references` rather than `reference` because `NodeTypeReference` already claims `"reference"` as a concept *node type* (`memory-nodes/constants.go:8`). |
| Label syntax | Explicit `as=` kwarg | **Open `type` with reserved words** -- more ergonomic and removes the boot-refusal entirely, but re-merges the two axes and turns a typo into a silent custom label. **Both** -- two ways to say one thing ages badly. |
| Zero-user types (`alias`, `equals`, `contains`) | Keep and gate | Cutting them applied YAGNI on a false premise: this is a product-agnostic engine and client repos are the users. Absence here is the expected state, not evidence. |
| Traversal surface | Keep and properly gate | Cutting it guts a headline capability of a self-described graph database. Undocumenting it leaves known-buggy code reachable, which is how `memql#3432` happened. |
| `dependsOn` / `formedFrom` | Clean break to labels | Silent normalization is the compatibility layer CLAUDE.md forbids. |
| Duplicate labels on a concept | Allowed; traversal unions | The per-field duplicate rule already blocks the ambiguous case. |

---

## Issue breakdown

Tracked under epic **memql#3651**, sized for parallel claiming.

| Issue | Title | Labels |
|---|---|---|
| memql#3652 | `as` label -- open domain vocabulary on `@relationship` | `dsl` `engine` `enhancement` |
| memql#3653 | Unresolvable relationship target silently drops the edge | `engine` `bug` `reliability` |
| memql#3654 | `field` is never validated against the concept; write and read disagree on case | `engine` `bug` `reliability` |
| memql#3655 | Retire `dependsOn` / `formedFrom` as structural types | `dsl` `engine` |
| memql#3656 | Label-scoped traversal plus `as` on the `GraphEdge` wire | `engine` `dsl` `enhancement` |
| memql#3657 | Edge-label vocabulary: one source of truth; public docs are wrong | `engine` `documentation` `bug` |
| memql#3658 | Traversal coverage matrix; decide `bidirectional` before it freezes | `engine` `dsl` |
| memql#3659 | Relationship load gate must fire for a mounted product bundle | `engine` `reliability` |
| memql#3660 | `_reference/_concept.memql` teaches an invalid type | `dsl` `documentation` |
| memql#3661 | `sense` and `dslspec` support for `as=` and label-scoped traversal | `dsl` |
| memql#3663 | Rename the `interactsWith` relationship type to `references` | `dsl` `engine` |

**Ordering.** memql#3653, memql#3654, and memql#3659 are the correctness spine;
they are independent of the feature work and can ship first and stand alone.
memql#3652 gates memql#3655, memql#3656, and memql#3661. memql#3657 and
memql#3660 are independent cleanups claimable by anyone. memql#3663 comes last
of all -- it needs memql#3652, memql#3655, and memql#3657, and must land as one
atomic PR, because a half-renamed vocabulary is worse than either name.
