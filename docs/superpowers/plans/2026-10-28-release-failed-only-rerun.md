# Failed-only Release Re-runs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a release attempt fails, a failed-only re-run of the same release
is adopted by the saved release and becomes its candidate, so the same version
ships without a bump or a full rebuild.

**Architecture:** The pipeline runner keeps artifact-free passes and re-runs
artifact producers. Each release run reports its attempt chain. The saved
release adopts a descendant of a failed recorded attempt. Candidate coverage
proves each carried receipt against the attempt that executed it, and the
engine recipe accepts only proven carries.

**Tech Stack:** Go 1.26; MemQL DSL (`dsl/cluster/automations.memql`); TimescaleDB
for the db-gated tests.

**Spec:** `docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md`

## Global Constraints

- Release runs never reuse push, pull request or merge-group evidence. Leave
  `reuseVerification` in `component/pipelinerun/evidence.go` as it is.
- The bound is 24 hours between the first attempt's `queuedAt` and the carrying
  attempt's `queuedAt`. `queuedAt` is required on every run; `startedAt` is not.
- A chain is at most 50 attempts. A missing link, a cycle, a different run key
  or an attempt that does not decrease yields no chain. Observation keeps
  working, and adoption refuses.
- Adopt only when all hold: the recorded run is in the chain; the recorded run
  is `completed` with a conclusion other than `success`, or it was cancelled;
  `CandidateID` is empty; `PublicationStarted` is false.
- Blocked reason, verbatim: "The selected release build did not succeed. Re-run
  its failed steps from its run; this release continues with that attempt when
  it passes."
- Verify with `make test`. The db-gated cases run with `MEMQL_REQUIRE_DB=1` and
  `MEMQL_DATABASE_DSN` pointing at the local `memql-testdb` container
  (127.0.0.1:55432). Never verify with `go test ./...`.
- Stage files by explicit path. No emojis in docs. A harness change updates
  `docs/internal/ops/harness-decisions.md` in the same change.

## Review Focus

1. A re-run opened while the recorded attempt is still running must not be
   adopted. The journal keeps the running attempt. (Task 4 test.)
2. A step carried twice, from attempt 1 through attempt 2 into attempt 3, must
   verify against attempt 1's executed receipt. (Task 5 test.)
3. Attempt 2 of a release (one that failed and was itself carried) must not be
   accepted as the executing receipt. Only a `done` command counts. (Task 5
   test.)
4. A GitHub "Re-run" on a release check (trigger `rerun`, not `manual`) builds
   the same chain. Lineage depends only on `rerunOf`. (Task 2 test.)
5. A candidate prepared from attempt 1 before this change must still verify.
   Nothing carried means no new reads. (The existing db tests stay green.)

---

### Task 1: Carried receipt inspector

**Files:**
- Modify: `component/pipelines/release_evidence.go`
- Test: `component/pipelines/release_evidence_test.go`

**Interfaces:**
- Produces: `func InspectCarriedReleaseReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error)`.
  It admits an enclosing work run whose status is `succeeded`, `failed` or
  `cancelled`, including `cancelRequested`. It requires a `done` command step
  with exit 0 and no artifact intents. Every other check is that of
  `InspectReleaseStepReceipt`.

- [ ] **Step 1: Write failing tests.**
  - `TestCarriedReleaseReceiptAdmitsAFailedEnclosingRun`: the existing fixture
    with the run status set to `failed`, and also `cancelRequested: true`. It
    returns `Status == "done"` and the same `ReceiptDigest` as
    `InspectReleaseStepReceipt` gives for a succeeded run.
  - `TestCarriedReleaseReceiptRefusesWhatCannotBeCarried`: each case errors:
    artifact intent IDs present; status `skipped`; `pipelineKind` `notify`; exit
    code 1; `callerSuppliedPayload: true`; run status `running`.
- [ ] **Step 2: Run them and watch them fail.**
  Run: `go test github.com/znasllc-io/memql/component/pipelines -run 'CarriedRelease' -count=1`
  Expected: FAIL, `InspectCarriedReleaseReceipt` undefined.
