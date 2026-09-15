# Task 12 report: the OS surface, Cluster > Automations (#5384)

Status: NOT STARTED (checkpointed during research). No code written, no commits.
Worktree `/home/znas/memql-projects/wt-loops-s5-surface` (branch `loops/s5-surface`) is clean at
HEAD `3564d0672`. No QA harness was built, so nothing was copied to the scratchpad and no screenshots exist.

## Checkpoint

### State of each part

| Part (brief / design) | State | What remains |
|---|---|---|
| `dsl/platform/concepts.memql` virtual concept `automationNode` | not started | Add it after `dataOrigin` (~line 339), documented like it. Types: concepts use `int!` (e.g. `seq int!`), not `integer!`; check before writing. No field name collides with the reserved list (`component/database/memory-nodes/constants.go`). |
| `dsl/common/builtins.memql` builtins `automationGraph`, `automationLoopStops` | not started | Place after `dataOrigins` (~line 572). Use `///` doc comments, as `dsl/work/builtins.memql` does. A `///` on a builtin resolves to its description, as the generated `CreateGoalArgs` doc shows. The doc-comment codemod leaves builtins' and seeds' `@description` alone, so either form passes the convergence gate. |
| `component/memql/automation_graph_read.go` (interface, setter, two executors) | not started | Plan below. |
| Registration in `executor_builtin.go:55` plus constants in `engine_types.go` (~473) | not started | |
| `dsl/work/queries.memql` `@serverOnly` query `workRunsStoppedByLoops`, plus its `server_only_parsed_test.go` want entry | not started | Template: `workRunsForAutomation` (queries.memql:58-70). |
| `component/automations/scheduler.go` `AutomationGraphRows()` (cached), and the `app/engine.go` wiring beside `SetAutomationCataloger` (line 312) | not started | |
| `dsl/rbac/seeds.memql` `read app:cluster/automations` for owner, developer and admin | not started | Copy the `app:cluster/logs` triple at lines 335-339, `@description` form as in that file. |
| OS: `settings.ts`, `ClusterApp.tsx`, `src/cluster/automations/*` (section, hook, layout, map, detail, stops) | not started | Invoke `frontend-design:frontend-design` first. |
| OS tests: `test/cluster/automations-layout.test.ts`, `automations-section.test.tsx` | not started | |
| Go test `component/memql/automation_graph_read_test.go` | not started | |
| Fan-out gates, `make os-test`, `make os-build`, browser pass and screenshots | not started | |

No gate has run. The worktree has no `clients/os/node_modules` yet, so `make os-test` runs `npm ci` first.

### Research findings, so the next session need not repeat them

**Engine**

- **The builtin capability gate exists.** `evaluateBuiltinFunctionExpression` (executor_builtin.go:157) calls `refuseBuiltinBelowRequiredCapability` before the handler. It refuses with `capability_not_held: "<name>" requires the read on app:cluster/automations capability; the caller (role "...") does not hold it`. Internal origin passes it.
  - The load-time vocabulary reads the seed declarations. The resource therefore exists once the three seeds are added.
- **Stops read context.** Follow `readinessEvaluateContext` (component/memql/readiness_write.go:~218):
  - a named synthetic `system:maintenance:<x>` actor with `Role: auth.RoleOwner`, `Unranked`, `Synthetic`, and `auth.ContextWithInternalOrigin`, replacing the caller
  - not `auth.MaintenanceActor`, which is keyed on automation names
  - `component/memql` is already in the root gate's per-package stamp allowlist (`call_origin_conformance_test.go:486`). Its reason string and the comment above it should name the new request-derived reader, and a test should assert that the context replaces the caller and that the query takes no caller argument (the readiness precedent).
  - `TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin` matches `query <name>(` in string literals and requires that file to reference `ContextWithInternalOrigin`.
- **The stops query.** Proposed form:

  ```
  @serverOnly
  @actor
  query run workRunsStoppedByLoops {
    filter row => row.errorCode == "loop_depth_exceeded" && actor.isClusterOwner == true
    sort "row.createdAt", "desc"
    paginate 50
    shape <a narrow new shape>
  }
  ```

  - The narrow shape is proposed as `workRunLoopStop`: row.id, row.createdAt, automationName, errorCode, finishedAt, outcome. `workRunFull` would carry the input, variables and triggerEvent payloads. Grep the name for uniqueness across `dsl/` first.
  - The `///` doc must contain "actor.userId": system runs have a present-and-empty owner, so an owner conjunct matches none of them.
  - Use the literal `"loop_depth_exceeded"` with a comment naming `work.TerminalLoopDepthExceeded`.
