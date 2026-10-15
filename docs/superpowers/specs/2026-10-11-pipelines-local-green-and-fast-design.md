# Pipelines, local green and fast -- the engine's own pipeline passes on a local cluster, then a pull-request run takes no longer than GitHub Actions

- **Date:** 2026-10-11
- **Status:** agreed with the owner in the 2026-10-11 brainstorm. Every fork
  below was put to the owner and answered (the phase's goal, the speed target,
  how runs are triggered, what green covers, the capacity budget, the deep-scan
  cadence, where the required check will run, the approach); the six design
  sections were presented and each approved.
- **October 15 owner update:** phase 2 requires one passing full run on the
  epic branch instead of three. The full post-merge run on main remains.
- **What it is:** the next phase of the pipelines program
  ([2026-09-16-pipelines-program-design.md](2026-09-16-pipelines-program-design.md),
  epic #5480), narrowed to continuous integration. The engine repository's own
  `memql-package.yaml` passes every stage on a local MemQL cluster on the
  owner's amd64 workstation; it then reports a non-required `MemQL / memql`
  check on every pull request and main push; the four GitHub Actions scan
  workflows are retired; and a pull-request run is brought to the GitHub
  Actions baseline or faster.
- **Why now:** the program's code is largely built (seam, substrate, OS,
  delivery hardening) but nothing reports. One local rehearsal on 2026-10-06
  posted real check runs; the passing ones used the Cockpit-placed package, the
  Workbench package that replaced it the same day has run once and failed, and
  no MemQL check has appeared on any commit since. Speed was the program's
  first reason (section 1 of the program record), and the runs that passed took
  1 to 2 hours against about 11 minutes on Actions.
- **Supersedes:** where it conflicts, the Workbench-first implementation
  sequence in `docs/internal/planning/pipelines-self-hosting.md` for CI. Release
  candidates and installation updates in that plan are parked, not cancelled.
- **Repository:** `memql` only. Cockpit, the project template and the instance
  repository are untouched in this phase.

## 1. Where it stands (measured 2026-10-11)

**What exists.** CI is DSL plus native Go, as the boundary record
(`docs/public/build/integration-boundary.md`) prescribes:

- DSL, `dsl/pipelines/`: the `pipeline`, `run` and `channel` concepts
  (`@serverWritten`); three trigger automations (a GitHub delivery, the
  every-minute poll, the daily scan); the sealed default workflow
  `runPipelineStages` (stage order, `parallel(16)` steps, blocking after a
  failure) and the pure policy templates it pins (mode for an event, stage
  inclusion, full versus affected selection, buckets, notification copy); 13
  builtins and 2 actions, all `integration.pipelines.*`.
