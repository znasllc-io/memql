# Automations Package Architecture

> **Last Updated:** 2025-12-07

This document describes the architecture of the `automations/` package, which provides the multi-step workflow execution engine for MemQL.

## Package Overview

The automations subsystem is split across two trees: the Go runtime
lives under `component/automations/`; the .memql definitions plus the
`go:embed` declaration live under `dsl/v1/automations/`.

```
component/automations/
├── arch.md              # This architecture document
├── types.go             # Type definitions (Automation, Step, StepResult, etc.)
├── loader.go            # Loads automations from the embedded FS
├── scheduler.go         # Cron and event-based triggering
├── executor.go          # Orchestrates automation execution
├── evaluator.go         # Resolves $ expressions at runtime
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

dsl/v1/automations/
├── CLAUDE.md            # DSL-side guidance for authoring automations
├── embed.go             # go:embed + Source() helper -- imported by the loader
└── v1/                  # Automation definitions (.memql + .md only)
    └── <domain>/        # Domain grouping (cognition, common, data, ...)
        └── <name>/      # Individual automation
            ├── automation.memql  # MemQL source
            └── automation.md     # Flow diagrams & docs
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

Responsible for loading automation definitions from the filesystem.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                              LOADER FLOW                                          │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   automations/v1/                                                                 │
│        │                                                                          │
│        ├── bootstrapUser/                                                         │
│        │   ├── automation.memql  ──────┐  (.memql has priority)                   │
│        │   ├── automation.md           │                                          │
│        │   └── bootstrapUser.json      │  (ignored if .memql exists)              │
│        │                               │                                          │
│        ├── leadClassification/         │                                          │
│        │   ├── automation.md           │                                          │
│        │   └── leadClassification.json─┼──┐                                       │
│        │                               │  │                                       │
│        └── ...                         │  │                                       │
│                                        ▼  ▼                                       │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │                           LOADER                                        │     │
│   │                                                                         │     │
│   │  Priority Order:                                                        │     │
│   │  1. Look for automation.memql → Compile via engine/compiler             │     │
│   │  2. Fall back to .json files → Parse directly                           │     │
│   │                                                                         │     │
│   │  Rules:                                                                 │     │
│   │  • One automation per directory                                         │     │
│   │  • .memql must contain exactly ONE automation definition                │     │
│   │  • Files starting with _ are skipped                                    │     │
│   │  • Directory with .memql ignores sibling .json files                    │     │
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

**Directory Structure Convention:**

Each automation should have its own directory under `v1/`:

```
automations/v1/{automationName}/
├── automation.memql          # MemQL source (preferred)
├── automation.md             # Flow diagrams and documentation (required)
└── {automationName}.json     # Compiled JSON or legacy definition
```

| File | Purpose |
|------|---------|
| `automation.memql` | MemQL source definition (takes priority over .json) |
| `automation.md` | Human-readable documentation with flow diagrams, timestamp |
| `{name}.json` | Compiled output or legacy JSON definition |

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
    Async       bool             // Run asynchronously (from @async)
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

MemQL enforces **Command-Query Separation (CQS)** principles at the file level.

### Rules for Automation Files

| Rule | Description |
|------|-------------|
| **Exactly 1 automation per file** | Workflows are complex; single source of truth |
| **Can have helper queries** | Supporting queries for validation, checks |
| **No mutations** | Standalone mutations belong in `mutations/` directory |

### Valid Composition

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    VALID FILE COMPOSITION (automations/)                        │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  automation.memql                                                               │
│  ┌───────────────────────────────────────────────────────────────────────────┐  │
│  │  @enabled                                                                 │  │
│  │  @trigger(event="session.opened")                                         │  │
│  │  automation bootstrapUser() {                                             │  │
│  │    checkUser: query { ... }        ← Steps ARE inside automation          │  │
│  │    createUser: mutation when ... { ... }                                  │  │
│  │    return ...                                                             │  │
│  │  }                                 ← ONE automation (workflow owner)      │  │
│  │                                                                           │  │
│  │  query helperValidation() { ... }  ← External helper query OK             │  │
│  └───────────────────────────────────────────────────────────────────────────┘  │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Invalid Compositions

| Composition | Error |
|-------------|-------|
| 2+ automations in one file | `only one automation definition allowed per file` |
| Standalone mutation | `mutation X should be in mutations/ directory, not automations/` |
| Automation + mutation (outside automation) | `cannot mix automation and mutation` |

### Why CQS for Automations?

1. **Single Source of Truth**: One workflow per file for clear ownership
2. **Debugging**: Clear stack traces and execution logs
3. **Auditability**: Each automation is an isolated, traceable unit
4. **Separation**: Reusable mutations go in `mutations/`, automation-specific steps stay internal

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
| | `@deprecated` | `"message"` (optional) | Marks as deprecated |
| | `@version` | `"v1"` | Version tag |
| **Documentation** |
| | `@description` | `"..."` | Human-readable description |
| **Access Control** |
| **Performance** |
| | `@timeout` | `"30s"` | Execution timeout |
| | `@rateLimit` | `requests=N, per="1m"` | Throttle execution |
| **Reliability** |
| | `@retry` | `count=3` | Retry on failure |
| **Auditing** |
| | `@audit` | none | Log all executions for audit trail |
| **Triggers** |
| | `@trigger` | `event="..."`, `filter="..."` | Event-based trigger |
| | `@schedule` | `cron="..."` | Cron-based schedule |
| | `@async` | none | Runs asynchronously when triggered |

**Note:** Automations are **enabled by default** (#2604, the uniform lifecycle ruling); `@enabled` is an accepted no-op and `@disabled` is the off-switch.

See also: `/docs/attribute-matrix.md` for the full attribute reference across all function types.

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

-- Async automation
@enabled
@async
@trigger(event="report.requested")
automation generateReport() { ... }
```

### JSON Format (`.json`) - Legacy/Compiled

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

```bash
# Run all automation tests
go test ./automations/...

# Run with verbose output
go test ./automations/... -v

# Run specific test
go test ./automations -run TestLoadBootstrapUserAutomation

# Run integration tests
go test ./automations -run "Test.*Integration"
```

---

## Files Reference

| File | Purpose |
|------|---------|
| `types.go` | Core type definitions |
| `loader.go` | .memql and .json file loading |
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

*For engine architecture, see `/engine/arch.md`*
*For functions architecture, see `/queries/arch.md`*
*For system-wide architecture, see `/docs/arch.md`*