- [ ] **Step 3: Implement.** Replace `inspectPipelineStepReceipt`'s
  `scanner bool` with a three-value admission (`release`, `scanner`, `carried`).
  For `carried`: admit `succeeded`, `failed` and `cancelled` runs; skip the
  `CancelRequested` refusal; then require a done command, exit 0 and no artifact
  intents. Keep `InspectScannerReceipt`'s behavior byte-identical.
- [ ] **Step 4: Run the package.**
  Run: `go test github.com/znasllc-io/memql/component/pipelines -count=1`
  Expected: PASS.
- [ ] **Step 5: Commit**, `feat(pipelines): inspect a carried release receipt
  inside a failed attempt`.

### Task 2: Attempt chain facts

**Files:**
- Modify: `component/pipelines/release_build.go` (`ReleaseBuildRun`)
- Modify: `component/pipelinerun/release_build.go` (`existingReleaseBuild`,
  `projectReleaseBuild`)
- Test: `component/pipelinerun/release_build_test.go`

**Interfaces:**
- Produces:
  ```go
  type ReleaseBuildAttempt struct {
  	ID                 string
  	Attempt            int
  	Status, Conclusion string
  	CancelRequested    bool
  }
  ```
  `ReleaseBuildRun` gains `RerunOf string` and `Chain []ReleaseBuildAttempt`,
  listing earlier attempts newest first. `Chain` is nil when the latest attempt
  is 1 or the chain is broken.
- Produces: `func releaseAttemptChain(latest Run, byID map[string]Run) []pipelines.ReleaseBuildAttempt`
  in `component/pipelinerun/release_build.go`.

- [ ] **Step 1: Write failing tests**, unit tests over `Run` values:
  - `TestReleaseAttemptChainFollowsRerunOf`: attempts 3 to 2 to 1 give the
    chain IDs `[a2, a1]` with their statuses. Attempt 2 has trigger `rerun`.
  - `TestReleaseAttemptChainIsEmptyWhenBroken`: each of these gives `nil`: a
    missing link, a cycle (a2 to a3), 51 attempts, a different `RunKey`, and an
    attempt that does not decrease.
- [ ] **Step 2: Run and watch them fail.**
  Run: `go test github.com/znasllc-io/memql/component/pipelinerun -run 'ReleaseAttemptChain' -count=1`
  Expected: FAIL, undefined.
- [ ] **Step 3: Implement.** `existingReleaseBuild` builds `byID` from the
  release rows it already validates. It sets
  `RerunOf = bareID(latest.RerunOf)` and
  `Chain = releaseAttemptChain(*latest, byID)`.
- [ ] **Step 4: Run the package.**
  Run: `go test github.com/znasllc-io/memql/component/pipelinerun -count=1`
  Expected: PASS, including `TestReleaseBuildObservesAndDrivesSameRunOnAnotherReplica`.
- [ ] **Step 5: Commit**, `feat(pipelinerun): report a release run's attempt
  chain`.

### Task 3: Release re-run planning

**Files:**
- Modify: `component/pipelinerun/driver.go` (`carryFromOriginal`, `carryPassed`)
- Modify the call sites: `component/pipelinerun/definition_test.go`, `driver_test.go`
- Test: `component/pipelinerun/driver_test.go`

**Interfaces:**
- Changes `carryPassed(tracks []*stepTrack, prior []WorkStep, priorAttempt int, release bool)`.
  When `release` is true, a row with `len(ArtifactFileIDs) > 0` is not carried.
- Produces `const releaseCarryWindow = 24 * time.Hour` and
  `func (dr *runDriver) releaseChainFresh(ctx context.Context) (bool, error)`.
  It walks `RerunOf` through `Store.RunByID`, at most 50 steps, to attempt 1.
  It answers `attempt1.QueuedAt >= dr.run.QueuedAt - releaseCarryWindow`. A read
  error stops the drive, as `carryFromOriginal` already does; a broken chain
  answers false.

- [ ] **Step 1: Write failing tests.**
  - `TestCarryPassedReleaseRerunsArtifactProducers`: with `release = true`, a
    passed step with `ArtifactFileIDs` is not carried and a passed step without
    them is. With `release = false`, both are carried.
  - `TestReleaseRerunCarriesNothingPastTheWindow`: in the drive harness, a
    failed-only re-run of a release whose attempt 1 was queued 25 hours before
    the re-run runs every step, with no `pipeline_passed_earlier`. At 23 hours
    the passed artifact-free step is carried.
