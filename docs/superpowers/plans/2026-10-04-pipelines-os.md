# Pipelines in MemQL OS -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Epic memql#5479 end to end in ONE pull request: the Runs tab, the run page as a rail of stops across the top, Checks on the source page and the Overview map, connect-a-pipeline as the rail-as-form page over the source, and the owner's optional Pipelines readiness item under the gear -- with the parts seeded and pinned.

**Architecture:** Pipelines stay a fact about a Deployables source (D12): no new app. The OS reads the seam's owner-scoped rows (`v1:pipelines:pipeline`, `v1:pipelines:run`, `v1:work:step`, the Library index) through feeds retained at the Deployables app root, and every lifecycle act goes through a builtin gated by a seeded `app:deployables/<part>`. Four engine additions carry what the rows cannot: a read-only connect preview, a re-run of failed steps, the readiness report's three settings as a lane, and a read of the GitHub App's installations whose permissions lag the manifest.

**Tech Stack:** Go 1.26 (workspace), MemQL DSL edition 2026, MemQL OS (React + TypeScript, vitest + jsdom), generated TS/Go SDKs (`make sdk-gen`).

**Spec:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md` (D12-D15, section 4 epic 4, section 6 testing, section 9 implementation notes) and the six task issues #5498-#5503, plus the documentation-program amendments on #5499/#5500 (the docs stage reads like any other; its artifacts list on the run page). OS rules: `clients/os/DESIGN.md`, `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, `clients/os/README.md`.

## Global Constraints

- One PR, branch `epic/pipelines-os`, closes #5479 #5498 #5499 #5500 #5501 #5502 #5503, each on its own `Closes #n` line.
- Stage by explicit path; never `git add -A` (shared tree, peer session on epic/pipelines-substrate).
- No new HTTP endpoint. The Library content route (`GET /_memql/artifacts/{id}/content`, single Range) already exists and is the only HTTP the OS adds a caller of.
- The OS never composes canonical ids; compare ids by their bare tail (`idTail`/`sameRow` in `apps/nexus/rows.ts`, already precedent).
- Every state word comes from one module (`apps/deployables/pipelines/words.ts`); no raw `pipeline_*` code is ever shown as text -- refusals render through `ProblemNotice` with `packages/refusals.ts` copy.
- Acts follow the state in one `ActionBar` per page, at most three, primary last, ONE button; an act that is not legal is ABSENT.
- `driverHeartbeatAt` (and any `*At` liveness field) never enters an arrival-cue fingerprint; the Runs list rings only on a terminal state.
- Loading is the shape of the content (`RecordListSkeleton`, `ContentSkeleton`, `InlineSkeleton`); never "Loading" copy.
- No emojis. Sentence case. Plain verbs. A refusal names its remedy.
- Pre-release: no compatibility shims for Go seams; the DSL keeps loading every form it loads today.

## Review Focus

1. **A run whose steps exist before they run.** `queue()` writes every step `pending` first; the run page must draw `pending` steps of a stage nobody reached as waiting, not as a failure, and a stage table on the run row that lags the step rows must lose to the step rows (D13 reads stops from the step rows).
2. **The steps feed folding another run's steps.** Work-step events are scoped by concept only; two runs of one owner at once fold into each other's list unless `inScope` keeps `runId` equal (Task 7 test).
3. **A re-run while the key's newest attempt is still running.** The engine refuses (`ErrRunInProgress`); the run page must not offer Re-run or Re-run failed on any attempt while a newer attempt of the same run key is unfinished (Task 7 test).
4. **A failed step whose log never reached the Library** (Library refused: quota, no storage). `logFileId` empty plus a `pipeline_artifact_missing` note: the page shows the step's message and the note, never an empty log box or a broken "Open the full log" (Task 7 test).
5. **A viewer who is not the pipeline's owner** (a cluster owner opening a colleague's source, an account member). Owner-scoped reads answer nothing; the Checks section must say nothing is connected *for them* only when it was asked of their own rows -- it renders the feed's honest absence, and no connect act is offered to a non-owner (Task 8 test).

---

## File structure

**Engine**
- `dsl/rbac/seeds.memql`, `component/auth/rbac_model.go` -- the parts and section reads.
- `dsl/pipelines/builtins.memql` -- re-pointed capabilities; `pipelinesPreview`, `pipelinesInstallations`; `pipelinesRerun(failedOnly)`.
- `dsl/pipelines/concepts.memql`, `shapes.memql`, `mutations.memql` -- `run.rerunFailedOnly`.
- `component/pipelines/refusal.go` -- `pipeline_passed_earlier` (skip), `pipeline_nothing_to_rerun` (refusal).
- `component/packages/refusal.go` -- their literals (parity gate).
- `component/pipelinerun/preview.go` (new), `connect.go` (shared inspection), `rerun.go`, `open.go`, `driver.go` (carry pass), `installations.go` (new), `capabilities.go`, `store_dsl.go`, `types.go`, `ports.go`.
- `component/packages/githubapp/installation.go` -- `Installations(ctx)`.
- `component/memql/readiness/integration.go`, `component/memql/readiness_eval.go` -- an integration report's settings ride the row as one lane.

