# Task 9 report: the tree's cycles fixed; the refusal flipped on (#5381)

Status: CHECKPOINT (interrupted by the coordinator before any edit). No commits.
Worktree `/home/znas/memql-projects/wt-loops-s3-graph`, branch `loops/s3-graph`, HEAD `3564d0672` (unchanged, `git status` clean).

## Checkpoint

The session so far went to reading and verifying. No file in the worktree has been changed, and nothing was committed. One throwaway probe test was written and deleted, and the tree is clean.

### Brief items

| Item | State | What remains |
|---|---|---|
| forge `routeRequest`: `firstVersion bool` in args plus `@filter(row => args.firstVersion == true)`; rewrite the "Loop safety" paragraph (dsl/forge/automations.memql:20-23) | not started | See "Tests that break with the forge fix" below |
| fleet: filters on the six instance automations; rewrite "NO LOOPS, BY CONSTRUCTION" (deploy/fleet/dsl/fleet/automations.memql:17-24) | not started, verified | Filters verified against the file (below) |
| deploypack: filters on `driveDeploymentInProgress` and `recordReconciledState` | not started | Read `examples/deploypack/dsl/logic.memql` to confirm which statuses each logic acts on |
| Re-measure after each bundle | not started | Run `go test -count=1 -v -run TestLoopCheckMeasureTheTrees github.com/znasllc-io/memql/component/automations/` |
| False-positive count | not started | None expected: Task 8 found 3 real cyclic components |
| Library prose (dsl/library/automations.memql:245-254) | not started | |
| The flip in unified_loader.go:291-299 (append `loopProblems` to `problems`) | not started | Also reword `logLoopCheckNow`'s "report-only" WARN |
| strict_automation_boot_test.go extension | not started | |
| Corpus negative case `refuse_load` `loop_cycle` | not started | Blocked on the runner (see finding 1) |
| Boot check through the measurement test | not started | |

### Carried items

| Item | State | What remains |
|---|---|---|
| 1. Exclude disabled automations | not started, designed | See finding 2 before writing it |
| 2. Wiring gate for `Functions: a.engine.Functions()` (app/engine.go:131-137), and "not run" at WARN (loop_check.go:62) | not started | Model it on app/automation_preflight_wiring_test.go (a source-text read of engine.go) |
| 3. Activation covers shipped + every active authored + candidate | not started, designed | See finding 3 |
| 4. Test: a non-@loop member's self-edge inside an SCC another member's @loop covers is refused | not started | `covered()` (loop_graph_scc.go:125) already refuses this, so the new test only pins it |

### Last measurement output

None was run in this session. The last measurement on record is Task 8's fix round 1 (task-8-report.md): 4 distinct problems.

- forge `routeRequest`, a self-edge.
- The fleet's five-member component {provisionInstanceOnCreate, reRenderInstanceOnShapeChange, resumeInstanceOnRequest, suspendInstanceOnRequest, tearDownInstanceOnRequest}, with all 25 edges decided.
- deploypack's {driveDeploymentInProgress, recordReconciledState}, with all 4 edges decided.
- The same problems again, re-counted in each bundle's run, with the refusal-text change.

## Findings from the reading (for whoever resumes)

1. **The corpus runner cannot see a loop_cycle yet.** `corpusAutomationProblems` (test/conformance/corpus_test.go:909-935) builds its loader without `Functions`, so the check logs "not run" and the negative case would come back `load_ok`. The conformance tests would then be red. The plan gives the runner's `Functions` to Task 10 (task-10-brief.md), but Task 10 also says "the Task 9 negative case runs through the runner", which contradicts Task 9 Step 2's green conformance run. There are two ways out:
   - (a) Task 9 wires the runner inline: after `LoadUnifiedConcepts`, run `memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))` then `eng.Init(memoryNodes.DefaultRegistry())`, as `measureLoops` does, and pass `Functions: eng.Functions()`. Task 10 would later swap in `NewOfflineEngine`.
   - (b) The negative case moves to Task 10.

   My plan was (a), flagged as a Task 10 overlap. With the runner wired, every `load_ok` automation cell is also loop-checked, so the whole corpus must be re-run after the flip.
