# Node - Distributed Node System

**Purpose:** Inter-node communication, peer discovery, and bootstrap strategy for memQL cluster mode
**Language:** Go
**Proto:** `node.proto` (NodeService gRPC bidirectional stream)

---

## Overview

memQL uses **Go build tags** to compile separate binaries for each node type.
Each binary includes only the code relevant to its purpose. The node type is
determined at compile time via `node.CompiledNodeType()`, with `MEMQL_NODE_TYPE`
env var as a runtime fallback for the default (BFF) build.

See [docs/public/build/build-tags.md](../../docs/public/build/build-tags.md) for full build tag documentation.

In the default build (no tags), BFF code is included.

---

## Node Types

| Type | Build Tag | Purpose | Components |
|------|-----------|---------|------------|
| **bff** | (none, default) | Backend for frontend | Engine + PeerManager + EventBridge + NodeServer + WorkerDialer + AiForwardRouter |
| **voice** | `voice` | Voice transport (audio WS, LiveKit) | Engine + PeerManager + EventBridge + Polyphon transport + NodeServer |
| **cognition** | `cognition` | Cognition pipeline | Engine + PeerManager + EventBridge + Polyphon + NodeServer |
| **agent** | `agent` | Task execution, SI | Engine + PeerManager + EventBridge + SI + Tools + NodeServer |
| **planner** | `planner` | Task planning | Engine + PeerManager + EventBridge + NodeServer |

---

## Directory Structure

```
component/node/
├── CLAUDE.md              # This file
├── identity.go            # NodeType enum, Identity struct, env var parsing
├── lifecycle.go           # NodeLifecycle state machine (Starting/Ready/Draining/Stopped, #1268)
├── node.proto             # NodeService proto (11 message types)
├── generate.go            # protoc go:generate directive
├── gen/                   # Generated gRPC code
│   ├── node.pb.go
│   └── node_grpc.pb.go
├── peer.go                # PeerManager -- in-memory peer table with liveness
├── connection.go          # peerConnection -- gRPC stream with reconnect backoff
├── server.go              # NodeServer -- gRPC server for NodeService
├── stream_handler.go      # nodeService -- stream message dispatch
├── eventbridge.go         # EventBridge -- bridges local events.Bus across nodes
├── routing.go             # Event routing rules (block/forward/broadcast)
├── dedup.go               # Ring buffer dedup for distributed events
├── bootstrap.go           # NodeBootstrap interface, BootstrapFor() factory, DiscoverPeerAddress()
├── bootstrap_bff.go
├── bootstrap_voice.go
├── bootstrap_cognition.go
├── bootstrap_agent.go
├── bootstrap_planner.go
├── capability_router.go   # Route capability lookups across the mesh
├── query_proxy.go         # Forward queries to concept-owning nodes
├── parent_connector.go    # ParentConnector -- child dials its MEMQL_PARENT_ADDRESS
└── worker_dialer.go       # WorkerDialer -- BFF opens outbound streams to workers
                            # (seeded by MEMQL_WORKER_PEERS, reconciled via v1:cluster:node
                            # events + 30s ticker)
```

---

## Key Components

### Identity (`identity.go`)
Reads `MEMQL_NODE_TYPE`, `MEMQL_NODE_ADDRESS`, `MEMQL_NODE_ID`
from environment. Defaults to BFF. `MEMQL_PARENT_ADDRESS` is an optional
fallback for peer discovery; the primary mechanism is DB-based discovery.

### PeerManager (`peer.go`)
In-memory peer table indexed by node ID and node type. Tracks liveness via heartbeat
timestamps. Removes stale peers after configurable timeout. `AttachConnection(nodeId,
conn)` / `DetachConnection(nodeId)` bind an outbound `*peerConnection` onto a
`PeerEntry` so callers (AiForwardRouter, EventBridge, CapabilityRouter) can
`Send` on it. Only peers this node is the client for have `Connection != nil`.
The PeerManager also owns THIS node's `NodeLifecycle` (`Lifecycle()`); the
heartbeat builders read `Lifecycle().Health()` so the advertised gossip health
tracks the node's own lifecycle state.

### NodeLifecycle (`lifecycle.go`, memql#1268)
The node's explicit, self-asserted operational state machine -- distinct from
`common.Lifecycle` (component start/stop machinery) and from a peer's observed
`NodeHealthStatus`. States and legal forward edges:

