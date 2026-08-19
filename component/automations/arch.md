# Automations Package Architecture

> **Last Updated:** 2026-07-26

This document describes the architecture of the `automations/` package, which provides the multi-step workflow execution engine for MemQL.

## Package Overview

The automations subsystem is split across two trees. The Go runtime lives
under `component/automations/` (107 top-level files plus 58 more under
`steps/` -- mostly test coverage for the sandboxed expression/logic
runtime; see "Key entry points" below for the files worth reading first).
The `.memql` definitions live inline in each domain's own
`dsl/<domain>/automations.memql` file -- one bundled file per domain
(cognition, common, data, ...), each declaring many `automation { ... }`
blocks -- not a per-automation directory tree. (An earlier layout gave
each automation its own directory under `dsl/v1/automations/v1/<domain>/
<name>/`; that directory does not exist any more -- see "Loader Flow"
below, memql#2858.)

Key entry points in `component/automations/`:

```
component/automations/
├── arch.md              # This architecture document
├── types.go             # Type definitions (Automation, Step, StepResult, etc.)
├── loader.go            # Loads automations from the unified DSL tree
├── unified_loader.go    # Slices + compiles automation { ... } blocks out of dsl/<domain>/automations.memql
├── scheduler.go         # Cron and event-based triggering
├── executor.go          # Orchestrates automation execution
├── evaluator.go         # Resolves $var.NAME expressions at runtime
├── cluster_guard.go     # Cross-replica exactly-once claim for event-triggered automations
├── cron_leader.go       # Cluster-singleton cron firing (Postgres advisory lock)
├── integration_test.go  # Integration tests
└── steps/               # Step type executors
    ├── steps.go         # Step registry
    ├── query.go         # MemQL query execution
    ├── shape.go         # Data transformation
    ├── webhook.go       # HTTP requests
    ├── event.go         # Event publishing
    ├── function.go      # Function invocation
    ├── foreach.go       # Iteration
    ├── parallel.go      # Concurrent execution
    ├── switch.go        # Conditional branching
    └── automation.go    # Sub-automation invocation
```

---

## System Architecture

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                         AUTOMATION SYSTEM OVERVIEW                                │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   Triggers                                                                        │
│   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                            │
│   │    Cron      │  │    Event     │  │   Manual     │                            │
│   │  "*/5 * * *" │  │"session.open"│  │   API Call   │                            │
│   └──────┬───────┘  └──────┬───────┘  └──────┬───────┘                            │
│          │                 │                 │                                    │
│          └─────────────────┼─────────────────┘                                    │
│                            ▼                                                      │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                          SCHEDULER                                      │     │
│   │                        scheduler.go                                     │     │
│   │                                                                         │     │
│   │   • Loads automations via Loader                                        │     │
│   │   • Registers cron jobs                                                 │     │
│   │   • Subscribes to event triggers                                        │     │
│   │   • Dispatches to Executor                                              │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                            │                                                      │
│                            ▼                                                      │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                          EXECUTOR                                       │     │
│   │                        executor.go                                      │     │
│   │                                                                         │     │
│   │   • Creates execution context                                           │     │
│   │   • Runs input query                                                    │     │
│   │   • Iterates through steps                                              │     │
│   │   • Evaluates conditions                                                │     │
│   │   • Handles errors and retries                                          │     │
│   │   • Publishes execution events                                          │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                            │                                                      │
│        ┌───────────────────┼───────────────────┐                                  │
│        ▼                   ▼                   ▼                                  │
│   ┌─────────┐         ┌─────────┐         ┌─────────┐                             │
│   │Evaluator│         │  Step   │         │MemQL    │                             │
│   │         │◄───────▶Registry ────────▶ Engine                                  │
│   └─────────┘         └─────────┘         └─────────┘                             │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

---

## Component Details

### 1. Loader (`loader.go`)

Responsible for loading automation definitions from the unified DSL tree.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                              LOADER FLOW                                          │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   dsl/<domain>/automations.memql   (one bundled file per domain)                   │
│        │                                                                          │
│        │   each file declares MANY automations + their logic blocks                │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                    LoadAll -> LoadFromUnifiedTree                       │     │
│   │                                                                         │     │
│   │  1. Slice each `automation <name> { ... }` out of the bundled source     │     │
│   │  2. Compile each slice in isolation via compileMemQL                     │     │
│   │  3. Stamp Origin = "unified:<path>:<name>" and Trusted = true           │     │
│   │                                                                         │     │
│   │  Rules:                                                                 │     │
│   │  • Files starting with _ are skipped                                    │     │
│   │  • A malformed construct refuses boot (strict-boot gate)                │     │
│   │                                                                         │     │
│   │  There is NO second pass and no .json loading path. A legacy walker     │     │
│   │  over a Loader.fsys field, plus an on-disk `.json` automation format,   │     │
│   │  were unreachable and were deleted in memql#2858 -- LoadAll is now a    │     │
│   │  thin wrapper. (parseJSON survives, but only as the LogicRunner's       │     │
│   │  in-memory compiler-JSON -> Automation step; it reads no files.)        │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                            │                                                      │
│                            ▼                                                      │
│                     []*Automation                                                 │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Key Methods:**
| Method | Description |
|--------|-------------|
| `LoadAll()` | Loads all automations from configured paths |
| `LoadByName(name)` | Loads a specific automation by name |
| `compileMemQL(source)` | Compiles `.memql` source to `Automation` struct |
| `parseJSON(data)` | Parses JSON data to `Automation` struct |
| `validateSteps(steps)` | Validates step configuration |

**RETIRED: the per-automation directory convention.** Before memql#2858, each
automation lived in its own directory (`automations/v1/{automationName}/`,
carrying `automation.memql`, a required `automation.md` flow-diagram doc, and
sometimes a compiled `{name}.json`). None of that exists any more -- no
`automation.md` file exists anywhere in the tree today, and the on-disk
`.json` loader was deleted in the same change. The live layout is the one
"Package Overview" above and "Loader Flow" below describe: one bundled
`dsl/<domain>/automations.memql` file per domain, with no per-automation
directory and no separate doc file.

---

### 2. Scheduler (`scheduler.go`)

Manages automation triggering via cron schedules and event subscriptions.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                            SCHEDULER INTERNALS                                    │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                         SCHEDULER                                       │     │
│   │                                                                         │     │
│   │   ┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐     │     │
│   │   │   Cron Engine   │    │  Event Subs     │    │  Automation Map │     │     │
│   │   │ (robfig/cron)   │    │                 │    │                 │     │     │
│   │   │                 │    │ session.opened  │    │ bootstrapUser   │     │     │
│   │   │ */30 * * * * *  │    │ graph.node.*    │    │ leadClassify    │     │     │
│   │   │ 0 0 * * *       │    │ automation.#    │    │ ...             │     │     │
│   │   └────────┬────────┘    └────────┬────────┘    └────────┬────────┘     │     │
│   │            │                      │                      │              │     │
│   │            └──────────────────────┼──────────────────────┘              │     │
│   │                                   ▼                                     │     │
│   │                          TriggerAutomation()                            │     │
│   │                                   │                                     │     │
│   │                                   ▼                                     │     │
│   │                             Executor.Execute()                          │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Lifecycle:**
```go
scheduler.Start(ctx)     // Load automations, start cron, subscribe to events
scheduler.Stop()         // Stop cron, unsubscribe events, wait for completion
scheduler.Reload()       // Hot-reload automation definitions
```

**Trigger Types:**
| Trigger | Configuration | Example |
|---------|---------------|---------|
| Cron | `schedule: "*/5 * * * *"` | Every 5 minutes |
| Event | `trigger.event: "session.opened"` | On session connect |
| Manual | API call | `scheduler.TriggerAutomation(ctx, "name")` |

---

### 3. Executor (`executor.go`)

Orchestrates the execution of an automation's steps.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                          EXECUTION FLOW                                           │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   Execute(ctx, automation, triggeredBy)                                           │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  1. Create Execution Context                                            │     │
│   │     • Inject system actor (system:automation:name)                      │     │
│   │     • Create AutomationExecution record                                 │     │
│   │     • Initialize Evaluator with $timestamp, $event                      │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  2. Run Input Query (if defined)                                        │     │
│   │     • Execute automation.input.query via MemQL engine                   │     │
│   │     • Store result as $input                                            │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  3. Execute Steps (sequential)                                          │     │
│   │     FOR each step:                                                      │     │
│   │       • Check cancellation                                              │     │
│   │       • Evaluate condition (skip if false)                              │     │
│   │       • Dispatch to step executor                                       │     │
│   │       • Store result as $steps.{id}.result                              │     │
│   │       • Handle errors (stop/continue/retry)                             │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│        │                                                                          │
│        ├──── Success ──▶ Run onComplete hook                                     │
│        │                                                                          │
│        └──── Failure ──▶ Run onError hook                                        │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Error Strategies:**
| Strategy | Behavior |
|----------|----------|
| `stop` (default) | Halt automation, run onError hook |
| `continue` | Log warning, proceed to next step |
| `retry` | Retry up to `retryCount` times |

---

### 4. Evaluator (`evaluator.go`)

Resolves `$` expressions in step configurations at runtime.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                         EXPRESSION EVALUATION                                     │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   Input:  "Hello $event.payload.firstName, your role is $var.DEFAULT_ROLE"        │
│                    │                                      │                       │
│                    ▼                                      ▼                       │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                         EVALUATOR                                       │     │
│   │                                                                         │     │
│   │   Data Sources:                                                         │     │
│   │   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                  │     │
│   │   │   $input     │  │   $steps     │  │   $event     │                  │     │
│   │   │              │  │              │  │              │                  │     │
│   │   │ Input query  │  │ Step results │  │ Trigger event│                  │     │
│   │   │   result     │  │ by step ID   │  │   payload    │                  │     │
│   │   └──────────────┘  └──────────────┘  └──────────────┘                  │     │
│   │                                                                         │     │
│   │   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                  │     │
│   │   │   $var       │  │   $item      │  │  $timestamp  │                  │     │
│   │   │              │  │              │  │              │                  │     │
│   │   │  Variables   │  │ forEach item │  │  Current UTC │                  │     │
│   │   │  from DB     │  │              │  │  ISO 8601    │                  │     │
│   │   └──────────────┘  └──────────────┘  └──────────────┘                  │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                    │                                      │                       │
│                    ▼                                      ▼                       │
│   Output: "Hello John, your role is member"                                       │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Expression Reference:**
| Expression | Description | Example |
|------------|-------------|---------|
| `$input` | Input query result | `$input[0].payload.name` |
| `$steps.{id}.result` | Step output data | `$steps.checkUser.result` |
| `$steps.{id}.metadata` | Step metadata | `$steps.fetch.metadata.itemCount` |
| `$steps.{id}.status` | Step status | `$steps.save.status` |
| `$event` | Triggering event | `$event.payload.subject` |
| `$var.{NAME}` | Variable from DB | `$var.DISCORD_WEBHOOK_URL` |
| `$item` | Current forEach item (JSON automations) | `$item.email` |
| `$index` | Current forEach index | `$index` |
| `$timestamp` | Current UTC time | `$timestamp` |
| `$automation.errors` | Accumulated errors | `$automation.errors` |
| `$pretty($path)` | Prettified JSON string | `$pretty($steps.fetch.result)` |
| `$coalesce(a, b, ...)` | First non-empty value | `$coalesce($steps.x.result.id, $event.payload.id)` |

---

### 5. Step Registry (`steps/steps.go`)

Provides executors for each step type.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                           STEP TYPES                                              │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                       STEP REGISTRY                                     │     │
│   │                                                                         │     │
│   │   Execute(ctx, step, stepCtx) → Routes to appropriate executor          │     │
│   │                                                                         │     │
│   │   ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐    │     │
│   │   │   query     │  │   shape     │  │  webhook    │  │   event     │    │     │
│   │   │             │  │             │  │             │  │             │    │     │
│   │   │  MemQL      │  │   Data      │  │   HTTP      │  │  Publish    │    │     │
│   │   │  Queries    │  │  Transform  │  │  Requests   │  │  to Bus     │    │     │
│   │   └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘    │     │
│   │                                                                         │     │
│   │   ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐    │     │
│   │   │  function   │  │  forEach    │  │  parallel   │  │   switch    │    │     │
│   │   │             │  │             │  │             │  │             │    │     │
│   │   │   Named     │  │  Iterate    │  │ Concurrent  │  │ Conditional │    │     │
│   │   │  Functions  │  │  Items      │  │  Branches   │  │  Branching  │    │     │
│   │   └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘    │     │
│   │                                                                         │     │
│   │   ┌─────────────┐                                                       │     │
│   │   │ automation  │                                                       │     │
│   │   │             │                                                       │     │
│   │   │   Invoke    │                                                       │     │
│   │   │   Sub-auto  │                                                       │     │
│   │   └─────────────┘                                                       │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Step Type Details:**

| Step Type | File | Purpose |
|-----------|------|---------|
| `query` | `query.go` | Execute MemQL queries/mutations |
| `shape` | `shape.go` | Transform data using templates |
| `webhook` | `webhook.go` | Make HTTP requests |
| `event` | `event.go` | Publish events to the bus |
| `function` | `function.go` | Invoke named MemQL functions |
| `forEach` | `foreach.go` | Iterate over collections |
| `parallel` | `parallel.go` | Execute branches concurrently |
| `switch` | `switch.go` | Conditional branching |
| `automation` | `automation.go` | Invoke sub-automations |

---

## Data Types

### Automation Structure

```go
type Automation struct {
    Name        string           // Unique identifier (camelCase)
    Description string           // Human-readable description
    Schedule    string           // Cron expression (optional)
    Trigger     *TriggerConfig   // Event trigger (optional)
    Input       *AutomationInput // Initial data query (optional)
    Steps       []*Step          // Ordered operations
    OnComplete  *Step            // Success hook (optional)
    OnError     *Step            // Failure hook (optional)
    Enabled     *bool            // Active flag (default: true since #2604; @disabled clears it)
    Origin      string           // Source file path
}
```

### Step Structure

```go
type Step struct {
    ID         string          // Unique within automation
    Name       string          // Human-readable description
    Type       StepType        // query, shape, webhook, etc.
    OnError    ErrorStrategy   // stop, continue, retry
    RetryCount int             // For retry strategy
    Condition  string          // Expression that must be true

    // Type-specific configuration (one set per step):
    Query      *QueryStepConfig
    Shape      *ShapeStepConfig
    Webhook    *WebhookStepConfig
    Event      *EventStepConfig
    Function   *FunctionStepConfig
    ForEach    *ForEachStepConfig
    Parallel   *ParallelStepConfig
    Switch     *SwitchStepConfig
    Automation *AutomationStepConfig
}
```

### Execution Result

```go
type AutomationExecution struct {
    ID             string                  // Unique execution ID
    AutomationName string                  // Which automation ran
    Status         string                  // running, completed, failed, cancelled
    Input          any                     // Input query result
    Steps          map[string]*StepResult  // Results by step ID
    Error          string                  // Failure message
    StartedAt      time.Time
    CompletedAt    time.Time
    Duration       time.Duration
    TriggeredBy    string                  // schedule, manual, event:{topic}
}
```

---

## CQS File Composition Rules

MemQL enforces **Command-Query Separation (CQS)** principles (`component/language/compiler/composition.go`,
`ValidateFileComposition`). "File" here means the compiled unit these
rules run against, which today is a SLICE the unified loader cuts out of
the bundled `dsl/<domain>/automations.memql` (one `automation { ... }`
block, isolated and compiled on its own -- see "Loader Flow" above), not
the bundled file as a whole.

### Rules for Automation Files

| Rule | Description |
|------|-------------|
| **Exactly 1 automation per file** | Workflows are complex; single source of truth |
| **Can have helper queries** | Supporting queries for validation, checks |
| **No mutations** | A slice with an automation may not also carry a mutation; standalone mutations belong in the domain's own `dsl/<domain>/mutations.memql` |

### Valid Composition

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    VALID FILE COMPOSITION (one automation slice)                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  @enabled                                                                       │
│  @trigger(event="session.opened")                                               │
│  automation bootstrapUser() {                                                   │
│    checkUser: query { ... }        ← Steps ARE inside automation                │
│    createUser: mutation when ... { ... }                                        │
│    return ...                                                                   │
│  }                                 ← ONE automation (workflow owner)            │
│                                                                                 │
│  query helperValidation() { ... }  ← External helper query OK                   │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Invalid Compositions

The real error messages, verified against `component/language/compiler/composition.go`:

| Composition | Error |
|-------------|-------|
| 2+ automations | `only one automation definition allowed per file` |
| 2+ mutations | `only one mutation definition allowed per file` |
| Automation + mutation | `cannot mix automation and mutation in the same file` |

### Why CQS for Automations?

1. **Single Source of Truth**: One workflow per compiled unit for clear ownership
2. **Debugging**: Clear stack traces and execution logs
3. **Auditability**: Each automation is an isolated, traceable unit
4. **Separation**: Reusable mutations go in the domain's `mutations.memql`, automation-specific steps stay internal

---

## Automation Definition Formats

### MemQL Format (`.memql`) - Preferred

#### `.memql` strict reference rules

Automation `.memql` files use a strict, ergonomic subset of the JSON automation reference syntax:

- **Do not use** `$steps.*` in `.memql` sources. Use **bare step IDs** instead (e.g. `getAgent.result.Bundle.nodes`).
- **for-range loops** must use the item variable name **`item`**.
- **Bare dotted paths** are only auto-resolved when they start with a **known step ID** or the reserved **`item.*`** root.

If you need a literal string containing dots, quote it explicitly.

Automations use **attribute decorators** (`@`) for configuration, keeping metadata separate from logic:

```memql
-- automations/v1/bootstrapUser/automation.memql
@enabled
@trigger(event="session.opened")
@description("Auto-provision user on WebSocket connect")
automation bootstrapUser() {
  
  checkUser: query {
    concept==v1:identity:user;payload.authorizerId==event("payload.subject")
  }
  
  createUser: mutation when step("checkUser").metadata.itemCount == 0 {
    insert("v1:identity:user",
      id=concat("user-", event("payload.subject")),
      payload={
        "authorizerId": event("payload.subject"),
        "email": event("payload.email"),
        "role": var("MEMQL_DEFAULT_USER_ROLE")
      }
    )
  }
  
  return coalesce(step("createUser"), first(step("checkUser")))
}
```

### Available Attributes

| Category | Attribute | Arguments | Description |
|----------|-----------|-----------|-------------|
| **Lifecycle** |
| | `@enabled` | none | Accepted no-op; automations are enabled by default (#2604) |
| | `@disabled` | none | Explicitly disables the automation |
| **Documentation** |
| | `@description` | `"..."` | Human-readable description |
| **Triggers** |
| | `@trigger` | `event="..."`, `filter="..."`, `schedule="..."` | Event- or schedule-based trigger |
| | `@filter` | `expression` | Predicate over the triggering event's payload |
| | `@schedule` | `cron="..."` | Cron-based schedule (synonym for `@trigger(schedule=...)`) |

Automations also accept `@actor` (declares the body reads `actor.*`) and `@mcp`.

**Note:** Automations are **enabled by default** (#2604, the uniform lifecycle ruling); `@enabled` is an accepted no-op and `@disabled` is the off-switch. The annotations the parser also folds on automations -- `@deprecated`, `@version`, `@timeout`, `@retry`, `@audit`, `@async`, `@rateLimit` -- are **not** valid: the automation runtime never honored them, and the load gate now rejects them (#2712). (Most are dead vocabulary on the function-style constructs generally, removed from the allow-lists in #989 -- see attribute-matrix.md; `@rateLimit` is valid only on tools, `@version` only on seeds/concepts.) Only `@schedule` among the once-tolerated extras is live on automations (it feeds the cron scheduler).

See also: [`docs/public/language/attribute-matrix.md`](../../docs/public/language/attribute-matrix.md) for the full attribute reference across all function types.

**Examples:**

```memql
-- Event-triggered automation
@enabled
@trigger(event="session.opened")
automation bootstrapUser() { ... }

-- Scheduled automation (disabled for safety)
@disabled
@schedule(cron="*/30 * * * *")
automation leadClassification() { ... }
```

### JSON Format (`.json`) - RETIRED (memql#2858; no on-disk loader exists)

```json
{
  "name": "bootstrapUser",
  "trigger": { "event": "session.opened" },
  "steps": [
    {
      "id": "checkUser",
      "type": "query",
      "query": { "query": "concept==v1:identity:user;..." }
    },
    {
      "id": "createUser",
      "type": "query",
      "condition": "$steps.checkUser.metadata.itemCount == 0",
      "query": { "query": "insert(...)" }
    }
  ]
}
```

**Condition syntax**

Conditions are evaluated by the automation expression evaluator:

- AND: `;` or `&&`
- OR: `,` or `||`
- Literals: `null`, `true`, `false`, and numbers

---

## Event Flow

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                          EVENT LIFECYCLE                                          │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   Published Events (automation emits):                                            │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  automation.started    → { automationName, executionId, triggeredBy }   │     │
│   │  automation.completed  → { automationName, executionId, duration }      │     │
│   │  automation.failed     → { automationName, executionId, error }         │     │
│   │  automation.step.*     → { automationName, stepId, status, result }     │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                   │
│   Consumed Events (automation triggers on):                                       │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  session.opened       → User connected via WebSocket                    │     │
│   │  graph.node.created   → New node inserted                               │     │
│   │  graph.node.*         → Wildcard pattern matching                       │     │
│   │  custom.topic.name    → Application-specific events                     │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

---

## Testing

Run via `make test` (see CLAUDE.md's Testing section -- `go test ./...` from
the repo root misses this package). To target just this package directly,
name the full module path or `cd` into the directory first:

```bash
# Run all automation tests
go test github.com/znasllc-io/memql/component/automations/...

# Run with verbose output
go test github.com/znasllc-io/memql/component/automations/... -v

# Run specific test
go test github.com/znasllc-io/memql/component/automations/... -run TestEvaluatorSeesArgs

# Or, from inside the package directory:
cd component/automations && go test ./... -run TestEvaluatorSeesArgs
```

---

## Files Reference

| File | Purpose |
|------|---------|
| `types.go` | Core type definitions |
| `loader.go` | .memql compilation (LoadAll -> LoadFromUnifiedTree) |
| `scheduler.go` | Cron and event triggering |
| `executor.go` | Step orchestration |
| `evaluator.go` | $ expression resolution |
| `steps/steps.go` | Step executor registry |
| `steps/query.go` | MemQL query execution |
| `steps/shape.go` | Data transformation |
| `steps/webhook.go` | HTTP requests |
| `steps/event.go` | Event publishing |
| `steps/foreach.go` | Collection iteration |
| `steps/parallel.go` | Concurrent execution |
| `steps/switch.go` | Conditional branching |

---

*For engine architecture, see [`component/memql/arch.md`](../memql/arch.md)*
*For functions architecture, see [`docs/public/language/functions.md`](../../docs/public/language/functions.md)*
*For system-wide architecture, see [`docs/public/concepts/architecture.md`](../../docs/public/concepts/architecture.md)*