- **The refusal row shape.** The runtime stream's commit `f3d5180e9` on `loops/s4-runtime`, `component/automations/journal_loop.go`, writes:
  - `outcome.loop = {reason: "depth"|"loop_bound", depth, cap, correlationId, chain}`
  - `chain` holds the PRIOR runs `[{automation, runId}]`, oldest first, without the refused run. The refused automation is the row's `automationName`.
  - For `loop_bound`, `cap` is that automation's `@loop` maxDepth.
  - A parent run landing terminal with the code may carry no `loop`. The projection must then leave depth and cap absent, and the OS draws an em dash.
- **The wire shape.**
  - A top-level builtin reply is one id-keyed node map. The SDK's `Result.rows()` unwraps it and sorts by `createdAt` descending at ns precision. Stamp each stop node's CreatedAt from its finishedAt.
  - Egress bare-ifies any 4-segment canonical id (`wire_bareids.go`). A stop node with `Concept: "v1:work:run"` and a canonical ID reaches the client with a bare id. Concept ids (3 segments) and topics survive.
  - OS test fakes must answer builtins in this shape: `builtinReply(name, rows)` in `test/stores/harness.tsx:38` is the helper to copy.
- **The graph data.**
  - `GraphAutomation.Origin` is `unified:<file>:<name>`. Project the file with `automationFile(origin, name)` (loop_check.go:159).
  - `Trigger` is the folded topic, e.g. `graph.node.created.v1:forge:request`. `ast.SplitTriggerTopic` gives back (kind, concept). On the trigger kind: created means any write (MemQL is append-only), and updated means an update.
  - `LoopConfig{MaxDepth, Until}` and `ModeConfig{Kind, Max}` live in `types.go:190-202`.
  - The real embedded tree: 58 automations, 7 edges, 1 cycle (the routeRequest self-edge). About 50 stand alone.
- **The cache.** `Scheduler.automations` (the enabled, registered set) is filled only in `run()`. Build with `BuildLoopGraph(set, newFunctionSource(s.loader.functions, s.loader.registry), 0)` over that set, not `StaticGraph()`, which re-walks the tree through LoadAll.
  - Key the cache on the sorted (name, pointer) set of registered automations, so it rebuilds only when the set changes.
  - Put the pure projection in a new `loop_graph_rows.go`.
- **No scheduler wired.** The graph builtin should return an error rather than an empty list, because an empty list would read as "no automations". Every node type wires a scheduler in app/engine.go.

**OS**

- `useReading` (`src/cluster/reading.ts`) says that each reading settles on its own. `OriginsSection` uses two readings.
  - The brief says "one useReading over both builtins". The design needs the stops band to fail on its own ("absent is not zero").
  - Plan: `useAutomationGraph` wraps TWO `useReading`s behind one `reread`. Record the deviation.
- Kit: `Head` (meta slot), `Button tone="quiet" busy busyLabel="Reading"`, `Notice tone="error" sentence detail`, `Panel`, `Subhead`, `Field`, `Chip`, `Caption`, `Measure` / `absent()` for the em dash.
- The Deployables `map/layout.ts` is the pure-layout precedent. Follow its patterns: byte-order `compare`, `ellipsize` and `middleEllipsize` from character budgets, no DOM reads.
- The Cluster test harness (`test/cluster/harness.tsx`) mocks `../../src/live/connection` with `vi.mock`.
- QA harness rules (memory notes `os-surfaces-need-a-real-browser-pass` and `os-qa-harness-module-swap-and-role-ladder`):
  - a `resolveId` plugin with `enforce: "pre"`, swapping on the RESOLVED path
  - `setRoleLadder(SEEDED_LADDER)`
  - the `data-theme` attribute on `<html>`
  - its own `cacheDir` and `root: __dirname`
  - a free port from `ss -ltnp`
  - an open-intent query param, not scripted clicks
  - delete the harness before committing

### Gates run

None.

### Screenshots

None.

## Concerns

- The brief's "depth badge '16 of 16'" and the design's sentence "depth 17, past the cap of 16" disagree. The design is the settled direction.
- The brief asks for one `useReading` over both builtins, and the reading rule (and the design's separate failure state for the stops band) needs two. The plan above uses two readings behind one hook.