- Go: `component/pipelines` (the stdlib-only compiler, affected-set selection,
  sharding, run keys), `component/pipelinerun` (the `pipelines` plug-in and the
  run driver on agent nodes), `integrations/pipelinesteps` (the agent executor
  and the workbench's Kubernetes Job runner), with `component/pipelinenotify`,
  `component/identity/githubapp` (check runs, SARIF), `component/inbound` and
  the workbench forward actions beside them.
- OS: Deployables (Runs, the run page, Checks, the connect rail) and
  Settings > Pipelines.

**What ran.** On 2026-10-06 a local k3d installation, connected through the
rehearsal GitHub App `memql-znas-delivery-rehearsal` (polling), reported
`MemQL / memql` on main pushes and pull-request heads. The passing runs all
used the earlier seven-step package whose steps ran on the Cockpit fleet:

| Commit | Result |
|---|---|
| `e26d5e6760` | passed, 2 stages in 1h 05m 24s (the first complete cycle) |
| `5521e78404` | passed, 2 stages in 1h 07m 48s |
| `c4612216ee` (head of PR #5859) | passed, 3 stages in 1h 54m 08s |

The same day the owner moved portable CI to Workbench Kubernetes Jobs, and the
package was rewritten for them. Its one run, at `774d846608`, failed: checks 3
passed and 10 failed, and the 22 test steps never ran. Eight of the ten failures
were `pipeline_execution_uncertain` (a step's previous attempt had left no Job
and no result, so recovery refused to start another), one was a clone failing
with a GnuTLS receive error, and one was a secret scan reaching its 30-minute
deadline. Fixes aimed at those causes landed afterwards (#5879, #5875, #5885);
none has been re-run. No MemQL check has appeared on any commit since.

**Why the passing runs were slow.** On the fleet one build slot ran the 11 test
steps one after another (the tests stage took 58m 33s). On the Workbench path
the local overlay allows one Job at a time (`count/jobs.batch: "1"`), each
container capped at 2 CPUs and 4 GiB. Every step also shallow-fetches its commit
from GitHub, downloads the Tailwind CLI (22 step definitions run
`scripts/identity/build-css.sh`), and compiles with `GOFLAGS=-p=1
GOMEMLIMIT=512MiB GOGC=50`, settings sized for Cockpit's 2 GiB containers. The
Go build and module caches do persist on the cache volume.

**What GitHub Actions takes.** The last 40 green pull-request `ci.yml` runs,
timed from creation to completion and not filtered to Go changes, have a median
of 11.4 minutes (middle half 9.4 to 13.0); 3 of 40 finished in 7 minutes or
less. Every job runs in parallel, so the critical path is the slowest job: in a
recent Go-touching run `go-checks` took 8m 09s, `os-checks` 8m 30s and
`db-tests (memql-1)` 7m 12s. Of the scan workflows, a pull request's
`gitleaks dir .` takes about a minute; a main push runs CodeQL (29 to 35
minutes), govulncheck (about a minute), SBOM (about a minute, only when
`go.mod` or `go.sum` change) and a full-history gitleaks scan that ran 59 to 139
minutes and failed on the recent pushes.

**What blocks a run today.**

- The package pins `platform: linux/arm64`, which becomes a
  `kubernetes.io/arch` node selector (`integrations/pipelinesteps/execution.go`).
  The workstation and AKS are amd64; the only amd64 toolchain image is 1.0.0
  (built 2026-10-04), the newest is 1.1.0-arm64 (2026-10-06).
- The default run ceiling is 120 minutes (`MEMQL_PIPELINES_RUN_MAX_MINUTES`);
  the package declares a 6-hour step (`secrets-history`) and a 3-hour one
  (`codeql-go`). Only `components/examples` composes the 8-hour
  `components/pipelines-long-runs` profile.
- `make up` cannot create the dedicated build node the runner's
  `MEMQL_PIPELINES_NODE_POOL` setting selects.
- No local cluster exists on the workstation, and the 2026-10-06 one ran on an
  ARM64 machine.

**What a full run costs.** The full-history scan measured 3h 53m on 4 CPUs and
6 GiB (2026-10-07, 10,137 commits, clean); CodeQL Go took over two hours on two
ARM64 CPUs, 41m 49s of it SARIF export. Main received between 4 and 48 pushes a
day from 2026-10-01 to 2026-10-10. Running the deep scans on every push would
need about 24 CPU-hours per push, several hundred a day, against 384 in the
budget below.

**What GitHub provides without Actions.** Secret scanning with push protection,
Dependabot alerts and Dependabot security updates are enabled on the
repository. CodeQL's default setup is not configured, and would run on Actions
if it were.

## 2. Decisions

### D1 -- This phase is CI only, and local

Get the current Workbench package green on a local cluster on the owner's amd64
workstation, then make pull-request runs fast. Release candidates and
installation updates (the CD half of `pipelines-self-hosting.md`) are parked
until this phase closes. Cockpit stays disconnected; the portable cycle is
proven without it.

### D2 -- Fast means the GitHub Actions baseline or better

The target is a pull-request run at or under the Actions median for the same
work: 11.4 minutes on today's sample, re-measured properly as the M0 baseline
(#5697) before tuning starts. T is that baseline's median for Go-touching pull
requests on section 7's clock, from the `ci.yml` run's creation to
`ci-required`'s completion; #5697's own first-job clock is recorded beside it.
Both systems are timed on the same clock (section 7).

### D3 -- Manual runs until green, then shadow checks

Phase 2 starts every run from **Run latest** on a disconnected pipeline. A
manual run may name a branch of the same repository (section 8, engine change
4), so the epic's own branch is run and fixed before it merges. A manual run
writes its check run on the commit it tests, so during phase 2 `MemQL / memql`
appears only on the epic's own commits. Once green, the pipeline reconnects
with `poll` delivery and reports a non-required `MemQL / memql` check on every
pull request and main push. No ruleset change is made in this phase.

### D4 -- Green covers everything in the package

Every stage passes, the deep scans included. Full runs therefore use the 8-hour
run profile.

### D5 -- The pipeline may use 16 CPUs and 40 GB of the workstation

Enforced at the build node (section 4), leaving 8 CPUs and about 22 GB for the
serving cluster and the owner's other work.

### D6 -- Deep scans run daily and on releases; Actions' scan workflows retire

GitHub's push protection and Dependabot cover push time. The quick scans
(govulncheck, npm audit, `gitleaks dir .`) run on every pull request and push.
The deep scans (full-history gitleaks, CodeQL for three languages, SBOM) run on
the daily schedule, on releases and on manual runs, with CodeQL's SARIF uploaded
to code scanning. `codeql.yml`, `gitleaks.yml`, `govulncheck.yml` and `sbom.yml`
are deleted once the MemQL scans have proved themselves (section 6). Per-push
deep scanning is not reinstated in this phase. A deep finding on main surfaces
within a day instead of within the hour; that includes a secret added and
removed inside one pull request that push protection does not recognise.

### D7 -- The required check will run on the ZNAS cloud instance, in the next phase

A required check gates every merge, so it must not depend on a workstation that
sleeps. Moving the pipeline to the cloud instance (with AKS network-policy
enforcement and an amd64 build pool, both owner approvals), making
`MemQL / memql` required and deleting `ci.yml` are the next phase. There the
pipeline runs under a released engine, never the tree under test (program
record D16).

### D8 -- Tune the runner and reshape the package; four small engine changes

Chosen over teaching the default workflow dependency scheduling (more general,
but it touches the compiler, recovery fingerprints, the check summary and the OS
run page before a first green run) and over collapsing a run into a few large
steps (it breaks the per-lane mapping to `ci.yml`, coarsens failures and does
not spread across nodes). The engine changes are a git mirror on the build node,
SARIF publication, a `trees:` step filter, and a branch argument for manual runs
(section 8). Stopping later work
after a failed build is given up; GitHub Actions already behaves that way.

### D9 -- The package targets linux/amd64

The manifest's `platform` becomes `linux/amd64` with an amd64 toolchain pinned
by digest. An ARM64 cluster cannot run the engine's package until a manifest
can name more than one platform; that is recorded as a follow-up, not built
here.

### D10 -- The local cluster runs main's engine

This phase's engine changes have to run where they are tested, so the local
cluster is built from the working tree with `make dev`. A bad engine change can
break local CI; that is accepted for a shadow check and is why D7 puts the
required check on a released engine.

## 3. Phases and exit criteria

| Phase | What happens | Done when |
|---|---|---|
| 0. Baseline | #5697 as specified: a week of Go-touching pull requests on Actions, per lane, recorded in `docs/internal/ops/ci-design.md` D6 | The target number T is recorded |
| 1. Bring-up | The local cluster with a build node, the amd64 toolchain, the 8-hour profile, the Job count, this cluster's GitHub App and the engine repository as a source (section 4) | A **Run latest** run reaches a terminal state with at least one step executed on the build node |
| 2. Green | Manual full runs of the epic branch's head; every failure fixed at its cause (section 5) | One full run passes every stage on the epic branch, with no engine rollout during it; after the merge, one more on main's head |
| 3. Shadow and scans | Reconnect with polling; the scan cadence changes; SARIF upload; the four scan workflows retire (section 6) | Seven consecutive scheduled deep-scan runs pass with their CodeQL results visible in code scanning, and the four workflows are deleted |
| 4. Fast | Measured tuning on shadow pull-request runs (section 7) | Over the last 20 Go-touching pull-request heads both systems completed, MemQL's median is at most T, and there is no false green (section 7) |

Phases 0 and 1 may run together. Phase 3's shadow checks start when phase 2
exits; its scan retirement and phase 4 then run side by side.

## 4. Phase 1: the local cluster and bring-up

**Topology.** `make up` gains a build-node option that adds one k3d agent
labelled `memql.io/pipeline-pool=builds` and tainted
`memql.io/pipeline-pool=builds:NoSchedule` (k3d 5.9's `--k3s-node-label`,
`--k3s-arg --node-taint` and `--agents-memory`). Serving workloads never land
on it; only pipeline Jobs and isolation probes do. `make scale N=2` gives two
replicas of each node type, so run claims and step forwards cross replicas. The
local cache volume (local-path, ReadWriteOnce) binds to the build node on first
use.

**The budget is enforced at the node.** The build node's container is capped at
40 GB through `--agents-memory` (which also sets what the kubelet sees) and at 16
CPUs through `docker update --cpus` on the cgroup-v2 host, reserving CPU and
memory capacity for serving workloads and the rest of the workstation. These
caps do not isolate the physical disk shared by all k3d nodes. `make up`
derives both caps from the Docker host's capacity, leaving roughly a third of
the host's CPUs and memory outside the build node (16 CPUs and 40 GB on this
workstation, less on a smaller machine), and takes explicit values when given
them. Inside the node the Job count
(raised from 1 to 6 to start) and the per-step limits decide how many steps run
at once; phase 4 tunes both from measured peaks. The October 15 cold-cache
rehearsal in #6018 exposed disk stalls and control-plane restarts with six
Jobs, so the local ceiling was lowered to two during phase 2. Any later
increase needs cold-cache storage and serving-health evidence alongside CPU
and memory measurements.

**Local overlay values.** `MEMQL_PIPELINES_NODE_POOL=builds` on every workbench
replica, `components/pipelines-long-runs` composed, and the Job count. These are
values only; no engine code branches on the deploy target. Because the overlay
selects the pool, every developer's `make up` creates the build node (an idle
agent is cheap), and its size follows that machine's Docker host.

**The amd64 toolchain.** Before phase 1's PR, one owner-approved dispatch of
`build-toolchain-image.yml` from main builds today's `deploy/toolchain-image`
Dockerfile for `linux/amd64` under a new immutable version on a native amd64
runner, with Go taken from `go.work` (1.27.2). The workflow builds only from
main, so a Dockerfile change and its digest pin cannot share a PR; the Tailwind
change (#5816) is therefore made without a toolchain rebuild (section 10). The package
pins the new digest, and `memql_package_manifest_test.go`, which asserts
linux/arm64 today, moves with it. The database and bash images the package pins
already serve `ci.yml`'s amd64 runners. Image builds stay on Actions until M4.

**GitHub.** This cluster gets its own GitHub App through Settings > Pipelines,
created from the app manifest with one owner click in the browser. It is
installed on `znasllc-io/memql` with contents read, checks write, pull requests
read and code scanning alerts write (for D6). GitHub cannot reach a local
cluster, so delivery is `poll` and the app has no webhook. The engine
repository becomes a source in Deployables through that connection. The pipeline
is connected once (connect reads and validates the manifest) and then
disconnected, so phase 2's runs start only from **Run latest**.

**Nothing merges before the first run.** A run reads `memql-package.yaml` at
the commit it tests. A manual run names the epic branch, whose head carries the
amd64 manifest and the overlay values, and `make up` points Argo CD at the
current branch, so the cluster runs the epic's own overlay.

## 5. Phase 2: green

**Runs.** Each is **Run latest** naming the epic branch: a full run of its
head. With today's stage
order (checks, vulnerabilities, secrets-current, secrets-history, tests, gates,
analysis) the full-history scan (3h 53m measured) and CodeQL (up to two hours)
sit inside the sequence, so a run should take about 7 hours, inside the 8-hour
profile but close to it. If a run is cut off by the ceiling, the local overlay
raises it (the engine accepts up to 24 hours) rather than a stage being dropped.
Phase 4's reshape runs the deep scans beside everything else, bringing a full
run down to about the history scan's length.

**The loop.** A failure is debugged to its root cause, reproduced by a failing
test, fixed, rolled into the local cluster with `make dev`, and run again.
Engine fixes are verified locally from the epic's branch before they merge, so
they do not wait on the merge queue one by one. A failure that does not
reproduce is unexplained, not fixed, and stays open until its cause is found; a
passing re-run does not clear it.

**Operating rules.**

- The engine is never rolled while a run is in flight. An interrupted step is
  refused as uncertain by recovery, by design (#5835).
- One run at a time during this phase, so the long scans do not compete.
- Every failure keeps its evidence: the run id, the step and the log tail from
  the owner's Library.

**Likely first failures.** Uncertain recovery after an interruption; clone
network errors (#5875 retries them; the mirror in phase 4 removes most clones);
time or memory on the deep scans; first-run image pulls; the isolation probe on
k3s's built-in network-policy enforcement; and a toolchain whose Go version
lags `go.work`, which would make every step download a toolchain or fail.

**Exit.** One full run passes every stage on the epic branch, with no engine
rollout during it. After the merge, one more full run on main's head confirms
it there. Their per-step
timings are phase 4's starting profile.

## 6. Phase 3: shadow checks, scan cadence and retiring the scan workflows

**Shadow checks.** The pipeline reconnects with `poll` delivery. Every pull
request head gets an affected-mode run and every main push a full-mode run, each
reporting the non-required `MemQL / memql` check. A newer push to the same pull
request cancels its stale run, as it already does. While the workstation is off
no check appears; a run cut off midway is recovered or concluded when the
cluster returns. A missing or late shadow check blocks nothing.

**Scan cadence, in the manifest.** `secrets-history` and `analysis` take
`on: [release, schedule, manual]`, losing `push` and `merge_group`.
`vulnerabilities` and `secrets-current` keep running on pull requests and
pushes. The package already declares `schedule: daily`, so the existing
`schedulePipelines` automation (03:30 UTC) opens one scheduled run on main's
newest head, and the sealed workflow includes only the stages marked
`schedule`.

**SARIF publication (engine change 2).** A step names its SARIF files in the
manifest; each must also be one of the step's declared `artifacts`. After the
step's artifacts are verified, a new run-scoped action uploads each file through
the cluster's GitHub App with the existing `UploadSARIF` and `SARIFUploadStatus`
calls in `component/identity/githubapp`, against the run's commit and ref, with
the step key as the category. A file is uploaded whatever the step's exit status,
because findings must reach code scanning even from a failing scan. The upload is
journaled like any external effect: intent before, receipt after, and a status
read rather than a second upload after an ambiguous reply. The sealed default
workflow decides when it is called (default-branch and release runs); the native
action holds the authority, the journal and the reconciliation. An ingestion
failure is recorded on the run and in its check summary; it does not change the
step's verdict, and it blocks the retirement below.

**Retiring the four workflows.** After seven consecutive scheduled deep-scan
runs pass, with their CodeQL results visible in code scanning, one PR deletes
`codeql.yml`, `gitleaks.yml`, `govulncheck.yml` and `sbom.yml`, and updates
everything that names them: `docs/internal/ops/ci-design.md`, the root
`CLAUDE.md` sentence about CodeQL (that file is gate-scanned), and any CI test
that reads workflow files. A night the workstation is off neither counts toward
the seven nor resets them; a red scheduled run resets them. Scorecard stays on
Actions; it is not in the package.

## 7. Phase 4: fast

**Measurement.** Phase 2's runs give per-step timings split into queue wait, pod
start (cache preparation, clone, services) and command time. A small comparison
tool under `scripts/ci` reports, per commit, both systems' times and verdicts:

- **Start:** the creation time of the commit's `ci.yml` run, which is when
  GitHub saw the push. It is the start for both systems, so the poll's delay
  counts against MemQL.
- **End:** the completion of `MemQL / memql` and of `ci-required`.
- **False green:** MemQL concluded success on a commit where a `ci.yml` lane
  that has a MemQL equivalent failed. A failure in a lane MemQL does not have
  (the editor-extension lanes) is not one.

**Levers, applied one at a time and measured.**

1. **Concurrency.** The Job count and per-step CPU and memory, tuned from
   measured peaks inside the build node's caps, starting at 6 Jobs. Heavy steps
   reserve more through `cpuMilli` and `memoryMiB`.
2. **Reshape for parallelism.** The pull-request path becomes one stage, with
   checks, quick scans, tests and gates side by side; `go-checks` splits into
   two or three steps (build and vet; lint and engine load; the drift checks);
   the five per-tag build passes and seven per-tag test passes fold into a few
   steps sized from measurements. Daily and release runs put the deep scans in
   the same parallel stage, within their declared reservations.
3. **Per-step overhead.** The git mirror (engine change 1, section 8) replaces
   the per-step fetch from GitHub; the Tailwind CLI comes from a declared
   cache directory (#5816); the Cockpit-era `-p=1` and 512 MiB settings are lifted
   wherever measured memory allows.
4. **Shards.** `db-tests` splits along `ci.yml`'s budget classes
   (`component/memql` serial at 600s on two shards; `component/packages` at
   300s; `integrations/work` and `integrations/library` serial at 180s; the rest
   at 180s on two shards), which needs the `trees:` filter (engine change 3).
   `go-tests` is rebalanced from the timing table, refreshed from full runs.

**If the target is still missed**, the next lever is the one `ci-design.md` D6
predicts: splitting the heaviest packages (`component/memql`,
`cmd/memqllint`) by concern. That is its own work and is not planned here.

## 8. The engine changes

Each keeps the boundary record's split: the manifest and the sealed DSL decide,
native code performs one bounded operation and enforces its safety.

1. **A git mirror on the build node.** A bare mirror per owner, repository and
   trust lives on the cache volume, under the same sub-path scheme as the step
   caches, so a step can read only its own repository's objects. A step's clone
   container fetches the run's commit into the mirror only when it is missing,
   under a file lock, and then checks the commit out from the mirror into
   `/workspace`. The first step of a run pays one fetch; the rest clone locally.
   The installation token stays in the clone container, as it does today.
2. **SARIF publication.** As specified in section 6: a manifest field naming a
   step's SARIF files, a run-scoped action the sealed workflow calls, the
   GitHub App's code scanning permission, and a journaled upload that is
   reconciled rather than repeated.
3. **A `trees:` step filter.** A step may name directory prefixes that restrict
   the packages it takes after `packages` and `only` have selected them. A plain
   entry includes; a `!` entry excludes, matching the bucket grammar; a list of
   only `!` entries means everything selected except those trees. The compiler
   applies the filter mechanically. That every db-gated package lands in exactly
   one class is held by the engine repository's own manifest test, not by the
   compiler.
4. **A manual run names a branch.** `pipelinesRunLatest` takes an optional
   `branch`; empty keeps the default branch. A name GitHub would not accept is
   refused `pipeline_branch_invalid` before any request leaves the cluster.
   MemQL OS offers it as **Other branch** beside **Run latest**, listing the
   repository's branches through the source's own connection. Epic A needs it
   to prove its own branch before the single merge.

## 9. Testing

- **The mirror:** unit tests for the clone container's script and the mirror
  path; a concurrency test with two steps refreshing and reading one mirror at
  once; and an opt-in live k3d test in which a step clones from the mirror with
  no route to GitHub.
- **SARIF publication:** tests that an ambiguous upload reply leads to a status
  read and never a second upload, that an ingestion failure is recorded on the
  run, that only default-branch and release runs upload, and that a failing scan
  step still uploads, against the existing in-process GitHub stand-in.
- **The `trees:` filter:** pure tests in `component/pipelines` for inclusion,
  exclusion and an exclusion-only list, plus compiler refusals for a malformed
  entry.
- **The engine's manifest:** `memql_package_manifest_test.go` and
  `memql_pipeline_portable_coverage_test.go` move to amd64 and the reshaped
  stages, assert that the folded steps still cover all five build tags and all
  seven test tags, and hold the `db-tests` classes equal to `ci.yml`'s
  `CLASSES` table, which closes today's shard drift.
- **The overlay and `make up`:** the existing render tests cover the new local
  values; the build-node option gets a script test.
- **Branch runs:** leaf tests for `ValidBranchName`, `RunLatest` tests for a
  named branch, a malformed name refused before GitHub, and one commit through
  two branches being one attempt; OS tests for **Other branch**.
- **Live evidence:** the passing branch and main runs, the scheduled scans and the shadow
  pull-request runs, with run ids recorded in the issues.

## 10. Tracking

Two epics under #5480, each labelled `epic`, `epic:<name>` and `claude`, with
`claude`-labelled tasks, tracked with ccpm. The owner asked on 2026-10-11 for
one PR per epic covering all its issues, so each epic ships as a single PR.
Each epic gets its own implementation plan: epic A's from this record, epic
B's once phase 2 exits, because its tuning starts from phase 2's measurements.

- **Epic A, local green (phases 0 to 2).** One owner-approved dispatch of the
  amd64 toolchain (section 4) precedes it. Its single PR carries the
  build-node option for `make up`, the local overlay values, the amd64
  manifest and its tests, #5697's baseline recorded in `ci-design.md`, the
  branch argument and **Other branch** (engine change 4), and the fixes found
  in the green loop, each verified locally on the epic branch before the
  merge. #5835's local-demonstration evidence comes from these runs.
- **Epic B, shadow, scans and fast (phases 3 and 4).** Its single PR carries
  the scan cadence, SARIF publication (engine change 2) and its app
  permission, the git mirror and the `trees:` filter (engine changes 1 and 3),
  the reshaped package with the lifted Cockpit-era settings, and the deletion
  of the four scan workflows once their exit is met. One PR rules out a
  toolchain rebuild inside the epic, because the toolchain workflow builds
  only from main, so its Tailwind change takes #5816's second proposal: the
  tools directory comes from the environment and points at a declared cache.
- **Existing issues:** #5697 joins epic A; #5816 joins epic B; #5813 stays with
  the cloud phase; #5504 to #5509 stay open under #5480.

## 11. Documentation

- This record.
- `docs/internal/planning/pipelines-self-hosting.md`: the status, its checklist
  reconciled with what merged, and its #5844 citation.
- `docs/public/operate/pipelines-substrate.md`: the stale "engine's own
  pipeline" section.
- `docs/public/operate/pipelines.md`: the scan cadence, SARIF publication, the
  `trees:` filter and the mirror.
- The program record: a short notes section pointing here.
- `docs/internal/ops/ci-design.md` and the root `CLAUDE.md`, when the scan
  workflows retire.

## 12. Risks and accepted limits

- **The workstation is the host.** Shadow checks and the daily deep scans exist
  only while it is on. Accepted until D7's cloud phase.
- **Shared capacity.** The build node's caps protect the serving cluster and the
  owner's other work, and the Job count protects the build node; neither makes
  the workstation's own sessions lighter.
- **Main's engine runs the pipeline.** A bad engine change can break local CI.
  Accepted for a shadow check; D7 runs a released engine.
- **Toolchain drift.** When `go.work`'s Go version changes, the toolchain must
  be rebuilt and re-pinned, or every step downloads a toolchain. Bring-up checks
  the two agree; a gate holding them together is a follow-up.
- **Shared working tree.** `make dev` builds whatever is in the working tree,
  and the repository's main checkout is shared by several sessions, so this work
  runs from its own worktree.

## 13. Out of scope, for the next phase

- Moving the pipeline to the ZNAS cloud instance, the AKS network-policy and
  build-pool approvals, making `MemQL / memql` required, deleting `ci.yml` and
  the remaining milestones M1 to M4 (#5506 to #5509).
- The lanes `ci.yml` has that the package lacks: the VS Code and productivity
  extension lanes, the Docker stage of `os-checks`, and the breaking-change check
  against a pull request's base.
- Release candidates, installation updates, notification channels (#5504) and
  verify-rollout (#5505).
- Cockpit, native and macOS steps (memql-cockpit #485, #486).
- Manifests that name more than one platform.
- Per-push deep scanning.
