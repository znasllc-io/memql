# Row authorization on graph subscriptions -- the one egress that never asked

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project B of nine)
**Owner:** `component/grpc` (fan-out), `component/memql` (row authz), `component/language` (the tier grammar)

Sub-project B of the 2026-08-22 backlog brief. It is a prerequisite for Nexus
(sub-project I), which is built on subscriptions, and it changes what the
portal's live band can receive today.

---

## 1. Problem

A signed-in stream can `Subscribe` to any concept's change feed and receive
every user's rows. `handleBusEvent` (`component/grpc/server.go:1173-1223`)
matches the event topic against each subscription's patterns and sends the
whole flattened payload; `handleSubscribe` (`:2035-2135`) has no role or owner
gate; the dispatch at `:1692` applies none. The events bus itself has "no
AccessContext and no authorization hook of any kind"
(`component/memql/executor_mutation.go:1039-1042`).

Read against the row-authz model, the gap is specific:

- The engine has **one function** for "may this caller see this row":
  `rowAuthzAdmits(ctx, concept, id, payload)` (`component/memql/rowauthz_enforce.go:263`).
  It is what admits or denies a row on a raw client query string, on graph
  expansion, on a top-level builtin result, and -- through the write guard --
  on update and delete, "so who owns this row has one answer in both
  directions" (`rowauthz_enforce.go:14`). The `per-row-authz-audit.md` doc
  enumerates the egresses that depend on row admission. Subscriptions are the
  one egress of rows that never calls it, and the doc does not list them.
- For the **31 concepts that declare a tier** (`^@rowAuthz(` across
  `dsl/*/concepts.memql`: campaigns, library, notes, todos, calendar, sites,
  telephony calls, authored constructs, portal views, ...) this is a real
  leak: a read denies the row, the subscription delivers it.
- For the **~90 undeclared concepts** -- `plan`, `task`, `utterance`, `agent`,
  `auditEvent`, `user`, worker `registration`, `authSession` among them -- the
  read path already admits everyone: `rowAuthzAdmits`' undeclared branch
  returns admit, "unmeasured, in the undeclared gate's own words"
  (`rowauthz_enforce.go:264-274`), narrowed only for `user`'s PII fields. A
  subscription there leaks nothing a raw query cannot already read. That is
  the standing row-authz long tail (207 grandfathered reads in
  `rowauthz_undeclared_gate_test.go`), not a subscription bug -- but it is the
  reason a Nexus view of `plan` rows would be a view of everyone's plans.
- The `granted` tier cannot be decided from a row in isolation
  (`rowAuthzUndecided`, `:250-253`): its predicate is a relationship spec that
  needs the join only a filter performs.

Neither `docs/public/operate/auth/per-row-authz-audit.md` nor
`docs/internal/design/auth-threat-model.md` mentions subscriptions. The gap is
unrecorded, not accepted.

---

## 2. What the tree already has

### 2.1 Row admission is one function with three outcomes

`rowAuthzAdmits` returns `rowAuthzAdmit`, `rowAuthzDeny` or
`rowAuthzUndecided` from the row's own concept declaration. It reads the
caller from the context (`rowAuthzActorUserId`, `rowAuthzIsClusterOwner`) and
nothing from any filter. A stream session holds exactly one access context
(`streamSession.currentAccess()`, `server.go:1230`), so admission at fan-out
is one call per event per stream.

### 2.2 The tiers, and the one that is missing

`public` admits everyone. `clusterOwner` admits cluster owners and injects the
admin gate (`TestClusterOwnerTierInjectsTheAdminGate`). `owner="<field>"`
admits the row's owner -- **and nobody else**: `rowauthz_enforce.go:296-320`
has no cluster-owner bypass. `via="<spec>"` (granted) is declared by no
concept in `dsl/` today (only the reference skeleton shows it). There is no
form for "the owner, or a cluster owner", which is what an operator console
over per-user rows needs and, plausibly, why so much of the long tail is
still undeclared.

### 2.3 What the portal does with live events

