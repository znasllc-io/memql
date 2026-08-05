---
title: Per-row authorization audit
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Per-row authorization audit

> **Status:** Framework + initial gap closure shipped 2026-05-20.
> The 11 constructs flagged *at that time* were closed via `@public`
> with per-construct comments documenting the intent + the follow-up
> tightening path. The classification test
> (`dsl.TestPerRowAuthzClassification`) hard-fails on any new
> flagged construct.
>
> That "11" is history, not an inventory. The live figures move, and
> this document does not carry them: regenerate with
> `go test ./dsl/ -run TestPerRowAuthzClassification -v`. The `#54`-era
> counts and tables that
> used to sit further down have been **deleted rather than corrected**
> (memql#2983) — see "Counts: regenerate, do not read".

## Context

This framework was written while memQL still relied on
**partition-as-isolation-boundary** for defense-in-depth: a request
authenticated as user X could only read rows under partition X, so a
DSL query with a bug still had the partition boundary under it.

**That boundary is gone.** Issue #56 removed partitioning; only its
phase-8 cleanup remains. So the per-row check in the DSL is not a
belt-and-braces addition ahead of a future change — it is the *only*
gate, which is what the rest of this document is about.

The `PartitionACL` middleware this section used to cite, in
`component/auth/access/middleware.go`, no longer exists: neither the
symbol nor that directory is in the tree. Nothing derives scope from
the request envelope any more — the `partition` wire field is
`reserved` in `component/grpc/memql.proto`.

## The buckets

Conceptually there are four. The classifier reports **six states**, and
it checks `serverOnly` **first** — a `@serverOnly` construct is not
client-callable, so no caller-check applies and it is never flagged.

| Bucket | Definition | Required gating |
|---|---|---|
| **owned** | Row carries `ownerUserId` (or `userId` for identity-domain concepts) | `filter` must include `ownerUserId == actor.userId` (the caller can only read rows they own) |
| **granted** | Row visible via a relationship (e.g. space participant, group member) | Filter must reference a relationship spec that gates on `actor.userId` |
| **admin** | Cluster-owner-only (e.g. audit log, identity admin views) | A top-level conjunct `actor.isClusterOwner == true`, or an admin context-spec |
| **public** | Globally readable by intent (concept catalogs, role registry, public lookup tables) | `@public` annotation on the construct |

The two reported states that are not buckets: **`srvOnly`**, checked
first as above, and **`other`** — everything the classifier did not
place. `other` is by far the largest column and is not a finding.

> **Payload fields are BARE.** The `payload.` prefix this table used to
> prescribe was retired by memql#2292 and is hard-failed by
> `dsl/conformance_test.go`; a filter written that way does not load.
> Row intrinsics take the `row.` namespace (`row.id`, `row.createdAt`).

> **`granted` is not implemented.** The classifier has no counter for it
> and does not resolve relationship specs, so a granted construct lands
> in `owned` or `other` depending on its filter. The row above is the
> authoring rule, not something the gate measures.

> **There is no `requiresClusterOwner` spec.** This table used to say
> "compose `spec("requiresClusterOwner")`", and neither half was live:
> the spec is declared nowhere in `dsl/` (`dsl/admin_gate_test.go` says
> so outright), and `spec("...")` is the retired stringly form. The
> engine rejects it in `component/memql/ast_converter.go`, naming the
> replacement as the predicate form `spec <name>` — no quotes, no
> parens — which in a filter is the spec written as a top-level
> conjunct.
>
> **Do not take the list of live context-specs from here.** That is the
> kind of hand-written inventory the rest of this document was rewritten
> to stop carrying, and it drifts the same way. Read it off the tree:
>
> ```
> grep -rn '^spec actorEnvelope ' dsl/
> ```
>
> Worth knowing why that is not a formality: the pairing is not the
> obvious one. `requiresOwnerOrAdmin` lives in `dsl/common/specs.memql`
> beside `requiresAdmin` — not in `dsl/deployment/specs.memql` beside
> `requiresOwner` — having moved there in memql#2800. #2983 asserted the
> deployment pairing, the first correction copied it unchecked, and both
> were wrong. `requiresDeveloperOrAbove` is a fourth live
> `actorEnvelope` spec that neither list mentioned; note it is absent
> from the admin recogniser, so it is a role gate the admin classifier
> does not see.
>
> `requiresClusterOwner` survives inside that recogniser as a #54
> placeholder, which is why the retired spelling still reads as real
> from the outside.

The `@public` annotation is a marker for the validator — it has no
runtime effect. Adding `@public` to a construct is the author's
explicit acknowledgement that "yes, this is meant to be visible to
unauthenticated callers / cross-user reads / etc."

## Declaring the tier: `@rowAuthz` on the concept

The buckets above are inferred, per construct, by a text scanner
**plus** a boolean-structure check on the filter clause — not by a
regex alone; a bare pattern match would accept a caller-check sitting
under a top-level `||`, which gates nothing. That is the defect #2803
rules on: a construct's caller-check lives in a
term the author has to remember to type, and deleting it leaves a
construct that still loads, still passes every gate, and returns
every row. **Absence has no syntax**, so review cannot catch it.

`@rowAuthz(...)` moves the bucket onto the **concept**, declared once,
where a new query over that concept inherits it instead of having to
restate it:

| Bucket | Declaration | Predicate it will eventually inject |
|---|---|---|
| owned | `@rowAuthz(owner="<field>")` | `<field> == actor.userId` |
| owned (self) | `@rowAuthz(owner="id")` | `row.id == actor.userId` |
| admin | `@rowAuthz(clusterOwner)` | `actor.isClusterOwner == true` |
| public | `@rowAuthz(public)` | none — explicit and greppable |
| granted | `@rowAuthz(via="<spec>")` | the relationship spec, gated on `actor.userId` |

```memql
@rowAuthz(owner="ownerUserId")
concept note { ... }        // dsl/notes/concepts.memql

@rowAuthz(clusterOwner)
concept call { ... }        // dsl/telephony/concepts.memql
```

Both examples are tree-true. The first used to read
`@rowAuthz(owner="requestedBy") concept plan`, and `planner.plan` is
**undeclared** — deliberately, because `requestedBy` is caller-supplied
by `createPlan`, which is the write-path hazard #2982 is about and the
reason this same document argues at length that declaring it `owned`
would be wrong. Illustrating the annotation with the one concept the
rest of the page uses as a counter-example was memql#2984.

Rules:

- **One tier per concept.** Two different tiers inside one annotation
  is a load error, and so is a second `@rowAuthz` on the same concept
  (the parser folds attributes in source order, so without that check a
  reader scanning top-down would see a tier the engine does not use).
- **`owner="<field>"` must name a field the concept declares**, with the
  one exception below. Checked at load, against the parsed property set,
  with the declared fields listed in the diagnostic.
- **`owner="id"` is the SELF-OWNED form** (memql#3029), for a concept whose
  owner is the row's own identity — `v1:identity:user` is the motivating
  case, since a user has no `ownerUserId` to name. `id` is a row intrinsic
  rather than a payload property, so it is admitted explicitly rather than
  by the property lookup. **`id` and only `id`:** `createdBy` is the
  dangerous near-miss — it means "who WROTE the row", not "whose row it
  is", so a row an admin creates on a user's behalf has a `createdBy` that
  is not the owner. Admitting it would let a concept declare an owner tier
  that is false, which is the class this gate exists to catch.
- **`public` is spelled explicitly.** "No annotation" and "declared
  public" are different states — making absence stop being silently
  permissive is the entire point.
- The parameterised tiers use `=` and a quoted value, matching every
  other keyword-arg annotation (`@relationship(field="x")`,
  `@displayCard(primary="name")`).

### Status: Phase 1 is inert (memql#2920)

**Nothing is enforced.** The tier is parsed, validated, and carried on
the concept; no predicate is injected anywhere and no query returns a
different row set than it did before. `TestRowAuthzIsInert` enforces
that by walking the Go tree and failing if any file outside the
allow-list reads the row-authz surface.

**The allow-list is not reproduced here.** Read it from the gate:

```
sed -n '/Files permitted to reference/,/^\t}/p' \
  component/database/memory-nodes/concept_rowauthz_test.go
```

It is longer than a sentence suggests, and the block comment the range
starts at is the justification for the whole list -- which a paraphrase
drops. (The range deliberately starts at that comment, not at the map
literal: a version of this command that started at `allowed :=` printed
the entries and discarded the reasoning, which is the very thing this
paragraph says a paraphrase loses.) This
document twice carried a hand-written version of that list: once a phase
behind (it named only the detector, loader and codemod), and once
corrected to a version that was still three files short on the day it
shipped. The second is why this points at the source instead
(memql#2984). Enforcement arriving without a decision is what the gate
exists to catch, so what counts as permitted is the gate's to state.

A concept with no declaration still loads. It produces one aggregated
boot **warning** naming every undeclared concept; escalation to a load
error is a later phase, once the tree is clean.

### Shadow mode (memql#2921)

Phase 2 computes the predicate a declaration *would* inject, decides
whether the author's filter already implies it, and records a verdict.
It still enforces nothing.

```bash
# The measurement. Prints the full table; changes nothing.
go test ./component/memql/ -run TestRowAuthzShadowReport -v

# Runtime instrumentation on a live node. Off by default.
MEMQL_ROWAUTHZ_SHADOW=1 <node>
```

Three verdicts, and `undecidable` is a first-class one:

| verdict | meaning |
|---|---|
| `already-implied` | the filter already guarantees the predicate; enforcement is a no-op here |
| `would-narrow` | enforcement would remove rows this access returns today |
| `undecidable` | implication cannot be decided statically — enforcement would change this blindly |

Implication is decided on **top-level conjuncts of the parsed AST**. A
term under a top-level `||` does not imply the predicate (memql#2832),
and anything the analyzer does not positively understand — an
unexpanded spec, a relationship traversal, a builtin — is `undecidable`
rather than assumed. An overstated blast radius misleads a ruling as
badly as an understated one.

Two implementation constraints worth knowing before changing it:

- **The analyzer must run before `resolveActorReferences`.** That
  function substitutes `actor.userId` with the caller's concrete id, so
  measuring the resolved form reports `would-narrow` for every
  construct that already hand-writes the term — inverting the whole
  measurement. `TestShadowMustSeeUnresolvedActorReferences` fails if
  the hook moves.
- **A loaded query's `Expr` is the whole read pipeline**,
  `shape(paginate(sort(<filter>)))`, not the filter. The analyzer peels
  those wrappers; they change projection, ordering and windowing, never
  which rows match.

**Graph expansion has its own hook**, because `expandGraph` traverses
relationships from a row the caller already has and never reaches the
filter path at all (#2803 design decision 3). It has no filter, so a
traversal into a narrowing-tier concept is `would-narrow` by
construction.

#### What the measurement currently says, and its limit

Over the declared set the result is **33 of 33 already-implied**, and
that is **tautological**: Phase 1's codemod declared a tier only where
every query over the concept already carried the term as a top-level
conjunct, and shadow mode asks the same question. It is still worth
having as a **cross-validation** — Phase 1 decided textually on blanked
source before load, this analyzer decides structurally on the AST after
load, and the two agreeing on all 33 is evidence they encode one rule.
`TestRowAuthzShadowReport` fails on any `would-narrow` over the
declared set for exactly that reason: it would mean the two disagree.

What the **declared** set does not produce is a blast radius — over
those concepts the analyzer agrees with Phase 1 by construction, so the
answer is tautological. The blast radius lives in the constructs over
the concepts that declare nothing, where no predicate can be computed
from a tier that was never stated. That is what the report's
`HYPOTHETICAL TIERS` section exists to estimate, so "shadow mode gives
you no blast radius" is too strong and this paragraph used to say it
(memql#2984). The concepts
graph expansion actually walks into — `v1:identity:user` (46 inbound
relationships), `v1:agents:agent` (19), `v1:planner:plan` (11) — are
all in that undeclared set.

### The write path: a declared owner tier is gated (memql#2982)

`@rowAuthz(owner="F")` asserts that F identifies the row's owner. That
assertion is worthless if a caller can write F — and worse than
worthless, because it is false in the direction that reads as safe: an
auditor who sees a declared owner stops looking.

`TestDeclaredOwnerFieldsAreServerStamped` gates it. Every concept
declaring an owner tier must have that field stamped from
`actor.userId` and unwritable from caller args through **any** mutation.

The check derives from the **loaded `MutationTemplate`**, not from
scanning `accept { ... }` blocks, because the source spelling and the
runtime behaviour are different questions:

- `appendDocumentVersion` wrote a bare `args.ownerUserId` mirror with
  no `accept` block anywhere. That was **memql#2989**; it stamps from
  `actor.userId` now, but the shape stays expressible and a source
  scanner would still miss it.
- `updateCalendarEvent` splatted `args.payload` with **no overlay**, so
  the field was caller-writable without appearing near an `accept`
  block. That was **memql#2988**, a live defect on a concept that
  declared the tier and whose field doc called it "the load-bearing
  per-row authz guard". Fixed by re-stamping the owner in the update
  block, which is what puts it in `PayloadOverlayTemplate` where
  memql#401's overlay-wins precedence engages.

**If a mutation splats a caller-supplied payload, it must re-stamp every
authz-relevant field explicitly.** The hazard is not that the
create-time stamp fails to carry over — on an `update`, a partial
read-merge preserves any field the payload omits. It is that a splat
lets the caller *explicitly name* the field, and only an overlay entry
displaces what they named.

`updateNote` is the contrast, and the difference is narrower than it
looks: it is an `insert`-kind mutation where the caller threads the full
merged payload, while `updateCalendarEvent` is an `update`-kind partial
read-merge. What matters is not the kind but that `updateNote` carries
an explicit `ownerUserId: actor.userId` line, which is what puts the
field in the overlay.

**The exemption list is empty.** It held two concepts —
`library.generatedOutput` and `library.documentVersion`, both
**memql#2989** — whose field docs claimed edits "run server-side on the
owner's behalf" while their three mutations took `ownerUserId` from
caller args, with nothing enforcing the claim.

Both were fixed rather than annotated. The `@serverOnly` route looked
like the honest one and was built and **refuted**: `@serverOnly` is
enforced as `fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal()`,
so annotating requires the callers to stamp internal origin — and all
five run on **request-derived** contexts (an agent turn driven by a user
utterance, a library edit handler, a workbench dispatch inside an agent
turn). Internal origin is the only thing that opens the `@serverOnly`
gate at all, so stamping it there would open *every* server-only
construct for the remainder of that request.
`TestOnlyAllowlistedPackagesStampInternalOrigin` catches exactly this.

The actual fix was three `stamp { ownerUserId: actor.userId }` lines
**plus a correction to the synthetic-actor helper they depend on**, and
the second half is the part worth reading.

The original change rested on the claim that every call site already ran
the mutation under the owner's actor, because `withUserActor` stamps
`sub: ownerUserId`. That claim was false, and the review measured it:
`actor.userId` does not resolve from claims. It resolves from the
**AccessContext** (`resolveActorReference` -> `auth.AccessFromContext`),
and `withUserActor` set only claims + TokenInfo. So the stamp resolved to
the **inbound caller** — or to `""` on a detached context, because
`ActorEnvelopeValue` returns `("", true)` for a nil AccessContext rather
than an error, meaning the row was written and the call SUCCEEDED.

That is the same failure `contextWithSystemActor` is warned about at
`component/server/server.go:396-402`, reached from the other direction.

The five byte-identical copies of `withUserActor` are now one helper,
`auth.ContextWithUserActor`, which binds all three surfaces — claims and
TokenInfo (read by `createdBy` and the mutation-actor check) **and** the
AccessContext (read by `actor.*`). With that in place the original claim
holds: no written value changes, and the field stops being forgeable.

**Scope of that guarantee.** It covers the named-mutation surface. It
does not cover raw `insert(...)`, which short-circuits the planner
(`component/memql/parser.go:520`) and bypasses `args` / `accept` /
`stamp` entirely; only three concepts carry a per-concept Go guard on
that path, and neither library concept is one of them. Tracked as
**memql#3059**.

One edge is load-bearing and now asserted: `auth.ContextWithUserActor`
returns the context **unchanged** for a blank owner, so a write on that
path would be stamped with whatever actor the inbound caller carried. All
five sites refuse before reaching the mutation — the two library handlers
error, the three promotion paths return early — and every one of those
guards now trims whitespace, matching the helper, so a whitespace-only
owner cannot slip past the guard and then no-op inside it.

An empty exemption map means every declared owner tier in the tree is
server-stamped. That is the precondition **memql#2803** records for
ruling on read-time enforcement: a tier over a caller-writable field
would be enforcement the attacker sets, which is strictly worse than none
because a declared tier stops an auditor looking.

Two rules if you are tempted to add an entry back: **fix it instead if
you can**, and if you must exempt, file the decision first and reference
it in the map. An entry without one is how a gate turns into decoration.

### Seeding the tree

```bash
memqlmigrate --rewrite=row-authz -w dsl/*/concepts.memql
```

The codemod infers a tier from how a concept's existing queries filter,
and it is deliberately conservative — it declares **only** `owned` and
`clusterOwner`, the two tiers evidenced by a top-level filter conjunct
that demonstrably narrows the row set.

**A tier is a floor, so every query has to clear it.** The predicate
will eventually be AND-ed into *every* access of the concept, which
means a sibling query carrying no caller-scope term is not a neutral
bystander — it is a counterexample, reading rows the floor would
exclude. One such query blocks the declaration. (Counting only the
positive votes declares `planner.plan` owned off 2 of its 10 queries
while the primary user-facing read is space-scoped, and declares
`library.artifact` owned when `libraryWorkspaceLiveSources` documents
its rows as having no owner at all.) The single exception is
`@serverOnly`, which is not a client-callable read.

It never infers:

- **`public`**, because no filter can evidence a widening claim. The
  nearest candidate, a construct-level `@public`, answers a different
  question ("this *call* is intentionally unscoped") and carries no
  runtime semantics, so promoting it would re-create exactly the silent
  permissiveness this tier exists to end.
- **`granted`**, because resolving the relationship spec is the phase
  that actually computes predicates.

**What it does not examine, stated rather than silent.** The inference
reads *queries*. A mutation cannot vote — `actor.userId` there is a
stamped value (`ownerUserId: actor.userId`), recording who owns a new
row rather than which rows the construct may reach — and it must not
block either: an ungated `update { id: args.x }` is the gap #2803
exists to close, not evidence that a concept's rows are unowned. (That
asymmetry with queries is the point. An unscoped *query* returns other
users' rows by design; an ungated *update* just says "update the row I
name.") #2803 sequences mutations after reads for the same reason. So a
concept whose mutations contradict its queries is not detected here;
measuring that is Phase 2's job. The codemod prints this limit at the
end of every run.

An undeclared concept is a **visible state**, not a gap: it is what
the Phase 2 shadow-mode measurement (#2921) has to cover, and a guessed
tier would launder an absence of evidence into a declaration the
measurement then treats as ground truth.

Current distribution over `dsl/`: 12 `owned`, 1 `clusterOwner`, 87
undeclared. The undeclared break down as 50 blocked by a query whose
filter does not gate on the caller, 26 with no queries at all, 6
blocked by a `@public` sibling, 4 blocked by an unfiltered query, and
1 where two queries disagree (`authoring.bundle`:
`authoringBundlesForOwner` is caller-scoped, `systemActiveAuthoringBundles`
is admin-gated — the concrete instance of the per-concept-floor vs
per-construct-override question).

### The constraint carried forward, not solved

`userByIdSystem` **bootstraps the actor**:
`component/auth/identity_resolver.go` calls it to resolve `sub` → user
in order to *build* the `AccessContext`. So `actor.userId` is circular
for the one construct that creates the actor, and any rule assuming
"every user-scoped read is expressible as a filter over the actor" is
false there. The query is `@serverOnly` for precisely that reason
(#2800).

> **It is `userByIdSystem`, not `userById`.** This section named the
> latter until memql#2984. `userById` (`dsl/identity/queries.memql`) is
> a different query, gated by `requiresOwnerOrAdmin` — so anyone who
> followed the citation found a *gated* construct and reasonably
> concluded the constraint was imaginary. The constraint is real and
> unchanged; only the name was wrong, in this section, in the
> `@rowAuthz` grammar comment, and above `userById` itself.

Phase 1 needs no answer, because nothing is enforced. It does need the
grammar to be **able** to express one, and it is: bare-flag tiers
(`public`, `clusterOwner`) and keyword tiers (`owner=`, `via=`) occupy
separate namespaces inside the argument list, so a fifth tier is a new
flag or a new keyword and disturbs neither. See #2803's thread for why
the escape hatch must be argued from Phase 2 data rather than granted
as an exemption knob.

## Validator

`dsl.TestPerRowAuthzClassification` walks every `query`, `mutate`
**and `seed`** in the tree and classifies each one.

It does not merely scan for a substring. Flagging evaluates the
**boolean structure** of the filter over the row-selection surface —
a caller-check has to hold on every path, so an admin gate that is a
top-level conjunct counts and the same term as a disjunct does not
(memql#2832, memql#2840). A text scan alone would accept
`ownerUserId == actor.userId || status == "open"`, which gates nothing.

**What fails the build, and what does not.** The document used to say
the test is "informational (does not fail the build)", and the status
banner at the top says it hard-fails. Both are half right, which is
worse than either:

- the **per-domain table is informational** — it is `t.Logf`, and the
  test prints the word "informational" in its own banner;
- a **flagged construct is a hard failure** — `t.Errorf`, one line per
  construct, plus the resolution options;
- a **stale exemption is a hard failure** too — an entry in
  `userScopeSelectionExemptions` whose construct no longer matches must
  be pruned rather than left to rot into a blanket that covers nothing.

So: counts to read, findings that fail.

## Counts: regenerate, do not read

This document used to carry two hand-written tables here — a
per-domain query/mutation snapshot ("**Total:** 217 queries + 135
mutations across 11 domains") and a post-sweep classification
breakdown. **Both are deleted rather than corrected.**

They were hand-copied from what the classifier already prints, so they
began drifting the day they were written. Measured against the tree
while this was being fixed, almost every row of the snapshot was
numerically wrong, it used the retired `mutation` keyword (the surface
keyword is `mutate`, memql#2041), it had no Seeds column, and many live
namespaces were missing from it entirely. The classification block was
worse — a fraction of the live namespaces, no `srvOnly` column, and it
reported no `owned` constructs outside cognition when the tree is full
of them.

The counts that sentence originally quoted are gone from it on purpose.
Describing a stale table by writing down today's figures replaces one
hand-maintained number with another and re-lights the same fuse; the
argument for deleting the tables does not need them, and the classifier
prints them on demand.

A hand-maintained count in a document is a defect with a delay fuse.
This page has now produced three (memql#2914's call sites, memql#2918's
`@public` list, and these), so the fix is to stop asserting the numbers
in prose at all: a table that does not exist cannot drift.

**To get the live figures:**

```bash
go test ./dsl/ -run TestPerRowAuthzClassification -v
```

That prints the per-domain table across all six states — `owned`,
`admin`, `public`, `srvOnly`, `FLAG`, `other` — and it is the same
computation the gate fails on, so it cannot disagree with the gate.

For construct totals per kind:

```bash
grep -rhoE '^(query|mutate|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*\{' \
  --include='*.memql' dsl/ | awk '{print $1}' | sort | uniq -c
```

The one number worth stating in prose is the one that must stay put:
**`FLAG` is 0, and the gate hard-fails if it ever is not.**

## Why `@public` (and not "no caller-check")

Each `@public` flag is paired with a comment explaining WHY the
construct is intentionally not caller-scoped. The categories that
emerged from the initial sweep:

1. **System-actor-only paths** — queries called from
   `systemActorContext` (planner agent loop, agent factory dedupe,
   worker registration sweep, etc.). Anyone with a token CAN call
   them, but the tool-loop surface that exposes them is itself
   gated. Follow-up tightening: split into system-only +
   user-self variants; the user-self variant drops the `arg.userId`
   and derives from `actor.userId`.
2. ~~**Going-away-with-#56**~~ — **this category is now empty.** It
   held `queryAccessForUser` and `queryPartitionsForUser`; both are
   gone along with the partition concept they were tied to. Kept as a
   numbered entry so the categories below keep their numbers.
3. **Admin-only paths** — audit-event queries. Tracked under #54 once
   the admin surface is consolidated. The fix here used to be written
   as "composing a `requiresClusterOwner` spec", which does not exist —
   see the note under "The buckets"; the live context-specs are
   `requiresAdmin`, `requiresOwner` and `requiresOwnerOrAdmin`, named
   as a bare top-level conjunct rather than through `spec("...")`.
4. **Web-authenticated user-self** — PAT + worker-token list
   queries backing the `/me/...` pages. The web handler authenticates
   the caller and supplies their own userId as the arg. Proper
   tightening: stop accepting the arg, derive from `actor.userId`.

The follow-up paths are tracked as code comments next to each
`@public` annotation rather than as separate issues -- they're
small, well-scoped changes that land naturally alongside the
features that need them (e.g. the PAT-list tightening lands when
the `/me/pats` route gets its next refactor).

## The `@public` annotation

Parser-recognised. Carries no runtime semantics. The validator
treats it as "author explicitly acknowledges this construct does
not require a caller-check."

The test is **what the construct returns** — does every caller get the
same rows? — not *when it runs*. Examples of legitimate `@public` use.
Each **named construct** below is gated against the tree by
`dsl.TestPublicExamplesAreAnnotated` — it must be declared, in the file
named, carrying a real `@public` attribute. The reasons given are prose
and are not gated; they were true when written:

- the catalog reads in `dsl/rbac/queries.memql` — global, immutable,
  deployment-wide configuration. That file's header is the doctrine and
  the reference case.
- `activeAgentRoles` (`dsl/agents/queries.memql`) — the agentRole
  catalog: `concept agentRole` declares no owner field and no `@pii`
  field, and `agentRoleFull` omits `row.createdBy`, so there is no
  per-caller dimension to scope by.

**Same marker, two meanings — read the comment before extending this
list.** The other three `@public` queries in `dsl/agents/queries.memql`
are the opposite kind: they take a caller-supplied `userId` /
`ownerUserId`, return per-user rows, and carry #54 follow-up comments
explaining the debt. A construct belongs on the list above only if it
is unscoped *because it has nothing to scope by*.

`userByEmail` used to head this list, as the login-path lookup that
runs before the caller is authenticated. That reasoning was right
about *when* it runs and wrong about what follows: it projects
`userFull` — every `@pii` field plus the cluster-wide auth `role` —
so "there is no caller to check yet" left it readable by every
caller, forever. It is `@serverOnly` as of memql#2881, which keeps
the login path working while removing it from the wire.

The lesson generalises, and it is the one to take from this section:
**"the caller is not authenticated yet" is an argument for
`@serverOnly`, not for `@public`.** `@public` carries no runtime
semantics at all, so on a construct returning personal data it
records an intention and enforces nothing.

### `@public` has two jobs, and only one of them is an apology

Conflating them is what made the old list unreadable, so state them
apart:

1. **Acknowledging a flag.** The construct selects rows by a user-scope
   column without a caller-check, the classifier flags it, and `@public`
   is the author saying "yes, intentionally" — with a comment explaining
   why. This is the `#54`-debt kind. The three `@public` queries in
   `dsl/agents/queries.memql` other than `activeAgentRoles` are these.
2. **Documenting intent on reference data.** The construct is *never*
   flagged — it references no user-scope field at all — and `@public`
   records that the global surface is deliberate rather than an
   oversight. `dsl/rbac/queries.memql`'s header states this outright:
   *"None of these queries reference a user-scope payload field, so the
   per-row-authz classifier does not flag them; they are nonetheless
   explicitly `@public` to document the intent."* `activeAgentRoles` is
   this kind too.

Use (2) sparingly and never reflexively: `@public` is matched **ahead of**
the flagged bucket and it hard-blocks memql#2920 tier inference, so every
one spends a suppressor. Spend it where a reader would otherwise
reasonably ask "is this global on purpose?" — a catalog every principal
resolves against — and not merely to decorate a construct nobody
questions.

### Why the cluster-topology bullet was retired

It named constructs that **do not carry the annotation** (`dsl/cluster/queries.memql`
has zero `@public`), and its stated justification was false. There is no
"unauthenticated cluster bootstrap path": boot-time peer discovery reads
`v1:cluster:node` through an in-process raw concept filter
(`component/node/bootstrap.go`), not through any named query, so nothing
in that file serves it.

Whether those queries *should* carry `@public` under sense (2) above is a
live question, not one this list settles — they are unflagged reference
data much like the rbac catalog. What is settled is that the list must
not claim they already do.

`@serverOnly` is definitely not the answer there: it is runtime-enforced
against `auth.CallOrigin`, so it would break the topology reconciler
(`component/node/reconciler.go` calls with a plain context), and that
break could not be repaired in place — `call_origin_conformance_test.go`
forbids `component/node` from stamping internal origin.

If you find yourself reaching for `@public` to "just make the
validator happy" without a clear reason, the construct probably
needs a real caller-check instead.

## Related issues

- #55 — JWT claims → caller envelope contract
- #56 — Remove partitioning (blocked on this audit completing)
- #57 — id cleanup (independent; already in flight)
- #2803 — the ruling that concept-declared row authz is worth building,
  scoped to Phases 1–2 with the enforcement decision deferred until the
  measurement exists
- #2920 — Phase 1: this section's vocabulary + loader validation, inert
- #2921 — Phase 2: shadow mode, measuring what enforcement would change