2. **The authored scheduler wires a @disabled automation, and the core scheduler does not.** Probed and confirmed: `Activate` has no `IsEnabled()` check (authored_scheduler.go:172-233), and `CompileSource` of `@disabled ...` gives `IsEnabled()==false`, yet it activates. The core scheduler skips it (scheduler.go:489), so `TriggerAutomationWithArgs` cannot reach it as a sub-automation either.

   So if `BuildLoopGraph` drops disabled automations, `refuseCandidateCycle` must judge its authored copies (the candidate and the active entries) as enabled. Otherwise `@disabled` is a way past the activation check. Planned fix: drop `!a.IsEnabled()` in BuildLoopGraph's input loop (loop_graph.go:129-133), and set `Enabled = nil` on the authored copies in refuseCandidateCycle. Tests to add:
   - loop_graph_test: a cycle through a disabled automation is not refused, and enabling it refuses.
   - loop_check_test: a self-cycling `@disabled` authored candidate is still refused.
3. **The scheduler already holds its registry, so carried item 3 needs no new plumbing.** It is `s.entries` under `s.mu`, and each entry holds `automation *Automation` and `owner`. Two things to handle:
   - Entries carry NO Origin (CompileSource does not stamp one, and executor.go:1260 reads `automation.Origin`), so copy each entry and stamp `authored:<owner>:<name>` on the copy.
   - EXCLUDE the entry the candidate replaces, the same `authoredEntryKey(owner, name)`, since a re-activation is a version bump.

   A possible sharpening: `byName` (loop_graph.go:140-144) resolves a sub-automation call to the first automation of that name by (name, origin), and "authored:" sorts before "unified:". At run time, `TriggerAutomationWithArgs` resolves only the core scheduler's shipped automations (scheduler.go:253-259). So a non-authored automation of a name should win.
4. **Tests that break with the forge fix, because each relies on routeRequest being a cycle.**
   - `TestLoopGraph_RefusalText` (loop_graph_test.go:705-742) builds the problem from the tree's routeRequest. It needs a fixture copy of the unfiltered source.
   - The `reRoute` half of `TestAuthoredActivate_RefusesACandidateThatClosesACycle` (loop_check_test.go:199-214) goes through routeRequest. Re-point it at `recordTransition`: a candidate on node.created requestEvent that advances the request closes candidate -> recordTransition -> candidate. The `shipped` list and `forgeFunctions` then need `recordTransition` and `transitionEventKind`.
   - The premise of `TestAuthoredActivate_AdmitsACandidateBesideAShippedCycle` and the comment on `authoredForgeScheduler`. Give the shipped set a synthetic cycle.

   Runtime forge tests (conf_1847, conf_1859) call `ExecuteWithEvent` directly, which evaluates no filter, so they are unaffected.
5. **The fleet filters, verified against automations.memql and logic.memql:34-45.**
   - provisionInstanceOnCreate and reRenderInstanceOnShapeChange: `row.status == "provisioning"`.
   - suspendInstanceOnRequest: `row.status == "suspending"`.
   - resumeInstanceOnRequest: `row.status == "resuming"`.
   - tearDownInstanceOnRequest: `row.status == "tearing_down"`.
   - welcomeOnInstanceRunning: `row.status == "running"`.

   The settled literals are `markInstanceRunning` "running" (mutations.memql:359-371), `markInstanceSuspended` "suspended" and `markInstanceTornDown` "torn_down". `status` is a plain enum field, with no read-merge annotations.

   A pre-existing double fire, not a loop: markInstanceProvisioning's update publishes both created and updated, so provisionInstanceOnCreate and reRenderInstanceOnShapeChange both run on a tier change. Its doc says it "Fires on row CREATION". Adding `args.firstVersion == true` would fix that, but it changes behaviour when an instance is re-created with an existing id. My plan was to follow the controller (status filter only) and report it.
6. **Dead automations in the fleet bundle.** deploy/fleet/dsl/fleet/trial.memql (6) and billing.memql (4) declare automations the loader never loads, because it reads only `*/automations.memql` (unified_loader.go:123). This is pre-existing and outside the graph. Worth one line to the controller.
7. **After the flip, make the measurement test fail on any problem.** It is the only check that loads deploy/fleet/dsl and examples/*/dsl with `Functions`. It runs in the db-tests lane (component/automations is db-gated), and CI uses no `-short`.
8. **The break-glass text after the flip.** unified_loader.go:313 says "a node with problems will boot with those automations missing". That is untrue for a loops problem: the automation stays in `out` and loads. Reword it for phase loops.

## Files changed

None.

## Concerns

- Finding 1 needs a controller ruling if (a) is not acceptable.
- Finding 5's double fire and finding 6's dead automations are pre-existing, and not this task's to fix.