```
Starting -> Ready -> Draining -> Stopped
```

There is no backward edge (a node never un-drains; `Stopped` is terminal);
`Transition` guards illegal edges and returns an error, leaving the state
unchanged. Idempotent self-edges (`X -> X`) are no-ops. `NodeLifecycle` is
concurrency-safe (the node is multi-goroutine) and notifies an optional
observer on every actual change.

How it is wired (in `app/cluster.go` + `app/run.go`):

- **Boot:** the PeerManager constructs the lifecycle in `Starting`.
- **Ready:** `app.MarkNodeReady()` flips it to `Ready` once every dependency
  has started (`Run` -> after `start(deps...)`), i.e. the node can actually
  serve.
- **Draining:** `app.BeginNodeDrain()` flips it to `Draining` on the shutdown
  signal (the existing SIGTERM path). This is only the MECHANISM; the
  in-flight-finish / flush / ordered-rollout BEHAVIOUR is memql#1269, and the
  operator-triggered drain is memql#1270.
- **Gossip advertisement:** `LifecycleState.Health()` maps the state onto the
  existing `NodeHealthStatus` wire enum (`Starting`->CONNECTING,
  `Ready`->HEALTHY, `Draining`->DRAINING, `Stopped`->STOPPED). The outbound
  heartbeat (`peerConnection` healthFn, set by ParentConnector + WorkerDialer)
  and the server-side heartbeat (`buildServerHeartbeat`) stamp this, so peers
  learn the state via the unchanged gossip contract and route AROUND a
  Draining node at once instead of after a missed-heartbeat timeout. Backward-
  compatible: a connection with no lifecycle source still advertises HEALTHY.
