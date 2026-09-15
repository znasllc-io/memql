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

## Checkpoint (second session)

Checkpointed at the coordinator's request (owner wrapping up). Worktree
`/home/znas/memql-projects/wt-loops-ui`, branch `loops/ui`. One commit on top of
`cb46bf0fb`, not pushed:

- `206772687` WIP -- Issue #5384: the automationGraph and automationLoopStops builtins, read on app:cluster/automations

The engine half is written and its own tests pass; it has NOT been through the
fan-out gates yet, hence WIP. The OS half is not started beyond one line in
`settings.ts`. No QA harness was built and no screenshots exist, so nothing was
copied to `.../scratchpad/automations-qa-harness/` or `.../automations-qa/`.

### State of each part

| Part | State | What remains |
|---|---|---|
| Engine: concepts | done | `v1:platform:automationNode` and a SECOND virtual concept `v1:platform:automationLoopStop` in `dsl/platform/concepts.memql` (see deviations). Run `make concept-snapshot` if its gate asks. |
| Engine: builtins | done | `automationGraph`, `automationLoopStops` in `dsl/common/builtins.memql`, `///` docs, `@sdk`, `@requiresCapability("read", "app:cluster/automations")`. Constants in `engine_types.go`, registered in `executor_builtin.go`. |
| Engine: query plus shape | done | `@serverOnly` `workRunsStoppedByLoops` (`paginate 50`, `actor.isClusterOwner == true` conjunct, literal "loop_depth_exceeded" with a comment naming `work.TerminalLoopDepthExceeded`) and shape `workRunLoopStop` (row.id, row.createdAt, automationName, errorCode, finishedAt, outcome). `server_only_parsed_test.go` want entry added (argument in the block comment, entry inside the alignment group so gofmt moves nothing). |
| Engine: graph rows + cache | done | `component/automations/loop_graph_rows.go`: pure `LoopGraphRows(g)` and `Scheduler.AutomationGraphRows()`, `BuildLoopGraph(set, newFunctionSource(loader.functions, loader.registry), 0)` over the registered set, cached on the sorted (name, pointer) key; one `graphRows graphRowsCache` field added to the `Scheduler` struct. |
| Engine: read + wiring | done | `component/memql/automation_graph_read.go` (interface, setter, both executors, `loopStopsReadContext` in the readinessEvaluateContext shape, `loopStopNode` projection, `loopFigure` via `core/num` with a `narrowing: SATURATE` note). Engine fields `automationGraphSource` and `loopStopRows` (a test seam, the `conceptRowCount` precedent) in `engine.go`. `app/engine.go` wires `SetAutomationGraphSource(a.automationScheduler)`. Root allowlist reason and comment for `component/memql` updated in `call_origin_conformance_test.go`. |
| Engine: seeds | done | Three `read app:cluster/automations` seeds (owner, developer, admin) in `dsl/rbac/seeds.memql`, AND the compiled mirror `appReadFloors` in `component/auth/rbac_model.go` (TestSeedMatchesCompiledMirror pins it; the brief did not list it). OS `settings.ts` names the section so TestOsRegistryRequiresMatchTheAppSeeds holds. |
| Engine: tests | done | `component/memql/automation_graph_read_test.go` (8 tests: capability gate over the real loaded builtins and seeded catalog, refused user/viewer with the `capability_not_held:` prefix and admitted owner/developer/admin as the reachable positive; graph projection; no-scheduler error; stop has exactly 8 keys and no payload; no-loop-record leaves depth/cap absent; no-id dropped; read context replaces the caller; query is serverOnly with no args). `component/automations/loop_graph_rows_test.go` (8 tests). TDD: both files were RED (compile) before the implementation. Negative controls run and restored: disabling the cache fails `BuiltOncePerRegisteredSet` ("an unchanged registered set was rebuilt"); leaking `input` into the stop fails `TestALoopStopCarriesNoPayload` naming the key and the leaked value. |
| Engine: fan-out gates | partial | PASSED: `go build github.com/znasllc-io/memql/...`; `go run ./cmd/memqllint dsl/` (OK, 308 files); `go test -count=1 github.com/znasllc-io/memql/component/memql/ -run 'Automation|Capability|RowAuthz|App|ServerOnly|Origin|OsRegistry'` (ok); `go test -count=1 github.com/znasllc-io/memql/component/automations/...` (ok); `go test -count=1 github.com/znasllc-io/memql/test/dslconformance/...` (ok); `go test -count=1 github.com/znasllc-io/memql/component/auth/...` (ok). NOT RUN: `make sdk-gen && make sdk-gen-check` (new builtins and concepts, so generated files ARE stale now); `npm ci && npm run build` in `sdk/ts`; `make concept-snapshot` if asked; `go test -count=1 .` and `go test -count=1 ./scripts/ci/...` (after git add; the commit is in, so run them now); the doc-comment/accept-stamp/required-sigil/same-domain-use codemods only if a convergence gate complains; `make arch-model` last (Go signatures moved). Optional but valuable: a db-gated test that refuses a real loop (`component/automations/journal_loop_db_test.go` has `openTestEngine`) and reads it back through `builtin automationLoopStops()`, proving the shape projects `outcome` and `finishedAt` end to end. |
| OS: section entry | partial | `settings.ts` entry added (between Data origins and Agents). `ClusterApp.tsx` has NO branch yet, so selecting Automations currently falls through to Readiness. Also add an Automations row to the floor table comment in `apps/registry.tsx`. |
| OS: hook | not started | `useAutomationGraph.ts`: two `useReading`s (graph, stops) behind one `reread` (controller ruling). Graph via `connection.query.automationGraph({}, {signal})`, stops via `automationLoopStops`, `result.rows()`. |
| OS: layout | not started | Pure `layout.ts` per the design (columns by stratum, width 220 gap 88, cards 196x56, cycle group first then barycenter, byte-order compare, character-budget ellipsis, edges as path strings, SCC enclosures, stand-alone list = automations with no edge in or out). |
| OS: map / detail / stops | not started | `AutomationsMap.tsx` (plain SVG, roving tab stop, arrows, Enter, Escape), `AutomationDetail.tsx` (own scroll column; replaces the map below 720px with a quiet back link), `LoopStops.tsx` (chain breadcrumbs, "12 more" expander, sentence per the design: "depth 17, past the cap of 16" / "the @loop on X allows 4 runs", em dash when depth/cap absent, empty state, failure Notice in the server's words). CSS in `index.css` under `.os-cluster-automations-*`. |
| OS: tests | not started | `test/cluster/automations-layout.test.ts`, `test/cluster/automations-section.test.tsx`; the Cluster harness answers flat rows, so the automation builtins must be answered in the builtin wire shape (`builtinReply` from `test/stores/harness.tsx`). |
| OS: os-test, os-build | not started | `make os-test` (runs `npm ci`), `make os-build`. `sdk/ts` needs `npm ci` + `npm run build` first in this fresh worktree. |
| Screenshots | not started | None taken. |

### Deviations from the brief, and findings the next session needs

1. **A second virtual concept, `v1:platform:automationLoopStop`.** A top-level
   builtin's reply is row-gated by concept on the CALLER's context
   (`engine.go` ~893, `filterRowAuthzBuiltinNodes`, fail-closed). A stop node
   under `v1:work:run` (the research's suggestion) is on the composite owner
   tier with an empty owner, so every developer and admin holding the
   capability would have read "No loop has been stopped" -- the audit-trail
   lie. The stop concept declares no tier; the capability is the wall.
2. **`AutomationGraphSource.AutomationGraphRows()` returns `([]map[string]any, error)`**,
   not the brief's error-less signature: a scheduler that has not registered
   yet, or whose loader has no function registry, has no graph, and an empty
   list would read "this node loaded no automations".
3. **Row fields beyond the brief's list:** `triggerKind`, `triggerConcept`
   (from `ast.SplitTriggerTopic`, so the client never parses a topic),
   `template`, `problem` (the loop_cycle refusal text), and `cycle.permitted`
   and `cycle.path`. The brief's `filter` field is **`triggerFilter`**: a
   concept line starting `filter` trips memqllint's retired-operator contract
   gate (`filter <predicate>` read as the retired form).
4. **Absent keys, never empty strings**, except `writes`, `publishes`,
   `opaque`, `edgesOut`, `cycle.members/permittedBy/path` which are always
   lists. `mode.max` is absent when the default applies.
5. **The OS cannot use the design's data tints.** The OS imports only the
   brand FACES (`brand/README.md`; `brand_shared_source_test.go` forbids
   redefining `--memql-*` under `clients/os/src`), so `--memql-data-string` /
   `--memql-data-number` are unreachable; adding `--os-data-*` roles is a
   theme-pack contract change. Plan: carry the data voice with the mono face
   and existing `--os-*` tokens only, and say so in the report. Likewise
   `--os-text-xl` does not exist (the OS scale stops at `--os-text-lg`); use
   `--os-text-lg` for the stratum numerals.
6. **Section files go under `clients/os/src/apps/cluster/automations/`**, not
   the brief's `src/cluster/automations/`: every Cluster section lives under
   `src/apps/cluster/<section>/`, and `src/cluster/` is the shared reading hook
   and runtime config.
7. **Mode copy presumes Task 7.** `@mode` parses but has no run-time effect
   until Task 7 lands; the detail's mode sentences describe the behaviour the
   epic ships in the same PR.
8. **A real-graph fixture for the browser pass is cheap**: the offline engine
   in `loop_tree_measure_test.go` (`memql.New` + `Init` + `loadFromTree`) plus
   `LoopGraphRows` dumps the real tree's rows as JSON; pair it with a
   synthetic rich fixture (undecided edges, a permitted @loop cycle, a refused
   cycle, stops with long chains) for the populated captures.

### Next, in order

1. `make sdk-gen && make sdk-gen-check`; `cd sdk/ts && npm ci && npm run build`;
   `make concept-snapshot` if asked; `go test -count=1 .` and
   `go test -count=1 ./scripts/ci/...`; fix and commit (drop the WIP prefix
   from the engine half, or add a follow-up commit).
2. Invoke `frontend-design:frontend-design`, then the OS half in the order of
   the table above, `make os-test`, `make os-build`.
3. The QA harness under `clients/os/qa/` per this report's research notes,
   screenshots to `.../scratchpad/automations-qa/`, harness copied to
   `.../scratchpad/automations-qa-harness/`, never committed.
4. `make arch-model` last.