`useConceptRows.ts:155-185` subscribes per concept; `liveBand.ts:1-49` puts
created rows in a separate "new since you opened" band and only counts
updates and deletes. `RowDetailDialog` always performs a fresh authoritative
read (`useConceptRows.ts:222-255`) rather than trusting the list copy -- the
exact pattern an id-only notification needs.

### 2.4 Subscription kinds

`SubscriptionKind` maps to topic prefixes (`component/events/proto.go:138-183`):
`TELEMETRY` -> `telemetry.#`, `MESSAGE` -> `message.#`, `AI_STREAM` -> `ai.#`
(`ai.completion.started/finished/error`), `GRAPH_EVENTS` -> `graph.#`, and
`ALL` -> `#`, which includes the graph topics. The portal uses only graph
subscriptions and the concept-registry follow.

### 2.5 Mesh delivery ends at the subscriber's node

Planner, cognition, cluster and identity graph topics are forwarded mesh-wide
(`component/node/routing.go`); a forwarded event is re-published on the
receiving node's bus and fanned out to that node's streams. Admission at
fan-out therefore runs where the subscriber is, with the subscriber's
context, and needs no forwarding change.

### 2.6 Tests run against a real engine

The row-authz tests use `probeEngine` with fixture concepts that declare each
tier (`rowauthz_enforce_test.go:140`); they are db-gated. A fake engine has no
gates, so fan-out tests must use the same harness.

### 2.7 Where task rows come from

Every `v1:planner:task` and `taskState` row is written by a DSL mutation
(`createSemanticTask`, `createToolInvocationTask`, `createTask`,
`persistTaskState` -- `dsl/planner/mutations.memql:53,82,410,442`); no Go code
inserts the concept directly. `plan.requestedBy` is `string!` and stamped from
`args.ownerUserId` at both plan-creating mutations (`:41`, `:262`).

---

## 3. Decisions

### D1 -- Mirror the read path; do not invent a second rulebook

Three options for undeclared concepts at the subscription seam: mirror the
read path (undeclared admits, declared enforces); default-deny undeclared for
non-owners on subscriptions only; declare all ~90 undeclared tiers in this
epic. The owner chose the first. A subscription that is stricter than a read
is a second authz implementation that will drift from the first; consistency
is what makes the model reviewable, and the hole closes concept by concept as
tiers are declared -- starting with the live-surface set in D6.

### D2 -- Admission at fan-out is the existing function

`handleBusEvent` calls `rowAuthzAdmits` with the stream's context for every
`graph.node.*` event. No new predicate, no copied logic; the PII narrowing on
`user` rows (`rowauthz_pii_unbound.go`) comes with it.

### D3 -- `Undecided` becomes an id-only notification

A `granted` row cannot be decided at fan-out, and silently dropping it would
make a future `via=` concept's live feed die without a trace. The
notification is sent with the payload omitted and a flag set; the client
re-reads through the authorized read path, which does the join. Today no
concept declares `via=`, so this path is built against a fixture.

### D4 -- Non-graph kinds are owner/admin-only

`TELEMETRY`, `MESSAGE`, `AI_STREAM` and `ALL` carry node-level events with no
row owner to decide by; `ALL` also carries every graph topic. They are refused
at subscribe time for callers below admin, and `ALL` gets graph admission by
topic prefix for the callers that may hold it. The portal uses none of them;
the cockpit is an operator console.

### D5 -- A composite tier: `@rowAuthz(owner="<field>", clusterOwner)`

"The owner, or a cluster owner." Filter injection ANDs
`(<field> == actor.userId) OR isClusterOwner`; row admission admits on either;
the write guard stays owner-only (a cluster owner reading a row is not a
cluster owner editing it as its author). Without this, declaring `plan` would
hide every other user's plans from the operator console, which is the wrong
trade and the likely reason the tail stayed undeclared.

### D6 -- Declare the live-surface set

`plan`, `task`, `taskState`, worker `registration`, `auditEvent` -- the rows
Nexus, the fleet section and the audit trail subscribe to, each with an
unambiguous owner field. Section 4.5 has the table.

### D7 -- What stays undeclared, on purpose

