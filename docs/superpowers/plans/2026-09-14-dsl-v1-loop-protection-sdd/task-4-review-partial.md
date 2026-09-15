# Task 4 review: PARTIAL (checkpointed before completion)

Worktree `/home/znas/memql-projects/wt-loops-s4-runtime`, diff d8f335822..f3d5180e9. This is a read-only review. No tests were run.

## Spec Compliance

- Verdict so far: spec compliant. Every brief item is present: `loop_runtime.go`, `journal_loop.go`, the metric plus its registration, the direct `go.mod` require, the three executor call sites, the resume/adopt restore, `CauseFromMap`, the `terminal.go` code, reason and order, and both mandated test files including the cap-32 negative control.
- I judged the disclosed deviations on the merits and accept them:
  - The refusal is decided at the top and acted on after the claim, so a refused fire runs under its parent's cause. This keeps the `Cause.Chain` invariant ("never longer than the depth cap") that the literal brief would break.
  - `runCause` runs after the adopt id override.
  - `openRun` gained a `parent` parameter.
  - An adopted refusal is close-only.
  - The kind is restored on resume and adopt.
- Cannot verify from the diff: the db-gated test results and the `-race`/`-count=20` claims. I did not re-run them.

## Named risks

- **(a) ctx stamping: CHECKED, holds.**
  - The stamp is at `component/automations/executor.go:489-491`, before the budget, `automation.started`, the input query, the steps, onComplete/onError and `automation.failed`/`completed`.
  - Steps derive `stepExecCtx` from ctx (`executor.go:1084`), and parallel uses `WithCancel(ctx)`.
  - A synchronous sub-automation passes the step ctx (`steps/automation.go:136,138`).
  - An async sub-automation uses `context.Background()` (`steps/automation.go:67`). That is a chain break the header already documents.
  - A refused run runs under its parent's cause (`loop_runtime.go:203-208`), so nothing it publishes, journal rows included, carries a chain past the cap.
- **(b) sub-automation depth: CHECKED, holds.**
  - The synthetic event in `scheduler.go:264-269` has a zero Cause, so `runCause` falls back to the ctx cause (`loop_runtime.go:150-154`). The child runs at depth+1 with the correlation inherited. There is no reset to a root, so recursion through sub-automations cannot escape the cap.
  - The no-args path (`TriggerAutomation` -> `scheduleExecutor.Execute(ctx)` with a nil event) also reads the ctx cause.
- **(c) root correlation: CHECKED, holds.** It is a pure function of {topic, kind, payload}, the Timestamp is excluded, and a test pins it. The restatement is listed under Minor.
- **(d) refused-run row: CHECKED, holds.**
  - `refuseLoop` is `openRun` plus exactly one `updateWorkRun`; `closeRun` is never reached (`journal_loop.go:28-59`).
  - A journal-skipped automation writes nothing (`loop_runtime.go:239-245`).
  - `updateWorkRun` accepts outcome, errorCode, errorMessage and finishedAt (`dsl/work/mutations.memql`). `outcome` and `triggerEvent` are free `object` fields, so storage is unchanged.
  - A refused row cannot be resumed, because `ValidateRunJournal` refuses `FailedStep == ""` (`resume.go:231`).
- **(e) @loop bound: CHECKED, holds.** It counts this automation's entries in `priorRuns(run)`, which is the parent's chain. `Loop` is nil-checked, as is `MaxDepth > 0` (`loop_runtime.go:185-195`).
- **(f) concurrency: CHECKED by reading, not run.**
  - No new shared mutable state besides the metric.
  - `maxChainDepth()` reads the env per call.
  - The test fakes are either synchronous (the registry runs on the executor goroutine) or mutex-guarded (`lifecycleCapture`, `subAutomationRegistry`, `onceClaimer`).

## Issues

### Critical

None found.

### Important

None found in what I checked.

### Minor

1. **Correlation restates an existing projection.** `rootCorrelation` re-spells the {topic, kind, payload} projection (`loop_runtime.go:128-132`) instead of calling `eventFingerprintData` (`fingerprint.go:229-238`), which is the named rule. The two can drift apart.
2. **Undocumented chain breaks.** Resuming a no-args sub-automation, or a compiled goal's adopted run, restarts at depth 1 because no `triggerEvent.cause` is journaled (`journal.go:337-339` records a cause only inside a triggerEvent). The break is in the safe direction (under-count, never a false stop) and was disclosed, but the `WHERE THE CHAIN BREAKS` header (`loop_runtime.go`, lines 41-45) does not list it.
3. **Resume/adopt of a journaled root can inherit an unrelated cause.** `journalRunCause` (`loop_runtime.go:268-271`) goes through `runCause`'s ctx fallback, so a recorded ROOT (zero cause) would take any cause the resume/adopt ctx carries. It is harmless today: the only ctx stamps are in `loop_runtime.go` and `resume.go`, and the callers (HTTP resume, `app/integrations_work_dispatch.go:183,206`, the proving runner) hold no cause. It is a latent trap for a caller that resumes from inside a run.
4. **Report inaccuracy (deviation 6).** The report says restoring the kind makes a recovered run's `initialChainHead` match its first attempt. That is not true: `ComputeInitialChainHead` reads only topic and payload (`fingerprint.go:246-267`). The change is harmless, but the stated effect is wrong.
5. **Refusal lifecycle and logging (disclosed).** A refused run publishes `automation.started` but no terminal `automation.failed`. The scheduler also logs a generic ERROR (`scheduler.go:723-730`) beside the WARN. Both follow from the brief's `return exec, r`.
6. **Goal runs can share a correlation.** A compiled goal run's correlation derives from the synthetic `work.run.dispatched` event (`adopt.go:118-123`). Two goals with identical variables therefore share a correlation id. The derivation is plan-mandated (D-A) and has no effect on enforcement.
7. **`kindNamed` belongs in component/events.** It is a 256-value reverse scan of `Kind.String()` (`loop_runtime.go:275-285`). A `Kind` parser beside `String()` in component/events would not need the magic bound.
8. **Refused runs cascade one step into work-row automations.** Journal writes carry the run's cause, so a refused row's work-row events carry depth 16. A journal-skipped `v1:work:*` automation reacting to them would itself be refused at depth 17. The cascade is bounded: that automation writes no row but is WARNed and counted. My grep found no shipped automation triggering on `v1:work:run`, but the grep pattern may be incomplete.
9. **A refused fire still runs its input query first.** The input query and its onError hook run before the refusal is acted on (`executor.go:611-616`). The ordering is plan-mandated and unreachable today, because nothing populates `Automation.Input`.

## Not reached

- Ran no focused test (neither the db test nor `-race`).
- Did not check reported test output for noise.
- Did not check whether a sandbox/dry-run executor's refusal increments the production metric (`stopLoop` counts unconditionally, `loop_runtime.go:231`).
- Did not check how `@loop` `until` interacts with the bound.

## Assessment (provisional)

**Task quality:** Approved (provisional).
**Reasoning:** Every named risk (a) through (e) holds in the code, and (f) holds by reading. The deviations improve on the brief. The findings are Minor documentation, duplication and report-accuracy points, plus one latent resume/adopt ctx-fallback trap.
