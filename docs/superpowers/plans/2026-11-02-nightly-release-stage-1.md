# Nightly release from main, stage 1: a train from main

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a repository can set `source: main`, so a train needs no pull request: it leaves from `main`, runs everything, and lands by approval or by itself.

**Architecture:** the release train coordinator (`component/pipelinerun/train*.go`) keeps its state machine, record, lease and compare-and-swap. A train from main has no cars, never rebuilds, may land after `main` moved, and follows a push to its own branch. Everything is off until a settings key names it.

**Tech Stack:** Go; MemQL DSL for the query and builtin; React and TypeScript for the OS panel.

**Spec:** `docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md`

## Global Constraints

- No key is written to `MEMQL_PIPELINES_TRAINS` by this work. Landing the code changes nothing on an installation with no `source` key.
- The `pull_requests` path behaves exactly as it does today. Every existing train test passes unchanged.
- A step takes one state transition under `casTrain`; GitHub is never called under the gate.
- Messages come from one composer, `component/pipelinenotify`, and mention only `ownerDiscordUserId`.
- Pull request bodies stay bounded: at most 50 commits listed.
- A machine is named nowhere new; a commit subject is escaped as any other repository text.

## Review Focus

- A repository with no `VERSION`, or a `VERSION` a person just changed on `main`: the train must still have a commit of its own, or GitHub refuses the pull request. A person's chosen version is kept, not bumped past.
- Landing conflicts because a person changed `VERSION` or a fix collides with newer `main`: hold with the reason; never retry the merge every minute.
- A push to the train branch while waiting for approval: an approval of the old head must not land the new one, and the train goes back to running.
- A night with nothing new after a cancelled night: no pull request, no message, and no row that blocks the next departure.
- More than 50 commits since the last release, or a comparison GitHub truncates: the body says how many it left out, and suspects are "not examined" rather than wrong.

---

### Task 1: Settings

**Files:**
- Modify: `component/pipelines/train.go`
- Test: `component/pipelines/train_test.go`

**Interfaces:**
- Produces: `TrainSourcePullRequests = "pull_requests"`, `TrainSourceMain = "main"`, `TrainLandingApproval = "approval"`, `TrainLandingAutomatic = "automatic"`; `TrainSettings.Source`, `TrainSettings.Landing` (always the resolved value, never empty); `func (s TrainSettings) FromMain() bool`; `func (s TrainSettings) LandsAutomatically() bool`.

- [ ] **Step 1: Write failing tests.** `TestParseTrainSettingsDefaultsToPullRequestsAndApproval` (an entry naming neither: `Source == "pull_requests"`, `Landing == "approval"`, `FromMain() == false`). `TestParseTrainSettingsReadsSourceAndLanding` (`source: main`, `landing: automatic`). Add to the `TestParseTrainSettingsRefuses` table: `source: "branches"`, naming `source`; `landing: "sometimes"`, naming `landing`; `landing: "automatic"` with no `source: main`, naming both.
- [ ] **Step 2: Run** `go test ./component/pipelines/ -run TrainSettings`. Expected: FAIL.
- [ ] **Step 3: Implement** in `ParseTrainSettings`, one refusal per key, each naming the repository.
- [ ] **Step 4: Run** the same. Expected: PASS.
- [ ] **Step 5: Commit** `feat(pipelines): name a train's source and how it lands in its settings`.

### Task 2: GitHub seam

**Files:**
- Modify: `component/identity/githubapp/train.go`, `component/pipelinerun/train_github.go`
- Test: `component/identity/githubapp/train_test.go`; fake in `component/pipelinerun/train_test.go`

**Interfaces:**
- Produces, on `Client` and `trainGitHub`: `CompareCommits(ctx, token, repository, base, head string) (commits []githubapp.ComparedCommit, total int, err error)`, with `ComparedCommit{SHA, Subject, Parent string}`, newest last, using `GET /compare/{base}...{head}?per_page=100`; `total` is GitHub's `total_commits`. `AddEmptyCommit(ctx, token, repository, branch, message string) (string, error)`: read the branch's head commit, `POST /git/commits` with its tree and itself as parent, `PATCH /git/refs/heads/{branch}` without force, return the new SHA.