- **Readiness != liveness:** an observer (wired in `app/cluster.go`) bridges
  `Draining`/`Stopped` to `component/server.SetDraining(true)`, so `/healthz`
  + `/readyz` (READINESS) report 503 while `/livez` (pure LIVENESS, #1117)
  stays 200. A draining node is de-routed but NOT liveness-killed.

### WorkerDialer (`worker_dialer.go`, BFF-only)
On BFF binaries, opens one outbound NodeService stream per worker-type peer
(voice, agent, cognition, planner). Targets are reconciled from two sources:

1. **Static seeds** in the `MEMQL_WORKER_PEERS` env var
   (format: `voice=voice:50059,agent=agent:50055,...`), used for
   deterministic first-boot before the DB has any worker rows.
2. **DB discovery** against `v1:cluster:node`, event-driven via
   subscriptions on `graph.node.created._system.v1:cluster:node` and
   `graph.node.updated._system.v1:cluster:node`, with a 30s ticker as
   fallback.

Each dial re-uses `peerConnection` (the same type ParentConnector uses). On
`NodeWelcome` the worker is registered as `Monitored` in `PeerManager` and its
connection is bound via `AttachConnection` so `AiForwardRouter.Forward` finds a
live `Send` handle. Inbound `AiForwardResponse` messages on any managed stream
are dispatched to the `AiForwardResponseSink` (set by `app/cluster.go` on BFF
binaries).

### ParentConnector (`parent_connector.go`)
Installed only when `MEMQL_PARENT_ADDRESS` is set. Dials the configured parent
peer and keeps a single outbound NodeService stream open. Complementary to
WorkerDialer: WorkerDialer runs on the BFF for outbound fan-out to workers,
ParentConnector runs on any node with a configured upstream.

### EventBridge (`eventbridge.go`, `eventbridge_bus.go`)
Subscribes to local `events.Bus` with `#` pattern. Forwards matching events to connected
peers based on routing rules. Inbound events are published locally after dedup and TTL checks.
When `SetWiring()` is configured, inbound peer events are published via `bus.EventPublishCh`
channel instead of calling `localBus.Publish()` directly, with fallback to direct publish
if the channel is full.

Routing rules use `*` to match any partition segment in event topics. For example,
`graph.node.created.*.v1:cluster:*` matches cluster node creation in any partition.

> The mesh push is becoming a best-effort **fast-path** over a durable
> outbox+cursor delivery substrate (the star-topology delivery bug, epic
> memql#1259). Decision + delivery contract:
> [docs/internal/design/mesh-delivery-substrate-adr.md](../../docs/internal/design/mesh-delivery-substrate-adr.md)
> (spike memql#1262; backbone memql#1263).

### NodeServer (`server.go`)
gRPC server implementing `NodeService.Stream`. Handles handshake (NodeHello/NodeWelcome),
heartbeats, peer introductions, spawn requests, event forwarding, capability queries.

### Connection lifecycle (CLI side)
The CLI's pool entry runs a per-connection heartbeat ticker plus a bounded
3-attempt dial cycle with linear backoff (15s → 30s → 45s). On stream loss the
entry transitions Connected → Backoff and re-enters the cycle automatically; the
user can short-circuit via Esc (cancel) or R (manual retry from stateFailed).
See `cli/pool.go` for the entryState machine.

### Bootstrap Strategy (`bootstrap.go`)
`BootstrapFor(nodeType)` returns a `NodeBootstrap` that creates the right dependencies
for each node type. Called from `app/cluster.go` during the cluster bootstrap phase.
`BootstrapContext` carries `Wiring *bus.Wiring` (optional) which is passed to EventBridge
via `SetWiring()` in all bootstraps.

---

## Topology

In a cluster the BFF acts as the **client** of each worker node for forwarding
purposes: `WorkerDialer` on the BFF opens one outbound stream per worker so that
`NodeClientMessage{AiForwardRequest}` can flow BFF->worker and the response
arrives as `NodeServerMessage{AiForwardResponse}` on the same stream. Workers
themselves are servers; they do not dial the BFF or each other by default.

```
              BFF (WorkerDialer)
               │
    ┌──────────┼──────────┬──────────┐
    ▼          ▼          ▼          ▼
  Voice     Agent    Cognition    Planner
  :50059    :50055    :50054      :50056
```

EventBridge runs on every node and rides the streams WorkerDialer (on BFF) and
the inbound handlers (on workers) maintain, so distributed events flow in both
directions once the mesh is up.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MEMQL_NODE_TYPE` | `bff` | Node type (bff, voice, cognition, agent, planner) |
| `MEMQL_NODE_ID` | Generated UUID | Unique node identifier |
| `MEMQL_NODE_ADDRESS` | — | Advertised NodeService gRPC address |
| `MEMQL_PARENT_ADDRESS` | — | Optional upstream address for ParentConnector (when a node wants a single outbound stream to its "parent") |
| `MEMQL_WORKER_PEERS` | — | BFF-only: comma-separated `type=address` list for first-boot dialing (e.g. `voice=voice:50059,agent=agent:50055`). After DB discovery populates `v1:cluster:node`, this becomes a redundant seed. |
| `MEMQL_NODE_SERVICE_ADDRESS` | `:50052` | NodeService listen address |
| `MEMQL_NODE_LABELS` | — | Comma-separated key=value metadata |

---

## NodeService Proto

Single bidirectional stream. `NodeClientMessage` envelopes flow client->server
(NodeHello, NodeHeartbeat, QueryForward, AiForwardRequest, AiForwardCancel,
etc.); `NodeServerMessage` envelopes flow server->client (NodeWelcome,
NodeHeartbeat, PeerIntroduction, QueryResponse, AiForwardResponse, etc.).

| Message | Direction | Purpose |
|---------|-----------|---------|
| NodeHello | Client -> Server | Handshake with identity |
| NodeWelcome | Server -> Client | Server's own node_id + peer table |
| NodeHeartbeat | Bidirectional | Liveness with health status (client + server-side tickers). `health` carries this node's advertised lifecycle state via `NodeLifecycle.Health()` (memql#1268). |
| PeerIntroduction | Bidirectional | Peer table updates |
| SpawnRequest/Result | Bidirectional | Node spawning |
| EventForward/Ack | Bidirectional | Distributed events |
| CapabilityQuery/Response | Bidirectional | Capability discovery |
| QueryForward / QueryResponse | C->S / S->C | Cross-node MemQL query routing |
| AiForwardRequest / AiForwardResponse / AiForwardCancel | C->S / S->C / C->S | BFF->worker AI/voice forwarding (see `component/grpc/ai_forward.go`) |
| NodeShutdown | Server -> Client | Graceful shutdown |
