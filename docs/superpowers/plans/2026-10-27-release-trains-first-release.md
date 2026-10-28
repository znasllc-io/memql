# Release Trains, First Release: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ready pull requests collect on one train branch, run the pipeline once together, and land on `main` only through the owner's GitHub approval of the app-authored train pull request, with Discord messages for every status and every action the owner must take.

**Architecture:** The decisions are pure functions in `component/pipelines` (standard library only, table tests). The effects live in `component/pipelinerun`: a one-minute scheduled tick on the agent node holding the `pipelines-trains` lease reads each configured repository's newest `v1:pipelines:train` record, acts through GitHub with the app's installation token, and writes the record back by compare-and-swap under a repository gate. Discord messages go through the existing channel and outbound-worker path. Settings come from the global variable `MEMQL_PIPELINES_TRAINS`.

**Tech Stack:** Go 1.26 (multi-module workspace), MemQL DSL edition 2026, GitHub REST v3, Discord webhooks via `component/outbound`, Python 3 standard library (memql-znas cost guard).

**Spec:** `docs/superpowers/specs/2026-10-27-release-trains-design.md` (sections 4 to 12; section 10 "First release" is this plan's scope).

## Global Constraints

- A model never approves. Landing requires the owner's latest GitHub review on the train pull request to be `APPROVED` with `commit_id` equal to the train pull request's current head.
- One train per repository at a time. Branch `train/<yyyy-mm-dd>-<n>`, pull requests merged in pull request number order, version file `VERSION` gets patch + 1 unless a pull request on board changed it.
- `maxRebuilds` defaults to 2; after that many rebuilds in one departure the train is `held`.
- Boarding: open, not a draft, base is the pipeline's default branch, head branch not `train/*`, `MemQL / memql` check completed `success` on the current head, no `train:hold` and no `train:dropped` label.
- Discord: only `ACTION NEEDED` messages mention anyone, and only the configured `ownerDiscordUserId` (digits, 17 to 20 characters). `allowed_mentions.parse` stays `[]`. Text labels `ACTION NEEDED`, `FAILED`, `PASSED`; never emojis. Pull request titles and reasons go through the existing escaper (`escapeDiscord`, `oneLineText`, `fitRunes` in `component/pipelinenotify/notify.go`).
- Gate discipline: never nest gates and never call GitHub under a gate (`TestEveryGatedPathKeepsTheGateDiscipline`, `component/pipelinerun/gate_discipline_test.go:115`).
- Cross-node: every train transition is a fresh read (`memql.ContextWithFreshRead`) and a compare-and-swap under `TrainGateKey(repository)`; a coordinator that moves replica resumes from the record.
- No new HTTP endpoint, no GitHub Actions, DSL edition 2026 forms only (`///` docs on args, `filter row => ...`, `paginate`, construct signatures).
- Engine before guard: the admission reader refuses unknown fields, so `allowTrainRuns` ships in the engine before memql-znas's guard emits it.
- Stage files by explicit path; docs and code comments change in the same commit as the behavior (CLAUDE.md "Harness changes").

## Review Focus

1. **A boarded pull request gets a new push while its train runs.** The train must keep merging the boarded commit (by SHA), the run tests that commit, and the landing comment names the commit that landed. Test: Task 6, `TestTrainMergesTheBoardedCommitNotTheNewHead`.
2. **The owner approves, then the train is rebuilt.** The old approval names the old head and must not land the new head. Tests: Task 2, `TestApprovalForAnOlderHeadDoesNotLand`; Task 7, `TestRebuiltTrainAsksAgain`.
3. **Two replicas tick at once.** Only one builds; the other sees the record moved and does nothing. Test: Task 6, `TestTwoTicksBuildOneTrain`.
4. **A pull request title carrying `@everyone`, `<@123>` or Markdown.** It is escaped, and no mention appears except the configured owner. Test: Task 4, `TestTrainMessageNeverMentionsFromText`.
5. **GitHub answers `/merges` with 204 (already in the branch) or 409 (conflict).** 204 is not a conflict and the pull request stays on board; 409 leaves it off with a comment. Tests: Task 3, `TestMergeIntoBranchOutcomes`; Task 6, `TestConflictingPullRequestIsLeftOffWithAComment`.

---

### Task 1: Train settings, departures, boarding and the version bump (pure)

**Files:**
- Create: `component/pipelines/train.go`
- Test: `component/pipelines/train_test.go`

**Interfaces:**
- Produces:
  - `type TrainSettings struct { Repository string; Enabled bool; Departures []string; TimeZone string; Channel string; OwnerDiscordUserID string; MaxRebuilds int }`
  - `func ParseTrainSettings(raw string) (map[string]TrainSettings, error)`: input `{"repositories":{"owner/name":{"enabled":true,"departures":["09:00","15:00"],"timeZone":"America/Chicago","channel":"znas-instance","ownerDiscordUserId":"123456789012345678","maxRebuilds":2}}}`; keys lower-cased into `Repository`; empty or blank `raw` returns an empty map and no error.
  - `func DepartureDue(s TrainSettings, last, now time.Time) (time.Time, bool)`: the latest configured departure in `s.TimeZone` that falls in `(last, now]`.
  - `type TrainCandidate struct { Number int; Title, HeadSHA, HeadRef, BaseRef, Author, URL string; Draft bool; Labels []string; CheckStatus, CheckConclusion string }`
  - `type TrainSkip struct { Number int; Reason string }`
  - `func BoardingOrder(defaultBranch string, cands []TrainCandidate, express []int) (board []TrainCandidate, skipped []TrainSkip)`
  - `func NextPatchVersion(current string) (string, error)`
  - `func TrainBranch(day time.Time, n int) string`
  - `const TrainHoldLabel = "train:hold"`, `const TrainDroppedLabel = "train:dropped"`, `const TrainBranchPrefix = "train/"`

- [ ] **Step 1: Write the failing tests**

```go
func TestParseTrainSettings(t *testing.T) {
	got, err := ParseTrainSettings(`{"repositories":{"ZnasLLC-io/MemQL":{"enabled":true,"departures":["09:00","15:00"],"timeZone":"America/Chicago","channel":"znas-instance","ownerDiscordUserId":"123456789012345678"}}}`)
	// err == nil; got["znasllc-io/memql"].MaxRebuilds == 2; Departures == ["09:00","15:00"]
}
func TestParseTrainSettingsRefuses(t *testing.T) {
	// each of these returns an error naming the field:
	// unknown field "speed"; departure "9am"; departure "24:00"; timeZone "Mars/Base";
	// channel ""; ownerDiscordUserId "abc"; maxRebuilds 6; repository key "memql" (no owner)
}
func TestDepartureDue(t *testing.T) {
	// settings 09:00 and 15:00 America/Chicago (UTC-5 on 2026-10-28)
	// last 13:59Z, now 14:00Z -> (14:00Z, true); last 14:00Z, now 14:01Z -> false
	// last 13:00Z, now 21:00Z -> (20:00Z, true): the latest one, not both
}
func TestBoardingOrder(t *testing.T) {
	// candidates #12, #7, #9: #9 draft, #12 label train:hold, #7 check success -> board [#7]
	// skipped holds #9 "draft" and #12 "held with train:hold"; a train/* head is ignored without a skip
	// a pending check -> skipped "MemQL / memql has not passed on <short sha>"
	// express [12,7] -> considers only #12 and #7
}
func TestNextPatchVersion(t *testing.T) {
	// "0.31.4\n" -> "0.31.5"; "1.2.9" -> "1.2.10"; "v0.31.4" and "0.31" -> error
}
func TestTrainBranch(t *testing.T) { /* (2026-10-28, 2) -> "train/2026-10-28-2" */ }
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestParseTrainSettings|TestDepartureDue|TestBoardingOrder|TestNextPatchVersion|TestTrainBranch' ./component/pipelines/`
Expected: FAIL, undefined `ParseTrainSettings`.

- [ ] **Step 3: Implement `component/pipelines/train.go`**

Decode with `json.Decoder.DisallowUnknownFields`; validate `HH:MM` with `time.Parse("15:04", s)`; zones with `time.LoadLocation`; Discord id `^[0-9]{17,20}$`; `MaxRebuilds` 0 means 2, allowed 1..5. Skip reasons are plain sentences. The package stays standard-library only (`component/pipelines/purity_test.go`).

- [ ] **Step 4: Run the tests and the purity gate**

Run: `go test -count=1 ./component/pipelines/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/pipelines/train.go component/pipelines/train_test.go
git commit -m "feat(pipelines): decide train settings, departures, boarding and version"
```

### Task 2: Approval and failure triage (pure)

**Files:**
- Create: `component/pipelines/train_triage.go`
- Test: `component/pipelines/train_triage_test.go`

**Interfaces:**
- Consumes: `TrainSkip` (Task 1); codes `CodeStepTimeout`, `CodeRunnerUnavailable`, `CodeExecutorError`, `CodeImagePullFailed`, `CodeCloneFailed` (`component/pipelines/refusal.go`).
- Produces:
  - `type TrainReview struct { Login, State, CommitID string; SubmittedAt time.Time }`
  - `func ApprovalLands(reviews []TrainReview, ownerLogin, headSHA string) bool`
  - `var EnvironmentalStepCodes = []string{CodeStepTimeout, CodeRunnerUnavailable, CodeExecutorError, CodeImagePullFailed, CodeCloneFailed}`
  - `type TrainFailedStep struct { Key, ErrorCode string; Packages []string }`
  - `type TrainTriageInput struct { Failed []TrainFailedStep; Retried bool; Rebuilds, MaxRebuilds int; Files map[int][]string; FilesComplete map[int]bool; ModulePath string; MainFailedKeys []string }`
  - `type TrainVerdict struct { Action string; Drop int; Reason string }` with `Action` one of `"retry"`, `"drop"`, `"hold"`
  - `func TriageTrainFailure(in TrainTriageInput) TrainVerdict`

- [ ] **Step 1: Write the failing tests**

```go
func TestApprovalLands(t *testing.T) {
	// owner "znas-io" APPROVED at head "abc" -> true; login compared case-insensitively
	// someone else's APPROVED -> false; owner COMMENTED after APPROVED -> still true
	// owner CHANGES_REQUESTED after APPROVED -> false
}
func TestApprovalForAnOlderHeadDoesNotLand(t *testing.T) {
	// owner APPROVED with CommitID "old", head "new" -> false
}
func TestTriageTrainFailure(t *testing.T) {
	// only pipeline_step_timeout, Retried false -> {retry}
	// same, Retried true -> falls through to suspects
	// failed "tests.go-unit#2" Packages [".../component/memql"]; #7 changed component/memql/x.go, #9 changed docs/a.md -> {drop 7}
	// both #7 and #9 touch component/memql -> {hold, "could not tell which pull request caused tests.go-unit#2"}
	// MainFailedKeys contains the failed key -> {hold, "main fails tests.go-unit#2 too"}
	// Rebuilds 2, MaxRebuilds 2 -> {hold, "stopped after 2 rebuilds"}
	// FilesComplete[9] false -> #9 is a suspect of every package-scoped failed step
	// a failed step with no Packages contributes no suspect
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestApproval|TestTriageTrainFailure' ./component/pipelines/`
Expected: FAIL, undefined `ApprovalLands`.

- [ ] **Step 3: Implement `component/pipelines/train_triage.go`**

Order of rules: environmental retry, then `main` failing the same key, then the rebuild cap, then suspects. A file's candidate packages are `ModulePath` joined with its directory and every ancestor directory (`dsl/pipelines/x.memql` suspects `.../dsl/pipelines` and `.../dsl`). The owner's latest review by `SubmittedAt` among `APPROVED` and `CHANGES_REQUESTED` decides; `COMMENTED` and `DISMISSED` never decide.

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./component/pipelines/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/pipelines/train_triage.go component/pipelines/train_triage_test.go
git commit -m "feat(pipelines): decide train approval and which pull request a failure drops"
```

### Task 3: GitHub client operations for trains

**Files:**
- Create: `component/identity/githubapp/train.go`
- Test: `component/identity/githubapp/train_test.go` (use `fakeHub` and `testClient` from `client_test.go`)

**Interfaces:**
- Produces, all on `*Client`, `bearer` is the installation token, errors are `*StatusError` as in `client.go`:
  - `TrainPullRequests(ctx, bearer, owner, repo, base string) ([]TrainPullRequest, error)`: `GET /repos/{o}/{r}/pulls?state=open&base={base}&per_page=100&sort=created&direction=asc`; `type TrainPullRequest struct { Number int; Title, HeadSHA, HeadRef, BaseRef, Author, URL string; Draft bool; Labels []string }`
  - `CheckRunConclusion(ctx, bearer, owner, repo, sha, name string, appID int64) (status, conclusion string, err error)`: newest `check_runs` entry with that name and app id; `("", "", nil)` when there is none
  - `CreateBranchRef(ctx, bearer, owner, repo, branch, sha string) error` (`POST /git/refs`)
  - `ResetBranchRef(ctx, bearer, owner, repo, branch, sha string) error` (`PATCH /git/refs/heads/{branch}`, `force: true`)
  - `DeleteBranchRef(ctx, bearer, owner, repo, branch string) error`
  - `MergeIntoBranch(ctx, bearer, owner, repo, base, headSHA, message string) (MergeOutcome, error)`; `type MergeOutcome struct { SHA string; Conflict, NothingToMerge bool }` (201, 204, 409)
  - `PutBranchFile(ctx, bearer, owner, repo, branch, path, message string, content []byte) (commitSHA string, err error)`: reads the blob sha at `?ref={branch}` then `PUT /contents/{path}`
  - `CreatePullRequest(ctx, bearer, owner, repo, head, base, title, body string) (number int, url string, err error)`: never a draft
  - `EditPullRequestBody(ctx, bearer, owner, repo string, number int, body string) error`
  - `PullRequestReviews(ctx, bearer, owner, repo string, number int) ([]PullRequestReview, error)`; `type PullRequestReview struct { Login, State, CommitID string; SubmittedAt time.Time }`
  - `PullRequestFiles(ctx, bearer, owner, repo string, number int) (files []string, complete bool, err error)`: up to 3 pages of 100; `complete` false beyond
  - `MergePullRequest(ctx, bearer, owner, repo string, number int, headSHA string) (mergeSHA string, err error)`: `merge_method: "merge"`, `sha: headSHA`
  - `CommentOnPullRequest(ctx, bearer, owner, repo string, number int, body string) error` (`POST /issues/{n}/comments`)
  - `AddPullRequestLabels(ctx, bearer, owner, repo string, number int, labels []string) error` (`POST /issues/{n}/labels`)
  - `PullRequestOpen(ctx, bearer, owner, repo string, number int) (open bool, merged bool, err error)`

- [ ] **Step 1: Write the failing tests**

`TestTrainPullRequestsDecodesDraftLabelsAndRefs`, `TestMergeIntoBranchOutcomes` (201 gives SHA, 204 gives `NothingToMerge`, 409 gives `Conflict` and no error, 422 is an error), `TestMergePullRequestSendsTheHeadGuard` (body carries `"sha":"<head>"` and `"merge_method":"merge"`), `TestPullRequestFilesStopsAtThreePages`, `TestCheckRunConclusionPicksTheNewest`, `TestCreatePullRequestIsNeverADraft`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestTrainPullRequests|TestMergeIntoBranch|TestMergePullRequest|TestPullRequestFiles|TestCheckRunConclusion|TestCreatePullRequestIsNeverADraft' ./component/identity/githubapp/`
Expected: FAIL, undefined methods.

- [ ] **Step 3: Implement `component/identity/githubapp/train.go`**

Copy the request pattern in `client.go:307-353` (`callJSON`, `repoEndpoint`, `Accept`, `X-GitHub-Api-Version`, bearer). A 204 from `/merges` is not an error.

- [ ] **Step 4: Run the package tests**

Run: `go test -count=1 ./component/identity/githubapp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/identity/githubapp/train.go component/identity/githubapp/train_test.go
git commit -m "feat(githubapp): branch, merge, review, label and comment calls for trains"
```

### Task 4: Train messages for Discord

**Files:**
- Create: `component/pipelinenotify/train.go`
- Test: `component/pipelinenotify/train_test.go`

**Interfaces:**
- Produces:
  - `type TrainEvent string` with `TrainDeparted`, `TrainDropped`, `TrainHeld`, `TrainAwaitingApproval`, `TrainReminder`, `TrainLanded`, `TrainAdmissionHeld`
  - `func (e TrainEvent) ActionNeeded() bool`: true for `TrainHeld`, `TrainAwaitingApproval`, `TrainReminder`, `TrainAdmissionHeld`
  - `type TrainItem struct { Number int; Title, URL, Reason string }`
  - `type TrainMessage struct { Event TrainEvent; Repository, Branch, Version, PullRequestURL, RunPageURL, OSOrigin string; OnBoard, LeftOff []TrainItem; Dropped *TrainItem; Failed []string; Reason, MentionUserID string; At time.Time }`
  - `func TrainDiscordMessage(m TrainMessage) ([]byte, error)`

Headlines (title is `<repository> · <headline>`):

| Event | Headline |
| --- | --- |
| `TrainDeparted` | `Train <branch> left with <n> pull requests` |
| `TrainDropped` | `Dropped #<n> from <branch>` |
| `TrainHeld` | `ACTION NEEDED: <branch> is held` |
| `TrainAwaitingApproval` | `ACTION NEEDED: approve <branch>` |
| `TrainReminder` | `ACTION NEEDED: <branch> is still waiting for your approval` |
| `TrainLanded` | `PASSED: <branch> landed on main` |
| `TrainAdmissionHeld` | `ACTION NEEDED: the cost guard is holding <branch>'s run` |

Fields, in order, each present only when it has a value: `Train` (link to the train pull request), `Version`, `Run` (run page), `What you need to do` (action events: approve the train pull request; close it to cancel or add `train:hold` to a pull request to rebuild without it; open an admission window), `On board`, `Left off`, `Dropped`, `Failed`, `MemQL OS`.

- [ ] **Step 1: Write the failing golden tests**

One test per event with the exact JSON bytes, for example `TestTrainAwaitingApprovalMessage` expects `"content":"<@123456789012345678> ACTION NEEDED: approve train/2026-10-28-1"` and `"allowed_mentions":{"parse":[],"users":["123456789012345678"]}`, and `TestTrainDepartedMessage` expects no `content` and `"allowed_mentions":{"parse":[]}`. Plus `TestTrainMessageNeverMentionsFromText` (a title `@everyone <@1> **x**` comes out escaped, and `users` holds only the configured id) and `TestTrainMessageRefusesABadMentionID` (`"abc"` returns an error).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestTrain' ./component/pipelinenotify/`
Expected: FAIL, undefined `TrainDiscordMessage`.

- [ ] **Step 3: Implement `component/pipelinenotify/train.go`**

Reuse `discordEmbed`, `discordField`, the escaper and the size limits from `notify.go`; add `Content string` (omitempty) and `Users []string` (omitempty) to the wire structs so run messages keep their exact bytes (`TestDiscordMessageOfAPassedRelease` must stay green).

- [ ] **Step 4: Run the package tests**

Run: `go test -count=1 ./component/pipelinenotify/`
Expected: PASS, including the existing run-message goldens.

- [ ] **Step 5: Commit**

```bash
git add component/pipelinenotify/train.go component/pipelinenotify/train_test.go component/pipelinenotify/notify.go
git commit -m "feat(pipelinenotify): train messages, with the owner mentioned only on actions"
```

### Task 5: The train record (DSL, store, fakes)

**Files:**
- Modify: `dsl/pipelines/concepts.memql` (new `concept train` after `run`), `dsl/pipelines/mutations.memql`, `dsl/pipelines/queries.memql`, `dsl/pipelines/shapes.memql`
- Modify: `component/pipelinerun/types.go` (`Train`, `TrainPatch`), `component/pipelinerun/ports.go` (Store methods), `component/pipelinerun/store_dsl.go`, `component/pipelinerun/fakes_test.go` (`memStore`), `component/pipelinerun/ids.go` (`TrainGateKey`, `TrainIDFor`)
- Modify: `test/dslconformance/server_only_parsed_test.go` (`want` list), regenerated `component/conceptfields/concept-fields.snapshot.json`, SDK files from `make sdk-gen`
- Test: `component/pipelinerun/train_store_test.go`

**Interfaces:**
- Concept fields: `ownerUserId @serverSet`, `accountId`, `pipelineId!`, `repository!`, `branch!`, `departure enum("scheduled","express")!`, `state enum("building","running","awaiting_approval","landing","landed","held","cancelled")!`, `onBoard []object` (`{number, title, sha, url}`), `leftOff []object` (`{number, reason}`), `dropped []object` (`{number, sha, reason}`), `version`, `baseSha` (the default branch head the train was built on), `headSha`, `pullRequest integer`, `pullRequestUrl`, `runId`, `rebuilds integer`, `retried boolean`, `approvedBy`, `approvedSha`, `landedSha`, `heldReason`, `notified []string` (`<event>@<headSha>` keys already sent), `departedAt datetime!`, `updatedAt datetime`. Annotations as `run`: `@serverWritten`, `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`, `@displayCard(primary="branch", secondary="repository", tertiary="state", status="state")`, parent relationships to `user` and `pipeline`.
- Mutations: `createPipelineTrain` (`@createOnly @serverOnly @actor`, stamps `ownerUserId: actor.userId`), `updatePipelineTrain` (`@serverOnly`).
- Queries: `pipelineTrainsForPipeline(pipelineId!)` (`@serverOnly`, `sort "departedAt", "desc"`, `paginate 20`), `pipelineTrainsForOwner(pipelineId)` (owner read, `paginate 50`). Shape `pipelineTrainFull`.
- Go: `type Train struct { ID, OwnerUserID, AccountID, PipelineID, Repository, Branch, Departure, State string; OnBoard []TrainCar; LeftOff []pipelines.TrainSkip; Dropped []TrainCar; Version, BaseSHA, HeadSHA string; PullRequest int; PullRequestURL, RunID string; Rebuilds int; Retried bool; ApprovedBy, ApprovedSHA, LandedSHA, HeldReason string; Notified []string; DepartedAt, UpdatedAt time.Time }`, `type TrainCar struct { Number int; Title, SHA, URL, Reason string }`, `type TrainPatch struct { ...pointer fields... }`.
- Store: `LatestTrain(ctx context.Context, pipelineID string) (*Train, error)`, `TrainsSince(ctx context.Context, pipelineID string, since time.Time) ([]Train, error)`, `CreateTrain(ctx context.Context, owner string, t Train) error`, `UpdateTrain(ctx context.Context, owner, id string, patch TrainPatch) error`.
- `func TrainGateKey(repository string) string`; `func TrainIDFor(pipelineID, branch string) string` (via `deriveID`).

- [ ] **Step 1: Write the failing store test** `TestTrainRecordRoundTrips` against `memStore` (create, latest, patch, `TrainsSince`), and add the DSL constructs.
- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestTrainRecordRoundTrips ./component/pipelinerun/`
Expected: FAIL, undefined `Train`.

- [ ] **Step 3: Implement the Go types, store methods (`ownerWrite` for create and update, `systemRead` with a fresh read for `LatestTrain`), and the DSL constructs.**

- [ ] **Step 4: Regenerate and run every gate a new concept touches**

Run, in order:
`make concept-snapshot && make sdk-gen && go run ./cmd/memqllint dsl/ && go test -count=1 -run TestTrainRecordRoundTrips ./component/pipelinerun/ && go test -count=1 -run 'TestEngineInitLoadsFullDSL|TestDeclaredOwnerFieldsAreServerStamped|TestRowAuthzEnforcementLandGate' ./component/memql/ && go test -count=1 ./test/dslconformance/... && go test -count=1 -run TestConceptFieldSnapshotIsNotStale .`
Expected: all PASS (add `pipelineTrainsForPipeline`, `createPipelineTrain` and `updatePipelineTrain` to `want` in `server_only_parsed_test.go` with `///` reasons).

- [ ] **Step 5: Commit** the DSL files, Go files, snapshot and generated SDK files by path, message `feat(pipelines): the train record`.

### Task 6: The coordinator: building, running and failures

**Files:**
- Create: `component/pipelinerun/train.go` (tick and state handlers), `component/pipelinerun/train_github.go` (port extension), `component/pipelinerun/train_notify.go` (channel send)
- Modify: `component/pipelinerun/notify.go` (move `targetOf` and `discordTarget` into a function the run driver and trains share), `component/pipelinerun/github.go` (implement the train port on `packagesGitHub`)
- Test: `component/pipelinerun/train_test.go`, extend `fakeGitHub` in `fakes_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 5; `Store.PipelinesForRepository`, `RunsForPipelineSHA`, `WorkSteps`, `ChannelForOwnerByName`, `PipelineByID`, `StageNotification`; `packagesGitHub.RepositoryWriterToken`.
- Produces:
  - `const TrainSettingsVariable = "MEMQL_PIPELINES_TRAINS"`
  - `type trainGitHub interface` with the Task 3 calls taking `(ctx, token, repository string, ...)`, reached by type assertion like `repositoryChangeGitHub`
  - `func (d Deps) stageChannelMessage(ctx context.Context, p Pipeline, channel, requestedBy, dedupeKey string, body []byte) error`
  - `type TrainTickResult struct { Repositories, Departed, Advanced int; Notes []string }`
  - `func (i *Integration) TrainTick(ctx context.Context) (TrainTickResult, error)`: `requireInternalOrigin(ctx, "trainTick")` first
  - `func (i *Integration) advanceTrain(ctx context.Context, s pipelines.TrainSettings, p Pipeline, now time.Time) (bool, error)`

Behaviour per state, one action per tick, record written by compare-and-swap under `TrainGateKey(p.Repository)` after the GitHub calls:
- **No train, or the newest is `landed` or `cancelled`:** depart when `DepartureDue(s, newest.DepartedAt or 24h ago, now)`. Build: token, `TrainPullRequests`, `CheckRunConclusion` per candidate (`MemQL / memql`, app id from the pipeline's installation), `BoardingOrder`, `CreateBranchRef` at the default branch head (recorded as `baseSha`), `MergeIntoBranch` per boarded head SHA (409 moves it to left off and `CommentOnPullRequest`), version via `PutBranchFile` unless a boarded file list contains `VERSION`, `CreatePullRequest`, record `running`, send `TrainDeparted`.
- **`running`:** newest pull-request run for `HeadSHA`. Unfinished: nothing (after 15 minutes `queued` with `AdmitRun` false, send `TrainAdmissionHeld` once). Succeeded: `awaiting_approval`, send `TrainAwaitingApproval`. Failed: build `TrainTriageInput` (failed `WorkSteps`, `PullRequestFiles` per boarded pull request, failed keys of `main`'s newest completed push run) and act on `TriageTrainFailure`: `retry` opens a failed-only rerun through the internal opening path (not `pipelinesRerun`, whose `personFrom` refuses the automation actor) and sets `retried`; `drop` comments, labels `train:dropped`, rebuilds with `ResetBranchRef` and sends `TrainDropped`; `hold` sets `heldReason` and sends `TrainHeld`.
- **`held`:** the train pull request closed means `cancelled`; a boarded pull request labelled `train:hold` means rebuild without it; otherwise nothing.
- Every message is sent once: its `<event>@<headSha>` key goes into `notified`, and the outbound `DedupeKey` is the train id plus that key.

- [ ] **Step 1: Write the failing tests** with `newHarness`, `memStore`, an extended `fakeGitHub` and `watchedGate`: `TestTrainDepartsAtTheScheduledTime`, `TestNoDepartureWithoutReadyPullRequests`, `TestConflictingPullRequestIsLeftOffWithAComment`, `TestTrainMergesTheBoardedCommitNotTheNewHead`, `TestTwoTicksBuildOneTrain`, `TestFailedTrainRetriesATimeoutOnce`, `TestFailedTrainDropsTheSingleSuspect`, `TestMainFailingTheSameStepHoldsTheTrain`, `TestClosingAHeldTrainCancelsIt`, `TestTrainMessagesAreSentOnce`, `TestTickRefusesAClientCaller` (pattern of `internal_origin_test.go:184`).
- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestTrain|TestNoDeparture|TestConflicting|TestTwoTicks|TestFailedTrain|TestMainFailing|TestClosingAHeld|TestTickRefuses' ./component/pipelinerun/`
Expected: FAIL, undefined `TrainTick`.

- [ ] **Step 3: Implement the three files and the shared channel send.**
- [ ] **Step 4: Run the package tests, including the gate discipline test**

Run: `go test -count=1 ./component/pipelinerun/`
Expected: PASS, including `TestEveryGatedPathKeepsTheGateDiscipline` and the existing notify tests.

- [ ] **Step 5: Commit** the new and modified files by path, message `feat(pipelines): the train coordinator departs, runs and drops`.

### Task 7: Approval, landing, express trains and the schedule

**Files:**
- Modify: `component/pipelinerun/train.go`, `component/pipelinerun/capabilities.go` (two capabilities), `dsl/pipelines/builtins.memql`, `dsl/pipelines/automations.memql`
- Modify: `component/packages/repository_authority.go` (`RepositoryAuthority.Login`, filled inside `repositoryAccess` from `githubapp.Client.User` with the credential's user token it already holds) and its test
- Modify: `app/automation_schedule_placement_test.go` (assert `tickPipelineTrains` on `agent`), `component/automations/strict_automation_boot_test.go` (`shippedAutomationCount` 193 to 194), corpus golden `component/automations/steps/testdata/automation_corpus/pipelines/tickPipelineTrains.json`
- Test: `component/pipelinerun/train_test.go`

**Interfaces:**
- Produces:
  - Builtin `pipelinesTrainTick()` → `integration.pipelines.trainTick` (no `@sdk`, internal origin only)
  - Builtin `pipelinesTrainExpress(repository string!, pullRequests []integer!)` → `integration.pipelines.trainExpress`, `@sdk`, `@requiresCapability("execute", "app:pipelines/rerun")`
  - `func (i *Integration) TrainExpress(ctx context.Context, repository string, numbers []int) (Train, error)`: `personFrom`, owner of the pipeline only
  - Automation: `@trigger(schedule="0 * * * * *", node="agent", lease="pipelines-trains") automation tickPipelineTrains { ticked := builtin pipelinesTrainTick() }`

Behaviour:
- **`main` moved:** in `awaiting_approval` or `held`, a default branch head other than `baseSha` rebuilds the train on the new head and asks again (`TrainAwaitingApproval` is sent for the new head). The same check runs again immediately before `MergePullRequest`.
- **`awaiting_approval`:** `PullRequestReviews`, then `ApprovalLands(reviews, ownerLogin, HeadSHA)` with `ownerLogin` taken from `RepositoryAccess(...).Login` for the pipeline's credential, so the approver is the verified GitHub identity of the pipeline's owner, never a configured string. On approval: record `landing`, `MergePullRequest(..., HeadSHA)`, record `landed` with `landedSha`, comment on each boarded pull request ("Landed on main in <train branch> at <sha>, merged as <boarded sha>"), send `TrainLanded`, delete the train branch. A boarded pull request labelled `train:hold` means rebuild without it. At the next scheduled departure time, send `TrainReminder` once.
- **`landing` found on a later tick:** `PullRequestOpen` decides between `landed` and retrying the merge.
- **Express:** records an express departure the next tick builds with only those numbers; refused while another train is not `landed` or `cancelled`, except that an express train may replace a train in `awaiting_approval` or `held` (that train is cancelled and its pull requests board the next scheduled train).

- [ ] **Step 1: Write the failing tests**: `TestApprovedTrainLands`, `TestRebuiltTrainAsksAgain`, `TestMainMovingRebuildsAWaitingTrain`, `TestRepositoryAccessReportsTheLogin` (in `component/packages`), `TestLandingResumesAfterAReplicaChange`, `TestReminderIsSentOncePerDeparture`, `TestExpressTrainCarriesOnlyTheNamedPullRequests`, `TestExpressRefusesAnotherOwner`.
- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestApproved|TestRebuilt|TestMainMoving|TestLanding|TestReminder|TestExpress' ./component/pipelinerun/ && go test -count=1 -run TestRepositoryAccessReportsTheLogin ./component/packages/`
Expected: FAIL.

- [ ] **Step 3: Implement the handlers, both capabilities (copy the `schedule` and `rerun` entries in `capabilities.go`), the builtins and the automation.**
- [ ] **Step 4: Run the package tests and the automation gates**

Run: `go test -count=1 ./component/pipelinerun/ && go test -count=1 -run TestAutomationCorpusRuns -update ./component/automations/steps/ && go test -count=1 ./component/automations/... ./app/ && make sdk-gen && go run ./cmd/memqllint dsl/`
Expected: PASS; the corpus golden for `tickPipelineTrains` is written once and then stable.

- [ ] **Step 5: Commit** by path, message `feat(pipelines): land approved trains, express trains and the one-minute tick`.

### Task 8: Admission admits train runs

**Files:**
- Modify: `component/pipelinerun/admission.go`, `component/pipelinerun/integration.go` (`AdmitRun func(time.Time, bool) (bool, error)`), `component/pipelinerun/lease.go:263` (pass `IsTrainRun(r)`), `app/integrations_pipelines_agent.go:44`
- Modify: `docs/public/operate/pipelines.md` (admission section near 1337), `docs/public/operate/env-vars.md`, `component/envregistry/manifest.yaml` (description of `MEMQL_PIPELINES_ADMISSION_FILE`)
- Test: `component/pipelinerun/admission_test.go`, `component/pipelinerun/lease_test.go`

**Interfaces:**
- Produces: `func IsTrainRun(r Run) bool` (`r.Event == pipelines.EventPullRequest && strings.HasPrefix(r.HeadBranch, pipelines.TrainBranchPrefix)`); `FileRunAdmission(path string) func(now time.Time, train bool) (bool, error)` accepting optional `allowTrainRuns` (bool); a train run is admitted when `allowNewRuns` or `allowTrainRuns` is true; every other rule (2-hour window, 4096 bytes, unknown fields refused) is unchanged.

- [ ] **Step 1: Write the failing tests** `TestAdmissionAdmitsATrainRunUnderTheTrainAllowance`, `TestAdmissionHoldsAnOrdinaryRunUnderTheTrainAllowance`, `TestAdmissionWithoutTheTrainFieldIsUnchanged`, `TestClaimPassesTheTrainFact`.
- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestAdmission|TestClaimPasses' ./component/pipelinerun/`
Expected: FAIL.

- [ ] **Step 3: Implement; update the docs in the same commit.**
- [ ] **Step 4: Run** `go test -count=1 ./component/pipelinerun/ ./app/` and the env registry gate `go test -count=1 ./component/envregistry/...`. Expected: PASS.
- [ ] **Step 5: Commit** by path, message `feat(pipelines): the cost guard can admit train runs on their own allowance`.

### Task 9: The cost guard's train allowance (memql-znas)

Separate repository `znasllc-io/memql-znas`, after the engine from Task 8 runs in production.

**Files:**
- Modify: `deploy/k8s/instance/azure-cost-guard/guard.py` (`admission_data`), `scripts/dev/azure_cost_guard_test.py` (field pins at :114 and the new cases), `docs/azure-cost-guardrail.md`, `deploy/k8s/instance/azure-cost-guard/resources.yaml` (env `TRAIN_ALLOWANCE_USD`, default `30`)

**Interfaces:**
- Produces: `allowTrainRuns` in the version-1 decision, true when no `operatorHold` and `reservedMonthEndCost <= LIMIT + TRAIN_ALLOWANCE_USD`; the field is emitted only when `TRAIN_ALLOWANCE_USD` is set, so an older engine never sees it.

- [ ] **Step 1: Write the failing tests** `test_train_allowance_admits_trains_over_the_limit`, `test_operator_hold_holds_trains_too`, `test_no_train_field_without_the_setting`.
- [ ] **Step 2: Run them to verify they fail**: `python3 -m unittest scripts/dev/azure_cost_guard_test.py` (FAIL).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests**: PASS.
- [ ] **Step 5: Commit** by path, message `feat(cost-guard): a daily train allowance`.

### Task 10: `merge-as-owner.sh` becomes emergency-only

**Files:**
- Modify: `scripts/dev/merge-as-owner.sh` (parse loop at 83-106, `merge_pr` at 467), `scripts/dev/merge_as_owner_required_checks_test.go`

**Interfaces:**
- Produces: `--emergency=<reason>` required for a merge (exit 2 without it, naming the release train); `--check` needs no reason; the reason is posted on the pull request with `gh pr comment` before the merge.

- [ ] **Step 1: Write the failing tests** `TestMergeRefusesWithoutAnEmergencyReason`, `TestEmergencyReasonIsRecordedOnThePullRequest`; pass `--emergency=test` in every existing test that expects a merge.
- [ ] **Step 2: Run** `go test -count=1 -run 'TestMerge|TestEmergency' ./scripts/dev/`: FAIL.
- [ ] **Step 3: Implement** in the existing `main`/function style (bash 3.2 portable: `scripts/lib/bash_portability_test.go`).
- [ ] **Step 4: Run** `go test -count=1 ./scripts/dev/ ./scripts/lib/`: PASS.
- [ ] **Step 5: Commit** by path, message `fix(dev): merge-as-owner only for emergencies once trains carry main`.

### Task 11: Documentation

**Files:**
- Modify: `docs/public/operate/pipelines.md` (new "Release trains" section: settings variable, boarding, departures, states, failure handling, approval, Discord messages, express trains, the fast-phase note), `docs/internal/ops/harness-decisions.md` (trains entry: triggers, evidence, chosen path, fallback, invariant), `CLAUDE.md` (Branch Workflow: sessions open a PR, get it green, stop; the train carries it; `train:dropped` is removed after a fix; urgent fixes ask for an express train; `merge-as-owner.sh --emergency` only), the spec's `status: draft` to `status: stable` once Task 12 step 6 passes
- Test: the docs gates.

- [ ] **Step 1: Write the docs.** No `znas.io` in engine docs (`vendor_domain_test.go`); no link from `docs/public` into `docs/internal` or `docs/superpowers` (`docs_public_boundary_test.go`); any `memql` block compiles or is marked `fragment`.
- [ ] **Step 2: Run the gates**: `go test -count=1 -run 'TestDocs|TestDocumentedTestCommandCoversTheEngine|TestVendor|TestEnvironmentTier|TestMakeTargetCitations' .` and `go test -count=1 ./scripts/dev/`. Expected: PASS.
- [ ] **Step 3: Commit** by path, message `docs: release trains, the branch workflow and the harness decision`.

### Task 12: Ship and switch (operator steps)

- [ ] **Step 1: Land Tasks 1 to 8, 10 and 11** on `main` through one pull request merged with the admin bypass, then wait for `main`'s `MemQL / memql / push` run to pass.
- [ ] **Step 2: Release and deploy** that commit as the next engine version (Releases → Cut release; publish; memql-znas pin pull request; ArgoCD sync), then verify `https://api.<domain>/healthz` reports it.
- [ ] **Step 3: Configure trains in production**: allow the `memql` pipeline on the `znas-instance` channel (`pipelinesChannelAllow`), and set the global variable `MEMQL_PIPELINES_TRAINS` with `enabled: false`, the owner's time zone, departures, channel and Discord user id.
- [ ] **Step 4: Land Task 9** in memql-znas and set `TRAIN_ALLOWANCE_USD`.
- [ ] **Step 5: Switch GitHub**: in `znasllc-io/memql`'s `default` ruleset, require `MemQL / memql` again and remove the merge queue rule; commit `.github/CODEOWNERS` with the owner as the sole owner of every path (`* @znas-io`, lead paths likewise); keep the admin bypass.
- [ ] **Step 6: Turn trains on** (`enabled: true`). Acceptance: the next scheduled departure opens a train pull request authored by the app, its run passes, Discord shows `ACTION NEEDED: approve ...`, the owner approves on GitHub, and within two minutes the train lands and Discord shows `PASSED: ... landed on main`.

## After the switch (not in this plan)

The two lanes, splitting a train with runs limited to named steps, the `main` push run recorded as verified by the train, the `pull_request_review` event route, the audit and goldens for today's run and deploy messages, the PR-lane admission allowance, and showing each pipeline step as its own GitHub check.