- [ ] **Step 1: Write failing tests** `TestCompareCommitsReadsSubjectsAndTotal` (subject is the message's first line; a merge's `Parent` is its first parent) and `TestAddEmptyCommitKeepsTheTree`, against an `httptest` server, as the file's other tests do.
- [ ] **Step 2: Run** `go test ./component/identity/githubapp/ -run 'CompareCommits|AddEmptyCommit'`. Expected: FAIL.
- [ ] **Step 3: Implement** both, and the `packagesGitHub` wrappers. Add them to `trainRemote` in the pipelinerun test file: `commitsBetween map[string][]githubapp.ComparedCommit` keyed `base...head`, and `AddEmptyCommit` moving the branch with `r.nextSHA("empty")`, carrying its files.
- [ ] **Step 4: Run** the same and `go build ./component/pipelinerun/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(githubapp): read the commits between two heads and add an empty commit`.

### Task 3: A train's run is always full

**Files:**
- Modify: `component/pipelinerun/policy.go`
- Test: `component/pipelinerun/policy_test.go`

- [ ] **Step 1: Write failing tests.** `TestATrainRunIsFullWhateverThePolicyAnswers` (`Opening{Event: pull_request, Branch: "train/2026-11-02-1", PullRequest: 7}` gives `ModeFull` under the default policy). `TestAnOrdinaryPullRequestKeepsThePolicysMode` (`Branch: "fix/x"` gives `ModeAffected`). `TestAPolicyDecliningATrainStillDeclines` (a policy answering `""` stays `""`).
- [ ] **Step 2: Run** `go test ./component/pipelinerun/ -run 'TrainRunIsFull|OrdinaryPullRequest|PolicyDeclining'`. Expected: first FAILs.
- [ ] **Step 3: Implement** in `policyModeForEvent`, beside the merge-group rule: when the mode is not empty and `pipelines.IsTrainEvent(o.Event, o.Branch)`, answer `ModeFull`, with a comment saying a train's check stands for the whole release (spec D4).
- [ ] **Step 4: Run** `go test ./component/pipelinerun/ -run 'Mode|Policy'`. Expected: PASS.
- [ ] **Step 5: Commit** `fix(pipelines): a train's run covers everything, whatever the policy says of a pull request`.

### Task 4: Building a train from main

**Files:**
- Create: `component/pipelinerun/train_main.go`
- Modify: `component/pipelinerun/train.go` (`build` dispatches at its top)
- Test: create `component/pipelinerun/train_main_test.go`

**Interfaces:**
- Consumes: Task 1's `FromMain`; Task 2's `CompareCommits`, `AddEmptyCommit`.
- Produces: `func (i *Integration) buildFromMain(ctx, d, s, p, t) error`; `func lastLandedTrain(ctx, d, p) (*Train, error)` (newest with state `landed` and a `LandedSHA`, over `TrainsSince(now minus 90 days)`); `func trainMainPullRequestBody(p Pipeline, t Train, base, version string, commits []githubapp.ComparedCommit, total int, automatic bool) string`; a harness helper `newMainTrainHarness(t, extra string)`, whose settings add `"source":"main"` and `extra`.

- [ ] **Step 1: Write failing tests.**
  - `TestATrainFromMainLeavesWithNoPullRequests`: no PRs open; at the departure the train is `running`, `OnBoard` empty, `BaseSHA == baseSHA`, version `0.31.5`, and a pull request titled `Train train/...` is open.
  - `TestATrainFromMainWithNothingNewIsCancelledQuietly`: a landed train whose `LandedSHA == baseSHA`; the departure ends `cancelled`, reason `Nothing new on main since the last release.`, no pull request, no messages, and a later departure with a new head builds normally.
  - `TestATrainFromMainKeepsAVersionAPersonChose`: last landed `Version: "0.31.4"`, `VERSION` on main `0.32.0`; version is `0.32.0`, `VERSION` unchanged on the branch, the head differs from the base through an empty commit.
  - `TestATrainFromMainWithNoVersionFileStillHasACommit`: no `VERSION`; version is empty, the head differs from the base.
  - `TestATrainFromMainListsItsCommitsAndSaysHowManyMore`: 60 commits since the last landed; the body lists 50 and says `and 10 more`, and says nothing about approving when `landing` is `automatic`.
  - `TestATrainFromMainAdoptsItsOpenPullRequest`: a build that died after opening its pull request; the rebuilt tick reuses it.