`utterance`, `participant`, `agent`: space-shared semantics; the right tier
is `granted` via participation, and that decision belongs to Nexus, which
will live with its id-only consequence. `authSession`, `magicLinkRequest` and
the other identity-internal rows: the identity service's own reads run under
an actor this design has not characterised, and an owned tier could deny the
service its own rows. Both groups keep their undeclared-gate entries; the
second gets a named follow-up in section 8.

---

## 4. The change

### 4.1 Fan-out

In `handleBusEvent`, for an event whose topic begins `graph.node.`:

```
concept, id, payload := from the event
switch rowAuthzAdmits(ctxWith(s.currentAccess()), concept, id, payload) {
case rowAuthzDeny:       drop (debug log, counter memql_subscription_rows_denied)
case rowAuthzAdmit:      send as today
case rowAuthzUndecided:  send {concept, id, action, createdAt}, payload_omitted=true
}
```

A stream with no access context (none resolved yet) is treated as an empty
actor, which the owned tier already denies ("no identity, no rows",
`rowauthz_enforce.go:302-305`). Non-graph topics are untouched here; D4 gates
them at subscribe time.

### 4.2 Wire and SDK

`EventNotification` (`component/grpc/memql.proto`) gains
`bool payload_omitted`. `subscribeGraph` in `sdk/ts/src/client/subscriptions.ts`
surfaces it on the delivered event. Nothing else on the wire changes; a client
that ignores the flag sees an event whose payload carries only the four keys.

### 4.3 Portal

`useConceptRows.ts`' live handler: an event with `payloadOmitted` is resolved
by the same authoritative read `useRowDetail` performs; a refused read drops
the event; an admitted one enters the live band as today. `liveBand.ts`'
policy (new rows in a band, updates counted) is unchanged.

### 4.4 Subscribe-time gate

`handleSubscribe`: kinds other than `GRAPH_EVENTS` require the stream's role
to be owner or admin; otherwise `PermissionDenied` with a reason string and a
`subscription_rejected` audit-free log line (it is a developer error, not a
security event). `ALL` subscriptions also pass through 4.1 for graph topics.

### 4.5 The composite tier

Grammar (`component/language`): `@rowAuthz(owner="<field>", clusterOwner)`;
the two arguments are order-independent; `owner` alone and `clusterOwner`
alone keep today's meaning. Enforcement (`rowauthz_enforce.go`): filter
injection produces `(<field> == actor.userId) OR <admin gate>`; row admission
returns admit on either branch; the write guard (`rowauthz_write_guard.go`)
ignores the second argument. The reference skeleton
(`dsl/_reference/_concept.memql`) documents the form; the conformance test's
classification accepts it as owned-tier for the authz bucket.

Declarations:

| Concept | Declaration | Work beyond the annotation |
|---|---|---|
| `v1:planner:plan` | `@rowAuthz(owner="requestedBy", clusterOwner)` | none; required and always stamped |
| `v1:planner:task`, `taskState` | `@rowAuthz(owner="ownerUserId", clusterOwner)` | new `ownerUserId string!` on both, stamped from the plan's `requestedBy` at the four creation mutations and their Go callers; a test walks every task and asserts owner == plan owner |
| `v1:worker:registration` | `@rowAuthz(owner="ownerUserId", clusterOwner)` | none; "every worker is owned by exactly one user" is already the rule; db-gated tests cover dispatch and the `workersForUser` read |
| `v1:identity:auditEvent` | `@rowAuthz(owner="actorUserId", clusterOwner)` | rows with an empty `actorUserId` (system actions) become owner-only -- intended: those are cluster facts |

Every retired undeclared-gate entry for these concepts is removed from
`rowauthz_undeclared_gate_test.go` with its reason string; the gate then
refuses any new unclassified read over them.

---

## 5. Security posture

