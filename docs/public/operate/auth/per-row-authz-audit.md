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
> All 11 flagged constructs are now classified via `@public` with
> per-construct comments documenting the intent + the follow-up
> tightening path. The classification test
> (`dsl.TestPerRowAuthzClassification`) hard-fails on any new
> flagged construct.

## Context

memQL currently relies on **partition-as-isolation-boundary** for
defense-in-depth: a request authenticated as user X can only read
rows under partition X (enforced by `PartitionACL` middleware in
`component/auth/access/middleware.go`). If a DSL query has a bug
that allows reading rows it shouldn't, the partition boundary
still catches the worst leaks.

Issue #56 removes partitioning. Before that lands, every read +
write path in the DSL needs an explicit caller-check so the
removal doesn't demote defense-in-depth to a single point of
failure.

## The four buckets

Every query and mutation in the DSL falls into exactly one of these:

| Bucket | Definition | Required gating |
|---|---|---|
| **owned** | Row carries `payload.ownerUserId` (or `payload.userId` for identity-domain concepts) | `filter` must include `payload.ownerUserId == actor.userId` (the caller can only read rows they own) |
| **granted** | Row visible via a relationship (e.g. space participant, group member) | Filter must reference a relationship spec that gates on `actor.userId` |
| **admin** | Cluster-owner-only (e.g. audit log, identity admin views) | Compose `spec("requiresClusterOwner")` or equivalent |
| **public** | Globally readable by intent (concept catalogs, role registry, public lookup tables) | `@public` annotation on the construct |

The `@public` annotation is a marker for the validator — it has no
runtime effect. Adding `@public` to a construct is the author's
explicit acknowledgement that "yes, this is meant to be visible to
unauthenticated callers / cross-user reads / etc."

## Declaring the tier: `@rowAuthz` on the concept

The four buckets above are inferred, per construct, by a regex. That
is the defect #2803 rules on: a construct's caller-check lives in a
term the author has to remember to type, and deleting it leaves a
construct that still loads, still passes every gate, and returns
every row. **Absence has no syntax**, so review cannot catch it.

`@rowAuthz(...)` moves the bucket onto the **concept**, declared once,
where a new query over that concept inherits it instead of having to
restate it:

| Bucket | Declaration | Predicate it will eventually inject |
|---|---|---|
| owned | `@rowAuthz(owner="<field>")` | `<field> == actor.userId` |
| admin | `@rowAuthz(clusterOwner)` | `actor.isClusterOwner == true` |
| public | `@rowAuthz(public)` | none — explicit and greppable |
| granted | `@rowAuthz(via="<spec>")` | the relationship spec, gated on `actor.userId` |

```memql
@rowAuthz(owner="requestedBy")
concept plan { ... }

@rowAuthz(clusterOwner)
concept call { ... }
```

Rules:

- **One tier per concept.** Two different tiers inside one annotation
  is a load error, and so is a second `@rowAuthz` on the same concept
  (the parser folds attributes in source order, so without that check a
  reader scanning top-down would see a tier the engine does not use).
- **`owner="<field>"` must name a field the concept declares.**
  Checked at load, against the parsed property set, with the declared
  fields listed in the diagnostic.
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
detector / loader / codemod reads the row-authz surface.

A concept with no declaration still loads. It produces one aggregated
boot **warning** naming every undeclared concept; escalation to a load
error is a later phase, once the tree is clean.

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

`userById` **bootstraps the actor**:
`component/auth/identity_resolver.go` calls it to resolve `sub` → user
in order to *build* the `AccessContext`. So `actor.userId` is circular
for the one construct that creates the actor, and any rule assuming
"every user-scoped read is expressible as a filter over the actor" is
false there.

Phase 1 needs no answer, because nothing is enforced. It does need the
grammar to be **able** to express one, and it is: bare-flag tiers
(`public`, `clusterOwner`) and keyword tiers (`owner=`, `via=`) occupy
separate namespaces inside the argument list, so a fifth tier is a new
flag or a new keyword and disturbs neither. See #2803's thread for why
the escape hatch must be argued from Phase 2 data rather than granted
as an exemption knob.

## Validator

`dsl.TestPerRowAuthzClassification` walks every query and mutation
in the tree and classifies each one. The test logs counts per
bucket and emits a flagged list of constructs that look user-scoped
but lack an actor-check (the `actor.userId == ...` reference or a
known actor-scope spec).

The test is **informational** today (logs findings; does not fail
the build). Once each domain's gaps are closed (follow-up PRs per
issue #54), the test flips to hard-fail.

## Snapshot at audit time (2026-05-20)

Aggregate counts across the DSL tree:

| Domain | Queries | Mutations | Notes |
|---|---|---|---|
| agents | 18 | 6 | `ownerUserId` on the row; most queries take `ownerUserId` as an arg without cross-checking `actor.userId`. Owner-only and admin-only paths both present. |
| cluster | 8 | 6 | Cluster topology — admin-only by intent. |
| cognition | 28 | 29 | Space + participant + utterance. Mixed: some owner-only, some space-participant-granted. |
| common | 0 | 0 | (no queries / mutations) |
| data | 10 | 8 | Data domain — needs classification pass. |
| identity | 76 | 36 | Largest domain. Mix of admin (audit events), owner (user preferences), and public (JWKS, login pages). |
| knowledge | 26 | 16 | Knowledge domains + documents — mix of workspace-scoped + private-per-user. |
| memql | 0 | 0 | (no queries / mutations) |
| planner | 17 | 11 | Per-user plans + tasks. |
| platform | 16 | 11 | Platform metadata. Some admin-only, some public. |
| router | 2 | 2 | Router ledger — admin/internal. |
| workbench | 4 | 3 | Per-Plan workspace. |
| worker | 12 | 7 | Per-user worker invocations. |

**Total:** 217 queries + 135 mutations across 11 domains.

## Per-domain gap closure (shipped)

The 11 flagged constructs identified by the classification test
have been classified via `@public` with per-construct comments
documenting the intent. The classification breakdown after the
sweep:

```
domain          owned admin public  FLAG other
agents              0     0     2     0    13
cluster             0     0     0     0    10
cognition           2     0     0     0    41
data                0     0     0     0    13
identity            0     0     6     0    68
knowledge           0     0     1     0    28
planner             0     0     0     0    19
platform            0     0     0     0    19
router              0     0     0     0     3
workbench           0     0     0     0     5
worker              0     0     2     0    11
```

11 flagged → 0 flagged. The classification test hard-fails on any
new flagged construct going forward.

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
2. **Going-away-with-#56** — `queryAccessForUser`,
   `queryPartitionsForUser`. Tied to the partition concept that
   #56 removes wholesale; no point caller-scoping them now.
3. **Admin-only paths** — audit-event queries. The proper fix is
   composing a `requiresClusterOwner` spec; tracked under #54 once
   the admin surface is consolidated.
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

Examples of legitimate `@public` use:

- the cluster-topology queries in `dsl/cluster/queries.memql` — need to
  be readable on the unauthenticated cluster bootstrap path.
- `activeAgentRoles` — role catalog; no per-user data.

This list has drifted from the tree: neither bullet currently carries
`@public` (`activeAgentRoles` at `dsl/agents/queries.memql` does not, and
`dsl/cluster/queries.memql` has no `@public` at all). Take them as
illustrations of the argument below, not as an inventory of annotated
constructs. Reconciling the list — in either direction, per construct —
is memql#2918; note that the lesson at the end of this section cuts
against `@public` for anything reachable pre-auth.

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
