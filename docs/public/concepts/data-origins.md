---
title: "Data origins: Mirror, Origin, Native"
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# Data origins: Mirror, Origin, Native

**MemQL is the origin of what it owns, a faithful mirror of what it does
not, and every concept says which.**

That sentence is the whole feature. MemQL aims to centralise a
business's memory and to be the source of truth of as much of it as it
can — but it cannot be the source of truth of everything. A client with
a working Shopify store keeps checkout, payment, tax and fulfilment
there, and that store must lose no function on the day MemQL arrives. So
MemQL holds a complete, faithful copy of what it does not own, keeps it
in sync, and can take ownership of a domain later without re-plumbing.
And a person reading a row must be able to tell which of those it is.

## The three states

| State | Changes are made at | Who else holds a copy | Writable in MemQL by a user or agent |
|---|---|---|---|
| **Mirror** | an external origin | MemQL holds the mirror | **no** — read-only by construction |
| **Origin** | MemQL | external systems hold mirrors, kept in sync outbound | yes, and every change propagates |
| **Native** | MemQL | nobody | yes |

Examples: Shopify products are a **mirror**. A wholesale price list MemQL
owns and pushes to Shopify B2B is an **origin**. Plans, agents, audit
rows and memories are **native** — which is what all but a handful of
concepts are.

### There is no fourth state

"Shared" — two systems both authoring one domain, with conflict rules —
is the option the model rejects, and the vocabulary makes it un-sayable:
a concept has exactly one origin. Two systems authoring one domain does
not produce a merged truth; it produces two truths and a reconciliation
policy nobody can state in advance. If MemQL should own a domain, move
the origin (see below). If it should not, mirror it.

## The two declarations

```memql fragment
@origin("shopify")
concept product { ... }                     // a MIRROR

@origin("memql")
@mirroredTo("shopify")
concept creditLimit { ... }                 // an ORIGIN with an external mirror

concept plan { ... }                        // NATIVE: no declaration needed
```

- `@origin("<name>")` — **where changes to this concept are made**.
  Absent means `"memql"`. An external name makes the concept a mirror.
- `@mirroredTo("a", "b")` — **who else holds a copy** of this
  MemQL-origin concept. Name every target in one annotation.

`dataState` is **derived**, never authored: `mirror` when the origin is
external, `origin` when MemQL originates it and at least one target is
named, `native` otherwise. It is exposed on every concept descriptor
(`ConceptInfo.dataState` / `.dataOrigin` / `.dataMirroredTo`, and the
same three in both SDKs), so a client renders a badge without parsing
DSL. `dataOrigin` is never empty: a concept that declared nothing reports
`"memql"`.

Note that a **construct's** `origin` is a different field answering a
different question — where its *source file* lives (`core` / `bundle` /
`promoted` / `staged`). The wire names them apart for that reason.

**Refused at load:** `@mirroredTo` beside an external `@origin`. A mirror
that also publishes is a second origin wearing the first one's badge;
re-mirroring is the origin's job, not MemQL's.

**Refused at boot:** an `@origin` or `@mirroredTo` naming a connector
this build does not serve. That refusal is not strictness for its own
sake — neither failure is visible at runtime. A mirror nobody fills is an
empty concept that reads as an empty catalog: every query succeeds and
returns nothing. A mirror target nobody drains is an outbox that
accumulates entries forever: every write succeeds and nothing arrives.
Both are silent data loss dressed as normal operation.

## What a reader may assume from a badge

A row rendered "Mirror of shopify" carries one guarantee, and it is worth
stating exactly: **nobody in MemQL has edited this, because nobody in
MemQL can.** The engine refuses every write to a mirror concept —
mutation, tool handler, raw insert, staged write — unless it comes from
the connector the concept's `@origin` names. The refusal is typed
(`mirror_write_refused{origin}`) and audited with the actor.

Two escapes that exist elsewhere in the engine deliberately do **not**
apply here:

- **Trusted internal server-side Go.** That stamp says *the engine is
  writing*, which is true of the connector and of every other internal
  path as well. It answers the wrong question.
- **The cluster owner.** On an ordinary owned-tier concept, refusing the
  operator would make administration impossible. Here it would not help
  them: an operator's edit to a mirror is reverted by the next
  reconciliation sweep exactly like anyone else's. Refusing is the honest
  answer; accepting would be a write that appears to work and does not
  last.

The badge is only worth reading because of that. A mirror a
sufficiently-privileged user could edit would be a row MemQL believes and
the origin has never heard of.

## Connectors

A **connector** is the code that fills a mirror from its origin, or
pushes a MemQL-origin change out to an external mirror. It is not a
fourth extension word: a connector is an *integration* that implements
the connector contract (see
[component vs integration vs pack](component-integration-pack.md)). What
makes an integration a connector is that a concept names it and the
connector registry can find it under that name.

A connector runs under `auth.ConnectorActor(<name>)` — an
`AccessContext` no request can mint. Row admission gives it a targeted
rule rather than a blanket bypass: **admitted to the concepts whose
`@origin` or `@mirroredTo` names it, regardless of tier, and to nothing
else.** The Shopify connector cannot read a campaign. Details and the
reasoning are in
[per-row authorization](../operate/auth/per-row-authz-audit.md).

The contract itself lives in `component/memql/sync`:

| Method | What it does |
|---|---|
| `Name` | the name concepts write in `@origin` / `@mirroredTo` |
| `Domains` | the concepts it handles, with each one's version field |
| `Apply` | one staged inbound delivery → version-stamped mirror writes |
| `Backfill` | resumable pages of the origin's current state |
| `Reconcile` | origin vs mirror, heal, count drift |
| `Propagate` | push one MemQL change out |
| `EnsureSubscriptions` | register or verify the origin's webhooks |

