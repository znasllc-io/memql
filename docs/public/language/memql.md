---
title: MemQL
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL

> **Last Updated:** March 24, 2026

MemQL is the query and mutation language that powers the memory engine. It provides a deterministic, append-only interface for reading and writing concept-backed data stored in TimescaleDB. This document is the canonical reference for MemQL behavior. **Whenever the query language changes or new capabilities ship, update this guide alongside the code change.**

## When to Use MemQL

- Retrieving concept instances (assistants, personas, conversations, etc.) with filterable JSON payloads.
- Traversing graph-like relationships (parent/child, contains, alias, owns, createdBy, interactsWith).
- Inserting new immutable records via `insert()` mutations.
MemQL.

## Quick Start

### Directory Structure

Concept schemas live under versioned directories. MemQL automatically discovers and loads all version directories (v1, v2, etc.):

```
concepts/
├── v1/
│   ├── myapp/
│   │   └── user/               → v1:myapp:user
│   └── examples/
│       └── game/
│           └── player/         → v1:examples:game:player
└── v2/
    └── myapp/
        └── profile/            → v2:myapp:profile
```

All concepts from all version directories are loaded and merged. The version is part of the concept name, so `v1:myapp:user` and `v2:myapp:user` are distinct concepts.

Specs are concept-agnostic and shared across all versions:

- `specs/v1/...` contains all spec files.
- Specs can be organized into subdirectories for convenience.

### Basic Query

```
concept==v1:examples:world;payload.status=="active"
```