- [ ] **Step 2: Run** `go test ./component/pipelinerun/ -run TrainFromMain`. Expected: FAIL.
- [ ] **Step 3: Implement** `buildFromMain`, following `build`'s scratch-ref order: base; last landed; cancel if `base == last.LandedSHA`; version; move the train branch; body; open or adopt; `casTrain` to running; the departed message. Version rule, in `trainVersionFromMain`: no parsable `VERSION` file, or a `VERSION` differing from `last.Version` when `last` is known, keeps what is there and calls `AddEmptyCommit` on the scratch ref with `chore: release <branch>`; otherwise `PutBranchFile` with the patch bump, as `trainVersion` does. `CompareCommits` from `last.LandedSHA` to `base`; with no last landed train, the body lists none.
- [ ] **Step 4: Run** `go test ./component/pipelinerun/ -run Train`. Expected: PASS, every existing test included.
- [ ] **Step 5: Commit** `feat(pipelines): build a release train from main with no pull requests on board`.

### Task 5: Landing from main

**Files:**
- Modify: `component/pipelinerun/train.go` (`observeRun`, `observeHeld`), `component/pipelinerun/train_land.go` (`awaitApproval`, `land`, `recordLanded`)
- Test: `component/pipelinerun/train_main_test.go`

**Interfaces:**
- Produces: `pipelines.TrainAutomaticApprover = "policy:landing-automatic"`, recorded as `ApprovedBy`.

- [ ] **Step 1: Write failing tests.**
  - `TestAMovedMainNeverRebuildsATrainFromMain`: waiting for approval and held; `main` moves; no rebuild, `Rebuilds == 0`, same head.
  - `TestATrainFromMainLandsAfterMainMoved`: approved, `main` moved; the pull request merges, state `landed`, and `LandedSHA` is the merge.
  - `TestAnAutomaticTrainLandsWhenItsRunPasses`: `landing: automatic`; a passing run takes it to `landed` with `ApprovedBy == "policy:landing-automatic"` and `ApprovedSHA` the head, and no awaiting-approval message goes out.
  - `TestARefusedMergeHoldsATrainFromMainAndDoesNotRetryEveryMinute`: GitHub answers 409; the train is `held` with the reason, and the next tick makes no merge call.
  - `TestAFailedRunOnAnAutomaticTrainDoesNotLand`: a failed run never reaches `landing`.
- [ ] **Step 2: Run** `go test ./component/pipelinerun/ -run 'MovedMain|LandsAfterMain|AutomaticTrain|RefusedMerge|FailedRunOnAnAutomatic'`. Expected: FAIL.
- [ ] **Step 3: Implement.** `rebuildIfAsked` answers false at once when `s.FromMain()`. `land` skips the moved-`main` rebuild when `s.FromMain()`. `observeRun`'s success goes to `TrainLanding` and calls `land` when `s.LandsAutomatically()`. `recordLanded` says `Landed main at <base> as <merge>.` for a train from main, and adds that it passed on its re-run when `t.Retried`.
- [ ] **Step 4: Run** `go test ./component/pipelinerun/ -run Train`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(pipelines): land a train from main by itself, even after main moved`.

### Task 6: Failing, fixing and following the head

**Files:**
- Modify: `component/pipelines/train_triage.go`, `component/pipelinerun/train.go`, `component/pipelinerun/train_main.go`
- Test: `component/pipelines/train_triage_test.go`, `component/pipelinerun/train_main_test.go`

**Interfaces:**
- Produces: `func TriageMainTrainFailure(failed []TrainFailedStep, retried bool) TrainVerdict` (Action `retry` when not retried and something failed; otherwise `hold`); `func SuspectCommits(g *Graph, commits []TrainCommit, failed []TrainFailedStep, fullGlobs []string) []TrainCommit`, with `TrainCommit{SHA, Subject string; Files []string; Complete bool}`; `func (i *Integration) followTrainHead(ctx, d, s, p, t, gh, token) (moved bool, err error)`.

- [ ] **Step 1: Write failing tests.**
  - `TestMainTrainTriageRetriesAnyFailureOnce` and `...HoldsTheSecond`, tabled with an ordinary exit and a timeout.
  - `TestSuspectCommitsAreThoseThatReachTheFailedPackages`: a commit under `core/num` reaches `component/memql`'s failed step, one under `component/other` does not, and an unbounded one (`go.mod`) is listed.
  - `TestATestFailureOnATrainFromMainRerunsOnceThenHolds`: first failure opens a failed-only re-run and sends a retrying message naming the step; the second holds, mentions the owner, and lists the suspect commit.
  - `TestATestFailureOnAPullRequestTrainIsStillNeverRetried`: the old rule, on a `pull_requests` train.
  - `TestAPushToAHeldTrainGoesBackToRunning`, and the same while running and while waiting for approval: head updated, `Retried` reset, `HeldReason` cleared, and `TestAnApprovalOfTheOldHeadDoesNotLandAMovedTrain`.
  - `TestAClosedPullRequestCancelsATrainFromMain`.
  - `TestASecondReplicaResumesATrainFromMain`: a second `Integration` over the same store continues from each of `running`, `held` and `landing`.
- [ ] **Step 2: Run** `go test ./component/pipelines/ ./component/pipelinerun/ -run 'MainTrain|SuspectCommits|TrainFromMain|PushToAHeld|OldHead|SecondReplica|StillNeverRetried'`. Expected: FAIL.
- [ ] **Step 3: Implement.** In `triage`, after the cancelled case and the failed-steps read, a train from main goes to `triageMain` and never reads `MainFailed`, `trainReach` or the rebuild cap. `followTrainHead` reads `BranchHead(t.Branch)` and, when it differs from `t.HeadSHA`, writes `HeadSHA`, state `running`, `Retried: false`, `RunID: ""`, `HeldReason: ""`, and answers true; a read error other than 404 returns the error, and a 404 leaves it to `settleClosedTrain`. Call it from `observeRun`, `observeHeld` and `awaitApproval` for a train from main, after `settleClosedTrain`. Suspects: at most 50 commits, each `CompareFiles(parent, sha)`, over the same graph `trainReach` reads; over 50, the message says they were not examined.
- [ ] **Step 4: Run** `go test ./component/pipelines/ ./component/pipelinerun/ -run Train`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(pipelines): a train from main re-runs a failure once, holds, and follows a fix`.