A connector that does not serve one of these yet returns a typed
not-implemented error, which the runtime distinguishes from a delivery
failure — an unconfigured capability is reported, not retried and
dead-lettered.

**Registration has two halves, and the split is deliberate.**
`sync.Declare(name)` runs from an `init()` and states that *this build
knows how to serve a connector by this name*; `sync.Bind(c)` attaches the
implementation once the runtime can construct it. The engine's boot check
reads the first, because it runs inside `MemQLEngine.Init` and the
bootstrap order is config → database → engine → integrations. It is also
an honest distinction on its own terms: a cluster with no Shopify
credentials still loads Shopify's concepts — it simply has no connector
bound to fill them, which is an operational condition rather than a boot
failure.

## Moving an origin

Taking ownership of a domain MemQL currently mirrors is a **runbook, not
a button**, and it is deliberately not automated: the step that matters
is a judgement about when the other system stops being authoritative.

1. **Backfill the other side.** Whatever MemQL will own must exist in
   MemQL completely before anything is switched. Run the connector's
   backfill to completion and check the drift count is zero.
2. **Freeze writes at the old origin.** Until this is true, both systems
   are authoring and the state the vocabulary refuses to name is exactly
   what you have.
3. **Flip the declaration.** Change `@origin("<connector>")` to
   `@origin("memql")` and add `@mirroredTo("<connector>")` if the old
   origin should keep receiving changes. The concept's `dataState` moves
   from `mirror` to `origin` (or to `native` if nothing mirrors it), and
   the write guard stops refusing user writes on the next boot.
4. **Verify the direction reversed.** A write in MemQL should now appear
   at the old origin. If it does not, the mirror targets are declared and
   undrained — the condition the boot check catches for an unregistered
   name but cannot catch for a registered one that is not running.

Reversing the move is the same list with the systems exchanged. Tooling
for it follows the first real move; the runbook is what exists now.

## The runtime

The declarations above say what is true; the runtime is what makes it
happen. It lives in `component/datasync` (the *contract* is in
`component/memql/sync`; the split is an import-cycle fact and the package
doc explains it).

**The outbox.** A write to an `origin` concept appends one
`v1:platform:outboxEntry` per `@mirroredTo` target **in the write's own
transaction**. That is not belt-and-braces: append-first-write-second
leaves an entry describing a change that never happened, and
write-first-append-second leaves a committed change nothing will ever
propagate. Only a transaction closes both, and a process dying between
two statements dies there more often than anywhere else. The transaction
is opened *only* for an origin concept — every other write keeps its
single-statement path.

Each entry carries an idempotency key of `(concept, row, version,
target)`, rendered once at append time and stored, so every attempt
presents the receiver the same key. The entry's own row id is derived
from the same tuple, so a replayed write appends the same entry rather
than a second delivery of one change.

**The drain worker** delivers entries oldest-first per connector, under a
cluster claim so exactly one replica drains a given connector at a time.
Every outcome is terminal or scheduled, never silent: `delivered`,
`failed` with a doubling backoff, or `dead` after
`MEMQL_SYNC_OUTBOX_MAX_ATTEMPTS` attempts. The attempt is counted at
*claim* time, so a worker that dies mid-delivery still spends one — a
crash-looping delivery must reach the ceiling rather than spin forever. A
connector that does not implement `Propagate` yet has its entries
**parked, not dead-lettered**: an unconfigured capability is not a
delivery failure.

A dead letter is an operator's decision, and there are exactly two:
retry it (attempts reset — the operator presumably fixed the cause) or
discard it (the row survives as audit history carrying the reason).

**Inbound** is an automation on `v1:platform:inboundRequest.created`: it
routes the staged delivery to the connector its `source` names, calls
`Apply` under that connector's actor, and writes what comes back behind
the **version guard** — a write older than what MemQL holds is recorded
`stale` and skipped, so an out-of-order webhook cannot regress a mirror.
The request row is stamped `processed` or `failed` either way.

What "older" means depends on what the origin gives, and
`DomainSpec.VersionField` is how a connector says which: a field on the
mirror row holding the origin's own version (exact), or — when the origin
publishes none — the delivery time compared against the row's `createdAt`
(coarser, and honest about it).

**Backfill and reconciliation** fill the same gap from different ends. A
webhook stream tells you what changed *after* you started listening;
backfill reads what was already there, and reconciliation compares the
two systems forever. Both write through the *same* version-guarded path
inbound uses — a sweep with its own write path would apply an old
snapshot over a new webhook and heal the mirror backwards. Backfill
persists its cursor after **every page**, so a restart resumes rather
than restarts.

**Health is a row.** `v1:platform:syncState`, one per (concept,
connector, direction), carries the backfill cursor and status, last
inbound time, lag, last reconciliation, drift count, outbox depth, dead
letters, and the operator pause switch. Its id is deterministic, so the
append-only history of that row *is* the domain's health timeline.

`MEMQL_SYNC_ENABLED=false` stops **delivery** and nothing else: a mirror
stays read-only, the outbox is still appended, every declaration is still
enforced. Turning off the worker must not turn off the invariants, so a
paused cluster accumulates work rather than losing it.

## See also

- [Component vs integration vs pack](component-integration-pack.md) — the three extension words
- [Per-row authorization audit](../operate/auth/per-row-authz-audit.md) — the connector actor's admission rule
- [Inbound delivery](../operate/inbound-delivery.md) — how a webhook becomes a staged row
- `dsl/_reference/_concept.memql` section 14 — the authoring skeletons
