# Data origins -- Mirror, Origin, Native: how MemQL relates to data it does not own

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project J0, the platform half of the tenth sub-project)
**Owner:** `component/memql` (declarations, enforcement, the outbox), `component/language` (the annotations), `integrations/` (the connector contract), `clients/portal`, `docs/public/concepts`

The tenth sub-project of the 2026-08-22 backlog brief was born inside F
("the Shopify storefront must keep track of everything"), and its first
decision turned out to be about the platform, not about Shopify: **what is
MemQL's relationship to data whose system of record is somewhere else?**
This record defines that relationship as a named feature with three states,
the declarations that express it, the enforcement that makes the words
true, and the contract a connector implements. J1 (the Shopify connector)
is the first full implementer.

---

## 1. The problem, in the owner's words

MemQL aims to centralise a business's memory and to be the source of truth
of as much of it as it can. It cannot be the source of truth of everything:
a client with a working Shopify store keeps checkout, payment, tax and
fulfilment there, and that store must lose no function on the day MemQL
arrives. So MemQL must hold a **complete, faithful copy** of what it does
not own, keep it in sync, and be able to **take ownership** of a domain
later -- pushing its changes out -- without re-plumbing. And people who
read MemQL must be able to tell, for any row, which of those it is.

The tree at `e479c8fc` has the pieces and not the concept:

- **Inbound**: `POST /inbound/{source}` stages deliveries as
  `v1:platform:inboundRequest` rows with per-source HMAC verification,
  dedupe, and redelivery idempotency (memql#2957,
  `docs/public/operate/inbound-delivery.md`); a product writes an
  automation on `node.created` to work them.
- **One mirror, unnamed**: `v1:shopify:shopifyProduct` (`dsl/shopify/concepts.memql:18`)
  is a thin index kept by `applyInboundProduct` / `reconcileProduct` /
  `reconcileIndex` (`integrations/shopify/capabilities.go:34-63`). Nothing
  says it is a mirror; nothing stops a user writing it; no page shows its
  sync health.
- **Outbound**: nothing generic. `integrations/shopify` pushes nothing; the
  campaigns drain worker (`component/campaigns/worker.go`) is the one
  durable, retried, audited outbound loop in the tree, and it is specific
  to email.