### Task 7: What a train from main says

**Files:**
- Modify: `component/pipelinenotify/train.go`, `component/pipelinerun/train.go` (`notifyTrain` sets the fields)
- Test: `component/pipelinenotify/train_test.go`

**Interfaces:**
- Produces: `TrainMessage.FromMain bool`, `TrainMessage.Commits int`, `TrainMessage.Suspects []string`; event `TrainRetrying TrainEvent = "retrying"`, not action-needed.

- [ ] **Step 1: Write failing golden tests.** Departed from main: headline `Release <branch> left from main`, description `<n> commits since the last release. The full run starts now.` Held from main: the todo says to push the fix to the release's branch or re-run its run, says closing cancels, and never mentions `train:hold`. Retrying: headline `Re-running failed steps of <branch> once`, the steps in the `Failed` field, no mention. A suspects field, escaped, with a subject carrying `@everyone` staying text.
- [ ] **Step 2: Run** `go test ./component/pipelinenotify/ -run Train`. Expected: FAIL.
- [ ] **Step 3: Implement** in `trainCopy` and `TrainDiscordMessage`.
- [ ] **Step 4: Run** the same. Expected: PASS, every existing golden test included.
- [ ] **Step 5: Commit** `feat(pipelines): say what a release from main is doing`.

### Task 8: Starting one by hand

**Files:**
- Modify: `component/pipelinerun/train_land.go` (`TrainExpress`), `dsl/pipelines/builtins.memql` (descriptions)
- Test: `component/pipelinerun/train_land_test.go`

