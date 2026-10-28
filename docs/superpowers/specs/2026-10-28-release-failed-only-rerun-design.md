# Failed-only release re-runs

Status: design approved by the owner on 2026-10-28; this spec records it.

## Problem

A release run that fails one step cannot be recovered. Two rules make the
version unreleasable:

1. The saved release binds one run. `observe` in
   `integrations/release/build_scope.go` refuses any other run id ("native
   release observation changed its execution identity"), although
   `existingReleaseBuild` (`component/pipelinerun/release_build.go`) already
   reads the newest attempt of the release run key.
2. Candidate preparation accepts only receipts the release run executed itself.
   `releasePrepareCandidateWorkflow` (`dsl/cluster/automations.memql`) refuses
   every skip except `pipeline_not_affected`. A failed-only re-run
   (`pipelinesRerun` with `failedOnly`) marks each passed step
   `pipeline_passed_earlier` (`carryPassed`, `component/pipelinerun/driver.go`),
   so it can never become a candidate.

On 2026-10-28 this cost four engine versions (0.31.4 to 0.31.7). Each one-step
failure required a version bump, a PR check and a full release run of about
three hours, most of it per-step fleet wait and cloud start-up.

## Goal

After a release attempt fails, the owner starts a failed-only re-run from the
run page. The same saved release adopts that attempt and prepares its candidate
from it. The re-run executes only:

- the steps that failed, were blocked or were cancelled;
- every step whose earlier result produced artifacts.

Success means that today's failure (one darwin editor lane timing out) is
recovered by one failed-only re-run of the same version, with no version bump.

## Non-goals

- No automatic retry. The owner starts the re-run.
- No change to non-release events, except where code is shared.
- No cross-event reuse. A release run still never reuses push, pull request or
  merge-group evidence (#6344, `component/pipelinerun/evidence.go`).
- No candidate format change.
- No new OS control. The run page already offers "Re-run failed steps".

## Design

### 1. Re-run planning

`carryPassed` gains one release rule. When the run's event is `release`, a step
stays in the plan (is not carried) if either holds:

- its earlier row named artifacts (`WorkStep.ArtifactFileIDs` is not empty).
  Every artifact the candidate pins must come from the attempt being released.
- the release is no longer fresh: the first attempt of the release run key
  was opened (`queuedAt`) more than 24 hours before this attempt was opened. In
  that case nothing is carried and every step runs. `queuedAt` is used because
  every run has it, while `startedAt` is optional.

The 24-hour bound matches evidence reuse. It makes a late re-run check
vulnerabilities and everything else again. It is a chain-level bound: every
carried result is younger than the first attempt, so one comparison covers all
of them. `carryFromOriginal` reads the first attempt by walking `RerunOf`, and
logs why it carried nothing when the bound is exceeded.

All other carry rules stay as they are: same definition fingerprint, same
packages, command steps only, never SARIF or repository-change producers.

### 2. Attempt chain facts

`pipelines.ReleaseBuildRun` gains:

- `RerunOf string`;
- `Chain []ReleaseBuildAttempt`, with `ID`, `Attempt`, `Status`, `Conclusion`
  and `CancelRequested`, listing the earlier attempts newest first.

`existingReleaseBuild` fills both from the release runs it already reads for the
commit. An attempt above 1 must name, through `RerunOf`, an earlier attempt of
the same run key. A missing link, a cycle or more than 50 attempts breaks the
chain.
The chain never crosses run keys, so it never mixes events, modes or commits.
A broken chain leaves `Chain` empty instead of failing the observation, so a
release with irregular history keeps today's behavior: it observes, and it
refuses adoption.

### 3. Saved release adoption

In `observe`, when the newest attempt (O) differs from the recorded run (R), O
is adopted only if all of these hold:

- R is in O's chain;
- R's chain entry is `completed`, and its conclusion is not `success` or it was
  cancelled;
- the journal has no candidate (`CandidateID` is empty) and publication has not
  started.

Adoption replaces `record.Run` with O, and `recordRun` recomputes the phase
(`building`, `built` or `blocked`). Any other change of identity keeps today's
refusal.

The blocked reason becomes: "The selected release build did not succeed.
Re-run its failed steps from its run; this release continues with that attempt
when it passes."

Adoption reads the chain from the native observation, not from the journal.
A run without re-run facts encodes exactly as before (`RerunOf` and `Chain`
are omitted when empty), so journals written before this change keep their
selection digest and still decode on an older engine.

### 4. Carried receipt verification

In `coverage` (`integrations/release/candidate_coverage.go`), every receipt with
status `skipped` and code `pipeline_passed_earlier` must be proven from the
chain:

1. Resolve the carrying pipeline run from the work run's input
   (`pipelineRunId`). Walk `RerunOf`, at most 50 attempts and without cycles.
   Every attempt must match the carrying run's owner, pipeline, repository,
   commit, mode `full`, event `release`, version and tag. The walk must end at
   attempt 1.
2. Freshness: attempt 1 was opened (`queuedAt`) at most 24 hours before the
   carrying attempt was opened.
3. For the step key, take the nearest earlier attempt that executed it, moving
   back past attempts that carried it. Inspect that step with the new
   `pipelines.InspectCarriedReleaseReceipt`. It differs from
   `InspectReleaseStepReceipt` only in admitting an enclosing work run that
   failed or was cancelled, because the earlier attempt failed elsewhere. It
   requires a `done` command with exit 0 and no artifact intents.
4. The executing receipt must have the same definition digest and the same
   packages (`call.packages`) as the carried one.
5. The carried item reports
   `carriedFrom {pipelineRunId, workRunId, attempt, receiptDigest}`.

If any step fails these checks, coverage returns an error naming the step and
the reason, and candidate preparation fails natively. Artifact producers are
already covered, because rule 1 re-runs them and coverage keeps requiring every
artifact producer inside the selected run.

### 5. Engine recipe

`releasePrepareCandidateWorkflow` accepts a skip only when it is one of these:

- `pipeline_not_affected`, as today;
- `pipeline_passed_earlier` together with `carriedFrom`.

Every other skip still refuses.

### 6. Documentation

- `docs/public/operate/release-cutting.md`: replace "A failed build stays linked
  to its failed run" with the recovery path.
- `docs/internal/ops/harness-decisions.md`: a new entry giving the trigger,
  evidence, chosen path, alternatives, stop conditions, invariants and tests.
- Code comments: `evidence.go` (the "cannot adopt a re-run" sentence),
  `driver.go`, `release_build.go`, `build_scope.go` and `candidate_coverage.go`.

## Invariants

- Release runs never reuse another event's receipts.
- A successful or running recorded attempt is never replaced.
- A prepared, approved or published candidate is never re-pointed.
- Every artifact in a candidate comes from the run the candidate names.
- A carried step is accepted only with a native proof, tied by digest to a step
  that executed within the same release run key.
- A chain older than 24 hours carries nothing.

## Testing

Unit tests (`make test`):

- `carryPassed` for a release run:
  - an artifact producer runs again;
  - a stale chain carries nothing;
  - non-release runs are unchanged.
- The chain built by `existingReleaseBuild`: a good chain, a missing link, a
  cycle, the 50-attempt bound and another run key.
- The adoption decision as a pure function:
  - a descendant of a failed attempt is adopted;
  - a successful or running recorded attempt is kept;
  - a recorded run outside the chain is refused;
  - an existing candidate or started publication is refused.

DB-gated tests (`MEMQL_REQUIRE_DB=1`, local TimescaleDB, then CI's db-tests
lane):

- Coverage accepts a verified carry.
- Coverage refuses each of these:
  - artifacts;
  - a changed definition or packages;
  - another commit or event;
  - a stale chain;
  - a missing or failed original step;
  - a chain longer than 50 or cyclic.
- The recipe accepts a verified carry and refuses a bare
  `pipeline_passed_earlier`.

## Rollout and limits

- This ships in the next engine release. Production benefits once it runs that
  engine. The release that carries the change is still prepared under the
  previous rules.
- No data migration. A run without re-run facts encodes as before, so existing
  journals keep their selection digest. An adopted re-run writes `RerunOf` and
  `Chain`, which an older engine cannot decode: rolling back after adopting a
  re-run leaves that one saved release unreadable to the older engine.
- Steps with artifacts, including editor lanes that keep reports, always run
  again. Carrying them would require a candidate that spans attempts.

## Alternatives considered

- **A candidate that spans attempts.** Carry everything and cover attempts 1
  and 2. Rejected: attempt 1's complete evidence includes the failed step,
  artifacts would be pinned from a failed run, and the candidate format changes.
- **Adopt full re-runs only.** Smallest change. It saves the version bump and PR
  check but not the long run. The design adopts full re-runs too; they simply
  carry nothing.
