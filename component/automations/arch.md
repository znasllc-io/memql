# Automations Package Architecture

> **Last Updated:** 2026-09-14

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
├── loader.go            # Compiles one automation slice: parse, check, compile
├── unified_loader.go    # Slices automation { ... } blocks out of dsl/<domain>/automations.memql
├── scheduler.go         # Cron and event-based triggering
├── executor.go          # Opens a run and drives it
├── sequence.go          # Runs a statement body's steps in the order written
├── statement_scope.go   # Names in a statement body: frames, roots, values
├── run_state.go         # The Evaluator: what a run has bound and recorded
├── journal.go           # The work journal (v1:work:run / v1:work:step rows)
├── logic_statements.go  # A logic's statement body, run on the same sequence runner
├── precondition.go      # First-class precondition blocks
├── args_binding.go      # Binds the trigger payload into the automation's args
├── cluster_guard.go     # Cross-replica exactly-once claim for event-triggered automations
├── cron_leader.go       # Cluster-singleton cron firing (Postgres advisory lock)
└── steps/               # Step type executors
    ├── steps.go         # Step registry
    ├── function.go      # A query / mutation / logic / builtin call
    ├── action.go        # An action call
    ├── automation.go    # A sub-automation call
    ├── event.go         # publish
    ├── foreach.go       # for
    ├── parallel.go      # parallel
    └── statements.go    # A for's body and a parallel's branches, run as lists
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
│   │  thin wrapper. (parseJSON survives, but only as the in-memory           │     │
│   │  compiler-JSON -> Automation step a logic's run uses; it reads no files)│     │
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
| Cron | `schedule: "0 */5 * * * *"` (six fields, seconds first) | Every 5 minutes |
| Event | `trigger.event: "session.opened"` | On session connect |
| Manual | API call | `scheduler.TriggerAutomation(ctx, "name")` |

---

### 3. Executor (`executor.go`, `sequence.go`)

Opens a run and drives an automation's statement body through the sequence
runner. It is the one execution model: every automation's statements run
through `runSequence`, and there are no completion or error hooks and no
input query -- a failure the body does not `on error continue` past ends the
run.

```
┌───────────────────────────────────────────────────────────────────────────────────┐
│                          EXECUTION FLOW                                           │
├───────────────────────────────────────────────────────────────────────────────────┤
│                                                                                   │
│   Execute(ctx, automation, triggeredBy)                                           │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  1. Open the run                                                        │     │
│   │     • Inject the system actor (system:automation:name)                  │     │
│   │     • Bind the trigger payload into the args contract (args_binding.go) │     │
│   │     • Seed the roots: args, event, actor, now, config, partition        │     │
│   │     • Evaluate the preconditions; a miss aborts the run as skipped      │     │
│   │     • Open the v1:work:run journal row                                  │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│        │                                                                          │
│        ▼                                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────┐     │
│   │  2. Run the statements in the order written (runSequence)               │     │
│   │     FOR each step:                                                      │     │
│   │       • Check cancellation                                              │     │
│   │       • Skip it when its branch condition is false (it binds nothing)   │     │
│   │       • Journal it at running, dispatch it, journal it at done/failed   │     │
│   │       • Bind its value under its statement's name (`binds`)            │     │
│   │       • retry(n), then on error continue, or stop the run               │     │
│   │       • A return step ends the run with its value                       │     │
│   └─────────────────────────────────────────────────────────────────────────┘     │
│        │                                                                          │
│        ▼                                                                          │
│   Close the run: completed, failed, cancelled or skipped, on the journal          │
│                                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**Error handling**, as the statement's trailing clauses write it:

| Clause | Step field | Behavior |
|--------|------------|----------|
| (none) | `onError: stop` | Halt the run (the default, never written) |
| `on error continue` | `onError: continue` | Record the failure, leave the name absent, go on |
| `retry(n)` | `retryCount: n` | Retry a failed call up to `n` more times first |

A `for` and a `parallel` hold lists of their own; their executors run each
list through the same runner in a frame of its own (`RunStatementBody`), so a
name bound inside exists only inside, and a `return` inside ends the body the
statement is in. A logic called as a statement runs on the same runner and
journals its statements as steps of the run (`logic_statements.go`).

---

### 4. Run state (`run_state.go`, `statement_scope.go`)

A run's `Evaluator` holds what the run has bound: the seeded roots (`args`,
`event`, `actor`, `now`, `config`, `partition`), a stack of name frames, the
step results and the resolvers behind `var(...)`, `secret(...)` and
`canonicalId()`. It evaluates nothing itself: every expression of a run -- a
branch condition, a trigger filter, a precondition, an argument, a `for`
source -- is edition-2026 source, parsed once at load and evaluated by
`memql.EvalExpr` over `RunScope`, which reads this state. A trigger filter and
a precondition read only the roots (`args`, `actor`, `event`, `config`,
`partition`, `now`) and the filter's own parameter, which the load checks.
A variable or a secret is the catalog call `var("NAME")`, `systemVar("NAME")`,
`secret("NAME")` or `systemSecret("NAME")`.

A bare name resolves against the frames first and the roots after. A
statement's name is its value -- a query's rows, a mutation's written row, a
logic's return value, a builtin's result, an action's capability result --
and a name bound in a branch that did not run reads absent. `CheckBody`
refused every name that could not resolve when the body compiled, so no name
is looked up any other way.

---

### 5. Step Registry (`steps/steps.go`)

Every statement compiles to one step (`compiler.CompileBody`); an `if` and a
`switch` flatten into the steps of their branches, each carrying its
branch's condition, so neither has a step type of its own.

| Step type | Written as | Executor |
|-----------|------------|----------|
| `function` | `x := query q(...)`, `mutation m(...)`, `logic l(...)`, `builtin b(...)` | `function.go` |
| `action` | `action a(...) [on surface("...")]` | `action.go` |
| `automation` | `automation s(...)` | `automation.go` |
| `event` | `publish "<topic>" { ... }` | `event.go` |
| `forEach` | `for x in <source> [if <filter>] { }` | `foreach.go` |
| `parallel` | `parallel { branch n { } } [wait any]` | `parallel.go` |
| `block` | one `branch` of a parallel | `statements.go` |
| `expression` | `x := <expression>` | evaluated by the runner itself |
| `return` | `return [<expression>]` | evaluated by the runner itself |

---

## Data Types

### Automation Structure

```go
type Automation struct {
    Name          string           // Unique identifier (camelCase)
    Description   string           // Human-readable description
    Schedule      string           // Cron expression (optional)
    Trigger       *TriggerConfig   // Event trigger (optional)
    Args          *ArgsSchema      // The args contract the trigger payload binds into
    Preconditions []*Precondition  // Checked before the first statement runs
    Steps         []*Step          // The compiled statements, in source order
    Enabled       *bool            // Active flag (default: true since #2604; @disabled clears it)
    Origin        string           // Source file path
}
```

### Step Structure

```go
type Step struct {
    ID         string          // Unique within its list; names it in the run record
    Type       StepType        // function, action, automation, event, forEach, ...
    OnError    ErrorStrategy   // stop (default) or continue
    RetryCount int             // retry(n)
    Condition  string          // The branch condition a flattened if / switch carries
    Binds      string          // The name the statement binds its value under
    Returns    bool            // A `return <call>`: the value ends the body
    Expression string          // An expression step's source

    // Type-specific configuration (one set per step):
    Function   *FunctionStepConfig
    Action     *ActionStepConfig
    Automation *AutomationStepConfig
    Event      *EventStepConfig
    ForEach    *ForEachStepConfig
    Parallel   *ParallelStepConfig
    Block      *BlockStepConfig
}
```

### Execution Result

```go
type AutomationExecution struct {
    ID             string                  // Unique execution ID
    AutomationName string                  // Which automation ran
    Status         string                  // running, completed, failed, cancelled
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
│  @trigger(event="node.created", concept="v1:todos:todo")                        │
│  automation indexTodoOnCreate {                                                 │
│    args {                                                                       │
│      id any                                                                     │
│    }                                                                            │
│    rows := query libraryArtifactBySourceConceptRef(sourceConceptRef: args.id)   │
│    if rows.empty() {                                                            │
│      mutation createArtifact(sourceConceptRef: args.id, ...)                    │
│    }                                                                            │
│  }                                 ← ONE automation (workflow owner)            │
│                                                                                 │
│  query helperValidation { ... }    ← External helper query OK                   │
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

## Writing an Automation

An automation is written in the statement body language -- the full
specification is [Bodies](../../docs/public/language/memql.md#bodies) in the
language reference. The trigger payload is bound into the `args { }` block
and read as `args.<field>`; every call names its kind and its arguments; and
the statements run in the order written:

```memql
/// Every 10 min: mark departed cluster nodes as health='stopped'.
@trigger(schedule="0 */10 * * * *")
automation pruneStaleClusterNodes {
  decide := logic pruneStaleClusterNodes(event: event)
  for node in decide {
    mutation updateNodeHealth(
      id:       node.id,
      health:   "stopped",
      lastSeen: now
    )
  }
}
```

The retired body forms -- `step` blocks, `body { }`, the terse `=> logic`
header, `steps.<id>` reads, `forEach`, `publishEvent(...)`, `partition=` on
`@trigger` and the `@schedule` annotation -- are refused at parse, and
`memqlmigrate --rewrite=bodies` rewrites a tree that still holds them.

### Available Attributes

| Category | Attribute | Arguments | Description |
|----------|-----------|-----------|-------------|
| **Lifecycle** |
| | `@enabled` | none | Accepted no-op; automations are enabled by default (#2604) |
| | `@disabled` | none | Explicitly disables the automation |
| **Documentation** |
| | `@description` | `"..."` | Human-readable description (a `///` doc comment is the usual form) |
| **Triggers** |
| | `@trigger` | `event="..."`, `concept="..."`, `schedule="..."`, `filter=row => ...` | Event- or schedule-based trigger |
| | `@filter` | `row => <predicate>` | Predicate over the triggering row |
| **Other** |
| | `@template` | none | A work-spine template, invoked by the run that names it; carries no `@trigger` |

Automations also accept `@actor` (declares the body reads `actor.*`) and `@mcp`.

**Note:** Automations are **enabled by default** (#2604, the uniform lifecycle ruling); `@enabled` is an accepted no-op and `@disabled` is the off-switch. Seven annotations once tolerated on automations -- `@deprecated`, `@version`, `@timeout`, `@retry`, `@audit`, `@async`, `@rateLimit` -- are **not** valid: the automation runtime never honored them, and they have been refused since #2712. `@timeout`, `@retry`, `@audit` and `@async` are retired, and the refusal names their history (the first three were removed from the function allow-lists in #989); the [Retired](../../docs/public/language/attribute-matrix.md#retired) table of the attribute matrix lists them, with `@schedule` (retired for `@trigger(schedule=...)`, memql#5370). `@rateLimit` is valid only on tools and `@version` only on seeds and concepts, so both are refused here as misplaced, and `@deprecated` is refused as unknown. A retry is written on the call it retries, `retry(n)`.

See also: [`docs/public/language/attribute-matrix.md`](../../docs/public/language/attribute-matrix.md) for the full attribute reference across all function types.

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
| `unified_loader.go` | Slicing automations out of the unified tree |
| `scheduler.go` | Cron and event triggering |
| `executor.go` | Opening and driving a run |
| `sequence.go` | Running a statement body in order |
| `statement_scope.go` | Names and values in a statement body |
| `run_state.go` | A run's bound state (the Evaluator) |
| `journal.go` | The work journal |
| `resume_statements.go` | Resuming a statement body from its journal |
| `logic_statements.go` | A logic's statement body |
| `steps/steps.go` | Step executor registry |
| `steps/function.go` | A construct call |
| `steps/action.go` | An action call |
| `steps/event.go` | `publish` |
| `steps/foreach.go` | `for` |
| `steps/parallel.go` | `parallel` |
| `steps/statements.go` | The lists a `for` and a `parallel` hold |

---

*For engine architecture, see [`component/memql/arch.md`](../memql/arch.md)*
*For functions architecture, see [`docs/public/language/functions.md`](../../docs/public/language/functions.md)*
*For system-wide architecture, see [`docs/public/concepts/architecture.md`](../../docs/public/concepts/architecture.md)*
