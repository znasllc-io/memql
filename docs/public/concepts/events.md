---
title: MemQL Events System
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# MemQL Events System

**Last Updated:** 2026-02-21

This document describes the event pub/sub system in MemQL, which enables real-time notifications for graph mutations, queries, AI completions, and session lifecycle events.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    EVENT BUS (Pure Go)                          │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │  • sync.RWMutex + map for subscriber registry             │ │
│  │  • Go channels for async event delivery                   │ │
│  │  • Goroutine per subscriber for non-blocking fan-out      │ │
│  │  • Topic-based routing with glob patterns                 │ │
│  └────────────────────────────────────────────────────────────┘ │
│                                                                 │
│  Per-node in-memory bus (no Redis/NATS needed)                 │
└───────────┬─────────────────────────────────────────────────────┘
            │ Publish()                            
            ▼                                      
┌───────────────────────────────────────────────────────────────┐
│  Event Emitters                                               │
│  • MemQL Engine (node created/deleted/updated)                │
│  • Query executor (query executed)                            │
│  • AI runtime (completion started/finished/error)             │
│  • System (session opened/closed, subscription changes)       │
└───────────────────────────────────────────────────────────────┘
```

## Event Topics

Events are organized into hierarchical topics using dot notation. Subscribers can use glob patterns to match multiple topics.

### Graph Node Events

| Topic | Kind | Description |
|-------|------|-------------|
| `graph.node.created.{concept}` | `NODE_CREATED` | Concept-specific creation (e.g., `graph.node.created.v1:cognition:participant`) |
| `graph.node.updated.{concept}` | `NODE_UPDATED` | Concept-specific update |
| `graph.node.deleted.{concept}` | `NODE_DELETED` | Concept-specific deletion |

Graph CDC topics are exactly four segments: `graph.node.{action}.{concept}`.
The concept id carries no dots (it is colon-delimited), so it occupies the
single trailing segment. (The old `{partition}` segment between the action
and the concept was retired in #56 -- topics are concept-keyed, not
partition-keyed.)

> **Composing these topics is the SERVER's job.** Clients do NOT write
> topic strings. A structured graph subscription carries a `concept` +
> a set of `actions`, and the engine composes the bus topic (memql#2460).
> See [Client Subscriptions](#client-subscriptions) below. The topic
> grammar in this table is documented for observability, not as a
> client-authored wire string.

**Payload for node events:** the node's own `concept` (type id) and the
`eventKind` (`node_created` / `node_updated` / `node_deleted`) are
first-class fields -- a client matches on those, never by parsing the
`topic` string.
```json
{
  "id": "abc123",
  "nodeId": "abc123",
  "concept": "v1:agents:agent",
  "eventKind": "node_created",
  "actor": "user@example.com",
  "nodeType": "object",
  "createdAt": "2026-03-24T10:30:00Z"
}
```
(Ids in the payload are **bare** on the client wire per the bare-ids
contract; `concept` / `topic` / `eventKind` are concept-carrier keys and
stay verbatim. See [Node Identifier Conventions](identifiers.md).)

### Query Events

| Topic | Kind | Description |
|-------|------|-------------|
| `query.executed` | `QUERY_EXECUTED` | Emitted after a query completes |

**Payload:**
```json
{
  "durationMs": 42,
  "resultCount": 15,
  "cached": false
}
```

### AI Completion Events

| Topic | Kind | Description |
|-------|------|-------------|
| `si.completion.started` | `SI_COMPLETION_STARTED` | Emitted when an AI request begins |
| `si.completion.finished` | `SI_COMPLETION_FINISHED` | Emitted when an AI request succeeds |
| `si.completion.error` | `SI_COMPLETION_ERROR` | Emitted when an AI request fails |

**Payload for started/finished:**
```json
{
  "templateId": "summarize",
  "provider": "openai",
  "durationMs": 1234,
  "cached": false
}
```

**Payload for error:**
```json
{
  "templateId": "summarize",
  "provider": "openai",
  "durationMs": 500,
  "error": "rate limit exceeded"
}
```

### Session Events

| Topic | Kind | Description |
|-------|------|-------------|
| `session.opened` | `SESSION_OPENED` | Emitted when a gRPC streaming session starts |
| `session.closed` | `SESSION_CLOSED` | Emitted when a gRPC streaming session ends |

**Payload:**
```json
{
  "subject": "user@example.com"
}
```

### Automation Events

| Topic | Kind | Description |
|-------|------|-------------|
| `automation.started` | `AUTOMATION_STARTED` | Emitted when an automation begins execution |
| `automation.completed` | `AUTOMATION_COMPLETED` | Emitted when an automation completes successfully |
| `automation.failed` | `AUTOMATION_FAILED` | Emitted when an automation fails |
| `automation.step.started` | `AUTOMATION_STEP_STARTED` | Emitted when an automation step begins |
| `automation.step.completed` | `AUTOMATION_STEP_COMPLETED` | Emitted when an automation step completes |
| `automation.step.failed` | `AUTOMATION_STEP_FAILED` | Emitted when an automation step fails |

> Note: `automation.#` events stay **node-local** — they are blocked from
> cross-node forwarding (see the mesh routing rules). A consumer that must
> see automation activity on a different replica needs a dedicated topic
> with its own forward rule. The self-healing precondition-miss signal below
> is exactly such a topic.

