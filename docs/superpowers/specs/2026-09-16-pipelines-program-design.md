# Pipelines -- MemQL runs its own CI, a five-epic program, with a bridge first

- **Date:** 2026-09-16
- **Status:** agreed with the owner in the 2026-09-16 brainstorm. Every fork below
  was put to the owner and answered (the risk posture, where the pipeline
  definition lives, where a step executes, the app placement, the tab's name,
  the run page's layout from two rendered mockups); the derived decisions
  follow from those answers and were presented as seven sections, each
  approved. Issues were filed on 2026-09-16; the table in section 8 names
  them.
- **What it is:** the design for CI/CD as a platform feature -- a pipeline is
  declared in the repository's `memql-package.yaml`, delivered by the GitHub
  App that GitHub Connect already installs, compiled to a work goal, executed
  as Kubernetes Jobs in the cluster (or on a fleet machine when a step names a
  need the cluster cannot meet), reported back to GitHub as a check run,
  watched in MemQL OS inside Deployables, and announced on a channel with the
  run page as the report link -- plus the bridge that gets the current
  GitHub Actions loop down this week using the same selection and sharding
  the pipeline model will express.
- **Why it is pivotal:** the pull-request loop is the cadence of every change
  to the platform. A Go-touching pull request takes 14 to 19 minutes per push
  today and is re-run two or three times per merge; several sessions iterate
  in parallel, so the loop is the bottleneck of the whole program. And the
  feature is a customer surface: a client connects a repository through the
  OS and gets checks, deploys and notifications for it, which Deployables
  already half-provides. MemQL building MemQL is the same feature with the
  owner as the customer -- dogfooding with no special path.
- **Repositories:** `memql` (`.github/workflows`, `scripts/ci`, `dsl/pipelines`,
  `component/pipelines`, `component/inbound`, `component/packages/githubapp`,
  `integrations/workbench`, `component/work`, `deploy/k8s`, `clients/os`);
  `memql-znas` (the Python deployment-notifications observer retires);
  `memql-project` (its design's placeholder is replaced); `memql-cockpit`
  (the fleet half of a routed step reuses the existing dispatch).
- **Measurements this record rests on:** taken 2026-09-16 from the last 40
  pull-request runs and the last 7 days of `ci.yml` runs; reproduced in
  section 1.

## 1. Problem

A pull request that touches any Go file runs every Go test, every db-gated
test, all seven build-tag passes and all 51 modules' boundary checks. Path
filtering exists and works, but at Go granularity it is all-or-nothing.

| Pull request touches | Wall clock | Jobs |
|---|---|---|
| any `.go` file | 14 to 19 min | about 28 (24 `ci.yml` + 3 CodeQL + gitleaks) |
| only `clients/os` | 4 to 5 min | about 6 |

Two lanes set the critical path, both on one 2-core `ubuntu-latest` runner
with serial steps:

| Lane | Median | What dominates |
|---|---|---|
| `db-tests` | 10 to 20 min | `component/memql` about 7 min; `packages` 84 s; `work` 79 s |
| `go-tests` | 12 to 14 min | the root package uncached (53 s of repo-walking gates), then 169 packages, then 7 tag passes each re-running `app` and `component/node` |

The pipeline is not slow because Go compiles slowly. A 1,073,806-line tree
with 2,244 test files runs its full suite on the smallest runner GitHub
offers, one lane at a time, with no sharding and no impact analysis.

The 15 minutes is then paid more than once per change:

- Every push re-runs everything. In 7 days: 187 pull-request runs, 56
  cancelled by a newer push, 26 failed.
- A merge to `main` runs the full suite unconditionally (67 push runs in 7
  days, about 17 min each) and a merge-group run when queued.
- The ruleset's `strict_required_status_checks_policy` is on. Every merge
  flips every other open pull request to `BEHIND`, which forces
  `update-branch` and a fresh full run. The merge queue makes that rule
  redundant, and [merge-queue.md](../../internal/ops/merge-queue.md) already
  records the cost.
- CodeQL runs for three languages with no path filter on every pull request
  (flagged in the 2026-08-06 audit, unchanged); no Node lane caches npm; the
  Go cache key spans all 51 `go.sum` files, so one dependency bump cold-starts
  every job.

The repository is public, so GitHub-hosted standard runners are free and
unlimited: cost is not the reason to move CI, and larger runners are not
free. Speed, control and the customer surface are the reasons.

Splitting the repository would not help. The suite does not shrink by
moving, the cross-module contract tests become version-bump coordination,
and the atomic pull request is lost. Large Go monorepos keep one repository
and run what a change affects, which Go makes exact: `go list -deps` gives the
reverse import graph.

## 2. What the tree already has

- **Inbound webhooks** (`component/inbound`, `POST /inbound/{source}`): a
  deny-by-default source allowlist with per-source HMAC, and the verifier
  already handles GitHub's `X-Hub-Signature-256`. A delivery lands as a
  `v1:platform:inboundRequest` row an automation can trigger on.
- **GitHub Connect** (`component/packages/githubapp`): a GitHub App
  installation is stored as a `sourceCredential` grant. An installation is
  both a credential and an event subscription, and it can write check runs.
- **The workbench** (`integrations/workbench`): clones a GitHub source and runs
  a manifest's build command for Deployables. Today that is `/bin/sh -c` in a
  per-run directory on the workbench pod's own filesystem.
- **The work spine** (`v1:work:{goal,run,step,approval}`): journaled steps,
  resume after a crash, human gates, the Nexus drawing.
- **The fleet**: user-owned machines with labels, a router, consent and
  cross-node dispatch, which is what a self-hosted runner pool is.
- **Deployables** (`memql-package.yaml`, `component/packages`): the manifest in
  the repository, the source page, the composition diagram, the CD half.
- **The Discord deploy notification**: a standalone Python observer in the
  instance cluster's `memql-notifications` namespace, watching two Argo
  applications, posting Version, a MemQL OS link and the literal text
  "Coming soon in MemQL OS." as Deployment details. Its design
  (`memql-project`, `docs/design/deployment-notifications`) says a future OS
  report link replaces that placeholder.
- **The OS interface language** (`clients/os/DESIGN.md`) and Supervised Visual
  Composition (`clients/os/SUPERVISED-VISUAL-COMPOSITION.md`): the twelve
  rules, the rail-as-form device, guided workflows for setup and
  compositional workspaces for ongoing configuration.
- **CI's own record**: [ci-design.md](../../internal/ops/ci-design.md),
  [ci-audit.md](../../internal/ops/ci-audit.md),
  [merge-queue.md](../../internal/ops/merge-queue.md),
  [ruleset-baseline.md](../../internal/ops/ruleset-baseline.md), and the
  dependency-closed filter buckets of memql#3163, declared in `ci.yml` and
  consumed by no lane.

## 3. Decisions

### D1 -- Affected subset on a pull request, the full suite in the queue

A pull-request run executes only what the change affects, computed from the
Go import graph plus the existing path buckets, with the long lanes sharded.
A merge-group or push run executes everything, once, on the exact tree that
lands, and that is the gate. A pull request can therefore be green and fail
in the queue; the author fixes it then. The count of such false greens is
measured from run rows (section 7, M1) and is what this decision rests on.

### D2 -- The bridge first, and it is not a detour

Section 1's fixes land inside `ci.yml` and the ruleset before any engine
work: affected-package selection, sharding, tag passes as a gated matrix,
CodeQL off the pull-request path, caches that hit, the strict flag off.
Every one of those but the last two is exactly what the manifest and the
pipeline runner must express later, so it is solved and measured by the
time the pipeline runs it. The end state is stated in D16 so the bridge
cannot become a second CI.

### D3 -- The pipeline definition lives in the repository

A `pipeline:` block in `memql-package.yaml`, the file Deployables already
reads, so a repository has one manifest and one place the platform looks. It
is versioned with the code, a pull request can change its own pipeline, and
the OS shows runs but does not author steps. There is no OS-authored
pipeline.

### D4 -- Trigger in through the GitHub App; check run out; polling stands in

The app GitHub Connect installs gains `checks: write`, `pull_requests: read`
and subscriptions for `push`, `pull_request`, `merge_group` and `check_run`.
GitHub delivers to `POST /inbound/github`, an allowed source under the
existing verifier; no per-repository webhook is ever created. One shipped
automation turns the row into a run. A pipeline row carries
`delivery: webhook | poll`; under `poll` a scheduled automation lists the
default-branch head and open pull-request heads through the installation
token and opens runs for unseen SHAs. A run is keyed on
`(repository, sha, mode)`, so a webhook and a poll for the same head are one
run. One check run per pipeline per SHA is created `queued`, `in_progress`
and `completed`, with the OS run page as its details link and the stage
table as its summary. Both directions are needed whatever the trigger.

### D5 -- The mode is decided by the event

| Event | Mode |
|---|---|
| `pull_request` opened or synchronized | affected subset |
| `merge_group` checks requested | full suite |
| `push` to the default branch | full suite plus deploy and notify stages |
| `check_run` rerequested | the original run's mode |

### D6 -- Fork pull requests are refused, not queued

The repository is public and a fork head is arbitrary code from a stranger.
The run row records the refusal, the OS shows it, and a later allow-list of
trusted forks is an owner decision. This is deliberately stricter than
GitHub Actions' first-time approval gate.

### D7 -- A stage is ordered, a step is parallel, a step is a shell command

Steps in a stage run at once; `needs` orders stages. `shards: N` expands to N
steps with a package slice each, assigned from a timing table the run writes
back after every full run. A step's `run` is a shell command in the image's
working copy, the same contract as a deployable's `build.command`. There is
no step language. The manifest compiles to one work goal with one
`v1:work:step` per step, so journaling, resume, approvals and the Nexus
drawing apply without a second step model.

```yaml
formatVersion: 1
name: memql
pipeline:
  image: ghcr.io/znasllc-io/memql-toolchain@sha256:...
  services:
    postgres:
      image: ghcr.io/znasllc-io/timescaledb-pgvector@sha256:...
  caches: [go, npm]
  select:
    go: import-graph
    buckets:
      gates: ["**/*.md", "**/*.memql", "dsl/**", "scripts/**", "deploy/**"]
      os:    ["clients/**", "sdk/ts/**", "brand/**"]
  stages:
    - name: checks
      steps:
        - name: build-vet
          run: go build ./... && go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: go-tests
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 4
        - name: db-tests
          run: MEMQL_REQUIRE_DB=1 go test $MEMQL_PACKAGES
          packages: affected
          only: db-gated
          services: [postgres]
          shards: 4
        - name: os-checks
          run: make os-typecheck os-test os-build
          when: { bucket: os }
          needs: { docker: true }
    - name: deploy
      on: [push]
      steps:
        - name: verify-rollout
          run: memql-verify --target=https://api.znas.io --version=$MEMQL_VERSION
    - name: notify
      on: [push]
      channel: znas-instance
```

### D8 -- Selection is declared, not scripted

`packages: affected` reads the import graph in affected mode and means
everything in full mode. `only: db-gated` intersects with the trees the
platform knows need a database. `when: { bucket: <name> }` is the path-bucket
gate for non-Go lanes. A step with none of these always runs. `needs` is the
workbench's existing environment hint (`docker`, `display`, `macos_tooling`,
`gpu`, `user_files`); anything the cluster job cannot satisfy routes the step
to a fleet machine with the matching label, and an unknown need is a typed
refusal, never a silent fallback to somebody's laptop. `on` restricts a
stage to modes. Secrets are references resolving to a `globalSecret` under
the pipeline owner's actor; a manifest never carries a value.

### D9 -- A bad manifest is a failed run with a typed refusal

Shown in the OS and in the check run, in the vocabulary Deployables uses
(`deployable_target_not_offered` and its siblings). Never a skipped run,
because a skipped run reads as green.

### D10 -- A step is a Kubernetes Job; the fleet is the routed exception

Each step is a Job in a dedicated `memql-pipelines` namespace, created and
watched by the workbench node: an init container shallow-clones the SHA under
the installation token; the manifest image is the main container running the
step's command with `MEMQL_PACKAGES`, `MEMQL_RUN_ID`, `MEMQL_VERSION` and the
resolved secrets in its environment; named services run as sidecars; the cache
volume is mounted; no cluster credential of any kind is present. Resource
limits, the concurrency ceiling and how the cache volume is backed (a PVC on
k3d, blob-backed on AKS) are overlay values. The same path runs locally and
in the cloud. A step naming a need goes to the fleet through the existing
dispatch with the same clone-and-command contract; a machine on a sibling
replica is skipped, never failed.

### D11 -- Logs, artifacts and lifecycle reuse what exists

Step output streams into the log store tagged with the run as subject
(`logger.Subject`), so the run page's log view is the Logs surface; a size
cap applies and the full log archives to the Library. `artifacts:` paths
upload as Library files owned by the pipeline owner. A new push to the same
pull request cancels its running affected-mode run; a full-mode run is never
cancelled. Per-step timeout (default 20 min) and a run wall-clock ceiling are
typed failures. The Job is deleted once its log is captured; caches persist;
runs and logs are retained for a configured number of days, archive first,
delete second. Every pipelines concept's events carry broadcast routing rules
from day one.

### D12 -- Inside Deployables, on the source; a Runs tab; no rename

Not a new app and not Settings. The Deployables list already groups "From a
source" and the source page already shows Repository, Tracking, Deployed,
Latest upstream and "Apps it produces"; a pipeline is a fact about that same
repository. The composition diagram gains Checks between Source and the
apps. The engine repository is a source with a pipeline and zero apps, with
no special case. A third section, **Runs**, lists every run across every
source; connecting a pipeline happens on the source page. The app keeps its
name; a rename to the delivery arc was considered and declined as not worth
the capability churn.

### D13 -- The run page is the add-a-machine device applied to a run

Chosen from two rendered mockups: stops across the top (A) over stops down
the left (B). Breadcrumb "Runs > source > branch"; the commit message as the
title; a meta line with the pull request, SHA, mode and duration; the stages
as stops read from the step rows in `railFor`'s standing mode (the current
stop open, later stops ahead, a refused run not dimming the stops after it);
the open stop's steps with where each ran and for how long; a failed step's
last lines inline with "open the full log" and "save to Library"; a skipped
step with its reason in words; artifacts; and one ActionBar carrying the
state and the legal acts, Open on GitHub, Re-run failed, Re-run. It replaces
the list per rule 11.

