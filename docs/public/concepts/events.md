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

This document describes the event pub/sub system in MemQL, which enables real-time notifications for graph mutations, queries, SI completions, and session lifecycle events.

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
│  Single-instance deployment (no Redis/NATS needed)             │
└───────────┬─────────────────────────────────────────────────────┘
            │ Publish()                            
            ▼                                      
┌───────────────────────────────────────────────────────────────┐
│  Event Emitters                                               │
│  • MemQL Engine (node created/deleted/updated)                │
│  • Query executor (query executed)                            │
│  • SI runtime (completion started/finished/error)             │
│  • System (session opened/closed, subscription changes)       │
└───────────────────────────────────────────────────────────────┘
```

## Event Topics

Events are organized into hierarchical topics using dot notation. Subscribers can use glob patterns to match multiple topics.

### Graph Node Events

| Topic | Kind | Description |
|-------|------|-------------|
| `graph.node.created` | `NODE_CREATED` | Base topic for node creation |
| `graph.node.created.{partition}.{concept}` | `NODE_CREATED` | Partition + concept-specific (e.g., `graph.node.created.acme.v1:cognition:participant`) |
| `graph.node.deleted` | `NODE_DELETED` | Base topic for node deletion |
| `graph.node.deleted.{partition}.{concept}` | `NODE_DELETED` | Partition + concept-specific deletion |
| `graph.node.updated` | `NODE_UPDATED` | Base topic for node updates |
| `graph.node.updated.{partition}.{concept}` | `NODE_UPDATED` | Partition + concept-specific updates |

Event topics include a partition segment between the base topic and the concept. The `*` wildcard matches any single partition in subscription patterns (e.g., `graph.node.created.*.v1:cognition:participant`).

**Global-scoped concepts and the `_system` partition.** Concepts that
carry `@scope("global")` in their `.memql` definition (cluster
topology, partition registry, and similar infrastructure metadata)
store rows in the reserved `_system` partition regardless of the
request envelope. Their events therefore fire under topics like
`graph.node.created._system.v1:cluster:node`. Subscribers that use a
wildcard on the partition segment (e.g. `node.*.*.v1:cluster:node`)
match these events without modification; subscribers that need to
target only global events can use `graph.node.created._system.#`. The
underscore prefix on `_system` is reserved and cannot be used as a
user-chosen partition name.

**Payload for node events:**
```json
{
  "partition": "acme",
  "nodeId": "acme:v1:common:agent:abc123",
  "concept": "v1:common:agent",
  "actor": "user@example.com",
  "nodeType": "object",
  "createdAt": "2026-03-24T10:30:00Z"
}
```

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

### SI Completion Events

| Topic | Kind | Description |
|-------|------|-------------|
| `si.completion.started` | `SI_COMPLETION_STARTED` | Emitted when an SI request begins |
| `si.completion.finished` | `SI_COMPLETION_FINISHED` | Emitted when an SI request succeeds |
| `si.completion.error` | `SI_COMPLETION_ERROR` | Emitted when an SI request fails |

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
  string filter = 3;
  google.protobuf.Struct config = 4;
}

enum SubscriptionKind {
  SUBSCRIPTION_KIND_UNSPECIFIED = 0;
  SUBSCRIPTION_KIND_TELEMETRY = 100;
  SUBSCRIPTION_KIND_MESSAGE = 200;
  SUBSCRIPTION_KIND_QUERY_SPEC = 300;
  SUBSCRIPTION_KIND_AI_STREAM = 400;
  SUBSCRIPTION_KIND_GRAPH_EVENTS = 500;
  SUBSCRIPTION_KIND_AUTOMATION_EVENTS = 600;
  SUBSCRIPTION_KIND_ALL = 700;
}
```

### Subscription Kinds

| Kind | Value | Default Pattern |
|------|-------|-----------------|
| `SUBSCRIPTION_KIND_TELEMETRY` | 100 | `telemetry.#` |
| `SUBSCRIPTION_KIND_MESSAGE` | 200 | `message.#` |
| `SUBSCRIPTION_KIND_QUERY_SPEC` | 300 | `query.#` |
| `SUBSCRIPTION_KIND_AI_STREAM` | 400 | `ai.#` |
| `SUBSCRIPTION_KIND_GRAPH_EVENTS` | 500 | `graph.#` |
| `SUBSCRIPTION_KIND_AUTOMATION_EVENTS` | 600 | `automation.#` |
| `SUBSCRIPTION_KIND_ALL` | 700 | `#` (matches everything) |

### Filter Patterns

The `filter` field allows further refinement using glob patterns:

- `*` - Matches exactly one segment
- `#` - Matches zero or more segments

**Examples:**

| Pattern | Matches | Doesn't Match |
|---------|---------|---------------|
| `graph.node.*` | `graph.node.created`, `graph.node.deleted` | `graph.node.created.Skills` |
| `graph.node.created.*` | `graph.node.created.Skills` | `graph.node.created` |
| `graph.#` | All graph events | `si.completion.started` |
| `*.*.created` | `graph.node.created` | `graph.node.created.Skills` |

### Example: Subscribe to All Graph Events

```javascript
// Via WebSocket
ws.send(JSON.stringify({
  message_id: "sub-1",
  payload: {
    subscribe: {
      subscription_id: "my-graph-sub",
      kind: 500, // SUBSCRIPTION_KIND_GRAPH_EVENTS
      filter: ""
    }
  }
}));
```

### Example: Subscribe to Specific Concept Events

```javascript
// Subscribe only to Skills concept events
ws.send(JSON.stringify({
  message_id: "sub-2",
  payload: {
    subscribe: {
      subscription_id: "skills-events",
      kind: 500, // SUBSCRIPTION_KIND_GRAPH_EVENTS
      filter: "node.created.Skills"  // Results in pattern: graph.node.created.Skills
    }
  }
}));
```

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
  // SI events (500s)
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
        "topic": "graph.node.created.Skills",
        "eventKind": "node_created",
        "nodeId": "skills:programming-go",
        "concept": "Skills",
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

The event system requires no external infrastructure (Redis, NATS, etc.). All event routing happens in-memory within the single MemQL instance.

### Event Delivery

- Events are cloned before delivery to prevent mutation
- Each subscriber receives events in a separate goroutine
- If a subscriber's channel is full, events are dropped with a warning log

### Cleanup

- Subscriptions are automatically cleaned up when a session ends
- The event bus properly shuts down all subscriptions when the server stops