- [ ] **Step 1: Write failing tests.** `TestStartingAReleaseFromMainByHand` (empty list on a train from main creates an express train with nothing requested, and the next step builds it). `TestAReleaseFromMainCarriesNoNamedPullRequests` (a named number is refused, saying so). `TestStartingByHandReplacesAHeldReleaseAndIsRefusedWhileRunning`. `TestAnExpressTrainFromPullRequestsStillNeedsANumber` (today's rule). `TestOnlyTheOwnerStartsAReleaseFromMain`.
- [ ] **Step 2: Run** `go test ./component/pipelinerun/ -run 'ReleaseFromMain|StartingByHand|StillNeedsANumber'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Read the settings before the empty-list check; skip boarding when `s.FromMain()`; the rest of `TrainExpress` is unchanged. Update the builtin's description, then regenerate what the repository generates from DSL descriptions if a test says so.
- [ ] **Step 4: Run** `go test ./component/pipelinerun/ -run 'Express|ReleaseFromMain|StartingByHand'`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(pipelines): start a release from main by hand`.

### Task 9: The OS panel

**Files:**
- Create: `clients/os/src/apps/cluster/releases/ReleaseTrainPanel.tsx`
- Modify: `clients/os/src/apps/cluster/releases/ReleasesSection.tsx`, `clients/os/src/apps/cluster/releases/model.ts`
- Test: beside the existing `clients/os` tests for this section

**Interfaces:**
- Consumes: `pipelineTrainsForOwner` (read) and `pipelinesTrainExpress` (`{repository, pullRequests: []}`). Confirm both reach the OS client; a query not exposed to the SDK gets `@sdk`, in this task.

- [ ] **Step 1: Write failing tests.** Shows the newest train per repository: its branch, state in words, version and held reason, linked to its pull request. **Start release now** calls `pipelinesTrainExpress` with an empty list, and a refusal shows its sentence. With no trains, the panel renders nothing. Follow the kit and `memql-ui-design`.
- [ ] **Step 2: Run** the section's test command. Expected: FAIL.
- [ ] **Step 3: Implement.** The button appears only for a repository whose train is not building or running; the engine refuses otherwise anyway.
- [ ] **Step 4: Run** the section's tests and the OS typecheck. Expected: PASS.
- [ ] **Step 5: Commit** `feat(os): show the release and start one by hand`.

### Task 10: The rules, the audit and the documents

**Files:**
- Modify: `scripts/ci/ruleset-drift.sh`, its test, `memql-package.yaml` (the audit steps), `docs/internal/ops/ruleset-baseline.md`, `CLAUDE.md`, `AGENTS.md`, `docs/public/operate/pipelines.md`, `docs/public/operate/pipelines-substrate.md` if it states the rule, `docs/internal/ops/harness-decisions.md`, `dsl/pipelines/concepts.memql` (descriptions), `docs/superpowers/specs/2026-10-27-release-trains-design.md` (a pointer)

- [ ] **Step 1: Write failing test.** The drift script's test asserts the new baseline: ruleset `26357803`, expecting `deletion`, `non_fast_forward`, `required_status_checks`; a second assertion, on `26357817` with no bypass, that `deletion` and `non_fast_forward` hold; and a required-check list that is empty reported as a warning naming the fix, not a pass.
- [ ] **Step 2: Run** the script's test. Expected: FAIL.
- [ ] **Step 3: Implement** the script and manifest change, removing the `ruleset-enforcement-copilot` step, whose ruleset no longer exists. Then rewrite `CLAUDE.md`'s Branch Workflow: pull `main`, run the change's tests, push to `main`; no pull request; no force push; a release leaves nightly. Add `source`, `landing` and the failure order to `pipelines.md`'s release trains section, and a decision entry to `harness-decisions.md`.
- [ ] **Step 4: Run** the repository's doc and manifest gates: `go test . -run 'Manifest|Docs|Claude'` and the drift test. Expected: PASS.
- [ ] **Step 5: Commit** `docs: record that code lands on main and a release leaves nightly`.

### Task 11: The product repository

**Files, in `memql-znas`:**
- Modify: `dsl/znasdelivery/automations.memql` (`znasDeliveryEventMode`), `CLAUDE.md`, `scripts/dev/merge-as-owner.sh`, any test pinning the policy

- [ ] **Step 1: Write failing test.** Beside the existing policy tests: `push` to `znasllc-io/memql` and `znasllc-io/memql-znas` answers no mode; `push` to another repository still answers `full`.
- [ ] **Step 2: Run** the product's DSL check: `cd ../memql && go run ./cmd/memqllint <abs-path>/dsl`, and the policy test. Expected: test FAILS.
- [ ] **Step 3: Implement.** The two repositories named in the policy, with a comment naming spec D6. `merge-as-owner.sh` reads the active ruleset that has the bypass, not the first.
- [ ] **Step 4: Run** the same. Expected: PASS.
- [ ] **Step 5: Commit** `feat(delivery): stop running on every push to main; the nightly release runs everything`.

## After the tasks

Run `make test` for the touched trees and the db-gated pipelinerun tree, then one fresh review of the whole branch, before anything lands.