- **Internal actors are uncharacterised** (memql#4366): engine-internal
  reads run with no `AccessContext` and are refused by owned-tier concepts.
  A connector is exactly an internal writer that needs a named identity.
- **The registry already carries per-construct metadata** to clients
  (`ListConstructs`, memql#3749/#3750; `ConceptsListMsg`), which is where
  an origin badge would come from without parsing DSL.

---

## 2. The three states

| State | Where a change is made | Who else holds a copy | Writable in MemQL by a user or agent | Examples |
|---|---|---|---|---|
| **Mirror** | an external origin | MemQL holds the mirror | **no** -- read-only by construction; change it at the origin, or move the origin | Shopify orders, products, customers |
| **Origin** | MemQL | external systems hold mirrors, kept in sync outbound | yes, and every change propagates | wholesale price lists pushed to Shopify B2B; agent-written product content |
| **Native** | MemQL | nobody | yes | plans, agents, audit, memories, constructs |

The sentence for the docs: **MemQL is the origin of what it owns, a
faithful mirror of what it does not, and every concept says which.**

There is no fourth state. "Shared" -- two systems both authoring one
domain with conflict rules -- is the option the owner rejected for Shopify
and the vocabulary makes it un-sayable: a concept has exactly one origin.

---

## 3. Decisions

### D1 -- The words are Mirror, Origin, Native

Chosen over "source of truth / replica" (the first is the phrase people
wrongly assume about MemQL; the second reads as a database copy) and over
"master / mirror" (precise in MDM, carries connotations, does not pair with
the inbound direction). One symmetric pair plus the case with no partner.

### D2 -- Two declarations produce all three states

`@origin("memql" | "<connector>")` on a concept, absent meaning `memql`;
`@mirroredTo("<connector>", ...)` on a MemQL-origin concept. Derived
`dataState` is `mirror` when the origin is external, `origin` when MemQL
originates it and at least one mirror target is named, `native` otherwise.
A named connector that is not registered refuses boot: a mirror with
nobody to fill it is a lie, and a mirror target nobody drains is a silent
drop.

### D3 -- A mirror is read-only by construction

The write guard refuses a user or agent write to a mirror concept --
mutations, raw inserts, tools, staged writes -- with a typed error that
names the origin. Only the connector's apply path writes it, and only under
the connector's actor. This is the property that lets a reader trust the
badge.

### D4 -- Connectors are named actors

A connector runs under `auth.ConnectorActor(name)`: an `AccessContext`
whose role is `connector`, admitted to write the mirrors whose `@origin`
names it, to read the concepts it mirrors or propagates, and to act as the
cluster for reconciliation. A targeted rule in row admission, not a
blanket bypass: the connector for `shopify` cannot read `campaigns`. This
is the characterised internal actor memql#4366 asks for, for this class
of writer; the planner's system actor is a separate decision and is not
touched here.

### D5 -- Origins propagate through a durable outbox

A write to an `origin` concept appends one `v1:platform:outboxEntry` per
mirror target, ordered per row, in the same transaction as the write. A
drain worker per connector delivers entries through `Propagate` with an
idempotency key (concept, row, version, target), retries with backoff,
dead-letters after a bounded number of attempts, and audits every outcome.
The campaigns drain worker is the pattern, generalised.

### D6 -- Apply is version-guarded

Every inbound mirror write carries the origin's version (`updated_at`, a
sequence, or the delivery timestamp when the origin offers nothing
better). A write whose version is older than the stored row's is recorded
as `stale` and not applied, so out-of-order webhooks cannot regress a
mirror; MemQL's own insert-versioning keeps the full history regardless.

### D7 -- Health is a row, not a log line

`v1:platform:syncState`, one per (concept, connector): backfill cursor and
status, last inbound event, lag, last reconciliation, drift count, outbox
depth, dead-letter count, paused. The Data origins page reads it; the
connector's automations write it.

### D8 -- The first implementer is the product path that already exists

`v1:shopify:shopifyProduct` declares `@origin("shopify")`; its
`applyInboundProduct` / `reconcileProduct` / `reconcileIndex` become the
Shopify connector's `Apply` / `Reconcile` for one domain, under the
connector actor, with no behaviour change. That proves the contract before
J1 widens it to the whole store.

---

## 4. The declarations, the registry, the enforcement

### 4.1 DSL

```memql
@origin("shopify")
concept shopifyProduct { ... }            // a mirror

@origin("memql")
@mirroredTo("shopify")
concept wholesalePriceList { ... }        // an origin with an external mirror

concept plan { ... }                      // native: no declaration needed
```

`component/language`: both annotations parsed on `concept` only; string
arguments; `@mirroredTo` refused on a concept whose origin is external (a
mirror cannot also be mirrored onward from here -- re-mirroring is the
origin's job). `dsl/_reference/_concept.memql` documents the forms.

### 4.2 Registry

`ConceptInfo` / `ListConstructs` carry `origin`, `mirroredTo[]`,
`dataState`; the TS and Go SDKs surface them. A virtual read,
`dataOrigins`, lists every concept's state and connectors (the
`v1:router:modelCatalog` pattern: produced from the live registry, never
persisted); `syncStateFor(concept, connector)` reads the persisted health.

### 4.3 Enforcement in the engine

- `executeWrite` (`component/memql/executor_mutation.go:501`): when the
  target concept's `dataState` is `mirror` and the actor is not the
  connector named by its origin, refuse with
  `mirror_write_refused{origin}`; the refusal is audited with the actor.
  Staged writes, raw inserts and tool handlers all pass through the same
  seam (the write guard's existing position).
- When the target concept's `dataState` is `origin`: after the row write,
  in the same transaction, append an `outboxEntry` per target in
  `@mirroredTo` with the row's new version.
- Row admission (`rowauthz_enforce.go`): a `ConnectorActor(name)` is
  admitted to rows of concepts whose `@origin` or `@mirroredTo` names
  `name`, regardless of tier; to nothing else. Tests pin both sides.

### 4.4 `v1:platform:outboxEntry` and `v1:platform:syncState`

`outboxEntry`: `concept`, `rowId`, `action`, `version`, `target`,
`status enum(pending, delivering, delivered, failed, dead)`, `attempts`,
`nextAttemptAt`, `lastError`, `idempotencyKey`, `createdAt`. Cluster-owner
tier; the drain worker reads under the operator identity (the campaigns
precedent).

`syncState`: `concept`, `connector`, `direction enum(inbound, outbound)`,
`backfillCursor`, `backfillStatus enum(none, running, complete, failed)`,
`lastInboundAt`, `lagSeconds`, `lastReconcileAt`, `driftCount`,
`outboxDepth`, `deadLetterCount`, `paused`. Cluster-owner tier; written by
the connector runtime and its automations.

---

## 5. The connector contract

In Go, registered through the plug-in system (`memql.RegisterPlugin`,
`component/memql/plugins.go:240`) with a `Connector` interface the plug-in
factory may additionally satisfy:

```go
type Connector interface {
    Name() string                                        // the origin / target name used in DSL
    Domains() []DomainSpec                               // concepts it originates or mirrors, with version fields
    Apply(ctx, req InboundRequest) ([]MirrorWrite, error) // staged webhook -> version-stamped mirror writes
    Backfill(ctx, concept string, cursor string) (BackfillPage, error) // resumable pages
    Reconcile(ctx, concept string, since time.Time) (ReconcileReport, error) // origin vs mirror, heal, count drift
    Propagate(ctx, entry OutboxEntry) (PropagateResult, error)           // push one MemQL change
    EnsureSubscriptions(ctx) error                       // register / verify the origin's webhooks
}
```

The runtime around it (`component/memql/sync/`): the inbound dispatcher
(an automation on `inboundRequest.created` routes to the connector named by
`source`, applies under the connector actor, stamps `processed` /
`failed`), the backfill runner and the reconciliation runner (scheduled
automations per domain, progress on `syncState`), the outbox drain worker
per connector (one goroutine per node with an advisory lock, the campaigns
shape), and the pause switch.

---

## 6. The portal and the docs

- **Origin badge** on every row and concept header from registry metadata:
  "Mirror of shopify · synced 2 min ago", "Origin → shopify", "Native".
  Rendered in the generic path without a concept literal
  (`portal_render_path_test.go`).
- **Data origins** page (`/data-origins`, Cluster group): every concept
  with state, connectors and health from `dataOrigins` + `syncState`; per
  connector and domain: backfill now, reconcile now, pause / resume, the
  dead-letter queue with retry / discard -- cluster owners only.
- `docs/public/concepts/data-origins.md`: the three states and the
  sentence; the declarations; what a reader may assume from a badge; how
  an origin moves (declare the new origin, backfill the other side, flip --
  a runbook, not a button); what a connector implements. `GLOSSARY.md`
  entries for Mirror, Origin, Native, Connector, Outbox. CLAUDE.md "Key
  Concepts" gains the section; `integrations/CLAUDE.md` gains the connector
  recipe; `component-integration-pack.md` names a connector as an
  integration that implements the contract.

---

## 7. Security posture

| Concern | Handling |
|---|---|
| A user or agent edits mirrored data and the origin never learns | refused by construction (D3), audited |
| A connector writes outside its domains | the connector actor is admitted only where its name is declared (D4) |
| A replayed or late webhook overwrites newer data | version guard (D6); the stale write is recorded |
| An outbound change lost or doubled | durable outbox, idempotency key, dead-letter with an operator queue (D5) |
| A mirror that nobody fills | boot refuses an unregistered connector name (D2) |
| Secrets | connector credentials stay in `globalSecret` / the secret store; the runtime never logs a payload body |

---

## 8. Testing

1. Parser: both annotations; `@mirroredTo` on an external-origin concept
   refused; an unregistered connector name refuses boot.
2. Registry: `dataState` derivation for all three; the SDKs surface it.
3. Write guard: a user, an agent tool, a raw insert and a staged write to a
   mirror are refused with the origin named; the connector actor's write
   succeeds; a different connector's write is refused.
4. Row admission: the connector actor reads its domains and nothing else.
5. Outbox: append in the write's transaction; drain delivers once under
   redelivery; backoff; dead-letter after N; the operator queue retries.
6. Apply: an out-of-order fixture leaves the newer row; `stale` recorded.
7. Backfill resumes from its cursor; reconciliation heals a seeded drift
   and counts it.
8. The migrated product path: identical rows and identical audit to before.
9. Portal: badges on rows and headers; the page; the actions.
10. Cluster: the drain worker runs on exactly one replica at a time
    (advisory lock), and the inbound dispatcher on any.

---

## 9. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the declarations and the actor | annotations, registry, `dataState`, the connector actor + row admission rule, the write guard, the `Connector` contract, the product path migrated | nothing |
| 2 -- the runtime | `outboxEntry` + drain worker, the inbound dispatcher, backfill and reconciliation runners, `syncState`, the automations | PR 1 |
| 3 -- the portal and the docs | badges, the Data origins page, the concept doc, glossary, CLAUDE.md, the connector recipe | PR 1 (badges), PR 2 (health) |

One `Closes #N` line per issue. Eight tasks.

---

## 10. Out of scope

- The Shopify connector beyond the existing product domain -- J1.
- Moving an origin (the runbook is documented; tooling for it follows the
  first real move).
- Bidirectional or shared authorship (rejected by the model).
- Characterising the planner's system actor (memql#4366).
- Outbound delivery for native concepts (by definition none).

---

## 11. References

- Code: `component/memql/{executor_mutation,rowauthz_enforce,rowauthz_write_guard,plugins,concept_registry_broadcast}.go`,
  `component/language` (annotation registry), `component/campaigns/worker.go`,
  `component/server/inbound*.go`, `dsl/platform/concepts.memql`,
  `dsl/shopify/*.memql`, `integrations/shopify/*.go`, `dsl/router/concepts.memql`
  (the virtual-catalog pattern).
- Docs: `docs/public/operate/inbound-delivery.md`, `docs/public/concepts/component-integration-pack.md`,
  `docs/public/operate/auth/per-row-authz-audit.md`.
- Related: epic #4339 (F: the storefront kind), #4366 (internal actors),
  #4368 (the subscription seam), memql#2957 (inbound delivery).
