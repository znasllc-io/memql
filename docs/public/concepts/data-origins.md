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
concept shopifyProduct { ... }              // a MIRROR

@origin("memql")
@mirroredTo("shopify")
concept wholesalePriceList { ... }          // an ORIGIN with an external mirror

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

## What is not covered here

The outbox that carries an origin concept's changes outward
(`v1:platform:outboxEntry`), the per-connector drain worker, the inbound
dispatcher, the backfill and reconciliation runners, and the per-domain
health row (`v1:platform:syncState`) are the *runtime* half of this
feature. They are described where they land.

## See also

- [Component vs integration vs pack](component-integration-pack.md) — the three extension words
- [Per-row authorization audit](../operate/auth/per-row-authz-audit.md) — the connector actor's admission rule
- [Inbound delivery](../operate/inbound-delivery.md) — how a webhook becomes a staged row
- `dsl/_reference/_concept.memql` section 14 — the authoring skeletons