This returns all active worlds. MemQL responses use **omission semantics**—fields are only present when they contain data (see [Response Envelope](#response-envelope)):

```
{
  "result": {
    "bundle": {
      "nodes": [
        {
          "id": "v1:examples:world:world-aurora",
          "concept": "v1:examples:world",
          "payload": {
            "title": "Aurora Grid",
            "status": "active"
          }
        }
      ],
      "edges": [
        {
          "type": "contains",
          "fromId": "v1:examples:world:world-aurora",
          "toId": "v1:examples:module:module-foundations",
          "depth": 1
        }
      ],
      "rootIds": ["v1:examples:world:world-aurora"]
    }
  }
}
```

- `result.bundle.nodes` is a flat slice of every memory node touched during evaluation (matching records + relationship expansions).
- `result.bundle.edges` describes the relationships that were traversed. Edge types include `child`, `contains`, `aliases`, `createdBy`, `interactions`, and `owns`. Omitted when no edges exist.
- `result.bundle.rootIds` captures the IDs that directly satisfied the query before relationship expansion.
- `result.data` mirrors the caller-provided `shape()` template (see below). Omitted when no shape directive is used; when shaped, contains one element per root.
- `errors` is omitted on success; on failure, contains an array of structured issues (`code`, `message`, optional `metadata`).

## Response Envelope

MemQL uses **omission semantics**—fields are only included when they contain data:
- Present fields with data = included in response
- Absent/empty/not-applicable fields = omitted entirely

```
// Regular query (no shape) - has bundle, no data
{
  "result": {
    "bundle": {
      "nodes": [...],
      "edges": [...],      // omitted if empty
      "rootIds": [...]
    }
  }
}

// shape() query - has data, no bundle
{
  "result": {
    "data": [...]
  }
}

// shapeWithBundle() query - has both
{
  "result": {
    "bundle": {...},
    "data": [...]
  }
}

// Error response
{
  "result": {...},         // may be partial
  "errors": [...]
}
```

**Field semantics:**

- `result.bundle` – Contains the graph structure (`nodes`, `edges`, `rootIds`). For regular queries, the bundle is present with matched nodes. When using `shape()`, the bundle is omitted—use `shapeWithBundle()` to include it.
- `result.bundle.nodes` – Array of matched memory nodes. Omitted when empty.
- `result.bundle.edges` – Array of relationship edges. Omitted when no edges exist.
- `result.bundle.rootIds` – Array of root node IDs. Omitted when empty.
- `result.data` – Array of shaped payloads from `shape()` or `shapeWithBundle()`. Omitted when no shape directive is used.
- `errors` – Array of error objects when failures occur. Omitted on success.

Consumers should check for the presence of `errors` before operating on the result. This keeps backend services, clients, and SI agents aligned on the same contract.

Concept names mirror the versioned directory path under `concepts/`: every segment becomes a colon-delimited token (for example `v1/assistant` → `v1:assistant`). Each segment must be a single lowercase alphanumeric word; invalid names cause the loader to reject the concept.

### Concept Metadata Overview

Each concept directory contains a `concept.json` metadata file that tells the memory engine how to treat the records it stores. Every field is consumed by MemQL (and now available through `concepts()`—see below):

| Property            | Description                                                                                                                                                                  |
|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `description`       | Human-readable summary of what the concept stores. Required so humans and SI systems can reason about the dataset.                                                           |
| `type`              | Node classification: `object`, `collection`, or `reference`. Determines default relationship expectations.                                                                   |
| `skipDeleted`      | When true, default queries omit deletion tombstones emitted by `delete.json`.                                                                                                |
| `defaultFilter`    | Optional MemQL filter expression automatically applied by higher-level services. Leave empty to return all records.                                                          |
| `cacheTTLSeconds` | Per-concept cache TTL (seconds). `<= 0` disables caching. The global cache honors the smallest TTL among all nodes in the result tree.                                       |
| `relationships`     | Array describing graph edges. Each entry includes `type` (e.g. `parent`, `contains`, `createdBy`), `field` (payload or metadata field used as the pointer), `targetConcept`, and `direction` (`outgoing`, `incoming`, or `bidirectional`). The engine now infers whether `field` points at payload JSON or node metadata—reserved columns (`id`, `createdBy`, `concept`, etc.) may only be referenced at the top level. |

### Relationship Types

The `type` field in a relationship definition determines how MemQL interprets the graph edge. Valid types:

| Type | Description | Use When |
|------|-------------|----------|
| `parent` | This node belongs to a parent node | The field stores a single ID pointing to the parent |
| `contains` | This node contains other nodes | The field stores an array of IDs of contained nodes |
| `owns` | This node owns other nodes | Similar to contains, but implies exclusive ownership |
| `alias` | This node is an alias for another | The field stores the ID of the aliased node |
| `createdBy` | This node was created by another | The field stores the creator's ID (can use metadata) |
| `interactsWith` | This node interacts with another | Generic association between nodes |

**Common Mistake: Confusing `parent` vs `child`**

When a concept has a field that points TO another concept (like `spaceId` pointing to a space), use `"type": "parent"`. The relationship type describes the direction from the current node's perspective.

**Correct - Session belongs to Space (parent relationship):**

```json
// session/concept.json
{
  "relationships": [
    {
      "type": "parent",
      "field": "spaceId",
      "targetConcept": "v1:cognition:space",
      "direction": "outgoing"
    }
  ]
}
```

**Incorrect - Using "child" when the field points outward:**

```json
// session/concept.json - WRONG
{
  "relationships": [
    {
      "type": "child",
      "field": "spaceId",
      "targetConcept": "v1:cognition:space",
      "direction": "outgoing"
    }
  ]
}
// Error: relationship type "child" is invalid
```

**Rule of thumb:**
- If concept A has a field storing concept B's ID → A uses `"type": "parent"` pointing to B
- If concept A has an array of concept B IDs → A uses `"type": "contains"` pointing to B
- The `"child"` type is not directly used; child relationships are inferred by querying `childOf()` which finds nodes that have a `parent` relationship to the target

## System Concepts

MemQL includes built-in system concepts that provide infrastructure for workflows, configuration, and automation.

### Variable Concept (`v1:variable`)

The variable concept stores configuration values that can be referenced at runtime by workflows and automations. Variables allow configuration to be stored in the database without changing workflow definitions or injecting process environment variables.

**Location:**
- `concepts/v1/memql/variable/`

**Schema:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Variable name (uppercase with underscores, e.g., `DISCORD_WEBHOOK_URL`) |
| `value` | string | Yes | The variable value (URL, API key, token, etc.) |
| `description` | string | No | Human-readable description |
| `category` | enum | No | One of: `webhook`, `api`, `credential`, `config`, `other` |
| `sensitive` | boolean | No | Whether this contains sensitive data (default: `true`) |
| `active` | boolean | No | Whether the variable is active (default: `true`) |

**Querying Variables:**

```memql
-- Get a specific variable by name
concept==v1:variable;payload.name=="DISCORD_WEBHOOK_URL"

-- Get all webhook variables
concept==v1:variable;payload.category=="webhook"

-- Get all active variables
concept==v1:variable;payload.active==true
```

**Creating/Updating Variables:**

Variables follow the immutable data model—to "update" a variable, insert a new version:

```memql
insert("v1:variable", id="discord-webhook", payload={
  "name": "DISCORD_WEBHOOK_URL",
  "value": "https://discord.com/api/webhooks/ID/TOKEN",
  "description": "Discord webhook for workflow notifications",
  "category": "webhook",
  "sensitive": false,
  "active": true
})
```

**Using Variables in Workflows:**

Workflows reference variables using the `$var.NAME` syntax. The workflow engine resolves these at runtime by querying the variable concept:

```json
{
  "type": "webhook",
  "webhook": {
    "url": "$var.DISCORD_WEBHOOK_URL",
    "headers": {
      "Authorization": "Bearer $var.API_TOKEN"
    }
  }
}
```

**Benefits:**

- **No environment variables needed** – Configuration is stored in the database, not the process environment
- **Runtime updates** – Change variable values without restarting the application
- **Audit trail** – Full history of all variable changes (who, when, what)
- **Queryable** – Variables can be managed via standard MemQL queries

### Executing from Go

```go
tree, err := memEngine.Execute(ctx, `
	sort(
		paginate(
			concept==v1:examples:world;payload.status=="active",
			50
		),
		"createdAt","desc"
	)
`)
```

### Executing via MCP `memql`

```json
{
  "query": "sort(paginate(contains(id==\"project-abc\");payload.status==\"open\",25),\"payload.due_date\",\"asc\",\"createdAt\",\"desc\")"
}
```

### WebSocket Stream

Browser clients connect to `/memql/ws`, which upgrades to a long-lived WebSocket and forwards frames to the `MemqlService.Stream` gRPC method. Auth cookies/JWTs are reused automatically because the upgrade happens after the standard middleware stack.

Frames are JSON encodings of the existing protobuf envelopes. A typical request/response pair looks like:

```json
{
  "messageId": "req-123",
  "executeQuery": {
    "requestId": "req-123",
    "query": "concept==v1:examples:world;payload.status==\"active\""
  }
}
```

```json
{
  "messageId": "resp-123",
  "queryResult": {
    "requestId": "req-123",
    "result": {
      "bundle": {
        "nodes": [
          { "id": "v1:examples:world:world-aurora", "concept": "v1:examples:world" }
        ],
        "rootIds": ["v1:examples:world:world-aurora"]
      }
    },
    "done": true
  }
}
```

The bridge enforces a small per-connection window (four concurrent queries and a 5 MiB frame limit). Clients should reuse a single socket and listen for `queryResult.done` or `queryError` payloads.

Configuration variables (prefixed with `MEMQL_WS_`) let you tune the gateway:

| Variable | Description | Default |
|----------|-------------|---------|
| `MEMQL_WS_DIAL_TIMEOUT_MS` | How long to wait when dialing the internal gRPC server. | `5000` |
| `MEMQL_WS_WRITE_TIMEOUT_MS` | Per-message write deadline applied to the WebSocket. | `10000` |
| `MEMQL_WS_MAX_CONCURRENT_REQUESTS` | Maximum in-flight `executeQuery` messages per WebSocket. | `4` |
| `MEMQL_WS_MAX_MESSAGE_BYTES` | Maximum accepted frame size from the browser. | `5242880` (5 MiB) |
| `MEMQL_WS_PING_INTERVAL_MS` | Interval for server-side WebSocket keepalive pings. Prevents idle connection timeouts on edge/proxy infrastructure. Set to `0` to disable. | `30000` (30s) |

## Query Structure

| Component            | Description                                                                                                     |
|----------------------|-----------------------------------------------------------------------------------------------------------------|
| Filters              | Comparison expressions joined by `;` or `&&` (AND) or `,` (OR).                                                 |
| Fields               | `concept`, `id`, `type`, `createdAt`, `createdBy`, or `payload.<path>`.                                       |
| Operators            | `==`, `!=`, `>`, `>=`, `<`, `<=`, `in`, `not in`, `has`, `==nil`, `!=nil`.                                      |
| Parentheses          | Group complex logic: `(concept==v1:assistant,concept==v1:examples:persona);payload.active==true`.               |
| Timestamp suffix     | Append `@ "<RFC3339>"` to pin reads to a historical snapshot.                                                   |
| Limit & offset       | Use `paginate(<expr>, limit, offset?)` to request explicit windows; omitting it applies the engine defaults (limit=`MEMORY_ENGINE_MAX_RESULTS`, offset=`0`). |

IDs are persisted as `<concept>:<raw-id>`. MemQL supports both full IDs and short IDs (when concept context is provided):

**Full ID (always works):**
- `id=="v1:examples:world:world-aurora"` – exact match on full storage ID

**Short ID (requires concept context):**
- `concept==v1:examples:world;id=="world-aurora"` – short ID resolved using concept from query
- `id in ("world-aurora","world-nebula")` – multiple short IDs (requires concept in query)

**Important:** Short IDs without concept context will return an error:
```
// This will ERROR - no concept context to resolve short ID
id=="world-aurora"

// This works - concept provides context for ID resolution
concept==v1:examples:world;id=="world-aurora"

// This also works - full ID doesn't need context
id=="v1:examples:world:world-aurora"
```

This design ensures predictable, exact-match behavior and avoids ambiguous results.

### Filters

Filters are core comparison expressions that narrow the set of returned nodes:

```text
concept==v1:assistant;payload.active==true
```

- Use `;` for AND logic
- Use `,` for OR logic
- Group with parentheses: `(concept==A,concept==B);payload.active==true`
- Field paths support dot notation for nested JSON: `payload.profile.name`

### Directives

Directives wrap filters and apply transformations or constraints. They must enclose the entire filter expression:

| Directive | Description | Example |
|-----------|-------------|---------|
| `asOf()` | Evaluate the expression at a specific timestamp | `asOf(concept==v1:assistant, "2025-01-01T00:00:00Z")` |
| `shape()` | Apply a result-shaping template | `shape(concept==v1:assistant, {...})` |
| `select()` | Project specific fields | `select(concept==v1:assistant, "id", "payload.name")` |
| `sort()` | Order results by field(s) | `sort(concept==v1:assistant, "createdAt", "desc")` |
| `paginate()` | Limit and offset results | `paginate(concept==v1:assistant, 10, 0)` |
| `parentOf()` | Traverse to parent nodes | `parentOf(concept==v1:module;id=="mod-1")` |
| `childOf()` | Traverse to child nodes | `childOf(concept==v1:world;id=="world-1")` |
| `withDepth()` | Limit traversal depth for relationships | `withDepth(parentOf(...), 2)` |

Directives can be nested: `sort(paginate(concept==v1:assistant, 10), "createdAt", "desc")` returns assistants sorted by creation date with pagination.

### Dot-Path Field Access

MemQL supports deep dot-notation for accessing nested JSON fields:

```text
payload.profile.settings.theme == "dark"
```

Path segments follow JSON keys, including arrays via numeric indices when needed.

## Result Shaping

MemQL supports `shape()` and `select()` for transforming query output from raw nodes into structured responses.

### shape(expr, template | "shapeName")

Applies a projection template to each matched node. The template can be either an inline object or a named shape reference from `shapes/v1/`.

**Named shape reference (preferred):**
```memql
shape(concept==v1:cognition:participant;payload.spaceId=="space-123", "participantFull")
```

Named shapes are defined in `shapes/v1/<domain>/<shapeName>.memql` and registered with the engine at startup. Each concept has one comprehensive shape (e.g., `participantFull`, `agentFull`, `spaceFull`).

**Inline template (for one-off projections):**
```memql
shape(
  concept==v1:examples:challenge:attempt,
  {
    "id": node("id"),
    "title": node("payload.title"),
    "status": node("payload.status"),
    "metadata": {
      "created": node("createdAt"),
      "author": node("createdBy")
    }
  }
)
```

**Template Functions:**

| Function | Description |
|----------|-------------|
| `node("path")` | Extract a field from the current node |
| `literal(value)` | Insert a constant value |
| `children(template)` | Recursively render child nodes |
| `contains(template)` | Render contained nodes |
| `owns(template)` | Render ownership relationships |
| `aliases(template)` | Render alias relationships |
| `createdBy(template)` | Render creator relationships |
| `interactions(template)` | Render interaction relationships |
| `si(template, vars, provider?, ttl?)` | Generate SI content (projection only) |

### select(expr, ...fields)

Projects specific fields without a full template:

```memql
select(concept==v1:assistant, "id", "payload.name", "payload.status")
```

Returns objects with only the requested fields. Useful for lightweight queries.

## SI Integration

MemQL's SI integration is intentionally scoped so that language models can only influence projected output—filters, joins, sorts, and grouping remain deterministic.

### `si()` Function

The `si()` function invokes prompt templates defined in `prompts/v1/**/*.memql`:

```memql
shape(
  concept==v1:examples:module,
  {
    "id": node("id"),
    "title": node("payload.title"),
    "summary": si("docSummary.v1", { "content": node("payload.description") })
  }
)
```

**Signature:** `si(templateId, variables, provider?, cacheTTL?)`

| Parameter | Description |
|-----------|-------------|
| `templateId` | ID of the prompt template (string literal) |
| `variables` | Key-value map passed to the template |
| `provider` | Optional provider override (string literal) |
| `cacheTTL` | Optional cache TTL in seconds (0-300) |

**Example with all parameters:**

```memql
si("summarize.v1", { "text": node("payload.body") }, "openai", 60)
```

### `json()` Function

Serializes values to JSON strings for SI prompts:

```memql
si("analyze.v1", { "data": json(node("payload")) })
```

### `match()` / `case()` / `default()`

Conditional value selection for deterministic business logic inside `shape()`:

```memql
match(
  case(node("payload.score") > 90, "excellent"),
  case(node("payload.score") > 70, "good"),
  default("needs improvement")
)
```

### SI Constraints

- `si()` can only appear inside projections (`shape()`, `select()`, spec outputs)
- Using `si()` in filters, joins, sorts, or grouping raises an error
- Cache TTL is clamped to 300 seconds maximum
- Specs with `usesSI: true` cannot be used in filter expressions

### SI Configuration

- **SI cache env vars** control response reuse:  
  - `MEMQL_SI_CACHE_DEFAULT_ENABLED` (`true`/`false`) toggles whether `si()` calls cache results when no explicit TTL is provided.  
  - `MEMQL_SI_CACHE_MAX_SECONDS` caps any SI cache entry (and doubles as the default TTL when caching is enabled). The engine clamps this to **≤ 300 seconds (5 minutes)**.

### Calling `si()` inside `shape()`

Only string literals are allowed for the template and provider arguments. If the provider override is omitted, the engine uses the template's `defaultProvider`, then falls back to `MEMQL_DEFAULT_PROVIDER`. The engine enforces projection-only usage—if `si()` appears inside filters, joins, sorts, or grouping expressions the query fails with `si() cannot be used in filter, join, sort, or group expressions; use it only in projection.`

The shape template reference documents `si()` alongside other helpers (`node()`, `children()`, etc.). Because `si()` ultimately calls a language model, response latency and cost are higher than local projections. For time-critical use cases, set a short `cacheTTL` (e.g. 30 seconds) and lean on `MEMQL_SI_CACHE_DEFAULT_ENABLED=true` so repeated queries hit the cache.

## Smart Logic Engine patterns

Warning: Large language models are expensive and should be treated like accelerator cards in the execution plan. MemQL keeps SI usage safe by only allowing `si()` inside projections (`select`, `shape`, or spec outputs that are themselves projected). Filters, joins, sorts, pagination, and mutations remain 100 % deterministic. Before wiring any of the patterns below into prod, confirm that the surrounding specs already catch abuse cases and that operators understand where the SI spend occurs.

At a high level each pattern pairs:

- **Deterministic queries** (filters, specs, sorts) that gather context
- **An SI template** for optional narrative or classification output

The result is a "smart logic engine" that can reason over fresh time-series data while leaving the database, cache, and relationship traversals deterministic.

### Pattern catalog

#### 1. Summarize new entries since last check

Goal: Hand a time-bounded slice of data to a summarizer template.

```memql
shape(
  concept==v1:examples:event;createdAt>"2025-05-01T00:00:00Z",
  {
    "id": node("id"),
    "timestamp": node("createdAt"),
    "headline": si("eventHeadline.v1", { "event": json(node("payload")) }, "", 30)
  }
)
```

Why it helps: Time-bounded filters keep the result set small; `si()` caching means repeated queries hit the cache.

#### 2. Smart triage queue

Goal: Use a spec to label records as `needsReview`, then let `si()` suggest next actions.

```memql
shape(
  spec==needsReview,
  {
    "caseId": node("id"),
    "suggestedAction": si("triageAdvice.v1", { "payload": json(node("payload")) })
  }
)
```

Why it helps: Specs remain pure filters; the model only shapes output.

#### 3. Localized copy generation

Goal: Generate templated marketing copy in user locale.

```memql
shape(
  concept==v1:examples:campaign;id=="spring-sale",
  {
    "emailSubject": si("localizedSubject.v1", { "title": node("payload.title"), "locale": "es-MX" })
  }
)
```

Why it helps: Guarantees template-consistent copy in any supported locale while MemQL enforces schema validity for the inputs.

#### 4. "Why was this flagged?" helper

Goal: Pair deterministic specs (for example `needsReview` defined as `payload.amount>10000`) with an explainer template (`reviewReason.v1`).

```memql
shape(
  spec==needsReview,
  {
    "id": node("id"),
    "reason": si("reviewReason.v1", { "record": json(node("payload")) })
  }
)
```

Why it helps: Users get a human-readable rationale that the LLM generates from the exact payload that matched the spec.

#### 5. Bulk enrichment pipeline

Goal: Enrich records in batch while staying under rate limits and reusing cache.

```memql
shape(
  paginate(concept==v1:examples:contact;payload.enriched==nil, 50),
  {
    "id": node("id"),
    "enrichment": si("contactEnrich.v1", { "email": node("payload.email") }, "", 120)
  }
)
```

Why it helps: Paginate keeps each batch small; si() caching means subsequent runs for overlapping IDs hit the cache.

#### 6. Composite analyst report

Goal: Chain multiple specs and projections, ending with an SI narrative.

```memql
shape(
  concept==v1:examples:report;createdAt>"2025-04-01",
  {
    "reportId": node("id"),
    "executiveSummary": si("execSummary.v1", {
      "metrics": json(node("payload.metrics")),
      "period": "Q2 2025"
    })
  }
)
```

Why it helps: The template can reference runtime context (`period`) plus data from the node.

#### 7. Personalized onboarding checklist

Goal: Generate a tailored checklist for new users based on their profile.

```memql
shape(
  concept==v1:examples:learner:profile;payload.onboardingComplete==false,
  {
    "learnerId": node("id"),
    "checklist": si("onboardingChecklist.v1", { "profile": json(node("payload")) })
  }
)
```

Why it helps: Each learner sees unique guidance; the model only influences the projected checklist, not who qualifies.

#### 8. Smart notification copy

Goal: Produce push/SMS/email copy variants from a single canonical record.

```memql
shape(
  concept==v1:examples:notification;payload.sent==false,
  {
    "notificationId": node("id"),
    "pushCopy": si("pushCopywriter.v1", { "payload": json(node("payload")) }),
    "smsCopy": si("smsCopywriter.v1", { "payload": json(node("payload")) })
  }
)
```

Why it helps: Downstream workers can send the templated copy directly, keeping email/SMS pipelines deterministic.

#### 9. Contextual search snippets

Goal: Generate highlighted snippets for search results.

```memql
shape(
  concept==v1:examples:document;payload.body=contains="MemQL",
  {
    "docId": node("id"),
    "snippet": si("searchSnippet.v1", { "body": node("payload.body"), "query": "MemQL" })
  }
)
```

Why it helps: Lets the LLM craft readable snippets around the search term without altering the filter logic.

#### 10. Inline spec + si() combo

Goal: Declare a spec inline and project an SI-generated analysis block.

```memql
shape(
  concept==v1:examples:challenge:attempt
  with spec highFailure as payload.stats.failureRate > 0.5
  ;spec==highFailure,
  {
    "attemptId": node("id"),
    "analysis": si("failureAnalysis.v1", { "stats": json(node("payload.stats")) })
  }
)
```

Why it helps: Gives engineers a named analysis block they can project anywhere while the parser enforces "no filters" on `usesSI` specs.

#### 11. Release regression digest

Goal: Summarize failing tests for a release candidate.

```memql
shape(
  (concept==v1:release;payload.shippingState=="shipped"),
  (concept==v1:examples:test:run;payload.status=="failed"),
  {
    "releaseId": node("id"),
    "failureSummary": si("regressionDigest.v1", { "failures": json(node("payload.failures")) })
  }
)
```

Why it helps: QA leads get a one-paragraph summary of what's broken without manually parsing test logs.

## Cache Behavior

MemQL caches query results to improve performance for repeated queries. Understanding cache behavior helps optimize query patterns.

### Cache Keys

Cache keys are computed from:

1. The normalized query expression
2. The concept(s) involved
3. Any `@cache()` hints
4. Field projections (`@fields()` annotations)

### `@cache()` Hints

Override default cache behavior per-concept:

```memql
concept@cache(60)==v1:assistant
```

This caches results for 60 seconds regardless of the concept's default `cacheTTLSeconds`.

**Important behaviors:**

- The parser folds `@cache(<seconds>)` into the canonical `concept==` comparison (`concept@cache(30)==v1:assistant`), so different hint values generate different cache keys. Two queries that only differ by `@cache(30)` vs `@cache(0)` will never share or overwrite cache entries.
- Non-zero `@cache()` hints can re-enable caching for concepts whose `cacheTTLSeconds` is `0`. The hint value becomes the effective TTL (still clamped by `CACHE_MAX_TTL`) as long as it stays above zero.
- Even though `@cache(0)` produces a unique cache signature, it still prevents caching. `cacheTTLForTree()` clamps every concept's TTL (global ceiling → concept default → hint override) and skips writes entirely when the resolved TTL is `0`.
- Concept-scoped `@fields(...)` annotations also participate in the cache key through the projection signature. Adjusting those per-concept projections creates unique cache entries, ensuring callers never receive a broader or narrower payload than they requested.

### Cache Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `CACHE_MAX_TTL` | Maximum cache TTL in seconds | `300` |
| `MEMQL_SI_CACHE_DEFAULT_ENABLED` | Enable SI response caching | `false` |
| `MEMQL_SI_CACHE_MAX_SECONDS` | Maximum SI cache TTL | `300` |

## Relationships

MemQL models relationships between nodes as edges in a directed graph.

### Relationship Types

| Type | Description | Example |
|------|-------------|---------|
| `parent` | Hierarchical parent-child | World → Module → Quest |
| `child` | Inverse of parent | Quest → Module → World |
| `contains` | Collection membership | Collection contains items |
| `owns` | Ownership | User owns resources |
| `alias` | Identity aliasing | Multiple IDs for same entity |
| `createdBy` | Creator relationship | Resource → Creator |
| `interactsWith` | Interaction edges | User interacts with content |

### Traversal Functions

**In filters:**

```memql
parentOf(concept==v1:module;id=="module-1")
childOf(concept==v1:world;id=="world-1")
```

**In shape templates:**

```memql
shape(
  concept==v1:world,
  {
    "id": node("id"),
    "modules": children({ "id": node("id"), "title": node("payload.title") })
  }
)
```

### Depth Limiting

Use `withDepth()` to prevent runaway traversals:

```memql
withDepth(parentOf(concept==v1:challenge:attempt;id=="attempt-1"), 2)
```

This limits the traversal to 2 levels up the parent chain.

## Mutations

MemQL follows an **append-only, immutable data model**. Records are never updated in place; instead, new versions are inserted.

### insert()

Create or "update" a record:

```memql
insert("v1:lead", id="lead-123", payload={"name": "John", "status": "new"})
```

To "update" an existing record, insert a new version with the same ID:

```memql
insert("v1:lead", id="lead-123", payload={"name": "John", "status": "contacted"})
```

### Content-Addressed IDs

When no `id` is provided, MemQL generates a **deterministic content-addressed ID** derived from the concept name and payload using SHA256. This provides:

- **Idempotent inserts**: The same payload always produces the same ID, preventing accidental duplicates
- **Reproducibility**: Given a payload, you can predict or verify its ID
- **Natural deduplication**: Identical content maps to the same record

```memql
-- No id specified: ID is derived from concept + payload
insert("v1:lead", payload={"name": "John", "email": "john@example.com"})
-- Returns: v1:lead:a3f8b2c1d4e5f6... (64-char hex hash)

-- Running the same insert again produces the same ID
-- This creates a new version of the same record, not a duplicate
```

The generated ID is a 64-character hexadecimal SHA256 hash. An optional server-side salt (configured via `MEMORY_NODES_ZNASLLC_LAB_CONTENTID_SALT`) can be added for deployment isolation.

**Note:** Explicit `id` parameters always take precedence over content-addressed derivation.

#### Identical Payloads Create Versions, Not New Records

This is a critical design behavior that developers must understand:

When you insert the same payload without an explicit ID, you are **creating a new version of an existing record**, not a new independent record.

**Example scenario:**

```memql
-- First insert: Creates record with ID derived from payload
insert("v1:cognition:space", payload={"name": "New Space", "active": true})
-- Returns: v1:cognition:space:d681fc9d... (created at 12:18:30)

-- Second insert with SAME payload: Creates new VERSION of same record
insert("v1:cognition:space", payload={"name": "New Space", "active": true})
-- Returns: v1:cognition:space:d681fc9d... (created at 12:28:46)
-- Same ID, newer timestamp - this is a new VERSION, not a new record

-- Query returns only one result (the most recent version)
concept==v1:cognition:space;id=="d681fc9d..."
-- Returns: 1 record with createdAt: 12:28:46
```

**To create truly unique records without explicit IDs, the payload must differ:**

| Goal | Solution |
|------|----------|
| Create multiple independent records | Use unique values in the payload (different names, UUIDs, etc.) |
| Update an existing record | Insert with the same payload/ID (this is the intended pattern) |
| Ensure uniqueness | Pass an explicit `id` parameter |

**Creating unique records without explicit IDs:**

```memql
-- These create DIFFERENT records (different payloads = different IDs)
insert("v1:cognition:space", payload={"name": "Space Alpha", "active": true})
insert("v1:cognition:space", payload={"name": "Space Beta", "active": true})
insert("v1:cognition:space", payload={"name": "Space Gamma", "active": true})

-- Or include a unique identifier in the payload
insert("v1:cognition:space", payload={"name": "New Space", "active": true, "uuid": "abc-123"})
insert("v1:cognition:space", payload={"name": "New Space", "active": true, "uuid": "def-456"})
```

**Creating unique records with explicit IDs:**

```memql
-- These create DIFFERENT records (explicit IDs override content-addressing)
insert("v1:cognition:space", id="space-1", payload={"name": "New Space", "active": true})
insert("v1:cognition:space", id="space-2", payload={"name": "New Space", "active": true})
insert("v1:cognition:space", id="space-3", payload={"name": "New Space", "active": true})
```

This content-addressed behavior is intentional and provides powerful guarantees for idempotent operations, replay safety, and natural deduplication.

### Soft Deletes

To "delete" a record, insert a version with `active: false`:

```memql
insert("v1:lead", id="lead-123", payload={"active": false})
```

### Querying Current State

Queries always return the most recent version of each record:

```memql
concept==v1:lead;id=="lead-123"
```

### Historical Queries

Use `asOf()` to query data as it existed at a specific time:

```memql
asOf(concept==v1:lead;id=="lead-123", "2025-01-01T00:00:00Z")
```

## Common Patterns

### Finding Unprocessed Items

```memql
concept==v1:task;payload.processed==nil
```

### Filtering by Date Range

```memql
concept==v1:event;createdAt>"2025-01-01";createdAt<"2025-02-01"
```

### Combining Multiple Concepts

```memql
concept==v1:user,concept==v1:admin
```

### Nested Field Queries

```memql
concept==v1:profile;payload.settings.notifications.email==true
```

## Error Handling

### Structured Error Format

MemQL returns machine-actionable structured errors for SI agent consumption. Errors follow this JSON format:

```json
{
  "error": "ERROR_TYPE",
  "code": "SPECIFIC_CODE",
  "message": "Human-readable description",
  "details": {
    "concept": "v1:crm:lead",
    "field": "email"
  },
  "suggestion": {
    "description": "How to fix this error",
    "template": "concepts()"
  }
}
```

**Fields:**
- `error` – High-level error category (same as `code` for consistency)
- `code` – Specific error code from a fixed set (see below)
- `message` – Human-readable description
- `details` – Error-specific structured data (optional)
- `suggestion` – Recovery guidance with static template (optional)
- `position` – Character offset in query where error occurred (optional)
- `context` – Query fragment around error position (optional)

### Error Codes

| Code | Meaning | Common Cause |
|------|---------|--------------|
| `VALIDATION_FAILED` | Payload doesn't match schema | Schema validation error |
| `MISSING_REQUIRED_FIELDS` | Required fields absent | Missing fields in insert payload |
| `INVALID_FIELD_TYPE` | Field has wrong type | Type mismatch in payload |
| `UNKNOWN_CONCEPT` | Concept not registered | Typo in concept name or concept not loaded |
| `UNKNOWN_FUNCTION` | Function not found | Typo in function name |
| `SYNTAX_ERROR` | Query parse failure | Malformed MemQL expression |
| `INVALID_OPERATOR` | Unknown comparison operator | Unsupported operator for field type |
| `RELATIONSHIP_NOT_FOUND` | Relationship type not defined | Using relationship function on concept without that relationship |
| `INVALID_ARGUMENT` | Invalid argument provided | Wrong argument type or missing required argument |
| `NOT_FOUND` | Requested resource not found | ID doesn't exist |

This is a fixed, enumerated set. No dynamic error codes are generated.

### Suggestion Templates

Suggestions are static templates to help agents recover from errors. They never involve SI generation:

```
MISSING_REQUIRED_FIELDS → "Add the missing required fields: {fields}"
UNKNOWN_CONCEPT        → "Check available concepts with: concepts()"
UNKNOWN_FUNCTION       → "Check available functions with: functions()"
SYNTAX_ERROR           → "Check MemQL syntax with: memqlDocs()"
INVALID_OPERATOR       → "Supported operators: ==, !=, >, >=, <, <=, in, not in, has"
RELATIONSHIP_NOT_FOUND → "Check concept relationships with: help(\"conceptName\")"
```

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `unknown concept` | Concept not defined | Check concept path spelling |
| `invalid query syntax` | Malformed expression | Review operator/parentheses usage |
| `si() cannot be used in filter` | SI in non-projection context | Move `si()` inside `shape()` or `select()` |
| `spec not found` | Referenced spec doesn't exist | Define the spec or check spelling |

### Debugging Tips

1. Start with simple queries and add complexity
2. Use `select(..., "id")` to verify filter matches before full projection
3. Check concept definitions for required fields
4. Review spec definitions for filter compatibility
5. Use `validate(concept, payload)` to check payloads before insert
6. Use `functions()` to list available functions
7. Use `help(name)` to get detailed help on any function or tool

## Troubleshooting

### Shape Template Errors

**Problem:** `invalid query syntax: unknown shape function "interactsWith"`

The `interactsWith` relationship helper is used incorrectly. Check that:

1. You're using the correct helper for the relationship type
2. The relationship exists in your concept definition

**Fix:** Use the appropriate helper function:

```memql
shape(
  concept==v1:user,
  {
    "id": node("id"),
    "interactions": interactions({ "id": node("id") })
  }
)
```

Error: `invalid query syntax: unknown shape function "interactsWith"` – the template helper isn't recognized.

### Walking Parent Chains

To traverse parent relationships:

```memql
withDepth(
  parentOf(concept==v1:challenge:attempt;id=="attempt-1"),
  3
)
```

Use `parentOf()` in filters, then `withDepth()` to control traversal depth.

### Common Mistakes

**Wrong:** Using function calls as literals in inserts

```memql
insert("v1:entity", id=uuid(), payload={...})
```

Error: `invalid query syntax: expected '"' to start string literal ...` – `uuid()` isn't a valid literal.

**Use this instead**

Pre-generate IDs and pass them as strings:

```memql
insert("v1:entity", id="entity-abc123", payload={...})
```

## Query Execution

Queries can be executed via the gRPC `MemqlService.Stream` bidirectional RPC or through the WebSocket bridge at `/memql/ws`. Both paths share the same backend validation, so every expression that references `si()` or inline specs follows the same rules described in this guide.

## Subscriptions & Events

MemQL provides a real-time event system that delivers notifications for graph mutations, query execution, SI completions, and session lifecycle events. Clients subscribe over the existing bidirectional gRPC stream (or WebSocket bridge) and receive `EventNotification` messages as changes occur.

### Subscribing to Events

Send a `SubscribeMsg` over the stream to register for events:

```json
{
  "messageId": "sub-1",
  "subscribe": {
    "subscriptionId": "my-graph-events",
    "kind": 5,
    "filter": ""
  }
}
```

**Subscription Kinds:**

| Kind | Value | Default Pattern |
|------|-------|-----------------|
| `SUBSCRIPTION_KIND_TELEMETRY` | 1 | `telemetry.#` |
| `SUBSCRIPTION_KIND_MESSAGE` | 2 | `message.#` |
| `SUBSCRIPTION_KIND_QUERY_SPEC` | 3 | `query.#` |
| `SUBSCRIPTION_KIND_SI_STREAM` | 4 | `si.#` |
| `SUBSCRIPTION_KIND_GRAPH_EVENTS` | 5 | `graph.#` |
| `SUBSCRIPTION_KIND_ALL` | 6 | `#` (everything) |

The `filter` field accepts glob patterns for finer control:
- `*` matches exactly one segment (e.g., `graph.node.*` matches `graph.node.created`)
- `#` matches zero or more segments (e.g., `graph.#` matches all graph events)

### Available Event Topics

| Topic | Event Kind | Description |
|-------|------------|-------------|
| `graph.node.created.{concept}` | `NODE_CREATED` | Graph node inserted |
| `graph.node.deleted.{concept}` | `NODE_DELETED` | Graph node deleted |
| `graph.node.updated.{concept}` | `NODE_UPDATED` | Graph node updated |
| `query.executed` | `QUERY_EXECUTED` | Query completed |
| `si.completion.started` | `SI_COMPLETION_STARTED` | SI request began |
| `si.completion.finished` | `SI_COMPLETION_FINISHED` | SI request succeeded |
| `si.completion.error` | `SI_COMPLETION_ERROR` | SI request failed |
| `session.opened` | `SESSION_OPENED` | gRPC session started |
| `session.closed` | `SESSION_CLOSED` | gRPC session ended |

### Receiving Events

Events arrive as `EventNotification` payloads:

```json
{
  "messageId": "evt-abc123",
  "event": {
    "subscriptionId": "my-graph-events",
    "kind": 10,
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
```

### Unsubscribing

```json
{
  "messageId": "unsub-1",
  "unsubscribe": {
    "subscriptionId": "my-graph-events"
  }
}
```

Subscriptions are automatically cleaned up when the session closes.

> See [docs/events.md](../concepts/events.md) for the full architecture, payload schemas, and implementation details.

## Upcoming Features

These roadmap items are planned but not yet implemented. Update this section as features land or priorities change.

- **Streaming Responses** – add streaming execution so clients can start reading partial MemQL results before the query finishes (instead of waiting for a single HTTP response body).

## Keeping This Guide Up to Date

Any change to MemQL parsing, execution options, relationships, or mutations **must** be reflected here. Before merging query-language changes:

1. Update the relevant sections (syntax, operators, options, examples, roadmap).
2. Reference this document in pull requests so reviewers verify documentation parity.

## Automation Step Style

Automations support function-call step syntax for data operations:

```memql
checkExisting := query({ query: "concept==v1:cognition:session; payload.participantId==$event.payload.id" })

createSession := if checkExisting.metadata.itemCount == 0 {
  mutation({
    concept: "v1:cognition:session",
    id: concat("session-", event.payload.id),
    payload: { participantId: event.payload.id }
  })
}
```

Control-flow blocks (`for`, `if`, `switch`, `parallel`) remain unchanged.

For migration details, see `docs/guides/migrating-from-inline-blocks.md`.

## MemQL Language Reference for SI Agents

This is a condensed syntax specification designed to fit within limited context windows. Use this for quick reference; for detailed explanations and examples, see the sections above.

### Basic Filter Syntax

```text
concept==v1:namespace:name
id=="node-id"
payload.field==value
payload.nested.path==value
createdAt>"2025-01-01T00:00:00Z"
```

### Operators

| Operator | Example | Description |
|----------|---------|-------------|
| `==` | `payload.status=="active"` | Equality |
| `!=` | `payload.status!="left"` | Inequality |
| `>` | `payload.score>0.5` | Greater than |
| `>=` | `payload.count>=10` | Greater than or equal |
| `<` | `payload.age<30` | Less than |
| `<=` | `payload.priority<=5` | Less than or equal |
| `in` | `payload.name in ("a","b")` | Value in list |
| `not in` | `payload.role not in ("admin")` | Value not in list |
| `has` | `payload.tags has "urgent"` | Array contains value |
| `==nil` | `payload.field==nil` | Field is null/absent |
| `!=nil` | `payload.field!=nil` | Field exists |

### Logical Operators

- `;` – AND (also `&&`)
- `,` – OR
- `()` – Grouping
- `!` – Boolean negation (e.g., `!spec==mySpec`)

### Null Coalescing Operator

The `??` operator provides null coalescing, returning the first non-null operand:

```memql
node("payload.nickname") ?? node("payload.name") ?? "Unknown"
```

This is equivalent to `coalesce(node("payload.nickname"), node("payload.name"), "Unknown")`. Use `??` for inline fallback chains in shape templates.

### Result Shorthand Accessors

Query results support shorthand accessors for common operations:

| Accessor | Equivalent | Description |
|----------|------------|-------------|
| `.count` | `len(result.bundle.nodes)` | Number of matched nodes |
| `.nodes` | `result.bundle.nodes` | The matched node array |
| `.empty` | `len(result.bundle.nodes) == 0` | True when no nodes matched |
| `.first` | `result.bundle.nodes[0]` | First matched node (nil if empty) |

These are available on the result of any query expression used as a step in automations or assigned to a variable:

```memql
results := concept==v1:task;payload.status=="open"
notify("admin", format("There are %d open tasks", results.count))

latest := (concept==v1:event;sort("createdAt","desc");paginate(1)).first
```

### Core Directives

```text
asOf(expr, "timestamp")         # Historical query
sort(expr, "field", "desc")     # Sort by field descending
paginate(expr, limit, offset?)  # Pagination
```

### Relationship Traversal

```text
parentOf(expr)                  # Get parents
childOf(expr)                   # Get children
withDepth(traversal, n)         # Limit depth
```

### Projections

```text
select(expr, "field1", "field2")
shape(expr, { "key": node("path") })
```

### Shape Template Functions

```text
node("path")              # Get node field
literal(value)            # Constant value
children(template)        # Child nodes
contains(template)        # Contained nodes
owns(template)            # Owned nodes
si(templateId, vars)      # SI generation (projection only)
json(value)               # Serialize to JSON
match(case(...), ...)     # Conditional
```

### Mutations (Append-Only)

```text
insert("concept", id="id", payload={...})
```

### Specs

```text
spec==specName                           # Use named spec
with spec name as filter;spec==name      # Inline spec
```

### Example Patterns

```memql
# Get active users
concept==v1:user;payload.active==true

# Get users created this year
concept==v1:user;createdAt>"2025-01-01T00:00:00Z"

# Shape with children
shape(concept==v1:world, {
  "id": node("id"),
  "modules": children({ "id": node("id") })
})

# SI summary in projection
shape(concept==v1:document, {
  "id": node("id"),
  "summary": si("summarize.v1", { "text": node("payload.body") })
})
```

Use this reference when constructing MemQL queries. Always validate syntax and concept paths against the engine's response. MiB) |

## Query Structure

| Component            | Description                                                                                                     |
|----------------------|-----------------------------------------------------------------------------------------------------------------|
| Filters              | Comparison expressions joined by `;` or `&&` (AND) or `,` (OR).                                                 |
| Fields               | `concept`, `id`, `type`, `createdAt`, `createdBy`, or `payload.<path>`.                                       |
| Operators            | `==`, `!=`, `>`, `>=`, `<`, `<=`, `in`, `not in`, `has`, `==nil`, `!=nil`.                                      |
| Parentheses          | Group complex logic: `(concept==v1:assistant,concept==v1:examples:persona);payload.active==true`.               |
| Timestamp suffix     | Append `@ "<RFC3339>"` to pin reads to a historical snapshot.                                                   |
| Limit & offset       | Use `paginate(<expr>, limit, offset?)` to request explicit windows; omitting it applies the engine defaults (limit=`MEMORY_ENGINE_MAX_RESULTS`, offset=`0`). |

IDs are persisted as `<concept>:<raw-id>`. MemQL automatically prefixes bare identifiers, so all of the following are valid:

- `id=="v1:examples:world:world-aurora"` – fetch a specific record.
- `id=="v1:examples:world"` – page through every `v1:examples:world` record via prefix match.
- `id=="v1:examples:world:*"` – explicit wildcard syntax (equivalent to the previous example).

These semantics also apply inside `ids()` and other helpers that rely on ID filters.

### Directives vs Functions

MemQL distinguishes between **directive wrappers** and **expression functions**:

- **Directives** wrap an entire expression and peel off before evaluation. They include `sort()`, `paginate()`, `select()`, `asOf()`, and `withDepth()`. Only one instance of each directive can wrap a query, and directives must form the outermost stack (for example `sort(select(paginate(...)))`).
- **Functions** such as `parentOf()`, `contains()`, `ids()`, or `concepts()` participate directly in the expression tree and can appear anywhere a comparison can. They do not get peeled off by the parser.

Keep this separation in mind when composing helpers: wrap the final expression with directives in whatever order you need (usually `select()` closest to the expression, followed by `paginate()`/`sort()`), and place relationship functions inside that stack.

## Result Shaping

Graph bundles are easy to inspect programmatically, but sometimes you want the engine to return a custom JSON shape. The `shape(<expr>, <template>)` directive lets you describe the output structure in-line with the query.

- The template syntax looks like JSON and supports nested objects/arrays.
- Use helper functions inside the template to reference the current node or related nodes:
  - `node("field", "payload.path.*")` – pulls metadata/payload fields from the current node. With no arguments the entire node is returned.
  - `children(<template>)`, `aliases(<template>)`, `contains(<template>)`, `owns(<template>)`, `createdBy(<template>)`, `interactions(<template>)` – traverse edges of that type and apply the nested template to every match. **Note:** Relationship pointer fields are optional—if a node has a null or missing pointer field, it is silently skipped during traversal rather than causing an error.
  - `json(<template>)` – serializes the inner template result to a JSON string. Essential for embedding structured data in SI prompt templates. See [JSON Serialization with `json()`](#json-serialization-with-json) for details.
  - `match(case(...), default(...))` – conditional value selection based on specs or inline comparisons. See [Conditional Logic with `match()`](#conditional-logic-with-match) for details.
  - `si("templateId", {data}, "provider", ttl)` – invoke an SI template for dynamic content generation. See [SI providers, prompts, and `si()`](#si-providers-prompts-and-si) for details.
- **Single-field projection optimization:** When `node()` extracts exactly one field, the value is returned directly rather than wrapped in a map. This produces cleaner JSON output:
  ```
  // Single field - returns value directly
  node("payload.email")  // Returns: "user@example.com"

  // Multiple fields - returns map with field keys
  node("payload.email", "payload.name")  // Returns: {"email": "user@example.com", "name": "John"}
  ```
  This optimization applies to all contexts where `node()` is used, including inside relationship helpers like `children()` and `createdBy()`.
- **Functions NOT allowed in shape templates:** `select()`, `paginate()`, `sort()`, `asOf()`, `withDepth()`, `parentOf()`, `childOf()`, `ids()`, and `concepts()` are **directive or expression functions** that operate at the query level, not inside templates. To include related data in a shape template, use the relationship helpers listed above (`contains()`, `children()`, etc.) instead of trying to run sub-queries.
- When multiple root nodes match the query, `shape()` populates `result.data` with an array of template results. With a single root, the template result is still wrapped in a one-element array so consumers never need to branch on type.
- **`shape()` omits the bundle by default.** The response contains only `result.data`—no `result.bundle`. This reduces payload size when you only need the transformed data.
- Both `shape()` and `shapeWithBundle()` **require a template argument**. Calling either without a template results in an error.

### `shape()` vs `shapeWithBundle()`

| Function | Bundle Included | Use Case |
|----------|-----------------|----------|
| `shape(<expr>, <template>)` | No | Most common—return only the shaped data |
| `shapeWithBundle(<expr>, <template>)` | Yes | When you need both the shaped data and the underlying graph |

Use `shapeWithBundle()` when you need access to the raw nodes, edges, and relationships alongside the transformed output—for example, when building UIs that display both a summary view and a detailed graph visualization.

Example:

```
shape(
  withDepth(parentOf(concept==v1:examples:quest;id=="v1:examples:quest:quest-nodes"), 2),
  {
    "world": node("payload.title"),
    "modules": children({
      "id": node("id"),
      "name": node("payload.name"),
      "quests": children({
        "id": node("id"),
        "title": node("payload.title")
      })
    })
  }
)
```

The result collapses into a single JSON object containing the parent world, its modules, and the quests within each module—no client-side traversal required.

With `shape()`, the response contains only `result.data`—the bundle is omitted. If you need both the shaped data and the underlying graph, use `shapeWithBundle()` instead, which includes `result.bundle` alongside `result.data`.

## SI providers, prompts, and `si()`

MemQL's SI integration is intentionally scoped so that language models can only influence projected output—filters, joins, sorts, and grouping remain deterministic.

- **Providers** live in `dsl/providers/providers.memql`. Each is declared in **struct form** (`provider NAME { ... }`); the legacy `func (Provider)` receiver form is retired and rejected at parse time. Every provider specifies a name, `@type` attribute, `@model` attribute, `auth` block, and optional `params` block. The first provider with `@default` becomes the fallback (unless `MEMQL_DEFAULT_PROVIDER` is set). Example:

  ```memql
  @extends("openai")
  @model("gpt-5.4-mini")
  provider chat54Mini {
    params {
      maxCompletionTokens  16384
    }
  }
  ```

  **Lifecycle (`@enabled` / `@disabled`).** Providers accept the lifecycle
  flags. `@enabled` is the explicit-on default (no-op); `@disabled` skips
  the provider at load -- **not registered, no auth resolution** -- so it
  emits zero "registered as unavailable" warnings while staying in the
  tree for a future re-enable. `@disabled` on a `@base` **propagates** to
  every child that `@extends` it, turning the whole vendor lane off:

  ```memql
  @disabled
  @base
  @type("Google")
  provider google {
    auth { apiKey env("MEMQL_SI_GOOGLE_API_KEY") }
  }
  ```

  Dependents degrade gracefully: a policy whose `@primary` is disabled
  routes via its `@fallback`; a prompt whose `@defaultProvider` is
  disabled falls back to the default structured provider.

  > **Semantics.** `@disabled` means the construct is **not loaded/active
  > at runtime right now**. It does NOT mean deprecated, abandoned, or
  > exempt from maintenance / refactors / conformance -- it is a reversible
  > on/off switch (a separate axis from `@deprecated`). This applies to
  > every construct that takes the flag (functions / builtins / prompts /
  > specs / seeds / providers).
  
  **Provider types** (registered in `component/memql/si_providers.go`):
  - `OpenAI` / `OpenAIChat` — chat completions (non-streaming)
  - `OpenAIStream` — streaming chat completions (progressive text)
  - `OpenAITTS` — text-to-speech via `/v1/audio/speech`
  - `Anthropic` — Claude chat / vision

  Voice-to-voice via the OpenAI Realtime conversation mode was retired
  in favour of the Polyphon pipeline (LiveKit + ASR/TTS in
  `integrations/deepgram/` and `integrations/openai/`). The
  `OpenAIRealtime` provider type and the corresponding `realtime*`
  configs have been removed; the only Realtime usage today is
  transcription-only ASR.

  **Available text providers** (representative; full list in
  `providers/v1/`):

  | Provider | Type | Model | Tier | Use case |
  |----------|------|-------|------|----------|
  | `chat54Nano` | OpenAI | gpt-5.4-nano | Budget | Ultra-cheap fallback |
  | `chat54Mini` | OpenAI | gpt-5.4-mini | Economy | Cost-effective chat (default for most prompts) |
  | `chat54` | OpenAI | gpt-5.4 | Standard | Full chat |
  | `chat54Pro` | OpenAI | gpt-5.4-pro | Premium | High-stakes tasks |
  | `stream54Mini` | OpenAIStream | gpt-5.4-mini | Economy | Streaming economy |
  | `stream54` | OpenAIStream | gpt-5.4 | Standard | Streaming full |
  | `claudeSonnet`, `claudeHaiku`, `claudeOpus` (and `streamClaude*`) | Anthropic | various | -- | Claude alternates |

  See docs/polyphon-architecture.md for
  the voice pipeline.

- **Prompt templates** live in `prompts/v1/**/*.memql`. Each `.memql` file defines a single prompt using the `func (Prompt)` receiver. Prompts include `@description`, `@defaultProvider`, `@templateFile` attributes, and an `@input` block that declares the expected arguments (replacing the former JSON Schema validation).
- **SI cache env vars** control response reuse:  
  - `MEMQL_SI_CACHE_DEFAULT_ENABLED` (`true`/`false`) toggles whether `si()` calls cache results when no explicit TTL is provided.  
  - `MEMQL_SI_CACHE_MAX_SECONDS` caps any SI cache entry (and doubles as the default TTL when caching is enabled). The engine clamps this to **≤ 300 seconds (5 minutes)**.

### Calling `si()` inside `shape()`

```
shape(
  concept==v1:document;payload.needsSummary==true,
  {
    "id": node("id"),
    "title": node("payload.title"),
    "summary": si(
      "docSummary.v1",
      {
        "title": node("payload.title"),
        "content": node("payload.content")
      }
    )
  }
)
```

`si("template.id" [, dataObject [, "providerName"]])` accepts:

1. **Template ID** – required string literal matching a prompt file.
2. **Data object** – optional JSON-like object literal evaluated with the shape helpers (`node()`, `children()`, etc.). When omitted, an empty object is used.
3. **Provider override** – optional string literal to force a provider for a single call.
4. **Cache TTL** – optional integer literal (seconds) that enables caching for this specific call. The value must be between `0` and `MEMQL_SI_CACHE_MAX_SECONDS` (inclusive); `0` disables caching even if the global default is on.

Only string literals are allowed for the template and provider arguments. If the provider override is omitted, the engine uses the template's `defaultProvider`, then falls back to `MEMQL_DEFAULT_PROVIDER`. The engine enforces projection-only usage—if `si()` appears inside filters, joins, sorts, or grouping expressions the query fails with `si() cannot be used in filter, join, sort, or group expressions; use it only in projection.`

### SI cache behavior

The SI cache sits inside the runtime and hashes `{templateId, provider, renderedPrompt}` as the cache key. When caching is enabled (globally or via the per-call TTL argument), a successful provider response is reused until its TTL expires—preventing duplicate LLM calls for identical prompts. The cache TTL is always clamped to five minutes. Set `MEMQL_SI_CACHE_DEFAULT_ENABLED=false` to require explicit TTLs on every `si()` call, or pass `0` as the per-call TTL to skip caching for a specific invocation even when the default is enabled.

The provider response is inserted directly into the shaped output: if the model returns valid JSON it is decoded into native structures, otherwise the raw string is returned.

## JSON Serialization with `json()`

The `json()` function serializes a template value to a JSON string. This is essential when embedding structured data in SI prompt templates, where the prompt text expects valid JSON syntax rather than Go's native map format.

### Why `json()` is needed

SI prompt templates are rendered using Go's `text/template` package. When you pass a map or object directly, it renders as Go's internal format:

```
Without json(): map[name:John email:john@example.com]
With json():    {"name":"John","email":"john@example.com"}
```

The SI model needs valid JSON to parse the data correctly. Use `json()` whenever you're embedding structured data in a prompt that expects JSON.

### Basic Usage

```
shape(
  concept==v1:incident,
  {
    "id": node("id"),
    "triage": si("incidentTriage.v1", {
      "incidentJSON": json(node("payload"))
    })
  }
)
```

The `json(node("payload"))` serializes the entire payload object to a JSON string, which the prompt template can embed directly.

### Common Patterns

#### Embedding node payload

```
si("analyzer.v1", { "dataJSON": json(node("payload")) })
```

#### Embedding selected fields

```
si("summarizer.v1", {
  "contextJSON": json({
    "id": node("id"),
    "title": node("payload.title"),
    "status": node("payload.status")
  })
})
```

#### Embedding related data

```
si("playbook.v1", {
  "serverJSON": json(node("payload")),
  "eventsJSON": json(select(concept==v1:event;payload.serverId==node("id"), "payload"))
})
```

### When to use `json()` vs direct values

| Scenario | Use |
|----------|-----|
| Prompt expects `{{.fieldName}}` as a JSON object string | `json(node("payload"))` |
| Prompt expects `{{.title}}` as a plain string | `node("payload.title")` |
| Embedding complex nested structures | `json({...})` |
| Simple scalar values (strings, numbers) | Direct value, no `json()` needed |

## Conditional Logic with `match()`

The `match()` function provides conditional value selection inside `shape()` templates. It evaluates conditions in order and returns the value of the first matching case, enabling deterministic business logic without SI overhead.

### Basic Syntax

```
match(
  case(<condition>, <value>),
  case(<condition2>, <value2>),
  default(<fallback>)
)
```

- **`case(condition, value)`** – Evaluates the condition; if true, returns the value and stops (short-circuit evaluation).
- **`default(value)`** – Returns this value if no case matches. Optional but recommended.
- Cases are evaluated in declaration order. The first match wins.

### Condition Types

#### 1. Spec References

Use a named spec as the condition. The spec is evaluated against the current node's payload:

```
shape(
  concept==v1:customer,
  {
    "id": node("id"),
    "origin": match(
      case(hispanicMiddleName, "hisp"),
      case(asianMiddleName, "asian"),
      default("unknown")
    )
  }
)
```

Specs referenced in `match()` must be defined in `specs/v1/*.json` or as inline specs:

```json
{
  "name": "hispanicMiddleName",
  "description": "Middle name appears in Hispanic name list.",
  "expression": "payload.middleName in (\"Garcia\",\"Rodriguez\",\"Martinez\",\"Lopez\",\"Hernandez\")"
}
```

> **Note:** Relationship-based specs (e.g., `parentOf(...)`, `childOf(...)`) are not supported in `match()` conditions because they require graph traversal beyond the current node.

#### 2. Inline Comparisons

Use `node()` with a comparison operator for ad-hoc conditions:

```
shape(
  concept==v1:customer,
  {
    "displayStatus": match(
      case(node("payload.status") == "active", "Active Account"),
      case(node("payload.status") == "pending", "Pending Activation"),
      case(node("payload.status") == "suspended", "Account Suspended"),
      default("Unknown Status")
    )
  }
)
```

### Supported Comparison Operators

| Operator | Example | Description |
|----------|---------|-------------|
| `==` | `node("payload.x") == "value"` | Equals |
| `!=` | `node("payload.x") != "value"` | Not equals |
| `>` | `node("payload.score") > 90` | Greater than |
| `>=` | `node("payload.score") >= 80` | Greater than or equal |
| `<` | `node("payload.score") < 50` | Less than |
| `<=` | `node("payload.score") <= 60` | Less than or equal |
| `in` | `node("payload.country") in ("US","CA","MX")` | Value in list |
| `not in` | `node("payload.tier") not in ("free","trial")` | Value not in list |
| `has` | `node("payload.tags") has "urgent"` | Array contains value |
| `==nil` | `node("payload.middleName") ==nil` | Field is null or absent |
| `!=nil` | `node("payload.email") !=nil` | Field exists and is not null |

### Short-Circuit Evaluation

Cases are evaluated in order. Once a condition matches, remaining cases (including any `si()` calls) are skipped:

```
shape(
  concept==v1:order,
  {
    "priority": match(
      case(node("payload.total") > 10000, "critical"),
      case(node("payload.total") > 5000, "high"),
      case(node("payload.total") > 1000, "medium"),
      default("low")
    )
  }
)
```

An order with `total: 7500` matches the second case (`> 5000`) and returns `"high"` without evaluating subsequent cases.

### Hybrid Pattern: Deterministic + SI Fallback

The most powerful use of `match()` combines deterministic specs with SI fallbacks. Known cases are handled instantly without SI cost; unknown cases fall through to an SI classifier:

```
shape(
  concept==v1:customer,
  {
    "id": node("id"),
    "middleName": node("payload.middleName"),
    "nameOrigin": match(
      case(hispanicMiddleName, "Hispanic"),
      case(asianMiddleName, "Asian"),
      case(europeanMiddleName, "European"),
      default(si("nameOriginClassifier.v1", {
        "name": node("payload.middleName")
      }))
    )
  }
)
```

**Benefits:**
- **Cost efficiency**: Known patterns (Hispanic, Asian, European names) are handled deterministically—no SI call.
- **Accuracy**: Unknown patterns still get intelligent classification via SI.
- **Transparency**: You can see exactly which cases trigger SI spend.
- **Caching**: SI responses can be cached per the TTL rules, further reducing cost.

### Nested Match Expressions

Match expressions can be nested inside objects or used as values in other match cases:

```
shape(
  concept==v1:transaction,
  {
    "id": node("id"),
    "riskAssessment": match(
      case(node("payload.amount") > 10000, {
        "level": "high",
        "reason": match(
          case(internationalTransaction, "Cross-border high-value transfer"),
          default("Domestic high-value transfer")
        )
      }),
      case(node("payload.amount") > 1000, {
        "level": "medium",
        "reason": "Standard review threshold"
      }),
      default({
        "level": "low",
        "reason": "Below review threshold"
      })
    )
  }
)
```

### Match with Relationship Data

While `match()` conditions cannot use relationship functions directly, you can use match inside relationship traversals:

```
shape(
  concept==v1:examples:world,
  {
    "id": node("id"),
    "modules": children({
      "id": node("id"),
      "tierBadge": match(
        case(node("payload.tier") == "gold", "[Gold]"),
        case(node("payload.tier") == "silver", "[Silver]"),
        default("[Bronze]")
      )
    })
  }
)
```

### Best Practices

1. **Order cases by specificity**: Put more specific conditions first to ensure correct matching.
2. **Always include a default**: Prevents `null` values when no case matches.
3. **Use specs for reusable logic**: Define complex conditions as specs for reuse and testability.
4. **Reserve SI for genuinely ambiguous cases**: Deterministic rules are faster and cheaper.
5. **Leverage caching**: When using `si()` in default, consider setting a cache TTL to reduce repeated calls.

## Smart Logic Engine patterns

Warning: Large language models are expensive and should be treated like accelerator cards in the execution plan. MemQL keeps SI usage safe by only allowing `si()` inside projections (`select`, `shape`, or spec outputs that are themselves projected). Filters, joins, sorts, pagination, and mutations remain 100 % deterministic. Before wiring any of the patterns below into prod, confirm that the surrounding specs already catch abuse cases and that operators understand where the SI spend occurs.

At a high level each pattern pairs:

- **Specs** for deterministic triage/flagging
- **`select`/`shape`** for assembling structured context
- **An SI template** for optional narrative or classification output

The result is a “smart logic engine” that can reason over fresh time-series data while leaving the database, cache, and relationship traversals deterministic.

### Pattern catalog

Each entry calls out the intended goal, the template ID, a MemQL snippet, and why the approach is useful. Feel free to mix-and-match the building blocks so long as SI remains in the projection layer.

#### 1. Incident triage bundle

Goal: Attach structured risk guidance to every open incident using `incidentTriage.v1` (returns `{riskScore,userImpact,recommendedActions}`).

```
shape(
  concept==v1:incident; payload.status=="open",
  {
    "incidentId": node("id"),
    "severity":   node("payload.severity"),
    "service":    node("payload.service"),
    "triage": si("incidentTriage.v1", { "incidentJSON": json(node("payload")) })
  }
)
```

Why it helps: UIs can sort or threshold on `triage.riskScore` without the LLM ever participating in filters.

#### 2. Document summaries via API

Goal: Summarize documents flagged for review with `docSummary.v1`.

```
shape(
  concept==v1:document; payload.needsSummary==true,
  {
    "id":      node("id"),
    "title":   node("payload.title"),
    "summary": si("docSummary.v1", { "title": node("payload.title"), "content": node("payload.content") })
  }
)
```

Why it helps: Clients can show human-readable snippets with zero additional round-trips.

#### 3. Multi-language anomaly explainers

Goal: Provide operator-facing descriptions in a requested locale using `anomalyExplainMultilang.v1`.

```
shape(
  concept==v1:event; payload.isAnomaly==true,
  {
    "eventId":     node("id"),
    "timestamp":   node("payload.timestamp"),
    "explanation": si("anomalyExplainMultilang.v1", { "language": "es-MX", "eventJSON": json(node("payload")) })
  }
)
```

Why it helps: Guarantees template-consistent copy in any supported locale while MemQL enforces schema validity for the inputs.

#### 4. “Why was this flagged?” helper

Goal: Pair deterministic specs (for example `needsReview` defined as `payload.amount>10000`) with an explainer template (`reviewReason.v1`).

```
shape(
  concept==v1:transaction; needsReview,
  {
    "txId":   node("id"),
    "amount": node("payload.amount"),
    "flags":  ["needsReview"],
    "explanation": si("reviewReason.v1", { "transactionJSON": json(node("payload")) })
  }
)
```

Why it helps: Compliance tooling can show both the spec name and the generated explanation while all enforcement remains deterministic.

#### 5. Per-server remediation playbooks

Goal: Aggregate a server record plus recent events and feed them into `serverPlaybook.v1`.

```
shape(
  concept==v1:server,
  {
    "serverId": node("id"),
    "hostname": node("payload.hostname"),
    "playbook": si(
      "serverPlaybook.v1",
      {
        "serverJSON": json(node("payload")),
        "recentEvents": json(select(concept==v1:event; payload.serverId==node("id"), "payload"))
      }
    )
  }
)
```

Why it helps: Delivers SRE-ready runbooks derived from the live event stream.

#### 6. Notification copy factory

Goal: Pre-compute subject/body pairs for communications using `incidentNotificationEmail.v1`.

```
shape(
  concept==v1:incident; payload.status=="open",
  {
    "incidentId": node("id"),
    "severity":   node("payload.severity"),
    "email":      si("incidentNotificationEmail.v1", { "incidentJSON": json(node("payload")) })
  }
)
```

Why it helps: Downstream workers can send the templated copy directly, keeping email/SMS pipelines deterministic.

#### 7. Semantic classification on user events

Goal: Attach semantic labels with `userEventClassifier.v1`.

```
shape(
  concept==v1:memql:backend:userEvent,
  {
    "eventId": node("id"),
    "userId":  node("payload.userId"),
    "raw":     node("payload"),
    "classification": si("userEventClassifier.v1", { "eventJSON": json(node("payload")) })
  }
)
```

Why it helps: Analytics stacks can group on `classification.labels` without invoking SI themselves.

#### 8. Policy/compliance narratives

Goal: Explain compliance posture with `policyExplain.v1`.

```
shape(
  concept==v1:accessLog,
  {
    "logId":  node("id"),
    "user":   node("payload.user"),
    "action": node("payload.action"),
    "policyEval": si(
      "policyExplain.v1",
      { "accessJSON": json(node("payload")), "policyId": "PCI-DSS-ACCESS-01" }
    )
  }
)
```

Why it helps: Specs enforce the real policy; SI adds human-readable justification for auditors.

#### 9. Activity digests for people or assets

Goal: Combine a base record plus recent events in `activitySummary.v1`.

```
shape(
  concept==v1:memql:backend:user,
  {
    "userId": node("id"),
    "name":   node("payload.name"),
    "activitySummary": si(
      "activitySummary.v1",
      {
        "userJSON": json(node("payload")),
        "recentEvents": json(select(concept==v1:memql:backend:userEvent; payload.userId==node("id"), "payload"))
      }
    )
  }
)
```

Why it helps: Customer-success consoles get ready-to-read summaries with no extra joins.

#### 10. Projection-only SI specs

Goal: Wrap an SI helper inside a spec that is explicitly projection-only.

```
{
  "name": "incidentSIAnalysis",
  "description": "Diagnostic analysis of an incident using SI.",
  "expression": "si(\"incidentDebugAnalysis.v1\", { \"incidentJSON\": json(payload) })",
  "usesSI": true
}
```

```
shape(
  concept==v1:incident; payload.status=="open",
  {
    "incidentId": node("id"),
    "analysis":   incidentSIAnalysis
  }
)
```

Why it helps: Gives engineers a named analysis block they can project anywhere while the parser enforces “no filters” on `usesSI` specs.

#### 11. Release regression digest

Goal: After every deployment, summarize regressions detected by specs (`recentRegression` or similar) via `releaseRegressionDigest.v1`.

```
shape(
  concept==v1:release;payload.shippingState=="shipped",
  {
    "releaseId": node("id"),
    "preview": si(
      "releaseRegressionDigest.v1",
      {
        "releaseJSON": json(node("payload")),
        "regressions": json(select(concept==v1:incident;payload.releaseId==node("id");recentRegression, "payload"))
      }
    )
  }
)
```

Why it helps: Product/QA teams receive a machine summary citing every deterministic regression node included in the prompt.

#### 12. Adaptive SLA guardrails

Goal: Turn latency metrics plus spec-evaluated burn rates into plain-language alerts using `slaGuardrailAdvisor.v1`.

```
shape(
  concept==v1:service; payload.active==true,
  {
    "serviceId": node("id"),
    "latencyP95": node("payload.latencyP95"),
    "burningSLA": slaBurnAlert,
    "advisor": si(
      "slaGuardrailAdvisor.v1",
      {
        "serviceJSON": json(node("payload")),
        "slaStatus": json(slaBurnAlert)
      }
    )
  }
)
```

Why it helps: Operators can read `advisor` for guidance while automation still keys off the boolean `slaBurnAlert` spec.

#### 13. Learning world overview

Goal: Provide learners with a structured overview of an active world, including a summary of its modules.

```
shape(
  concept==v1:examples:world;payload.status=="active",
  {
    "worldId": node("id"),
    "title": node("payload.title"),
    "overview": si(
      "docSummary.v1",
      {
        "title": node("payload.title"),
        "content": json(contains({
          "name": node("payload.name"),
          "tier": node("payload.tier"),
          "summary": node("payload.summary")
        }))
      },
      "",
      180
    )
  }
)
```

Why it helps: Learners get a cached summary they can review before starting a learning path, without hitting the SI endpoint repeatedly. The `contains()` helper traverses the world→module relationship and `json()` serializes the module data for the SI prompt.

> **Note:** Directive functions like `select()`, `paginate()`, and `sort()` are **not valid inside shape templates**. These are top-level query directives, not shape template helpers. To include related data inside a shape template, use relationship helpers like `contains()`, `children()`, `owns()`, etc. See [Invalid Query Examples](#invalid-query-examples) for more details.

### Comparison Examples

- `concept==v1:examples:world`
- `payload.status in ("active","comingSoon")`
- `createdAt>="2025-01-01T00:00:00Z"`
- `payload.metadata.tags==nil`

### Operator Reference

| Operator          | Example                                                                 | Notes                                                                                     |
|-------------------|-------------------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `==` / `!=`       | `payload.status=="open"`                                                | Direct equality / inequality.                                                             |
| `>` / `<`         | `payload.score>0.85`                                                    | Numeric comparisons (strings use lexical ordering).                                       |
| `>=` / `<=`       | `payload.attempts<=3`                                                   | Greater-than-or-equal / less-than-or-equal.                                               |
| `in` / `not in`   | `payload.stage in ("lead","qualified","won")`                           | Membership against a list. Works with both scalar fields and string arrays. `not in` matches everything *not* in the set. |
| `has`             | `payload.tags has "urgent"`                                             | Array contains value. Returns true if the array field includes the specified element.     |
| `==nil`           | `payload.metadata.notes==nil`                                           | Field absent or explicitly `null`. Apply to payload paths or intrinsic columns.           |
| `!=nil`           | `payload.metadata.tags!=nil`                                            | Field present with a non-null value.                                                      |

Combined example covering several operators:

```
concept==v1:lead;
payload.status in ("new","contacted");
payload.source!=nil;
payload.metadata.owner==nil
```

The query above returns all leads in `new`/`contacted` state that already have a `source` value but still need an assigned `owner`.

### Array Field Support

The `in` and `not in` operators work seamlessly with both scalar fields and string array fields:

**Scalar field** (traditional behavior):
```memql
payload.status in ("active","pending")
```
Matches if `status` equals "active" OR "pending".

**Array field** (automatic detection):
```memql
payload.topics in ("filters","shape")
```
Matches if `topics` array contains "filters" OR "shape".

For example, a module with `topics: ["filters", "limits", "sorting"]` would match the query above because it contains "filters".

The `not in` operator works inversely:
```memql
payload.tags not in ("admin","system")
```
- For arrays: matches if the array contains NONE of the specified values
- For scalars: matches if the value is NOT in the list

The `has` operator provides a more readable alternative for single-value array containment checks:
```memql
payload.tags has "urgent"
```
This is equivalent to `payload.tags in ("urgent")` but reads more naturally for single values.

> **Note:** Array support is limited to string arrays. Numeric or boolean array matching is not supported.

## Relationship Functions

Relationship expressions wrap another MemQL query and expand results through concept-defined edges:

| Function        | Purpose                                                                                                  |
|-----------------|----------------------------------------------------------------------------------------------------------|
| `parentOf(expr)`| Finds parents referenced by `parent` relationships.                                                      |
| `childOf(expr)` | Retrieves children whose payload points to the parent ID.                                                |
| `contains(expr)`| Expands collection membership arrays.                                                                    |
| `owns(expr)`    | Resolves ownership links in both directions.                                                             |
| `aliasOf(expr)` | Collects nodes sharing alias groups.                                                                     |
| `equals(expr)`  | Follows equality relationships similar to alias.                                                         |
| `interactsWith` | Traverses recorded interaction edges (e.g., conversation participants).                                  |
| `createdBy`     | Resolves creator nodes using payload or table-backed metadata.                                           |
| `ids(expr)`     | Returns lightweight nodes (no payload/schema) useful for identifier lists.                               |

Relationship outputs can be combined with filters:  
`contains(id=="project-123");payload.status=="open";payload.priority<=2`

### Sorting Function

Use `sort(<expr>, "<field>", "<direction>?", ...)` to order results. The function:

- Accepts any MemQL expression as the first argument.
- Requires at least one string literal field name; directions are optional (`"asc"` or `"desc"`, defaulting to `"desc"`).
- Allows multiple field/direction pairs for deterministic tie-breaking.
- Must wrap the entire query expression (i.e., `sort(...)` should be the outermost call).

Example:

```
sort(
  paginate(childOf(concept==v1:examples:world;id=="v1:examples:world:world-aurora"), 100),
  "createdAt","desc",
  "id","asc"
)
```

### Cache Hints & TTL Hierarchy

MemQL applies cache TTLs in three layers (shortest non-zero value wins, zero disables caching):

1. **Global ceiling** – `CACHE_MAX_TTL` (seconds) limits how long any query can live in the cache. Set to `0` to remove the limit.
2. **Concept defaults** – each `concept.json` declares `cacheTTLSeconds`. These values are clamped so they never exceed the global ceiling.
3. **Per-query hints** – add `@cache(<seconds>)` to a `concept` comparison to override (or disable with `cache(0)`) that concept for the current query.

Hints only attach to `concept` comparisons using `== "concept-name"`. Examples:

- **Basic – disable caching for hot lists**

```
paginate(
  concept@cache(0)==v1:examples:world;payload.status=="active",
  7,
  0
)
```

- **Medium – shorten TTL for related data**

```
concept==v1:examples:world;
contains(
  concept@cache(30)==v1:examples:module;
  payload.worldId=="v1:examples:world:world-aurora"
)
```

The parent `world` nodes use their concept default (bounded by `CACHE_MAX_TTL`), while related `module` nodes expire after 30 seconds.

- **Complex – mix hints across relationships**

```
sort(
  paginate(
    concept@cache(0)==v1:examples:world;
    childOf(
      concept@cache(300)==v1:examples:module;
      payload.worldId=="v1:examples:world:world-aurora"
    );
    createdBy(concept@cache(600)==v1:memql:backend:user),
    50
  ),
  "createdAt","desc"
)
```

This query disables caching for the root conversations, limits cached messages to five minutes, and leaves users at ten minutes (or whatever is lower between their concept default and `CACHE_MAX_TTL`).

If any layer resolves to `0`, the engine skips caching for the entire query, guaranteeing fresh reads after high-churn writes.

#### Advanced: Cache Hints, Cache Keys, and @fields Overlaps

- The parser folds `@cache(<seconds>)` into the canonical `concept==` comparison (`concept@cache(30)==v1:assistant`), so different hint values generate different cache keys. Two queries that only differ by `@cache(30)` vs `@cache(0)` will never share or overwrite cache entries.
- Non-zero `@cache()` hints can re-enable caching for concepts whose `cacheTTLSeconds` is `0`. The hint value becomes the effective TTL (still clamped by `CACHE_MAX_TTL`) as long as it stays above zero.
- Even though `@cache(0)` produces a unique cache signature, it still prevents caching. `cacheTTLForTree()` clamps every concept’s TTL (global ceiling → concept default → hint override) and skips writes entirely when the resolved TTL is `0`.
- Concept-scoped `@fields(...)` annotations also participate in the cache key through the projection signature. Adjusting those per-concept projections creates unique cache entries, ensuring callers never receive a broader or narrower payload than they requested.

### Pagination Function
### select() Projections

`select(<expr>, "<field>", "<field2>", ...)` projects the result payloads down to explicit paths. Fields must be quoted strings that start with either a metadata token (`"meta.concept"`, `"meta.*"`, `"id"`, etc.) or a payload path (`"payload.profile.displayName"`). The parser enforces valid paths and supports direct-child wildcards with the `payload.<object>.*` suffix.

- The directive can wrap any MemQL expression, sits alongside `sort()`/`paginate()`, and may only appear once per query.
- Metadata becomes opt-in: `id` is always present, but `concept`, `type`, `createdAt`, `createdBy`, and `schema` are excluded unless you request them explicitly (for example via `"meta.concept"` or `"meta.*"`). Bare tokens (`"concept"`, `"createdAt"`) behave the same for convenience.
- Nested selections prune maps so only the requested branches survive. For example, `select(concept==v1:assistant, "payload.profile.displayName")` strips every other payload property but leaves `payload.profile.displayName` intact.
- Wildcards copy every direct child of the specified object: `select(..., "payload.profile.*")` keeps the full `profile` object without enumerating its keys, while `select(..., "meta.*")` restores the entire metadata envelope.
- Use `"payload"` (without a wildcard) when you need the full payload. The `payload.*` shorthand only works on nested objects (for example `payload.profile.*`) and is rejected at the root level.
- Include `"meta.schema"` in the field list when you need the embedded JSON Schema document; otherwise it is omitted along with unrequested metadata.
- If no payload fields remain after intersection (see `@fields` below), the engine returns an empty `payload` object so callers can rely on consistent shapes.

Projection examples:

```
select(
  paginate(concept==v1:assistant;payload.active==true, 20),
  "id",
  "payload.profile.displayName",
  "payload.profile.*"
)
```

```
select(
  concept==v1:assistant;payload.active==true,
  "meta.*",
  "payload.profile.displayName"
)
```

### Concept Field Annotations

Attach `@fields("...")` to `concept==` comparisons to request concept-specific fields that optionally intersect with the global `select()` list:

```
concept@fields("payload.title","payload.status")==v1:examples:world;
concept==v1:examples:module
```

- `@fields` only applies to `concept==` comparisons with string literals. The engine validates the syntax and errors if the annotation is attached to other fields or operators.
- When both global and concept-specific selections exist, the engine intersects them so each concept only returns the overlap. When no global `select()` exists, the concept-specific list stands on its own.
- You can combine multiple annotated concepts inside the same query to shape related records differently.

`paginate(<expr>, limit, offset?)` constrains result windows. When omitted, the engine uses the defaults (`limit = MEMORY_ENGINE_MAX_RESULTS`, `offset = 0`). The function:

- Requires at least one integer argument (limit) greater than zero.
- Accepts an optional second integer argument for offset.
- Can be combined with other helpers (e.g., `sort(paginate(...), ...)`).

Example:

```
paginate(concept==v1:examples:module;payload.worldId=="v1:examples:world:world-aurora", 200, 400)
```

### Temporal Snapshots

`asOf(<expr>, "<timestamp>")` evaluates a query using a consistent historical snapshot.

- Supply an RFC3339/RFC3339Nano timestamp string to pin to a specific moment.
Example:

```
asOf(concept==v1:assistant;payload.active==true, "2025-11-01T00:00:00Z")
```

### Depth Overrides

`withDepth(<expr>, depth)` customizes relationship traversal depth.

- Depth must be a positive integer.
- Combine with other helpers to control how aggressively the engine expands relationships.

Example:

```
withDepth(parentOf(concept==v1:examples:quest;id=="v1:examples:quest:quest-nodes"), 3)
```

### Invalid Query Examples

The parser expects directive helpers (such as `withDepth()`, `paginate()`, `sort()`, `asOf()`) to wrap the entire base expression. Embedding them inside `shape()` helpers or relationship functions causes syntax errors.

**Invalid**

```
shape(
  concept==v1:examples:challenge:attempt;payload.learnerProfileId=="learner-ada";payload.isCorrect==true,
  {"achievements": owns(withDepth(parent(node()), 1))}
)
```

Error: `invalid query syntax: unknown shape function "withDepth"` – the directive is nested inside `owns(...)`, so the parser treats it as an unknown shape helper.

**Use this instead**

```
shape(
  withDepth(
    concept==v1:examples:challenge:attempt;payload.learnerProfileId=="learner-ada";payload.isCorrect==true,
    1
  ),
  {
    "attempt": node("payload.challengeId","payload.awardedXP"),
    "achievements": owns(parent(node()))
  }
)
```

By wrapping the full expression with `withDepth()`, the directive is applied before `shape()` executes, keeping the syntax valid while still limiting traversal depth.

Relationship helpers inside `shape()` templates must also be limited to the supported list (`node`, `children`, `contains`, `owns`, `aliases`, `createdBy`, `interactions`, `match`, `ai`). Trying to call other helper names (for example `parent()`) results in the parser treating them as unknown shape functions. To walk up the graph, wrap the base expression with `parentOf()` (optionally guarded by `withDepth()`) before applying `shape()`.

**Invalid**

```
shape(
  concept==v1:examples:challenge:attempt;payload.isCorrect==true,
  {"achievements": owns(parent(node()))}
)
```

Error: `invalid query syntax: unknown shape function "parent"` – templates do not support arbitrary relationship helpers.

**Use this instead**

```
shape(
  concept==v1:examples:challenge:attempt;payload.isCorrect==true,
  {"achievements": owns(node("payload.title","payload.tier"))}
)
```

If you need to traverse parents of the owned nodes, express that traversal in the base MemQL expression using `parentOf(owns(...))` (and then shape the result) rather than nesting unsupported helpers inside the template.

Relationship functions such as `interactsWith()` must also live in the MemQL expression, not inside the template.

**Invalid**

```
shape(
  concept==v1:examples:mentor;id=="mentor-lira",
  {"modules": interactsWith(node("payload.name"))}
)
```

Error: `invalid query syntax: unknown shape function "interactsWith"` – the template helper isn’t recognized.

### Walking Parent Chains

Use `parentOf(<expr>)` to change the root set to the desired ancestors, then optionally clamp recursion with `withDepth()`. There is no `parents()` template helper—relationship traversals must happen in the base MemQL expression before shaping.

```
shape(
  withDepth(
    parentOf(concept==v1:examples:module;payload.id=="module-auriga"),
    3
  ),
  {
    "ancestor": node("payload.title"),
    "concept": node("concept"),
    "id": node("id")
  }
)
```

The query above returns one shaped object per ancestor (closest parent first) with `result.data` containing the lineage list. Increase the `withDepth()` value to walk further up the graph, or pair the expression with `shapeWithBundle()` when you also need access to the raw nodes/edges for visualization.

**Use this instead**

```
shape(
  interactsWith(concept==v1:examples:mentor;id=="mentor-lira"),
  {"mentor": node("payload.name","payload.title"), "modules": interactions(node("payload.name","payload.summary"))}
)
```

Run `interactsWith()` outside `shape()` (where relationship functions are supported), then use `interactions()` inside the template to traverse from the nodes returned by that expression.

Insert statements have a similar constraint: the `id` argument must be a string literal (or omitted so the engine generates one). Calling helper functions there (for example `uuid()`) triggers a syntax error because the parser is expecting the opening quote for a string.

**Invalid**

```
insert("v1:examples:challenge:attempt", id=uuid(), payload={...})
```

Error: `invalid query syntax: expected '"' to start string literal ...` – `uuid()` isn’t a valid literal.

**Use this instead**

```
insert("v1:examples:challenge:attempt", payload={...})
```

or provide a literal identifier:

```
insert("v1:examples:challenge:attempt", id="attempt-lab-demo", payload={...})
```

Shape template objects require **quoted string keys** and **`node()` function calls** for field access. Using bare identifiers (JavaScript-style object shorthand) triggers a syntax error.

**Invalid**

```
shape(
  concept==v1:examples:world;payload.status=="active",
  {
    id: id,
    title: payload.title,
    status: payload.status
  }
)
```

Error: `invalid query syntax: shape template object keys must be strings` – bare identifiers like `id:` are not recognized; the parser expects quoted keys.

**Use this instead**

```
shape(
  concept==v1:examples:world;payload.status=="active",
  {
    "id": node("id"),
    "title": node("payload.title"),
    "status": node("payload.status")
  }
)
```

Both the keys (`"id"`, `"title"`, `"status"`) and the field references (`node("id")`, `node("payload.title")`) must follow the correct syntax. This applies to all shape template objects, including nested templates inside `children()`, `owns()`, and `si()` data arguments.

## Sorting & Paging

- Sorting is declared inline with `sort(<expr>, "<field>", "<direction>?", ...)`.
- Supported fields: `id`, `concept`, `createdAt`, `createdBy`, `type`, and `payload.<path>`.
- Provide multiple field/direction pairs to add tiebreakers. Directions are optional (default `desc`).
- Limits and offsets always apply **after** sorting.
- Sorting on payload properties may cause the engine to fetch up to `MEMORY_ENGINE_MAX_WINDOW` rows to guarantee correctness.

## Introspection Functions

MemQL now exposes the documentation and concept catalog directly through the expression language so clients (human or SI) can bootstrap themselves dynamically.

These introspection calls are builtins declared in `functions/v1/builtin/*.json`. Their names, aliases, and argument contracts are loaded into the function registry at startup; the parser and executor route them through registry metadata rather than hardcoded name branches.

### `memqlDocs()`

Returns the embedded `docs/memql.md` file as a synthetic memory node:

```
memqlDocs()
```

Response payload (truncated for brevity):

```json
{
  "result": {
    "bundle": {
      "nodes": [
        {
          "concept": "memql:docs",
          "payload": {
            "format": "markdown",
            "content": "# MemQL Guide\n..."
          }
        }
      ],
      "edges": [],
      "rootIds": ["memql:docs:memql"]
    },
    "data": []
  },
  "errors": []
}
```

Use this when an agent needs to refresh its understanding of the language without shipping the guide alongside every request.

### `concepts()` / `concepts("pattern")`

Lists the concepts (and their schemas) available in the current deployment. An optional pattern argument filters concepts by name (case-insensitive substring match).

```
// List all concepts
concepts()

// Filter concepts by pattern (e.g., all CRM-related concepts)
concepts("crm")

// Combine with paginate() to page through long lists
paginate(ids(concepts()), 5, 0)
```

Each child node includes:

- `metadata`: normalized view of `concept.json` (name, description, type, skipDeleted, defaultFilter, cacheTTLSeconds, relationships).
- `schemas`: JSON objects for every schema file (e.g. `definition`, `delete`).

Example payload fragment:

```json
{
  "concept": "memql:concept",
  "payload": {
    "concept": "v1:assistant",
    "metadata": {
      "type": "object",
      "description": "Assistant definitions that combine prompt configuration with branding metadata.",
      "skipDeleted": false,
      "defaultFilter": "",
      "cacheTTLSeconds": 300,
      "relationships": [
        {"type":"createdBy","field":"createdBy","targetConcept":"v1:memql:backend:user","direction":"outgoing"}
      ]
    },
    "schemas": {
      "definition": { "$id": "v1.assistant", "...": "..." },
      "delete": { "...": "..." }
    }
  }
}
```

Because the result set is synthetic, wrap the call with `paginate()` whenever you expect many concepts.

> Directive helpers such as `paginate()`, `sort()`, `asOf()`, and `withDepth()` must be the outermost wrapper around the query expression. Wrap any other functions (like `ids()` or relationship traversals) **inside** these directives so the parser can peel them off cleanly.

### `validate()`

Validates a payload against a concept's JSON schema without persisting anything. Useful for agents to check payloads before attempting an insert:

```
validate({"concept": "v1:crm:lead", "payload": {"email": "test@example.com", "name": "John"}})
```

Returns a validation result node with:

- `valid`: boolean indicating if validation passed
- `errors`: array of validation error objects (empty if valid)
- `required`: sorted array of required field names from the schema
- `provided`: sorted array of field names present in the payload
- `schema`: summary including `$id` and property names

Example response for a valid payload:

```json
{
  "result": {
    "bundle": {
      "nodes": [
        {
          "concept": "memql:validate",
          "payload": {
            "valid": true,
            "errors": [],
            "required": ["email"],
            "provided": ["email", "name"],
            "schema": {
              "$id": "v1.crm.lead",
              "properties": ["email", "name", "phone", "source"]
            }
          }
        }
      ]
    }
  }
}
```

Example response for an invalid payload (missing required field):

```json
{
  "payload": {
    "valid": false,
    "errors": [
      {
        "instanceLocation": "",
        "keywordLocation": "/required",
        "error": "missing properties: 'email'"
      }
    ],
    "required": ["email"],
    "provided": ["name"],
    "schema": { "$id": "v1.crm.lead", "properties": ["email", "name"] }
  }
}
```

Agents can use `validate()` to:
1. Check payload shape before inserting (fail fast without side effects)
2. Discover required fields for a concept
3. Get schema property lists for building payloads

### `functions()`

Returns a minimal list of all registered user-defined functions. Designed for agent discovery with minimal payload size:

```
functions()
```

Returns:

```json
{
  "payload": {
    "functions": [
      {"name": "activeConversations", "description": "Returns active conversations", "kind": "query"},
      {"name": "createUser", "description": "Creates a new user", "kind": "mutation"}
    ],
    "count": 2
  }
}
```

Each entry includes only `name`, `description`, and `kind`. Use `help(name)` to get full details for a specific function.

### `tools()`

Returns MCP-compatible tool definitions for SI model integration:

```
tools()
```

Returns:

```json
{
  "payload": {
    "tools": [
      {
        "name": "searchDocuments",
        "description": "Search for documents by query",
        "inputSchema": {
          "type": "object",
          "properties": {
            "query": {"type": "string"},
            "limit": {"type": "number"}
          }
        }
      }
    ],
    "count": 1
  }
}
```

Each entry includes `name`, `description`, and `inputSchema` for MCP compatibility. Use `help(name)` for handler details.

### `help()`

Returns full details for a specific function or tool by name:

```
help("myFunction")
help({"name": "myTool"})
```

For functions, returns:

```json
{
  "payload": {
    "type": "function",
    "name": "myFunction",
    "description": "Full description",
    "kind": "query",
    "enabled": true,
    "argsSchema": [
      {"name": "userId", "type": "string", "optional": false}
    ],
    "cacheTTL": "5m"
  }
}
```

For tools, returns:

```json
{
  "payload": {
    "type": "tool",
    "name": "myTool",
    "description": "Full description",
    "inputSchema": {...},
    "handlerType": "query",
    "annotations": {"destructive": false}
  }
}
```

Returns an error if no function or tool matches the name.

### `shapeTemplates()`

Lists available shape templates for result projection. Optionally filter by concept:

```
shapeTemplates()                              -- All shapes
shapeTemplates("v1:crm:lead")                 -- Filter by concept (string shortcut)
shapeTemplates({"concept": "v1:crm:lead"})    -- Filter by concept (object)
```

Returns:

```json
{
  "payload": {
    "shapes": [
      {"name": "leadCard.v1", "description": "Standard projection for CRM leads"}
    ],
    "count": 1
  }
}
```

Each entry includes only `name` and `description`. Use `shapeHelp(name)` to get full template details.

### `shapeHelp()`

Returns full details for a shape template by name, including the template structure and input schema:

```
shapeHelp("leadCard.v1")
shapeHelp({"name": "leadCard.v1"})
```

Returns:

```json
{
  "payload": {
    "name": "leadCard.v1",
    "description": "Standard projection for CRM leads",
    "concepts": ["v1:crm:lead"],
    "template": {
      "id": "node(\"id\")",
      "name": "node(\"payload.name\")",
      "email": "node(\"payload.email\")"
    },
    "inputSchema": {
      "type": "object",
      "required": ["payload.source"],
      "properties": {
        "payload.name": {"type": ["string", "null"]},
        "payload.email": {"type": ["string", "null"]}
      }
    }
  }
}
```

Agents can use `shapeHelp()` to understand the exact template structure before applying shapes.

### `contentId()`

Predicts the content-addressed ID that would be generated for a concept+payload combination, without actually inserting the data. Uses the same SHA256 algorithm as `insert()`:

```
contentId({"concept": "v1:crm:lead", "payload": {"name": "Ada", "email": "ada@example.com"}})
```

Returns:

```json
{
  "payload": {
    "valid": true,
    "id": "sha256:abc123...",
    "concept": "v1:crm:lead"
  }
}
```

Error cases return structured error objects:

```json
{"valid": false, "error": "MISSING_REQUIRED_FIELD", "target": "concept"}
{"valid": false, "error": "CONCEPT_NOT_FOUND", "target": "v1:unknown:concept"}
```

### `previewInsert()`

Performs complete preflight validation without inserting: validates payload against schema, predicts the content ID, and checks if a record with that ID already exists:

```
previewInsert({"concept": "v1:crm:lead", "payload": {"name": "Ada", "source": "website"}})
```

Success response:

```json
{
  "payload": {
    "valid": true,
    "id": "sha256:abc123...",
    "exists": false,
    "warnings": []
  }
}
```

Validation failure response:

```json
{
  "payload": {
    "valid": false,
    "error": "SCHEMA_VALIDATION_FAILED",
    "details": [
      {"instanceLocation": "/source", "keywordLocation": "/required", "error": "missing properties: source"}
    ]
  }
}
```

Error codes:
- `MISSING_REQUIRED_FIELD` - Required argument (concept) not provided
- `CONCEPT_NOT_FOUND` - Concept does not exist
- `SCHEMA_ERROR` - Problem with concept schema
- `SCHEMA_VALIDATION_FAILED` - Payload doesn't match schema

Agents can use `previewInsert()` to:
1. Validate payloads before inserting (fail fast without side effects)
2. Predict the ID that will be assigned to a record
3. Check if a record already exists (for idempotent operations)
4. Get detailed validation errors for building correct payloads

## insert() Mutations

MemQL supports append-only inserts via the `insert()` function:

```
insert(
  "v1:examples:world",
  id="world-nebula",
  payload={
    "title":"Nebula Grid",
    "slug":"nebula-grid",
    "status":"active",
    "difficultyCurve":"advanced"
  }
)
```

Rules:

1. One `insert()` per statement; no mixing reads and writes.
2. Payload must match the concept schema (validated automatically).
3. Relationship hints (`parent`, `aliasOf`) rewrite the payload before persistence.
4. Inserts return the created node inside `result.bundle` (single node, empty edge list, and `rootIds` containing the inserted ID). `result.data` stays `[]` unless you wrap the mutation with `shape()`.
5. Stored identifiers always take the form `<concept>:<id>`; providing a bare `id` argument automatically applies the prefix.
6. When the `id` argument is omitted, the engine auto-generates a UUID v4 (and still prefixes it with the concept name) before returning the created node so callers can persist the assigned identifier.

### Versioning via Insert (The "Update" Pattern)

MemQL is built on TimescaleDB and follows an **append-only, immutable data model**. There is no `update()` mutation by design. Instead, to change a record's state:

1. **Insert a new version** with the same ID but updated payload fields
2. **Query to retrieve the most recent version** of each record (queries always return current state)
3. **Full history is preserved** and queryable via `asOf()`

**Example: Updating a lead's classification**

```
-- Original lead (unclassified)
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com"})

-- "Update" by inserting a new version with the same ID
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com", "classification": "hot"})

-- Query current state (returns the classified version)
concept==v1:lead;id=="lead:lead-123"

-- Query unclassified leads (current version missing classification field)
concept==v1:lead;payload.classification==nil
```

**Example: Archiving a world**

```
-- Mark as comingSoon instead of deleting
insert("v1:examples:world", id="world-aurora", payload={"title": "Aurora Grid", "status": "comingSoon"})

-- Query only active worlds
concept==v1:examples:world;payload.status=="active"
```

**Why append-only?**

| Benefit | Description |
|---------|-------------|
| **Audit trail** | Complete history of all changes with timestamps and actors |
| **Time travel** | Query data as it existed at any point: `asOf(expr, "2025-01-01T00:00:00Z")` |
| **No data loss** | Records are never destroyed; "deletes" are soft (set `active: false`) |
| **Determinism** | Same query + same timestamp = identical results, always |

This pattern is fundamental to MemQL. When building automations, functions, or any data workflows, always think **"insert new version"** rather than **"update existing record."**

## Worked Examples

### Basic Listing

```
concept==v1:assistant;payload.active==true @ "2025-11-01T00:00:00Z"
```

List all assistants that were active at the start of November 2025.

### Paginated Worlds by Recency

```
sort(
  paginate(concept==v1:examples:world;payload.status=="active", 25, 25),
  "createdAt","desc"
)
```

Returns the second page of active worlds, sorted by recency.

### Graph Traversal

```
childOf(concept==v1:examples:world;id=="v1:examples:world:world-aurora");payload.tier=="silver"
```

Fetch all silver-tier modules that belong to `world-aurora`.

### Mixed Relationships + Filters

```
parentOf(
  contains(
    concept==v1:examples:module;payload.tier in ("silver","gold")
  )
);payload.status=="active"
```

Return active worlds whose child modules have specific tiers, then pull their parents for auditing.

### Insert Examples

- **Basic insert**

  ```
  insert(
    "v1:memql:backend:user",
    id="user-123",
    payload={"email":"user@example.com","role":"developer"}
  )
  ```

- **Insert with relationships**

  ```
  insert(
    "v1:examples:module",
    id="module-advanced",
    parent="v1:examples:world:world-aurora",
    payload={
      "worldId":"v1:examples:world:world-aurora",
      "name":"Advanced Patterns",
      "tier":"gold",
      "summary":"Master complex query patterns and optimizations."
    }
  )
  ```

## Specifications

Specifications (specs) are named boolean predicates that can be embedded anywhere a regular filter expression is allowed. **Specs are concept-agnostic**—they reference only payload fields and relationship functions, never specific concepts. This makes specs universally shareable: the same spec works for any concept that has the required fields.

### Global Specs

Global specs can be defined in two formats: **JSON** (legacy) or **MemQL DSL** (preferred).

#### MemQL DSL Format (Preferred)

Create a `.memql` file in `specs/v1/<specName>/spec.memql`. This format uses the same syntax as functions and automations:

```memql
@enabled
@description("Node includes both email and phone number fields.")
func (Spec) hasUserContact() {
  payload.email!=nil;payload.phoneNumber!=nil
}
```

The spec name comes from the function name. Attributes like `@enabled` and `@description` provide metadata.

**Spec with OR conditions:**
```memql
@enabled
@description("Node has at least one contact method (email or phone)")
func (Spec) hasContactMethod() {
  payload.email!=nil,payload.phone!=nil
}
```

**Spec with relationship:**
```memql
@enabled
@description("Node's parent has status active")
func (Spec) hasActiveParent() {
  parentOf(payload.status=="active")
}
```

#### JSON Format (Legacy)

Alternatively, specs can be defined in `specs/v1/*.json`. Each document supplies `name`, optional `description`, and an `expression` string:

```json
{
  "name": "hasUserContact",
  "description": "Node includes both email and phone number fields.",
  "expression": "payload.email!=missing;payload.phoneNumber!=missing"
}
```

#### Loading Priority

When both `.memql` and `.json` files exist, the `.memql` file takes priority. Names must be camelCase (for example `hasEmail`). At startup the engine parses every file, validates the expression syntax, and rejects duplicates.

Global specs can reference other global specs; the loader resolves these dependencies, detects cycles, and pre-expands the final expression that the executor uses at runtime.

### Specs with Relationships

Specs can include relationship functions to create powerful reusable predicates. The relationship resolution uses the concept's relationship definitions at runtime, so these specs remain concept-agnostic:

```json
{
  "name": "hasActiveParent",
  "description": "Node's parent has status active.",
  "expression": "parentOf(payload.status==\"active\")"
}
```

```json
{
  "name": "inActiveCollection",
  "description": "Node belongs to an active collection.",
  "expression": "childOf(payload.active==true)"
}
```

When a relationship spec is applied, the engine:
1. Evaluates the inner expression to find matching nodes
2. Looks up each node's concept to find its relationship definitions
3. Traverses the relationships using the concept-specific configuration

This means the same spec works across different concepts that define compatible relationships.

### Inline Specs

Inline specs provide ad-hoc definitions inside a MemQL query. Declare them before the main expression using `:=` syntax:

```
hasEmail := payload.email!=missing
isActive := payload.active==true
activeWithEmail := hasEmail ; isActive

select(concept==v1:memql:backend:user;activeWithEmail, "payload")
```

Inline specs can reference previously declared inline specs or any global spec. Their names must also be camelCase and they cannot shadow global specs. Unlike global specs, inline specs **can** include concept constraints since they are defined in the context of a specific query.

### Spec Rules

Both global and inline specs:

- Must evaluate to a boolean expression (logical operators, comparisons, relationship functions, or nested specs).
- Track a reserved `UsesSI` flag whenever `si()` appears inside the spec expression. Specs flagged in this way cannot be used in filter expressions yet—the engine raises `Spec '<name>' uses si() and cannot be used in filter expressions.` until projection-safe spec contexts are supported.

**Global specs only:**

- Must not constrain `concept==`—they are concept-agnostic by design.
- Payload path validation happens at query time when the spec is applied to a specific concept, not at load time.

During parsing the engine resolves every spec reference into the underlying expression tree, so the resulting query plan behaves exactly as if the spec contents were written inline.

### Important: Specs Are Not Variables

MemQL is a declarative query language with **no concept of variables or mutable state**. Specs are **named predicates** (rules that evaluate to true/false against nodes), not variables that store values.

**Invalid — cannot assign literal values:**

```
isActive := true                    -- Cannot assign boolean literals
counter := 5                        -- No numeric variables  
result := someOtherVariable         -- No variable references
```

**Valid — specs define predicates (rules):**

```
isActive := payload.active==true    -- "node's active field equals true"
hasHighScore := payload.score>90 -- "node's score field is greater than 90"
combined := isActive ; hasHighScore -- Predicate composition with AND
```

Specs answer the question "does this node match?" — they don't hold values. This design keeps MemQL fully deterministic: every query produces the same result given the same data, with no hidden state.

## Functions

Functions are named, reusable MemQL queries that accept an optional JSON object argument. Unlike specs (which are boolean predicates for filtering), functions encapsulate complete queries with parameter-based filtering.

### Syntax Distinction

| Artifact | Syntax | Purpose |
|----------|--------|---------|
| **Spec** | `hasEmail` (no parens) | Boolean predicate for filtering |
| **Function** | `activeSpaces()` (parens required) | Reusable query returning nodes |
| **Function with args** | `activeSpaces({"userId": "u-1"})` | Parameterized query |

The parentheses make functions immediately recognizable: when you see `foo`, it's a spec; when you see `foo()`, it's a function call.

### Function Directory Structure

Named functions are flat `.memql` files. All functions from all version directories (v1, v2, etc.) are automatically discovered and loaded:

```
functions/
├── v1/
│   ├── _functionSchemaReference.memql   # Documentation (not loaded)
│   ├── queryActiveSpaces.memql          # Query function
│   ├── querySpaceParticipants.memql     # Query function
│   ├── mutationCreateSpace.memql        # Mutation function
│   └── builtin/                         # Built-in system functions
│       ├── concepts.json
│       └── memqlDocs.json
└── v2/
    └── queryNewFunction.memql
```

**query*.memql / mutation*.memql** — The MemQL expression with optional `arg()` references and `?.` conditional filters:

```memql
-- Returns participants in spaces.
-- Optional filters: spaceId, status, participantType
concept==v1:cognition:participant;
?.payload.spaceId==args.spaceId;
?.payload.status==args.status;
?.payload.participantType==args.participantType
```

**`args { ... }` block** — Inline argument schema defining function arguments.
For struct-form queries / mutations, the block lives inside the body;
for procedural functions it sits at file-top above the `func (...)`
declaration:

```memql
args {
  spaceId          string
  status           string  @enum("active", "idle", "left")
  participantType  string  @enum("human", "si")
}
```

Annotations: `@required` (non-optional), `@enum("a", "b", ...)`
(value set), `@default(<expr>)` (default when caller omits the
field), `@description("...")`.

Comments in function `.memql` files start with `--` (double dash) and are extracted as the function's description.

### Calling Functions

Functions accept an optional JSON object argument. Empty parentheses `()` are equivalent to `({})`:

```memql
-- No args (returns all matching records)
activeSpaces()
activeSpaces({})

-- With single filter
activeSpaces({"userId": "user-123"})

-- With multiple filters
spaceParticipants({"spaceId": "space-456", "status": "active"})

-- Combine with directives
sort(spaceUtterances({"spaceId": "s-1"}), "createdAt", "desc")
paginate(activeSpaces({"userId": "u-1"}), 10)

-- Use in shape templates
shape(
  activeSpaces({"status": "active"}),
  {
    "id": node("id"),
    "name": node("payload.name")
  }
)
```

### Argument References: args.fieldName

Use `args.fieldName` to reference argument values in function expressions:

```memql
payload.spaceId==args.spaceId
payload.status==args.status
createdBy==args.userId
```

Nested fields are supported with dot notation:

```memql
args.options.limit
args.filter.status
```

### Conditional Filters: ?.filter

Prefix a filter with `?.` (optional chaining style) to make it conditional — the filter is only applied if the referenced argument is provided:

```memql
concept==v1:cognition:participant;
?.payload.spaceId==args.spaceId;    -- Only filter if spaceId provided
?.payload.status==args.status        -- Only filter if status provided
```

This enables flexible calling patterns:

| Call | Behavior |
|------|----------|
| `spaceParticipants()` | Returns ALL participants |
| `spaceParticipants({"spaceId": "s-123"})` | Filters by spaceId only |
| `spaceParticipants({"status": "active"})` | Filters by status only |
| `spaceParticipants({"spaceId": "s-123", "status": "active"})` | Both filters applied |

### Argument Validation

Arguments are validated against the function's `args { ... }` schema at runtime:

- **Type validation**: Ensures argument types match (string, number, boolean, etc.)
- **Enum validation**: Rejects values not in the `@enum(...)` set
- **Required fields**: Returns error if `@required` arguments are missing
- **Additional properties**: Rejects unknown arguments

Example validation errors:

```json
{
  "error": "function 'activeSpaces': argument validation failed: status: expected string"
}
```

```json
{
  "error": "function 'spaceParticipants': argument validation failed: participantType: value must be one of \"human\", \"si\""
}
```

### Functions Can Use Specs

Functions can reference any registered spec:

```memql
-- functions/v1/queryContactableUsers.memql
-- Users with contact information

concept==v1:memql:backend:user;
payload.active==true;
hasUserContact
```

### Functions Can Call Functions

Functions can call other functions (circular dependencies are detected and rejected at load time):

```memql
-- functions/v1/queryPriorityConversations.memql
-- Active conversations that need attention

activeConversations();payload.priority=="high"
```

### Function Rules

- Filenames have NO prefix; the directory (`queries/`, `mutations/`)
  names the kind. The function declaration inside DOES carry the
  `query` / `mutation` prefix (`queryActiveSpaces`, `mutationCreateSpace`).
  The loader derives the expected name and rejects mismatches at startup.
- Function args are declared via an `args { ... }` block — body
  sub-block in struct form, file-top block in procedural form.
- Files starting with `_` are skipped (use for documentation)
- Circular dependencies are detected and rejected
- Functions are loaded after specs (so they can reference specs)

### Struct Form (canonical)

```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }

@enabled
@description("Get space participants")
query participant querySpaceParticipants {
  args {
    spaceId          string
    status           string  @enum("active", "idle", "left")
    participantType  string  @enum("human", "si")
  }
  filter  ?.payload.spaceId == args.spaceId;
          ?.payload.status == args.status;
          ?.payload.participantType == args.participantType
  shape   participantFull
}
```

Mutations follow the same shell with an `insert { ... }` or
`update { id: ..., ... }` block in place of `filter` / `shape`.
The concept binding lives in the signature (`query <Concept> <name>`
/ `mutation <Concept> <name>`); cross-file dependencies come in via
file-top `use <module>.{ ... }` imports. The legacy `@useConcept`
annotation family is retired and rejected at parse time.

### Procedural Form (internal post-rewrite shape)

The rewriter still emits a `func (Receiver) NAME(ctx any) (any,
error) { ctx.output = ...; return ctx, nil }` shape for the engine
parser, with `args.X` source-rewritten to `ctx.X`. **Don't author
that form.** Every receiver kind has a struct form -- queries
above, mutations next to them, logic with `body { ... ; return
<expr> }`, automations as `step` lists. The `(ctx any)` parameter
and `ctx.output =` boilerplate were retired in memql#302 / #303;
the canonical form returns the value directly.

### Built-in Functions

Built-in functions are system functions with Go executor logic, defined in `functions/v1/builtin/`:

```memql
concepts()           -- Returns metadata for all registered concepts
concepts("pattern")  -- Filter concepts by name pattern (case-insensitive)
memqlDocs()          -- Returns embedded MemQL documentation as nodes
validate()           -- Validates payload against concept schema (no persistence)
functions()          -- Returns minimal function list (name + description + kind)
tools()              -- Returns MCP-compatible tool definitions
help()               -- Returns full details for a function or tool by name
shapeTemplates()     -- Lists available shape templates, optionally filtered by concept
shapeHelp()          -- Returns full details for a shape template by name
contentId()          -- Predicts content-addressed ID for concept+payload (no insert)
previewInsert()      -- Validates payload, predicts ID, checks existence (no insert)
```

Built-in functions support all directives:

```memql
shape(concepts(), {"name": node("payload.concept")})
paginate(concepts(), 10)
concepts("crm")  -- Filter to concepts containing "crm" in their name
validate({"concept": "v1:crm:lead", "payload": {"email": "test@example.com"}})
help("myFunction")
```

### Workflow Integration

Functions can be triggered as steps within workflows:

```json
{
  "id": "processStale",
  "name": "Process Stale Conversations",
  "type": "function",
  "function": {
    "name": "staleConversations"
  }
}
```

See `workflows/v1/_workflowSchemaReference.jsonc` for full workflow documentation, including multi-step pipelines, conditional branching, forEach iteration, and parallel execution.

### Automation Context Expressions

Automations have access to special context expressions that are **only valid within automation definitions**. These expressions cannot be used in regular queries or functions.

| Expression | Context | Description |
|------------|---------|-------------|
| `event()` | Event-triggered automations | References the triggering event payload |
| `input()` | Step execution | References the input passed to a step |
| `item()` | forEach loops | References the current item being processed |
| `index()` | forEach loops | References the current iteration index |
| `step("stepId")` | After step execution | References the result of a previous step |
| `error()` | onError handlers | References the current error in error handlers |
| `error("message")` | Control flow | Throws an error with the specified message |

**Important:** These expressions are resolved at automation runtime via `$variable` string substitution (e.g., `$event.payload.userId`, `$error`). They cannot be evaluated in query context and will return an error if attempted.

### `.memql` automation bare-reference rules (strict)

In automation `.memql` files, MemQL supports a small amount of “bare reference” convenience, but it is **strictly limited**:

- **for-range loops**: the loop variable **must be named `item`**
  - Valid: `for item := range someStep.result { ... }`
  - Invalid: `for lead := range ... { ... }`
- **Bare dotted paths** (without a `$` prefix) are only auto-resolved when they start with:
  - **a known step ID** (e.g. `agent.result.0.name` where `agent := shape { ... }`), or
  - the reserved **`item.*`** root inside a for-range body (e.g. `item.id`, `item.payload.name`)
- If you need a **literal string containing dots**, quote it: `"foo.bar"`.

Note: `.memql` automation sources must not use JSON-style `$steps.*` references. Use bare step IDs (`stepId.result...`) instead.

Example usage in automation `.memql` files:
```memql
@enabled
@trigger(event="user.created")
func (Automation) welcomeUser() {
  // Access event payload
  notify(event().payload.email, "Welcome!")

  // Error handling in onError block uses error()
  // to access the error that occurred
}
```

#### `@filter` Annotation

The `@filter` annotation provides a standalone way to attach a filter predicate to an automation, as an alternative to embedding the filter directly in `@trigger`. This is useful when the filter expression is complex or when you want to separate trigger definition from filtering logic:

```memql
@enabled
@trigger(event="graph.node.created")
@filter(concept==v1:common:agent && payload.status=="active")
func (Automation) onActiveAgentCreated() {
  // Only fires for active agent nodes
  notify("admin", format("New active agent: %s", event().payload.name))
}
```

Without `@filter`, the equivalent would embed the filter in the trigger:
```memql
@trigger(event="graph.node.created", filter="concept==v1:common:agent;payload.status==\"active\"")
```

The `@filter` annotation accepts the same expression syntax as query filters, including `&&` as an alternative to `;` for AND conditions.

See `automations/v1/_automationSchemaReference.jsonc` for complete automation syntax documentation.

## Snippet Helpers

MemQL provides several building blocks for composing queries:

- **Concept paths** -- Insert `concept==v1:...` paths using the versioned concept hierarchy under `concepts/`.
- **Global specs** -- Reference specs from `specs/v1/*.json` in filter expressions. Inline spec declarations are also supported.
- **Relationship helpers** -- Wire `contains()`, `owns()`, `parentOf()`, etc. between two concept IDs to produce ready-to-run filter fragments.
- **SI assist** -- Build `si(“templateId”, {...}, “provider”, ttl)` calls using prompt templates from `prompts/v1/**/*.memql` and providers from `providers/v1/**/*.memql`. Cache TTL is clamped to `MEMQL_SI_CACHE_MAX_SECONDS` (currently 300s). The projection-only rules described above apply.

All query composition ultimately executes through the standard gRPC or WebSocket path, so every expression follows the same backend validation described in this guide.

## Subscriptions & Events

MemQL provides a real-time event system that delivers notifications for graph mutations, query execution, SI completions, and session lifecycle events. Clients subscribe over the existing bidirectional gRPC stream (or WebSocket bridge) and receive `EventNotification` messages as changes occur.

### Subscribing to Events

Send a `SubscribeMsg` over the stream to register for events:

```json
{
  "messageId": "sub-1",
  "subscribe": {
    "subscriptionId": "my-graph-events",
    "kind": 5,
    "filter": ""
  }
}
```

**Subscription Kinds:**

| Kind | Value | Default Pattern |
|------|-------|-----------------|
| `SUBSCRIPTION_KIND_TELEMETRY` | 1 | `telemetry.#` |
| `SUBSCRIPTION_KIND_MESSAGE` | 2 | `message.#` |
| `SUBSCRIPTION_KIND_QUERY_SPEC` | 3 | `query.#` |
| `SUBSCRIPTION_KIND_SI_STREAM` | 4 | `si.#` |
| `SUBSCRIPTION_KIND_GRAPH_EVENTS` | 5 | `graph.#` |
| `SUBSCRIPTION_KIND_ALL` | 6 | `#` (everything) |

The `filter` field accepts glob patterns for finer control:
- `*` matches exactly one segment (e.g., `graph.node.*` matches `graph.node.created`)
- `#` matches zero or more segments (e.g., `graph.#` matches all graph events)

### Available Event Topics

| Topic | Event Kind | Description |
|-------|------------|-------------|
| `graph.node.created.{concept}` | `NODE_CREATED` | Graph node inserted |
| `graph.node.deleted.{concept}` | `NODE_DELETED` | Graph node deleted |
| `graph.node.updated.{concept}` | `NODE_UPDATED` | Graph node updated |
| `query.executed` | `QUERY_EXECUTED` | Query completed |
| `si.completion.started` | `SI_COMPLETION_STARTED` | SI request began |
| `si.completion.finished` | `SI_COMPLETION_FINISHED` | SI request succeeded |
| `si.completion.error` | `SI_COMPLETION_ERROR` | SI request failed |
| `session.opened` | `SESSION_OPENED` | gRPC session started |
| `session.closed` | `SESSION_CLOSED` | gRPC session ended |

### Receiving Events

Events arrive as `EventNotification` payloads:

```json
{
  "messageId": "evt-abc123",
  "event": {
    "subscriptionId": "my-graph-events",
    "kind": 10,
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
```

### Unsubscribing

```json
{
  "messageId": "unsub-1",
  "unsubscribe": {
    "subscriptionId": "my-graph-events"
  }
}
```

Subscriptions are automatically cleaned up when the session closes.

> See [docs/events.md](../concepts/events.md) for the full architecture, payload schemas, and implementation details.

## Upcoming Features

These roadmap items are planned but not yet implemented. Update this section as features land or priorities change.

- **Streaming Responses** – add streaming execution so clients can start reading partial MemQL results before the query finishes (instead of waiting for a single HTTP response body).

## Keeping This Guide Up to Date

Any change to MemQL parsing, execution options, relationships, or mutations **must** be reflected here. Before merging query-language changes:

1. Update the relevant sections (syntax, operators, options, examples, roadmap).
2. Reference this document in pull requests so reviewers verify documentation parity.

## Runtime parser opt-in (#248 / epic #218)

The runtime grammar consumed by `engine.Execute(ctx, query string)` --
function invocations (`funcName({k: v, ...})`), filter expressions
(`concept==X; payload.Y==Z`), accessor references (`actor.X`,
`args.X`) -- is historically parsed by `component/memql/parser.go`,
which lives alongside the load-time parser in
`component/language/parser`. These two parsers were the duplication
behind memql#216 / #221 / #239, and the broader retirement is
tracked under epic #218 across three sequenced slices: #248 (add
opt-in path), #249 (flip default after soak), #250 (delete the old
parser).

For the soak window, the langparser-backed runtime path is
**opt-in** via:

```go
engine.UseLangparserRuntime(true)  // flag default is OFF
```

When ON, `engine.Execute(ctx, string)` routes the query through
`langparser.ParseExpression` + `ASTConverter` for the shapes it
covers (every shape SDK-generated builders produce + the
`concept==X` hand-written form). Two shapes still fall back to the
old parser:

- **Timestamp suffix** -- `concept==X @latest` /
  `concept==X @"2026-01-01T00:00:00Z"`. Handled post-parse on the
  memql path; the langparser path rejects it upfront via a
  sentinel so the fall-back is transparent.
- **Inline spec definitions** -- `name := expr`. Same.

Behavior is byte-identical for every supported shape -- guarded
by the cross-parser equivalence test
`TestParseViaLangparser_Equivalence` in
`component/memql/parser_langpath_test.go`. Add a row there if a
new caller adopts a shape the corpus doesn't cover.

The flag flips to ON-by-default in #249 after one to two release
cycles of dev/staging use with no parser-related regressions.