### D14 -- The source page, the Runs tab and the connect rail

The source page's composition gains a Checks node showing the last run per
branch; the facts list gains a Pipeline line (manifest, stage count,
delivery, compute); "Latest upstream" gains "checks passed, not yet
deployed"; the ActionBar reads "Tracked, N apps, N live, checks on" with
Pipeline settings and Deploy. The Runs tab is newest first, grouped by day,
one line per run (the commit message; source, branch, SHA and trigger; the
outcome with stage and duration; the time), Refine behind the Head, a
retained collection, the arrival cue on a terminal state only. Connecting a
pipeline is the rail-as-form page over the source with three stops,
Repository, Compute, Confirm, the manifest read at the first stop so the
stages show before confirming.

### D15 -- The readiness item, owner-only through a capability

Pipelines is an optional item in the cluster's readiness list with three
sub-steps: GitHub App installed, at least one repository connected, compute
confirmed. The settings icon carries the marker for a viewer holding
`read app:settings/pipelines`, seeded on the owner, while any optional item
is neither done nor dismissed; "Not now" clears it and the item stays in
Settings. "Pre-configured" is the GitHub app-manifest flow: the cluster
generates the app definition with permissions, subscriptions and the inbound
URL filled in, and the owner's click creates and installs it. A
permissions-changed prompt from GitHub is surfaced by the item rather than
left to block runs silently.

