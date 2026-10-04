# Pipelines, the seam -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Epic memql#5477 end to end in ONE pull request: a GitHub delivery (or the poll) opens a pipelines run keyed on (repository, SHA, mode, event), an agent node compiles the repository's `pipeline:` block into a work goal with one `v1:work:step` per step, hands each step to the registered executor, and reports a check run `queued` -> `in_progress` -> `completed` with the stage table as its summary.

**Architecture:** Three layers. `component/pipelines` is a nested Go module, standard library only: the manifest block's types, validation and compiler, the event-to-mode table, GitHub delivery classification, the Go import graph read from source, the affected set, the shard split, timing parsing and the check-run text. `component/pipelinerun` is a root-module plug-in (`pipelines`) that reads and writes rows, mints installation tokens through Deployables' verified grant path, writes check runs, and drives runs on agent nodes under a row lease. DSL under `dsl/pipelines` declares the three concepts, their reads and server-only writes, the builtins and the two automations; the work spine gains the fields and the runner-owned exemption a pipeline run needs.

**Tech Stack:** Go 1.26 (workspace of nested modules), MemQL DSL edition 2026, PostgreSQL + TimescaleDB (append-only `MemoryNodes`), GitHub REST v3 (check runs, compare, pulls, tarball), MemQL OS (TypeScript, only the refusal copy and readiness registry).

**Spec:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md` (section 3 D1-D16, section 4 epic 2, section 6) plus the documentation program's D15 (`docs/superpowers/specs/2026-09-27-documentation-program-design.md`: `release` joins the event table). Issues: #5477 (epic), #5486, #5487, #5488, #5489, #5490, #5491.

## Global Constraints

- One PR for the whole epic; it closes #5477, #5486, #5487, #5488, #5489, #5490, #5491, each on its OWN `Closes #n` line (a comma list links only the first).
- `component/pipelines` imports the standard library only (`purity_test.go`); it imports no YAML package.
- No GitHub token is ever written to a row, a log line, a journal row or a check run. Tokens live in memory for one call path.
- A manifest that cannot compile is a FAILED run carrying a typed refusal, never a skipped run (D9). A fork head is REFUSED, never queued (D6).
- Every `v1:pipelines:*` concept's `graph.node.created` and `.updated` events carry broadcast routing rules in `component/node/routing.go`'s `core` slice (D11).
- Every refusal code is in the `pipeline_` family, declared in `component/pipelines/refusal.go`, spelled as a literal in `component/packages/refusal.go`, and has OS copy in `clients/os/src/apps/deployables/packages/refusals.ts`.
- Secret values exist only inside `StepRequest.Secrets`; manifests, steps and journals carry names. A secret resolves only when the pipeline row's `secretNames` allowlist names it, and a name beginning `MEMQL_` never resolves.
- Multi-node by default: state crossing a node boundary rides a row or a message. The cross-node behaviour is proven by an in-process hop test, not by a single-node test.
- gRPC-first: no new HTTP endpoint. `POST /inbound/github` already exists.
- No `if env == ...` branches; the domain is a value.
- Stage files by explicit path; never `git add -A` / `git add .`.
- No emojis anywhere. Public docs never call MemQL a database (`TestNoDatabaseProductClaims`).
- DSL: struct form, `filter row => ...`, `///` doc comments on args, `@serverOnly` constructs carry a `///` reason and a `want` entry in `test/dslconformance/server_only_parsed_test.go`, owner fields stamped from `actor.userId`.
- Use `langparser.QuoteString` (never `%q`) when composing a MemQL call string in Go.

## Decisions this plan takes (the record leaves them open)