| Threat | Before | After |
|---|---|---|
| Subscribe to a declared-owned concept, receive other users' rows | Delivered | Dropped at fan-out by the same function that denies the read |
| Subscribe to a clusterOwner concept as a reader | Delivered | Dropped |
| Subscribe to `v1:identity:user` | Every user's PII fields delivered | Narrowed exactly as the read path narrows them |
| Subscribe to `plan` / `task` / `registration` / `auditEvent` | Everyone's rows (undeclared) | Own rows, or all rows for a cluster owner |
| Subscribe to `ALL` / `TELEMETRY` / `AI_STREAM` as a reader | Delivered | Refused at subscribe time |
| A future `granted` concept | Would have been delivered | Id-only; the client's read does the join |
| Undeclared concepts not in D6 | Everyone's rows | Unchanged -- and unchanged on reads too; tracked by the undeclared gate |

---

## 6. Testing

Engine-backed (`probeEngine`, db-gated; a fake engine has no gates):

1. Two streams, two actors, one write per fixture tier: owned delivers only
   to the owner; clusterOwner only to the cluster owner; public and
   undeclared to both; composite to the owner and the cluster owner and not
   to a third user; granted arrives id-only with `payload_omitted`; a `user`
   row arrives narrowed.
2. An `update()` (which emits `created` and `updated`) is admitted or denied
   consistently on both events.
3. A reader subscribing `ALL` / `TELEMETRY` / `AI_STREAM` is refused; an admin
   subscribing `ALL` receives graph events only for rows they may read.
4. Composite tier: filter injection on a named query returns own rows for a
   user and all rows for a cluster owner; the write guard still refuses a
   cluster owner's update of another user's row.
5. Declarations: every `task`/`taskState` row written by each creation
   mutation carries its plan's `requestedBy`; `workersForUser` and dispatch
   still work for the owning user; a user reads their own audit rows and an
   admin reads all; a reader browsing `v1:identity:auditEvent` sees only
   their own.
6. Cluster e2e (`test/clustere2e/`): user B writes an owned row on node 1;
   user A, subscribed on node 2, receives nothing; B on node 2 receives it.
7. `TestSubscriptionFanOutAppliesTheRowGate` is added to
   `rowauthz_doc_gate_test.go`'s `docNamedTests`, so `per-row-authz-audit.md`
   must cite it and enumerate subscriptions as an egress.

---

## 7. Delivery

Two independent PRs:

| PR | Contains | Closes |
|---|---|---|
| 1 -- the seam | fan-out admission; `payload_omitted` on the wire; SDK + portal re-read; non-graph kind gate; docs | the three seam tasks + docs |
| 2 -- the tiers | the composite tier; the five declarations and the task owner stamping | the two tier tasks |

One `Closes #N` line per issue in each PR body.

---

## 8. Out of scope and follow-ups

- Declaring `utterance`, `participant`, `agent` -- Nexus (sub-project I).
- Declaring identity-internal concepts (`authSession`, `magicLinkRequest`,
  `identity`, ...) -- needs a characterisation of the actor the identity
  service's Store runs under; filed as a follow-up on the row-authz long tail.
- Server-side subscription predicates ("only this plan") -- a filtering
  feature, not an authz one; Nexus decides whether it needs it.
- Ordered or replayable delivery for subscriptions (`DeliverySubstrate`) --
  Nexus.
- Row authz on in-process bus subscribers (automations) -- they run as the
  engine, not as a user; unchanged.

---

## 9. References

- Code: `component/grpc/server.go` (`handleBusEvent`, `handleSubscribe`,
  `currentAccess`), `component/memql/rowauthz_enforce.go`,
  `rowauthz_write_guard.go`, `rowauthz_pii_unbound.go`,
  `rowauthz_undeclared_gate_test.go`, `rowauthz_doc_gate_test.go`,
  `component/events/proto.go`, `component/node/routing.go`,
  `sdk/ts/src/client/subscriptions.ts`,
  `clients/portal/src/cluster/useConceptRows.ts`,
  `clients/portal/src/concepts/liveBand.ts`, `dsl/planner/mutations.memql`.
- Docs: `docs/public/operate/auth/per-row-authz-audit.md`,
  `docs/internal/design/auth-threat-model.md`, `docs/public/concepts/events.md`.
- Related: memql#3172 (read-path enforcement), #3174 (write guard), #3982
  (top-level builtin egress), #2460 (structured graph subscriptions), #4274
  (dynamic views, live policy).