### D16 -- Notifications are a stage; rollout is verified from outside; the bridge retires

A notification is a manifest stage naming a `v1:pipelines:channel` (kind plus
a `globalSecret` reference; Discord webhook first, email reusing the sender),
delivered over the existing `outboundRequest` path. Its message carries
Version, the MemQL OS link and the run page as Deployment details, which
replaces the placeholder. The observer's rollout checks become a
`verify-rollout` step that probes the target from outside (the deploy-version
endpoint, the OS bundle hash, identity and API health), so a customer's
target and ours are verified the same way and no pipeline needs a target
cluster's Argo. The engine repository's own pipeline runs on the ZNAS
instance under a released engine and deploys to that instance by the existing
release path -- never the tree under test. GitHub keeps the repository, the
ruleset and the merge queue permanently; MemQL writes the required check.
`ci.yml` retires by the milestones in section 7. No interim link to a GitHub
run is added to the Discord message; the placeholder stays until the run
page exists.

## 4. The five epics

### Epic 1 -- The CI bridge (priority P16, `epic:ci-bridge`)

Inside `ci.yml` and the ruleset. Turn off
`strict_required_status_checks_policy` and record why; CodeQL off the
pull-request path, npm cache, Go restore-keys; the tag passes as a gated
matrix; the affected-packages tool under `scripts/ci`; `go-tests` and
`db-tests` as shard matrices from a checked-in timing table. Push and
merge-group runs stay unconditional. Two PRs. Exit: a Go-touching pull
request under 7 minutes for a week (M0). Expected:

| | Today | After |
|---|---|---|
| PR run, typical | 15 to 19 min | 5 to 7 min |
| PR run, small change | 15 to 19 min | 3 to 4 min |
| Queue run | 15 to 19 min | 8 to 10 min, once |
| Extra runs from BEHIND | one per sibling merge | none |

### Epic 2 -- The seam (priority P17, `epic:pipelines-seam`)

The concepts, the GitHub App's new permissions and subscriptions, the inbound
source, the trigger and poll automations, the manifest compiler and
affected-set computation in a leaf package, the run over the work spine, the
check run, and the in-process hop test. One PR.

### Epic 3 -- The substrate (priority P18, `epic:pipelines-substrate`)

The namespace and RBAC in `deploy/k8s`, the Job runner on the workbench node,
fleet routing for a step naming a need, logs and artifacts, lifecycle and
retention, the toolchain and Postgres images, and the cluster-e2e leg. One PR.

### Epic 4 -- MemQL OS (priority P19, `epic:pipelines-os`)

Seeds and capabilities, the Runs tab, the run page in layout A, the source
page's Checks node and facts, the connect rail, and the Settings readiness
item with the app-manifest flow. One PR.

### Epic 5 -- Delivery (priority P20, `epic:pipelines-delivery`)

Channels and the notify stage, `verify-rollout`, then the milestones: the
engine's own pipeline on the ZNAS instance (M1), the queue (M2), the observer
retired (M3), the image build moved and `ci.yml` deleted (M4). Five PRs, one
per milestone after the first.

