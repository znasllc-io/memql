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
> `go test ./test/dslconformance/ -run TestPerRowAuthzClassification -v`. The `#54`-era
> counts and tables that
> used to sit further down have been **deleted rather than corrected**
> (memql#2983) — see "Counts: regenerate, do not read".

## Context

This framework was written while MemQL still relied on
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

### RANK is not a fifth bucket (epic memql#4832)

`rankVisible`, `rankStrict` and `unowned="<role>"` are **arguments of the
owned tier**, exactly as `clusterOwner` is. They widen who the owned
bucket admits; they do not create a bucket, and every construct over a
concept carrying them is still classified **owned** and still has to carry
the owner conjunct.

- **`rankVisible`** — reads admit rows owned by anyone at or *below* the
  caller's rank. Peers included at every rung (`<=`).
- **`rankStrict`** — writes admit rows owned by someone *strictly* below,
  plus your own unconditionally. It also **withdraws the cluster-owner
  write escape**: peer rows are read-only owner-to-owner included, which
  is what makes [ownership transfer](#ownership-transfer) necessary rather
  than optional.
- **`unowned="<role>"`** — a row whose owner field is *present and empty*
  is the deployment's, not a principal's, so it has no rank to compare.
  This names the actor rank from which such a row is readable. An
  **absent** owner key stays denied, which is what the owned tier has
  always done with one.

Two properties an operator reading a refusal should know:

1. **The owner's rank is resolved per request**, from the principal
   table, and is never stamped on the row. Promoting somebody
   retroactively changes who may see the rows they already own, so a
   stamped rank would be stale the moment any role changed — and stale in
   the direction of showing too much.
2. **An unresolvable rank floor denies everybody.** A typo'd role slug in
   `unowned=` or `@requiresRank` refuses boot, and the runtime backstop
   refuses the call, because the natural spelling of a floor check
   (`actorRank >= rankOf(slug)`) reads correctly and fails *open* — every
   rank clears 0.

**`@requiresRank("<role>")`** is the other half and is not a bucket
either: it is an actor-rank floor on a *construct*, deciding who may CALL
it. The tier still decides which rows come back. A caller below the floor
gets a **refusal, not an empty page** — an empty page is
indistinguishable from "there is nothing here", which is the answer
somebody who may not reach the surface should never be handed.

### Ownership transfer

With peer writes refused at every rung, no human can repair another
human's rows, and an offboarded principal leaves rows no living principal
can edit. The answer is transfer, not a break-glass write escape: an
escape is available to everyone who can reach it — the whole cluster-owner
tier — and would hollow out the rule on the day it shipped.

`integration.identity.transferRowOwnership` is cluster-owner-only,
narrowable to one concept or one row, and audited once per transfer on
`v1:identity:auditEvent` under the `rowOwnership` target type. It refuses
a destination that does not exist or is deactivated (transferring into a
void leaves the rows exactly as unwritable, with an audit trail saying
otherwise), skips cluster-owned rows, and excludes self-owned tiers
entirely — their "owner field" is the row's own id, so a transfer would
rename the row.

> **Payload fields are BARE.** The `payload.` prefix this table used to
> prescribe was retired by memql#2292 and is hard-failed by
> `test/dslconformance/conformance_test.go`; a filter written that way does not load.
> Row intrinsics take the `row.` namespace (`row.id`, `row.createdAt`).

> **`granted` is not implemented.** The classifier has no counter for it
> and does not resolve relationship specs, so a granted construct lands
> in `owned` or `other` depending on its filter. The row above is the
> authoring rule, not something the gate measures.

> **There is no `requiresClusterOwner` spec.** This table used to say
> "compose `spec("requiresClusterOwner")`", and neither half was live:
> the spec is declared nowhere in `dsl/` (`test/dslconformance/admin_gate_test.go` says
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
| owned + admin | `@rowAuthz(owner="<field>", clusterOwner)` | `(<field> == actor.userId) \|\| (actor.isClusterOwner == true)` |

```memql fragment
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
- **The COMPOSITE form is one tier, not two** (memql#4312).
  `@rowAuthz(owner="<field>", clusterOwner)` is the only accepted
  two-argument list, and its arguments are order-independent (an
  attribute's argument list is a map, so there is no order to depend
  on). Every other pair — `public, clusterOwner`, `owner=` with `via=` —
  is refused at load, naming the accepted forms.

  It is the **owned tier carrying a cluster-owner bypass**, not a fifth
  tier, and that matters beyond taxonomy: the owner-field machinery
  (the stamping requirement below, the actorless-read refusal, the
  conformance authz bucket) all switch on the owned tier, and a new tier
  value would have fallen silently out of every one of them.

  Reach for it when an **operator console must read across users**. A
  plain `owner=` tier has no cluster-owner bypass at all, so declaring
  a live operator surface plain-owned hides every other user's rows
  from the operator too — the wrong trade, and plausibly why so much of
  the undeclared long tail stayed undeclared.

  **The write guard ignores the second argument.** Reading a row as an
  administrator is not authoring it. (The cluster-owner escape a write
  already has is `rowAuthzWriteEscape`'s; it pre-dates this form and
  applies to every declared tier alike.)
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

  Since epic memql#4541 that distinction is load-bearing rather than
  stylistic, because `public` now has a caller that no other tier does: an
  ANONYMOUS one. On a cluster with `MEMQL_PUBLIC_READS_ENABLED=true` the
  WebSocket bridge admits a session carrying no credential, pinned to
  query execution and graph subscriptions, and row admission lets that
  actor reach `@rowAuthz(public)` concepts **and nothing else — undeclared
  included**.

  That last clause is the rule to remember. `rowAuthzAdmits` admits an
  undeclared concept for every other caller, which is what makes the ~88
  undeclared concepts in the tree readable at all. An anonymous caller
  does not inherit it: undeclared is *unmeasured*, and a public tier that
  read it as *publishable* would publish most of the graph the day it
  shipped, silently, with every gate reporting exactly what it was asked.
  `component/memql/rowauthz_anonymous.go` answers before the tier switch
  runs and never falls through, the same shape the connector actor uses
  and for the same reason.

  Three further properties, each enforced rather than conventional:
  `public` is a READ tier (there is no anonymous write — refused at
  `executeWrite`, the single write chokepoint, whatever the concept
  declares); subscription admission inherits the read rule unchanged
  (memql#4309's property, so the live feed cannot be looser than the
  query); and every anonymous visitor is ONE actor, which is what lets a
  public result be cached once and served to everyone.

  The tier ships **enforced and empty**: nothing in the engine tree
  declares it, `TestTheEngineTreeDeclaresNoPublicConcepts` keeps it that
  way, and `TestPublicTierConceptsCarryNoPII` refuses the combination of
  `public` with `@pii` fields. Product bundles declare it on their own
  content concepts. Operator instructions:
  [site-hosting.md](../site-hosting.md).
- The parameterised tiers use `=` and a quoted value, matching every
  other keyword-arg annotation (`@relationship(field="x")`,
  `@displayCard(primary="name")`).

### Status: the tier IS enforced (memql#3172 / #3174 / #3175)

> **Corrected (memql#3350).** This section previously read
> "**Nothing is enforced.**" and described Phase 1 as inert. That has
> been false since memql#3172. It also told you to `sed` the allow-list
> out of `TestRowAuthzIsInert`, a gate **retired by that same issue** --
> the command printed nothing. Both are fixed below; do not calibrate
> against any copy of this page that still says the tier is inert.

A declared tier is enforced at the seams below -- and one further gate rides in
the same seams without being a tier at all, listed last so it is not mistaken for
one. Read the behaviour off these files, not off prose:

| Mechanism | File | Covers |
|---|---|---|
| Filter injection | `component/memql/rowauthz_enforce.go` (`enforceRowAuthzOnPlan`) | every read whose plan has a **bound concept**; the predicate is ANDed at the **root**, so an author's `a \|\| b` becomes `((a) \|\| (b)) && (authz)` |
| Row admission | same file (`rowAuthzAdmits`) | reads with **no** bound concept -- a raw client-supplied query string -- **and** graph expansion, which has no filter to AND anything into, **and** a top-level builtin call, whose rows come out of a Go handler rather than a query (memql#3982) |
| Subscription fan-out | `component/memql/rowauthz_subscription.go` (`AdmitSubscriptionRow`), wired in `component/grpc/server.go` (`handleBusEvent`) | every `graph.node.*` event on its way to a subscribed stream -- see [Subscriptions are an egress](#subscriptions-are-an-egress-memql4309) |
| Anonymous refusal | same file (`refuseRowAuthzWithoutActor`) | a read carrying no caller identity **errors** rather than comparing against `""` and returning rows owned by nobody |
| Write guard | `component/memql/rowauthz_write_guard.go` | `update` / status-flip / a raw `insert(` onto an existing id -- the engine resolves the target row and refuses when its owner is not the actor |
| Create stamping | `component/memql/rowauthz_insert_stamp.go` | the raw-`insert(` create path that bypasses accept/stamp |
| Staged-data visibility | `component/memql/staged_enforce.go` (`admitStagedRow`, `filterStagedSet`, `filterStagedNodes`, `enforceStagedDataOnPlan`) | **NOT a `@rowAuthz` tier.** Rows of a concept whose DATA is staged (epic memql#3974). It asks *"is this row visible to anyone yet"* where the tier asks *"may this caller see it"*, so it rides **outside** the authz gate and no authz answer can readmit a row it withheld -- see [Staged-data visibility](#staged-data-visibility-memql3974) |

Escapes from the **write** guard are enumerated in exactly one place
(`rowAuthzWriteEscape`): internal origin stamped per-write by an
allow-listed package, and cluster owner. Nothing else. `admin` is
deliberately not among them.

**The read path has no such escape.** `enforceRowAuthzOnPlan` takes no
context, so filter injection cannot be waived for a trusted caller. That
asymmetry is not an oversight to route around -- it is the constraint
that decides which concepts can carry a tier at all (see
[Concepts that cannot carry a tier](#concepts-that-cannot-carry-a-tier-memql3349--memql3350)).

The land-time gate that replaced `TestRowAuthzIsInert` is
`component/memql/rowauthz_enforce_gate_test.go`; it re-derives at PR head
that every construct over a declared concept already carries the tier's
term as a top-level conjunct.

A concept with no declaration still loads. It produces one aggregated
boot **warning** naming every undeclared concept, and the undeclared
population is pinned shrink-only by
`component/memql/rowauthz_undeclared_gate_test.go`. Escalation to a load
error is a later phase, once the tree is clean.

### Subscriptions are an egress (memql#4309)

A subscription DELIVERS ROWS, so it is an egress of rows in exactly the sense
this page enumerates -- and until memql#4309 it was the one egress that never
called `rowAuthzAdmits`. `handleBusEvent` matched an event's topic against each
subscription's patterns and sent the whole flattened payload; `handleSubscribe`
had no gate; and the event bus itself has "no AccessContext and no
authorization hook of any kind" (`component/memql/executor_mutation.go`). For
the concepts that declare a tier this was a real leak: the read path denied the
row and the subscription delivered it.

The gap was **unrecorded rather than accepted**. This page did not list
subscriptions among the egresses at all, which is why it went unnoticed for as
long as it did -- and why the fix comes with a doc gate rather than only with
code. `TestSubscriptionFanOutAppliesTheRowGate` (`component/memql`) is named in
`rowauthz_doc_gate_test.go`'s `docNamedTests`, so the `@rowAuthz` annotation doc
must keep citing it, and a rename or deletion has to come back here.

**The rule is the read path's rule.** Undeclared admits, declared enforces --
one seam, not a second rulebook (design D1). A subscription that were stricter
than a read would be a second authorization implementation, and it would drift
from the first; a subscription looser than a read is the leak. The consequence
worth stating plainly: for the ~90 concepts that declare no tier, a subscription
still delivers every user's rows -- and so does a raw query, which is what makes
that acceptable rather than a second finding. The hole closes concept by concept
as tiers are declared.

Three outcomes per event, one per admission verdict:

| Verdict | What the stream receives |
|---|---|
| admit | the event, unchanged |
| deny | nothing. The stream is not told the row exists -- being told that a row you may not read exists is itself a disclosure. The operator sees it: a debug log and `memql_subscription_rows_denied_total{concept}` |
| undecided (`granted`) | an **id-only** notification: `{concept, id, action, createdAt}` with `payload_omitted=true` on the wire. The client re-reads the row through the authorized read path -- which performs the join a `granted` tier needs and a single row cannot answer -- and drops the event when that read refuses |

The id-only path exists so a future `granted` concept's live feed cannot die
without a trace. No concept declares `via=` today, so it is built against a
fixture on purpose (design D3).

Two further properties, neither obvious:

- **Admission runs where the SUBSCRIBER is.** A forwarded mesh event is
  re-published on the receiving node's bus and fanned out there, so two
  subscribers on one receiving node get different answers to the same
  forwarded event. No forwarding change was needed; the cross-node gate is
  `component/grpc/subscription_rowauthz_mesh_test.go`.
- **The subscription read is stamped UNBOUND**, which is what engages the
  `@pii` narrowing on `v1:identity:user` (`rowauthz_pii_unbound.go`). Without
  that stamp, subscribing to a concept that declares no tier but does declare
  `@pii` would hand every user's PII fields to any signed-in stream --
  memql#3350's generic-browse hole arriving through a different door.

**Non-graph subscription kinds are owner/admin-only** (memql#4311).
`TELEMETRY`, `MESSAGE` and `AI_STREAM` carry node-level events with no row
owner to decide by, and `ALL` (`#`) carries every graph topic besides. They are
refused at subscribe time below admin, with `PermissionDenied` and a reason. An
`ALL` subscription an admin does hold still passes each graph event through
fan-out admission -- the topic-prefix check keys on the EVENT, not on the
subscription's kind, so `#` is not a way to reach rows `GRAPH_EVENTS` would be
denied.

### Staged-data visibility (memql#3974)

The last row of the table above is a **different question asked at the same
seams**, and the distinction has to survive being read quickly: the tier asks
*may this caller see this row*, staged-data asks *is this row visible to anyone
yet*. Staged-data is the outer of the two -- a staged row is withheld from every
caller, so no authorization answer readmits it -- and it is applied immediately
after the tier for exactly that reason.

The mechanism: a promoted concept can carry `conceptDataStaged` on its
`v1:authoring:construct` row, and while it does, the rows written under it are
present, addressable and withheld from the ordinary read path until the concept
is **trained**. The lifecycle is documented from the authoring side in
[Training constructs into a running cluster](../../language/training.md#staged-data-rows-can-arrive-before-the-concept-is-trained).
Note before reading further that `staged` names two unrelated things in this
codebase; that page's
[disambiguation table](../../language/training.md#two-things-are-called-staged-and-they-disagree-about-the-subject)
is the one to read first.

#### The architecture INVERTS this document's

For a declared tier, **filter injection is the primary mechanism** and row
admission covers the residue -- the unbound reads and graph expansion, where
there is no filter to AND a term into.

For staged data it is the other way round, and the emphasis is not stylistic:

- **The row gate is the correctness mechanism.** memql#3977 measured the marking
  models and chose concept-grain: there is no staged marker on the rows at all,
  so visibility is a pure function of the CONCEPT -- and every row carries its
  concept. A gate reading `node.Concept` therefore decides staging correctly with
  no injection whatsoever, and it inherits the property the row-authz gate beside
  it already documents at `component/memql/executor.go` (around line 240): it is
  "immune to how the filter was spelled: naming a row by id, a top-level `||` and
  a negated concept all reach it identically."
- **The injected conjunct is a pushdown optimization.** It exists so the engine
  does not FETCH rows the gate would then discard. A pushdown is allowed to be
  incomplete. It is not allowed to be wrong.

#### It is deliberately NOT gated on a bound concept

`enforceStagedDataOnPlan` does not copy `enforceRowAuthzOnPlan`'s early return on
an empty `plan.BoundConcept`, and that is the single most important difference
between the two injectors.

Row-authz **must** resolve a binding, because the tier is a per-concept
declaration and there is nothing to resolve it from otherwise. The staged
predicate binds nothing -- it names the staged set directly, so it is exactly as
meaningful on a raw client-supplied query string, on a `concept==A || concept==B`
union, and on a bound query. Requiring a binding would have reproduced the
top-level-builtin hole memql#3982 had just closed, on the same fifth of the tree.

#### `undecidable` here means UNPLACEABLE, not UNCOVERED

Read this before quoting the shadow-mode figure at anyone.

Staged-data shadow mode (memql#3981) is modelled on the row-authz shadow mode
below, but it asks a different question. Row-authz asks *is the predicate already
implied by what the author wrote*, which only makes sense because that predicate
is a function of the caller and an author can have hand-written it. The staged
predicate is a CONSTANT over a column every row carries, so there is nothing for
implication to turn on. What is left to decide statically is whether the
predicate can be **placed** on this construct at all.

Regenerate rather than trust the number:

```bash
go test ./component/memql/ -run TestStagedDataShadowMeasuresTheTree -v
```

At the run the enforcement ruling was taken against, 115 of 619 measured
constructs came back `undecidable` -- 82 builtins and 33 logic functions, all of
them constructs that bind no concept.

**That does NOT mean a fifth of the tree is ungated at runtime.** It means a
STATICALLY PLACED predicate cannot reach those constructs, which is a fact about
the pushdown and not about coverage. Both halves have to be said, because each
alone is misleading:

- The injector does not require a binding (previous section), so it is not the
  case that nothing is injected for an unbound read.
- The row gate is reached from the concept on the row, so a read that bypasses
  the filter path **entirely** is still gated -- which is precisely what
  memql#3982 made true for the top-level builtin at `plan.Root`, and why the row
  gate is trustworthy for this population.

The verdict named `would-hide` is the mechanism WORKING, which is the other
inversion: in row-authz `would-narrow` is the alarming verdict, here hiding is
the designed behaviour and `undecidable` is the alarming one.

#### The population that neither mechanism reaches

A read issued **inside a Go executor** goes to storage without passing a plan, so
neither the injected conjunct nor the row gate sees it. That population is
inventoried by memql#3984, and the inventory is deliberately a CLASSIFICATION
rather than a blanket requirement -- memql#3978 ruled that some of those reads
must be **prohibited** from carrying the predicate, not merely exempted from it.
The concrete case: the counters that decide retire-versus-remove and that refuse
a breaking schema change read storage under no actor at all, so staged rows are
inside their counts. Make them carry the staged predicate and a concept holding
ten thousand staged rows counts as empty -- it becomes removable rather than
retirable, and a breaking schema change lands unrefused against rows about to be
made live under a schema they do not satisfy. A check that mechanically required
the predicate everywhere would not prevent that bug; it would create it.

#### Three implementation constraints, each load-bearing

- **No `context.Context`, by ruling.** The predicate is injected from
  `parseWithFunctionsAmbient`, which has none, and memql#3976 explicitly declines
  to give `enforceRowAuthzOnPlan` one -- the three recorded justifications for its
  context-freedom stand unamended. The predicate is therefore a constant, which is
  what keeps the actor-less callers out of the special-case business: an
  automation starting from `context.Background()` under a synthetic system actor
  gets the same answer as anybody else, and internal origin does not become an
  accidental staged-read grant.

  **The scope memql#3976 called for now exists (memql#4040), and it did not cost
  the injectors a context.** The resolved scope rides down as a *parameter*, the
  same route `auth.CallOrigin` (memql#2800) and the ambient envelope
  (memql#3024) already take into that function: `executeWith` reads the request
  once and hands the ctx-free parse path the answer. So the predicate is still a
  constant -- just a constant of the resolved scope rather than of the staged set
  alone. Three properties make it safe to have:

  | | |
  |---|---|
  | **Declared, never inferred** | The caller names concept ids on the request (`memql.ContextWithStagedScope`). Being the cluster owner *authorizes* a scope; it never *grants* one, so an owner's ordinary read still sees nothing staged. |
  | **Authorization split from the predicate** | Permission is identity-derived and resolved **once**, in `stagedScopeFor`, where the actor exists. By the time the injection site sees a scope it is already authorized, so no identity reaches the predicate. An unauthorized declaration resolves to the *empty* scope regardless of entry point, and `Execute` additionally refuses it outright rather than silently returning an ordinary read. |
  | **Both seams honour the same set** | The conjunct (`stagedConceptIds`) and the row gate (`admitStagedRow`) resolve through one function, `stagedConceptWithheld`. A scope honoured by one and not the other would return *some* staged rows and not others, which is worse than either answer. |

  It is a **cluster-owner** capability and a **Go-level** one, matching the reach
  of the write side (`WithConceptDataStaged` has no wire surface either). The
  concept's own owner would be the more natural authorization, but resolving it
  means a store read inside a gate that must stay a single `sync.Map` load.

  One consequence is easy to miss and is a leak if it is: the resolved scope is
  a **result-cache key term**. A scope covering every staged concept leaves
  nothing to exclude, so `plan.Root` is byte-identical to an ordinary caller's --
  and without the term the operator's staged-inclusive result would be cached
  under that key and served to the next caller.
- **An SQL-only predicate would not survive the read path.** Both filtered seams
  return through `latestMatchingNodes`, which reloads each candidate's true latest
  version through a query filtering on `id` and `createdAt` and nothing else, then
  swaps it in. A predicate that existed only in emitted SQL would be defeated by
  that swap while every test stayed green. Two things close it: the predicate is
  evaluable in Go, so the re-check rejects the swapped-in candidate, and the swap
  is co-gated by `admitStagedRow` directly.
- **The spelling is a chain of `!=`, never `out`.** `out` compiles to a tidy
  `concept NOT IN (...)` and HARD-ERRORS in the in-process re-check, which
  implements equality and inequality only on that intrinsic -- so it would not
  leak, it would break every read the moment any concept was staged. The
  conjunction of `!=` terms is the one spelling both halves accept.

On NULL-safety -- the standing warning on this path is the `isNotDeleted` bug,
where a plain SQL `<>` yielded NULL rather than true for a row that never carried
the key and silently excluded every such row. Concept-grain sidesteps it
STRUCTURALLY rather than mitigating it: the predicate references `concept`, a
`notnull` column every row has and no row can leave absent, and there is no
"every existing row carries no marker" hazard because there is no marker. The
comparison is tightened to `IS DISTINCT FROM` regardless, so the guarantee rests
on the operator rather than on a schema annotation staying true.

### Concepts that cannot carry a tier (memql#3349 / memql#3350)

An undeclared concept is not "safe" and not "unchanged" -- it is
**unmeasured**. But two identity concepts are undeclared for a reason
stronger than backlog, and the reason is the same one in both cases:
**filter injection is unconditional and caller-blind**, and both
concepts carry reads that must run *before an actor exists*.

- **`v1:identity:identity`** is the credential concept, and it carries
  **three** independent blockers (memql#3349), each measured by declaring
  `@rowAuthz(owner="userId")` and running the existing gates:

  1. **`userId` is not an ownership claim.**
     `TestDeclaredOwnerFieldsAreServerStamped` reports **seven** mutations
     writing it from caller args and **zero** stamping it from the actor.
     That is the field's meaning, not a bug: an admin mints a PAT / worker
     token / badge *for another user*, and a node token's `userId` is a
     machine binding. It names the credential's **subject**.
  2. **Four pre-actor reads build the actor** -- `patIdentityByKeyHash`,
     `workerTokenByKeyHash`, `badgeByKeyHash`,
     `nodeTokenIdentityByBinding`. `component/identity/pat/verifier.go`
     says it outright: *"This is a pre-actor read, so it cannot be
     caller-scoped."* With the tier declared,
     `TestRowAuthzEnforcementLandGate` reports **nine of the eleven** reads
     undecidable -- credential verification itself is in the blast radius.
  3. **It is a union of eight credential kinds** with different subjects.
     `node_token` and `voice_agent_token` authenticate a *process*;
     `nodeTokenIdentities` is an operator listing of every node credential
     in the cluster and is caller-scopable by nobody.

  The interim answer is an **inventory**, not an annotation:
  `component/memql/identity_credential_rowauthz_inventory_3349_test.go`
  pins all eleven reads with a class (pre-actor / self-scoped /
  admin-scoped / machine-credential) and a reason, derived from the loaded
  registry so a new read must classify itself. The second closing option
  in the list below -- **splitting the concept by credential kind** -- is
  the one specific to this concept, and it is a data migration plus a
  wire-contract change across every caller, not a one-line annotation.
- **`v1:identity:user`** adds two more obstacles: `userByEmail` /
  `userByIdSystem` are pre-actor for the same reason, and
  `userDisplayById` (`@public`) plus `usersActiveInSpace` are
  **legitimate cross-user reads for ordinary callers** -- they render one
  participant's name in another's chat. Its admin reads
  (`searchUsers`, `userById`) are an admin **roll-up**, which `owned`
  cannot express: the owned predicate is ANDed with no cluster-owner
  escape, so declaring it would narrow an admin's user list to the
  admin's own row and return a confidently wrong answer.

The recorded decision for `v1:identity:user` is therefore: **role-gating
for that concept lives at the projection, not at the row.** Users may
legitimately see each other; what they may not see is each other's
`@pii`. The named queries already draw exactly that line, with their
**shapes**.

Which left one surface uncovered, and it was a real exposure: the
**generic concept browse** (`browseConceptPage`) sends a raw query
string, projects no shape, and returns the full payload. Over
`v1:identity:user` that handed every authenticated caller, of any role,
all eight `@pii` fields. It is closed at row admission for **unbound
reads only** (`component/memql/rowauthz_pii_unbound.go`): a row whose
concept declares `@pii` fields and declares **no** tier is admitted only
to the row's own subject or to an owner/admin. Bound reads are
untouched, so `userDisplayById` still works.

That gate is keyed off the `@pii` annotation rather than a concept list,
so a concept that grows a `@pii` field is covered the moment it does --
the same property the hard-delete PII scrub (memql#1711) already relies
on. `v1:identity:user` is currently the **only** `@pii`-bearing concept
in the tree, asserted by `TestPIIBearingConceptPopulation`.

What would let a tier be declared on either concept, in dependency
order: **(a)** a read-path escape mirroring `rowAuthzWriteEscape`, so the
pre-actor and system reads survive injection; **(b)** a tier that
composes self-access with an admin roll-up, since `owned` cannot express
one. Neither was a prerequisite for closing the browse hole.

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

**This has since happened again on the read side, with the same answer.**
`authSessionsForSubject(subject: ...)` filtered on a caller-supplied id and
nothing else, so any signed-in caller could read anyone's sessions; the
`@serverOnly` route was unavailable for the same reason as here (both callers
live in `component/grpc`, which may never stamp). It became
`authSessionsForSelfIncludingRevoked()` — no argument, `filter
subject==actor.userId` — which is this section's `stamp { ownerUserId:
actor.userId }` fix in read form (memql#4768). The preference order both cases
landed on is now written down at the ban itself, in
`component/auth/call_origin.go`: caller-scope the construct first; if the
operation is genuinely about somebody else, use a purpose-built allowlisted
package with a precondition test (`adminops`); never stamp in a handler
(memql#4769).

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

**Scope of that guarantee.** It covers the named-mutation surface. It did
not cover raw `insert(...)`, which short-circuits the planner
(`component/memql/parser.go`) and bypasses `args` / `accept` / `stamp`
entirely; the per-concept Go guards on that path are reactive -- one per
past incident -- and neither library concept was among them. Closed by
**memql#3175** (carrying **memql#3059**): the engine server-stamps the
field named by `@rowAuthz(owner=...)` on every write that did NOT come
from a rendered mutation template, overwriting a caller-supplied value
(`component/memql/rowauthz_insert_stamp.go`). It is driven by the
declaration rather than a concept list, so a concept is covered the
moment it declares a tier. The escape set is the write guard's --
cluster owner, or trusted server-side Go stamping internal origin for
that one write (memql#3174) -- and a call carrying no resolved caller
identity is refused rather than stamped with an empty owner. Both
library concepts additionally carry `@serverSet` on `ownerUserId`, which
turns "never accepted from caller args" into a load-time rejection on
the template path.

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
while the primary user-facing read is space-scoped, and declared
`library.artifact` owned while `libraryWorkspaceLiveSources` documented
its rows as having no owner at all — that read was rescoped when
memql#4340 declared the concept's tier by hand, which is the other way
the same rule gets satisfied.) The single exception is
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

This document does not carry a snapshot of the current owned /
clusterOwner / undeclared split, for the same reason the "Counts:
regenerate, do not read" section below gives for deleting the
per-domain tables: a hand-copied number begins drifting the day it is
written. Regenerate it against the tree instead:

```bash
go test ./test/dslconformance/ -run TestPerRowAuthzClassification -v
```

The `authoring.bundle` concept is worth knowing about regardless of
the current counts, as the concrete instance of the per-concept-floor
vs per-construct-override question: `authoringBundlesForOwner` is
caller-scoped while `systemActiveAuthoringBundles` is admin-gated, so
its two queries disagree about who may read the concept's rows.

### The residuals a client is filtering today (memql#4369)

The portal's Nexus surface draws one goal's world -- the plan, its tasks, the
agents it raised, the bundle it authored and the artifacts it produced -- and
four of those concepts reach it through reads this document classifies as
**undeclared**. The narrowing that is actually applied is therefore partly in
a browser, which is recorded here rather than left to be discovered:

| Concept | State | What Nexus does |
|---|---|---|
| `v1:planner:plan` | undeclared; blocked on #4366 | the client refuses to draw a goal whose `requestedBy` is not the caller's own user id, and says so on the page |
| `v1:planner:task` | undeclared; blocked on #4366 | filtered by `planId` client-side |
| `v1:agents:agent` | undeclared, long tail | `agentsForPlan` is `@public` and narrows by `lineage.originatingPlanId`; filtered again client-side |
| `v1:authoring:bundle` | undeclared, long tail | narrowed by `sourcePlanId` on an owner-gated read; filtered again client-side |

Two things follow, and neither is a criticism of the surface:

- **A client-side filter is not a gate.** It closes the deep-link hole a
  goal-shaped URL would otherwise open -- following someone else's link shows
  you a refusal rather than their goal -- and it changes nothing about what
  the underlying reads admit. The reads are exactly as wide as they were
  before Nexus existed.
- **The client needs no change when the tier lands.** Every live event the
  page consumes is already resolved through the authorized read and dropped
  when that read refuses (the `granted` tier's id-only shape, #4309), so the
  declaration #4366 is waiting on narrows the surface without touching it.

`v1:library:artifact`, `v1:authoring:construct` and
`v1:authoring:dependencyEdge` are owner-gated in their reads and are not part
of this residual.

### The MemQL OS Training app's residual (memql#4737, one of two CLOSED in memql#4970)

The Training app (`clients/os/src/apps/training/`) had two entries here. One is
gone; the other is different in kind and remains. Both are recorded for the
reason Nexus's are: the narrowing that is actually applied is partly in a
browser.

| Concept | State | What the app does |
|---|---|---|
| ~~`v1:planner:plan`~~ | **CLOSED, memql#4970** | the app no longer reads a plan at all -- see below |
| `v1:knowledge:documentChunk` | undeclared, long tail | **nothing**, and that is the honest entry -- see below |

The first entry closed by the app being re-keyed rather than by the concept
being declared, which is worth stating precisely because the two look the same
from here and are not. Epic memql#4970 moved the app off the space attachment
route onto the Library (`POST /artifacts`), so its feeds are now
`v1:library:file` and `v1:work:run` -- both of which declare
`@rowAuthz(owner="ownerUserId", clusterOwner)`. Row admission gates
subscriptions through the same function it gates reads with (memql#4309), so
other people's rows never arrive and there is nothing left for a client-side
filter to drop. `planBelongsHere` and its viewer-id check are deleted.

`v1:planner:plan` itself is UNCHANGED and still undeclared: Nexus's entry above
still stands, and this is one consumer leaving rather than the concept being
fixed.

The second is a different shape and is worth stating plainly rather than
filing under the long tail. `setChunkValidationStatus`
(`dsl/knowledge/mutations.memql`, memql#4739) is the first CLIENT-REACHABLE
WRITE this app makes, and `v1:knowledge:documentChunk` declares no tier and
carries no owner field to declare one against -- so any authenticated caller
may approve or reject any chunk. That is the standing position of its three
sibling mutations (`createDocumentChunk`, `writeKnowledgeChunk`,
`markChunkSuperseded`), which are equally client-reachable; the new mutation
inherits it rather than widening it. The Training app's `writer` role gate is
PRESENTATION and is not what stops anyone.

Declaring a tier here is a real authorization decision rather than a
formality, and it needs an owner concept the schema does not have: a chunk
belongs to a knowledge domain, and `v1:knowledge:knowledgeDomain` is declared
in no `.memql` file at all. Closing it means declaring that concept first.

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

The classifier lives in `component/memql/dslgate` and walks every
`query`, `mutate` **and `seed`** in the tree, classifying each one.

**It runs at LOAD time** (memql#3629). `MemQLEngine.Init` scans the merged
tree -- embedded core plus every registered overlay, which is where a
`MEMQL_DSL_PATH` product bundle lands -- records each flagged construct on
the `LoadReport`, and strict boot REFUSES rather than warns
(`MEMQL_DSL_ALLOW_SKIPS` is the operator break-glass). Until then the only
thing running this classification was a Go test over this repo's own tree,
so a product's DSL -- the primary delivery path under platform
consolidation, memql#2472 -- was classified by nothing at all.
`dsl.TestPerRowAuthzClassification` still runs it over the embedded corpus,
through the same code rather than a second copy of it.

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
  `dslgate.UserScopeSelectionExemptions` whose construct no longer matches must
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
go test ./test/dslconformance/ -run TestPerRowAuthzClassification -v
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

## The connector actor: a named internal writer

Row admission has one actor that is neither a user nor a role: a
**connector** (epic memql#4378, decision D4). It is the characterised
internal actor memql#4366 asks for, **for this class of writer only** —
the planner's system actor is a separate decision and is not this one.
The engine's housekeeping READS are answered by the [maintenance
principal](#the-maintenance-principal-a-named-internal-sweep) below.

**The rule, in one sentence.** A connector actor is admitted to the rows
of a concept whose `@origin` or `@mirroredTo` names it, regardless of
that concept's declared tier, and to no other concept whatever its tier.

```
auth.ConnectorActor("shopify")   ->  v1:shopify:product          admitted
                                     (the concept declares @origin("shopify"))
                                 ->  v1:campaigns:sendJob        refused
                                 ->  an UNDECLARED concept       refused
```

That last line is the half that makes this a targeted rule rather than a
bypass. An undeclared concept admits everyone, so a connector *falling
through* to the ordinary tiers would inherit exactly that — and the
undeclared population is the long tail this document is otherwise about.
`rowAuthzAdmits` therefore answers the connector question **first and in
both directions**: named → admit, not named → deny, never fall through.

**Why not the internal-origin escape.** The write guard already lets
trusted server-side Go past a tier by stamping
`auth.ContextWithInternalOrigin` for one write. That stamp says *the
engine is doing this*, which is true of a connector and decides nothing:
it would admit the Shopify connector to campaign rows, to identity rows,
and to every mirror belonging to some other origin. The connector actor
says *which* connector is writing, and the concept's own declaration
answers.

**It is never minted from a request.** There is no header, claim, token
class or role value that produces one. `auth.ConnectorActor` is the only
constructor, called by the runtime immediately before it invokes a
connector's contract method. `RoleConnector` sits deliberately outside
`ValidRoles()` and outside the rank model, so no user row can carry it,
no identity can be issued with it, and a value that somehow leaked into
an ordinary request context would grant *less* than a reader.

**A second gate stands beside this one, and it is stricter.** A concept
whose `dataState` is `mirror` is read-only by construction: *every* write
to it is refused unless it comes from the connector its `@origin` names —
no cluster-owner escape and no internal-origin escape, unlike the write
guard above. Read
[data origins](../../concepts/data-origins.md) for why an operator's edit
to a mirror is refused rather than accepted: it would be reverted by the
next reconciliation sweep, so accepting it is a write that appears to
work and does not last.

Implementation: `component/auth/connector_actor.go`,
`component/memql/rowauthz_connector.go`,
`component/memql/mirror_write_guard.go`. Measured by
`TestConnectorRowAdmissionIsScopedToTheConceptsThatNameIt` and
`TestMirrorWritesAreRefusedForEveryActorButItsConnector`.

## The maintenance principal: a named internal sweep

The connector actor above characterises one class of internal caller. The
engine has a second, and it is the one that blocks declarations rather
than enabling them: **housekeeping reads that span every owner by
definition** — a retention sweep.

**The failure it exists to prevent.** `contextWithSystemActor`
(`component/automations/executor.go`) stamps `RoleReader`, deliberately:
`"system"` is not in `auth.AllRoles`, and Reader keeps
`actor.isClusterOwner` FALSE for a caller with no identity of its own
(memql#2801). Right for an automation acting on one user's data — and
fatal for a sweep. The moment a swept concept declares an owned tier, the
injected predicate compares each row's owner against
`system:automation:<name>`, matches nothing, and the sweep retires
nothing:

> no error, no log line, and a retention window that goes on looking like
> a setting while the table is never pruned.

Every gate in this document stays green through that. memql#4406 measured
it for `v1:worker:invocation` and refused to declare the tier until the
principal existed, because the alternative was discovering it in
production as *"why is this table growing"*.

**The decision: an identity, not an escape hatch.** The argument is
`component/campaigns/worker.go`'s, and it is the one that settles it —

> The alternative would be an escape hatch in the enforcement layer,
> which is strictly worse: a bypass is available to every caller that can
> reach it, whereas an identity is only as powerful as the queries it is
> used for.

So a listed automation runs as a **named synthetic cluster owner**,
`system:maintenance:<automation>` at `RoleOwner`, which is the composite
tier's only escape. Two consequences worth stating:

- **The queries say so themselves.** `expiredWorkerInvocations` carries
  `actor.isClusterOwner==true` as a top-level conjunct rather than
  relying on the tier's injection. That makes the arrangement legible at
  the read instead of only in the Go that stamps the principal — and it
  makes the failure loud: remove the principal and the read returns
  nothing, with the filter explaining why.
- **The prefix is distinct from `system:automation:`.** The two
  principals differ in exactly the way that matters, so a `createdBy`
  stamp, an audit line and a log field all record which one ran.

**Why the list is compiled in and not a DSL annotation.** An
`@maintenance` automation annotation was the obvious shape and is the
wrong one: `MEMQL_DSL_PATH` mounts product DSL from disk at boot, so a
DSL annotation conferring cluster-owner authority would let a product
bundle grant *itself* the cluster's maintenance principal — privilege
escalation by dropping a file into a volume. The list is in Go, so a
mounted bundle cannot reach it. It is also why `auth.MaintenanceActor`
being exported confers nothing: the authority comes from the list, so a
name that is not on it gets `nil` however loudly it asks.

**When NOT to reach for it.** Only for reads that span owners *by
nature*. A server-side path that merely finds an owner-scoped read
inconvenient should borrow exactly ONE owner's authority with
`auth.ContextWithUserActor` — the shape `component/worker`'s store, the
campaigns drain worker and the workbench integration all use.

Implementation: `component/auth/maintenance_actor.go`, consumed by
`component/automations/executor.go`. Two entries today, both retention
sweeps: `workerInvocationRetentionSweep` and
`auditEventRetentionSweep`. Measured by
`TestMaintenanceAutomationsAreArgued` (the list is pinned, every entry
argued, every name resolves to an automation that loads, and the wire
from list to executor is asserted in both directions), and end to end by
`TestWorkerInvocationRetentionSweepStillRetiresARowUnderTheTier` and
`TestAuditEventSweepReadsUnderTheMaintenancePrincipalAndCreatesStillWork`
— both Postgres-gated and both built around a NEGATIVE control, because a
test where the sweep's read returns the row is equally satisfied by a
tier that is not enforced at all.

The audit one carries a second assertion worth naming: that
`createAuditEvent` still succeeds for an **ordinary** caller. The
`clusterOwner` tier would be a severe regression if it gated creates —
every sign-in, session and role change would stop being recorded — and
the reason it does not is that the write guard resolves a TARGET ROW, so
it covers updates and deletes only. That sentence is load-bearing, so it
is evidence rather than a claim.

### What is still undeclared, and what it is waiting for

memql#4366 named five concepts. Three are now settled:
`v1:worker:registration` (memql#4349), `v1:worker:invocation`
(memql#4406) and `v1:identity:auditEvent` (above). The **planner
trio — `v1:planner:plan`, `v1:planner:task`, `v1:planner:taskState` —
is not**, and the reason is specific enough to write down so the next
attempt starts from it:

- **Every internal reader is chicken-and-egg.** `planById` has seven Go
  call sites (`integrations/workbench`, `integrations/agent/worker`,
  four in `integrations/planner`, plus `integrations/cognition`), and
  every one of them is a `loadPlan(ctx, planId)` helper that takes a
  plan id and *no owner*. `integrations/workbench`'s `resolvePlanOwner`
  is the clearest case: it reads the plan **in order to discover the
  owner**, which an owner-gated read cannot answer. So the fix is not a
  stamp at each call site — the value to stamp is not in hand.
- **The maintenance principal is the wrong tool here.** It would work
  mechanically and make the tier decorative for exactly the code that
  touches plans most: declared, and unenforced where it matters. The
  right shape is threading the owner down from wherever the plan id
  came from, which is a refactor of the planner's dispatch plumbing.
- **`plansForSpace` is no longer the blocker it was recorded as.** It
  has no live consumer at all — only the generated SDK and the gate
  fixtures — so the "reads collaborators' rows BY DESIGN" ruling
  (`cmd/memqlmigrate/rowauthz_infer.go`) is now a statement about dead
  surface. Deleting it, or scoping it, is a decision available for free.
- `task` and `taskState` additionally need a new `ownerUserId` field
  with server stamping at four mutations.

And it touches the planner agent loop, which
[llm-cost-control.md](../../ai/llm-cost-control.md) asks to be read
first. Nexus's client-side filters (memql#4369, above) stand in
meanwhile and need no change when the tier lands.

## Related issues

- #55 — JWT claims → caller envelope contract
- #56 — Remove partitioning (blocked on this audit completing)
- #57 — id cleanup (independent; already in flight)
- #2803 — the ruling that concept-declared row authz is worth building,
  scoped to Phases 1–2 with the enforcement decision deferred until the
  measurement exists
- #2920 — Phase 1: this section's vocabulary + loader validation, inert
- #2921 — Phase 2: shadow mode, measuring what enforcement would change
