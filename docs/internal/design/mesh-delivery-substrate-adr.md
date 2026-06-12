---
title: Mesh delivery substrate -- subscription mechanism + delivery contract
audience: internal
status: accepted
area: internal
sinceVersion: 0.9.36
owner: znas
---

# ADR: Mesh delivery substrate -- subscription mechanism + delivery contract

> SPIKE deliverable for memql#1262, Phase 1 of epic memql#1259 (Resilient
> multi-node mesh). This ADR is the contract the durable event delivery
> backbone (memql#1263) implements against. It does NOT build the backbone --
> it decides the DB-subscription mechanism and pins the delivery contract
> precisely enough to implement.

## 1. Context

The cluster mesh is a **star**, not a full graph. Each worker
(cognition / agent / planner / workbench / voice) holds a live NodeService
stream only to its **parent** BFF; it learns about the other BFF replicas
through gossip but those peer entries carry `Connection == nil`
(`PeerManager`, `component/node/peer.go`). Cross-node events fan out over
those streams via `EventBridge` (`component/node/eventbridge.go`).

The consequence: a worker **cannot reliably deliver a message to the specific
BFF replica that owns a given user's WebSocket**. On staging (2 replicas per
mesh node) a chat reply produced on a worker can be pushed to the wrong BFF
replica -- the one that does NOT hold the user's socket -- and is silently
dropped, or pushed to both and duplicated. The old double-dispatch masked
this; the memql#1217 gate removed that redundancy and exposed it. memql#1232
(per-peer outbox) and memql#1245 (dead-peer skip) patched the symptom, and
memql#1245 **regressed** it (it skips *live* non-parent replicas). The bug is
not reproducible on the single-replica local stack, so it failed in
production instead of CI -- the parity work in Phase 0 (memql#1260 /
memql#1261) closes that gap.

Root cause restated in one line: **delivery is addressed to a physical node
(`bff-pod-3`) instead of to the logical owner of a key (`whoever owns space
X`), and the producer has no reliable path to that physical node.**

The locked architecture (epic memql#1259, decisions 1-3) inverts this:

- The **durable graph (Postgres) is the delivery guarantee.** Producers write
  durable rows; consumers subscribe by **logical key** (space / session /
  user), not by node-id, and **pull** their stream -- so the producer never
  needs to reach a specific physical replica.
- The **mesh stays a best-effort low-latency fast-path**, deduped against the
  durable path by **event-id** so the two paths can never double-deliver.
- **No new broker** is introduced. The substrate must run identically on the
  local Docker Postgres+Timescale stack and on Tiger Cloud Postgres
  (staging / prod), per the env-agnostic-architecture rule.

This ADR decides *how* a consumer discovers and replays the durable rows
addressed to its key -- the DB-subscription mechanism -- and the contract the
backbone exposes.

## 2. Mechanisms evaluated

Our hard constraints, used as the evaluation axes:

- **C1 Replay / catch-up.** A consumer that (re)connects -- pod restart,
  rollout, network blip, brand-new WS owner taking over a key -- MUST receive
  everything addressed to its key since its last acknowledged position. This
  is the requirement that exposed the star-mesh bug; it is non-negotiable.
- **C2 Tiger Cloud Postgres capability.** Whatever we pick must work on
  managed Tiger Cloud (Timescale Cloud) Postgres, not just self-hosted, and
  identically on the local Docker Postgres.
- **C3 No new broker.** No Kafka/NATS/Redis-streams to run, secure, and
  reach parity on in two environments.
- **C4 Scale + operational cost.** Bounded connection/slot count, prunable
  storage, survives replica churn.
- **C5 Logical addressing + per-key ordering + idempotent dedup** (the
  contract in section 4) must be expressible on top of it.

### 2a. LISTEN / NOTIFY

Postgres `LISTEN`/`NOTIFY` (optionally fired by a trigger calling
`pg_notify`) pushes a payload to every currently-listening session.

- C1 Replay: **FAILS.** NOTIFY is fire-and-forget to *connected* listeners
  only. A consumer that is down, mid-reconnect, or simply slow MISSES the
  notification entirely -- there is no cursor, no backlog, no redelivery.
  This is exactly the failure mode (a BFF replica that wasn't reachable at
  send time never gets the message) we are trying to design out. Recovering
  it would require pairing NOTIFY with a durable table and a catch-up query
  anyway -- at which point NOTIFY is just an optional wake-up, not the
  substrate.
- C2 Tiger Cloud: supported, but the 8 KB payload limit forces "notify with
  an id, then SELECT the row," and NOTIFY does not cross Postgres physical
  replicas / read-replicas cleanly.
- C3 Broker: no new broker (plus).
- C4 Scale: every consumer holds a dedicated listening session; the single
  per-database NOTIFY queue is a serialization point under high write volume.
- C5: no native ordering or dedup; would be bolted on.

Verdict: **rejected as the substrate.** It structurally cannot do C1. It
survives only as an optional *wake-up nudge* layered on a durable table, and
in our design the mesh fast-path already fills that "low-latency nudge" role,
so NOTIFY adds a second fragile push path for no gain.

### 2b. Logical replication / CDC (replication slot, `pgoutput` / wal2json)

Consume the WAL via a logical replication slot and turn row changes into an
event stream.

- C1 Replay: **strong** in principle -- the slot tracks a confirmed LSN, so a
  reconnecting reader resumes from where it left off.
- C2 Tiger Cloud: **the blocker.** Logical replication slots on managed
  Tiger Cloud / Timescale Cloud are restricted -- creating slots, running a
  replication-protocol connection, and the WAL retention a stalled slot pins
  are not cluster-owner-grantable knobs we control the way a self-hosted box
  is. A slot that a dead consumer never advances pins WAL and can fill the
  managed volume -- an availability risk on a DB we do not operate. Timescale
  hypertables (which our graph + observability tables already are) add chunk
  churn that complicates a row-level CDC mapping.
- C3 Broker: no new broker, but it needs a **replication-consumer process**
  (a Debezium-class component or hand-rolled `pgoutput` reader) that is, in
  operational weight, broker-shaped -- a stateful long-lived consumer per
  cluster that must be deployed and monitored in both environments.
- C4 Scale: one slot per logical stream does not scale to per-key fan-out;
  realistically you get ONE WAL stream and you re-fan it in-process, which
  reintroduces a single owning node -- the very coupling we are removing.
- C5: ordering is global WAL order, not per-key; logical addressing and the
  idempotent-dedup keying must be reconstructed from raw row images.

Verdict: **rejected for OUR deployment.** CDC is the textbook
"replay over Postgres" answer and would be the lean if we self-hosted, but on
managed Tiger Cloud the slot/WAL-retention constraints (C2) plus the
broker-shaped consumer + global-order re-fan (C3/C4/C5) make it the heaviest,
least-portable option. The going-in lean explicitly allowed "outbox+cursor OR
CDC"; the Tiger Cloud constraint is what breaks the tie against CDC.

### 2c. Outbox + cursor (CHOSEN -- see section 3)

Producers append a durable row to an **outbox** table keyed by logical
routing key; consumers hold a per-(key, consumer) **cursor** and pull rows
strictly after their cursor, advancing it as they acknowledge. A wake-up
(mesh fast-path, or a cheap NOTIFY/poll) tells a consumer "there is new work,
pull now"; correctness never depends on the wake-up arriving.

- C1 Replay: **native.** Catch-up is `SELECT ... WHERE routing_key = $1 AND
  seq > $cursor ORDER BY seq`. A reconnecting or brand-new consumer just
  reads from its stored cursor (or from 0 / a watermark). This is the whole
  point.
- C2 Tiger Cloud: **plain tables + plain SQL.** Nothing managed-Postgres
  restricts. Runs byte-identical on local Docker Postgres. The outbox can be
  a normal table or a Timescale hypertable for time-based retention -- our
  choice, no slot, no replication protocol.
- C3 Broker: **none.** It is rows in the database we already run.
- C4 Scale: a monotonic per-key sequence + a covering index on
  `(routing_key, seq)`; retention by pruning acknowledged/aged rows (the
  `automation_execution_claims` table, memql#561, is the existing precedent
  for a small prunable coordination table). Consumers are stateless beyond
  their cursor row.
- C5: logical addressing is the `routing_key` column; per-key ordering is the
  per-key `seq`; idempotent dedup reuses the existing **event-id** dedup
  (`component/node/dedup.go`). All three fall out of the schema directly.

Verdict: **chosen.** It is the only option that satisfies C1 + C2 + C3
simultaneously, and it expresses the full contract (C5) natively.

### 2d. Summary

| Axis | LISTEN/NOTIFY | CDC / logical-repl | Outbox + cursor |
|---|---|---|---|
| C1 Replay / catch-up | No (fire-and-forget) | Yes (LSN) | Yes (cursor) |
| C2 Tiger Cloud managed | Partial | Restricted (slot/WAL) | Yes (plain SQL) |
| C3 No new broker | Yes | Broker-shaped consumer | Yes |
| C4 Scale / ops cost | NOTIFY queue + session/consumer | One WAL stream, re-fan | Indexed per-key seq, prunable |
| C5 Addr + order + dedup | Bolt-on | Reconstruct from WAL | Native columns |

## 3. Decision

**Adopt the outbox + cursor mechanism as the durable delivery substrate.**

- Producers write a durable outbox row per deliverable event, stamped with a
  **logical routing key** and a **per-key monotonic sequence**.
- Consumers subscribe by logical key, replay from a persisted **cursor** on
  every (re)connect, and advance the cursor as they acknowledge.
- The existing **mesh push (`EventBridge`) becomes a best-effort fast-path**:
  it carries the same event with the same **event-id**; a consumer that
  already saw that event-id via the durable pull (or vice-versa) drops the
  duplicate using the existing **`eventDedup`** window
  (`component/node/dedup.go`). The mesh is a latency optimization; the outbox
  is the guarantee. Mesh-down still delivers, just slower.

Rationale: it is the only candidate that clears all three of replay (C1),
Tiger-Cloud-managed-Postgres (C2), and no-new-broker (C3) at once, and it
expresses logical addressing, per-key ordering, and idempotent dedup as
first-class schema rather than bolt-ons. CDC was the alternate lean and is
rejected specifically because Tiger Cloud's managed replication-slot / WAL
constraints make it the least portable and most operationally heavy option
for us; NOTIFY is rejected because it cannot replay at all.

## 4. Delivery contract (what memql#1263 must satisfy)

The substrate MUST guarantee the following. These are the acceptance criteria
for the backbone; the same four guarantees are reused by Phase 2's RPC and
streaming patterns (epic decision 3).

### 4.1 Logical addressing

Every deliverable is addressed by a **logical routing key**, never a physical
node-id. A routing key is a typed string: `<kind>:<id>`, e.g.
`space:<spaceId>`, `session:<sessionId>`, `user:<userId>`. "Deliver to whoever
owns space X" resolves to `space:X`; the substrate does not know or care which
BFF replica currently holds that key's consumer. A consumer subscribes to one
or more routing keys and is the logical owner of that key for as long as it
holds the subscription.

### 4.2 At-least-once delivery + idempotent dedup

Delivery is **at-least-once**: a deliverable written to the outbox is
delivered to every active subscriber of its key at least once, across the
durable pull and/or the mesh fast-path, surviving consumer restart and mesh
churn. Because the two paths can both deliver the same event, every
deliverable carries a stable **event-id**, and consumers **dedup by
event-id** before acting, reusing the existing time-windowed dedup
(`eventDedup` in `component/node/dedup.go`; `Check(eventId)` returns true on a
repeat within the TTL window). The backbone MUST stamp the event-id at produce
time and carry it byte-identical on both paths so the dedup window catches the
duplicate regardless of which path arrives first.

> Reuse, do not reinvent: the dedup primitive is `component/node/dedup.go`.
> The backbone extends its *scope* from "mesh re-circulation" to
> "mesh-vs-durable cross-path," but keeps the same event-id key and
> time-window semantics.

### 4.3 Per-key ordering

Deliverables for a **single routing key** are delivered in produce order.
This is provided by a per-key monotonic **sequence** (`seq`) on the outbox
row; consumers read strictly in ascending `seq` for a given key and advance
their cursor monotonically. No global ordering across keys is promised or
needed -- ordering is per-key only, which is what fan-out parallelism
requires.

### 4.4 Replay / catch-up on (re)connect (cursor semantics)

On every (re)connect a consumer resumes from its **cursor**: the highest
`seq` it has acknowledged for each subscribed key. Catch-up is "read all rows
for key K with `seq > cursor` in ascending order, deliver, then advance the
cursor."

A **brand-new consumer** for a key (no cursor row -- e.g. the per-pod
consumer a rollout mints) **starts at the key's current high watermark**
(amended by memql#1328): on first subscribe its cursor is atomically
initialized AND persisted at the key's highest committed `seq`, so it
receives only deliverables produced after it first subscribed. **Backlog
replay is opt-in per subscription** (`WithReplayBacklog`) for consumers that
genuinely need first-subscribe history -- the per-request stream and RPC
keys, where the "backlog" is the head of the very exchange being consumed;
the opt-in mode starts from seq 0 and replays the retained backlog.
**Retention bounds the replayable window** either way (the memql#1328 sweep,
PR #1348). Long-lived keys with per-pod consumer ids (the chat-reply space
subscriptions) use the high-watermark default: replaying a retention window
of history into every fresh pod was the failure mode the default removes.

The high-watermark init MUST NOT race a concurrent producer into losing
events: anything committed after the cursor is initialized has `seq` above
the watermark and is delivered; anything committed before it MAY be skipped
-- that is the point. The init therefore snapshots only committed
seq-allocator state (per-key appends commit in seq order under the allocator
row lock) and resolves synchronously inside Subscribe, so "produced after
Subscribe returns" implies "delivered". An EXISTING cursor row always wins
over both modes: the consumer resumes exactly -- never re-pinned forward,
never rewound.

The cursor MUST be durable enough to survive the consumer's restart --
either persisted per (key, consumerId) in the database, or reconstructed
from the consumer's own already-applied/deduped state on reconnect. The
choice between DB-persisted-cursor and reconstruct-from-applied-state is
left to memql#1263, but the *semantics* above are fixed.

### 4.5 Mesh fast-path coexistence

The mesh (`EventBridge.forwardToPeers` / `ForwardInboundToPeers`) MAY deliver
the same deliverable ahead of the durable pull for latency. Rules:

1. The mesh-pushed copy carries the **same event-id** as the durable row.
2. A consumer applies whichever copy arrives first and **dedups the other**
   by event-id (4.2).
3. The mesh is **best-effort**: it is never the source of truth and a consumer
   MUST make correctness-critical progress from the durable pull alone
   (mesh-down still delivers, per memql#1263 acceptance).
4. Receiving a mesh copy MAY advance the consumer's cursor only once the
   corresponding durable row is confirmed applied; the mesh copy alone does
   not let the cursor skip a `seq` (otherwise a dropped durable row between a
   delivered-via-mesh event and the cursor would be lost on replay). The
   simplest correct rule: the **cursor advances on the durable path only**;
   the mesh path only short-circuits latency and feeds the dedup window.

## 5. Concrete sketch (for memql#1263 to implement)

This is a sketch to pin the contract, not a finished schema. memql#1263 owns
final column types, indexing, and retention tuning.

### 5.1 Proposed table shape (outbox + cursor)

```sql
-- Durable delivery outbox: one row per deliverable, addressed by logical key.
-- A normal table to start; can be promoted to a Timescale hypertable on
-- created_at for time-based retention if volume warrants. No replication
-- slot, no NOTIFY dependency -- plain rows, plain SQL, Tiger-Cloud-safe.
CREATE TABLE IF NOT EXISTS mesh_outbox (
  routing_key  text        NOT NULL,          -- logical addr, e.g. 'space:<id>'
  seq          bigint      NOT NULL,          -- per-key monotonic sequence
  event_id     text        NOT NULL,          -- stable id; the dedup key (4.2)
  topic        text        NOT NULL,          -- e.g. graph.node.created....
  kind         int         NOT NULL,          -- events.Kind
  payload      jsonb       NOT NULL,
  origin_node  text        NOT NULL,          -- producer node id (audit only)
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (routing_key, seq)
);
-- Covering read path for catch-up: WHERE routing_key=$1 AND seq>$cursor.
CREATE INDEX IF NOT EXISTS idx_mesh_outbox_key_seq
  ON mesh_outbox (routing_key, seq);
-- Dedup / idempotent-produce guard: same event_id must not double-insert.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mesh_outbox_event_id
  ON mesh_outbox (event_id);

-- Per-(key, consumer) cursor. Optional if the consumer reconstructs its
-- position from applied/deduped state; required if it persists the cursor.
CREATE TABLE IF NOT EXISTS mesh_cursor (
  routing_key  text        NOT NULL,
  consumer_id  text        NOT NULL,          -- logical consumer, not node-id
  cursor_seq   bigint      NOT NULL DEFAULT 0,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (routing_key, consumer_id)
);
```

Per-key `seq` allocation: a monotonic counter per `routing_key` (a small
`mesh_key_seq(routing_key, next_seq)` upsert returning the next value inside
the producing transaction, or `max(seq)+1` under the per-key advisory lock).
The exact allocator is memql#1263's call; the contract only requires that
`seq` is gap-tolerant-readable and strictly increasing per key.

Retention mirrors the `automation_execution_claims` precedent (memql#561): an
outbox row is prunable once every active subscriber's cursor is past it (or
after a max-age watermark that bounds how far a brand-new consumer replays).

### 5.2 Consumer subscribe API surface (logical key -> stream)

A Go interface sketch (final shape owned by memql#1263). The point is: the
subscriber names a **logical key**, gets an ordered, replayed, deduped stream,
and never names a node:

```go
// DeliverySubstrate is the durable backbone. Producers Publish; consumers
// Subscribe by logical key. The mesh fast-path is layered on top and is not
// part of this surface.
type DeliverySubstrate interface {
    // Publish appends a deliverable to the outbox for its routing key and
    // returns the assigned per-key seq. eventID must be stable + unique;
    // a duplicate eventID is a no-op (idempotent produce, 4.2).
    Publish(ctx context.Context, d Deliverable) (seq int64, err error)

    // Subscribe returns an ordered, replayed, deduped stream for one logical
    // key. An existing consumer replays from its cursor (4.4) before tailing
    // live; a brand-new consumer starts at the key's high watermark unless
    // WithReplayBacklog opts it into replaying the retained backlog
    // (memql#1328). Cancel via ctx.
    Subscribe(ctx context.Context, key RoutingKey, consumerID string, opts ...SubscribeOption) (<-chan Deliverable, error)

    // Ack advances the cursor for (key, consumerID) to seq (4.4).
    Ack(ctx context.Context, key RoutingKey, consumerID string, seq int64) error
}

type RoutingKey struct {
    Kind string // "space" | "session" | "user"
    ID   string
}                                  // -> "space:<id>"  (4.1)

type Deliverable struct {
    EventID    string         // dedup key, carried on BOTH paths (4.2)
    Key        RoutingKey     // logical addressing (4.1)
    Seq        int64          // per-key ordering (4.3); set by Publish
    Topic      string
    Kind       events.Kind
    Payload    map[string]any
    OriginNode string         // audit only; NOT used for addressing
}
```

### 5.3 Dedup + ordering keys (explicit)

- **Dedup key:** `event_id`. Carried byte-identical on the durable row and the
  mesh `EventForward` (`component/node/gen` `EventForward.EventId`). Consumers
  call the existing `eventDedup.Check(eventID)` (`component/node/dedup.go`)
  before applying; first path wins, second is dropped.
- **Ordering key:** `(routing_key, seq)`. Per-key monotonic; the consumer
  reads ascending and the cursor advances monotonically. No cross-key order.
- **Cursor key:** `(routing_key, consumer_id)` -> `cursor_seq`. Survives
  restart; defines the replay start point on (re)connect.

### 5.4 How this removes the bug by construction

A worker producing a chat reply for space X calls
`Publish({Key: space:X, ...})` -- it never has to find "the BFF replica that
owns the socket." Whichever BFF replica currently subscribes `space:X` pulls
the row from its cursor and delivers it to the WS; if that replica restarts or
a different replica takes over the key, the new owner replays from the cursor
and the reply is still delivered. The mesh fast-path may deliver it sooner;
the dedup window prevents a double-reply. The star topology is now irrelevant
to delivery correctness.

## 6. Consequences

- memql#1263 implements the outbox + cursor backbone against this contract;
  memql#1264 migrates the chat-reply path onto it (turns the memql#1261
  parity test green).
- memql#1245 (dead-peer skip) and the memql#1232 outbox become **fast-path-only
  heuristics** -- they affect latency, not correctness -- and are revisited in
  Phase 4 (memql#1271 reverts memql#1245; memql#1267 folds out the superseded
  ad-hoc forwards) once the durable path is the guarantee.
- No new infrastructure: the substrate is tables in the Postgres+Timescale we
  already run, identical local and on Tiger Cloud (env-agnostic).
- Phase 2 (memql#1265 RPC, memql#1266 streaming) builds request/response and
  ordered streaming on these same four guarantees.

## 7. References

- Epic: memql#1259 (architecture decisions 1-6 are locked there).
- This spike: memql#1262. Backbone: memql#1263. Chat-reply migration: memql#1264.
- Symptom patches superseded by this design: memql#1232, memql#1245; gate
  re-enabled in memql#1272; revert in memql#1271.
- Existing primitives reused: `component/node/dedup.go` (event-id dedup),
  `component/node/eventbridge.go` (mesh fast-path), `component/node/peer_outbox.go`
  (per-peer buffer), `automation_execution_claims` migration (memql#561, the
  prunable-coordination-table precedent).
- Parity foundation: memql#1260 / memql#1261;
  `docs/public/operate/reproduce-staging-locally.md`.
</content>
</invoke>