- [ ] **Step 2: Run them and watch them fail.**
  Run: `go test github.com/znasllc-io/memql/component/pipelinerun -run 'CarryPassedRelease|ReleaseRerunCarriesNothing' -count=1`
- [ ] **Step 3: Implement.** In `carryFromOriginal`, when
  `dr.run.Event == pipelines.EventRelease`, ask `releaseChainFresh` first. If it
  answers false, log `pipelines: a release re-run past its 24-hour window runs
  every step` and carry nothing. Pass `release` through to `carryPassed`.
  Update the doc comments to say why.
- [ ] **Step 4: Run the package.**
  Run: `go test github.com/znasllc-io/memql/component/pipelinerun -count=1`
  Expected: PASS, including `TestCarryPassedCarriesOnlyIdenticalPasses`.
- [ ] **Step 5: Commit**, `feat(pipelinerun): a release re-run re-runs artifact
  producers and stale chains`.

### Task 4: Saved release adoption

**Files:**
- Create: `integrations/release/build_adopt.go`
- Modify: `integrations/release/build_scope.go` (`observe`, and the reason in `recordRun`)
- Test: `integrations/release/build_adopt_test.go`

**Interfaces:**
- Consumes: `pl.ReleaseBuildRun.Chain` (Task 2).
- Produces: `func adoptableBuildRun(record releaseBuildRecord, observed pl.ReleaseBuildRun) bool`.
  It returns true only under the Global Constraints' adoption rule.

- [ ] **Step 1: Write failing tests**, as a table in
  `TestAdoptableBuildRun`:

  | Case | Adopted? |
  | --- | --- |
  | Descendant of a failed recorded attempt | yes |
  | Descendant of a cancelled recorded attempt | yes |
  | Recorded attempt succeeded | no |
  | Recorded attempt still `running` (Review Focus 1) | no |
  | Recorded run not in the chain | no |
  | Empty chain | no |
  | `CandidateID` set | no |
  | `PublicationStarted` set | no |
  | Observed ID equals the recorded ID | no; that is not an adoption |

- [ ] **Step 2: Run and watch it fail.**
  Run: `go test github.com/znasllc-io/memql/integrations/release -run 'AdoptableBuildRun' -count=1`
- [ ] **Step 3: Implement.** In `observe`, keep today's error unless
  `adoptableBuildRun` returns true. On adoption, set `record.Run = &run` and fall
  through to `recordRun`. Replace the blocked reason with the verbatim text.
  Comment the trigger, evidence, refusals and the invariant.
- [ ] **Step 4: Run the package**, with the database.
  Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<local> go test github.com/znasllc-io/memql/integrations/release -run 'AdoptableBuildRun|ReleaseBuild' -count=1`
  Expected: PASS.
- [ ] **Step 5: Commit**, `feat(release): the saved release adopts a failed-only
  re-run of its own run`.

### Task 5: Carried receipt verification

**Files:**
- Create: `integrations/release/candidate_carried.go`
- Modify: `integrations/release/candidate_coverage.go`
- Test: `integrations/release/candidate_carried_db_test.go`

**Interfaces:**
- Consumes: `pl.InspectCarriedReleaseReceipt` (Task 1).
- Produces:
  ```go
  type candidateCarriedFrom struct {
  	PipelineRunID string `json:"pipelineRunId"`
  	WorkRunID     string `json:"workRunId"`
  	Attempt       int    `json:"attempt"`
  	ReceiptDigest string `json:"receiptDigest"`
  }
  ```
  `candidateSkippedStep` gains `CarriedFrom *candidateCarriedFrom` with the JSON
  tag `carriedFrom,omitempty`.
- Produces: `func (r candidateEvidenceReader) carriedFrom(ctx context.Context, owner, workRunID string, carried pl.ReleaseWorkReceipt) (candidateCarriedFrom, error)`.
  It applies spec section 4 rules 1 to 5 through `workRunForOwner`,
  `workStepForOwnerRun` and `pipelineRunForOwner`, with the 24-hour
  `queuedAt` bound.

- [ ] **Step 1: Write failing db-gated tests.** The fixture follows
  `profileCompletedRun`: `createPipelineRun` for attempt 1 (status `completed`,
  conclusion `failure`) and attempt 2 (`rerunOf` set to attempt 1, the same
  `runKey`, event `release`, mode `full`), each with a journal work run. In
  attempt 1, `test.full` is `done` and the work run fails. In attempt 2,
  `test.full` is skipped `pipeline_passed_earlier` and the run succeeds.
  - `TestCarriedReceiptVerifiesAgainstItsExecutingAttempt`: coverage reports
    `Skipped[0].CarriedFrom.Attempt == 1`, with attempt 1's receipt digest.
  - `TestCarriedReceiptThroughTwoAttempts` (Review Focus 2): attempt 3 carries
    from attempt 2, which carried from attempt 1. `CarriedFrom.Attempt == 1`.
  - `TestCarriedReceiptRefusals`: each case errors, naming the step:
    - artifacts on the original;
    - a different definition fingerprint;
    - different `call.packages`;
    - a different commit, event or run key;
    - attempt 1 queued 25 hours before attempt 2;
    - the original step `failed` (Review Focus 3);
    - a missing `rerunOf` link;
    - a 51-attempt chain.
- [ ] **Step 2: Run and watch them fail.**
  Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<local> go test github.com/znasllc-io/memql/integrations/release -run 'CarriedReceipt' -count=1`