**OS** (`clients/os/src/apps/deployables/pipelines/`, new)
- `rows.ts` -- projections: `PipelineRow`, `RunRow`, `StageRow`, `StepRow`, `RunFileRow`; fingerprints.
- `words.ts` -- every pipelines word: outcome, trigger, mode, stage/step status, where, duration, day labels, delivery, compute.
- `feeds.ts` -- `usePipelineRuns()`, `usePipelines()` (root-retained), `useRunSteps(workRunId)`, `useRunFiles(workRunId)`.
- `runs.ts` -- pure: filter, group by day, newest attempt per key, last run per branch, latest-upstream reading.
- `stops.ts` -- pure: a run's stages as stops (standing rules) and the open stop.
- `acts.ts` -- pure: the run page bar and the source page bar additions.
- `github.ts` -- pure: Open-on-GitHub URLs.
- `logTail.ts` -- the failed step's last lines through the Library content route.
- `calls.ts` -- rerun, cancel, connect, disconnect, preview, installations; error -> problem parsing.
- `openRun.ts` -- `?pipelineRun=` capture and hand-off.
- `RunsSection.tsx`, `RunPage.tsx`, `StopsAcross.tsx`, `ChecksPart.tsx`, `PipelinePage.tsx`, `connect/useConnectFlow.ts`, `connect/flow.ts`, `connect/ConnectPage.tsx`.
- `pipelines.css` -- one stylesheet, sections per surface.
- Modified: `DeployablesApp.tsx` (feeds, Runs pane, intents), `DeployablesSection.tsx` (views `run`, `pipeline`, `connect`; source-open requests), `page/SourceView.tsx` (Pipeline fact, Latest upstream, Checks part, bar), `list.ts`/`DeployablesSection.tsx` SourceLine ("pipeline" where the app count goes), `map/layout.ts` + `map/DeployMap.tsx` + `map/MapSection.tsx` (Checks node), `settings.ts` (Runs section), `parts.ts` (four parts), `packages/refusals.ts` (two codes).
- Settings: `apps/settings/PipelinesSection.tsx` (new), `apps/settings/pipelinesReadiness.ts` (new, pure), `apps/registry.tsx` (section), `SettingsApp.tsx` (route).
- Shell: `chrome/OptionalReadiness.tsx` (new) -- publishes the gear marker for optional, dismissable modules.

---

## Design (frontend-design pass, inside the OS system)

The palette, type and controls are the OS's own tokens and kit; nothing here invents a colour, a face or a field size. The one place the epic spends boldness is the run page's **stops across the top**: the same marks the vertical rail draws (check, held ring, pulse, cross, dash), laid on one horizontal thread with the stage name and its time under each mark, so a run reads left to right like the order the pipeline enforces. Everything around it is quiet: record rows, facts, one bar.

Words: "pipeline" is the configured thing, "checks" is what it reports, "run" is one attempt. Acts keep their names through a flow: **Connect pipeline** opens "Connect pipeline" and a refusal says the pipeline "was not connected".

### Runs tab

```
 Runs                                                   [Refine]
 ----------------------------------------------------------------
 Today
 (x) Add pipelines OS surfaces            Failed  at tests, 7m 40s   13:05
     memql   epic/pipelines-os   a1b2c3d   Pull request #5810
 (v) Fix shard balance                    Passed  4 stages, 4m 12s   12:41
     memql   main   3f9c2ab   Push
 Yesterday
 (o) Bump toolchain                       Running tests             --
 ...
                                        Show older runs
```
- Head: title, `meta` = count of the filtered list once the read settles; Refine (search: commit message or SHA; facets Source, Branch, Outcome as removable chips).
- One `RecordRow` per run: icon = the run's mark; name = commit message's first line ("Commit a1b2c3d" when GitHub gave none); secondary = source, branch, short SHA, trigger, as separate spans; state = one outcome word (Passed, Failed, Running, Queued, Cancelled, Refused) with tone; `stateExtra` = where and how long ("at tests, 7m 40s", "4 stages, notify skipped, 9m 12s", "pull request from a fork"); trailing = time of day.
- Grouped by day under quiet subheads: Today, Yesterday, then "Thursday, October 1" within a week, "September 28" within the year, "September 28, 2025" otherwise.
- `LiveList` per day group keyed on the filter, fingerprint = status + conclusion + stage statuses (never heartbeats); arrival cue fires only when a run REACHES a terminal state (a new queued row is not news; its conclusion is).
- Empty after a successful empty read: "No runs yet" + "A run starts with the next push or pull request to a source with a connected pipeline." Empty under a filter: "No runs match" + chips stay to remove.