## 5. Failure modes

- **A pull request is green and the queue fails it.** Accepted under D1;
  counted per M1; the author fixes forward. If the count is not small the
  selection is wrong, not the decision, and the affected-set fixtures grow.
- **The affected set is too small.** The tool's floors refuse an implausibly
  small answer, a changed `go.mod`, `go.work`, the tool itself or `ci.yml`
  means everything, and the queue run is the backstop.
- **A shard table drifts.** A package missing from the table lands in the
  lightest shard; the table is rewritten after every full run.
- **The webhook never arrives.** `delivery: poll` on the pipeline row, with
  the run key making a late webhook a no-op.
- **A fork pull request.** Refused before any Job exists; the refusal is a
  run row the OS shows.
- **A manifest that cannot compile.** A failed run with a typed refusal,
  visible in both the OS and the check; never a skipped run.
- **A step needs what the cluster lacks.** Routed to the fleet by need; an
  unknown need refused; no machine online means the step fails typed, and a
  plan never waits for a laptop.
- **The engine breaks its own CI.** The pipeline runs on a released instance,
  not the tree under test, so the fix's pipeline runs on the previous
  release. The local cluster runs the same pipeline under poll for a
  developer's own branches and is never the gate.
- **A Job outlives its run.** TTL plus deletion after log capture; the
  retention sweep archives before it deletes.
