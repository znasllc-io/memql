# DSL v1 loop protection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. This plan is deleted in the epic's merge.
>
> **Resuming?** Read `2026-09-14-dsl-v1-loop-protection-checkpoint.md` beside this file first, when it exists: where the branch stands, what the peers owe, and what comes next.

**Goal:** Automations cannot loop unbounded. At load, the engine builds one directed graph over every automation, from the concepts each writes and the events each triggers on. It assigns strata, and refuses any cycle that no `@loop` annotation covers. At run time, every event carries a causation id, a correlation id and a chain depth. A chain deeper than the cap stops as a `loop_depth_exceeded` run failure that records the chain. The dedup key stops hashing the whole payload, each (automation, row) pair gets its own budget, and every automation can declare a `mode`. An automation that only adjusts fields on its triggering row can declare a before-write body, so no second write or second event exists. The stopped-loop counter, the graph in the architecture model, an OS surface and corpus scenarios make all of this visible (epic memql#5380, tasks #5381-#5384).

**Architecture:**
- `component/events` gains `Cause` (causation, correlation, depth, chain) on the `Event` envelope and in a context value. The internal mesh protos (`node.proto` `EventForward`, `bus.proto` `EventPublish`) carry it across the node hop.
- `component/automations` computes a run's cause at start from its triggering event, or from the context for a sub-automation. It refuses a run past the cap, stamps the cause into the step context, and every publisher reads it from there: the engine's graph-write events, the `publish`/event step, and the executor's own lifecycle events.
- The static graph is a pure build over the loaded automations plus the engine's function registry, and it is `work.UnionFootprint`'s first production caller. It is run by the automation loader (strict boot), by `memqllint`, by the corpus runner, by `cmd/memql-arch` and by a new `automationGraph` builtin that the OS Cluster app draws.
- The before-write body is an epic-3 statement position. `executeWrite` applies it in-process, so it waits for epic 3 (#5370) to merge.

**Tech Stack:** Go 1.26 multi-module workspace; protobuf (`scripts/dev/proto-gen.sh`); PostgreSQL 16 + TimescaleDB for db-gated tests; React + TypeScript (Vite, vitest) for `clients/os`.

**Spec:** `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`: section 2.7 (loops), D3, D18, D19, D20, section 4 epic 5, section 5 (cross-cutting rules), section 6 ("static loop analysis that cries wolf"). Read those before any task. The decisions below make the record concrete where it left a choice open. They are as binding as the record, and the PR body names each one.

## Global Constraints

- **Delivery**
  - One PR from branch `epic/dsl-v1-loop-protection` into `main`. Worktree: `/home/znas/memql-projects/epic-dsl-v1-loop-protection`.
  - The PR closes #5380, #5381, #5382, #5383 and #5384, with one `Closes #n` line each (`Closes #a, #b` links only the first).
  - This plan and its checkpoint are deleted in the PR's last commit.
- **Base and sequencing**
  - The base is `origin/main` at `a0e44069a`, with epics 1 and 2 merged.
  - Epic 3 (#5370, memql-22, local branch `tmp/dsl-v1-bodies-flip2`) is NOT merged, and this epic depends on it (record section 8).
  - Phase A (Tasks 1-12) builds on `main` as it stands.
  - Phase B (Tasks 13-17) starts only after epic 3 is on `origin/main`. It begins by merging `origin/main` into this branch.
  - Never enqueue before epic 3 merges.
  - Never edit memql-22's worktrees (`/home/znas/memql-projects/epic-dsl-v1-bodies`, `/tmp/claude-1000/.../c91c208e-*/scratchpad/wt-*`).
- **No backwards-compat shims** (repo rule 3). A refusal names the construct, its position when there is one, and the fix, and ends with the rule id in square brackets: `... [loop_cycle]` (D24). The corpus reads the code with `\[([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\]\s*$`.
- **Values, not constants** (record section 5). The depth cap, the per-row budget and the queued-mode default are env values:
  - They are registered in `scripts/secrets/manifest.yaml` (component `safety`, scope `node`, `optional: true`) and synced with `make env-registry-sync`; `make env-registry-check` must pass.
  - They are documented in `docs/public/ai/llm-cost-control.md`'s automation table and `docs/public/operate/env-vars.md` if it lists the budget.
  - Defaults: `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH=16`, `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW=30` (per budget window), `MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX=10`.
- **Multi-node is the default** (root CLAUDE.md). The cause crosses the mesh on the proto, and a hop test proves it (Task 1). Modes and budgets are per process, like the budgets that exist today, and the docs say so.
- **Wire**
  - No client wire change: `component/grpc/memql.proto` is untouched.
  - The mesh protos (`node.proto`, `bus.proto`) are internal to one engine version. Their new fields are additive and optional, so a mixed-version rollout reads a zero cause from an old node.
  - Regenerate with the PRIMARY tree's protoc plugins (memory: proto-gen plugin cache is per worktree). Before regenerating, copy `/home/znas/memql-projects/memql/bin/tools/protoc-plugins-*/protoc-gen-go{,-grpc}` into this worktree's `bin/tools/protoc-plugins-*/`. After regenerating, `git diff --name-only -- component/*/gen` must list only the two edited protos' outputs.
- **Verify with the MODULE PATH**, never `go test ./...`:
  - `go test github.com/znasllc-io/memql/component/automations/...`, `.../component/events/...`, `.../component/node/...`, `.../component/memql/...`, `.../component/language/...`, `.../test/conformance/...`, then `make test`.
  - Db-gated: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql_loops?sslmode=disable' go test -count=1 -p 1 <pkgs>`. Create the database once: `docker exec memql-throwaway-laneb psql -U memql -d postgres -c "CREATE DATABASE memql_loops OWNER memql;"`. It is a private database in the shared container, and memql-22 uses its own container on 55434.
- **Root gates**: run `go test -count=1 .` and `go test -count=1 ./scripts/ci/...` after adding any file, AFTER `git add`, because several gates walk `git ls-files`.
- **Staging and style**
  - Stage files by explicit path; never `git add -A` or `git add .`.
  - No emojis.
  - `gofmt -w` only files you touched.
  - Never run prettier (the OS is hand-formatted).
- **Commits**: `Issue #<N>: <description>`, ending with the attribution trailer from the session reminder:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Cpiovt79hsMfKyyoeD1RbE
  ```
- **The arch model** (`component/architecture/embedded/topology.model.json`): regenerate it LAST in each phase (`make arch-model`). On a conflict, take one side wholesale and regenerate. Abort if the file exceeds 70MB.
- **Do not touch** the running k3d cluster `memql`.
- **Shared files with memql-22 (epic 3)**
  - Their F5 rewrites `component/automations/executor.go`, `loader.go`, `types.go`, `resume.go`, `args_resolution.go`, `logic_runner.go`, `statement_scope.go` and `steps/sandbox_registry.go`, and deletes `steps/switch.go`. They edited `component/language/annotations/registry.go` and `retired.go`.
  - Keep this epic's changes to those files to call sites and additive fields. New logic goes in NEW files named `loop_*.go`, `mode_*.go` and `before_write*.go`. On the Phase B merge, take THEIR side of each and re-apply the call sites.
  - Name nothing that epic 3 deletes: `StepTypeSwitch`, `SwitchStepConfig`, `LogicSteps`, `compileBodyToAutomation`.
- **Grammar version**: epic 1's D25 parity gate (`cmd/memql-lsp/editorparity_test.go`) requires these to move together in one commit: `parser.GrammarVersion` (`component/language/parser/grammar_version.go`, format `<year>.<month>-<epic-slug>-<8 hex surface digest>`), `editors/vscode/package.json` `memql.grammarVersion` and `version`, `editors/vscode/CHANGELOG.md`, and `parser.EditorRelease` (`component/language/parser/edition.go`). Phase A moves it once (Task 3). Phase B moves it again on top of epic 3's (Task 14).

---

## Decisions this plan makes (the record's open choices, made concrete)

### D-A. Depth, correlation, chain

- **Depth**
  - A ROOT event (a person's write, a Go-side publish with no automation behind it, a cron tick) has depth 0.
  - A run's depth is its parent's depth plus 1, so the first automation a user write triggers runs at depth 1.
  - Every event a run causes carries the run's depth.
  - A run whose depth would exceed the cap (`MEMQL_AUTOMATION_MAX_CHAIN_DEPTH`, default 16) is refused, which is the record's "a chain of 17 fires is stopped at 16".
- **Parent.** The parent is the triggering event's cause. With no triggering event, it is the context's cause: a sub-automation call runs inside its caller's chain and counts as a run. A logic call is a statement of its run, not a run, and does not count.
- **Correlation**
  - The chain's correlation id is inherited from the parent.
  - A root event carries none. The executor derives it deterministically as `"evt-" + fingerprint(topic, kind, payload)`, which every replica computes identically from the same event.
  - A run with neither an event nor a context cause (cron, manual) takes its own run id as its correlation.
- **Chain.** `[(automation, runId), ...]`, oldest first: every run from the root to here. It is capped by the depth cap, so it never exceeds 16 entries.
- **Causation.** Each event's causation id is the run id whose work published it.

### D-B. Where the cause travels

- In-process, it travels in `events.Event.Cause` and in the context. Across the mesh, it travels on typed fields of `EventForward` and `EventPublish`.
- **Durable run-delivery substrate** (`component/node/delivery_substrate.go` `Deliverable`, `mesh_outbox`): not plumbed. It carries `v1:work:*` journal events, and an event that arrives through it arrives as a root: a chain BREAK, never a false stop. The docs say so.
- **Go-side `bus.Subscribe` handlers** that write (about 25): not plumbed either, for the same reason. The per-row budget (D-F) is the backstop for a loop that escapes the chain through them, through an external round trip, or through an async boundary.

### D-C. The failure a depth refusal records

A refused run writes one `v1:work:run` row at `failed`:

- `errorCode: "loop_depth_exceeded"`
- an `errorMessage` naming the depth, the cap and the chain
- `outcome: {executorStatus: "failed", loop: {reason, depth, cap, correlationId, chain: [{automation, runId}...]}}`
- `triggerEvent.cause` holding the parent cause

It bypasses the failure classifier, because a loop is terminal by construction. `component/work/terminal.go` gains `TerminalLoopDepthExceeded = "loop_depth_exceeded"`, so a PARENT run whose sub-automation step was refused also lands terminal, with that code.

- An automation the journal skips (`journalSkipsAutomation`) records no row. It logs a WARN with the chain and counts the stop.
- The refusal happens AFTER the dedup and cluster-guard claim, so one event refused on two replicas writes one row.
- `reason` is one of two values:
  - `depth`: the global cap.
  - `loop_bound`: the `@loop` maxDepth, D-H.

### D-E. The dedup key

- **What is hashed.** `ComputeInitialChainHead` hashes `{automation, triggeredBy, eventTopic, correlation, projection}`.
  - The projection is the payload restricted to `Automation.Reads` when the automation is EVENT-PURE: every step it reaches, transitively through logic, is a mutation, a publish, an expression or a logic/query call that reads nothing but its arguments.
  - Otherwise it is the whole payload, minus the top-level `createdAt` on `graph.node.*` topics. That field is the version clock, and it is removed surgically, per the comment on `eventFingerprintData`.
  - `Reads` are the declared `args` fields plus every `row.<f>` in the `@filter` plus every `args.<f>` / `event.payload.<f>` in the steps. A whole-`event` reference makes the automation not event-pure.
- **Why the correlation is in the key.**
  - A root's correlation is the event's own fingerprint, so a root keeps today's exactly-once-per-event identity.
  - A chained event dedups only against its own chain: an echo, meaning a rewrite that changes nothing the automation reads, is skipped.
  - A row that returns to an earlier state from a NEW cause, such as a person toggling a status twice, still fires.
  - Without the correlation, the cluster guard's one-hour, once-ever claim would drop that second toggle cluster-wide. That is the correctness bug the literal reading of the record would ship.
- **The acceptance case**, "two writes differing only in createdAt dedup to one execution", is therefore stated within one chain.
- **The per-process dedup now registers at START** (in flight), keeps the entry on success, and drops it on failure or skip. A writer's echo that arrives while the run is still executing is caught.
- **Counting echoes.** A duplicate whose event identity (the old whole-payload fingerprint) differs from the registered one is an echo, and is counted (`reason="echo"`). A redelivery of the same event is not counted.

### D-F. The per-(automation, row) budget

- The default is 30 executions per (automation, row id) per budget window (60s), set by `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW`. It is enforced per process, beside the global and per-automation budgets.
- The row id is `payload.nodeId`, else `payload.id`, on `graph.node.*` topics only. Any other topic has no row and no row budget.
- Stale entries are pruned when the window rolls, so the map is bounded by rows active in one window.
- Exceeding the budget skips the run with status `skipped`, logs an ERROR once per window, and counts `reason="row_budget"`.

### D-G. Modes

**Forms.** `@mode(single)`, `@mode(queued)`, `@mode(queued, max=N)`, `@mode(restart)`, `@mode(parallel)`, `@mode(parallel, max=N)`. With no `@mode`, an automation is unbounded parallel, which is today's behaviour.

**What each mode does:**

| Mode | While a run of this automation is in flight in this process |
|---|---|
| `single` | A new fire is refused (`skipped`, WARN naming the in-flight run, `reason="mode"`). |
| `queued` | A new fire waits in FIFO order. More than `max` waiting (default `MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX`, 10) refuses. |
| `restart` | A new fire cancels the in-flight run's context, which then records `cancelled`, and starts. |
| `parallel` | A new fire beyond `max` concurrent runs is refused. |

**Gate placement.** The mode gate runs before the executor's concurrency slot, so a queued run holds no slot while it waits.

**What the load refuses:**
- `max` on `single` or `restart`
- two mode flags in one annotation
- `max < 1`

**Scope.** A mode is per process, per automation, and applies to sub-automation invocations too.

### D-H. `@loop(maxDepth=N, until=row => P)`

**Where it may appear.** Only on an event-triggered automation (`@trigger(event=...)`), with `N` an integer from 1 to the cap.

**The filter must exclude converged rows.** The automation's `@filter` must hold the negation of `P` as a top-level conjunct: either `!(P)`, or, when `P` is one comparison, its inversion (`==`/`!=`, `<`/`>=`, `>`/`<=`). The until lambda's parameter is renamed to the filter's before comparing. The refusal prints the exact conjunct to add.

**When a cycle is permitted.** A cyclic SCC is permitted iff removing its `@loop` automations leaves it acyclic: every cycle passes through at least one `@loop` automation.

**At run time.**
- A run of a `@loop` automation is refused (`loop_depth_exceeded`, `reason="loop_bound"`) when the chain already holds `N` runs of it.
- A `@loop` on an automation in no cycle is a load WARN, because it still bounds the automation at run time.
- A converging loop stops because the filter no longer matches. That is not a stop and is not counted.

### D-I. The static graph's edges

**The rule.** `A -> B` iff one of A's writes produces a topic B's trigger pattern matches (`events.Match`), and B's `@filter` is not refuted by what the write is known to set. Every edge carries its reason, and the "via" call path that reaches the write.

**What produces a topic:**

| A's write | Topics produced |
|---|---|
| Insert mutation | `graph.node.created.<C>` |
| Update mutation | `graph.node.created.<C>` and `graph.node.updated.<C>` |
| `publish "t"` / event step with a literal topic | `t` |
| Event step whose topic is an expression | Matches every non-graph trigger, reason "topic known only at run time" |

**What A's writes include.**
- Mutations a step calls directly, and through logic, transitively (`work.UnionWrites`).
- Through sub-automations, transitively: their writes and publishes.
- Builtins and actions are OPAQUE: no edges, and they are listed on the node.
- The engine's own journal writes (`v1:work:*`) are not modelled. `journalSkipsAutomation` is their guard.

**What is known about a written row:**
- Fields the mutation template sets to a literal.
- Fields set to `args.<x>` where the call site passes a literal for `x`. This propagates one level, from an automation step to a mutation, not through logic parameters.
- `row.concept` is always `C`.
- An update: `firstVersion` is false and fields it does not set are unknown.
- An insert whose template names no id: `firstVersion` is true and fields it does not set are absent.
- An insert with an id: `firstVersion` and the fields it does not set are unknown.

**Deciding the filter.** It is evaluated three-valued (true / false / unknown), with D8's absence table (`!=` is true on absent). Only `false` removes an edge. An `unknown` edge is kept, and its reason names the filter it could not read and why.

**Strata.** Strata are Datalog-style: the longest-path layer of the condensation DAG, where all members of one SCC share a stratum. Scheduled, template and raw-topic automations nothing publishes are sources.

**Refusal codes:**
- `loop_cycle`: an uncovered cycle. It prints one representative path, one line per edge with its reason, and the fix.
- `loop_until_not_in_filter`
- `loop_not_event_triggered`
- `loop_max_depth_range`
- `loop_until_form`: until is not a one-parameter lambda
- `mode_flags`: zero or two mode flags
- `mode_max_not_allowed`
- `mode_max_range`

### D-J. Where the graph check runs

1. **Strict boot.** The automation loader runs it after the tree walk, in the same problem list: phase `loops`, refused unless `MEMQL_DSL_ALLOW_SKIPS`. `LoaderOptions.Functions` is required for the check, and `app/engine.go` passes `a.engine.Functions()`.
2. **`memqllint`** in directory mode, over the mounted bundle plus the embedded tree, so a product bundle's CI sees the refusal.
3. **The corpus runner.**
4. **`AuthoredScheduler.Activate`**, over the shipped automations plus the candidate. An authored automation that closes an uncovered cycle is refused at activation.

A loader with no function registry reports the check as not run (a log line plus `GraphCoverage`), never as clean.

### D-K. Architecture model

- Kind `"automation"` (id `automation:<name>`, parent the cluster node) and edge kind `"triggers"`.
- Node attrs: `stratum`, `trigger`, `filter`, `loop`, `mode`, `writes`, `origin`.
- Edge attrs: `concept`, `topic`, `decided`, `via`.
- `cmd/memql-arch --automations` adds them from the embedded tree through an offline engine. The Makefile's `arch-model` passes the flag.
- The staleness gate's CI path filter and gate-input rows gain `dsl/**/automations.memql`, `dsl/**/mutations.memql` and `dsl/**/logic.memql`.

### D-L. The OS surface

- A new Cluster app section, `automations`, "Automations", `requires: "app:cluster/automations"`, seeded `read` for owner, developer and admin.
- It is fed by the `@sdk` builtin `automationGraph` (`@requiresCapability("read", "app:cluster/automations")`). It returns one virtual `v1:platform:automationNode` row per automation, the dataOrigins pattern, plus `automationLoopStops`: the latest 50 `loop_depth_exceeded` runs, projected without payloads, read in Go under a cluster-owner context after the capability check.
- It is read on demand with `useReading`, not a live feed, because nothing broadcasts the graph.
- It is drawn as plain SVG, one column per stratum, with the frontend-design skill (Task 12). No WebGL.

### D-M. The before-write body (Phase B; shape reviewed by the epic-3 session on 2026-09-14)

The timing goes on the TRIGGER, because it is WHEN the automation runs (D15 keeps triggers as annotations). A `before write { }` wrapper block is refused, as epic 3's owner ruling refused the `body { }` wrapper. The body is the ordinary statement list. The one new statement kind is the field write `row.<field> = <expr>`: `:=` binds a name, `=` writes a field of the row being written.

```memql
@trigger(before="create", concept="v1:forge:request")
automation routeRequest {
  decided := logic requestRouteStatus(submitterRole: row.submitterRole)
  row.status = decided
  if decided == "queued" {
    row.approvedByUserId = row.submitterUserId
  }
}
```

**`@trigger(before=..., concept=...)`.** `before` is `"create"`, `"update"` or `"write"`. This is a new `@trigger` key: the registry's `triggerKeys` gains `before`, and `before` excludes `event` and `schedule`.
- `create`: the write materializes the row's first version.
- `update`: a prior version exists (an `update()`, or an insert onto an existing id).
- `write`: both.
- An optional `@filter(row => ...)` is evaluated against the incoming row, before the body runs.

**What the body may contain:**
- binds to an expression, or to a `logic`/`query` call whose transitive footprint writes nothing
- `if` / `else`
- `return`, which ends the body
- field writes `row.<field> = <expr>`

`row` is a root only in a before-write automation. It is the row as it will be written: the read-merged payload plus this write's delta, with the earlier field writes applied. `args`, when declared, bind from that same payload.

**Refusal codes:**
- `before_write_writes`, naming the call: a mutation, builtin, action, automation or publish anywhere in the body. So a before-write body that writes another concept refuses at load, per the acceptance criteria.
- `before_write_field`: an intrinsic, a nested path, or a field the trigger concept does not declare.
- `before_write_outside`: a field write in an automation that is not before-write.
- `before_write_trigger`: `before` combined with `event` or `schedule`, or `before` with no `concept`.

**When it runs.**
- In-process, inside `executeWrite` on the node doing the write, after the read-merge and before schema validation. It runs on every write to the concept whose kind matches `before` and whose `@filter` holds. Matching automations run in name order.
- One row version is written and one event is published. There is no `v1:work:run` row, because that would be a second write.
- The automation is never subscribed on the bus.

**The breadth of a new statement kind.** It touches every walker that switches on `ast.BodyStatementKinds`:
- `ast/body_test.go` and `compiler/body_compile_test.go` pin the set.
- `compiler/body_scope.go` and the body compile.
- `component/memql/callgraph` (`statementConditions` and the D14 table).
- `component/automations` (`validateSteps`, step types).
- sense (`complete_statements.go`), memql-lsp, and dslspec (lexicon, constructs, nextrules).
- `grammar_surface_drift_test.go` (a corpus entry per new legal form) and the GrammarVersion digest.
- `component/language/tiers`: the field-write value is a new expression position. It needs a manifest row plus its `writtenAs` text (`tiers/docs_test.go`), then `-update-docs` for the memql.md region.

Parser hook points:
- `v1_body.go` `parseV1Statement` sends an identifier to `parseV1Assign`. `row` followed by `.` parses the member path and requires `=`.
- `body_scope.go` adds `row` to the roots when the construct is before-write, and refuses a field write elsewhere.
- The body compile emits a new step type carrying the field plus an expression leaf.

**Journal shape: settled in Task 14, and the PR says which.**
- The adjusted row carries the adjustment in its row metadata (`component/metadata` LineageMeta, if that column exists): automation name plus fields set.
- Otherwise it goes to the log store through `logger.Subject(concept, id)`.
- Both come with `memql_automation_before_writes_total{automation}`.

**Every node that writes must hold the before-write registry.** Task 14 verifies which node types load automations, and registers the bodies on the engine at `Init`.

---

## Phase A / Phase B, and parallel streams

| Task | Needs | Phase |
|---|---|---|
| 0 Scaffold shared types | nothing | A (coordinator, first) |
| 1 Cause on the envelope and across the mesh | 0 | A, stream 1 |
| 2 Stamping at every publisher | 1 | A, stream 1 |
| 3 `@loop` and `@mode`: the surface | 0 | A, stream 2 |
| 4 The run's chain, the depth cap, the journaled refusal | 1, 2, 3 | A, stream 4 |
| 5 The dedup key | 4 | A, stream 4 |
| 6 The per-row budget | 4 | A, stream 4 |
| 7 Modes at run time | 3, 4 | A, stream 4 |
| 8 The static graph and its refusal | 0, 3 | A, stream 3 |
| 9 The tree's cycles fixed; the refusal flipped on | 8 | A, stream 3 |
| 10 memqllint and the corpus runner run the graph | 8 | A, stream 3 |
| 11 The architecture model renders the graph | 8 | A, stream 5 |
| 12 The OS surface: Cluster, Automations | 4, 8 | A, stream 5 |
| 13 Merge epic 3; the graph walks the statement form | epic 3 on main | B |
| 14 The before-write body | 13 | B |
| 15 Loop scenarios in the corpus | 13, 14 | B |
| 16 Documentation | all | B |
| 17 Verification and delivery | all | B |

- Streams 1, 2 and 3 run in parallel, in separate worktrees branched from the scaffold commit. The coordinator merges them into `epic/dsl-v1-loop-protection`.
- Streams 4 and 5 start after that merge.
- File ownership across streams:
  - **Stream 1:** `component/events`, `component/node`, `component/bus`, and the publish sites in `component/memql/executor_mutation.go` / `engine.go` and `component/automations/steps/event.go`.
  - **Stream 2:** `component/language/**`, the automation fields and prepare/validate code, the corpus cells, `editors/vscode`.
  - **Stream 3:** `component/work/footprint.go`, `component/automations/loop_graph*.go`, `loader.go`/`unified_loader.go` call sites, `app/engine.go` loader options, `dsl/**` fixes, `cmd/memqllint`, `component/memql/lint_parity.go`, `test/conformance/corpus_test.go`.

---

## File Structure

| Path | Responsibility | Task |
|---|---|---|
| `component/events/cause.go` (new) | `Cause`, `Link`, the context value, map round-trip, `Event.WithCause` | 0, 1 |
| `component/events/event.go` | `Event.Cause` field; `Clone` copies it | 1 |
| `component/node/node.proto`, `component/node/gen/*` | `EventForward.cause` (`EventCause`, `EventCauseLink`) | 1 |
| `component/node/eventbridge.go`, `eventbridge_bus.go` | carry the cause out, in, and on the relay | 1 |
| `component/bus/bus.proto`, `component/bus/gen/*`, `component/events/bus_channel.go` | `EventPublish.cause` and its decode | 1 |
| `component/memql/engine.go`, `executor_mutation.go` | graph-write events carry the context's cause | 2 |
| `component/automations/steps/event.go` | the published event carries the context's cause | 2 |
| `component/language/annotations/registry.go` | `@loop`, `@mode` placements, keys, docs | 3 |
| `component/language/ast/ast.go`, `parser/ast.go`, `parser/parser.go`, `parser/v1_positions.go` | attr names, `AutomationDef.Loop/Mode`, lambda for `until=` | 3 |
| `component/language/compiler/automation_generator.go` | emit `loop`, `mode` | 3 |
| `component/automations/types.go` | `LoopConfig`, `ModeConfig`, `Automation.Loop/Mode/Reads` | 0, 3, 5 |
| `component/automations/loop_prepare.go` (new) | parse `until` once; `@loop`/`@mode` load validation | 3 |
| `component/automations/loop_runtime.go` (new) | the run's cause, the cap, `LoopRefusal` | 4 |
| `component/automations/executor.go` | call sites: cause at start, refusal after the claim, stamp ctx, mode gate, row budget | 4-7 |
| `component/automations/journal.go`, `journal_loop.go` (new) | `refuseLoop`, `triggerEvent.cause` | 4 |
| `component/automations/resume.go`, `adopt.go` | restore the cause from `triggerEvent.cause` | 4 |
| `component/work/terminal.go` | `TerminalLoopDepthExceeded` | 4 |
| `component/metrics/automations.go` (new) | `memql_automation_loops_stopped_total{automation,reason}` | 4 |
| `component/automations/fingerprint.go`, `dedup.go`, `loop_reads.go` (new) | the narrowed, chain-scoped key; in-flight registration; `Reads` | 5 |
| `component/automations/budget.go` | per-row window | 6 |
| `component/automations/mode_gate.go` (new) | the four modes | 7 |
| `component/work/footprint.go` | `FieldValue`, `WriteSpec`, `Write`, `UnionWrites`, `Target.Write` | 8 |
| `component/automations/loop_graph.go`, `loop_graph_filter.go`, `loop_graph_scc.go`, `loop_graph_report.go`, `loop_graph_source.go` (new) | build, decide filters, SCC/strata/coverage, refusal text, the function-registry adapter | 8 |
| `component/automations/loader.go`, `unified_loader.go`, `authored_scheduler.go`, `app/engine.go` | `LoaderOptions.Functions`, the check in the walk and at activation | 8, 9 |
| `dsl/forge/automations.memql`, `deploy/fleet/dsl/fleet/automations.memql`, `examples/deploypack/dsl/automations.memql` (+ any the measurement finds) | cycles fixed; stale "no cycle" prose corrected | 9 |
| `component/memql/offline_engine.go` (new), `lint_parity.go`, `cmd/memqllint/main.go`, `test/conformance/corpus_test.go` | offline engine helper; the automations pass with functions | 10 |
| `component/architecture/model/*`, `cmd/memql-arch/*`, `Makefile`, CI path filter + gate-input rows | the graph in the model | 11 |
| `dsl/platform/concepts.memql`, `dsl/common/builtins.memql`, `component/memql/automation_graph_read.go` (new), `component/automations/scheduler.go` | `automationNode` virtual concept, the two builtins, the graph-source seam | 12 |
| `dsl/rbac/seeds.memql`, `clients/os/src/apps/cluster/**`, `clients/os/src/cluster/automations/**` (new) | the section | 12 |
| `component/language/parser/v1_body.go`, `compiler/body_scope.go`, `compiler/body_compile.go`, `component/memql/before_write.go` (new), `executor_mutation.go` | the before-write body | 14 |
| `test/conformance/2026/scenarios/loops/**` | the three shapes as scenarios | 15 |
| `docs/public/language/memql.md`, `authoring-rules.md`, `attribute-matrix.md`, `docs/public/concepts/events.md`, `docs/public/ai/llm-cost-control.md`, `docs/public/operate/env-vars.md` | documentation | 16 |

---

## Task 0: Scaffold the shared types (coordinator, before the streams)

**Files:**
- Create: `component/events/cause.go`
- Modify: `component/automations/types.go` (add `LoopConfig`, `ModeConfig`, `Automation.Loop`, `Automation.Mode`, `Automation.Reads`)

**Interfaces — Produces** (every later task imports these names exactly):

```go
// component/events/cause.go
package events

import "context"

// Cause is an event's causal lineage (D19): the automation run whose work
// published it, the chain it belongs to, and how deep that chain is. The zero
// value is a ROOT event: a person's write, a cron tick, anything no automation
// run caused.
type Cause struct {
	// CausationId is the run id of the automation run that caused the event.
	CausationId string `json:"causationId,omitempty"`
	// CorrelationId is the chain's id, shared by every event and run the same
	// root cause led to.
	CorrelationId string `json:"correlationId,omitempty"`
	// Depth is how many automation runs stand between the root cause and this
	// event. 0 for a root event.
	Depth int `json:"depth,omitempty"`
	// Chain names those runs, oldest first. At most the depth cap long.
	Chain []Link `json:"chain,omitempty"`
}

// Link is one run in a chain.
type Link struct {
	Automation string `json:"automation"`
	RunId      string `json:"runId"`
}

// IsZero reports whether no run caused the event.
func (c Cause) IsZero() bool {
	return c.CausationId == "" && c.CorrelationId == "" && c.Depth == 0 && len(c.Chain) == 0
}

// Clone returns a copy whose Chain shares no backing array with c.
func (c Cause) Clone() Cause {
	if c.Chain != nil {
		c.Chain = append([]Link(nil), c.Chain...)
	}
	return c
}

// Next is the cause a run carries: the parent's chain plus this run, one
// deeper, with this run as the causation.
func (c Cause) Next(automation, runId, correlationId string) Cause {
	chain := make([]Link, 0, len(c.Chain)+1)
	chain = append(chain, c.Chain...)
	chain = append(chain, Link{Automation: automation, RunId: runId})
	return Cause{CausationId: runId, CorrelationId: correlationId, Depth: c.Depth + 1, Chain: chain}
}

type causeKey struct{}

// ContextWithCause returns ctx carrying c.
func ContextWithCause(ctx context.Context, c Cause) context.Context {
	return context.WithValue(ctx, causeKey{}, c.Clone())
}

// CauseFromContext returns the cause ctx carries, if any.
func CauseFromContext(ctx context.Context) (Cause, bool) {
	if ctx == nil {
		return Cause{}, false
	}
	c, ok := ctx.Value(causeKey{}).(Cause)
	return c, ok && !c.IsZero()
}
```

```go
// component/automations/types.go (added beside TriggerConfig)

// LoopConfig is @loop(maxDepth=N, until=row => P) (D18, D-H of the loop
// protection plan): the automation closes a deliberate cycle, stops when P
// holds on its triggering row, and runs at most MaxDepth times in one chain.
type LoopConfig struct {
	MaxDepth    int             `json:"maxDepth"`
	Until       string          `json:"until"`
	UntilLambda *ast.LambdaExpr `json:"-"`
}

// ModeConfig is @mode(<kind>[, max=N]) (D19, D-G).
type ModeConfig struct {
	Kind string `json:"kind"` // single | queued | restart | parallel
	Max  int    `json:"max,omitempty"`
}

// Mode kinds.
const (
	ModeSingle   = "single"
	ModeQueued   = "queued"
	ModeRestart  = "restart"
	ModeParallel = "parallel"
)
```

Add to `Automation` (after `Template`): `Loop *LoopConfig \`json:"loop,omitempty"\``, `Mode *ModeConfig \`json:"mode,omitempty"\``, and `Reads []string \`json:"-"\`` (doc: "the payload fields the dedup key keeps (loop_reads.go); nil keeps the whole payload but its version clock").

- [ ] **Step 1:** Write both files as above. Add `component/events/cause_test.go`, covering the zero value `IsZero`, `Next` appending one link and incrementing depth without aliasing the parent's chain (mutate the parent's slice after `Next`, assert the child unchanged), and the context round trip returning `false` for a zero cause.
- [ ] **Step 2:** `go test github.com/znasllc-io/memql/component/events/ -run TestCause` passes; `go build github.com/znasllc-io/memql/component/automations/...` passes.
- [ ] **Step 3:** Commit `Issue #5382: the cause type and the loop and mode fields every stream builds on`.

---

## Task 1: The cause on the envelope, and across the mesh (#5382)

**Files:**
- Modify: `component/events/event.go` (`Cause Cause` field on `Event`; `Clone` copies `e.Cause.Clone()`), `component/events/cause.go` (`func (e Event) WithCause(c Cause) Event`)
- Modify: `component/node/node.proto` (message `EventCause {string causation_id = 1; string correlation_id = 2; int32 depth = 3; repeated EventCauseLink chain = 4;}`, `EventCauseLink {string automation = 1; string run_id = 2;}`, `EventForward.cause = 8`), regenerate `component/node/gen`
- Modify: `component/node/eventbridge.go` (`onLocalEvent` sets `Cause`; `HandleInbound` rebuilds `Event.Cause`; `ForwardInboundToPeers` copies the field), `component/node/eventbridge_bus.go` (`publishViaBus` sets `EventPublish.Cause`)
- Modify: `component/bus/bus.proto` (the same two messages, `EventPublish.cause` at the next free number), regenerate `component/bus/gen`; `component/events/bus_channel.go` (`handleChannelMessage` decodes it)
- Create: `component/events/cause_proto.go` if a shared converter keeps both bridges honest (it may not import the node gen; converters live beside each proto's user)
- Test: `component/events/cause_test.go` (Clone), `component/node/eventbridge_cause_hop_test.go` (new)

**Interfaces:**
- Consumes: Task 0's `events.Cause`, `events.Link`.
- Produces: `Event.Cause` survives `Clone`, `Bus.Publish` fan-out, `EventForward` out and in, the relay, `EventPublish` and `handleChannelMessage`.

- [ ] **Step 1: Failing tests.**
  - In `cause_test.go`: `Event.Clone` returns an equal `Cause` whose `Chain` does not alias the original.
  - In `eventbridge_cause_hop_test.go`, model the test on the existing bridge tests in `component/node` (grep `func TestEventBridge`):
    - Node A's bridge receives a local event carrying `Cause{CausationId:"run-1", CorrelationId:"evt-x", Depth:3, Chain:[{a,run-0},{b,run-1}]}` on a topic its routing forwards.
    - Capture the `EventForward` it sends and feed it to node B's `HandleInbound`.
    - Subscribe on B's bus and assert the delivered event's `Cause` equals the original: depth, correlation, causation, chain order.
    - Also cover the relay: `ForwardInboundToPeers` preserves it with `Ttl-1`.
    - Also cover the channel path: an `EventPublish` decoded by `handleChannelMessage` keeps it.
  - Negative control: comment out the `Cause` line in `onLocalEvent`, run, see the test fail naming depth 0, restore, `cmp` the file against a scratchpad copy.
- [ ] **Step 2:** Run `go test github.com/znasllc-io/memql/component/node/ -run Cause` and `go test github.com/znasllc-io/memql/component/events/`. Expect FAIL.
- [ ] **Step 3: Implement.**
  1. Copy the primary tree's protoc plugins (Global Constraints). Edit both protos and run `scripts/dev/proto-gen.sh`.
  2. Check `git diff --name-only -- component/*/gen`: it must list only `component/node/gen/node*.pb.go` and `component/bus/gen/bus*.pb.go`. If it lists more, the plugin disagrees: rebuild per memory `proto-gen-check-green-locally-red-in-ci` (`GOTOOLCHAIN=go1.26.1`).
  3. Convert with two small functions per side: `causeToProto(events.Cause) *nodev1.EventCause` and `causeFromProto(*nodev1.EventCause) events.Cause`. A nil proto gives the zero cause.
- [ ] **Step 4:** Run the tests again (PASS), then `go test github.com/znasllc-io/memql/component/node/... github.com/znasllc-io/memql/component/events/... github.com/znasllc-io/memql/component/bus/...`, then commit. Then run `make proto-gen-check`, which compares against HEAD.
- [ ] **Step 5:** Commit `Issue #5382: the event envelope carries its cause, and the mesh hop keeps it`.

---

## Task 2: Every automation publisher stamps the cause (#5382)

**Files:**
- Modify: `component/memql/engine.go`. `publishEventWithActor` becomes `publishGraphWriteEvent(ctx context.Context, topic string, kind events.Kind, payload map[string]any, actorId string)`: when `events.CauseFromContext(ctx)` holds, `event = event.WithCause(cause)`. Its two call sites in `executor_mutation.go` (`executeWrite`'s `.created`, `executeUpdate`'s `.updated`) pass `ctx`.
- Modify: `component/automations/steps/event.go`. After `events.NewEvent(...)`: `if c, ok := events.CauseFromContext(ctx); ok { event = event.WithCause(c) }`.
- Modify: `component/automations/executor.go`. `publishEvent` takes `ctx` and stamps the same way; update every call site in the file (the lifecycle and precondition events).
- Test: `component/automations/steps/event_cause_test.go` (new, DB-free); `component/memql/graph_write_cause_test.go` (new).

**Interfaces:**
- Consumes: Task 1's `Event.Cause`, `Event.WithCause`, `events.CauseFromContext`.
- Produces: any write or publish made under a context carrying a cause emits an event with that cause. Task 4 relies on this.

- [ ] **Step 1: Failing tests.**
  - `event_cause_test.go`:
    - Build a `steps.EventExecutor`, a bus with a capturing subscriber, a prepared event step (copy the setup from an existing `steps` event test; grep `EventExecutor{}` in `component/automations/steps/*_test.go`), and a ctx with `events.ContextWithCause(ctx, c)`.
    - Execute and assert the captured event's `Cause` equals `c`.
    - A second case with no cause asserts `Cause.IsZero()`.
  - `graph_write_cause_test.go`:
    - Use the lightest existing harness that drives `executeWrite` to a captured `graph.node.created` event. Grep `firstVersion` in `component/memql/*_test.go` for tests that already assert on the published write event and copy their setup. It is db-gated if they are.
    - Assert the `.created` and `.updated` events of an `update()` both carry the cause.
- [ ] **Step 2:** Run the two tests. Expect FAIL.
- [ ] **Step 3:** Implement as listed. Leave `publishEvent` (cache invalidation, query executed) alone: those are not automation consequences, and a cause on `cache.invalidate` would be noise.
- [ ] **Step 4:** Run `go test github.com/znasllc-io/memql/component/automations/steps/ -run Cause`, and the db-gated test with the Global Constraints DSN. Then `go build github.com/znasllc-io/memql/...`.
- [ ] **Step 5:** Commit `Issue #5382: graph writes, publish steps and lifecycle events carry the run's cause`.

---

## Task 3: `@loop` and `@mode`, the surface (#5381, #5382)

**Files:**
- Modify: `component/language/annotations/registry.go`. Add the key sets and the placements beside the other Automation placements, plus the `Docs` entries:

```go
	loopKeys = []ArgSpec{
		{Name: "maxDepth", Type: "int", Doc: "The most runs of this automation one causal chain may hold. An integer from 1 to the depth cap (MEMQL_AUTOMATION_MAX_CHAIN_DEPTH, default 16)."},
		{Name: "until", Type: "expression", Doc: "The convergence predicate: a lambda of one parameter over the triggering row, until=row => row.status == \"done\". The automation's @filter must hold its negation as a top-level conjunct, which is what stops the loop."},
	}
	modeKeys = []ArgSpec{
		{Name: "single", Type: "flag", Doc: "One run at a time in this process; a fire while one runs is refused with a warning."},
		{Name: "queued", Type: "flag", Doc: "Fires wait their turn in order; more than max waiting are refused."},
		{Name: "restart", Type: "flag", Doc: "A fire cancels the run in flight and starts again."},
		{Name: "parallel", Type: "flag", Doc: "Runs concurrently; more than max at once are refused. The default when no @mode is written, with no max."},
		{Name: "max", Type: "int", Doc: "With queued, the most fires that may wait (default 10); with parallel, the most runs at once."},
	}
```

```go
		{Receiver: Automation, Name: "loop", Forms: FormKeywords, Keys: loopKeys, Example: `@loop(maxDepth=4, until=row => row.status == "done")`},
		{Receiver: Automation, Name: "mode", Forms: FormKeywords, Keys: modeKeys, Example: `@mode(queued, max=10)`},
```

  `Docs["loop"]`: "On an automation that closes a deliberate cycle: permits the cycle the load would otherwise refuse, bounds it to maxDepth runs of this automation per causal chain, and names the predicate that ends it. The @filter must exclude the rows where until holds." `Docs["mode"]`: "How concurrent fires of this automation behave in one process: single, queued, restart or parallel, with max for queued and parallel. Without it an automation runs every fire in parallel."
- Modify: `component/language/ast/ast.go` (`AttrLoop = "loop"`, `AttrMode = "mode"`; `AutomationDef.Loop *LoopDef`, `AutomationDef.Mode *ModeDef` with `LoopDef{MaxDepth int; MaxDepthSet bool; Until *LambdaExpr}` and `ModeDef{Flags []string; Max int; MaxSet bool}`), `parser/ast.go` (aliases), `parser/v1_positions.go` (`parseAttributeArgValue`: `attrName == AttrLoop && argName == "until"` parses `parseOneParamLambda("@loop(until=...)")`; any other value refuses with `loop_until_form`), and `parser/parser.go` (`processAutomationAttributes` folds both; `max`/`maxDepth` read as ints, a non-integer refuses with `mode_max_range` / `loop_max_depth_range`).
- Modify: `component/language/compiler/automation_generator.go`. It emits `"loop": {"maxDepth": N, "until": "<canonical source>"}` and `"mode": {"kind": "<flag>", "max": N}`; two flags emit both, so the load can refuse them with the names.
- Create: `component/automations/loop_prepare.go`. `prepareLoopAndMode(a *Automation) error` is called from the loader's prepare step (beside `prepareExpressions`, `loader.go:351`) and from `ensurePrepared`:
  - **@loop:**
    - re-parse `Loop.Until` into `UntilLambda` (one parameter)
    - `loop_not_event_triggered` unless `a.IsEventTriggered()`
    - `loop_max_depth_range` unless `1 <= MaxDepth <= maxChainDepth()`
    - `loop_until_not_in_filter` unless `untilInFilter(a.Trigger.FilterLambda, a.Loop.UntilLambda)`; the message prints `@filter(row => <existing> && <negation>)` with the negation formatted by `ast.FormatExpr`
  - **@mode:**
    - `mode_flags` unless exactly one flag
    - `mode_max_not_allowed` for `max` on single or restart
    - `mode_max_range` for `max < 1`
  - Each message ends `[<code>]`.
- Create: `component/automations/loop_until.go`, with `untilInFilter(filter, until *ast.LambdaExpr) bool` and `negateComparison(ast.ExpressionNode) (ast.ExpressionNode, bool)`:
  1. Rename `until`'s parameter to the filter's (walk `IdentExpr` nodes named the until parameter).
  2. Build the candidates: `!(P)` and, when `P` unparens to a `BinaryExpr` with an invertible comparison op, the inverted comparison.
  3. Compare `ast.FormatExpr` of each candidate with `ast.FormatExpr` of each `ast.Conjuncts(filter.Body)` element.
- Modify: `component/language/parser/grammar_surface_drift_test.go` (the surface entries for the two annotations), `grammar_version.go` (new `GrammarVersion` `2026.09-dsl-v1-loop-protection-<digest>`: run the test, copy the digest it prints), `parser/edition.go` (`EditorRelease = "0.5.0"`), `editors/vscode/package.json` (`version` `0.5.0`, `memql.grammarVersion`), `editors/vscode/CHANGELOG.md` (a `## 0.5.0` section: "Automations can declare `@loop` and `@mode`; completion and diagnostics know both").
- Modify: `docs/public/language/attribute-matrix.md` via `make docs-matrix`.
- Create: corpus cells.
  1. Run `go test github.com/znasllc-io/memql/test/conformance -run TestScaffoldCorpusCells -scaffold` after adding the `loop`/`mode` entries to `test/conformance/corpus_scaffold_skeletons_test.go` (`scaffoldAnnotations`, `scaffoldDocs`, the `scaffoldAutomation` switch).
  2. Hand-write the cells in `test/conformance/2026/cells/automation/loop/` and `.../mode/`. The loop cell's automation must self-cycle, because a skeleton that writes an unrelated concept proves nothing about `@loop`:

`test/conformance/2026/cells/automation/loop/permits-a-converging-self-cycle.memql`:
```memql
use loopcell.concepts.{ ticket }

@trigger(event="node.created", concept="v1:loopcell:ticket")
@filter(row => row.status != "done")
@loop(maxDepth=4, until=row => row.status == "done")
automation advanceTicketLoopCell {
  args {
    id     any
    status any
  }
  step advance {
    mutation advanceTicketLoopCell (id: id, status: "done")
  }
}
```
  (the fixture declares `concept ticket { status string }` and `mutate ticket advanceTicketLoopCell { args { id string! status string } update { id: args.id, status: args.status } }`; follow the neighbouring cells' fixture spelling exactly). Cases:
  - `load_ok` for the above.
  - `refuse_load` `loop_until_not_in_filter` for the same automation with `@filter(row => row.status == "open")`.
  - `refuse_load` `loop_max_depth_range` for `maxDepth=0`.
  - `refuse_parse` `annotation_key` for `@loop(depth=4, ...)`.
  - `refuse_load` `loop_not_event_triggered` for a scheduled automation.
  - Mode cases:
    - `load_ok`: `@mode(single)` and `@mode(queued, max=3)`.
    - `refuse_load` `mode_flags`: `@mode(single, queued)`.
    - `refuse_load` `mode_max_not_allowed`: `@mode(single, max=2)`.
    - `refuse_parse` `annotation_key`: `@mode(serial)`.

  Until Task 8 lands, the self-cycle loads; after it, the permitted cycle still loads, which is why the cell is `load_ok` either way.
- Modify: `component/memql/annotation_registry_consistency_test.go` only if the new examples do not parse on its probe (they must; if `@loop`'s example needs a filter, give the probe a matching `@filter`).
- Test: `component/automations/loop_prepare_test.go`, `loop_until_test.go` (new, DB-free).

**Interfaces:**
- Consumes: Task 0's `LoopConfig`, `ModeConfig`.
- Produces:
  - `Automation.Loop` / `Automation.Mode` populated at load, with `Loop.UntilLambda` prepared.
  - `untilInFilter(filter, until *ast.LambdaExpr) bool`, used by Task 8's coverage.
  - `maxChainDepth() int` (defined here in `loop_prepare.go`, reading `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH` with default 16 via `envIntDefault`; Task 4 reuses it).

- [ ] **Step 1: Failing tests.**
  - `loop_until_test.go`:
    - `row.status == "done"` against filters `row => row.status != "done"` (true), `row => !(row.status == "done") && row.kind == "a"` (true), `row => row.status == "open"` (false), `x => x.status != "done"` (renaming, true), `row => row.n < 3` with until `row.n >= 3` (true).
  - `loop_prepare_test.go`:
    - each refusal code from compiled automations built by `NewLoader(...).CompileSource(src, "test")`.
    - the message of `loop_until_not_in_filter` contains the suggested `&& row.status != "done"`.
  - Parser tests beside `component/language/parser`'s annotation tests: `@loop(maxDepth=2, until=row => row.x == 1)` parses to `LoopDef{MaxDepth:2, Until:<lambda>}`, and `@mode(queued, max=5)` to `ModeDef{Flags:["queued"], Max:5}`.
- [ ] **Step 2:** Run `go test github.com/znasllc-io/memql/component/language/... github.com/znasllc-io/memql/component/automations/ -run 'Loop|Mode'`. Expect FAIL.
- [ ] **Step 3:** Implement as listed. Then run in order:
  - `make docs-matrix`
  - the drift test (copy the digest)
  - the editor-parity test (`go test github.com/znasllc-io/memql/cmd/memql-lsp/ -run Parity`)
  - the scaffold command
  - the corpus: `go test github.com/znasllc-io/memql/test/conformance/ -run TestCorpus`
- [ ] **Step 4:** Run the gates: `go test github.com/znasllc-io/memql/component/language/... github.com/znasllc-io/memql/component/automations/... github.com/znasllc-io/memql/test/conformance/... github.com/znasllc-io/memql/cmd/memql-lsp/...`, `go test -count=1 .` (attribute matrix parity), `go test -count=1 ./scripts/ci/...`. Then run `go test github.com/znasllc-io/memql/component/memql/ -run AnnotationRegistry`.
- [ ] **Step 5:** Commit `Issue #5381: @loop and @mode are annotations the registry, the parser and the load know`.

---

## Task 4: The run's chain, the depth cap, and the journaled refusal (#5382, #5384)

**Files:**
- Create: `component/automations/loop_runtime.go`:

```go
// LoopRefusal is a run the loop protection stopped before it started
// (D19, D-C): loop_depth_exceeded, carrying the chain.
type LoopRefusal struct {
	Reason     string       // metrics.LoopStopDepth | metrics.LoopStopLoopBound
	Automation string
	Depth      int          // the depth the refused run would have had
	Cap        int          // the bound it exceeded
	Cause      events.Cause // the refused run's cause, chain included
}

func (r *LoopRefusal) Error() string // "loop_depth_exceeded: <automation> would run at depth 17, past the cap of 16; chain: a (run-1) -> b (run-2) -> ... [loop_depth_exceeded]"

// rootCorrelation is a root event's correlation id: deterministic, so every
// replica derives the same one from the same event (D-A).
func rootCorrelation(ev *events.Event) string

// runCause is the cause a run carries, and its parent: the triggering event's
// cause, else the context's (a sub-automation), else none.
func runCause(ctx context.Context, automation *Automation, runId string, ev *events.Event) (parent, run events.Cause)

// loopBound refuses a run whose depth passes the cap, or whose @loop allows no
// further run of itself in this chain.
func loopBound(automation *Automation, run events.Cause, cap int) *LoopRefusal
```

  Correlation: `parent.CorrelationId`, else `rootCorrelation(ev)` when `ev != nil`, else `runId`. The loop bound counts `parent.Chain` entries whose `Automation == automation.Name`: `>= Loop.MaxDepth` refuses with `Cap: Loop.MaxDepth`.
- Create: `component/metrics/automations.go`:

```go
const (
	LoopStopDepth     = "depth"
	LoopStopLoopBound = "loop_bound"
	LoopStopEcho      = "echo"
	LoopStopRowBudget = "row_budget"
	LoopStopMode      = "mode"
)

var automationLoopsStopped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "automation",
	Name:      "loops_stopped_total",
	Help:      "Automation fires the loop protection stopped, by automation and reason: depth (the chain passed MEMQL_AUTOMATION_MAX_CHAIN_DEPTH), loop_bound (a @loop's maxDepth), echo (a rewrite in the same chain that changed nothing the automation reads), row_budget (MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW), mode (a @mode refused the fire). A converging loop that stops because its filter no longer matches is not a stop and is not counted.",
}, []string{"automation", "reason"})

func AutomationLoopStopped(automation, reason string) { automationLoopsStopped.WithLabelValues(automation, reason).Inc() }
func AutomationLoopsStoppedValue(automation, reason string) float64 {
	return counterValue(automationLoopsStopped.WithLabelValues(automation, reason))
}
```

  Register it in `metrics.go`'s `init` `MustRegister` list. Add `github.com/znasllc-io/memql/component/metrics` as a DIRECT require in `component/automations/go.mod` (it is `// indirect` today). Run `go test -count=1 ./scripts/ci/...` for the module-boundary gate.
- Create: `component/automations/journal_loop.go`. `func (j *workJournal) refuseLoop(ctx context.Context, automation *Automation, exec *AutomationExecution, ev *events.Event, parent events.Cause, r *LoopRefusal)`:
  - `openRun(...)` then one `updateWorkRun` with `status: "failed"`, `finishedAt`, `errorCode: work.TerminalLoopDepthExceeded`, `errorMessage: r.Error()`, and `outcome: {"executorStatus": "failed", "loop": {"reason", "depth", "cap", "correlationId", "chain": [{"automation","runId"}...]}}`.
  - It does NOT go through `closeRun`, so the classifier never sees it.
  - Extend `openRun`'s `triggerEvent` map with `"cause": parent` (JSON shape of `events.Cause`) whenever `!parent.IsZero()`.
- Modify: `component/automations/executor.go`:
  1. Near the top of `executeWithEvent`, after `exec := NewExecution(...)`, compute `parent, runCause := runCause(ctx, automation, exec.ID, triggeringEvent)` and keep both.
  2. Stamp `ctx = events.ContextWithCause(ctx, runCause)` BEFORE the input query and the steps, so every write and publish of the run carries it.
  3. After the cluster-guard claim (end of the chain-tracking block) and before the journal opens, call `if r := loopBound(automation, runCause, maxChainDepth()); r != nil { ... }`. That branch:
     - logs WARN with `automation`, `depth`, `cap` and `chain`
     - calls `metrics.AutomationLoopStopped(automation.Name, r.Reason)`
     - calls `journal.refuseLoop(...)` unless `journalSkipsAutomation(automation)`
     - sets `exec.Status = "failed"`, `exec.Error = r.Error()`, `exec.ErrorValue = r`, `exec.CompletedAt`
     - returns `exec, r`
  4. An adopted run keeps its adopted cause: resume and adopt pass the restored cause in through the triggering event (below).
- Modify: `component/automations/resume.go`, `adopt.go`. Where they rebuild the triggering event from `triggerEvent {topic, kind, payload}` (grep `triggerEvent` in both), also restore `Cause` from `triggerEvent.cause`. Add `events.CauseFromMap(map[string]any) events.Cause`, which reads the JSON shape and tolerates float64 depth, to `cause.go`.
- Modify: `component/work/terminal.go`. Add `TerminalLoopDepthExceeded = "loop_depth_exceeded"` to the constants, to `terminalFailureCodes` (keep the longest-first order), and to `TerminalReason`'s switch: "A chain of automations passed its depth bound. Another attempt runs the same chain into the same bound: the fix is in the automations, a converging @filter or an @loop, and the chain on this run names them."
- Test: `component/automations/loop_runtime_test.go` (new, DB-free):
  - `runCause` on a root event (depth 1, correlation `evt-...`, identical for two calls on the same event).
  - On a chained event (depth `ev.Depth+1`, correlation inherited, chain extended).
  - From a context cause with no event.
  - `loopBound` at depth 16 (nil) and 17 (refusal, `Reason=depth`, `Cap=16`).
  - A `@loop(maxDepth=2)` automation whose parent chain holds it twice (refusal, `loop_bound`).
- Test: `component/automations/loop_depth_chain_test.go` (new, DB-free):
  - Drive a real `Executor` with a fake step registry and a capturing journal executor. Grep `journalExecutor` in `journal_test.go` for the fake to copy.
  - The automation's one step re-publishes its own trigger topic with the step context's cause, so the executor sees fire after fire. Feed the next fire by calling `ExecuteWithEvent` with the event the step published, up to 17 fires.
  - Assert runs 1-16 complete and run 17 returns a `*LoopRefusal`.
  - Assert the journal holds one `createWorkRun` + `updateWorkRun{status:"failed", errorCode:"loop_depth_exceeded"}` for run 17, whose `outcome.loop.chain` lists 16 links whose run ids are the 16 completed runs, in order.
  - Assert `metrics.AutomationLoopsStoppedValue(name, "depth")` rose by exactly 1.
  - Negative control: set the cap env to 32 via `t.Setenv`, run, and assert all 17 complete and the counter did not move.

- [ ] **Step 1:** Write both tests. Run `go test github.com/znasllc-io/memql/component/automations/ -run 'RunCause|LoopBound|LoopDepth'`. Expect FAIL (undefined).
- [ ] **Step 2:** Implement `loop_runtime.go`, the metric, `journal_loop.go`, the executor call sites, the resume and adopt restore, and `terminal.go`, plus `component/work/terminal_test.go` for the new code.
- [ ] **Step 3:** Run the tests (PASS), then `go test github.com/znasllc-io/memql/component/automations/... github.com/znasllc-io/memql/component/work/... github.com/znasllc-io/memql/component/metrics/...` and the db-gated `./component/automations/...`.
- [ ] **Step 4:** STRUCK: Task 3 already registered `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH` (the forward-drift gate required it with the first read). Do not register it again; `make env-registry-check` must still pass.
- [ ] **Step 5:** Commit `Issue #5382: a run carries its chain; one past the cap is a loop_depth_exceeded failure naming the chain`.

---

## Task 5: The dedup key, narrowed and scoped to the chain (#5382)

**Files:**
- Create: `component/automations/loop_reads.go`. `computeReads(a *Automation, fns FunctionSource) []string` returns nil unless the automation is event-pure (D-E). Otherwise it returns the sorted, deduplicated union of:
  - the declared args names
  - the `@filter` lambda's parameter member paths (first segment; `ast.MemberPaths`)
  - every `args.<f>` and `event.payload.<f>` first segment in the step expressions: walk `Step.Exprs` and the value leaves with `ast.WalkV1`, the same leaves `expressions_v1.go` prepares
  - `id` and `nodeId`

  A bare `event` or `event.payload` value, an `event.<x>` other than `payload`, a builtin/action/webhook/automation step, or a logic whose transitive body holds one of those, makes it nil. `FunctionSource` is Task 8's; until Task 8 merges, stream 4 defines a local interface of the same method set in this file and deletes it on merge.
- Modify: `component/automations/fingerprint.go`.
  - `eventFingerprintData(ev *events.Event, a *Automation, correlation string)` returns `{topic, kind, correlation, payload: projection}`. On `graph.node.*` topics the projection is `a.Reads` restricted to those keys when `a.Reads != nil`; otherwise the payload with the top-level `createdAt` deleted (on a copy). On any other topic, it is the whole payload.
  - `ComputeInitialChainHead` hashes `correlation` too.
  - `eventIdentity(ev *events.Event) string` is the old whole-payload fingerprint (topic, kind, payload), kept for the echo test.
- Modify: `component/automations/dedup.go`.
  - `isDuplicate` becomes `claimOrDuplicate(automation, key, identity, execId string) (dup bool, echo bool)`. It registers the key as in-flight when absent. It reports `echo` when a present entry's identity differs from the caller's.
  - `register` marks the entry complete on success. `release(automation, key, execId)` drops an in-flight entry on failure or skip.
- Modify: `component/automations/executor.go`.
  - The chain-tracking block passes the correlation into `eventFingerprintData`.
  - It calls `claimOrDuplicate`. On `echo` it counts `metrics.AutomationLoopStopped(name, metrics.LoopStopEcho)`.
  - The failure and early-return paths after the claim call `dedup.release`.
  - `Reads` are computed once at load (the loader sets `automation.Reads = computeReads(...)` after prepare), not per fire.
- Test: `component/automations/dedup_chain_test.go` (new, DB-free):
  1. **Same chain, differing only in createdAt.** Two `graph.node.created.v1:t:x` events carry the SAME chained cause (depth 2, correlation `c-1`) and payloads differing only in `createdAt`, for an automation reading `status`. The second `ExecuteWithEvent` returns `skipped` "duplicate execution detected". The acceptance case.
  2. **Two root writes.** The same two payloads, each a ROOT event: both execute (flip-flop safety).
  3. **Same chain, differing in a read field.** Both execute.
  4. **Not event-pure.** The automation calls a builtin; two same-chain writes differing in an UNREAD field both execute, because the whole payload minus the clock is kept.
  5. **In-flight echo.** The first run's step blocks on a channel while the second fire arrives (same chain, same projection). The second is skipped and counted `echo`, and the counter moves by one.
  6. **Retry after failure.** A failed run releases its entry, so a retry of the same event runs.
  - Update `chain_head_clock_test.go` / `fingerprint_test.go` expectations only where they pinned the old map shape. Keep `TestEventFingerprintDataExcludesTheWallClock` meaningful: the Timestamp still never enters the key.

- [ ] **Step 1:** Write `dedup_chain_test.go`. Run it. Expect FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** Run `go test github.com/znasllc-io/memql/component/automations/... -count=1` and the db-gated `./component/automations/...` (`cluster_guard_db_test.go` must still hold: the claim key is still deterministic for one event across replicas).
- [ ] **Step 4:** Commit `Issue #5382: the dedup key keeps the fields an automation reads, scoped to its chain`.

---

## Task 6: The per-(automation, row) budget (#5382)

**Files:**
- Modify: `component/automations/budget.go`.
  - `automationBudget` gains `perRowMax int` (env `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW`, default 30) and `perRow map[string]*windowCount`, keyed `automation + "\x00" + rowId`.
  - `admit(automationName, rowId string) (allowed bool, dimension string, alert bool)`:
    - `dimension` is `"global"`, `"per-automation"` or `"per-row"`.
    - `alert` is true once per window per dimension, keeping the loud-once rule.
    - `rowId == ""` skips the row dimension.
  - When the global window rolls, entries older than the window are pruned, so the map stays bounded.
- Modify: `component/automations/executor.go`.
  - The budget call site passes `budgetRowId(triggeringEvent)` (`payload["nodeId"]`, else `payload["id"]`, on `graph.node.*` only).
  - On `per-row` it counts `metrics.AutomationLoopStopped(name, metrics.LoopStopRowBudget)` and sets `exec.Error = "automation execution budget exceeded for this row (per-row, memql#5382)"`.
- Test: `component/automations/budget_test.go` (extend):
  - 30 admits for one row pass and the 31st is refused `per-row`.
  - Another row is unaffected.
  - The counter moves once per refusal.
  - The pruning keeps `len(perRow)` at the rows seen in the current window: inject `now`, admit 100 rows, roll past the window, admit one, and assert `len(perRow) == 1`.
  - `rowId == ""` never refuses on the row dimension.
- [ ] **Step 1:** Extend the tests. Run `go test github.com/znasllc-io/memql/component/automations/ -run Budget`. Expect FAIL.
- [ ] **Step 2:** Implement, and register the env var in the manifest: component `safety`, default `"30"`, description "Per-(automation, row) execution ceiling within the budget window: a backstop for a loop that escapes the chain's depth, through an external round trip or a Go subscriber. Further fires for that row are skipped". Then `make env-registry-sync && make env-registry-check`.
- [ ] **Step 3:** Run the package tests (PASS).
- [ ] **Step 4:** Commit `Issue #5382: a per-(automation, row) budget stops the loop the chain cannot see`.

---

## Task 7: Modes at run time (#5382)

**Files:**
- Create: `component/automations/mode_gate.go`:

```go
// modeGate holds each automation's in-flight runs in this process and
// applies its @mode (D-G). The zero Mode is unbounded parallel.
type modeGate struct {
	mu     sync.Mutex
	byName map[string]*modeState
}

type modeState struct {
	active  map[string]context.CancelFunc // run id -> cancel
	waiting []chan struct{}                // queued fires, FIFO
}

// acquire admits a fire or refuses it. On admit it returns a release func the
// caller defers, and a context the run executes under (cancelled by a restart).
// On refusal err is a *ModeRefusal naming the mode and the in-flight run.
func (g *modeGate) acquire(ctx context.Context, a *Automation, runId string) (context.Context, func(), error)

type ModeRefusal struct {
	Automation, Mode, InFlightRunId string
	Max                              int
}
func (r *ModeRefusal) Error() string // "mode single: automation X has run <id> in flight; this fire is refused [mode_refused]"
```

- Modify: `component/automations/executor.go`.
  - `Executor` holds `modes *modeGate`, and every executor built by `NewExecutor` shares a package-level one, the way the budget is shared.
  - `executeWithEvent` calls `acquire` FIRST, before the concurrency slot. A queued fire waits there without holding a slot, and gives up with `ctx.Err()`.
  - On a `*ModeRefusal`: log WARN (acceptance: "with a warning"), count `metrics.LoopStopMode`, and return `skipped` with `exec.Error = refusal.Error()` and a nil error.
  - Restart cancels the in-flight run's context; that run then stops at its next boundary as `cancelled` through the existing `ctx.Done()` branch.
- Test: `component/automations/mode_gate_test.go` (new, DB-free, with a fake step registry whose step blocks on a channel):
  - `single`: the second concurrent fire is skipped, a WARN is captured (use a `slog` handler that records), and the counter moves.
  - `queued, max=1`: the second fire waits and runs after the first; the third, while one waits, is refused.
  - `restart`: the first run ends `cancelled` and the second completes.
  - `parallel, max=2`: the third concurrent fire is refused.
  - No `@mode`: 5 concurrent fires all run.
  - A queued fire holds no concurrency slot: build an executor with `MaxConcurrentExecutions: 1`, and a queued waiter does not block a different automation's run.
- [ ] **Step 1:** Write the tests. Expect FAIL.
- [ ] **Step 2:** Implement, and register `MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX` (default `"10"`) in the manifest.
- [ ] **Step 3:** Run `go test github.com/znasllc-io/memql/component/automations/... -race -run Mode` and the package suite.
- [ ] **Step 4:** Commit `Issue #5382: @mode single, queued, restart and parallel govern concurrent fires`.

---

## Task 8: The static graph and its refusal (#5381)

**Files:**
- Modify: `component/work/footprint.go`. Add the types below, and set `Target.Write *WriteSpec` (a mutation's write). Keep `UnionFootprint` unchanged: it is the node's `Writes` list, its first production caller.

```go
// FieldValue is what a mutation template writes into one field, as far as
// its source says at load.
type FieldValue struct {
	Literal    any    `json:"literal,omitempty"`
	HasLiteral bool   `json:"hasLiteral,omitempty"`
	Arg        string `json:"arg,omitempty"` // the value is exactly args.<Arg>
}

// WriteSpec is one mutation's write.
type WriteSpec struct {
	Kind   string                `json:"kind"` // insert | update
	NewRow bool                  `json:"newRow,omitempty"`
	Fields map[string]FieldValue `json:"fields,omitempty"`
}

// Write is one mutation a set of names reaches, with the path that reaches it.
type Write struct {
	Concept  string
	Mutation string
	Path     []string
	Spec     WriteSpec
}

// UnionWrites walks the call graph from each name, as UnionFootprint does, and
// returns every mutation it reaches, sorted by (Concept, Mutation, Path).
func UnionWrites(names []string, reg Registry) []Write
```

- Create: `component/automations/loop_graph_source.go`.
  - `type FunctionSource interface { Registry() work.Registry }`.
  - `NewFunctionSource(fns *memql.FunctionRegistry) FunctionSource` builds the registry once from every function:
    - **query:** `ConstructQuery`.
    - **mutation:** `ConstructMutation`; `Concept` is `BoundConcept`, else `MutationTemplate.Concept` (the idiom of `component/memql/rowauthz_owner_provenance.go:172-181`); `Write` comes from `MutationTemplate`:
      - `Kind` (empty means insert), and `NewRow` is `IDTemplate == nil`.
      - `Fields` come from `PayloadTemplate`'s top-level keys. A plain JSON scalar leaf is a literal. A `{"$expr": src}` leaf that parses to exactly `args.<x>` is `Arg: x`. Anything else is an unknown value (present in `Fields` with neither set).
      - Read `mutation_templates.go` and `value_leaves.go` for the exact leaf encoding before writing this, and pin it with a test over a real loaded mutation.
    - **logic:** `ConstructLogic`, with `Calls` = the names its body calls. Walk `fn.Expr` for `CallExpr`/`FunctionCallExpr` names and `fn.LogicSteps`' steps for function-step names; epic 3 replaces both in Task 13.
    - **builtin:** `ConstructBuiltin` with empty `Effects`.
  - Bare names are keyed with `LookupIndex()` (`core/baseregistry/registry.go:249`) the way `engine_bootstrap.go:403-407` resolves them.
- Create: `component/automations/loop_graph.go`. `BuildLoopGraph(automations []*Automation, src FunctionSource, depthCap int) *LoopGraph`, with the types below. It walks each automation's steps: nested `ForEach.Do`, `Parallel.Branches`, block steps, `OnComplete`, `OnError`, and on `main` also the legacy switch cases through a helper that Task 13 deletes. It collects:
  - direct function callees, with their call-site args (`Step.Function.Args`)
  - sub-automation names
  - event-step topics (literal, or "expression")
  - opaque calls (builtin/action names)

  Then:
  - `work.UnionFootprint` gives `Writes`, and `work.UnionWrites` gives edges.
  - A direct mutation's `Arg` fields resolve against the step's literal args.
  - Sub-automations union their callees' writes and publishes, cycle-safe.
  - Edges follow D-I, with the filter decided by `loop_graph_filter.go`.

```go
type LoopGraph struct {
	Automations []GraphAutomation
	Edges       []GraphEdge
	Cycles      []GraphCycle
	Problems    []LoopProblem
	Coverage    GraphCoverage
}
type GraphAutomation struct {
	Name, Origin, Trigger, Schedule, Filter string
	Template                                bool
	Stratum                                 int
	Writes, Publishes, Opaque               []string
	Loop                                    *LoopConfig
	Mode                                    *ModeConfig
	Cycle                                   int // index into Cycles, -1 when none
}
type GraphEdge struct {
	From, To, Concept, Topic string
	Via                      []string
	Decided                  bool
	Reason                   string
}
type GraphCycle struct {
	Members, PermittedBy, Path []string
}
type LoopProblem struct {
	Automation, Origin, Code, Message string
}
type GraphCoverage struct {
	Automations, Resolved, Unresolved, Opaque int
}
```

- Create: `component/automations/loop_graph_filter.go`. `decideFilter(filter *ast.LambdaExpr, row knownRow) (tri, string)` is a three-valued partial evaluator over the v1 node set:
  - literals, `nil`
  - `row.<f>` and `args.<f>` (both read the written payload field `f`; `args.firstVersion` reads `row.firstVersion`, and `row.concept` the concept)
  - `==`, `!=` under D8's absence table
  - `<`, `<=`, `>`, `>=` on numbers and strings of one type
  - `in` over a list literal, `startsWith`
  - `&&`, `||`, `!` under Kleene logic
  - `??`
  - `ParenExpr`

  Any other node is `unknown`, and the reason names the node (`ast.FormatExpr`) and the first field it could not read.

```go
type tri int8
const (triFalse tri = iota; triTrue; triUnknown)
type knownRow struct {
	Concept      string
	Fields       map[string]any  // known literal values
	Absent       map[string]bool // known absent (an insert of a new row that does not set them)
	OthersKnown  bool            // true when every unset field is absent (NewRow insert)
	FirstVersion tri
}
```

- Create: `component/automations/loop_graph_scc.go`, with `tarjan` (iterative, deterministic by sorted node order), `strata` (the longest-path layering of the condensation) and `covered(scc, loops) bool` (the SCC minus its @loop members has no cycle: re-run Tarjan on the induced subgraph).
- Create: `component/automations/loop_graph_report.go`. It holds `cyclePath` (the shortest cycle through the SCC's first uncovered member, BFS over the SCC minus @loop members) and `renderCycleProblem`, whose message form is:

```
automation cycle not covered by @loop: routeRequest -> routeRequest
  routeRequest -> routeRequest: routeRequest writes v1:forge:request through advanceRequest (update), which publishes graph.node.created.v1:forge:request, and routeRequest triggers on it with no @filter
  Permit it with @loop(maxDepth=<n>, until=row => <done>) on one automation of the cycle, with its negation in that automation's @filter; or give an automation a @filter this write cannot satisfy -- a trigger for first versions only is @filter(row => args.firstVersion == true) [loop_cycle]
```

  An undecided edge's line ends `; its @filter row => row.status == "submitted" could not be decided: advanceRequest sets status to args.status, known only at run time`.
- Create: `component/automations/loop_check.go`. `func (l *Loader) checkLoops(loaded []*Automation) (*LoopGraph, []automationLoadProblem)` runs `BuildLoopGraph` with `NewFunctionSource(l.functions)` and turns each `LoopProblem` into `automationLoadProblem{Path: <file of the named automation>, Name, Phase: "loops", Err: message}`. It also exports `func (l *Loader) StaticGraph() (*LoopGraph, error)` (LoadAll plus build; for Tasks 11-12).
- Modify: `component/automations/loader.go`.
  - `LoaderOptions.Functions *memql.FunctionRegistry`, carried on `Loader.functions`.
  - After prepare, set `automation.Reads = computeReads(automation, NewFunctionSource(l.functions))` when functions are set. The FunctionSource adapter is shared; build it once per `LoadAll` and pass it into compile through a loader field set for the walk.
- Modify: `component/automations/unified_loader.go`. After the walk (before the strict gate), `if l.functions != nil { _, ps := l.checkLoops(out); problems = append(problems, ps...) } else { log "static loop analysis not run: the loader has no function registry" }`.
- Modify: `app/engine.go:131`. `Functions: a.engine.Functions()`.
- Modify: `component/automations/authored_scheduler.go`. In `Activate`, after `CompileSource`, when the scheduler has a loader with functions: `BuildLoopGraph(append(shipped, candidate), ...)`, and refuse activation with the problem when one names the candidate (`shipped` from `loader.LoadAll()`, cached on the scheduler). If `Activate` cannot reach a function registry, record that in the checkpoint and wire it through `AuthoredRuntimeDeps` rather than skipping silently.
- Test: `component/work/footprint_test.go` (`UnionWrites` through logic, cycle-safe, sorted).
- Test: `component/automations/loop_graph_test.go` (new, DB-free). Build a `FunctionSource` from a literal `work.Registry` through a test adapter (`fakeSource{reg}`) and compiled automations from `CompileSource`. Cases:
  - **Self-edge.** An update self-edge with no filter: a cycle, refused `loop_cycle`, the path `a -> a`.
  - **Refuted by kind.** An insert to C, and B triggers on `node.updated.C`: no edge.
  - **Refuted by the filter.** An update stamping `status: "done"`, and B's filter is `row.status == "open"`: no edge.
  - **Undecided.** The same update with `status: args.s`, and the call site passes `s: args.x` (not a literal): an undecided edge whose reason names `status`.
  - **Resolved at the call site.** The same with call-site literal `s: "open"`: an edge, decided true.
  - **firstVersion.** B filters on `args.firstVersion == true` and A updates: no edge.
  - **The library pair.** The pair as in the tree: `archiveFileOnArtifactArchive -> indexFileOnCreate` undecided (status inherited), and no edge back.
  - **The mirror fixture.** The pair plus a mirror automation (file `node.updated`, filter `archived == true` -> an update on artifact setting `archived: true`): the refusal names both edges, `archiveFileOnArtifactArchive -> <mirror>` and `<mirror> -> archiveFileOnArtifactArchive`.
  - **Permitted.** A self-cycle with `@loop(maxDepth=3, until=row => row.status == "done")` and `@filter(row => row.status != "done")`: permitted, no problem, `PermittedBy` names it.
  - **Two @loop-free cycles.** Two cycles sharing a node, only one through a @loop automation: refused.
  - **Strata.** Chain `a -> b -> c` gives strata 0, 1, 2; an SCC `{b, c}` gives both the same stratum.
  - **Topic edges.** A publish of topic `x.y` gives an edge to an automation on `@trigger(event="x.y")`; an expression topic gives an edge to every raw-topic automation, undecided.
  - **Sub-automations.** A calls automation S, which updates C: A gets S's write.
  - **Coverage.** Coverage counts resolved, unresolved and opaque calls.
- Test: `component/automations/loop_graph_filter_test.go`: the three-valued table, including `!=` true on absent, `&&` false dominating unknown, `||` true dominating unknown, and `??`.

- [ ] **Step 1:** Write the `work` tests and `loop_graph_filter_test.go`. Run `go test github.com/znasllc-io/memql/component/work/ github.com/znasllc-io/memql/component/automations/ -run 'UnionWrites|DecideFilter'`. Expect FAIL.
- [ ] **Step 2:** Implement `footprint.go` and `loop_graph_filter.go`. PASS.
- [ ] **Step 3:** Write `loop_graph_test.go`. Expect FAIL.
- [ ] **Step 4:** Implement `loop_graph_source.go`, `loop_graph.go`, `loop_graph_scc.go` and `loop_graph_report.go`. PASS.
- [ ] **Step 5:** Wire `checkLoops` as REPORT-ONLY first: problems are logged, not appended. Add `loop_tree_measure_test.go`, which boots the embedded tree offline (Task 10's `memql.NewOfflineEngine`; until it merges, copy `LintUnifiedTree`'s New+Init pair locally) and prints every problem for `dsl/`, `deploy/fleet/dsl` and `examples/*/dsl` through the loader's `loadFromTree` over each bundle, the way `step_order_test.go` does. Run it with `-v` and paste the list into the checkpoint. That list is the input of Task 9.
- [ ] **Step 6:** Commit `Issue #5381: the static graph over automations, their writes and their triggers, measured on the tree`.

---

## Task 9: The tree's cycles fixed; the refusal flipped on (#5381)

**Files:**
- Modify: `dsl/forge/automations.memql`.
  - `routeRequest` gains `@filter(row => args.firstVersion == true)` and declares `firstVersion bool` in its args. The self-edge is refuted: `advanceRequest` is an update, so `firstVersion` is false.
  - The file's "Loop safety" paragraph is rewritten to say what the engine now checks: every write publishes `node.created`, so the first-version filter is what keeps `routeRequest` from re-firing on its own advance, and the load refuses the automation without it.
  - Phase B rewrites the automation again as a before-write body (Task 14).
- Modify: `deploy/fleet/dsl/fleet/automations.memql`. For each of the six automations on `v1:fleet:instance`, read its decide logic and its `markInstance*` mutations (`fleet/mutations.memql:359-401`), then give it the `@filter` that states the state it acts on: the status values its switch acts on, or `args.firstVersion == true` for the create path. A settled status written by the controller then refutes each self-edge.
  - Where a legitimate cycle remains, for example "re-render on shape change" rewriting the instance, declare `@loop` with the converged predicate.
  - Rewrite the "NO LOOPS, BY CONSTRUCTION" comment into what now holds it.
  - Re-run the measurement: zero problems.
- Modify: `examples/deploypack/dsl/automations.memql`. The same for `driveDeploymentInProgress` and `recordReconciledState`.
- Modify: every other problem the Task 8 measurement listed, one commit per bundle, with the reasoning in the commit body. A problem that is a false positive of the analysis, not a real cycle, is fixed IN THE ANALYSIS: sharpen the refinement and add the case to `loop_graph_test.go`. It is never waived. Record in the checkpoint how many false positives the tree had and what fixed each (record section 6 asks for this count).
- Modify: `component/automations/unified_loader.go`. Flip `checkLoops` from report-only to appending problems.
- Modify: `dsl/library/automations.memql`. The prose at `:245-254` changes from "would close a cycle ... by prose" to "the load refuses the mirror (the static loop check, memql#5381); the corpus holds that case".
- Test: `component/automations/strict_automation_boot_test.go` (extend): a tree with an uncovered cycle refuses with `[loop_cycle]` and phase `loops`, and `MEMQL_DSL_ALLOW_SKIPS=1` loads it with the ERROR logged. Also `test/conformance/2026/cells/automation/trigger/` or a new `negative/automation/`: a `refuse_load` `loop_cycle` case, a self-update with no filter, whose message prefix is `automation cycle not covered by @loop`.

- [ ] **Step 1:** Fix forge, the fleet and deploypack, re-running the measurement after each.
- [ ] **Step 2:** Flip the refusal on. Run `go test github.com/znasllc-io/memql/component/automations/... github.com/znasllc-io/memql/test/conformance/...` and `go test -count=1 github.com/znasllc-io/memql/component/automations/ -run 'StepOrder|UnifiedLoader|StrictAutomation'`.
- [ ] **Step 3:** Boot check: `go build -o /tmp/claude-1000/-home-znas-memql-projects-memql/d5c8d310-fa73-4a0a-a885-f54c7f160257/scratchpad/memql .`, then run the engine's automation load offline through the measurement test. There must be zero problems.
- [ ] **Step 4:** Commit per bundle: `Issue #5381: forge's routeRequest fires on a request's first version only; the load would refuse the self-update otherwise`, then fleet, then deploypack, then `Issue #5381: an automation cycle no @loop covers refuses the load`.

---

## Task 10: memqllint and the corpus runner run the graph (#5381)

**Files:**
- Create: `component/memql/offline_engine.go`. `func NewOfflineEngine(logger *slog.Logger, registry memorynodes.Registry) (*MemQLEngine, error)` is the `New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))` + `Init(registry)` pair `LintUnifiedTree` uses. `LintUnifiedTree` calls it: refactor, no behaviour change.
- Modify: `component/memql/lint_parity.go`. Add `LintUnifiedTreeWith(logger, root, extra func(eng *MemQLEngine) []LintDiagnostic)`; `LintUnifiedTree` calls it with nil. `extra` runs after Init, inside the mount window.
- Modify: `cmd/memqllint/main.go`. The directory mode passes an `extra` that runs `automations.NewLoader(automations.LoaderOptions{Logger: quiet, Registry: memoryNodes.DefaultRegistry(), Functions: eng.Functions()}).LoadAll()` and turns each `- <path>:<name> [<phase>] <err>` line of the refusal into a `LintDiagnostic{File: path, Message: ...}`.
- Modify: `test/conformance/corpus_test.go` `corpusAutomationProblems`. Build `memql.NewOfflineEngine(corpusQuiet, memoryNodes.DefaultRegistry())` after loading concepts, and pass `Functions: eng.Functions()` to the loader.
- Test:
  - `cmd/memqllint/main_test.go`: a bundle root with a self-updating automation reports `[loop_cycle]` with exit 1, and the same bundle with the `@loop` fix exits 0.
  - The corpus: the Task 9 negative case runs through the runner.

- [ ] **Step 1:** Write the memqllint test. Expect FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** Run `go test github.com/znasllc-io/memql/cmd/memqllint/ github.com/znasllc-io/memql/test/conformance/... github.com/znasllc-io/memql/component/memql/ -run 'Lint|Corpus'`, and `go run ./cmd/memqllint dsl/`, which must exit 0.
- [ ] **Step 4:** Commit `Issue #5381: memqllint and the corpus refuse an automation cycle as boot does`.

---

## Task 11: The architecture model renders the graph (#5384)

**Files:**
- Modify: `component/architecture/model/model.go`. `KindAutomation Kind = "automation"` and `EdgeTriggers EdgeKind = "triggers"`, each with a doc line, in the closed sets. `model/ids.go`: `func AutomationID(name string) ID { return ID("automation:" + name) }`. Update `component/architecture/CLAUDE.md`, whose ":235 The model is pure Go" line gains the one DSL-derived family, and its ":63 (Planned) ERD" line.
- Create: `cmd/memql-arch/automations.go`. `func addAutomations(m *model.Model, root string) error`:
  - Load concepts (`memql.LoadUnifiedConcepts`), `memql.NewOfflineEngine`, `automations.NewLoader(...).StaticGraph()`.
  - Append one node per automation (`Kind: KindAutomation`, `Parent`: the model's cluster node id, `Source` the automation's file and line when the origin gives them, `Attrs: stratum, trigger, schedule, filter, loop, mode, writes` (comma-joined) and `origin`).
  - Append one `EdgeTriggers` edge per graph edge (`Attrs: concept, topic, decided, via`).
  - Sort as `WriteJSON` requires.
  - `main.go` gains `--automations` and calls it after `extract.Run`.
- Modify: `Makefile` `arch-model` (append `--automations`).
- Modify: the CI path filter for the staleness gate (`.github/workflows/ci.yml:287` block) and the gate-input rows (`scripts/dev/gate_inputs_lane_scope_test.go:79,131`). Add `dsl/**/automations.memql`, `dsl/**/mutations.memql` and `dsl/**/logic.memql`, so a DSL-only change that removes an edge runs the gate.
- Test:
  - `component/architecture/model_automations_test.go` (new): the committed model holds an `automation:routeRequest` node with a `stratum` attr, and a `triggers` edge exists for at least one known in-tree path (read one from the measurement, e.g. `archiveFileOnArtifactArchive -> indexFileOnCreate`).
  - `cmd/memql-arch` unit test: `addAutomations` on the embedded tree adds as many nodes as the loader loads.

- [ ] **Step 1:** Write the tests. Expect FAIL.
- [ ] **Step 2:** Implement. Run `make arch-model` (this is the phase's last model regeneration unless Task 12 adds Go symbols after it; if so, regenerate again at the end of Task 12). Check the size: `stat -c %s component/architecture/embedded/topology.model.json` must be under 70MB.
- [ ] **Step 3:** Run `go test -count=1 github.com/znasllc-io/memql/component/architecture/... github.com/znasllc-io/memql/cmd/memql-arch/...` and `go test -count=1 ./scripts/...`.
- [ ] **Step 4:** Commit `Issue #5384: the architecture model draws every automation with its stratum and every edge between them`.

---

## Task 12: The OS surface: Cluster, Automations (#5384, the owner's UI ask)

**REQUIRED SKILL:** invoke `frontend-design:frontend-design` before writing any UI, and follow `clients/os/DESIGN.md`'s twelve rules and `clients/os/README.md`'s reading, arrival and absence rules.

**Files (engine):**
- Modify: `dsl/platform/concepts.memql`. Add a virtual concept `automationNode`, documented like `dataOrigin` ("produced by the `automationGraph` builtin from the loaded automations, never persisted"). Its fields:
  - `name string!`, `stratum integer!`
  - `trigger string`, `schedule string`, `filter string`
  - `writes []string`, `publishes []string`, `opaque []string`
  - `loop object`, `mode object`
  - `edgesOut []object` (each `{to, concept, topic, via, decided, reason}`)
  - `cycle object` (`{members, permittedBy}`, absent when none)
  - `origin string`
- Modify: `dsl/common/builtins.memql`. `@sdk @executor("automationGraph") @requiresCapability("read", "app:cluster/automations") builtin automationGraph { }` and `@sdk @executor("automationLoopStops") @requiresCapability("read", "app:cluster/automations") builtin automationLoopStops { }`, each with a `///` doc comment.
- Create: `component/memql/automation_graph_read.go`.
  - `type AutomationGraphSource interface { AutomationGraphRows() []map[string]any }` and `SetAutomationGraphSource(s)` (the `SetAutomationCataloger` seam), plus the two executors (register beside `dataOrigins` in `executor_builtin.go:55`).
  - `automationGraph` projects the source's rows into `memorynodes.MemoryNode` rows of `v1:platform:automationNode`, with id = the automation name.
  - `automationLoopStops` reads the latest 50 `v1:work:run` rows with `errorCode == "loop_depth_exceeded"`, newest first, under `auth.ContextWithInternalOrigin` plus a cluster-owner access context. Its read goes through a `@serverOnly` query `workRunsStoppedByLoops`, added to `dsl/work/queries.memql` with a `///` doc saying why caller scoping is impossible, plus its `server_only_parsed_test.go` entry. It returns rows projected to `{runId, automationName, finishedAt, depth, cap, reason, correlationId, chain}`, with no payloads.
- Modify: `component/automations/scheduler.go`. `AutomationGraphRows()` builds the graph from the loader, cached until the automation set changes, and `app/engine.go` wires `a.engine.SetAutomationGraphSource(a.automationScheduler)` beside `SetAutomationCataloger`.
- Modify: `dsl/rbac/seeds.memql`. Seed `read app:cluster/automations` for owner, developer and admin, the way `app:cluster/logs` is seeded (`:335-339`).
- Run: `make sdk-gen`, `make concept-snapshot` (a new concept), and `go run ./cmd/memqlmigrate --rewrite=doc-comment-descriptions,accept-stamp,required-sigil,same-domain-use -w dsl/platform dsl/common dsl/work` if any codemod gate complains.

**Files (OS):**
- Modify: `clients/os/src/apps/cluster/settings.ts` (`{ id: "automations", name: "Automations", requires: "app:cluster/automations" }` between Data origins and Agents), `ClusterApp.tsx` (the branch).
- Create: `clients/os/src/cluster/automations/AutomationsSection.tsx`, `useAutomationGraph.ts` (one `useReading` over both builtins), `layout.ts` (a PURE function from rows to `{columns, nodes: {x, y, w, h}, edges: path strings}`, tested without a DOM), `AutomationsMap.tsx` (plain SVG), `AutomationDetail.tsx`, and `LoopStops.tsx`.
- The design intent, which the frontend-design pass makes concrete:
  - **Head.** "Automations" with the reading's "looked at <time>" and a re-read action.
  - **The map: the graph read left to right.** One column per stratum, headed "Stratum 0: fired from outside" and so on. Each automation is a compact card: name, trigger in one line, a stratum-coloured edge, a loop mark when it carries `@loop`, and a mode chip when it declares `@mode`. Edges are curves labelled with the concept they pass through. An UNDECIDED edge is dashed, and its tooltip names the filter it could not read. A permitted cycle is drawn as one rounded group with the `@loop` automation marked and its `until` shown. Automations with no edges collapse to a quiet list under the map ("27 automations write nothing another automation reacts to"), so the map shows structure, not a wall of cards.
  - **Detail, in its own scroll column** (rule 11). Selecting a card shows its trigger, filter, writes and publishes, the calls the graph cannot see ("opaque: 2 builtins"), mode and loop, and in/out edges with their reasons and via paths.
  - **Loops stopped.** The latest stops, each drawn as its chain, a horizontal breadcrumb of automation names with the refused one last and a depth badge "16 of 16". Empty state: "No loop has been stopped." An absent counter is an em dash, never 0.
  - **Keyboard.** Arrow keys move across cards and Enter opens detail. Colour is never the only carrier: a dashed edge also says "undecided" in its label on hover and focus.
  - Both light and dark modes. Reduced motion respected.
- Test:
  - `clients/os/test/cluster/automations-layout.test.ts`: columns by stratum, stable order, an SCC grouped, no two cards overlap.
  - `clients/os/test/cluster/automations-section.test.tsx`: rows render, an undecided edge carries its label, the empty state for stops, a refused capability renders the server's words.
  - `component/memql/automation_graph_read_test.go`: the capability gate refuses a caller without the grant, rows are projected, and loop stops carry no payload keys.
- Verification: `make os-test` (it runs `npm ci`; do not run it while another session's OS build runs), `make os-build` (only it parses the stylesheet), and a real-browser pass per memory `os-surfaces-need-a-real-browser-pass` / `measure-portal-pixels-with-a-vite-qa-harness`: screenshots in both modes, empty and populated. Attach them to the PR.

- [ ] **Step 1:** Engine tests (the gate, the projection). Expect FAIL. Implement. PASS.
- [ ] **Step 2:** `make sdk-gen`, then `npm run build` in `sdk/ts` (the OS typechecks against `dist/`).
- [ ] **Step 3:** Invoke `frontend-design:frontend-design`, write the layout tests, and implement `layout.ts`.
- [ ] **Step 4:** The components, then the section test, `make os-test`, `make os-build`, and the browser pass.
- [ ] **Step 5:** `go test -count=1 .` (root gates), `go test github.com/znasllc-io/memql/component/memql/... -run 'Automation|Capability|RowAuthz|App'`, and `TestOsRegistryRequiresMatchTheAppSeeds`.
- [ ] **Step 6:** Commit `Issue #5384: the Cluster app draws the automation graph by stratum, and the loops it stopped`.

---

## Task 13: Merge epic 3; the graph walks the statement form (Phase B)

- [ ] **Step 1:** When memql-22 reports the merge SHA, run `git -C /home/znas/memql-projects/epic-dsl-v1-loop-protection fetch origin && git merge origin/main`. For each conflict:
  - `executor.go`, `loader.go`, `types.go`, `resume.go`, `registry.go`: take THEIRS, then re-apply this epic's call sites (cause at start, refusal after the claim, mode gate, row budget, `LoaderOptions.Functions`, the loop check in the walk, the `@loop`/`@mode` placements, the `Loop`/`Mode`/`Reads` fields). `runSequence` is the one loop now, so the ctx stamping stays in `executeWithEvent` before it.
  - `grammar_version.go`, `edition.go`, `editors/vscode/*`: take theirs, then bump again in Task 14.
  - `topology.model.json`: take theirs wholesale, and regenerate in Task 17.
- [ ] **Step 2:** In `loop_graph.go`, delete the legacy switch walk (epic 3 deleted the step type). `Step.Function.Kind` now names the callee's kind, so use it and keep the registry lookup as the source of the concept. Walk `StepTypeBlock` / `Parallel` branches, `ForEach.Do`, `StepTypeExpression` and `StepTypeReturn` (pure), and `StepTypeEvent` (publish). In `loop_graph_source.go`, logic `Calls` come from the logic's compiled statement steps: read `function_loader.go`'s compile-at-load output, per memql-22's Task 10.
- [ ] **Step 3:** Re-run the tree measurement: zero problems. Then `make test` and the db-gated trees.
- [ ] **Step 4:** Commit the merge with the conflicts listed in its body, then `Issue #5381: the loop graph walks the statement form`.

## Task 14: The before-write body (Phase B, #5383)

- [ ] **Step 1:** DONE 2026-09-14: the epic-3 session reviewed the shape, and D-M records the answer (timing on the trigger, no wrapper block).
- [ ] **Step 2: Grammar (D-M).**
  - The `triggerKeys` registry gains `before` (`"create"|"update"|"write"`). `before` excludes `event` and `schedule` and requires `concept`.
  - The field-write statement kind `row.<field> = <expr>` (`ast.FieldWriteStatement{Field string; Value ExpressionNode; Span}`, kind `"fieldWrite"` in `BodyStatementKinds`) is parsed in `parseV1Assign`'s identifier path.
  - `row` is a root in a before-write automation.
  - Update every walker D-M lists, the drift corpus entries, and the tiers manifest row plus `writtenAs`.
  - Refusals: `before_write_writes`, `before_write_field`, `before_write_outside`, `before_write_trigger`.
  - Corpus cells: `cells/automation/trigger/` gains the `before=` cases, plus a `negative/automation/` case for each code.
- [ ] **Step 3: Compile.** A before-write automation compiles to `Automation.BeforeWrite = &BeforeWriteConfig{On: "create"|"update"|"write", Concept}` with its steps, where a field write is a new step type `fieldWrite {field, value leaf}`. The scheduler never subscribes it.
- [ ] **Step 4: Runtime.**
  - `component/memql/before_write.go` holds `SetBeforeWriteHooks(map[concept][]BeforeWriteHook)` on the engine.
  - `executeWrite` calls the hooks for the written concept and write kind after the read-merge and before validation. Each hook evaluates its filter on the incoming row, runs the statements through epic 3's in-process evaluator (`memql.EvalExpr` over a scope with `row` and `args` bound from the merged payload), and applies assignments to the payload.
  - Every node type's boot must register them (check which `app/build_*.go` run `engineAndBus`); add a test that a write on a node with automations loaded applies the hook.
- [ ] **Step 5:** The journal shape per D-M, the metric `memql_automation_before_writes_total{automation}`, and the static graph: a before-write automation has no out-edges (its writes are the triggering row's own version), and the check refuses `before_write_writes`.
- [ ] **Step 6: Forge.** `routeRequest` becomes the D-M form (`@trigger(before="create", concept="v1:forge:request")`, field writes for `status` and `approvedByUserId`); its advance mutation call goes. The `routed` audit event moves to a first-version automation `recordRouted` (`@trigger(event="node.created", concept="v1:forge:request")`, `@filter(row => args.firstVersion == true)`), which inserts the `requestEvent`, a different concept, so no cycle.
  - Test: submitting a request publishes exactly ONE `graph.node.created.v1:forge:request` event (capture the bus), the stored row carries the routed status, and the `requestEvent` row exists.
  - Test: a before-write body calling a mutation on another concept refuses at load (`before_write_writes`), which is the acceptance.
- [ ] **Step 7:** Bump the grammar on top of epic 3's: `GrammarVersion` gets the new slug and digest, `EditorRelease` moves to epic 3's release plus one minor, and `package.json`, the CHANGELOG entry and the drift and parity tests move with it. Run `make docs-matrix` if the matrix changes.
- [ ] **Step 8:** Commit `Issue #5383: an automation that only adjusts its triggering row declares a before-write body; forge routes in one write`.

## Task 15: Loop scenarios in the corpus (Phase B, #5384)

Put them in `test/conformance/2026/scenarios/loops/<shape>/`, on epic 3's scenario runner (`test/conformance/scenarios_db_test.go`, `scenario.json`; read its README and one existing suite first). There are three shapes, each with three scenarios:

| Shape | Refused at load | Permitted and converging | Stopped at the cap |
|---|---|---|---|
| **self-update** (forge's shape, a fixture copy) | no filter | `@loop(maxDepth=4, until=row => row.status == "done")` with `!=` in the filter, and a step that advances status one stage per fire and reaches `done` | the same automation with a filter whose predicate never becomes false at run time |
| **mutual pair** (the library pair plus the mirror) | refuses naming both edges | `@loop` on the mirror with a converging filter | not applicable |
| **self-publish** (an automation publishing the topic it triggers on; synthetic, because D14 refuses the logic variant at load) | refused | `@loop` with `until` on the payload | the chain reaches the cap |

- The **converging** scenarios assert `memql_automation_loops_stopped_total` stays at 0 for that automation, read through `metrics.AutomationLoopsStoppedValue` in the runner.
- The **stopped-at-the-cap** scenarios need a cycle the load cannot see, because every cycle the load can see is refused or bounded by `@loop` (whose maxDepth is itself at most the cap). So the cycle runs through a builtin, which the static graph treats as opaque: the fixture registers a test builtin in the runner that writes the triggering concept. The load passes, and at run time the chain stops at depth 16 with `loop_depth_exceeded` and the chain on the refused run. The counter `reason="depth"` rises by exactly 1. For the self-update shape, the run is also bounded by its `@loop` maxDepth before the cap; that variant asserts `reason="loop_bound"`.
- The runner gains the metric assertion if it lacks one: `expect.counters: [{automation, reason, delta}]`.

- [ ] **Step 1:** Write the scenario files and the runner hook. Run `MEMQL_REQUIRE_DB=1 ... go test -count=1 -p 1 github.com/znasllc-io/memql/test/conformance/ -run 'Scenario'`. Each scenario PASSES, and each negative control FAILS for the right reason: remove `@loop`, and the converging one is refused at load with `loop_cycle`.
- [ ] **Step 2:** Commit `Issue #5384: the three loop shapes as corpus scenarios: refused, converging, stopped at the cap`.

## Task 16: Documentation

- [ ] Update these files:
  - `docs/public/language/memql.md`: a "Loops" section with the static graph, strata, `@loop`, `@mode`, `before write`, and the runtime depth, dedup and budgets. Also the Automations section.
  - `docs/public/language/authoring-rules.md`: new rules, "an automation that writes what it triggers on needs a filter the write cannot satisfy, or @loop" and "prefer before write for adjusting the triggering row".
  - `docs/public/concepts/events.md`: the cause fields, and what does and does not carry them (D-B).
  - `docs/public/ai/llm-cost-control.md`: the automation table gains the three env values.
  - `docs/public/operate/env-vars.md` if it lists the budget.
  - `component/automations/arch.md`.
  - Root `CLAUDE.md`: one bullet under Automations naming `@loop`, `@mode`, `before write` and the depth cap. The root file is gate-scanned: run `go test -count=1 .`.
  - `component/language/CLAUDE.md`: the two annotations and the statement.
- [ ] `make docs-matrix` and `go test -count=1 .`.
- [ ] Commit `Issue #5380: the loop protection documented`.

## Task 17: Verification and delivery

- [ ] Delete this plan and its checkpoint (`git rm`) in the last commit before the PR is marked ready.
- [ ] Run every lane, in this order, and fix what fails:
  1. `make test`
  2. `MEMQL_REQUIRE_DB=1` db-gated trees (`scripts/ci/db-gated-packages.sh --trees`)
  3. `go test -count=1 .` and `./scripts/...`
  4. `make proto-gen-check`, `make sdk-gen-check`, `make env-registry-check`, `make concept-snapshot` (check), `make frontdoor-paths-check`
  5. `go run ./cmd/memqllint dsl/`, `make os-test`, `make os-build`
  6. `make arch-model` LAST, then `make arch-model && git status --short -- component/architecture/embedded/`, which must be empty
- [ ] Push `epic/dsl-v1-loop-protection` and open the PR. Its body carries:
  - one `Closes #n` line per issue
  - the decisions D-A to D-M
  - the behaviour changes: forge fires once, the fleet and deploypack filters, the dedup key, modes default
  - the false-positive count
  - the OS screenshots
  - a frontend-team note: the new builtins `automationGraph` and `automationLoopStops` are an SDK addition, not a contract change
- [ ] Arm a watch: `gh pr checks <n> --watch`, then `scripts/dev/merge-as-owner.sh --pr=<n> --check`, then enqueue with `gh pr merge <n> --repo znasllc-io/memql` (bare). Watch queue membership over GraphQL until the PR is MERGED, and handle DIRTY, BEHIND and dequeue per memory.
- [ ] After the merge:
  - Close any issue the `Closes` lines missed, with a comment naming the merge commit.
  - Remove this epic's worktree and local branch. The remote branch is deleted by the queue.
  - Report the peers' worktrees, never touching them.
  - Update the program memory.