### Run page (layout A, replaces the list -- rule 11)

```
 <- | Runs > memql > main
 Add pipelines OS surfaces
 Pull request #5810   a1b2c3d   Affected   7m 40s   Attempt 2
   (v)----------(x)----------(-)----------(-)
  checks       tests        deploy       notify
   52s         7m 40s       Not run      Not run
 ----------------------------------------------------------------
 tests                                         8 passed, 1 failed
 (v) go-tests 1 of 4        Cluster              2m 10s
 (x) db-tests 3 of 4        Cluster              7m 40s
     | --- FAIL: TestRunLease (0.31s)
     |     lease_test.go:88: lease not renewed
     | FAIL
     Open the full log      Saved to your Library
 (-) os-checks              No change under bucket os.
 ----------------------------------------------------------------
 Artifacts
     dist/coverage.html     tests.go-tests 1 of 4      12 KB
 ================================================================
 Failed at tests, 7m 40s        Open on GitHub   Re-run   [Re-run failed]
```
- Trail: Runs > source > branch when opened from Runs; Sources > source > branch when opened from the source page; back follows the origin.
- Meta line: trigger (a link to the pull request when there is one), short SHA (link to the commit), mode (Affected / Full suite), duration, attempt ("Attempt 2, failed steps of attempt 1" for a failed-only re-run).
- Stops: one per stage of the run's plan, from the step rows (`stops.ts`). The open stop follows the run: a running run opens the stage it is at, a failed run opens the stage it stopped at, a passed run opens the last stage; a person's click overrides until the run moves.
- A refused run (fork, manifest, disconnected) has no plan: the stops are replaced by the refusal (`ProblemNotice`, the copy's remedy) and its scope chip.
- Step rows: mark, name ("go-tests 1 of 4" for a shard), where ("Cluster", "Fleet: studio-mac", nothing when no runner answered), duration; a failed step carries its message, its last lines (`<pre>`, 40 lines max, from the log file's tail), "Open the full log" (opens the file in Files) and the fact "Saved to your Library"; a skipped step carries its reason; notes render as `ProblemNotice tone="warn"` under the step.
- Artifacts: the steps' `artifactFileIds` as rows (display name with `__` read back as `/`), each opening in Files.
- Bar (`acts.ts`): queued/running -> Open on GitHub (text), Cancel (danger, asks first in the bar); completed with failed or cancelled steps -> Open on GitHub, Re-run (text), Re-run failed (primary); completed otherwise -> Open on GitHub, Re-run (primary); fork refusal or disconnected pipeline -> Open on GitHub alone; a newer attempt of the key unfinished -> no re-run act and the detail says "Attempt 3 is running".

### Source page

```
 Facts ...
   Tracking         main
   Pipeline         memql-package.yaml, 4 stages, webhook, cluster
   Deployed         3f9c2ab
   Latest upstream  a1b2c3d, checks passed, not yet deployed
 Checks                                                  All runs
   (v) main               Fix shard balance        Passed    12:41
   (x) epic/pipelines-os  Add pipelines OS ...     Failed    13:05
 Apps it produces                                               2
   ...
 ================================================================
 Tracked  2 apps, 2 live, checks on          Pipeline settings  [Review]
```
- Checks sits between the source's facts and "Apps it produces"; one row per branch, the branch's newest run, the default branch first then newest; "All runs" opens Runs refined to this source.
- Not connected: "Not connected." with the bar's **Connect pipeline**. Disconnected: "Disconnected. Its runs stay as history." with **Connect again**.
- Sources list: a source that produces no apps and has a pipeline reads "pipeline" where the app count goes.
- Overview map: a Checks node after the bundle for every site whose source has an active pipeline (one per source per domain group), labelled "Checks", sublabel the default branch's newest outcome; selecting it opens the source page.
- Pipeline settings page (`PipelinePage.tsx`): Facts (Check on GitHub "MemQL / <name>", Repository, Default branch, How changes arrive, Where steps run, Allowed secrets, Installation, Connected); bar "Checks on" -> Disconnect (text, asks first), Change (primary, the connect flow prefilled); "Disconnected" -> Connect again (primary).

### Connect pipeline (rail-as-form, kit `Wizard`)

```
 Connect pipeline                       | Repository
 Run acme/storefront's checks on this   |  acme/storefront at main
 cluster and report them on GitHub.     |  checks      build-vet
  (v) Repository  acme/storefront,      |  tests       go-tests (4 shards), db-tests,
                  4 stages              |              os-checks (needs docker)
  (o) Compute                           |  deploy      verify-rollout   push only
  ( ) Confirm                           |  notify      znas-instance    push only
 ================================================================
 Choose where steps run                                Cancel
```
- Repository: `pipelinesPreview(packageId)` on entry, a read that writes nothing; the stages appear before anything is confirmed. A refusal stops the step there (not declared, manifest invalid, already connected, grant unusable).
- Compute: Cluster (always offered); Cluster and your fleet (offered only when the owner's computer use is on AND one of their machines reports `pipelines=allowed`). A manifest with steps that name needs and no fleet available stops here with the remedy.
- Confirm: Facts (Check on GitHub, Stages, Where steps run, Secrets the steps read); one Field "How changes arrive" (Webhook / Poll every minute), defaulting to the preview's suggestion. The floor's one button is **Connect pipeline**.
- State lives above the section (`useConnectFlow`, held by `DeployablesApp`) so leaving and coming back keeps the answers; nothing is written until Connect.

### Settings > Pipelines

```
 Pipelines                                              Optional
 Pipelines run a repository's checks from its memql-package.yaml
 and report them on GitHub.
  (v) GitHub App      Registered as memql-znas
  (o) A repository    Connect a pipeline from a source's page.  Open Sources
  ( ) Compute         A runner is ready on this cluster.
 ! GitHub is asking acme to accept new permissions for this app.
   Until it does, check runs are refused.            Review on GitHub
 ================================================================
 Not set up  1 of 3                                       Not now
```
- Sub-steps read the readiness fold's `pipelines` lane (`githubApp`, `repository`, `runner`) -- the agent nodes' own facts; the GitHub App step reuses the existing app-manifest setup (`GithubAppMissing`) so permissions arrive pre-filled.
- Installations whose accepted permissions lag the manifest (`pipelinesInstallations`) each render a warn notice with "Review on GitHub" (GitHub's own `html_url`).
- **Not now** writes the viewer's attention receipt for `readiness:pipelines`; the marker clears and the section stays.
- The marker: `chrome/OptionalReadiness.tsx` publishes a runtime attention change on the Settings app's `pipelines` section while the module is optional, not configured and not dismissed, for a viewer holding `read app:settings/pipelines`; the dock's Settings icon draws it with the existing attention dot. It is never auto-acknowledged by visiting.

---

### Task 1: Seeds, parts and sections (#5498)

**Files:**
- Modify: `dsl/rbac/seeds.memql` (parts block ~655-734; settings sections ~529-617)
- Modify: `component/auth/rbac_model.go` (`appReadFloors`, `appPartGrants`)
- Modify: `dsl/pipelines/builtins.memql` (header prose; `@requiresCapability` of connect/disconnect/rerun/cancel)
- Modify: `component/memql/app_resource_os_parity_test.go` (pin the pipelines builtins to their parts)
- Modify: `clients/os/src/apps/deployables/settings.ts` (Runs section), `clients/os/src/apps/registry.tsx` (Settings Pipelines section), `clients/os/src/apps/deployables/parts.ts` (four parts)
- Modify tests: `clients/os/test/deployables/settings.test.ts`, `clients/os/test/settings/settingsContract.test.ts`, `clients/os/test/kit/readinessStates.test.tsx` (owner-only section target), navigation JSON regen
- Modify docs: `docs/public/operate/pipelines.md`, `docs/public/operate/auth/access-model.md`

**Interfaces:**
- Produces: resources `read app:deployables/runs` (owner, developer), `read app:settings/pipelines` (owner), `execute app:deployables/{connect,rerun,cancel,channels}` (owner, developer); `PartsHeld` gains `connect`, `rerun`, `cancel`, `channels`; Deployables section id `runs` (after `sources`); Settings section id `pipelines`.

- [ ] Seed rows in the existing one-line shape (`seed capability <name> { roleSlug: ... verb: ... resourceType: ... predefined: true }`), named per the header rule: `cap-<role>-read-app-deployables-runs`, `cap-owner-read-app-settings-pipelines`, `cap-<role>-execute-app-deployables-{connect,rerun,cancel,channels}`, each with an `@description` that names memql#5498.
- [ ] Mirror them in `rbac_model.go`; run `go test ./component/auth/ -run TestSeedMatchesCompiledMirror`.
- [ ] Re-point: `pipelinesConnect`/`pipelinesDisconnect` -> `("execute", "app:deployables/connect")`, `pipelinesRerun` -> `rerun`, `pipelinesCancel` -> `cancel`; update the header paragraph.
- [ ] Add the four builtins (and Task 3/4's `pipelinesPreview` -> connect, `pipelinesInstallations` -> `("read", "app:settings/pipelines")`) to `TestTheDeployablesPartsAreDeclaredOnTheirConstructs`'s want map; run it red, then green.
- [ ] OS: Runs section `{ id: "runs", name: "Runs", requires: "app:deployables/runs" }`; Settings `{ id: "pipelines", name: "Pipelines", requires: "app:settings/pipelines" }`; `DEPLOYABLE_PARTS` + `NO_PARTS`/`ALL_PARTS`.
- [ ] `component/memql/app_resource_os_parity_test.go` + `clients/os/test/system/rankLadder.test.tsx` green; `MEMQL_UPDATE_NAVIGATION=1 npm test -- test/ask/navigationContract.test.ts`; `make platform-graph`.
- [ ] Commit `Issue #5498: seed the pipelines parts and sections, and pin them`.

### Task 2: Re-run failed in the engine (#5500)

**Files:**
- Modify: `component/pipelines/refusal.go` (+ class, + `Codes()`), `component/packages/refusal.go` (literals)
- Modify: `dsl/pipelines/concepts.memql` (`run.rerunFailedOnly bool`), `shapes.memql`, `mutations.memql` (createPipelineRun accepts it), `builtins.memql` (`pipelinesRerun.failedOnly bool`)
- Modify: `component/pipelinerun/types.go` (`Run.RerunFailedOnly`), `store_dsl.go` (write/read), `open.go` (`rerunOf(original, deliveryID, failedOnly)`), `rerun.go`, `driver.go` (carry pass), `trigger.go` (callers pass false)
- Test: `component/pipelinerun/rerun_test.go`, `component/pipelinerun/driver_test.go`, `component/pipelines/summary_test.go`
- Modify: `clients/os/src/apps/deployables/packages/refusals.ts` (two copies; `pipeline_passed_earlier` in NOT_A_FAULT)
- `make concept-snapshot`, `make sdk-gen`

**Interfaces:**
- Produces: `pipelines.CodePassedEarlier = "pipeline_passed_earlier"` (ClassSkip), `pipelines.CodeNothingToRerun = "pipeline_nothing_to_rerun"` (ClassRefusal); `Integration.Rerun(ctx, runID string, failedOnly bool) (Run, error)`; builtin `pipelinesRerun(runId, failedOnly)` answering `{runId, attempt, rerunOf, status, failedOnly}`; run row field `rerunFailedOnly`.

Semantics (the carry pass, `driver.go`, fresh work only -- never on resume):
```go
// carryPassed marks a planned step skipped CodePassedEarlier when this
// attempt re-runs failed steps only and the attempt it re-runs PASSED that
// step with the same package slice. A shard whose slice moved since (the
// timing table changed) is re-run: a pass on other packages is not a pass.
func carryPassed(tracks []*stepTrack, prior []WorkStep, priorAttempt int) {
	byKey := map[string]WorkStep{}
	for _, row := range prior { byKey[row.Key] = row }
	for _, t := range tracks {
		row, ok := byKey[t.step.Key]
		if !ok || t.step.Skip != nil { continue }
		passed := row.Status == WorkStepDone
		carried := row.Status == WorkStepSkipped && row.Skip != nil && row.Skip.Code == pipelines.CodePassedEarlier
		if !(passed || carried) || !slices.Equal(row.Packages, t.step.Packages) { continue }
		reason := fmt.Sprintf("Passed in attempt %d.", priorAttempt)
		if carried { reason = row.Skip.Reason }
		t.step.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: reason}
	}
}
```
- `Rerun(failedOnly=true)` refuses `pipeline_nothing_to_rerun` unless the original concluded `failure` or `cancelled`, has a `workRunId`, and at least one of its steps is `failed` or `cancelled`. Fork and disconnected refusals stay first.
- [ ] Tests first (`rerun_test.go`): failedOnly recorded on the new row; nothing-failed refused; a refused (manifest) run refused for failedOnly but re-runnable whole. (`driver_test.go`): passed steps carried with "Passed in attempt 1.", the failed step and everything after it executed; a shard whose packages differ re-run; a second failed-only re-run keeps "Passed in attempt 1."; a whole re-run (failedOnly=false) carries nothing. Run red, implement, run green: `go test ./component/pipelinerun/ ./component/pipelines/ ./component/packages/`.
- [ ] Commit `Issue #5500: re-run only the steps that did not pass`.

### Task 3: Connect preview (#5502)

**Files:**
- Create: `component/pipelinerun/preview.go`; Modify: `connect.go` (shared `inspectSource`), `capabilities.go` (register `preview`), `dsl/pipelines/builtins.memql`
- Test: `component/pipelinerun/connect_test.go` (preview cases)

**Interfaces:**
- Produces builtin `pipelinesPreview(packageId)` (`@sdk`, `@requiresCapability("execute", "app:deployables/connect")`) answering ONE row:
```json
{ "repository": "acme/storefront", "defaultBranch": "main", "sha": "<40 hex>",
  "name": "storefront", "checkName": "MemQL / storefront",
  "stages": [ { "name": "tests", "on": ["push"], "channel": "",
                "steps": [ { "name": "os-checks", "needs": ["docker"], "secrets": [], "shards": 0, "bucket": "os" } ] } ],
  "needs": ["docker"], "secrets": ["VERIFY_TOKEN"], "suggestedDelivery": "webhook",
  "existing": { "pipelineId": "...", "status": "active", "delivery": "poll", "compute": "cluster", "secretNames": [] },
  "refusal": { "code": "pipeline_not_declared", "message": "...", "scope": "" } }
```
`existing` and `refusal` are null when absent. A refusal is DATA (the stop renders it), never an error; only an unreadable caller or an internal fault errors.
- `suggestedDelivery` is `webhook` when the cluster's GitHub App subscribes to events (its webhook is on: a publicly reachable domain, `githubconnect`'s own rule), else `poll`.
- [ ] Tests first: a declared block answers its stages in order with needs/secrets; no block -> `refusal.code == pipeline_not_declared` and no write; a repository another source runs -> `pipeline_already_connected`; nothing is written in any case (the fake store's write count stays 0). Implement by extracting connect's read+validate into `inspectSource(ctx, d, caller, packageID) (inspection, error)` used by both. `go test ./component/pipelinerun/`.
- [ ] `make sdk-gen`; commit `Issue #5502: preview a source's pipeline before connecting it`.

### Task 4: Readiness lane and installations (#5503)

**Files:**
- Modify: `component/memql/readiness/integration.go` (return the report's settings), `component/memql/readiness_eval.go` (integration arm writes one lane `report` with a slot per setting: `{name, present}`)
- Modify: `component/packages/githubapp/installation.go` (+ test): `Installations(ctx) ([]Installation, error)` via `GET /app/installations?per_page=100` (app JWT), paginated by `Link`.
- Create: `component/pipelinerun/installations.go` (+ test); Modify: `ports.go`, `capabilities.go`, `dsl/pipelines/builtins.memql`
- OS: `clients/os/src/system/readinessFold.ts` keeps lanes as today (verify the lane reaches `Verdict.lanes`).

**Interfaces:**
- Produces: on every `pipelines` moduleReadiness row (and every other integration-evaluated module's), `lanes: [{ name: "report", configurableFrom: "os", complete: <state configured>, slots: [{ name: "githubApp", present }, { name: "repository", present }, { name: "runner", present }] }]`.
- Produces builtin `pipelinesInstallations()` (`@sdk`, `@requiresCapability("read", "app:settings/pipelines")`) answering one row per installation of the cluster's app that has NOT accepted what the app requests: `{ installationId, account, accountType, htmlUrl, missingPermissions: ["checks:write"], suspended }`. No app configured -> zero rows. GitHub unreachable -> an error the section renders as "GitHub could not be asked".
- [ ] Tests first: integration evaluator lane carries the three slots with the report's presence (fixture through `IntegrationStatus`); email's existing verdict unchanged in state; githubapp `Installations` against `fakeHub` (two pages, a suspended one); `pipelinesInstallations` lists only lagging installations and compares `write` > `read` > none.
- [ ] Commit `Issue #5503: say which readiness facts hold, and which installations lag the app`.

### Task 5: The OS data layer and wiring (all issues)

**Files:** create `pipelines/rows.ts`, `words.ts`, `feeds.ts`, `runs.ts`, `stops.ts`, `acts.ts`, `github.ts`, `calls.ts`, `logTail.ts`, `openRun.ts`, `pipelines.css`; modify `DeployablesApp.tsx`, `DeployablesSection.tsx`, `main.tsx` (capture `?pipelineRun=`); tests under `clients/os/test/deployables/pipelines/`.

**Interfaces (TypeScript):**
```ts
export interface PipelineRow { id: string; packageId: string; name: string; repository: string; defaultBranch: string;
  installationId: string; delivery: "webhook" | "poll"; compute: "cluster" | "cluster_and_fleet"; status: "active" | "disconnected";
  secretNames: string[]; connectedAt: string; ownerUserId: string }
export interface StageRow { name: string; status: "waiting" | "running" | "passed" | "failed" | "cancelled" | "skipped" | "blocked"; durationMs: number; steps: number; failed: number }
export interface RunNote { code: string; message: string }
export interface RunRow { id: string; pipelineId: string; repository: string; sha: string; mode: "affected" | "full";
  event: "pull_request" | "merge_group" | "push" | "release"; runKey: string; attempt: number; trigger: "webhook" | "poll" | "rerun";
  rerunOf: string; rerunFailedOnly: boolean; pullRequest: number; headBranch: string; title: string; version: string;
  status: "queued" | "in_progress" | "completed"; conclusion: "" | "success" | "failure" | "cancelled" | "refused";
  refusalCode: string; refusalMessage: string; refusalScope: string; checkRunId: string; checkRunState: string;
  notes: RunNote[]; workRunId: string; cancelRequested: boolean; stages: StageRow[];
  queuedAt: string; startedAt: string; finishedAt: string; durationMs: number; ownerUserId: string }
export interface StepRow { id: string; runId: string; key: string; seq: number; stage: string; name: string; shard: { index: number; of: number } | null;
  status: "pending" | "ready" | "running" | "waiting" | "done" | "failed" | "skipped" | "cancelled"; refused: boolean;
  errorCode: string; errorMessage: string; reason: string; durationMs: number; startedAt: string; finishedAt: string;
  where: { surface: "cluster" | "fleet" | ""; nodeId: string; jobName: string; workerId: string; machine: string };
  logFileId: string; artifactFileIds: string[]; notes: RunNote[]; packages: string[] }
export interface RunFileRow { artifactId: string; fileId: string; name: string; sizeBytes: number; format: string }
export function runFromRow(row: Row): RunRow; export function pipelineFromRow(row: Row): PipelineRow; export function stepFromRow(row: Row): StepRow;
export function runFingerprint(run: RunRow): string; // status|conclusion|stage statuses; never heartbeats
export interface Outcome { word: string; detail: string; tone: "accent" | "warn" | "muted"; mark: "done" | "stopped" | "current" | "ahead" | "skipped" }
export function runOutcome(run: RunRow): Outcome;
export function triggerWords(run: RunRow): string;   // "Pull request #5810" | "Merge queue" | "Push" | "Release" | "Re-run of attempt 1"
export function modeWord(mode: RunRow["mode"]): string; // "Affected" | "Full suite"
export function durationWords(ms: number): string;   // "52s" | "7m 40s" | "1h 3m" | "" for 0
export function dayLabel(iso: string, now: Date): string;
export function whereWords(step: StepRow, machineName?: (workerId: string) => string): string; // "Cluster" | "Fleet: studio-mac" | ""
export function usePipelineRuns(): LiveCollectionHandle<Row>;   // key `deployables:pipelineRuns:<viewer>`
export function usePipelines(): LiveCollectionHandle<Row>;      // key `deployables:pipelines:<viewer>`
export function useRunSteps(workRunId: string): { steps: StepRow[]; state: string; error: string };
export function useRunFiles(workRunId: string): { byFileId: Map<string, RunFileRow>; state: string };
export function fetchLogTail(artifactId: string, bearer: () => Promise<string | null>, baseUrl: string): Promise<string[]>; // last 40 lines of the last 16 KiB
```
- [ ] Pure tests first (`runs.test.ts`, `words.test.ts`, `stops.test.ts`, `acts.test.ts`, `github.test.ts`): every outcome word; day grouping across midnight and years; newest attempt per key; last run per branch with the default branch first; stops from step rows when the stage table lags; refused run -> no stops; acts per state (the five bar readings above, the newer-attempt rule, parts absent -> act absent).
- [ ] Wiring: feeds retained in `DeployablesApp` (added to `reseedAll` and `feedError`), the Runs pane (`RetainedSection active={sectionId === "runs"}`, and `"runs"` added to the map pane's exclusion list), `intent.payload.runId` -> run page in Runs, `?pipelineRun=` captured at boot and handed out once (pattern: `sources/connectReturn.ts`), views `run`/`pipeline`/`connect` in `DeployablesSection`.
- [ ] Commit `Issue #5499: the OS's pipelines rows, words and feeds`.

### Task 6: The Runs tab (#5499)

**Files:** `pipelines/RunsSection.tsx`, `pipelines/pipelines.css` (Runs block); test `clients/os/test/deployables/pipelines/runsSection.test.tsx`.
- [ ] Tests first through `DeployablesApp` with `fakeConnection` (arm `query pipelineRunsForOwner()`): grouped by day; one row reads message, source, branch, SHA, trigger, outcome, time; Refine by source/branch/outcome narrows and chips remove; a queued run arriving does not ring, its completion does; a heartbeat-only update does not ring; empty state only after a live empty read; clicking a row opens the run page with the trail `Runs > source > branch`; "Show older runs" pages (`nextCursor`).
- [ ] Implement per the Design section; `npx vitest run test/deployables/pipelines`; commit.

### Task 7: The run page (#5500)

**Files:** `pipelines/RunPage.tsx`, `pipelines/StopsAcross.tsx`, css block; tests `runPage.test.tsx`.
- [ ] Tests first: stops in plan order with marks; the open stop rules; a failed step shows its last lines (fake content route answers a 206 tail) with Open the full log and Saved to your Library; empty `logFileId` -> no log box and the note renders; skipped step reason; artifacts listed; refused fork -> refusal copy, no stops, only Open on GitHub; the bar per state; Cancel asks first in the bar; Re-run failed calls `builtin pipelinesRerun(runId: ..., failedOnly: true)` and opens the new attempt when its row arrives; a newer unfinished attempt hides re-run; another run's step events do not fold in (`inScope`); `?pipelineRun=<id>` opens the page.
- [ ] Implement; commit.

### Task 8: The source page, the list row, the map, pipeline settings (#5501)

**Files:** `pipelines/ChecksPart.tsx`, `pipelines/PipelinePage.tsx`, `page/SourceView.tsx`, `DeployablesSection.tsx` (SourceLine), `map/layout.ts`, `map/DeployMap.tsx`, `map/MapSection.tsx`, `DeployablesApp.tsx`; tests `sourceChecks.test.tsx`, `pipelinePage.test.tsx`, `test/deployables/layout.test.ts` additions.
- [ ] Tests first: Pipeline fact text for active / disconnected / none; Latest upstream reads "checks passed, not yet deployed" only when the latest upstream SHA's newest run passed and differs from Deployed; Checks rows one per branch, default first; bar detail gains "checks on"; Pipeline settings / Connect pipeline / Connect again present only for the owner holding `connect`; a source with zero apps and a pipeline reads "pipeline" in the Sources list; the map draws one Checks node per source per group after the bundle and selecting it opens the source; PipelinePage disconnect asks first and calls `builtin pipelinesDisconnect(pipelineId: ...)`.
- [ ] Implement; commit.

### Task 9: Connect pipeline (#5502)

**Files:** `pipelines/connect/flow.ts`, `useConnectFlow.ts`, `ConnectPage.tsx`; tests `connectFlow.test.ts`, `connectPage.test.tsx`.
- [ ] Tests first: the preview is asked on entry and the stages show in the Repository step before anything is confirmed; every preview refusal stops Repository with its copy; Compute offers fleet only with computer use on and a machine labelled `pipelines=allowed`; needs without fleet stop Compute with the remedy; Confirm's delivery defaults to the suggestion; Connect calls `builtin pipelinesConnect(packageId, delivery, compute, secretNames)` with the manifest's secret names; a connect refusal lands at its stop; Leave keeps the draft (state above the section) and the source page reopens it where it was; reconnect ("Change") pre-fills from the existing pipeline.
- [ ] Implement; commit.

### Task 10: Settings > Pipelines and the gear marker (#5503)

**Files:** `apps/settings/PipelinesSection.tsx`, `apps/settings/pipelinesReadiness.ts`, `apps/settings/SettingsApp.tsx`, `chrome/OptionalReadiness.tsx`, `chrome/Shell.tsx` (mount), `system/modules.ts` (`pipelines` target); tests `test/settings/pipelinesSection.test.tsx`, `test/system/optionalReadiness.test.tsx`.
- [ ] Tests first: three sub-steps from the `report` lane (done/open), the GitHub App step offers the manifest setup to a holder who may set it up; lagging installations render one notice each with Review on GitHub; Not now writes `acknowledgeAttention(changeId: "readiness:pipelines", revision: "optional-1")` and the marker clears while the section stays; the dock's Settings icon shows the attention dot for an owner while pipelines is optional and unconfigured and not dismissed, and not for a developer (no `read app:settings/pipelines`), and not once configured; visiting the section does NOT acknowledge it.
- [ ] Implement; commit.

### Task 11: Docs, generated artifacts, verification, visual QA

- [ ] `docs/public/operate/pipelines.md`: the OS surfaces (Runs, run page, Checks, Connect pipeline, Settings item), the re-pointed parts, Re-run failed and its skip code, the preview and installations builtins; "What runs today" table updated; `docs/public/operate/auth/access-model.md` parts list; `clients/os/README.md` Deployables pipelines paragraph.
- [ ] Gates: `make sdk-gen-check`, `make concept-snapshot` check, `make arch-model`, `make platform-graph-check`, `make frontdoor-paths-check` (no new paths), `go build ./...` (+ `go vet` touched packages), `make test`, the db-gated pipelinerun tree against a real Postgres, `cd clients/os && npm run typecheck && npx vitest run && npm run build`.
- [ ] Visual QA: a throwaway `clients/os/qa/` harness (resolveId swap with `enforce: "pre"`, seeded ladder, `?at=`/open-intent hooks), screenshots of Runs (empty, populated), run page (running, failed with log, refused fork), source page with Checks, connect rail (each stop, a refusal), Settings > Pipelines (unset, partial, dismissed), at 1400x900, 1400x760 and 760x900, light and dark. Delete the harness before committing.
- [ ] Delete this plan in the epic's last commit.