- **The Runs page is correct on load and frozen after.** Broadcast routing
  rules for every pipelines concept are part of epic 2, not an afterthought.
- **The readiness marker nags forever.** The item is dismissable and optional.

## 6. Testing

- The manifest compiler and the affected-set computation are pure functions in
  a leaf package tested on fixtures, with no engine and no database, the way
  `component/work` is built.
- The trigger seam has an in-process hop test: an inbound row on one node, a
  run opened on another; fork refusal, dedup by run key and the event-to-mode
  table asserted. Single-node green is not the evidence.
- A cluster-e2e leg runs one manifest end to end on k3d: a Job with a Postgres
  sidecar, a sharded step, a fleet-routed step refused for a missing label, a
  fork refused, the check-run body asserted against a recorded GitHub fixture.
- The live reporting seam ships as a disarmed `workflow_dispatch` lane, the
  proving suite's pattern.
- Every `requires:` name in the OS is pinned to a seeded row by the existing
  gate; the run page and the Runs tab are judged as rendered screenshots,
  both modes, empty and populated, per `DESIGN.md`.
- The bridge is judged by a week of measured pull-request wall-clock recorded
  in `ci-design.md` against the numbers in section 1.

## 7. Milestones and the retirement of `ci.yml`

| Milestone | Exit criterion | What retires |
|---|---|---|
| M0, the bridge (epic 1) | a Go-touching pull request under 7 min for a week | nothing |
| M1, pipelines on pull requests | the MemQL check reports on every pull request for two weeks with no false green found by the queue run | the pull-request lanes in `ci.yml`; the ruleset requires the MemQL check |
| M2, the queue | full-mode runs gate the merge queue for two weeks | `ci.yml` on `merge_group` and `push` |
| M3, deploy and notify | `verify-rollout` and the notify stage run on three releases | the Python observer; the placeholder |
| M4 | the engine image build moves to a pipeline on a released instance | `ci.yml` deleted; `build-engine-images.yml` last |

"No false green" in M1 means a pull-request run that reported success on
content whose full-mode run then failed, not a sibling merge. The run rows
make it countable.

## 8. Issues

Filed 2026-09-16, all `claude`-labeled, each epic carrying its `epic:<slug>`
label and its program priority.

| Epic | Issue | Tasks |
|---|---|---|
| 1, the CI bridge | #5476 | #5481 strict flag off and recorded; #5482 CodeQL, npm and Go caches, deploy-gate concurrency; #5483 tag passes as a gated matrix; #5484 the affected-packages tool; #5485 sharded lanes from a timing table |
| 2, the seam | #5477 | #5486 concepts and routing rules; #5487 the GitHub App's permissions, subscriptions and the inbound source; #5488 the trigger and poll automations; #5489 the manifest compiler and affected set; #5490 the run over the work spine and the check run; #5491 the hop test |
| 3, the substrate | #5478 | #5492 namespace, RBAC and overlay values; #5493 the Job runner; #5494 fleet routing by need; #5495 logs and artifacts; #5496 lifecycle, retention and the images; #5497 the cluster-e2e leg |
| 4, MemQL OS | #5479 | #5498 seeds and capabilities; #5499 the Runs tab; #5500 the run page; #5501 the source page; #5502 the connect rail; #5503 the Settings readiness item |
| 5, delivery | #5480 | #5504 channels and the notify stage; #5505 verify-rollout; #5506 M1; #5507 M2; #5508 M3; #5509 M4 |