### Self-Healing Events

| Topic | Kind | Description |
|-------|------|-------------|
| `healing.precondition.missed` | `PRECONDITION_MISSED` | Emitted when a first-class automation precondition evaluates false at the start of a run. The clean self-healing repair trigger AND the cross-machine portability signal. Unlike `automation.#`, this topic **forwards across the mesh** (a `healing.#` broadcast routing rule) so a repair loop on any replica hears it. |

**Payload for a precondition miss:**
```json
{
  "automationName": "deployStaging",
  "automationOrigin": "unified:deploypack/automations.memql:deployStaging",
  "executionId": "exec-abc123",
  "preconditionId": "digestPinned",
  "check": "exists(args.imageDigest)",
  "literal": "imageDigest",
  "preconditionDescription": "the deploy needs a pinned image digest",
  "triggerTopic": "graph.node.updated.v1:cluster:deployment",
  "triggerPayload": { "imageDigest": "" }
}
```

The `literal` + `triggerPayload` together name the machine-specific value
that did not hold on this machine — the input the repair loop relativizes or
rebinds when it proposes a typed patch (Epic 4).

**Payload for automation started:**
```json
{
  "automationName": "leadClassification",
  "executionId": "exec-abc123",
  "triggeredBy": "cron"
}
```

**Payload for automation completed:**
```json
{
  "automationName": "leadClassification",
  "executionId": "exec-abc123",
  "duration": 1234,
  "stepCount": 5
}
```

**Payload for automation failed:**
```json
{
  "automationName": "leadClassification",
  "executionId": "exec-abc123",
  "error": "step 'classify' failed: timeout",
  "duration": 5000
}
```

**Payload for step events:**
```json
{
  "automationName": "leadClassification",
  "executionId": "exec-abc123",
  "stepId": "classify",
  "stepType": "function",
  "duration": 150
}
```

## Subscribing to Events

### Via gRPC Stream

Clients can subscribe to events by sending a `SubscribeMsg` over the bidirectional gRPC stream:

```protobuf
message SubscribeMsg {
  string subscription_id = 1;
  SubscriptionKind kind = 2;
  string filter = 3;                      // legacy free-text; NON-graph kinds only
  google.protobuf.Struct config = 4;
  string concept = 5;                     // structured graph subscribe (memql#2460)
  repeated GraphNodeAction actions = 6;   // structured graph subscribe
}

enum SubscriptionKind {
  SUBSCRIPTION_KIND_UNSPECIFIED = 0;
  SUBSCRIPTION_KIND_TELEMETRY = 100;
  SUBSCRIPTION_KIND_MESSAGE = 200;
  SUBSCRIPTION_KIND_QUERY_SPEC = 300;
  SUBSCRIPTION_KIND_AI_STREAM = 400;
  SUBSCRIPTION_KIND_GRAPH_EVENTS = 500;
  SUBSCRIPTION_KIND_DOMAIN_EVENTS = 550;
  SUBSCRIPTION_KIND_AUTOMATION_EVENTS = 600;
  SUBSCRIPTION_KIND_ALL = 700;
}

enum GraphNodeAction {
  GRAPH_NODE_ACTION_UNSPECIFIED = 0;
  GRAPH_NODE_ACTION_CREATED = 1;
  GRAPH_NODE_ACTION_UPDATED = 2;
  GRAPH_NODE_ACTION_DELETED = 3;
}
```

