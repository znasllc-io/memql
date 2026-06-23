---
title: MemQL
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL

> **Last Updated:** June 11, 2026

MemQL is the query and mutation language that powers the memory engine. It provides a deterministic, append-only interface for reading and writing concept-backed data stored in TimescaleDB. This document is the canonical reference for MemQL behavior. **Whenever the query language changes or new capabilities ship, update this guide alongside the code change.**

## When to Use MemQL

- Retrieving concept instances (agents, spaces, participants, etc.) with filterable JSON payloads.
- Traversing graph-like relationships (parent/child, contains, alias, owns, createdBy, interactsWith).
- Inserting new immutable records via mutations.

## Two Authoring Surfaces

MemQL has two surfaces, and this guide covers both:

1. **DSL constructs** — concepts, shapes, specs, traits, queries, mutations, logic, builtins, providers, prompts, tools, automations, and policies authored in `.memql` files under `dsl/<namespace>/`. These are loaded at engine startup and are the canonical way to define reusable behavior.
2. **Runtime query strings** — expressions passed to `engine.Execute(ctx, query)` (or over gRPC / the WebSocket bridge / MCP). These are plain filter expressions, calls to DSL-defined functions, `insert(...)` literals, and introspection meta-commands.

Anything that needs a projection (shape), a reusable predicate (spec), or AI involvement is defined in the DSL and *referenced* from runtime queries — the runtime string surface is intentionally small.

## Quick Start

### The DSL Tree

The DSL tree is flattened per construct: every namespace gets one directory under `dsl/<namespace>/`, and within it each construct kind is consolidated into a single `<construct>s.memql` file:

```
dsl/
├── cognition/
│   ├── concepts.memql      → v1:cognition:* concept schemas
│   ├── queries.memql       → named queries
│   ├── mutations.memql     → named mutations
│   ├── shapes.memql        → projections
│   ├── specs.memql         → boolean predicates
│   ├── logic.memql         → imperative procedures
│   ├── prompts.memql       → AI prompt templates
│   └── automations.memql   → event/schedule-triggered workflows
├── common/
│   ├── builtins.memql      → Go-backed builtin functions
│   ├── traits.memql        → cross-concept predicate scaffolds
│   └── shapes.memql
├── providers/
│   └── providers.memql     → AI provider records
└── policies/
    └── policies.memql      → AI provider-selection policies
```

Authoring reference skeletons live under `dsl/_reference/` (`_concept`, `_shape`, `_spec`, `_trait`, `_agent`); files whose path starts with `_` are never loaded.

When `MEMQL_DSL_PATH` is unset, the binary reads its baked-in embedded tree. Setting `MEMQL_DSL_PATH=/path/to/dsl-root` reads from disk instead, with per-namespace fallback to the embedded copy — useful for dev hacking, per-deploy patches, and test fixtures.

> **Retired layout.** The old versioned per-construct skeletons (`concepts/v1/...`, `specs/v1/...`, `functions/v1/...`, `shapes/v1/...`, `prompts/v1/...`, `providers/v1/...`, `automations/v1/...`) no longer exist. Constructs live in the flattened `dsl/<namespace>/<construct>s.memql` files described above.

### Basic Query

```
concept==v1:examples:world && payload.status=="active"
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
- `result.data` carries shaped output when the executed query carries a shape projection (i.e. a DSL-defined query with a `shape` directive). Omitted otherwise; when shaped, contains one element per root.
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

// Shaped query (a DSL query with a shape projection) - has data
{
  "result": {
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

- `result.bundle` – Contains the graph structure (`nodes`, `edges`, `rootIds`). For regular queries, the bundle is present with matched nodes; shaped queries omit it.
- `result.bundle.nodes` – Array of matched memory nodes. Omitted when empty.
- `result.bundle.edges` – Array of relationship edges. Omitted when no edges exist.
- `result.bundle.rootIds` – Array of root node IDs. Omitted when empty.
- `result.data` – Array of shaped payloads. Omitted when the query has no shape projection.
- `errors` – Array of error objects when failures occur. Omitted on success.

Consumers should check for the presence of `errors` before operating on the result. This keeps backend services, clients, and AI agents aligned on the same contract.

## Concepts

Concepts are schemas for nodes (like tables in SQL). Each concept is declared in struct form in `dsl/<namespace>/concepts.memql`. The full concept id is derived from the `@namespace` annotation plus the construct name: `@namespace("cognition")` + `concept space` → `v1:cognition:space`. Nested namespaces are colon-delimited (`@namespace("cognition:text")` + `concept chunk` → `v1:cognition:text:chunk`). Each segment must be a single lowercase alphanumeric word; invalid names cause the loader to reject the concept.

```memql
use cognition.concepts.{ space }
use agents.concepts.{ agent }