- [ ] **Step 3: Implement.** In `coverage`, for each receipt with status
  `skipped` and code `pipeline_passed_earlier`, call `carriedFrom`. Set
  `CarriedFrom`, or return an error naming the step and the reason.
- [ ] **Step 4: Run the package**, with the database.
  Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<local> go test github.com/znasllc-io/memql/integrations/release -count=1`
  Expected: PASS, including `TestCandidateEvidenceThroughRealJournalAndOwnerQueries`.
- [ ] **Step 5: Commit**, `feat(release): prove a carried receipt against the
  attempt that executed it`.

### Task 6: Engine recipe, docs and full verification

**Files:**
- Modify: `dsl/cluster/automations.memql` (`releasePrepareCandidateWorkflow`)
- Modify: `integrations/release/candidate_prepare_db_test.go`
  (`TestCandidateWorkflowChoosesAllowedPlannedSkips`)
- Modify: `component/pipelinerun/evidence.go` (its comment)
- Modify: `docs/public/operate/release-cutting.md`,
  `docs/internal/ops/harness-decisions.md`

- [ ] **Step 1: Write the failing test.** Add the case
  `pipeline_passed_earlier with carriedFrom`, where
  `evidence.skips[0].CarriedFrom` is set; it qualifies (`State == "ready"`). The
  existing bare `pipeline_passed_earlier` case keeps refusing.
- [ ] **Step 2: Run and watch it fail.**
  Run: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<local> go test github.com/znasllc-io/memql/integrations/release -run 'ChoosesAllowedPlannedSkips' -count=1`
- [ ] **Step 3: Implement the recipe:**
  `facts.skipped.any(item => item.code != "pipeline_not_affected" && !(item.code == "pipeline_passed_earlier" && item.carriedFrom != nil))`
  Update its comment. Then run
  `go test github.com/znasllc-io/memql/component/automations/steps -run TestAutomationCorpusRuns -count=1`.
  If only `releasePrepareCandidateWorkflow`'s golden differs, rewrite it with
  `-update` and read the diff.
- [ ] **Step 4: Write the docs.**
  - `release-cutting.md`: replace "A failed build stays linked to its failed
    run..." with the recovery path: re-run failed steps from the run; what runs
    again; the 24-hour bound; when adoption refuses.
  - `harness-decisions.md`: a new entry "A failed release re-runs only what
    failed (2026-10-28)" giving the trigger, evidence, chosen path,
    alternatives, stop conditions, invariants, tests and limits.
  - `evidence.go`: replace "a release request cannot adopt a re-run".
- [ ] **Step 5: Run the full verification.**
  Run `make test`, then
  `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=<local> go test -count=1 github.com/znasllc-io/memql/integrations/release/... github.com/znasllc-io/memql/component/pipelinerun/...`.
  Expected: PASS.
- [ ] **Step 6: Commit**, `feat(release): accept a proven carried receipt in the
  candidate recipe`, with the docs.