### Graph subscriptions are STRUCTURED (memql#2460)

A graph subscription (`SUBSCRIPTION_KIND_GRAPH_EVENTS`) carries a
`concept` + a set of `actions`, and the **server composes the bus
topic**. The client never writes a `graph.node.<action>.<concept>`
string, so the topic grammar is not part of the client wire contract --
a future grammar change is no longer a wire change.

- `concept` -- canonical concept TYPE id (e.g. `v1:cognition:utterance`).
  A concept type is legitimately client-visible; import it from the
  generated SDK (`Concepts.COGNITION_UTTERANCE`). **Empty = all concepts.**
- `actions` -- the CDC verbs to receive. **Empty = all actions.**

The server composes one bus pattern per action (`graph.node.<verb>.<concept>`),
using `#` for all-concepts and `*` for all-actions.

**The legacy free-text `filter` is REJECTED for graph subscriptions** --
sending it on a `GRAPH_EVENTS` subscribe returns a `subscription-error`.
`filter` survives only for the non-graph kinds below.

| Kind | Value | Subscribe surface | Default (empty) |
|------|-------|-------------------|-----------------|
| `SUBSCRIPTION_KIND_GRAPH_EVENTS` | 500 | **structured** (`concept` + `actions`) | all concepts, all actions |
| `SUBSCRIPTION_KIND_TELEMETRY` | 100 | free-text `filter` | `telemetry.#` |
| `SUBSCRIPTION_KIND_MESSAGE` | 200 | free-text `filter` | `message.#` |
| `SUBSCRIPTION_KIND_QUERY_SPEC` | 300 | free-text `filter` | `query.#` |
| `SUBSCRIPTION_KIND_AI_STREAM` | 400 | free-text `filter` | `ai.#` |
| `SUBSCRIPTION_KIND_DOMAIN_EVENTS` | 550 | free-text `filter` | `#` |
| `SUBSCRIPTION_KIND_AUTOMATION_EVENTS` | 600 | free-text `filter` | `automation.#` |
| `SUBSCRIPTION_KIND_ALL` | 700 | (none) | `#` (matches everything) |

Supplying the structured `concept`/`actions` on a non-graph kind is
also rejected.

**Free-text filter glob grammar** (non-graph kinds): `*` matches exactly
one segment, `#` matches zero or more segments.

### Example: Subscribe to All Graph Events

```javascript
// Via WebSocket -- empty concept + empty actions = every graph event.
ws.send(JSON.stringify({
  message_id: "sub-1",
  payload: {
    subscribe: {
      subscription_id: "my-graph-sub",
      kind: 500 // SUBSCRIPTION_KIND_GRAPH_EVENTS
    }
  }
}));
```

### Example: Subscribe to Specific Concept Events

```javascript
// Subscribe only to note creations. The SERVER composes the topic
// graph.node.created.v1:notes:note; the client sends no topic string.
ws.send(JSON.stringify({
  message_id: "sub-2",
  payload: {
    subscribe: {
      subscription_id: "note-events",
      kind: 500, // SUBSCRIPTION_KIND_GRAPH_EVENTS
      concept: "v1:notes:note",
      actions: ["GRAPH_NODE_ACTION_CREATED"]
    }
  }
}));
```

The SDKs wrap this: the TS core SDK exposes
`SubscriptionManager.subscribeGraph(handler, { concept, actions })` and
the Go SDK exposes `SubscriptionManager.SubscribeGraph(ctx, GraphSubscribeOptions{...})`.
The legacy free-text `subscribe(pattern)` / `Subscribe(kind, filter)`
paths remain for the non-graph kinds and reject `graph_events`.

### Example: Subscribe to Automation Events