@version("1.0.0")
@namespace("cognition")
@description("Per-(spaceId, agentId) audio control override.")
concept audioOverride {
  spaceId  string  @required @description("v1:cognition:space.id this override is scoped to.")
  agentId  string  @required @description("v1:agents:agent.id this override targets.")
  mode     enum("always_on", "always_off", "mirror_user")  @required
  active   bool    @default("true") @description("Soft-revoke flag.")

  @relationship(type="parent", field="spaceId", target=space, direction="outgoing")
  @relationship(type="interactsWith", field="agentId", target=agent, direction="outgoing")
}
```

**Concept-level annotations:** `@version`, `@namespace`, `@description` (required so humans and AI systems can reason about the dataset).

**Field annotations:** `@required`, `@default("...")` (honored on insert), `@description("...")`. Field types include `string`, `bool`, `int`, `float`, `object`, `datetime`, `[]object`, and inline `enum("a", "b", ...)` value sets.

**Cross-concept references** are plain string fields holding the target's id (e.g. `spaceId string`), optionally paired with an `@relationship` annotation so the engine can traverse the edge.

> **Retired.** The per-concept `concept.json` metadata file (with `type`, `skipDeleted`, `defaultFilter`, `cacheTTLSeconds`, `relationships` keys) is gone — concept schema, description, and relationships are all declared in the `.memql` construct shown above.

### Relationships

The `@relationship` annotation inside a concept body declares a graph edge:

```memql
@relationship(type="parent", field="spaceId", target=space, direction="outgoing")
```

- `type` — how MemQL interprets the edge (see table below).
- `field` — the payload field used as the pointer.
- `target` — the target concept (a short name resolved through the file-top `use ...concepts.{ ... }` import).
- `direction` — `outgoing`, `incoming`, or `bidirectional`.

| Type | Description | Use When |
|------|-------------|----------|
| `parent` | This node belongs to a parent node | The field stores a single ID pointing to the parent |
| `contains` | This node contains other nodes | The field stores an array of IDs of contained nodes |
| `owns` | This node owns other nodes | Similar to contains, but implies exclusive ownership |
| `alias` | This node is an alias for another | The field stores the ID of the aliased node |
| `createdBy` | This node was created by another | The field stores the creator's ID |
| `interactsWith` | This node interacts with another | Generic association between nodes |

**Common Mistake: Confusing `parent` vs `child`**

When a concept has a field that points TO another concept (like `spaceId` pointing to a space), use `type="parent"`. The relationship type describes the direction from the current node's perspective.

**Rule of thumb:**
- If concept A has a field storing concept B's ID → A declares `type="parent"` pointing to B
- If concept A has an array of concept B IDs → A declares `type="contains"` pointing to B
- The `child` type is not directly declared; child relationships are inferred by querying `childOf()`, which finds nodes that have a `parent` relationship to the target

## Executing Queries

### From Go

```go
tree, err := memEngine.Execute(ctx, `
	sort(
		paginate(
			concept==v1:examples:world && payload.status=="active",
			50
		),
		"createdAt","desc"
	)
`)
```

### Via MCP `memql`

```json
{
  "query": "sort(paginate(contains(id==\"project-abc\") && payload.status==\"open\",25),\"payload.due_date\",\"asc\",\"createdAt\",\"desc\")"
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
    "query": "concept==v1:examples:world && payload.status==\"active\""
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

The bridge enforces a small per-connection window (four concurrent queries and a 5 MiB frame limit). Clients should reuse a single socket and listen for `queryResult.done` or `queryError` payloads.

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
| Filters              | Comparison expressions joined by `&&` (AND) and `\|\|` (OR), with `!` (NOT) and parentheses, using Go precedence. |
| Fields               | `concept`, `id`, `type`, `createdAt`, `createdBy`, or `payload.<path>`.                                         |
| Operators            | `==`, `!=`, `>`, `>=`, `<`, `<=`, `==nil`, `!=nil` (plus `in` in DSL filter clauses — see Operator Reference).  |
| Parentheses          | Group complex logic: `(concept==v1:assistant \|\| concept==v1:examples:persona) && payload.active==true`.       |
| Limit                | Use `paginate(<expr>, limit)` to request an explicit page size; omitting both `paginate` and `sort` caps the read at `MEMORY_ENGINE_DEFAULT_LIST_CAP` (default 50, the unmarked-list backstop). Continuation is via keyset cursors, not an offset skip. |

> **Retired operator forms.** The legacy `;`-as-AND and `,`-as-OR separators, the `has` membership operator, the `?.` optional-chain prefix, and the `??` null-coalescing operator are all retired (#977, and `??` in the struct-form Phase 4 work). The parser rejects them in authored DSL filters with migration-pointing errors. Use `&&` / `||`, `in`, `when(args.x) { ... }`, and `coalesce(a, b, ...)` respectively.

IDs are persisted as `<concept>:<raw-id>`. MemQL supports both full IDs and short IDs (when concept context is provided):

**Full ID (always works):**
- `id=="v1:examples:world:world-aurora"` – exact match on full storage ID

**Short ID (requires concept context):**
- `concept==v1:examples:world && id=="world-aurora"` – short ID resolved using concept from query

**Important:** Short IDs without concept context will return an error:
```
// This will ERROR - no concept context to resolve short ID
id=="world-aurora"

// This works - concept provides context for ID resolution
concept==v1:examples:world && id=="world-aurora"

// This also works - full ID doesn't need context
id=="v1:examples:world:world-aurora"
```

This design ensures predictable, exact-match behavior and avoids ambiguous results.

### Filters

Filters are core comparison expressions that narrow the set of returned nodes:

```text
concept==v1:assistant && payload.active==true
```

- Use `&&` for AND logic, `||` for OR logic, `!` for negation
- Group with parentheses: `(concept==A || concept==B) && payload.active==true`
- Field paths support dot notation for nested JSON: `payload.profile.name`

In **authored DSL filter clauses** (the `filter` line of a struct query) two more forms are available:

- **Membership**: the single `in` operator — `args.x in payload.list` or `payload.kind in ["a", "b"]`.
- **Arg-conditional predicates**: the `when(args.x) { <expr> }` guard — if `args.x` is absent at call time, the guarded block and its connective are dropped as if never written (unambiguous under `||`).

### Directives

Directives wrap filters and apply transformations or constraints. They must enclose the entire filter expression and form the outermost stack:

| Directive | Description | Example |
|-----------|-------------|---------|
| `asOf()` | Evaluate the expression at a specific timestamp | `asOf(concept==v1:assistant, "2025-01-01T00:00:00Z")` |
| `sort()` | Order results by field(s) | `sort(concept==v1:assistant, "createdAt", "desc")` |
| `paginate()` | Bound the result page size (LIMIT) | `paginate(concept==v1:assistant, 10)` |
| `withDepth()` | Limit traversal depth for relationships | `withDepth(parentOf(...), 2)` |

Directives can be nested: `sort(paginate(concept==v1:assistant, 10), "createdAt", "desc")` returns assistants sorted by creation date with pagination. Relationship functions (`parentOf()`, `childOf()`, `contains()`, ...) participate directly in the expression tree inside the directive stack.

> **Retired at the runtime surface (#250).** `shape(...)` and `select(...)` are no longer accepted in runtime query strings — declare the projection in the DSL instead (a `query` construct with a `shape` directive). The `@ "<RFC3339>"` / `@latest` timestamp suffix and inline spec definitions (`name := expr`) are likewise rejected in runtime strings; use `asOf(...)` and DSL-defined specs. The engine returns a typed `ErrUnsupportedQueryShape` with a migration hint for each of these.

### Dot-Path Field Access

MemQL supports deep dot-notation for accessing nested JSON fields:

```text
payload.profile.settings.theme == "dark"
```

Path segments follow JSON keys, including arrays via numeric indices when needed.

## Operator Reference

| Operator          | Example                                                                 | Notes                                                                                     |
|-------------------|-------------------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `==` / `!=`       | `payload.status=="open"`                                                | Direct equality / inequality.                                                             |
| `>` / `<`         | `payload.score>0.85`                                                    | Numeric comparisons (strings use lexical ordering).                                       |
| `>=` / `<=`       | `payload.attempts<=3`                                                   | Greater-than-or-equal / less-than-or-equal.                                               |
| `in`              | `payload.stage in ["lead", "qualified", "won"]`                         | Membership against a collection. Works with scalar fields against lists and scalars against array fields. DSL filter clauses only — in a runtime query string, rewrite as a disjunction (`x=="a" \|\| x=="b"`). |
| `==nil`           | `payload.metadata.notes==nil`                                           | Field absent or explicitly `null`. Apply to payload paths or intrinsic columns.           |
| `!=nil`           | `payload.metadata.tags!=nil`                                            | Field present with a non-null value.                                                      |

Combined example covering several operators:

```
concept==v1:lead &&
payload.source!=nil &&
payload.metadata.owner==nil &&
(payload.status=="new" || payload.status=="contacted")
```

The query above returns all leads in `new`/`contacted` state that already have a `source` value but still need an assigned `owner`.

### Array Field Support

In DSL filter clauses, the `in` operator works with both scalar and string-array fields:

**Scalar field:**
```memql
payload.status in ["active", "pending"]
```
Matches if `status` equals "active" OR "pending".

**Array field (automatic detection):**
```memql
"filters" in payload.topics
```
Matches if the `topics` array contains "filters". For example, a module with `topics: ["filters", "limits", "sorting"]` matches.

> **Note:** Array support is limited to string arrays. Numeric or boolean array matching is not supported. The legacy reverse-direction `has` operator is retired; write `<scalar> in <collection>`.

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
`contains(id=="project-123") && payload.status=="open" && payload.priority<=2`

**Note:** Relationship pointer fields are optional — if a node has a null or missing pointer field, it is silently skipped during traversal rather than causing an error.

### Sorting

Use `sort(<expr>, "<field>", "<direction>?", ...)` to order results. The function:

- Accepts any MemQL expression as the first argument.
- Requires at least one string literal field name; directions are optional (`"asc"` or `"desc"`, defaulting to `"desc"`).
- Allows multiple field/direction pairs for deterministic tie-breaking.
- Must wrap the entire query expression (i.e., `sort(...)` should be the outermost call).
- Supported fields: `id`, `concept`, `createdAt`, `createdBy`, `type`, and `payload.<path>`.
- Limits and offsets always apply **after** sorting. Sorting on payload properties may cause the engine to fetch up to `MEMORY_ENGINE_MAX_WINDOW` rows to guarantee correctness.

Example:

```
sort(
  paginate(childOf(concept==v1:examples:world && id=="v1:examples:world:world-aurora"), 100),
  "createdAt","desc",
  "id","asc"
)
```

### Pagination

`paginate(<expr>, limit)` bounds the result page size. The function:

- Requires a single integer argument (limit) greater than zero — the page size.
- Can be combined with other helpers (e.g., `sort(paginate(...), ...)`).
- **Continuation is keyset, not offset.** Offset pagination was removed
  (epic 5, 5.13 / memql#1993): it was O(offset) and drifted under
  concurrent inserts. To fetch the next page, pass the `nextCursor` from
  the prior response back as the query cursor; the engine pushes a
  `WHERE (createdAt, id) <keyset> (?, ?)` predicate and continues from the
  encoded position. The first page is bounded by a plain SQL `LIMIT`.

**Default-cap backstop (memql#1965).** A query that arrives with NO
explicit window — neither `paginate` nor `sort` — is treated as an
unmarked list read and capped at `MEMORY_ENGINE_DEFAULT_LIST_CAP`
(default **50**), not `MEMORY_ENGINE_MAX_RESULTS`. This bounds the blast
radius of an accidental full-table read. A query that paginates or sorts
states its own window (capped at `MEMORY_ENGINE_MAX_WINDOW`); a query
marked `@unbounded("reason")` is rewritten to an explicit wide paginate
and bypasses the 50-cap. See the [pagination authoring rule](authoring-rules.md#23-list-returning-queries-must-declare-their-bound).

```
paginate(concept==v1:examples:module && payload.worldId=="v1:examples:world:world-aurora", 200)
```

### Temporal Snapshots

`asOf(<expr>, "<timestamp>")` evaluates a query using a consistent historical snapshot. Supply an RFC3339/RFC3339Nano timestamp string:

```
asOf(concept==v1:assistant && payload.active==true, "2025-11-01T00:00:00Z")
```

### Depth Overrides

`withDepth(<expr>, depth)` customizes relationship traversal depth. Depth must be a positive integer:

```
withDepth(parentOf(concept==v1:examples:quest && id=="v1:examples:quest:quest-nodes"), 3)
```

## Result Shaping (Shapes)

Shapes are reusable field-projection templates, declared in struct form in `dsl/<namespace>/shapes.memql`. Queries reference them via the `shape <name>` directive; the engine projects each matched row through the template and returns the result in `result.data`.

Each shape declares its **kind** (where its fields come from) via `@row` and/or `@actor`. At least one is required; both is allowed (mixed shape). The body is a list of field paths plus optional `include` statements — shapes have no inputs and no return.

**Row shapes** project a concept's payload + row intrinsics. The bound concept is named by the **signature** `shape <Concept> <name>` (the short name resolves through the file-top `use ...concepts.{ ... }` import):

```memql
use cognition.concepts.{ audioOverride }

@row
@description("Per-(space, agent) audio override projection")
shape audioOverride audioOverrideFull {
  row.id
  payload.spaceId
  payload.agentId
  payload.mode
  payload.active
  row.createdAt
}
```

Body path translations: `row.X` → row intrinsic (`id`, `createdAt`, `createdBy`, etc.); `payload.X` → payload field. Each path becomes a template entry keyed by the path's terminal segment.

**Actor shapes** project the engine envelope (the authenticated actor + engine timestamp + allow-listed config). They carry no signature concept. Closed field set: `actor.userId` / `actor.role` / `actor.identityId` / `actor.isClusterOwner` / `actor.now` / `actor.config.<allow-listed-key>`:

```memql
@actor
@description("Actor envelope projection: authenticated actor, role, and now.")
shape actorEnvelope {
  actor.userId
  actor.role
  actor.identityId
  actor.isClusterOwner
  actor.now
}
```

**Trait shapes** are `@row` shapes signature-bound to a generic trait concept — scaffolds for cross-concept predicates (`activeRowTrait`, `statusRowTrait`, `deletedRowTrait`, `archivedRowTrait`, etc. in `dsl/common/shapes.memql`).

**Composition.** A shape can `include` another shape; transitive inclusion is supported, cycles and field collisions are errors. Pure aliasing is a shape whose body is a single `include` line:

```memql
@row
shape space spaceCardAlias {
  include spaceCard
}
```

> **Retired forms.** Receiver-function shapes (`func (Shape) ...`), the `@template` annotation, `node("...")`-wrapped template bodies, and the `@concepts("v1:...")` binding annotation are all retired and rejected at parse time. The concept binding lives in the signature; the body is a plain path list. Runtime `shape(<expr>, {...})` / `select(<expr>, ...)` query strings are retired too (#250) — wanting a projection means defining (or reusing) a DSL query with a `shape` directive.

To discover available shapes at runtime, use the `shapeTemplates()` and `shapeHelp("name")` introspection commands (see [Introspection](#introspection-functions)).

## AI: Providers, Policies, Prompts, and `si()`

MemQL's AI integration is intentionally scoped: language models can only influence the *output* of explicitly AI-aware constructs (prompt calls in logic bodies); filters, sorts, pagination, and mutations remain deterministic.

### Providers

AI provider configurations (OpenAI and Anthropic — the only supported vendors) live in `dsl/providers/providers.memql`. Struct form, mirrors concepts / shapes / tools:

```memql
@extends("openai")
@model("gpt-5.4-mini")
@description("OpenAI GPT-5.4 Mini - balanced cost/latency chat (non-streaming)")
provider chat54Mini {
  params {
    contextWindow        128000
    maxCompletionTokens  16384
  }
}
```

Base providers (vendor-level auth + type) use the same form:

```memql
@base
@type("Anthropic")
provider anthropic {
  auth {
    apiKey  env("MEMQL_AI_ANTHROPIC_API_KEY")
  }
}
```

The legacy `func (Provider) name { ... }` form is retired; the parser rejects it with a migration hint.

**Provider types** (registered in `component/memql/ai_providers.go`) include `OpenAI` / `OpenAIChat` (chat completions), `OpenAIStream` (streaming chat), `OpenAITTS` (text-to-speech), and `Anthropic` (Claude chat / vision).

**Lifecycle annotations (`@enabled` / `@disabled`).** Providers accept the same lifecycle flags as functions / builtins / prompts / specs / seeds. `@enabled` is the explicit-on default (a no-op). `@disabled` skips the provider at load — it is **not registered and no auth resolution is attempted** — while staying in the tree for a future re-enable. `@disabled` on a `@base` **propagates**: every child that `@extends` it is skipped too. Dependents degrade gracefully — a policy whose `@primary` is disabled routes via its `@fallback`; a prompt whose `@defaultProvider` is disabled falls back to the default.

> **Semantics of `@disabled`** (shared across every construct that takes it): the construct is **not loaded/active at runtime right now**. It does NOT mean deprecated, abandoned, or exempt from maintenance / refactors / conformance — it is a reversible on/off switch. ("Deprecated / abandoned" is a separate axis carried by `@deprecated`.)

### Policies

The live `policy` construct is an **AI provider-selection record**: empty-bodied, annotated with `@primary` / `@fallback` / `@maxLatencyMs` / `@preferredRole`, consolidated in `dsl/policies/policies.memql` and consumed by the AI Router to pick chat/voice/embedding providers:

```memql
@primary("streamClaudeSonnet")
@fallback("stream54Pro")
@fallback("streamClaudeHaiku")
@maxLatencyMs(60000)
@description("Default chat policy for non-operator agents.")
policy balancedChat { }
```

> **Decision-policy tier — RETIRED (#984).** The cross-cutting decision model (`func (Policy)` constructs, `@tier` / `@audited` annotations, `engine.EvaluatePolicy`) is fully removed. Caller-based boolean checks (admin / owner / permission) are authored as **context-specs** and called via `spec("name")`; the only live `policy` surface is provider selection.

### Prompts

AI prompt templates with input schemas and default providers live in `dsl/<namespace>/prompts.memql`. Struct form — the body is a bare input-schema field list:

```memql
@defaultProvider("chat54Mini")
@templateFile("prompts/cognitionPrediction.tmpl")
@description("Predict conversation trajectory for proactive cognition behavior")
prompt cognitionPrediction {
  transcript  []object  @required @description("Recent transcript entries with speakerName, speakerType, text")
  agents      []object  @required @description("Available AI agents with name, role, and domains")
}
```

Logic prompts (routing / suggest / classification) use the structured-output path (`ChatStructuredProvider.CallChatStructured`); prose prompts (agent replies to users) use regular chat.

Two legacy forms are retired (both rejected at parse time):
- `func (Prompt) name(ctx any) { ... }` — receiver-function wrapping.
- `@input { ... }` — body-level wrapper around the field list.

### Calling `si()`

`si("<promptName>", <dataObject>)` performs a blocking LLM call against a registered prompt. It is called from **logic bodies** (and from Go integrations); it is not part of the runtime query-string grammar:

```memql
// Inside a logic body
siResponse := if existingResponse.Empty() {
  si(args.event.payload.promptTemplateId, args.event.payload.promptData)
}
```

1. **Prompt name** — the name of a registered `prompt` construct.
2. **Data object** — key-value map matched against the prompt's input schema. When omitted, an empty object is used.

If no provider override applies, the engine uses the prompt's `@defaultProvider`, then falls back to `MEMQL_DEFAULT_PROVIDER`. For agent-orchestrated, tool-using, planner-tracked work, use the `agent(...)` builtin instead — `si()` is for direct, blocking prompt calls.

Prompt templates are rendered with Go's `text/template` package. When embedding structured data in a template that expects JSON, serialize it first (pass JSON-encoded strings in the data object) rather than passing raw maps, which would render in Go's internal map format.

### AI Cache

- `MEMQL_SI_CACHE_DEFAULT_ENABLED` (`true`/`false`) toggles whether `si()` calls cache results when no explicit TTL is provided.
- `MEMQL_SI_CACHE_MAX_SECONDS` caps any AI cache entry (and doubles as the default TTL when caching is enabled). The engine clamps this to **≤ 300 seconds (5 minutes)**.

The AI cache hashes `{templateId, provider, renderedPrompt}` as the cache key. When caching is enabled, a successful provider response is reused until its TTL expires — preventing duplicate LLM calls for identical prompts.

## Mutations

MemQL follows an **append-only, immutable data model**. Records are never updated in place; instead, new versions are inserted. There are two write surfaces: the runtime `insert()` literal and DSL-defined named mutations.

### Runtime `insert()`

```
insert(
  "v1:examples:world",
  id="world-nebula",
  payload={
    "title":"Nebula Grid",
    "slug":"nebula-grid",
    "status":"active"
  }
)
```

Rules:

1. One `insert()` per statement; no mixing reads and writes.
2. Payload must match the concept schema (validated automatically).
3. Relationship hints (`parent`, `aliasOf`) rewrite the payload before persistence.
4. Inserts return the created node inside `result.bundle` (single node, empty edge list, and `rootIds` containing the inserted ID).
5. Stored identifiers always take the form `<concept>:<id>`; providing a bare `id` argument automatically applies the prefix.
6. The `id` argument must be a string literal or omitted — helper calls like `id=uuid()` are syntax errors. Pre-generate IDs and pass them as strings.

### Content-Addressed IDs

When no `id` is provided, MemQL generates a **deterministic content-addressed ID** derived from the concept name and payload using SHA256. This provides:

- **Idempotent inserts**: The same payload always produces the same ID, preventing accidental duplicates
- **Reproducibility**: Given a payload, you can predict or verify its ID (see `contentId()` / `previewInsert()`)
- **Natural deduplication**: Identical content maps to the same record

```
-- No id specified: ID is derived from concept + payload
insert("v1:lead", payload={"name": "John", "email": "john@example.com"})
-- Returns: v1:lead:a3f8b2c1d4e5f6... (64-char hex hash)

-- Running the same insert again produces the same ID
-- This creates a new version of the same record, not a duplicate
```

The generated ID is a 64-character hexadecimal SHA256 hash. An optional server-side salt (configured via the content-ID salt env var, `*_CONTENTID_SALT`) can be added for deployment isolation. Explicit `id` parameters always take precedence over content-addressed derivation.

**Identical payloads create versions, not new records.** Inserting the same payload without an explicit ID creates a new *version* of the existing record:

| Goal | Solution |
|------|----------|
| Create multiple independent records | Use unique values in the payload (different names, UUIDs, etc.) |
| Update an existing record | Insert with the same payload/ID (this is the intended pattern) |
| Ensure uniqueness | Pass an explicit `id` parameter |

### Versioning via Insert (The "Update" Pattern)

There is no in-place update by design. To change a record's state:

1. **Insert a new version** with the same ID but updated payload fields
2. **Query to retrieve the most recent version** of each record (queries always return current state)
3. **Full history is preserved** and queryable via `asOf()`

```
-- Original lead (unclassified)
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com"})

-- "Update" by inserting a new version with the same ID
insert("v1:lead", id="lead-123", payload={"name": "John", "email": "john@example.com", "classification": "hot"})

-- Query current state (returns the classified version)
concept==v1:lead && id=="lead-123"

-- Query unclassified leads (current version missing classification field)
concept==v1:lead && payload.classification==nil
```

**Soft deletes:** to "delete" a record, insert a version with `active: false`.

**Why append-only?**

| Benefit | Description |
|---------|-------------|
| **Audit trail** | Complete history of all changes with timestamps and actors |
| **Time travel** | Query data as it existed at any point: `asOf(expr, "2025-01-01T00:00:00Z")` |
| **No data loss** | Records are never destroyed; "deletes" are soft (set `active: false`) |
| **Determinism** | Same query + same timestamp = identical results, always |

### DSL Mutations (Struct Form)

Named mutations live in `dsl/<namespace>/mutations.memql`. The concept binding lives in the signature (`mutation <Concept> <name>`); the body carries an `args { ... }` block plus exactly one `insert { ... }` **or** `update { ... }` block (one write per body):

```memql
use cognition.concepts.{ space }

@enabled
@description("Insert a new version of a space record (typically used to archive a space).")
mutation space mutationArchiveSpace {
  args {
    spaceId  string  @required
    payload  object  @required
  }
  insert {
    id: args.spaceId
    ownerUserId: actor.userId
    args.payload
  }
}
```

- `insert { ... }` writes a new row of the signature concept; `update { id: ..., ... }` is the partial-update counterpart for read-merge-validate-write flows.
- A bare `args.X` entry spreads the field under its own name; `name: <expr>` assigns explicitly.
- Engine-provided names are available in the body: `now` (RFC3339 timestamp captured at eval start), `actor.userId` / `actor.role` / `actor.identityId` / `actor.isClusterOwner`, `partition`, and allow-listed `config.X`.
- Helper calls like `concat(...)`, `hash(...)`, `canonicalId(args.x, concept)`, `timestamp()`, and `coalesce(a, b, ...)` are available for computed fields.

## Specs

Specs are atomic boolean predicates, declared in struct form in `dsl/<namespace>/specs.memql`. The body is a **single boolean expression**. The engine classifies a spec by walking its field references and picks the evaluation strategy:

- **Row-specs** reference `payload.X` and/or row intrinsics (`id`, `concept`, `type`, `createdAt`, `createdBy`, `schema`). They compile into a SQL `WHERE` fragment and push down to the database for filtering.
- **Context-specs** reference `actor.X` only (e.g. `actor.role`, `actor.isClusterOwner`). They evaluate in-process against the auth-context envelope; call them via `spec("name")` for actor-based checks like "is admin."

Bodies that mix both flavors are rejected at load time.

```memql
use cognition.concepts.{ participant }

@description("Matches participants with human participantType")
spec specIsHumanParticipant {
  payload.participantType == "human"
}

@enabled
@description("Actor holds an admin role")
@shape("actorEnvelope")
spec requiresAdmin {
  actor.role == "admin"
}
```

**`@shape("name")` is optional** on a spec — the eval strategy is derived from the spec body's field references, not the shape. When present it documents/pins the shape the predicate reads: concept-specific specs pin a concept-bound `@row` shape; actor-side specs an `@actor` shape; cross-concept specs a **trait shape**.

### Traits

Traits are cross-concept predicate scaffolds declared in `dsl/common/traits.memql`:

```memql
@enabled
@description("Matches records with active==true field")
trait traitIsActiveRecord {
  payload.active==true
}
```

When a trait covers a predicate (e.g. `traitIsActiveRecord` for `payload.active==true`), **using the trait is mandatory** in authored query filters — inline `payload.active==true` / `payload.deleted==false` are rejected by the conformance test (`dsl/conformance_test.go`).

### Using Specs and Traits in Filters

Specs and traits are referenced by bare name inside DSL query filter clauses:

```memql
filter  payload.spaceId==args.spaceId && specIsHumanParticipant && traitIsActiveRecord
```

During load the engine resolves every spec reference into the underlying expression tree, so the resulting query plan behaves exactly as if the spec contents were written inline. Spec dependencies are resolved at load; cycles and duplicates are rejected.

> **Retired forms.** The receiver-function spec (`func (Spec) name(ctx any) bool { return <expr> }`), the JSON spec format (`specs/v1/*.json` documents with an `expression` string), and runtime inline spec definitions (`name := expr` inside a query string) are all retired and rejected. Author the predicate as a struct spec and reference it by name. Specs carry no `ctx`, no `return`, no parameter — the body is the bare boolean expression.

## Queries (Named Functions)

Named queries are reusable, parameterized reads, declared in struct form in `dsl/<namespace>/queries.memql`. The signature `query <Concept> <name>` binds the concept; cross-file dependencies (concepts, shapes, traits, specs) come in via file-top `use` imports:

```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord }

@enabled
@description("Get space participants")
query participant querySpaceParticipants {
  args {
    spaceId          string
    status           string  @enum("active", "idle", "left")
    participantType  string  @enum("human", "si")
  }
  filter  when(args.spaceId) { payload.spaceId==args.spaceId } &&
          when(args.status) { payload.status==args.status } &&
          when(args.participantType) { payload.participantType==args.participantType } &&
          traitIsActiveRecord
  shape   participantFull
}
```

Body directives: `filter` (the predicate), `shape` (named projection), and optional `sort "field", "dir"` / `paginate N` lines:

```memql
@enabled
@description("Returns the latest space-context row for a given spaceId")
query context queryLatestSpaceContextForSpace {
  args {
    spaceId  string  @required
  }
  filter  payload.spaceId==args.spaceId
  sort    "createdAt", "desc"
  paginate 1
  shape   spaceContextFull
}
```

### Imports

Every construct another file pulls into local scope is declared via a dotted-path import at file top:

```memql
use cognition.concepts.{ participant, space }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord, traitIsNotDeleted }
```

The dotted path maps to a file on disk (`cognition.concepts` → `dsl/cognition/concepts.memql`); the brace list names the constructs imported into local scope.

> **Retired.** The `@use*` annotation family (`@useConcept`, `@useShape`, `@useQuery`, `@useMutation`, `@useLogic`, `@useBuiltin`, ...) is retired and rejected at parse time with a migration-pointing error. The concept binding lives in the construct signature; everything else comes in through `use` imports.

### Argument Declaration and Resolution

`args { ... }` field syntax: `<name> <type> [@required] [@enum("a", "b", ...)] [@description("...")] [@maxLength(N)] [@pattern("re")]`. Omitting `@required` makes the field optional.

> **`@default` is not valid on an args field** (rejected at load, #991). Apply a default in the body via `coalesce(args.X, <default>)`, or use a concept-field `@default` (those ARE honored on insert).

How names resolve inside a body:

| Name pattern | Source | Available in |
|---|---|---|
| `args.X` | Caller-passed arg declared in `args { ... }` | every body |
| `actor.X` | Resolved auth context (`userId`, `role`, `identityId`, `isClusterOwner`) | every body |
| `now` | RFC3339 timestamp captured at eval start | every body |
| `partition` | Active partition for this call | every body |
| `config.X` | Allow-listed config | every body |
| `payload.X`, `id`, `concept`, `type`, `createdAt`, `createdBy`, `schema` | Row fields / intrinsics | queries' `filter` + `shape` only (SQL pushdown) |

**Reserved engine names.** `now`, `actor`, `partition`, `config`, `trace` are reserved as top-level identifiers. An `args` field that collides with one of these names is rejected at load time.

> **Retired.** The `ctx` envelope is gone from the author surface — no `ctx.input.X`, no `ctx.X` shorthand, no `ctx.output =` assignment. Authors read caller args as `args.X` and return values directly.

### Calling Functions at Runtime

Named queries and mutations are invoked from runtime query strings as function calls. They accept an optional JSON object argument; empty parentheses `()` are equivalent to `({})`:

```memql
-- No args (returns all matching records)
queryActiveSpaces()

-- With filters
querySpaceParticipants({"spaceId": "space-456", "status": "active"})

-- Combine with directives
sort(querySpaceUtterances({"spaceId": "s-1"}), "createdAt", "desc")
paginate(queryActiveSpaces({"userId": "u-1"}), 10)
```

The parentheses make functions immediately recognizable: `traitIsActiveRecord` (no parens) is a spec/trait reference inside a DSL filter; `queryActiveSpaces()` (parens) is a function call.

### Argument Validation

Arguments are validated against the function's `args { ... }` schema at runtime:

- **Type validation**: Ensures argument types match (string, number, boolean, etc.)
- **Enum validation**: Rejects values not in the `@enum(...)` set
- **Required fields**: Returns error if `@required` arguments are missing
- **Additional properties**: Rejects unknown arguments

Example validation errors:

```json
{
  "error": "function 'queryActiveSpaces': argument validation failed: status: expected string"
}
```

```json
{
  "error": "function 'querySpaceParticipants': argument validation failed: participantType: value must be one of \"human\", \"si\""
}
```

### Function Rules

- One consolidated `queries.memql` / `mutations.memql` file per namespace; the declaration name carries the `query` / `mutation` prefix (`queryActiveSpaces`, `mutationCreateSpace`).
- Functions can reference specs and traits (loaded after specs) and call other functions; circular dependencies are detected and rejected at load time.
- Comments use `//`; construct descriptions come from `@description("...")`.

### Procedural Form (internal post-rewrite shape)

The struct-form rewriter expands every author-side construct to a `func (Receiver) NAME(ctx any) (any, error)` shape for the engine's parser; the `ctx` parameter name is a placeholder identifier only. **Don't author that form** — every receiver kind has a struct form: queries and mutations as above, logic with `body { ...; return <expr> }`, automations as `step` lists. Receiver-function constructs in authored files are rejected at parse time with migration hints.

## Logic

Logic constructs are imperative procedures called from automation steps (or other logic). `args { ... }` declares inputs; `body { ... }` is a sequence of named statements ending in `return <expr>`:

```memql
use common.builtins.{ ensureDailySpaceForUser }

@enabled
@description("On user creation, ensure today's daily space exists.")
logic logicProvisionDailySpaceOnUserCreate {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser({ userId: args.event.payload.id })
  }
}
```

Multi-statement bodies use `name := <call>` steps followed by a trailing `return <expr>`. Step results support iteration and result accessors, and steps can be gated with `if`:

```memql
@description("Daily sweep that hard-deletes archived spaces whose retention window has elapsed.")
logic logicPurgeExpiredArchivedSpaces {
  args {
    event object @required
  }
  body {
    expired := queryExpiredArchivedSpaces({ now: timestamp() })

    for item := range expired.Nodes() {
      deleteStep := mutationDeleteSpaceNow({
        spaceId: item.id,
        payload: {
          item.payload.name,
          status: "archived",
          active: false,
          deleted: true
        }
      })
    }

    return expired.Len()
  }
}
```

**Result accessors.** A query-step result exposes `X.Nodes()` (the matched node slice), `X.Empty()` (true when nothing matched), and `X.Len()` (match count). Within a `for item := range X.Nodes()` loop, the loop variable **must be named `item`**; `item.id` and `item.payload.<field>` reach the current node.

**Conditional steps** use `if <cond> { <call> }`:

```memql
siResponse := if existingResponse.Empty() {
  si(args.event.payload.promptTemplateId, args.event.payload.promptData)
}
```

Logic functions return via the trailing `return <expr>` — there is no `ctx.output = ...`. The `LogicRunner` walks intermediate steps in dependency order through the same step registry the automation scheduler uses, then evaluates the trailing `return <expr>` as the function's return value.

## Builtins

Builtins wrap Go integrations behind a declarative schema, so they look like regular DSL function calls. Struct form with an `@executor` annotation naming the Go-side capability; the body is the input-schema field list:

```memql
@enabled
@executor("integration.auth.checkPermission")
@description("Check if the current authenticated user has a specific role. Returns boolean result.")
builtin authCheckPermission {
  role  string  @required
}
```

The introspection commands (`concepts`, `memqlDocs`, `functions`, `help`, `contentId`, `previewInsert`, ...) are themselves builtins declared in `dsl/common/builtins.memql` with engine-internal executors. Builtins are callable from runtime query strings and from logic bodies.

## Tools

Tools are AI-callable tool definitions — the AI-facing surface of queries, mutations, and builtins. Struct form; the body is a list of input-schema fields with types and annotations (`@required`, `@default`, `@enum`, `@description`); `@handler` binds the tool to its backing operation and `@executionTime` hints latency:

```memql
@enabled
@description("Search for users")
@handler(type="query", query="concept==v1:memql:backend:user")
@executionTime("fast")
tool searchUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Maximum number of results to return")
}
```

The tool loop binds tool-call args to handler args and forwards. The legacy `func (Tool)` form is retired; the parser rejects it with a migration hint.

## Automations

Automations are event- or schedule-triggered workflows declared in `dsl/<namespace>/automations.memql`. The body is a list of `step` blocks; steps call logic, named mutations/queries, or builtins:

```memql
use cognition.logic.{ logicBootstrapSession }

@enabled
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
@description("Auto-creates a session when a participant joins a space")
automation bootstrapSession {
  step run {
    logic bootstrapSession { event: event }
  }
}
```

### Triggers

| Form | Example | Fires |
|------|---------|-------|
| Node event | `@trigger(event="node.created", concept="v1:cognition:space", partition="*")` | When a node of the concept is created (`node.updated` / `node.deleted` likewise) |
| Custom topic | `@trigger(event="cognition.response.requested")` | When the named application event is published |
| Lifecycle | `@trigger(event="system.startup")` / `@trigger(event="system.shutdown")` | At engine start/stop |
| Schedule | `@trigger(schedule="0 0 2 * * *")` | On a 6-field cron schedule (seconds first) |

The triggering event is bound as the `event` value in step bodies and is conventionally forwarded to logic as `{ event: event }`; inside the logic body it is read as `args.event.payload.<field>`, `args.event.topic`, etc.

### `@filter` Annotation

`@filter` attaches a filter predicate to an automation as an alternative to embedding it in the trigger — useful when the expression is complex:

```memql
@enabled
@trigger(event="node.created", concept="v1:cognition:space", partition="*")
@filter(payload.active==true)
@description("On space creation, joins the creator's assistant plus any specialist agents.")
automation autoJoinSI {
  step run {
    logic autoJoinSI { event: event }
  }
}
```

The `@filter` annotation accepts the same expression syntax as query filter clauses.

### Parallel Fan-Out Step

A step body can be a `parallel { ... }` block that runs several branch steps CONCURRENTLY (the executor fans the branches out on goroutines):

```memql
automation gather {
  step layer0 {
    parallel {
      wait: "all"        // all | any | none (default: all)
      failFast: true      // default: false
      branches: [
        step sales   { automation fetchSales { } },
        step support { automation fetchSupport { } }
      ]
    }
  }

  step merge {
    if steps.layer0.status == "success" {
      automation mergeReports { }
    }
  }
}
```

Rules:

- `branches` is required and non-empty. Each entry is a full `step` block — any step body works (automation / logic / query / mutation calls), including per-branch `if <cond> { ... }` gating and nested `parallel` blocks.
- Branch ids must be unique within one parallel; at runtime they surface as `<parent>.<branch>` (e.g. `layer0.sales`).
- `wait` picks the join strategy: `"all"` (default) waits for every branch, `"any"` returns on the first success, `"none"` is fire-and-forget.
- `failFast: true` cancels the remaining branches when one fails and fails the parallel step — combine with a downstream `if steps.<id>.status == "success"` gate to skip dependents when any branch fails. Without `failFast`, branch errors do not fail the step under `wait: "all"`.
- The parallel step itself can be gated: `step layerN { if <cond> { parallel { ... } } }`.

This is also the shape the phased-authoring headline synthesizer emits for a dependency layer with 2+ independent phases (within-layer concurrency, memql#1164 / memql#1368); single-phase layers stay plain sequential steps.

### Bare-Reference Rules (strict)

In automation and logic bodies, MemQL supports a small amount of "bare reference" convenience, but it is **strictly limited**:

- **for-range loops**: the loop variable **must be named `item`**
  - Valid: `for item := range someStep.Nodes() { ... }`
  - Invalid: `for lead := range ... { ... }`
- **Bare dotted paths** are only auto-resolved when they start with a **known step ID** (e.g. `expired.Nodes()` where `expired := ...`), the reserved **`item.*`** root inside a for-range body, or a reserved engine name (`event`, `args`, `actor`, `now`, `timestamp`, ...).
- If you need a **literal string containing dots**, quote it: `"foo.bar"`.

Reserved reference names in condition strings (never step names): `event`, `item`, `index`, `arg`, `var`, `input`, `error`, `step`, `field`, `timestamp`, `now`, `true`, `false`, `nil`, `null`.

> **Retired.** The receiver form `func (Automation) name() { ... }` is rejected at parse time — automations are authored as the `automation <name> { step ... }` struct form shown above. JSON workflow definitions (`workflows/v1/**`) and the `$var.NAME` variable-substitution machinery they used are gone entirely.

## Introspection Functions

MemQL exposes the documentation and concept catalog directly through the expression language so clients (human or AI) can bootstrap themselves dynamically.

These introspection calls are builtins declared in `dsl/common/builtins.memql`. Their names, aliases, and argument contracts are loaded into the function registry at startup; a meta-command dispatch shim recognizes them upfront (before either parser), so they work uniformly across all execution paths.

### `memqlDocs()`

Returns the embedded MemQL guide as a synthetic memory node:

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
    }
  }
}
```

Use this when an agent needs to refresh its understanding of the language without shipping the guide alongside every request.

### `concepts()` / `concepts("pattern")`

Lists the concepts (and their schemas) available in the current deployment. An optional pattern argument filters concepts by name (case-insensitive substring match):

```
// List all concepts
concepts()

// Filter concepts by pattern (e.g., all CRM-related concepts)
concepts("crm")

// Combine with paginate() to page through long lists
paginate(ids(concepts()), 5, 0)
```

Each child node includes:

- `metadata`: normalized view of the concept declaration (name, description, relationships).
- `schemas`: JSON Schema objects derived from the concept's field declarations.

Because the result set is synthetic, wrap the call with `paginate()` whenever you expect many concepts.

### `validate()`

Validates a payload against a concept's schema without persisting anything. Useful for agents to check payloads before attempting an insert:

```
validate({"concept": "v1:crm:lead", "payload": {"email": "test@example.com", "name": "John"}})
```

Returns a validation result node with:

- `valid`: boolean indicating if validation passed
- `errors`: array of validation error objects (empty if valid)
- `required`: sorted array of required field names from the schema
- `provided`: sorted array of field names present in the payload
- `schema`: summary including `$id` and property names

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

### `functions()`

Returns a minimal list of all registered user-defined functions. Designed for agent discovery with minimal payload size:

```json
{
  "payload": {
    "functions": [
      {"name": "queryActiveSpaces", "description": "Returns active spaces", "kind": "query"},
      {"name": "mutationCreateSpace", "description": "Creates a space", "kind": "mutation"}
    ],
    "count": 2
  }
}
```

Each entry includes only `name`, `description`, and `kind`. Use `help(name)` to get full details for a specific function.

### `tools()`

Returns MCP-compatible tool definitions for AI model integration. Each entry includes `name`, `description`, and `inputSchema`:

```json
{
  "payload": {
    "tools": [
      {
        "name": "searchUsers",
        "description": "Search for users",
        "inputSchema": {
          "type": "object",
          "properties": {
            "active": {"type": "boolean"},
            "limit": {"type": "number"}
          }
        }
      }
    ],
    "count": 1
  }
}
```

### `help()`

Returns full details for a specific function or tool by name:

```
help("queryActiveSpaces")
help({"name": "searchUsers"})
```

For functions, returns `type`, `name`, `description`, `kind`, `enabled`, and `argsSchema`; for tools, returns `inputSchema`, `handlerType`, and `annotations`. Returns an error if no function or tool matches the name.

### `shapeTemplates()`

Lists available shape templates for result projection. Optionally filter by concept:

```
shapeTemplates()                                   -- All shapes
shapeTemplates("v1:cognition:participant")         -- Filter by concept (string shortcut)
shapeTemplates({"concept": "v1:cognition:participant"})  -- Filter by concept (object)
```

Each entry includes only `name` and `description`. Use `shapeHelp(name)` to get full template details.

### `shapeHelp()`

Returns full details for a shape template by name, including the projected field structure and input schema:

```
shapeHelp("participantFull")
shapeHelp({"name": "participantFull"})
```

Agents can use `shapeHelp()` to understand the exact projection before calling a query that uses the shape.

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

Error codes:
- `MISSING_REQUIRED_FIELD` - Required argument (concept) not provided
- `CONCEPT_NOT_FOUND` - Concept does not exist
- `SCHEMA_ERROR` - Problem with concept schema
- `SCHEMA_VALIDATION_FAILED` - Payload doesn't match schema

Agents can use `previewInsert()` to validate payloads before inserting, predict assigned IDs, check existence for idempotent operations, and get detailed validation errors.

### `memqlVersion()`

Returns the engine's language/version metadata. (The legacy `concept==memql:version` compatibility query shape is retired — call `memqlVersion()` directly.)

## Cache Behavior

Query-result caching is **opt-in per query**: a query with no cache hint is not cached. The engine clamps every resolved TTL by the global ceiling and skips writes entirely when the resolved TTL is `0`.

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `CACHE_MAX_TTL` | Maximum cache TTL in seconds | `300` |
| `MEMQL_SI_CACHE_DEFAULT_ENABLED` | Enable AI response caching | `false` |
| `MEMQL_SI_CACHE_MAX_SECONDS` | Maximum AI cache TTL | `300` |

Cache keys are computed from the normalized query expression (cache hints fold into the canonical form, so two queries that differ only by hint value never share or overwrite cache entries).

> **Retired.** The concept-level cache default (`cacheTTLSeconds` in the old `concept.json`) is retired along with the concept metadata file — caching no longer falls back to a per-concept TTL. The `@cache(...)` / `@fields(...)` annotations on runtime query strings are rejected post-#250 (any `@`-suffix on a runtime string errors with a migration hint).

## Common Patterns

### Finding Unprocessed Items

```memql
concept==v1:task && payload.processed==nil
```

### Filtering by Date Range

```memql
concept==v1:event && createdAt>"2025-01-01" && createdAt<"2025-02-01"
```

### Combining Multiple Concepts

```memql
concept==v1:user || concept==v1:admin
```

### Nested Field Queries

```memql
concept==v1:profile && payload.settings.notifications.email==true
```

## Worked Examples

### Basic Listing (historical snapshot)

```
asOf(concept==v1:assistant && payload.active==true, "2025-11-01T00:00:00Z")
```

List all assistants that were active at the start of November 2025.

### Paginated Worlds by Recency

```
sort(
  paginate(concept==v1:examples:world && payload.status=="active", 25, 25),
  "createdAt","desc"
)
```

Returns the second page of active worlds, sorted by recency.

### Graph Traversal

```
childOf(concept==v1:examples:world && id=="v1:examples:world:world-aurora") && payload.tier=="silver"
```

Fetch all silver-tier modules that belong to `world-aurora`.

### Mixed Relationships + Filters

```
parentOf(
  contains(
    concept==v1:examples:module && payload.tier in ["silver", "gold"]
  )
) && payload.status=="active"
```

(DSL filter clause — at the runtime string surface, write the membership as `(payload.tier=="silver" || payload.tier=="gold")`.)

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
      "tier":"gold"
    }
  )
  ```

## Error Handling

### Structured Error Format

MemQL returns machine-actionable structured errors for AI agent consumption. Errors follow this JSON format:

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

Suggestions are static templates to help agents recover from errors. They never involve AI generation:

```
MISSING_REQUIRED_FIELDS → "Add the missing required fields: {fields}"
UNKNOWN_CONCEPT        → "Check available concepts with: concepts()"
UNKNOWN_FUNCTION       → "Check available functions with: functions()"
SYNTAX_ERROR           → "Check MemQL syntax with: memqlDocs()"
RELATIONSHIP_NOT_FOUND → "Check concept relationships with: help(\"conceptName\")"
```

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `unknown concept` | Concept not defined | Check concept name spelling |
| `invalid query syntax` | Malformed expression | Review operator/parentheses usage |
| `unsupported query shape` | Runtime string uses a retired shape (`shape()`, `select()`, `in`, `@`-suffix, `:=`) | Follow the migration hint in the error; declare the construct in the DSL |
| `spec not found` | Referenced spec doesn't exist | Define the spec or check spelling |

### Debugging Tips

1. Start with simple queries and add complexity
2. Check concept definitions for required fields
3. Use `validate(concept, payload)` / `previewInsert(...)` to check payloads before insert
4. Use `functions()` to list available functions
5. Use `help(name)` to get detailed help on any function or tool
6. Use `shapeTemplates()` / `shapeHelp(name)` to inspect projections

## Query Execution

Queries can be executed via the gRPC `MemqlService.Stream` bidirectional RPC or through the WebSocket bridge at `/memql/ws`. Both paths share the same backend validation, so every expression follows the same rules described in this guide.

## Subscriptions & Events

MemQL provides a real-time event system that delivers notifications for graph mutations, query execution, AI completions, and session lifecycle events. Clients subscribe over the existing bidirectional gRPC stream (or WebSocket bridge) and receive `EventNotification` messages as changes occur.

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
| `graph.node.created.{partition}.{concept}` | `NODE_CREATED` | Graph node inserted |
| `graph.node.deleted.{partition}.{concept}` | `NODE_DELETED` | Graph node deleted |
| `graph.node.updated.{partition}.{concept}` | `NODE_UPDATED` | Graph node updated |
| `query.executed` | `QUERY_EXECUTED` | Query completed |
| `si.completion.started` | `SI_COMPLETION_STARTED` | AI request began |
| `si.completion.finished` | `SI_COMPLETION_FINISHED` | AI request succeeded |
| `si.completion.error` | `SI_COMPLETION_ERROR` | AI request failed |
| `session.opened` | `SESSION_OPENED` | gRPC session started |
| `session.closed` | `SESSION_CLOSED` | gRPC session ended |

> **#56 phase 8 caveat:** node-event topics still embed a partition segment between the action and the concept, which is why subscription patterns (and automation trigger matching) use a `.*.` wildcard there. That segment goes away in phase 8.

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
      "topic": "graph.node.created.default.v1:cognition:space",
      "eventKind": "node_created",
      "nodeId": "v1:cognition:space:space-abc",
      "concept": "v1:cognition:space",
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

> See [docs/public/concepts/events.md](../concepts/events.md) for the full architecture, payload schemas, and implementation details.

## MemQL Language Reference for AI Agents

This is a condensed syntax specification designed to fit within limited context windows. Use this for quick reference; for detailed explanations and examples, see the sections above.

### Basic Filter Syntax (runtime strings)

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
| `==nil` | `payload.field==nil` | Field is null/absent |
| `!=nil` | `payload.field!=nil` | Field exists |
| `in` | `payload.kind in ["a", "b"]` | Membership — DSL filter clauses only; in runtime strings write a disjunction |

### Logical Operators

- `&&` – AND
- `||` – OR
- `!` – NOT
- `()` – Grouping
- Go precedence: `!` > comparisons > `&&` > `||`

(The legacy `;`-AND / `,`-OR separators, `has`, `?.`, and `??` are retired and rejected.)

### Defaults / Fallbacks

```memql
coalesce(args.nickname, args.name, "Unknown")
```

`coalesce(a, b, ...)` returns the first non-null operand. (The `??` operator it replaces is retired.)

### Core Directives

```text
asOf(expr, "timestamp")         # Historical query
sort(expr, "field", "desc")     # Sort by field descending
paginate(expr, limit)           # Page size (keyset cursor continues)
withDepth(traversal, n)         # Limit relationship depth
```

### Relationship Traversal

```text
parentOf(expr)                  # Get parents
childOf(expr)                   # Get children
contains(expr)                  # Collection membership
owns(expr)                      # Ownership links
ids(expr)                       # Lightweight id-only nodes
```

### Mutations (Append-Only)

```text
insert("concept", id="id", payload={...})
```

### DSL Construct Cheat Sheet

```memql
use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
use common.traits.{ traitIsActiveRecord }

query participant queryX {
  args { spaceId string @required }
  filter payload.spaceId==args.spaceId && traitIsActiveRecord
  shape participantFull
}

mutation space mutationCreateSpace {
  args { spaceId string @required  name string @required }
  insert { id: args.spaceId  name: args.name  status: "active"  createdAt: now  createdBy: actor.userId }
}

spec specIsHumanParticipant { payload.participantType == "human" }

@row
shape participant participantCard { row.id  payload.displayName }

@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation bootstrapSession { step run { logic bootstrapSession { event: event } } }
```

### Example Patterns

```memql
# Get active users
concept==v1:user && payload.active==true

# Get users created this year
concept==v1:user && createdAt>"2025-01-01T00:00:00Z"

# Call a DSL-defined query with args
querySpaceParticipants({"spaceId": "space-123", "status": "active"})

# Sorted, paginated function call
sort(paginate(queryActiveSpaces(), 10), "createdAt", "desc")
```

Use this reference when constructing MemQL queries. Always validate syntax and concept paths against the engine's response.

## Runtime Parser (epic #218: #248 → #249 → #250)

The runtime grammar consumed by `engine.Execute(ctx, query string)` — function invocations (`funcName({k: v, ...})`), filter expressions (`concept==X && payload.Y==Z`), `insert(...)` literals, and introspection meta-commands — is parsed exclusively through the language parser (`langparser.ParseExpression` + `ASTConverter`). The legacy in-package recursive-descent runtime parser was retired in #328 / #250 after the soak window; there is no fallback path.

A small set of legacy runtime shapes is rejected upfront with a typed `ErrUnsupportedQueryShape` carrying a shape-specific migration hint:

- `shape(...)` — use a DSL-defined query with a `shape` projection instead.
- `select(...)` — declare the projection in the DSL query.
- `<expr> in (...)` — rewrite as a disjunction (`x=="a" || x=="b" || ...`) in runtime strings (DSL filter clauses keep `in`).
- `concept==memql:version` — call `memqlVersion()` directly.
- Trailing `@timestamp` / `@latest` suffix — pin the timestamp via `asOf(...)` or at the DSL definition site.
- Inline spec definition (`name := expr`) — define the spec in the DSL and reference it by name.
- Trailing comma in the query string.

Cross-parser equivalence for the supported shapes is guarded by `TestParseViaLangparser_Equivalence` in `component/memql/parser_langpath_test.go`. Add a row there if a new caller adopts a shape the corpus doesn't cover.

## Upcoming Features

These roadmap items are planned but not yet implemented. Update this section as features land or priorities change.

- **Streaming Responses** – add streaming execution so clients can start reading partial MemQL results before the query finishes (instead of waiting for a single HTTP response body).

## Keeping This Guide Up to Date

Any change to MemQL parsing, execution options, relationships, or mutations **must** be reflected here. Before merging query-language changes:

1. Update the relevant sections (syntax, operators, options, examples, roadmap).
2. Reference this document in pull requests so reviewers verify documentation parity.