1. **Run key carries the event.** `RunKey(repository, sha, mode, event)`. The record's (repository, SHA, mode) would collapse the merge queue's full run and the default branch's push run (both `full`, same SHA once merged) into one, so the push's deploy and notify stages would never run. The event keeps them apart while a webhook and a poll for the same head stay one run (the poll synthesizes `push` for the default branch and `pull_request` for PR heads). Amend D4 in the record.
2. **Stages run strictly in the order written; the first failed stage blocks every later stage** (`pipeline_stage_blocked`). `needs` must name earlier stages and is recorded; every step `dependsOn` the previous stage's steps. `on` accepts events (`pull_request`, `merge_group`, `push`, `release`) and modes (`affected`, `full`); a stage whose `on` excludes this run is absent from the plan, not skipped.
3. **The mode table:** `pull_request` (opened, synchronize, reopened) -> affected; `merge_group` (checks_requested) -> full; `push` to the default branch -> full; `release` (published) -> full; `check_run` / `check_suite` rerequested -> a new attempt of the original run's mode and event. `MEMQL_VERSION` is the release tag for a release, else the head SHA.
4. **The event is read from the SIGNED body's shape.** `X-GitHub-Event` is not signed; it is a cross-check only, and a disagreement is ignored with a log line.
5. **Selection is computed in the driver, from source.** The driver fetches the tarball at the SHA, keeping only `memql-package.yaml`, `go.mod`, `go.work`, `*.go`; `ScanGoTree` builds the import graph with `go/parser` (ImportsOnly), the union over build tags (a superset of `go list`, the safe direction). Changed files come from the compare API; a truncated (300+) or failed diff means everything. A file that fails to parse means everything.
6. **`select:` grows `dbGated` (trees) and `full` (globs).** D8's "trees the platform knows need a database" has no other source for a customer repository. `only:` accepts `db-gated` and `not-db-gated`.
7. **Services carry `env` and `ready`** (agreed with the substrate session): a Postgres sidecar needs `POSTGRES_PASSWORD`, and `ready` is its startup probe.
8. **Compute is a pipeline field:** `cluster` (default) or `cluster_and_fleet`. A step naming a need on a cluster-only pipeline fails the compile with `pipeline_fleet_not_consented` (scope = the step).
9. **No default runner ships here.** With no `Executor` registered a command step fails `pipeline_runner_unavailable`; the substrate (epic memql#5478, a peer session) registers the runner. Notify steps are skipped `pipeline_notify_unavailable` until epic memql#5480.
10. **The driver runs on agent nodes.** The trigger opens a `queued` run where it fires (the bff for a webhook; the poll is placed on agent replicas); an agent subscriber claims it under a row lease (`driverNodeId`, `driverHeartbeatAt`, advisory-gated read-modify-write with a fresh read, 30 s heartbeat, 120 s staleness) and drives it; the poll recovers unclaimed and stale runs every minute.
11. **Pipeline work runs are runner-owned.** `triggeredBy` is `pipeline:<mode>` (`pipelines.WorkTriggerPrefix`); the work dispatcher never takes them, the spine's waiting sweep skips them, and Nexus's re-run/branch acts refuse them.
12. **One pipeline per source.** A pipeline row hangs off a `v1:platform:package` (the source); its id is derived from the package id; its owner is the package's owner, whose `sourceCredential` grant mints every token (`verifyGrantRepository` runs each time).
13. **The first poll records a baseline** and opens nothing; runs start with the next change. Webhook delivery behaves the same way by construction.
14. **Check run name** `MemQL / <pipeline name>`; `external_id` = the run id; `details_url` = `https://os.<domain>/?pipelineRun=<bare run id>` (`pipelines.RunPageURL`). The OS reads that boot-time parameter in epic memql#5479. A 403 on a check-run write records `checkRunState: "refused"` plus the note `pipeline_check_permission_missing` on the run and never stops the run.
15. **Fork refusal writes a check run** (`completed`, conclusion `failure`, title "Refused: pull request from a fork"): `neutral` would satisfy a required check.

## Review Focus

1. A webhook redelivered (same body), or a webhook and a poll for the same head, must produce exactly ONE run and ONE check run. Tests: trigger dedup unit test; hop test case 3.
2. A `pull_request` `synchronize` delivery (top-level `before`/`after`, no `ref`) and a `check_suite` delivery must NOT move Deployables' update cue (`latestKnownVersion`) or start an auto-deploy. Test in Task 4.
3. An app installed before this epic (no `checks: write`): every check-run write answers 403. The run must still open, execute and close, carrying `checkRunState: refused` and the note. Test in Task 10a.
4. A pipeline block with a semantic mistake (unknown need, bad timeout, bucket nobody declared) must not refuse Deployables' analyze/deploy of the same source; only the pipeline run fails, typed. (A structurally unknown YAML key still refuses the manifest, as for any key.) Test in Task 4.
5. A step naming a secret the owner did not allow, or a `MEMQL_*` name, is refused before any executor sees it, and no secret value appears in a step row, the check-run text or a log line (masking applies to log tails). Tests in Tasks 1 and 10b.

---

## File Structure

**component/pipelines (nested module, stdlib only)** -- shipped in Task 0: `go.mod`, `doc.go`, `contract.go`, `executor.go`, `environment.go`, `needs.go`, `refusal.go`, `spec.go`, `purity_test.go`, `contract_test.go`. New:
- `validate.go`, `compile.go`, `glob.go`, `shard.go` (Task 1)
- `trigger.go`, `mode.go`, `url.go` (Task 2)
- `gograph.go`, `affected.go`, `timings.go`, `summary.go`, `mask.go` (Task 3)
- `testdata/manifests/*.yaml` (as JSON-equivalent Go fixtures; no YAML here), `testdata/github/*.json`, `testdata/gotree/**` (Tasks 1-3)

**component/packages (root module)** -- Task 4: `manifest.go` (Pipeline field), `refusal.go` (catalogue literals), `refusal_pipelines_parity_test.go`, `feeds.go` (event-aware push parse), `githubapp/client.go` (body requests), new `githubapp/checks.go`, `githubapp/repos.go`, `pipeline_access.go` (exported token + client accessors).

**component/identity/githubconnect + docs + OS copy** -- Task 5.

**component/workjournal, integrations/work** -- Task 7.

**dsl/pipelines, dsl/work, component/node/routing.go** -- Task 6.

**Readiness + OS** -- Task 8: `scripts/secrets/manifest.yaml`, `component/envregistry/modules.go`, readiness eval/write + concept fields + folds, `clients/os/src/system/modules.ts`, `clients/os/src/apps/deployables/packages/refusals.ts`.

**component/pipelinerun (root module, plug-in `pipelines`)** -- Tasks 10a/10b: `plugin.go`, `store.go`, `store_dsl.go`, `github.go`, `connect.go`, `trigger.go`, `open.go`, `poll.go`, `status.go`, `cancel.go`, `rerun.go`, `driver.go`, `lease.go`, `tree.go`, `secrets.go`, `checkrun.go`, `timings.go`, plus `app/integrations_pipelines.go`, `app/integrations_pipelines_agent.go` (`//go:build agent`), `app/automation_schedule_placement.go`.

**Hop test** -- Task 11: `test/pipelinehop/`.

**Docs** -- Task 12: `docs/public/operate/pipelines.md`, the record's amendment, `GLOSSARY.md`.

---

## Execution waves

- **Wave 0 (done):** Task 0.
- **Wave 1 (parallel, separate worktrees off `epic/pipelines-seam`):** Task 1, Task 2+3 (one agent), Task 4, Task 5, Task 6, Task 7.
- **Wave 2:** merge Wave 1; regenerate generated artifacts once; Task 8.
- **Wave 3:** Task 10a then 10b (one agent, sequential).
- **Wave 4:** Task 11, Task 12, then final verification and the PR.

Worktree rule for every parallel task: work ONLY in your own worktree (`/home/znas/memql-projects/ps-<task>`, branch `epic/pipelines-seam-<task>`), commit there, never push, never touch the shared checkout `/home/znas/memql-projects/memql`. Before the first build run `bash scripts/identity/build-css.sh` (a fresh worktree lacks the generated `component/identity/web/static/app.css`). Do NOT run `make arch-model` or `make platform-graph` (the coordinator regenerates once after merging). Run the package tests you touched plus the named gates; report what you ran.

---

### Task 0: The leaf module and the executor contract -- DONE

Commits `2d6336ba1`, `5b932b444` on `epic/pipelines-seam`. Shipped `component/pipelines` (`Mode`, `Event`, `Compute`, `StepKind`, `Repository`, `Service{Image, Env, Ready}`, `ShardRef`, `Skip`, `Step`, `StepRequest`, `Outcome`, `Failure`, `Where`, `StepResult` incl. `Notes`, `WorkTriggerPrefix`, `Executor`, `RegisterExecutor`, `CurrentExecutor`, `StepRequest.Environment()`, `Needs()`, `IsNeed`, `Refusal`, `Refuse`, the code catalogue with `Codes()`/`ClassOf`, `Spec`/`Select`/`StageSpec`/`StepSpec`/`When` and the vocabulary constants), wired into `go.work`, both go.mod replace blocks, both Dockerfiles and `scripts/ci/db-gated-packages.sh`. The JSON wire names are pinned by `contract_test.go` and are shared with the substrate session: do not rename them.

---

### Task 1: Validate and Compile (leaf)

**Files:**
- Create: `component/pipelines/validate.go`, `compile.go`, `glob.go`, `shard.go`, `validate_test.go`, `compile_test.go`, `glob_test.go`, `shard_test.go`

**Interfaces:**
- Consumes: Task 0 types; Task 3's `Graph` and `Affected` (write against the signatures below; until Task 3 merges, test Compile with the `Selector` seam, see below).
- Produces:

```go
// DefaultStepTimeout applies when a step names none; MaxStepTimeout caps one.
const DefaultStepTimeout = 20 * time.Minute
const MaxStepTimeout = 2 * time.Hour
const MaxShards = 8

// Validate reports the first thing in spec that cannot mean anything, as a
// typed refusal scoped to the stage or "stage/step"; nil when it compiles.
func Validate(spec *Spec) *Refusal

// CompileInput is everything a compile reads besides the spec.
type CompileInput struct {
	Mode    Mode
	Event   Event
	Compute Compute
	// AllowedSecrets is the pipeline row's secretNames.
	AllowedSecrets []string
	// Selector answers which Go packages a step selects. Nil when no step
	// selects packages; Compile refuses pipeline_select_missing if one does.
	Selector Selector
	// Changed and ChangedKnown gate `when: bucket` steps in affected mode.
	Changed      []string
	ChangedKnown bool
	// Timings is the pipeline's timing table: import path -> seconds.
	Timings map[string]float64
}

// Selector is the Go selection a compile consults (Task 3 implements it
// over a Graph; tests may fake it).
type Selector interface {
	// All is every package, sorted import paths.
	All() []string
	// Affected is the selection for this run; Full=true selects All().
	Affected() Selection
	// DirOf is a package's repo-relative directory ("" if unknown).
	DirOf(importPath string) string
}

type Plan struct {
	Mode   Mode
	Event  Event
	Stages []PlanStage
}

type PlanStage struct {
	Name  string
	Needs []string
	Steps []Step
}

// Steps flattens the plan in execution order.
func (p Plan) Steps() []Step

// NeedsSelector reports whether any step of spec selects packages.
func NeedsSelector(spec *Spec) bool

func Compile(spec *Spec, in CompileInput) (Plan, *Refusal)

// CompileGlob compiles one path glob: `**` crosses directories (zero or
// more), `*` and `?` stay inside one segment, a leading `!` negates;
// patterns are anchored at the repository root.
func CompileGlob(pattern string) (func(path string) bool, error)

// Partition splits packages over at most n shards by longest-processing-time
// first: heaviest first, each into the currently lightest shard (ties: lower
// index). An unmeasured package weighs UnknownSeconds. Each shard is sorted;
// empty shards are dropped.
const UnknownSeconds = 5.0
func Partition(packages []string, seconds map[string]float64, n int) [][]string
```

Task 3 defines `Selection` (`Full bool; Reason string; Seeds, Packages []string`); Task 1 references it. If Task 1 runs before Task 3 merges, declare `Selection` in `affected.go` exactly as Task 3 specifies and let Task 3 keep that declaration (coordinate through the plan: the type lives in `affected.go`).

**Validation rules** (each a separate test case asserting code and scope):
- `spec == nil` -> `pipeline_not_declared` (scope "").
- No stages -> `pipeline_stage_invalid` "declares no stages".
- Stage name empty, not `^[a-z0-9][a-z0-9-]{0,39}$`, or duplicate -> `pipeline_stage_invalid` (scope = the name or "stages[i]").
- `needs` naming an unknown stage, itself, or a LATER stage -> `pipeline_stage_invalid`.
- Stage with both `channel` and `steps`, or neither -> `pipeline_stage_invalid`. Channel name must match the stage-name pattern.
- `on` value not in {pull_request, merge_group, push, release, affected, full} -> `pipeline_event_unknown`.
- Step name empty/pattern/duplicate within its stage -> `pipeline_step_invalid` (scope "stage/step").
- Step `run` empty -> `pipeline_step_invalid`.
- `packages` not in {"", affected, all} or `only` not in {"", db-gated, not-db-gated} -> `pipeline_step_invalid`; `only` without `packages` -> `pipeline_step_invalid`; `only: db-gated|not-db-gated` with empty `select.dbGated` -> `pipeline_select_invalid`.
- `packages` set but `select` nil or `select.go != "import-graph"` -> `pipeline_select_missing`.
- `select.go` not in {"", import-graph} -> `pipeline_select_invalid`; any `select.full` or bucket glob that fails `CompileGlob` -> `pipeline_select_invalid`; a `dbGated` entry that is absolute, contains `..` or is empty -> `pipeline_select_invalid`; a bucket name not matching the stage-name pattern -> `pipeline_select_invalid`.
- `shards` < 0 or > `MaxShards`, or > 1 without `packages` -> `pipeline_step_invalid`.
- `when.bucket` not declared in `select.buckets` -> `pipeline_bucket_unknown`.
- A `needs` key outside `Needs()` -> `pipeline_need_unknown` (detail lists the closed set).
- A step `services` entry not declared in `spec.Services` -> `pipeline_service_unknown`; a declared service with empty `image`, or an env key not `^[A-Z_][A-Z0-9_]*$` -> `pipeline_step_invalid` (scope "services/<name>").
- `timeout` unparsable, < 1 m or > `MaxStepTimeout` -> `pipeline_step_invalid`.
- A secret name not `^[A-Z][A-Z0-9_]{0,127}$` or beginning `MEMQL_` -> `pipeline_secret_invalid`.

**Compile semantics:**
1. `Validate`; any refusal is returned.
2. Stage selection: a stage is in the plan when `On` is empty or contains `in.Event` or `in.Mode`.
3. For each planned stage in order: notify stage -> one `Step{Kind: StepNotify, Key: "<stage>/notify", Name: "notify", Channel}`; else each step compiles to one or more `Step`s:
   - Key `"<stage>/<step>"`, or `"<stage>/<step>#<i>"` for shard i of k (k > 1).
   - `DependsOn` = the keys of every step of the previous planned stage.
   - `Image` = `spec.Image`; `Caches` = `spec.Caches`; `Services` = the declared services the step names; `Artifacts`, `Secrets` copied; `Needs` = sorted keys whose value is true; `TimeoutSeconds` from `timeout` or the default.
   - A need on a step while `in.Compute != cluster_and_fleet` -> return refusal `pipeline_fleet_not_consented` (scope "stage/step").
   - A secret not in `in.AllowedSecrets` -> return refusal `pipeline_secret_not_allowed` (scope "stage/step").
   - `when.bucket`: in `ModeAffected` with `ChangedKnown`, when no changed path matches the bucket's globs -> `Skip{CodeNotAffected, "No change under bucket <name>."}`. Full mode or unknown changes -> runs.
   - `packages`: candidates = `Selector.All()` when `in.Mode == ModeFull`, or `packages == all`, or `Selector.Affected().Full`; else `Selector.Affected().Packages`. Then `only: db-gated` keeps candidates whose `DirOf` lies under a `dbGated` tree (prefix on a `/` boundary, or equal); `not-db-gated` keeps the rest. Empty -> one step with `Skip{CodeNotAffected, "No affected Go packages."}` (or "No db-gated packages are affected."). `shards: k>1` -> `Partition(candidates, in.Timings, k)`, one Step per non-empty shard with `Shard{i, len(shards)}`; unsharded -> `Packages = candidates`.
4. Return the plan.

**Glob semantics** -- port `scripts/ci/pathsfilter.go`'s `CompilePattern` (read it: `**`, `*`, leading `!`, fail-closed on anything else), adding `?`. Task 3 adds a parity test in the root module against `ci.CompilePattern` over a shared list of (pattern, path, want) triples.

**Partition** -- port the within-class greedy of `scripts/ci/selection/shard.go` (`Partition`): sort by seconds desc then name asc; place each into the lightest shard, ties to the lowest index; `n` capped at `MaxShards` and at `len(packages)`.

- [ ] Write `validate_test.go`: one table-driven test, one row per rule above, asserting `Code` and `Scope`, plus a row for the design record's D7 example manifest (built in Go as a `*Spec` literal) that must validate.
- [ ] Run `go test ./component/pipelines/ -run Validate` -> FAIL (undefined).
- [ ] Implement `validate.go`; run -> PASS.
- [ ] Write `glob_test.go` (`**/*.md` matches `a.md`, `x/y/a.md`; `dsl/**` matches `dsl/a/b.memql`, not `dslx/a`; `*.go` does not match `a/b.go`; `?` matches one non-slash rune; `!` negation; an unsupported `[` class returns an error) and `shard_test.go` (LPT placement with measured and unmeasured packages, n > len, n = 0 -> one shard, determinism under map iteration) -> FAIL -> implement -> PASS.
- [ ] Write `compile_test.go` against a fake `Selector`: (a) the D7 example in `pull_request`/affected: `deploy` and `notify` absent, `go-tests` sharded by the timing table with `DependsOn` the checks step keys, `os-checks` skipped "No change under bucket os" when nothing under `clients/**` changed and present when it did; (b) the same in `push`/full: every stage present, notify compiled to a notify step, `os-checks` runs; (c) `only: db-gated` and `not-db-gated` partition the candidates; (d) a need on a cluster-only pipeline -> `pipeline_fleet_not_consented` scoped "tests/os-checks"; (e) a secret outside the allowlist -> `pipeline_secret_not_allowed`; (f) affected selection empty -> one skipped step; (g) `Selection.Full` -> all packages.
- [ ] Run -> FAIL -> implement `compile.go` -> PASS. `gofmt -l`, `go vet`, then `go test -count=1 ./component/pipelines/` (purity test included).
- [ ] Commit: `pipelines: validate and compile the pipeline block` with `Refs #5489`.

---

### Task 2: Triggers -- the delivery, the mode table, the run key (leaf)

**Files:**
- Create: `component/pipelines/trigger.go`, `mode.go`, `url.go`, `trigger_test.go`, `mode_test.go`, `testdata/github/*.json`
- Modify: `.github/workflows/ci.yml` -- add `'component/pipelines/testdata/**'` to the `go` bucket next to `'component/procedure/testdata/**'`, with a two-line comment (the bucket test `TestEveryFilterBucketPathExists` requires the directory to exist, so add the line in the same commit as the first fixture).

**Interfaces (produced):**

```go
// Trigger is what one signed GitHub delivery asks for.
type Trigger struct {
	Event          Event  // "" for a rerequest
	Rerequest      bool   // check_run or check_suite rerequested
	CheckRunID     int64  // check_run.id for a check_run rerequest
	Repository     string // "owner/name", lower-cased
	DefaultBranch  string
	InstallationID int64
	SHA            string // head commit; "" for a release until resolved
	BaseSHA        string // pull_request.base.sha, merge_group.base_sha, push.before
	Branch         string // head branch name (no refs/heads/)
	PullRequest    int
	Fork           bool   // the head lives in another repository (D6)
	HeadRepository string // "owner/name" of the head, for the refusal text
	Title          string // first line of the commit message, the PR title, or the release name
	ReleaseTag     string
}

// Ignored is a delivery that asks this pipeline for nothing, with why.
type Ignored struct{ Reason string }

// ClassifyDelivery reads a GitHub webhook body by its SHAPE. headerEvent is
// the unsigned X-GitHub-Event; when present and it disagrees with the body,
// the delivery is ignored rather than believed.
func ClassifyDelivery(headerEvent string, body []byte) (Trigger, *Ignored, error)

// ModeFor is the event-to-mode table (D5 + release).
func ModeFor(e Event) (Mode, bool)

// Events is every event in the table, sorted.
func Events() []Event

// RunKey is the identity a run is deduplicated on (decision 1).
func RunKey(repository, sha string, mode Mode, event Event) string // "owner/name@<sha>:<mode>:<event>"

// Version is MEMQL_VERSION: the release tag for a release, else the SHA.
func Version(event Event, sha, releaseTag string) string

// RunPageURL is the check run's details link (decision 14).
func RunPageURL(osOrigin, runID string) string // osOrigin + "/?pipelineRun=" + url.QueryEscape(runID)

// CheckRunName is the check run's name for a pipeline.
func CheckRunName(pipelineName string) string // "MemQL / " + pipelineName
```

**Shape rules** (top-level keys of the JSON body):
- `merge_group` object: action must be `checks_requested` (else Ignored); Event merge_group; SHA `merge_group.head_sha`; BaseSHA `merge_group.base_sha`; Branch from `merge_group.head_ref` without `refs/heads/`; Title first line of `merge_group.head_commit.message`.
- `pull_request` object: action in {opened, synchronize, reopened} (else Ignored "pull request <action>"); SHA `pull_request.head.sha`; BaseSHA `pull_request.base.sha`; Branch `pull_request.head.ref`; PullRequest `number`; HeadRepository `pull_request.head.repo.full_name`; Fork = head repo null OR its full_name differs (case-insensitively) from `repository.full_name`; Title `pull_request.title`.
- `check_run` object: action `rerequested` -> Rerequest, CheckRunID `check_run.id`, SHA `check_run.head_sha`; else Ignored.
- `check_suite` object: action `rerequested` -> Rerequest, SHA `check_suite.head_sha`; else Ignored (check_suite `requested` arrives on every push once the app holds checks: write).
- `release` object: action `published` -> Event release, ReleaseTag `release.tag_name`, Title `release.name` or the tag; SHA stays "" (the trigger resolves the tag); else Ignored.
- Otherwise `ref` + `before` + `after`: Event push only when `ref == "refs/heads/" + repository.default_branch`; `after` all zeros (a deletion) -> Ignored; a tag ref or another branch -> Ignored; SHA `after`; BaseSHA `before` unless all zeros; Title first line of `head_commit.message`.
- Anything else -> Ignored "not a delivery pipelines run on".
- Every kind: Repository = lower-cased `repository.full_name`, DefaultBranch `repository.default_branch`, InstallationID `installation.id`. A SHA that is present but not 40 hex characters -> error. Missing `repository` or `installation` -> error.
- Header cross-check: `headerEvent` (when non-empty) must equal the body's kind name (`merge_group`, `pull_request`, `check_run`, `check_suite`, `release`, `push`); otherwise Ignored "header says X, body is Y".

**Fixtures** (`testdata/github/`, realistic, trimmed to the fields read; no secrets): `pull_request_opened.json`, `pull_request_synchronize.json`, `pull_request_closed.json`, `pull_request_fork.json`, `pull_request_deleted_fork.json` (head.repo null), `merge_group_checks_requested.json`, `push_default.json`, `push_other_branch.json`, `push_tag.json`, `push_delete.json`, `check_run_rerequested.json`, `check_suite_rerequested.json`, `check_suite_requested.json`, `release_published.json`, `ping.json`.

- [ ] Write `mode_test.go`: the table exactly (`pull_request`->affected; `merge_group`, `push`, `release`->full; unknown -> false), `Events()` sorted, `RunKey` distinct for merge_group vs push on one SHA and equal for two identical inputs, `Version`, `RunPageURL` escaping, `CheckRunName`.
- [ ] Write `trigger_test.go`: one case per fixture asserting the Trigger or the Ignored reason; header mismatch; malformed SHA error; fork detection (both shapes).
- [ ] Run -> FAIL -> implement `mode.go`, `url.go`, `trigger.go` (encoding/json into small structs with pointer fields) -> PASS.
- [ ] Add the ci.yml bucket line; `go test -count=1 ./scripts/ci/ -run TestEveryFilterBucketPathExists` -> PASS.
- [ ] Commit: `pipelines: classify GitHub deliveries and the event-to-mode table` with `Refs #5488`, `Refs #5491`.

---

### Task 3: The Go import graph, the affected set, timings, the check-run text (leaf)

**Files:**
- Create: `component/pipelines/gograph.go`, `affected.go`, `timings.go`, `summary.go`, `mask.go`, `gograph_test.go`, `affected_test.go`, `timings_test.go`, `summary_test.go`, `mask_test.go`, `testdata/gotree/**`, `testdata/go-test-output.txt` (copy of `scripts/ci/selection/testdata/go-test-output.txt`)
- Create (root module): `scripts/ci/pipelines_parity_test.go` (package `ci`) -- glob parity vs `CompilePattern`, `Partition` parity vs `selection.Partition` for a single default class, `ParseGoTestOutput` parity vs `selection.ParseGoTestOutput` filtered to the same prefix.
- Modify: root `go.mod` -- `require github.com/znasllc-io/memql/component/pipelines v0.0.0` (the replace exists); `GOWORK=off go mod tidy -diff` must be clean afterwards.

**Interfaces (produced):**

```go
type Package struct {
	ImportPath string
	Dir        string   // repo-relative, "." for the root
	Imports    []string // first-party import paths, sorted, deduplicated, test imports included
}

type Graph struct{ /* unexported */ }

// ScanGoTree reads the import graph from source. Every go.mod names a
// module; a directory holding .go files is a package of the nearest module
// above it; every import any file declares -- any build tag, test files
// included -- is an edge. Directories named testdata or vendor, or starting
// with "." or "_", are skipped, as the go tool skips them. A file that does
// not parse marks the graph Incomplete rather than failing the scan.
func ScanGoTree(fsys fs.FS) (*Graph, error)

func (g *Graph) Packages() []Package          // sorted by import path
func (g *Graph) Incomplete() []string           // paths that failed to parse, sorted
func (g *Graph) PackageAt(path string) (string, bool) // the package owning a repo path: its directory or the nearest above

// Selection is what a change selects.
type Selection struct {
	Full     bool
	Reason   string
	Seeds    []string // packages changed directly
	Packages []string // sorted import paths; every package when Full
}

// FullTriggers are the paths whose change selects everything, beside a
// pipeline's own select.full globs.
var FullTriggers = []string{"go.mod", "go.sum", "go.work", "go.work.sum"} // matched by base name anywhere
const ManifestPath = "memql-package.yaml" // matched at the root

// Affected selects what changed touches: the package each changed path
// belongs to (its directory or the nearest package above), plus every
// package importing one of them, transitively. Full when: the change list
// is empty, a full trigger or a select.full glob matches, or the graph is
// incomplete.
func Affected(g *Graph, changed []string, fullGlobs []string) (Selection, error)

// GraphSelector adapts a graph and its selection to Compile's Selector.
func GraphSelector(g *Graph, s Selection) Selector

// ParseGoTestOutput reads `go test` output and returns each PASSING
// package's wall time by import path: only anchored `ok  <pkg>  <N>s`
// lines count; cached results, failures and mid-line quotes do not.
func ParseGoTestOutput(r io.Reader) (map[string]float64, error)

// MergeTimings returns table with every observed package's time replaced
// and every other row kept.
func MergeTimings(table, observed map[string]float64) map[string]float64

// StepReport is one step as the check run reports it.
type StepReport struct {
	Key, Stage, Name string
	Status           string // succeeded|failed|cancelled|skipped|refused|pending|running
	DurationMs       int64
	Code, Message    string
	LogTail          string
}

// RunReport is a run as the check run reports it.
type RunReport struct {
	Pipeline   string
	Mode       Mode
	Event      Event
	SHA        string
	PullRequest int
	Status     string // queued|in_progress|completed
	Conclusion string // success|failure|cancelled|refused, when completed
	Refusal    *Refusal
	Steps      []StepReport
}

// CheckOutput renders the check run's title, summary and text (D4: the
// stage table as summary). Plain Markdown, no emojis; text carries the
// failed steps' last lines; each is cut to GitHub's 65535-byte field limit.
func CheckOutput(r RunReport) (title, summary, text string)

// MaskSecrets replaces every occurrence of each value (4+ bytes) with ***.
func MaskSecrets(text string, values []string) string
```

**Check-run copy** (invoke `frontend-design:frontend-design` before writing it; it is the one surface of this epic a person reads on GitHub; clean and minimal per `clients/os/SUPERVISED-VISUAL-COMPOSITION.md` -- proposals never read as results, a refusal names its remedy):
- Titles: `Queued`, `Running: <stage>`, `Passed: <n> stages in <duration>`, `Failed at <stage>: <step key>`, `Cancelled`, `Refused: <one-line reason>`.
- Summary: one meta line (`Mode affected · Pull request #42 · Commit abc1234`), then a table `| Stage | Result | Steps | Time |` with Result one of `Passed`, `Failed`, `Not run: an earlier stage failed`, `Skipped: <reason>`, `Running`, `Waiting`; then a `Failed steps` list naming each key with its code and message. A refused run renders the refusal's detail and scope and no table.
- Durations `1m 02s` / `45s`; unknown -> `-`.

- [ ] Fixture tree `testdata/gotree/` (a module `example.test/shop` with root package, `a`, `b` importing `a`, `c` importing `b` only from a `_test.go`, `tagged` importing `a` under `//go:build tools`, a nested module `sub` with its own go.mod importing `example.test/shop/a`, `testdata/` and `_hidden/` dirs that must be skipped, a `broken/` dir whose file does not parse). Note: files in `testdata` are invisible to the go tool, which is what makes them fixtures.
- [ ] Write `gograph_test.go` (edges incl. test-only and tag-only, nested module resolution, skipped dirs, Incomplete lists `broken/x.go`, `PackageAt` for a non-Go file) and `affected_test.go` (seed + reverse closure; go.mod anywhere -> Full; manifest -> Full; `select.full` glob -> Full; empty list -> Full; incomplete graph -> Full; a docs path under no package -> no seed) -> FAIL -> implement -> PASS.
- [ ] Write `timings_test.go` over the copied fixture (cached, FAIL, mid-line decoys ignored) and `MergeTimings` -> FAIL -> implement -> PASS.
- [ ] Write `summary_test.go` (each title shape; table rows for passed/failed/blocked/skipped; refused run; truncation at 65535 bytes; no emoji code points) and `mask_test.go` -> FAIL -> implement -> PASS.
- [ ] Root module: add the require, write `scripts/ci/pipelines_parity_test.go`, run `go test -count=1 ./scripts/ci/` and `GOWORK=off go mod tidy -diff` at the root.
- [ ] Commit: `pipelines: the import graph from source, the affected set, timings and the check run's text` with `Refs #5489`, `Refs #5490`.

---

### Task 4: component/packages -- the manifest field, the catalogue, the push parse, the GitHub client

**Files:**
- Modify: `component/packages/manifest.go` (add `Pipeline *pipelines.Spec \`yaml:"pipeline,omitempty" json:"pipeline,omitempty"\`` to `Manifest`; NO semantic validation in `ParseManifest`), `component/packages/refusal.go` (a `// Pipelines (epic memql#5477; substrate codes, epic memql#5478)` section with one `CodePipelineXxx = "pipeline_xxx"` literal per `pipelines.Codes()` entry), `component/packages/feeds.go` (`parseGitHubPush` event-aware), `component/packages/githubapp/client.go` (a request path with a JSON body)
- Create: `component/packages/refusal_pipelines_parity_test.go`, `component/packages/githubapp/checks.go`, `component/packages/githubapp/repos.go`, `component/packages/pipeline_access.go`, tests beside each
- Modify: root `go.mod` require for `component/pipelines` if Task 3 has not merged (one line; tidy-clean)

**Interfaces (produced):**

```go
// githubapp
type CheckRunOutput struct{ Title, Summary, Text string }
type CheckRun struct {
	Name, HeadSHA, Status, Conclusion, DetailsURL, ExternalID string
	StartedAt, CompletedAt time.Time // zero = omitted
	Output *CheckRunOutput
}
func (c *Client) CreateCheckRun(ctx context.Context, bearer, owner, repo string, run CheckRun) (int64, error)
func (c *Client) UpdateCheckRun(ctx context.Context, bearer, owner, repo string, id int64, run CheckRun) error
type RepositoryInfo struct{ FullName, DefaultBranch string; Private bool }
func (c *Client) Repository(ctx context.Context, bearer, owner, repo string) (RepositoryInfo, error)        // GET /repos/{o}/{r}
func (c *Client) BranchHead(ctx context.Context, bearer, owner, repo, branch string) (sha, message string, err error) // GET /repos/{o}/{r}/branches/{b}
type PullRequestHead struct{ Number int; Title, HeadSHA, HeadRef, HeadRepository, BaseSHA string }
func (c *Client) OpenPullRequests(ctx context.Context, bearer, owner, repo string) ([]PullRequestHead, error) // state=open, per_page=100, one page
func (c *Client) Compare(ctx context.Context, bearer, owner, repo, base, head string) (files []string, complete bool, err error) // complete=false at 300+ files
func (c *Client) CommitForRef(ctx context.Context, bearer, owner, repo, ref string) (sha, message string, err error)
func (c *Client) Tarball(ctx context.Context, bearer, owner, repo, sha string) (io.ReadCloser, error) // follows the codeload redirect; Authorization never sent cross-host
// ScopedInstallationToken mints an UNCACHED installation token narrowed to
// the named repositories and permissions (POST access_tokens with a body).
// The substrate mints the clone token a Job carries this way: contents read
// on one repository, never the installation-wide token the cache holds.
func (c *Client) ScopedInstallationToken(ctx context.Context, installationID int64, repositories []string, permissions map[string]string) (token string, expiresAt time.Time, err error)

// packages
// GitHub is the node's GitHub App client, or nil when the cluster has none.
func (i *Integration) GitHub() *githubapp.Client
// InstallationToken mints the installation token a source owner's grant
// reaches for owner/repo, after verifyGrantRepository confirms the grant
// still reaches it. Memory only.
func (i *Integration) InstallationToken(ctx context.Context, credentialID, ownerUserID, owner, repo string) (token string, installationID int64, err error)
```

Status errors keep `githubapp.StatusError{Status, Endpoint, RateLimited}`; callers test `errors.As(err, &se) && se.Status == 403`.

**parseGitHubPush:** a body carrying a top-level `pull_request`, `merge_group`, `check_run`, `check_suite` or `release` key is NOT a push (return a sentinel the handler treats as "nothing to note", no error); a body with an empty `ref` is not a push either (today an empty ref skips the ref filter -- that is the bug).

- [ ] Tests first: `manifest_test` additions (a manifest with the D7 block parses with `Pipeline` populated; a pipeline block with an unknown need STILL parses; an unknown key inside `pipeline:` is `package_manifest_invalid`, like any key); `analyze_test` addition (a source whose pipeline block names an unknown need analyzes with no refusal); `feeds_test` (a `pull_request synchronize` body and a `check_suite` body move nothing and start no auto-run; a real push still does); `refusal_pipelines_parity_test.go` (every `pipelines.Codes()` entry appears as a catalogued constant VALUE in `refusal.go`, read with the same regex the OS test uses: `^\s*(?:const\s+)?Code\w+\s*=\s*"([a-z_]+)"`); githubapp tests with the existing `fakeHub` (create body fields, update PATCH, 403 surfaces as StatusError 403, compare truncation at 300, pulls parsing incl. a deleted head repo, tarball redirect drops Authorization).
- [ ] Run -> FAIL -> implement -> PASS: `go test -count=1 ./component/packages/...` (db-gated cases skip without a DSN; the new ones are pure).
- [ ] `GOWORK=off go build ./... && GOWORK=off go mod tidy -diff` at the root.
- [ ] Commit: `packages: the pipeline block, the pipelines catalogue, an event-aware push feed and the GitHub calls a pipeline needs` with `Refs #5487`, `Refs #5489`.

---

### Task 5: The GitHub App asks for checks, pull requests and merge queues

**Files:**
- Modify: `component/identity/githubconnect/manifest.go` (`RequestedPermissions()` = `{checks: write, contents: read, merge_queues: read, metadata: read, pull_requests: read}`; public `DefaultEvents` = `[check_run, merge_group, pull_request, push, release]`; the hook-off branch keeps none; the description says the app reads the repositories people choose and reports checks on them; update the C8 comments at `manifest.go:30-32` and `component/identity/http/github_app_callback.go:46-49`), `component/identity/githubconnect/manifest_test.go`, `conversion_test.go` fixture, `component/identity/web/github_app_setup_test.go` (`len(DefaultPermissions)` 2 -> 5), `component/identity/http/github_app_callback_test.go` (wider-than-asked case still refuses), `docs/public/operate/github-connect.md` (permissions table, events, the "nothing else" prose becomes the pipelines reason; a section "Upgrading an app registered before pipelines": GitHub -> the app's settings -> Permissions & events -> add the three permissions and five events -> each installation accepts; until then check runs answer 403 and runs carry `pipeline_check_permission_missing`), `docs/public/operate/inbound-delivery.md` (the github block names the forwarded headers and dedupe key and says pipelines read the same deliveries), `docs/public/operate/packages.md:538-552` (the update feed reads pushes only; other deliveries are pipelines'), `clients/os/src/apps/deployables/sources/GithubAppSetup.tsx:52` copy (and any OS test pinning it).
- Every test listed in section 5 of the GitHub research (`TestTheManifestIsTheRegistrationTheOperatorGuideDescribes`, `TestTheAskIsContentsAndMetadataReadAndNothingElse` renamed to the new ask, `TestALocalClusterRegistersItsWebhookOff`, `TestPermissionsAreCheckedAgainstWhatWasAsked`).

- [ ] Update the tests to the new ask and events -> FAIL -> update `manifest.go` -> PASS (`go test -count=1 ./component/identity/...`; `go test -count=1 -tags bff ./app/ -run GitHub` for `app/transport_inbound_github_test.go`).
- [ ] Docs and OS copy; `cd clients/os && npm run typecheck && npx vitest run test/deployables` (use the OS lane's own setup; never run vitest from the repo root).
- [ ] Commit: `githubconnect: the app asks for checks, pull requests and merge queues` with `Refs #5487`.

---

### Task 6: dsl/pipelines, the work step's new fields, routing

**Files:**
- Create: `dsl/pipelines/concepts.memql`, `shapes.memql`, `queries.memql`, `mutations.memql`, `builtins.memql`, `automations.memql`
- Modify: `dsl/embed.go` (`all:pipelines`), `embed_inventory_test.go` (count measured from the gate's failure message), `dsl/work/concepts.memql` (step: `logFileId string`, `artifactFileIds []string`; goal: `requestedVia` gains `"pipeline"`), `dsl/work/shapes.memql` (`workStepFull` gains both), `dsl/work/mutations.memql` (`updateWorkStep` and `reassertWorkStepVersion` accept both), `component/node/routing.go` (core slice: created + updated broadcast for `v1:pipelines:pipeline`, `run`, `channel`, with the house comment "who writes, who reads, SAFE TO BROADCAST, checked"), `component/node/routing_reach_test.go` (three entries), `test/dslconformance/server_only_parsed_test.go` (`want` entries), `component/conceptfields/concept-fields.snapshot.json` (`make concept-snapshot`), generated SDKs (`make sdk-gen`), `component/automations/strict_automation_boot_test.go` (`shippedAutomationCount` 75 -> 77, with a note), `component/automations/steps/testdata/automation_corpus/pipelines/*.json` (regenerate with `-update`, read the diff), `app/automation_schedule_placement.go` (pollPipelines on agent replicas behind a scoped cron leader `pipelines-poll`, beside the package poll) + its test.

**Concepts** (all three: `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`, `ownerUserId string @serverSet`, `accountId string` with `@relationship(type="references", as="forAccount", field="accountId", target=account, direction="outgoing")`, a `@displayCard(...)`, `@relationship(type="parent", field="ownerUserId", target=user, ...)`):

`pipeline` -- `packageId string!` (references `package`, as `forSource`), `name string!`, `repository string!` (lower-cased owner/name: the trigger's match key), `defaultBranch string`, `installationId string!`, `credentialId string!` (references `sourceCredential`, as `mintsUnder`), `delivery enum("webhook","poll")!`, `compute enum("cluster","cluster_and_fleet")`, `status enum("active","disconnected")!`, `secretNames []string`, `channelIds []string`, `heads object` (`{"branch:<name>": sha, "pr:<n>": sha}`: what the poll has seen), `timings object` (import path -> seconds), `timingsRunId string`, `timingsUpdatedAt datetime`, `connectedAt datetime`.

`run` -- `pipelineId string!` (references `pipeline`), `repository string!`, `sha string!`, `mode enum("affected","full")!`, `event enum("pull_request","merge_group","push","release")!`, `runKey string!`, `attempt int!`, `trigger enum("webhook","poll","rerun")!`, `rerunOf string`, `deliveryId string`, `pullRequest int`, `headBranch string`, `baseSha string`, `title string`, `version string`, `status enum("queued","in_progress","completed")!`, `conclusion enum("", "success","failure","cancelled","refused")`, `refusalCode string`, `refusalMessage string`, `refusalScope string`, `checkRunId string`, `checkRunState enum("", "written","refused","unavailable")`, `notes []object` (`{code, message}`), `workRunId string` (references work `run`), `workGoalId string` (references work `goal`), `driverNodeId string`, `driverHeartbeatAt datetime`, `cancelRequested bool`, `cancelledBy string`, `stages []object` (`{name, status, durationMs, steps, failed}`), `queuedAt datetime!`, `startedAt datetime`, `finishedAt datetime`, `durationMs int`.

`channel` -- `name string!`, `kind enum("discord","email")!`, `secretRef string`, `recipients []string`, `status enum("active","archived")!`. (Writers arrive with epic memql#5480; this epic declares the concept, its tier and its routing.)

Bare names `run` and `channel` exist in other domains: inside `dsl/pipelines` same-domain resolution applies; verify with `go run ./cmd/memqllint dsl/` and `TestSpecBindingsResolveAcrossFullTree`. Id-bearing fields that name GitHub ids (`installationId`, `checkRunId`, `deliveryId`) must satisfy `TestIdBearingFieldsDeclareRelationship` the way `sourceConnection.installationId` does today -- read that gate and mirror the existing escape.

**Reads** (`queries.memql`):
- Person-facing, owner-scoped (`row.ownerUserId == actor.userId`): `pipelinesForOwner` (sorted `connectedAt` desc, paginated), `pipelineForOwner(pipelineId)`, `pipelineForPackage(packageId)`, `pipelineRunsForOwner(pipelineId?)` (sorted `queuedAt` desc, paginated), `pipelineRunForOwner(runId)`, `pipelineChannelsForOwner`.
- Server-only, cluster-owner conjunct (`actor.isClusterOwner == true`, `@actor`, `@serverOnly`, a `///` reason): `pipelinesForRepository(repository)` (status active), `pipelinesPolled()` (delivery poll, status active), `pipelineRunsForKey(runKey)`, `pipelineRunsForPipelineSha(pipelineId, sha)`, `pipelineRunByCheckRun(repository, checkRunId)`, `pipelineRunsUnfinished()` (status queued or in_progress), `pipelineRunById(runId)`, `pipelineById(pipelineId)`. Bound every list (`paginate`/limit); `@unbounded` cannot combine with `sort`.

**Writes** (`mutations.memql`, every one `@serverOnly` with a `///` reason -- Go writes them under the owner's borrowed authority with internal origin): `createPipeline` (stamps `ownerUserId: actor.userId`, `status: "active"`, `connectedAt: now`), `updatePipeline` (read-merge: name, defaultBranch, delivery, compute, status, secretNames, channelIds, heads, timings, timingsRunId, timingsUpdatedAt), `createPipelineRun` (stamps owner, every open-time field), `updatePipelineRun` (read-merge: every lifecycle field). Remember the read-merge `??` trap: a field a later writer clears must be written explicitly, never defaulted with `??` against the stored row.

**Builtins** (`builtins.memql`, `@args(profile="object")`): `pipelinesTrigger(inboundRequestId string!, source string, body string, headersJson string)` -> `integration.pipelines.trigger`; `pipelinesPoll()` -> `integration.pipelines.poll`; `pipelinesConnect(packageId string!, delivery string!, compute string, secretNames []string)` -> `integration.pipelines.connect`; `pipelinesDisconnect(pipelineId string!)` -> `integration.pipelines.disconnect`; `pipelinesRerun(runId string!)` -> `integration.pipelines.rerun`; `pipelinesCancel(runId string!)` -> `integration.pipelines.cancel`. Each field documented with `@description`.

**Automations** (`automations.memql` only):

```memql
/// A GitHub delivery opens pipeline runs (design record D4-D6). Filtered to the
/// first, received version of a github row: a status write republishes
/// node.created, and a redelivery collapses onto the same row id, so the
/// trigger's run key is what keeps either from opening a second run.
@trigger(event="node.created", concept="v1:platform:inboundRequest")
@filter(row => row.source == "github" && row.status == "received")
automation triggerPipelinesOnGitHubDelivery {
  args {
    id          any
    source      any
    body        any
    headersJson any
  }
  opened := builtin pipelinesTrigger(inboundRequestId: args.id, source: args.source, body: args.body, headersJson: args.headersJson)
}

/// Every minute: delivery=poll pipelines read the default branch's head and the
/// open pull requests' heads and open runs for SHAs not seen, and runs no agent
/// claimed, or whose driver went silent, are claimed again (D4, D11).
@trigger(schedule="0 * * * * *")
automation pollPipelines {
  polled := builtin pipelinesPoll()
}
```

(Adjust to the exact edition-2026 automation grammar; the `args` block and named-argument call are the shape `dsl/platform/automations.memql` uses.)

- [ ] Write the DSL; run `go run ./cmd/memqlmigrate --rewrite=doc-comment-descriptions,accept-stamp,required-sigil,same-domain-use -w dsl/pipelines/` and `go run ./cmd/memqllint dsl/` until clean.
- [ ] `make concept-snapshot`, `make sdk-gen`; read both diffs for deletions.
- [ ] Routing rules + reach-test entries; `go test -count=1 ./component/node/ -run 'Routing|Reach'`.
- [ ] Automation count, goldens (`go test -count=1 -run 'TestAutomationCorpusRuns$' ./component/automations/steps/ -update`, read the diff), schedule placement + test.
- [ ] `go test -count=1 . ./test/dslconformance/ ./dsl/... ./component/automations/... ./component/language/...` and the root package uncached; then the embed inventory.
- [ ] Commit: `dsl/pipelines: pipeline, run and channel, their reads and writes, the trigger and the poll` with `Refs #5486`, `Refs #5488`, `Refs #5490`.

---

### Task 7: The work spine -- the journal's new writes, runner-owned runs

**Files:**
- Modify: `component/workjournal/journal.go` (+ tests, `internal_origin_precondition_test.go` covering every new write), `integrations/work/dispatch.go`, `integrations/work/sweep.go`, `integrations/work/acts.go` (+ tests), `integrations/go.mod` (require `component/pipelines`)

**Interfaces (produced):**

```go
// workjournal
type Work struct {
	// ...existing fields...
	TriggeredBy string // default "system"; a pipeline run writes pipeline:<mode>
	// QueueSteps writes every declared step at `pending` at open, with its seq,
	// stepType, kind, call and dependsOn, so later stages exist as rows.
	QueueSteps bool
}
type StepDecl struct {
	Key, Kind string
	StepType  string         // default "function"
	DependsOn []string
	Call      map[string]any // {construct, name, stage}
}
// Reopen returns a handle on a run this journal (or another replica's) opened,
// for a resumed driver. It writes nothing.
func (j *Journal) Reopen(ownerUserID, goalID, runID string, steps []StepDecl, started time.Time) *Run
func (r *Run) Heartbeat(ctx context.Context)            // updateWorkRun heartbeatAt
func (r *Run) Cancelled(ctx context.Context, code, message string)
func (s *Step) Cancelled(ctx context.Context, why string)
// Receipt is a step's close with everything a pipeline step reports.
type Receipt struct {
	Status          string // done|failed|skipped|cancelled
	Result          map[string]any
	Code, Message   string
	DurationMs      int64
	Binding         map[string]any // surface, nodeId, workerId, machineLabels, jobName
	LogFileID       string
	ArtifactFileIDs []string
}
func (s *Step) Finish(ctx context.Context, r Receipt)
```

`integrations/work`: replace `isProcedureReplay` checks with two predicates -- `runnerOwned(triggeredBy)` (procedure: OR `pipelines.WorkTriggerPrefix`) used by `CanDispatchStoredRun` and `dispatchRun`; `runnerOwnsRecovery(triggeredBy)` (pipeline: only) used by `SweepWaiting` to SKIP the run entirely (the pipelines runner judges its own liveness). `acts.go requireExecutable` refuses a pipeline run with an error naming `pipelinesRerun`.

- [ ] Tests first: journal (QueueSteps writes pending rows with dependsOn; Finish renders durationMs/binding/logFileId/artifactFileIds/errorCode; Cancelled; Heartbeat; Reopen writes nothing; triggeredBy rendered), dispatch (`TestTheDispatcherNeverAdmitsAPipelineRun` mirroring the procedure test, both the event and the recovery path), sweep (a stale pipeline run is neither redispatched nor abandoned), acts (re-run/branch refuse) -> FAIL -> implement -> PASS: `go test -count=1 ./component/workjournal/ ./integrations/work/...`; `cd integrations && GOWORK=off go build ./... && GOWORK=off go mod tidy -diff`.
- [ ] Commit: `work: the journal writes a pipeline's queued steps and receipts; pipeline runs are their runner's` with `Refs #5490`.

---

### Task 8: The readiness item and the OS copy

**Files:**
- Modify: `scripts/secrets/manifest.yaml` (`modules:` entry `pipelines`: description "Pipelines run a repository's checks from its memql-package.yaml and report them on GitHub.", `hostedBy: {nodeTypes: [agent]}`, `evaluator: "integration:pipelines"`, `optional: true`, `dismissable: true`), embedded copy via `make env-registry-sync`, `component/envregistry/modules.go` (`Optional`, `Dismissable` with strict decode; `ValidateModules`: dismissable requires optional, optional excludes core) + tests, the readiness verdict path threading both flags exactly as `core` is threaded (`readiness_eval.go`, `readiness_write.go`, the readiness concept's two new fields + its mutation args, `component/memql/readiness` `Verdict`, Go and TS fold fixtures, `clients/os/src/system/readinessFold.ts`), `clients/os/src/system/modules.ts` (the four records; settings section as the existing optional modules map theirs), `component/envregistry/os_modules_parity_test.go` expectations, `docs/public/operate/configuration-readiness.md` ("Adding a module": the two keys).
- Modify: `clients/os/src/apps/deployables/packages/refusals.ts` -- copy (title, next) for every `pipeline_*` code; notes and skips get copy too; `clients/os/test/deployables/refusals.test.ts` must stay green. The substrate's sentences and remedies (from its session) are the source for its fourteen codes; adapt them to the table's title/next shape. Invoke `frontend-design:frontend-design` before writing copy: short, plain, a remedy for every failure, no blame, no emojis.

- [ ] Tests first (modules decode/validate cases; OS parity), implement, `make env-registry-check`, `go test -count=1 ./component/envregistry/ ./component/memql/readiness/...`, `cd clients/os && npm run typecheck && npx vitest run test/system test/deployables`, `make concept-snapshot` and `make sdk-gen` if the readiness concept changed.
- [ ] Commit: `readiness: an optional, dismissable pipelines item; OS copy for the pipeline codes` with `Refs #5486`.

---

### Task 10a: component/pipelinerun -- store, GitHub port, connect, trigger, poll, status, cancel, rerun

**Files:** `component/pipelinerun/{plugin.go, store.go, store_dsl.go, github.go, connect.go, trigger.go, open.go, poll.go, status.go, cancel.go, rerun.go}` + tests; `app/plugins_core.go` (blank import), `app/integrations_pipelines.go` (wiring on every node), `module_taxonomy_test.go` (`pipelines` plugin kind), the internal-origin allowlist (`component/auth/call_origin.go` + `call_origin_conformance_test.go`) with a written argument.

**Design:**
- `Integration` holds narrow ports so every decision is unit-tested with fakes: `Store` (typed reads/writes over the DSL constructs of Task 6), `GitHub` (token, check runs, compare, heads, pulls, commit-for-ref, filtered tree), `Gate` (`func(ctx, key string, fn func(context.Context) error) error`, production = `githubconnect.WithGate` over the direct DB), `Secrets` (`func(ctx, name) (string, error)`, production = `PluginContext.ResolveSystemSecret`), `OSOrigin func() string` (from the cluster domain via `component/frontdoor`'s platform-site rule), `Now`.
- Contexts: cross-owner reads under `auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "pipelines"))`; every write under `auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner))`; every read-modify-write under the gate with `memql.ContextWithFreshRead`.
- `connect(packageId, delivery, compute, secretNames)`: caller must be a person who owns the package (read under the caller's actor); the package must be a GitHub repo source with a credential; mint through `packages.InstallationToken` (proves the grant reaches the repo); read the repository's default branch and head; read `memql-package.yaml` at that head through the filtered tree; `Pipeline == nil` -> `pipeline_not_declared`; `pipelines.Validate` refusal returned as is; create or update the one pipeline row (id derived from the package id); answer `{pipelineId, repository, delivery, compute, stages: [names]}`.
- `trigger(inboundRequestId, source, body, headersJson)`: source must be `github`; classify; for each active pipeline of the repository whose `installationId` equals the delivery's: rerequest -> `rerun` of the run found by check-run id (check_run) or of the newest run per (pipeline, sha) (check_suite); release -> resolve the tag's SHA; else `open`.
- `open(pipeline, opening)`: under the gate keyed on the run key -- fresh read of `pipelineRunsForKey`; an existing run and not a rerun -> stop; attempt = max + 1; create the check run (queued; or completed/failure for a fork with the refusal text) with the run's id as `external_id`; create the run row (`queued`, or `completed`/`refused` for a fork) with `checkRunId` or `checkRunState`; a 403 -> `checkRunState: refused` + note `pipeline_check_permission_missing`; no app/token -> `checkRunState: unavailable`.
- `poll()`: for each polled pipeline: token, default branch head, open pull requests; first poll (no heads) records the baseline only; later polls open `push` for a moved default head and `pull_request` (fork-aware) for a new or moved PR head; drop closed PRs' keys; write heads back under the gate. Then `recover()` (Task 10b) when this node drives.
- `status(probe)`: the readiness evaluator's contract (read `readiness_eval.go:181-213, 283-301` and answer in its shape): app installed with checks: write (from `githubconnect` resolution), at least one active pipeline, compute confirmed (an executor is registered on this node).
- `RequestCancel(ctx, runID, by)` (exported Go API the substrate calls) and the `cancel` capability (caller must own the run): set `cancelRequested`, `cancelledBy` under the gate; a queued run with no driver is concluded cancelled on the spot (check run completed, conclusion cancelled).
- `rerun(runId)` capability: caller must own the run; opens attempt + 1 with the original's mode and event (trigger `rerun`, `rerunOf`).

- [ ] Fakes for every port; unit tests per behaviour above (including Review Focus 1 and 3); `go test -count=1 ./component/pipelinerun/`.
- [ ] Commit: `pipelinerun: connect, trigger, poll and the check run's opening` with `Refs #5487`, `Refs #5488`.

### Task 10b: component/pipelinerun -- the driver

**Files:** `component/pipelinerun/{driver.go, lease.go, tree.go, secrets.go, checkrun.go, timings.go, recover.go}` + tests; `app/integrations_pipelines_agent.go` (`//go:build agent`: subscribe `graph.node.created|updated.v1:pipelines:run` to the driver), `app/automation_schedule_placement.go` if Task 6 did not finish it.

**Design:**
- `HandleRunEvent(ev)`: a `queued` run with no `driverNodeId`, or one whose `cancelRequested` is set while this node drives it, is acted on; everything else returns at once.
- `claim(runID)`: under the gate keyed `pipelines.drive:<runID>`, fresh read; terminal -> no; another node's lease younger than 120 s -> no; else write `driverNodeId` = this node, `driverHeartbeatAt` = now.
- `drive(run)`: heartbeat goroutine every 30 s (gated RMW: lost lease -> stop driving without writing; `cancelRequested` -> cancel); load the pipeline (disconnected -> conclude `pipeline_disconnected`); token (failure -> conclude with the grant's code); `queued` -> `in_progress` + check run `in_progress`; fetch the filtered tree; `ParseManifest` (refusal -> conclude failure with it); graph + `Affected` when `NeedsSelector`; compare base...sha in affected mode; `Compile` (refusal -> conclude failure, no work run); open the work run (`workjournal.Begin` with `TriggeredBy: pipelines.WorkTriggerPrefix + mode`, `RequestedVia: "pipeline"`, `QueueSteps: true`, steps in plan order with `StepType: "exec"`, `Kind: "deterministic"`, `DependsOn`, `Call{construct: "pipeline", name: <step name>, stage}`) or `Reopen` it on resume (steps already `done`/`skipped` are kept; a step `running` with no receipt is re-sent with the SAME attempt); run stages in order, steps of a stage concurrently (at most 16 at once): skip -> `Finish(skipped)`; notify -> skipped `pipeline_notify_unavailable`; command -> resolve secrets (allowlist + `MEMQL_` refusal; missing -> failed `pipeline_secret_missing`), `Executor.Execute` with the request (`Environment()` is the runner's), a hard deadline of timeout + 10 min (`pipeline_step_timeout`), no executor -> failed `pipeline_runner_unavailable`, error -> failed `pipeline_executor_error`; receipt with binding from `Where`, `logFileId`, `artifactFileIds`, notes in result metadata. A failed stage -> every later step skipped `pipeline_stage_blocked`. Cancel -> `Executor.Cancel`, unfinished steps cancelled, conclude cancelled.
- `conclude`: pipelines run `completed` + conclusion + `stages` + `finishedAt` + `durationMs` (+ refusal fields); work run `Succeeded`/`Failed`/`Cancelled`; check run `completed` with `CheckOutput` (log tails masked with the step's secret values); a successful FULL run merges every result's `Timings` into the pipeline's table under the gate.
- `recover()`: `pipelineRunsUnfinished` -> claim and drive (a) queued runs no node claimed within 20 s, (b) in-progress runs whose heartbeat is older than 120 s.

- [ ] Unit tests with fakes: the full happy path (step order, dependsOn, receipts, check-run sequence queued -> in_progress -> completed with the table), compile refusal (no work run, failed run, check run failure), fork never driven, no executor (`pipeline_runner_unavailable`, check run failure), a failed stage blocks the rest, cancellation mid-stage reaches `Executor.Cancel`, lease lost stops writes, resume re-sends the same attempt and keeps done steps, timings merge only on a successful full run, masking (Review Focus 5).
- [ ] `go test -count=1 ./component/pipelinerun/`; `go vet -tags agent ./app/`; `go build -tags agent . && go build .`.
- [ ] Commit: `pipelinerun: an agent drives a run over the work spine and reports it` with `Refs #5490`.

---

### Task 11: The hop test

**Files:** `test/pipelinehop/{main_test.go, hop_test.go, fakes_test.go}`; `scripts/ci/db-gated-packages.sh` (`DB_GATED_TREES` += `test/pipelinehop`; follow the selector notes in that script and `scripts/cidb`).

**Shape:** modelled on `test/inboundhop` (two engines over one Postgres, `dbtest.EnsureSchema`, skip unless reachable or `MEMQL_REQUIRE_DB=1`) plus `test/clustere2e/automation_run_routing_test.go`'s mesh link (forward only what `node.ForwardDecisionFor` forwards). Engine A plays the bff: the real `inbound` handler stages an HMAC-signed GitHub delivery; the shipped `triggerPipelinesOnGitHubDelivery` automation runs on A (`ExecuteWithEvent`; note it skips `@filter`, so assert the filter separately). Engine B plays the agent: its pipelinerun driver is subscribed to B's bus; a fake executor records requests; a fake GitHub (`httptest`) serves the token mint, check runs, compare and a tarball of a tiny repository with a `pipeline:` block.

**Cases:** (1) a pull_request opened delivery on A opens a queued run; the run's created event crosses to B (and `v1:platform:inboundRequest` does not); B claims it (`driverNodeId` = B), executes the steps in order with `StepRequest.RunID`/`WorkRunID`/`StepKey` matching the rows, and the fake GitHub sees create(queued) from A, then update(in_progress) and update(completed, success) from B; (2) a fork pull request: one refused run, a failure check run, the executor never called; (3) the same delivery twice plus a poll for the same head: one run, one check run; (4) the table: pull_request -> affected, merge_group -> full, push -> full, release -> full (the tag resolved), check_run rerequested -> attempt 2 of the original's mode.

- [ ] Write the test; run it against a fresh database on the CI image (`docker run ... d8336a5c9cc4` per the db memory notes, `sslmode=disable`, the four extensions) with `MEMQL_REQUIRE_DB=1 -v`, reading `--- PASS` lines; add the tree to `DB_GATED_TREES`; `scripts/ci/db-gated-packages.sh --trees` lists it.
- [ ] Commit: `test/pipelinehop: a delivery on one node, the run driven on another` with `Refs #5491`.

---

### Task 12: Docs, the record, verification, the PR

- [ ] `docs/public/operate/pipelines.md` (front matter per `docs/DOCS_STANDARD.md`): what a pipeline is; connecting (`pipelinesConnect`); the `pipeline:` block reference (every key, its values, its refusal); events and modes table; the run key; fork refusal; secrets allowlist; check runs (name, details link, the 403 upgrade path); delivery webhook vs poll (a local cluster polls); compute; what runs today (no runner until the substrate) -- stated plainly. `GLOSSARY.md` entry. Read the memory notes on new docs reddening gates first.
- [ ] Record amendment: D4's run key, D5's release row, section 2's two corrections (installations are not rows; the workbench receives a tarball), decisions 5-15 above in a short "Implementation notes (epic 2)" section.
- [ ] Regenerate once: `make arch-model`, `make platform-graph`, `make sdk-gen-check`, `make env-registry-check`, `make frontdoor-paths-check` (unchanged), concept snapshot.
- [ ] Verify: `make test`; the db-gated lane on a fresh CI-image database; `go test -tags agent` / `-tags bff` for touched tagged files; the module-boundaries loop with `go mod tidy -diff`; `gitleaks dir . --no-banner`; `make os-typecheck os-test os-build`.
- [ ] Delete this plan in the last commit before the merge.
- [ ] PR against `main` with one `Closes #n` line per issue; arm CI; merge per the root CLAUDE.md.