```javascript
// Subscribe to all automation events
ws.send(JSON.stringify({
  message_id: "sub-3",
  payload: {
    subscribe: {
      subscription_id: "automation-events",
      kind: 600, // SUBSCRIPTION_KIND_AUTOMATION_EVENTS
      filter: ""  // Results in pattern: automation.#
    }
  }
}));

// Subscribe to only automation completions
ws.send(JSON.stringify({
  message_id: "sub-4",
  payload: {
    subscribe: {
      subscription_id: "automation-completions",
      kind: 600, // SUBSCRIPTION_KIND_AUTOMATION_EVENTS
      filter: "completed"  // Results in pattern: automation.completed
    }
  }
}));

// Subscribe to step-level events for a specific automation
ws.send(JSON.stringify({
  message_id: "sub-5",
  payload: {
    subscribe: {
      subscription_id: "step-events",
      kind: 600, // SUBSCRIPTION_KIND_AUTOMATION_EVENTS
      filter: "step.#"  // Results in pattern: automation.step.#
    }
  }
}));
```

## Receiving Events

Events are delivered as `EventNotification` messages:

```protobuf
message EventNotification {
  string subscription_id = 1;
  EventKind kind = 2;
  google.protobuf.Timestamp ts = 3;
  google.protobuf.Struct payload = 4;
}

enum EventKind {
  EVENT_KIND_UNSPECIFIED = 0;
  // Telemetry events (100s)
  EVENT_KIND_TELEMETRY = 100;
  // Message events (200s)
  EVENT_KIND_MESSAGE = 200;
  // Graph events (300s)
  EVENT_KIND_GRAPH_UPDATE = 300;
  EVENT_KIND_NODE_CREATED = 301;
  EVENT_KIND_NODE_DELETED = 302;
  EVENT_KIND_NODE_UPDATED = 303;
  // Query events (400s)
  EVENT_KIND_QUERY_EXECUTED = 400;
  // AI events (500s)
  EVENT_KIND_AI_EVENT = 500;
  EVENT_KIND_AI_COMPLETION_STARTED = 501;
  EVENT_KIND_AI_COMPLETION_FINISHED = 502;
  EVENT_KIND_AI_COMPLETION_ERROR = 503;
  // Session events (600s)
  EVENT_KIND_SESSION_OPENED = 600;
  EVENT_KIND_SESSION_CLOSED = 601;
  // Automation events (700s)
  EVENT_KIND_AUTOMATION_STARTED = 700;
  EVENT_KIND_AUTOMATION_COMPLETED = 701;
  EVENT_KIND_AUTOMATION_FAILED = 702;
  EVENT_KIND_AUTOMATION_STEP_STARTED = 703;
  EVENT_KIND_AUTOMATION_STEP_COMPLETED = 704;
  EVENT_KIND_AUTOMATION_STEP_FAILED = 705;
}
```

### Example Event Response

```json
{
  "message_id": "evt-abc123",
  "payload": {
    "event": {
      "subscription_id": "my-graph-sub",
      "kind": 301,
      "ts": "2025-12-02T10:30:00Z",
      "payload": {
        "topic": "graph.node.created.acme.v1:notes:note",
        "eventKind": "node_created",
        "nodeId": "v1:notes:note:9c2f64f1-...",
        "concept": "v1:notes:note",
        "actor": "user@example.com"
      }
    }
  }
}
```

## Unsubscribing

To stop receiving events for a subscription:

```javascript
ws.send(JSON.stringify({
  message_id: "unsub-1",
  payload: {
    unsubscribe: {
      subscription_id: "my-graph-sub"
    }
  }
}));
```

## Implementation Details

### Event Bus

The event bus is a pure Go in-memory pub/sub implementation:

- **Thread-safe**: Uses `sync.RWMutex` for subscriber registry
- **Non-blocking**: Events are delivered asynchronously via goroutines
- **Panic recovery**: Handler panics are caught and logged
- **Pattern matching**: Supports glob patterns with `*` and `#` wildcards

### No External Dependencies

The event system requires no external infrastructure (Redis, NATS, etc.). All event routing happens in-memory within each memQL node; in cluster mode the node-to-node `EventBridge` propagates events across the mesh (with dedup and TTL) over the same gRPC streams the nodes already share.

### Event Delivery

- Events are cloned before delivery to prevent mutation
- Each subscriber receives events in a separate goroutine
- If a subscriber's channel is full, events are dropped with a warning log

### Cleanup

- Subscriptions are automatically cleaned up when a session ends
- The event bus properly shuts down all subscriptions when the server stops

