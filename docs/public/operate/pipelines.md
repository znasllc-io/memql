---
title: Pipelines -- a repository's checks, run by the cluster and reported on GitHub
audience: public
status: stable
area: operate
sinceVersion: 0.25.0
owner: znas
---

# Pipelines

**Audience:** owners and developers connecting a repository's checks, and operators running the cluster they run on.
**Design:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md`, decisions D1-D16 and its implementation notes for epic memql#5477.

A pipeline is a repository's checks: declared in the `pipeline:` block of the
repository's `memql-package.yaml`, delivered by the cluster's GitHub App, run by
the cluster as a work goal, and reported back to GitHub as a check run on the
commit it checked. By default, pull requests select affected coverage; merge
queue, default-branch push and published-release events select full coverage
for their applicable stages. An installed policy can select component-specific
stages and explicitly reuse eligible verification evidence. Artifact preparation
and publication still require their own recorded effects.

> **What runs today.** This release takes deliveries, opens and deduplicates
> runs, plans them, executes their steps and reports them on GitHub. A step
> runs as a Kubernetes Job on the cluster, or on one of the owner's own machines
> when it names a need: [the pipelines substrate](pipelines-substrate.md) is
> that half, and what an operator sets up before the first run. Read
> [What runs today](#what-runs-today) before you make the check required.

Pipelines manages repository delivery in MemQL OS. Each configured source
(`v1:platform:package`) has at most one, owned by the source's owner, and every
GitHub token a run uses is minted through that owner's GitHub connection. It is
not the order a deploy runs in, which [Packages](packages.md#the-order-and-why-it-never-changes)
also calls a pipeline: a pipeline here checks commits, and never starts a
deploy by itself.

**A repository has one pipeline.** Two would write two check runs of the same
name on every commit, so connecting a repository that another source's pipeline
already runs is refused `pipeline_already_connected`. To move a repository's
checks to another source, disconnect the pipeline it has first
([Connecting a pipeline](#connecting-a-pipeline)).

| Row | One per | What it holds |
|---|---|---|
| `v1:pipelines:pipeline` | source | The repository, how deliveries arrive, where steps may run, which secrets they may resolve, what the poll has seen, the timing table. Never the manifest: every run reads that at its own commit |
| `v1:pipelines:run` | attempt of one run key | The event and mode, status and conclusion, the refusal that ended it, the run's own reason when it failed and no step reported why, the check run's id and state, the stage table, the agent driving it |
| `v1:pipelines:channel` | notify target | Where a notify stage delivers; managed in Pipelines → Settings → Channels |
| `v1:pipelines:train` | departure of a repository's [release train](#release-trains) | The pull requests on board, left off and dropped, the train branch, pull request and run, the state, the approval and the landed commit |

A run compiles into one `v1:work:goal` with one `v1:work:step` per step, so the
work spine's journal and the Nexus drawing show it like any other work. Every
row is readable by its owner, by a cluster owner, and by members of the account
its source belongs to -- exactly the people who read the source. None holds a
token, a secret value or a log line.

---

## The delivery lifecycle

**A successful check, a published release and a running installation are three
different results.** `main` can accumulate verified changes while an installation
continues running its selected release. A merge does not, by itself, select a
new release or authorize installation.

```mermaid
flowchart TD
    CHANGE["PR opened or updated"] --> CHECK["Scoped checks on the exact head; full coverage when uncertain"]
    CHECK --> REVIEW["Required tests and configured independent review"]
    REVIEW --> QUEUE["Protected merge queue checks the combined change"]
    QUEUE --> MAIN["Merge to main and verify"]
    MAIN -. "Operator chooses verified source and version" .-> BUILD["Cut and build the selected release"]
    BUILD --> CANDIDATE["Review retained artifacts and publication destinations"]
    CANDIDATE --> PUBLISH["Approve and publish the exact candidate"]
    PUBLISH --> CATALOG["Published release is available"]
    CATALOG -. "Operator selects an installation" .-> PLAN["Review the saved installation plan"]
    PLAN --> APPLY["Apply, observe and verify runtime health"]
```

The dotted arrows are explicit operator decisions. The solid arrows remain
subject to their configured checks, permissions and failure handling; they are
not a promise that every repository has enabled automatic merge or publication.
In `0.31.0`, native review requires a governed `deliver_change` proposal.
Version `0.31.2` includes an optional [ordinary PR review route](#independent-review-for-ordinary-pull-requests).
An installation must qualify real ordinary-PR and native-proposal reviews before
changing required review policy. If an existing code-owner requirement prevents
queue admission, replace it only through the reviewed, atomic transition
described [below](#independent-review-for-ordinary-pull-requests), then qualify
the first live mixed queue group under the new required checks. An unqualified
or failing group stays blocked.
A repository with [release trains](#release-trains) on replaces the queue
step: its pull requests stop at a passing check, and a train tests them together
and lands them when the owner approves the train pull request.
An existing automatic GitOps or package feed can reconcile changes to the source
it watches. Check that feed's settings before merging an installation update;
the separation above does not disable independently configured automation.

| Trigger | Purpose | What it does not prove |
|---|---|---|
| PR opened or new commits pushed | Verify that exact head; use affected coverage only when the dependency and change information is complete | That a later head or combined queue tree passes |
| Merge-queue group created | Verify the exact combined tree and required member reviews | That GitHub has merged it |
| Push to the default branch | Record verification of the merged source | That a release was selected or installed |
| Scheduled run | Perform the checks selected by the installed schedule and policy | Permission to change source, capacity or credentials |
| Explicit release build | Build artifacts from the selected immutable source and version | That the artifacts are published |
| Candidate publication | Publish approved artifacts and verify their destinations | That any installation has updated |
| Explicit installation selection | Prepare and review a saved plan; explicitly apply it, then observe the actual runtime | Completion until its identity, revision, images and health agree |

Use **Pipelines** for repositories, checks and runs; **Pipelines → Releases** for
release candidates and publication; and **Cluster → Updates** for the installed
version and installation decisions. **Nexus** explains goals, actions, decisions
and attention needed. **Deployables** describes the applications a source
produces and links to their delivery information. The detailed controls and
requirements are documented in [Release cutting](release-cutting.md) and
[Release candidates](release-candidates.md).

These permissions do not transfer between stages:

| Decision | What it permits | What it does not permit |
|---|---|---|
| Authorize a repository change | The captured source, paths and requested operation; `deliver_change` includes its independent review and ordinary queue recipe | Release publication, installation or changed repository protections |
| Accept an independent AI review | Evidence for the exact reviewed bytes and App-owned check | A permission grant or a substitute for required tests |
| Approve a release candidate | Publication of its exact verified artifacts to its declared destinations | Selecting or updating an installation |
| Apply an installation plan | The selected source and saved plan under current installation authority | Choosing a newer release or broadening the plan |

### Automated repair is an installed policy

The engine supplies the execution and permission boundaries. The installation's
DSL decides whether a failure should be diagnosed, what evidence is sufficient,
which review recipe to use, and when to stop. Installing the engine alone does
not enable a DevOps agent or grant it upstream write access.

```mermaid
flowchart TD
    FAIL["A check fails"] --> POLICY{"Installed policy permits diagnosis and has enough evidence?"}
    POLICY -->|No| ATTENTION["Explain what needs attention"]
    POLICY -->|Yes| DIAGNOSE["AI diagnoses the failure and proposes a bounded repair"]
    DIAGNOSE --> EVIDENCE["Independent review checks the evidence and scope"]
    EVIDENCE --> ENOUGH{"Evidence and scope accepted?"}
    ENOUGH -->|No| ATTENTION
    ENOUGH -->|Yes| AUTH{"Current maintainer authority permits the change?"}
    AUTH -->|No| REQUEST["Request maintainer approval or record refusal"]
    AUTH -->|Yes| PROPOSAL["Draft exact changes against current source"]
    PROPOSAL --> REVIEW["Independent review of the exact proposed bytes"]
    REVIEW --> ACCEPTED{"Accepted?"}
    ACCEPTED -->|No| ATTENTION
    ACCEPTED -->|Yes| PR["Create a repair PR"]
    PR --> CHECKS["Required tests, review and protected queue"]
    CHECKS --> OUTCOME["Record merge, refusal or attention needed"]
```

The policy must state its attempt, evidence and scope limits. Missing evidence,
an uncertain external effect, rejected review or revoked authority is not
permission to retry blindly. Recovery observes or resumes a saved operation
under the native contract; it must not invent a new successful result. AI may
explain a diagnosis and propose changes, but cannot grant permission, replace
test evidence or declare that a deployment is healthy. The journal records
these decisions and their evidence in readable terms; it is not an unfiltered
record of model reasoning.

Infrastructure, credential or capacity failures do not grant the repair recipe
permission to change those systems. The installed DSL must name its repair
limits and escalation path; a model cannot expand them. Likewise, repository
delivery recovery does not recover every other external operation. A lost
package confirmation may have reached publication or restarted its own node.
In `0.31.2`, observe that exact attempt before another action; automatic
continuation across package self-roll is not provided. See [Packages](packages.md).

Owners and developers on another host may request changes to their own
deployables. If the request also needs a core-engine change, that part needs
the core repository maintainer's authority. Being an owner of one installation
does not make the requester an owner of the upstream repository. The next
section describes the enforced request and review boundaries.

## Repository maintenance

Owners and developers can request a scoped change through Ask or a repository's
request form. The installation's `MEMQL_REPOSITORY_AUTHORITY_RESOLVER` selects
the maintainer source or request-only peer from operator bindings. Source
visibility and a local owner role do not grant upstream write permission.
Requests to another maintainer wait for that maintainer's recorded decision;
requests under the caller's own current provider write authority may start
immediately. The request records its repository's stable identity, allowed
paths, expiry, goal and execution. Related requests retain parent links, which
carry no additional authority.

The native proposal boundary accepts one immutable set of file replacements
against an exact base commit. A `propose_change` request authorizes a draft pull
request only. `deliver_change` additionally authorizes independent review and
merge-queue admission; it is available only when the installed authority route
declares both a review prompt and a bounded review-report and queue delivery recipe. An existing
draft request cannot be upgraded by retrying it with a different operation.

The `repositoryChangeReportReview` operation accepts only the native request ID.
It publishes the reserved app-owned `MemQL / review` check for the exact proposed
commit after the accepted independent review, then verifies GitHub's readback.
Its durable receipt binds the source, repository identity, proposal and captured
review. Queue admission requires that receipt and rechecks the exact provider
check; a matching title or success supplied by a caller grants nothing. Pipeline
names cannot claim this reserved review context. Required tests remain separate.
An unavailable provider leaves the same report waiting; the installed recovery
recipe explicitly resumes reporting before queueing, without drafting again.

The change recipe, independent review prompt with its partials, and delivery
recipe are captured before approval. Changing their content or routing
invalidates the delegation. The native reviewer receives the immutable source
and exact proposed replacements; a caller-supplied approval cannot substitute
for its recorded verdict. Delivery refuses any proposal that differs from the
approved review. Every effect rechecks the maintainer's current decision,
provider write permission, recipe and execution authority.

Queue admission verifies the same PR and reviewed head, required successful
GitHub checks, current default branch and applicable review rules. It may mark
a reviewed draft ready so GitHub can request required reviews. CODEOWNER and
other required approvals still block queue admission until satisfied; no admin
bypass or direct merge occurs. Pending checks and uncertain provider responses
retain the same durable delivery. A bounded scheduled recovery leases that
record across replicas and runs only its captured queue recipe. Recovery may
continue after the original run succeeds while its goal stays open; closing or
cancelling the goal, revoking approval, or changing the reviewed head prevents
further admission. `queued` records provider-confirmed admission, not a merge.
After admission, recovery only observes the same PR: it records a confirmed
merge or surfaces removal from the queue without automatically re-enqueueing.
An outage retains that observation-only state and reports the uncertainty.
Release publication and installation require their separate authorization.

Status and request-list reads retain the statement, requested operation,
decision, review and delivery response across reconnects and replicas. A local
maintainer receives links to their own goal and approval. Other requesters,
including remote installations, receive the request's status without private
upstream goal or approval identifiers.

A `repositoryChange` step applies a retained JSON proposal through the pipeline
source's current GitHub App grant. The command that generates the proposal
receives no writer token. This supports dependency tidy commits and draft
upgrade proposals while keeping file and branch authority in operator settings.

Enable this optional writer only when the source's installed GitHub App grants
**Contents: Read and write** and **Pull requests: Read and write**. The default
App setup now requests these permissions for governed delivery, along with
merge-queue writes, commit-status reads and read-only administration access to
observe classic branch protection. It never requests administration writes or
workflow-file writes. An existing App needs its permissions updated and the
installation owner must accept the change before a proposal or queue admission
can proceed. Build and proposal commands still receive only an explicitly
requested read token. The native writer checks the current source owner's
authority; an installed App's broader grant does not replace that check.

```yaml
- name: prepare
  on: [schedule, manual]
  steps:
    - name: upgrade
      run: python3 scripts/ci/repository-proposal.py engine-watch --output=.reports/engine-proposal.json
      artifacts: [.reports/engine-proposal.json]
- name: propose
  on: [schedule, manual]
  steps:
    - name: upgrade
      repositoryChange:
        rule: engine-upgrade
        step: prepare.upgrade
        path: .reports/engine-proposal.json
      timeout: 5m
```

The operator stores `MEMQL_PIPELINES_REPOSITORY_CHANGE_RULES` as a system
variable. Its JSON has `schema: 1` and a `rules` list. Each rule names a
`name`, exact `repository` (`owner/repo`), `mode`, `branchPrefix` ending in
`/`, and explicit `paths`. There are no wildcard grants. For example:

```json
{
  "schema": 1,
  "rules": [{
    "name": "engine-upgrade",
    "repository": "example/product",
    "mode": "propose",
    "branchPrefix": "engine-bump/",
    "paths": ["product.env"]
  }]
}
```

`propose` accepts only scheduled or owner-requested runs of the current default
branch. It creates a draft pull request and never merges it. An existing branch
is reconciled only when it identifies the exact prepared commit; a moved branch
or a deliberately closed proposal is not overwritten or reopened.

`update` also requires `author` and `actor`, such as `dependabot[bot]`. It accepts
only a same-repository pull request whose original signed webhook has that
actor, whose current author matches, and whose complete changed-file inventory
fits the rule. Poll-only runs lack this signed actor evidence and cannot update
a pull request. An owner retry retains the original delivery identity. Both
modes recheck the rule, source binding and grant immediately before writing.

The proposal contains `schema: 1`, the exact source `commit`, a short `key`,
`title`, `body`, and `files`. Each file has `path`, `baseSha256`, and UTF-8
`content`. An empty base digest requires an absent file. The limit is 4 MiB
for the proposal, 1 MiB per file and 256 files. Existing regular files retain
their Git mode, including executable files (`100755`); new files use `100644`.
The bounded branch writer supports content replacement, not mode changes.
Deletion, symlinks and workflow changes are not supported. An empty file list records
`unchanged` and mints no writer token.

The driver validates the producer's successful journal, object version, size
and SHA-256 before parsing the proposal. It pins the artifact while an external
write is unresolved. Branch writes use deterministic commits with the exact
source parent, refuse force pushes, and require read-back. A replica recovering
a lost response observes the same commit instead of repeating the branch write.
An unresolved write fails the step and retains its evidence for reconciliation.

### Independent review for ordinary pull requests

Since `0.31.1`, an installed policy can select an `independentReview` recipe
that names a captured review prompt. The engine default leaves this role empty.
The installed workflow calls `pipelineReviewPullRequest` without arguments on
its claimed PR run; the caller cannot choose another repository, commit,
owner or verdict.

The native adapter verifies current repository authority and reads the complete
changed files from immutable Git objects. By default, an ordinary PR receives a
tool-free review of those full bytes through the normal model admission policy.
This creates no change request and grants no authority to edit code, merge,
release or deploy. A governed proposal reuses its original valid review journal;
a rejected, partial or changed native journal cannot fall back to the ordinary
route.

Scheduled review restores the registered source owner's current local identity
before model routing; the scheduler's identity is not a person and cannot open
an App session. The adapter rechecks the active source binding, resolves the
owner's current role, and carries verified owner authority across replicas.
Missing identities and changed or removed source bindings refuse before the
model call. Existing current-provider, lease and policy checks still fence the
review and its report. Restoring identity does not enable an App or bypass its
consent, capacity, model-admission or spending checks. The call stays tool-free
and cannot inherit the draft author's prompt or model override.

An ordinary-review prompt whose first line is
`Input representation: memql.complete-review-hunks.v1` explicitly selects the
versioned complete-hunk format. Native evidence still retains the complete
before/after blobs and their commit, tree, path and mode identities. The engine
includes every changed path and line, including empty files and mode-only
changes, and verifies that the hunks and omitted equal regions reconstruct the
exact source bytes. The model receives the hunks with eight lines of adjacent
unchanged context; overlapping context windows are combined. Line endings and
missing final newlines are preserved.

The paired prompt must state that other unchanged context is omitted. The
reviewer has no tools to retrieve it and must refuse approval when that context
or a dependency is needed to assess the change. This is not a claim that the
model reviewed every unchanged byte. Native capture refuses incomplete,
unsupported or oversized evidence before recording a model attempt. The full
source limits below still apply; the hunk representation is additionally limited
to 512 KiB and its rendered prompt to 1 MiB. Normal model admission and budget
checks remain separate requirements.

Deploy the engine capability before installing its paired marked product
prompt, then qualify fresh reviews under that captured prompt. Unmarked prompts
keep the full-file behavior. Neither deployment automatically changes the
installed review policy, provider selection, authority or GitHub protections.

The run saves its evidence and model attempt before review. Rejection or an
unconfirmed result requires attention, rather than another automatic model call.
An accepted review may report the App-owned `MemQL / review` check for that exact
head after fresh authority checks. The reporting attempt is saved first; an
uncertain response is reconciled by observing that same check, without creating
a second one.

Queue validation resolves every member to its current native or ordinary proof
and checks the complete ordered change in the synthetic merge tree. Unrelated
advances of `main` preserve a review only when ancestry and a disjoint target
change are proved; the reviewed head, merge base, file bytes, prompt, policy and
source authority must still agree. Overlapping changes, additional merge
resolutions or changed membership require fresh evidence. Mandatory tests of
the combined tree remain separate.

Evidence is bounded to 256 changed files, 1 MiB per file and 4 MiB of changed
content. Incomplete trees, unsupported binary/link/submodule changes or
ambiguous history refuse review. First prove actual exact-head reviews and
required tests for an ordinary PR and a governed proposal with the installed
App, keeping current protections. If those protections prevent the first mixed
queue qualification, an operator may then apply a reviewed atomic replacement:
require both the test and review contexts from their exact App IDs while
replacing the superseded review requirement, preserving the queue and all
other protections. Never leave both review gates absent. Qualify the first
actual mixed group only after that transition; missing, moved-head,
changed-membership or unconfirmed evidence must block it. Offline group tests
are not a live queue proof. Installing the engine does not enable this role or
change GitHub protections.

## Release trains

**A release train collects a repository's ready pull requests, tests them
together once, and lands them on the default branch only when the pipeline's
owner approves.** It replaces merging pull requests one at a time. A repository
has trains only when the train settings name it ([below](#turning-trains-on));
without them nothing in this section applies.

Trains are for a repository whose pull requests are all opened under the
owner's own account. GitHub never lets a pull request's author approve it, so
each one merges through an administrator bypass, and the bypass skips the merge
queue: every merge leaves the other open pull requests behind the default
branch, and each runs the whole pipeline again. A train pull request is opened
by the cluster's GitHub App, so the owner can approve it, and one approval
covers every pull request on board.

```mermaid
flowchart LR
    PR["Pull request: its check passes"] --> WAIT["Waits; nothing merges on its own"]
    WAIT --> DEPART["Departure: scheduled or express"]
    DEPART --> BUILD["Train branch: default branch plus each ready pull request, and a version bump"]
    BUILD --> RUN["The train pull request's run"]
    RUN -->|Passed| ASK["ACTION NEEDED: approve the train pull request"]
    RUN -->|Failed| TRIAGE["Retry once, drop one pull request, or hold for the owner"]
    TRIAGE -->|Dropped| BUILD
    ASK -->|Owner approves the current head| LAND["Merged onto the default branch"]
```

### Turning trains on

Trains read the global variable `MEMQL_PIPELINES_TRAINS`: one JSON object,
keyed by repository.

```json
{"repositories":{"acme/shop":{"enabled":true,"departures":["09:00","15:00"],"timeZone":"America/Chicago","channel":"releases","ownerDiscordUserId":"123456789012345678","maxRebuilds":2}}}
```

| Key | Required | Meaning |
|---|---|---|
| `enabled` | no | `true` runs trains for the repository; `false` keeps the entry and runs none |
| `departures` | yes | One or more `HH:MM` times, in `timeZone`, at which boarding closes and a train leaves |
| `timeZone` | yes | An IANA zone name, such as `America/Chicago` |
| `channel` | yes | The name of the pipeline owner's Discord [notification channel](#configure-a-notification-channel) that every train message goes to. It must be active and accept the repository's pipeline |
| `ownerDiscordUserId` | no | The one Discord user (17 to 20 digits) an ACTION NEEDED message mentions. Without it, messages mention no one |
| `maxRebuilds` | no | How many times one departure may rebuild its train, 1 to 5; default 2 |

An unknown key, a malformed time or zone, or a repository that is not
`owner/name` turns trains off **in every repository** until it is fixed: each
tick logs `pipelines: release trains are off until the settings parse` with
the field at fault, and runs nothing. A train also needs the repository's
active pipeline, the GitHub App installation that pipeline uses (the train
writes branches, pull requests, comments and labels, under `contents` and
`pull_requests` write), and an agent replica: the `tickPipelineTrains`
automation runs every minute on the one agent that holds the
`pipelines-trains` lease.

### Boarding

A pull request boards a train when all of these hold:

- it is open, is not a draft, and targets the default branch;
- its head is a branch of this repository: a fork's pull request
  [never runs](#pull-requests-from-forks), so it never boards either;
- the pipeline's pull request check, `MemQL / <name>` written by the
  pipeline's own GitHub App, completed with success on its current head;
- it carries neither `train:hold` nor `train:dropped`.

A pull request whose head branch starts with `train/` is a train's own and
never boards. Ready pull requests board in pull request number order. Each one
left off is listed on the train pull request with its reason, such as
*MemQL / shop has not passed on 3f9c2ab* or *is held with train:hold*.

People and agent sessions open a pull request, get its check green, and stop;
nothing merges it but a train. `train:hold` keeps a pull request off trains
until the label is removed. `train:dropped` is the label a train adds when it
drops a pull request ([below](#when-a-trains-run-fails)); remove it once the
fix is pushed, and the next train can take the pull request.

### Departures

- **Scheduled.** A train leaves at each `departures` time unless the newest
  train has not finished: one train runs at a time, and ready pull requests
  keep collecting. A departure blocked that way leaves on the first minute
  after that train lands or is cancelled. A coordinator that was down across
  several departures runs one train, for the latest of them.
- **Express.** `pipelinesTrainExpress(repository, pullRequests)` starts a train
  at once carrying only the named pull requests, each of which must be ready to
  board. Only the pipeline's owner can start one, holding `execute` on
  `app:pipelines/rerun`. It replaces a train that is waiting for approval or
  held: that train is cancelled, *Replaced by an express train.*, its pull
  request is closed with a comment naming the express train, and its pull
  requests board the next train. It is refused while a train is building or
  running.
- **Nothing ready.** A departure with no ready pull request is recorded as a
  cancelled train, *No pull request was ready to board.*, and sends nothing.

A train's branch is `train/<yyyy-mm-dd>-<n>`: the date in `timeZone`, and the
day's count of that repository's trains.

### Building the train

Acting as the GitHub App, with an installation token minted through the
pipeline owner's GitHub connection (contents and pull requests write, checks
read), the train:

1. creates its branch at the default branch's head (on a rebuild, resets it
   there);
2. merges each boarding pull request's head into it on GitHub's side, in
   number order, with nothing checked out. A pull request that conflicts with
   the default branch, or with one merged onto the train before it, stays off
   and gets a comment saying so;
3. commits the version bump: the patch number of the repository's `VERSION`
   file plus one, unless a pull request on board already changed it. A
   repository with no `VERSION` file, or one that is not `MAJOR.MINOR.PATCH`,
   gets no bump;
4. opens the train pull request, *Train train/\<date\>-\<n\>*, listing the pull
   requests on board (number, title and head commit), those left off or dropped
   and why, and the version. A rebuild updates the same pull request.

The train pull request runs the pipeline as any pull request does and reports
the same `MemQL / <name>` check.

### States

| State | Meaning |
|---|---|
| `building` | Its branch and pull request are being made. A build belongs to the tick that claimed it for 10 minutes; one older than that was abandoned, and the next tick builds it again |
| `running` | The train pull request's run is under way |
| `awaiting_approval` | The run passed; the owner decides |
| `landing` | Approved, and being merged |
| `landed` | On the default branch |
| `held` | Needs the owner; the reason is on the record and in Discord |
| `cancelled` | Its pull request was closed, an express train replaced it, or nothing was ready. Its branch is deleted, which closes a pull request still open, after a comment saying why |

Each train is a `v1:pipelines:train` row, read with `pipelineTrainsForOwner`.
**The row is the state, not the process.** Each one-minute tick reads it afresh
and takes one step, and every step is a compare-and-swap under a Postgres lock
held per repository: a tick that moves to another agent replica resumes from
the row, and two ticks never act twice on one state.

### When a train's run fails

The train decides, in this order:

1. **The machinery failed: retry once.** When every failed step reports
   `pipeline_step_timeout`, `pipeline_runner_unavailable`,
   `pipeline_executor_error`, `pipeline_image_pull_failed` or
   `pipeline_clone_failed`, the failed steps run again once, as a failed-only
   re-run does. A failed test is never retried.
2. **The default branch fails too: hold.** When the default branch's newest
   completed push run failed one of the same steps, no pull request on the
   train is to blame, and the train is held. Fix the default branch first, for
   example with an express train carrying the fix.
3. **The cap: hold.** After `maxRebuilds` rebuilds in one departure, the train
   is held.
4. **One suspect: drop it.** Each failed step names the Go packages it tested.
   A pull request on board that changed files in one of those packages, or in a
   directory above one, is a suspect, and so is one whose changed files could
   not all be listed. Exactly one suspect is dropped: it gets `train:dropped`
   and a comment saying why, and the train is rebuilt without it and runs
   again. Anything else holds the train, because the train never drops a pull
   request it cannot single out. A failed step that tested no package names no
   suspect.

A run that failed with no failed step to blame, such as a refusal, holds the
train with the run's own reason.

**A held train waits for the owner.** Closing the train pull request cancels
the train, and its pull requests board the next one. Adding `train:hold` to a
pull request on board rebuilds the train without it, and so does the default
branch moving: a train held because the default branch failed is rebuilt by
itself once a fix lands there.

### Approval and landing

The owner approves the train pull request on GitHub. Every minute, the train
reads that pull request's reviews: the latest `APPROVED` or
`CHANGES_REQUESTED` review by the pipeline owner's GitHub login, learned from
their GitHub connection, decides, and it lands the train only when it approves
the train pull request's **current head**. An approval given before a rebuild
does not land the rebuilt train. Nothing in the train can create a review: a
model never approves.

To land, the default branch must still be the commit the train was built on.
The App then merges the train pull request with the `merge` method, guarded by
the approved head, under the branch's normal rules and with no bypass. If the
default branch moved, the train is rebuilt and asks again; if GitHub refuses
the merge, the train is held with GitHub's status. After landing, each pull
request on board gets a comment naming the train and the merge commit, and the
train branch is deleted. A train pull request someone merged by hand is
recorded `landed`.

While a train waits for approval, adding `train:hold` to one of its pull
requests, or the default branch moving, rebuilds it, and the owner is asked
again. A train still waiting when the next departure time passes sends one
reminder.

### Discord messages

Every train message goes to the settings' `channel` through the cluster's
outbound worker, which owns delivery, retries and the receipt. Messages come
from *MemQL Pipelines* and link the train pull request and the run's page in
MemQL OS.

| Event | Kind | Says |
|---|---|---|
| Departed | Status | The pull requests on board, and those left off and why |
| Dropped | Status | The pull request dropped, and why |
| Awaiting approval | ACTION NEEDED | The train passed: approve the train pull request |
| Held | ACTION NEEDED | Why the train is held, and its failed steps |
| Reminder | ACTION NEEDED | The train has waited past a departure for approval |
| Admission held | ACTION NEEDED | The train's run has been queued for 15 minutes while [run admission](#operator-run-admission) holds new runs |
| Landed | Status | How many pull requests landed, and the merge commit |

**Only ACTION NEEDED messages mention anyone, and only `ownerDiscordUserId`.**
The mention is the message's text, with `allowed_mentions` naming that one
user, because Discord never notifies a mention inside an embed. Text from pull
requests and commits is escaped, so a title cannot mention anyone. Each message
is sent once per event and train head, and one that cannot be sent is logged
and never stops the train. A pull request's own results stay on GitHub: it
appears in Discord only when it is left off or dropped.

### Moving a repository to trains

1. Create the Discord channel and allow the pipeline on it.
2. Make the owner the code owner of every path, so the owner's approval is the
   review GitHub requires on a train pull request.
3. Require the pipeline's pull request check, `MemQL / <name>`, on the default
   branch, and remove its merge-queue rule: the train does the batching.
4. Add the repository to `MEMQL_PIPELINES_TRAINS` with `enabled: true`. The
   next departure is the first train.

Keep the administrator bypass, for emergencies only. While a repository moves
to trains, its default branch can require the push run's check,
`MemQL / <name> / push`, instead: a release can then be cut from the default
branch's own run while pull requests still merge one at a time. The push check
never runs on a pull request, so require `MemQL / <name>` again when trains
start; a train pull request's run reports it. To fall back, set `enabled` to
`false`, restore the saved branch rules, and merge one pull request at a time.

### Not yet

- **Splitting.** A failure with several suspects holds the train; it does not
  yet split them to find the cause.
- **A shorter pull request check.** Every pull request, and every train pull
  request, runs the pipeline as its manifest selects it.
- **Reusing the train's result.** The default branch's push run tests the
  landed commit again, though its tree is the one the train tested.
- **Review events.** Approval is read every minute rather than delivered.
- **Releasing from the train.** Cut a release from the default branch after a
  train lands, as before.

## Connecting a pipeline

**Who:** the source's owner, holding `execute` on `app:pipelines/connect`.
**How:** in MemQL OS, open **Pipelines → Repositories**, select the repository,
and choose **Connect pipeline**
([below](#in-memql-os)); or the `pipelinesConnect` builtin.

Before connecting:

1. **The cluster has a GitHub App that holds checks write.** See
   [GitHub Connect](github-connect.md#creating-the-github-app); an app
   registered before pipelines needs
   [the upgrade](github-connect.md#upgrading-an-app-registered-before-pipelines).
   Webhook delivery also needs the app's webhook on and subscribed to the five
   events that page lists.
2. **The source is a GitHub repository fetched through your GitHub
   connection**, not a pasted token. A pipeline acts through the app's
   installation, which a pasted token does not have: switch the source to the
   connection first.
3. **`memql-package.yaml` at the default branch's head has a `pipeline:` block
   that validates** ([the block](#the-pipeline-block)).

Then:

```
builtin pipelinesConnect(packageId: "<package id>", delivery: "webhook")
```

or from an SDK: `pipelinesConnect({ packageId, delivery })` on the TS
`QueryClient`, `PipelinesConnect` in `sdk/go/client`.

| Argument | Values | Meaning |
|---|---|---|
| `packageId` | required | The source to connect. It must be yours |
| `delivery` | `webhook` or `poll`, required | How changes reach the pipeline ([Delivery](#delivery-webhook-or-poll)) |
| `compute` | `cluster`, the default, or `cluster_and_fleet` | Where steps may run ([Compute](#compute)) |
| `secretNames` | a list of names | The secrets steps may resolve: the owner's allowlist ([Secrets](#secrets)). A name beginning `MEMQL_` is refused |

Before any request leaves the cluster, connect refuses a repository another
source's pipeline already runs, `pipeline_already_connected`: a repository has
one pipeline, so its checks are reported once per commit. The remedy is to
disconnect the other source's pipeline -- its owner does, with
`pipelinesDisconnect`, or a cluster owner does when that owner has left -- or to
work from that source. The refusal says that such a pipeline exists and nothing
about whose it is; a disconnected pipeline blocks nothing.

Connect then proves the source's connection still reaches the repository by
minting an installation token through it, reads the default branch's head and
the manifest there, and validates the block. A source with no block is refused
`pipeline_not_declared`, and a block that does not validate is refused with its
own code ([Refusal codes](#refusal-codes)), so a block that can never compile is
never connected; a manifest that does not parse at all is refused
`package_manifest_invalid`, as it is for a deploy. Connect also asks what this
connection allows of every step, whatever event would plan it: a step naming a
need on a `cluster` pipeline, or a secret `secretNames` does not hold, is
refused with its own code now rather than on the first push that reaches it,
and every run asks again of the stages it runs ([Secrets](#secrets),
[Compute](#compute)). The connection's own failures are the ones a fetch gives
-- `credential_not_found` (the source fetches under a pasted token, or names no
connection), `credential_revoked`, `reconnect_required`,
`repository_not_installed`, `installation_pending`,
`github_app_not_configured` -- as
[GitHub Connect](github-connect.md#what-a-person-sees) describes them.

It answers `{pipelineId, repository, delivery, compute, stages}`, the stages
being the block's stage names in order. **Connecting runs nothing:** the first
run comes with the next push or pull request.

Afterwards:

- **Reconnect** with the same call. The pipeline keeps its id; the call restates
  delivery, compute and the allowed secret names and reactivates a
  disconnected pipeline, while what the poll has seen and the timing table
  carry over. Pass the whole configuration each time. Reconnecting is refused
  `pipeline_already_connected` when another source's pipeline has taken the
  repository meanwhile, as any second pipeline is. Reconnect, too, after
  reinstalling the GitHub App on the repository or renaming the repository:
  deliveries are matched on the installation and the repository name a
  pipeline was connected with ([Delivery](#delivery-webhook-or-poll)).
- **Disconnect** with `builtin pipelinesDisconnect(pipelineId: "<pipeline id>")`
  (`execute` on `app:pipelines/connect`). It opens no more runs, and a run no
  agent has started yet concludes `pipeline_disconnected`. The pipeline and its
  runs stay, as the history, and the repository is free for another source's
  pipeline.
- **Re-run** with `builtin pipelinesRerun(runId: "<run id>")` (`execute` on
  `app:pipelines/rerun`): the next attempt of the run's key, in the
  original's mode and event, with a new check run. The original stays as it
  ended.
- **Re-run failed** with `builtin pipelinesRerun(runId: "<run id>", failedOnly:
  true)`: the same new attempt, running only what the original did not pass.
  Every step the original passed **with the same package slice** is carried
  over as skipped `pipeline_passed_earlier` (*Passed in attempt 1.*), and
  everything else runs. A shard whose slice moved since -- the timing table
  learned between the attempts -- runs again, because a pass over other
  packages is not a pass of these. A notify stage is never carried: it
  announces the new attempt, which has an outcome of its own. A run with no
  failed or cancelled step (one that passed, or one refused before any step) is
  refused `pipeline_nothing_to_rerun`. GitHub's own re-request stays a whole
  re-run: one check run reports the whole run.
- **Cancel** with `builtin pipelinesCancel(runId: "<run id>")` (`execute` on
  `app:pipelines/cancel`). It flags the run; the agent driving it cancels
  what is executing at its next heartbeat and concludes the run cancelled. A
  queued run no agent has claimed is concluded cancelled at once. A new push
  to a pull request cancels the pull request's earlier runs the same way
  ([Cancel-on-push](pipelines-substrate.md#cancel-on-push)).

Each acts on your own pipeline or run only. A caller who does not own it is
refused by name before anything is written. The one exception is disconnect: a
cluster owner may disconnect any pipeline, so an operator can free a repository
whose pipeline belongs to somebody who has left. It writes the pipeline's
status and nothing else.

**Preview** with `builtin pipelinesPreview(packageId: "<package id>")`
(`execute` on `app:pipelines/connect`) to read what connecting would act on,
writing nothing: the same read connect makes -- the grant proved by a mint, the
default branch's head and the block there, validated -- answered as the stages
and steps (their shape, never a command), the needs and secrets they name, the
check's name, the source's existing pipeline for a reconnect, and a delivery
suggestion (webhook where GitHub can plausibly reach the cluster, poll
otherwise). A typed refusal is the answer's `refusal`, not an error.

## In MemQL OS

The **Pipelines** app owns **Overview**, **Repositories**, **Changes**,
**Runs**, **Releases**, **Logs** and **Settings**. Deployables shows source
context and links to Pipelines; it manages deployed applications. Cluster
**Updates** owns this installation’s selected release. Preparing a plan never
applies it. If preparation is interrupted, **Resume preparation** continues the
saved request after a reload. Before applying, its original operator can choose
**Choose a different release** and confirm cancellation to release that plan.
Once application has started, cancellation is unavailable; progress reads keep
checking the same installation attempt. A repository or proposal outage is shown
separately; saved preparation, plan cancellation and installation progress stay
available under the original native permission checks.

**Add repository** uses a left-step wizard: GitHub account, repository, MemQL
organization, then review. Repository details use local tabs for **Details**,
**Change requests**, **Checks**, **Deployables** and **Settings**. Change
requests read fresh repository authority; being an owner or developer of this
installation does not grant write access to an upstream repository. Requests
name the intended change and allowed repository paths. The local maintainer
can open their Nexus goal or approval; other requesters see the saved request
and response. Draft-only work never grants queue permission. Delivery requests
use the separately declared review and queue gates; neither option grants
release publication or installation authority.

- **The repository’s Checks tab** shows the newest run on each branch, the default branch first,
  each opening its run. Its facts gain a **Pipeline** line (the manifest, how
  many stages the newest run planned, how changes arrive, where steps run), and
  **Latest upstream** says what that commit's checks said -- *checks passed,
  not yet deployed* among them. The bar reads *checks on* and carries **Pipeline
  settings**, or **Connect pipeline** for a source that has none. A connected pipeline offers **Run**, which asks for a branch and defaults to
  the default branch. A disconnected pipeline offers **Run latest**, which
  runs the default branch, or **Other branch**, which asks for the branch under
  the facts: a list of the repository's other branches when the source's grant
  can read them, a text field when it cannot. Either is `pipelinesRunLatest`,
  whose optional `branch` names the branch (empty is the default branch); a
  name GitHub would not accept is refused `pipeline_branch_invalid` before any
  request leaves the cluster. MemQL reads the branch through that source's
  installation grant and pins the returned commit SHA. Automatic delivery stays
  as configured; the manual event has its own deduplication key. Only
  stages with no `on` selector or with `on: [manual]` run; publication stages
  should name their intended events explicitly. A manual run writes the
  pipeline's separate `/ manual` check on its commit, so a branch's run
  shows on that commit and on the pull request whose head it is. Repeating the
  request for the same head returns the same run, whichever branch reached it;
  use **Re-run** after it finishes to create another attempt. A
  source that produces no apps and runs a pipeline -- the engine repository is
  one -- reads *pipeline* where its app count goes in the Repositories list, and the
  Overview map draws a **Checks** node beside the deployables a checked source
  serves.
- **Connect pipeline** uses the same left-step wizard pattern as Fleet setup,
  with three steps. **Repository** reads `memql-package.yaml` at the default
  branch's head with `pipelinesPreview`, so the stages show before anything is
  confirmed. **Compute** offers *Cluster* and, only when one of your machines
  reports `pipelines=allowed`, *Cluster and your fleet* -- the fleet alone
  when a step names a need, which the cluster refuses -- and waits for a
  choice, because running steps on your own machines is consent. **Confirm**
  names the check, the stages and the secrets the steps read, and asks how
  changes arrive. A refusal lands at the stop it is about, with its remedy: a
  fleet code at Compute, a secret code at Confirm, everything else at
  Repository. Nothing is written until Connect, and leaving keeps the answers.
- **Runs** is a Pipelines section (`read app:pipelines/runs`):
  every run of every source you connected, newest first, grouped by day,
  Refine for the source, branch and outcome. A run rings once, when it has an
  answer.
- **A run’s page** draws the stages down the left, or across a narrow window,
  read from the step rows; the open stop's steps say where each ran and for how long; a failed
  step shows its last lines -- read from its full log, which the runner keeps in
  your Library -- with **Open the full log**; a skipped step says why; a run
  that failed with no step at fault says why above the stages
  ([A run can fail with every step passed](#a-run-can-fail-with-every-step-passed)); the
  artifacts open in Files. One bar carries the state and the acts the run
  allows: **Open on GitHub**, **Re-run**, **Re-run failed**, or **Cancel** while
  it runs. The check run's details link, `https://os.<domain>/?pipelineRun=<run
  id>`, opens it.
- **Pipelines → Settings → Infrastructure** (`read app:settings/pipelines`,
  the cluster owner) holds the [readiness](#readiness) report. Connections and
  notification channels are under **Settings → Connections**.

---

## The `pipeline:` block

The block sits beside `deployables:` in [the manifest](packages.md#the-manifest);
a repository with checks and no apps declares it alone. An example -- the
pipeline the design record sketches for MemQL's own repository:

```yaml
formatVersion: 1
name: memql
pipeline:
  image: ghcr.io/znasllc-io/memql-toolchain@sha256:...
  services:
    postgres:
      image: ghcr.io/znasllc-io/timescaledb-pgvector@sha256:...
      env:
        POSTGRES_PASSWORD: postgres
      ready: pg_isready -U postgres
  caches: [go, npm]
  select:
    go: import-graph
    dbGated: [component/memql, component/database]
    buckets:
      gates: ["**"]
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
        - name: source-contracts
          run: go test -count=1 ./ ./integrations
          when: { bucket: gates }
    - name: deploy
      on: [push]
      steps:
        - name: verify-rollout
          run: ./scripts/verify-rollout.sh --target=https://api.<domain> --version="$MEMQL_VERSION"
          secrets: [VERIFY_TOKEN]
    - name: notify
      on: [push]
      channel: znas-instance
```

Connected with `secretNames: [VERIFY_TOKEN]`, it runs `checks` and then `tests`
on every run, and `deploy` and `notify` only on a push to the default branch.
`dbGated` is what `only: db-gated` reads, and the block is refused without it;
the sidecar's `env` and `ready` are what a Postgres image needs to start and to
be waited on.

**Two checks, at two times.** The block is read by the manifest's one strict
decoder, so a misspelled key anywhere inside it -- `shardz:`, `cachez:` --
refuses the whole manifest with `package_manifest_invalid`, the deploy as well
as the run, exactly as `deployabels:` does. What the block *means* -- a need
nobody offers, a bucket nobody declared, a timeout that is not a duration -- is
checked when a run compiles, and fails only that run, with a `pipeline_*` code:
a mistake of meaning never refuses a deploy of the source it sits in.

The meaning is checked at connect, against the default branch's head, and again
on every run, against the run's own commit, because a pull request can change
its own pipeline. A run reads the whole block, including stages it will not
execute -- a typo in the deploy stage fails the pull request that makes it, not
the push after it lands -- and reports the first problem in a fixed order: the
services, the `select` block, then each stage and its steps as written. What
depends on the pipeline's own settings, an allowed secret or consent to the
fleet, is asked only of the stages the run executes.

### Names

Every name the block declares -- a stage, a step, a service, a bucket, a
channel -- is lower-case letters, digits and hyphens, starts with a letter or a
digit, and is at most 40 characters. A name is part of a step's key and of a
refusal's scope, so it never holds a dot, a slash or a hash.

### The block's keys

| Key | Value | Default | Refused when |
|---|---|---|---|
| `workflow` | Optional assertion of the operator-bound workflow name | Operator-bound workflow | A different name cannot override the installation binding; execution refuses it |
| `platform` | Default container platform (`linux/amd64` or `linux/arm64`); a step may override it | either Linux architecture on the cluster | Unknown OS/architecture: `pipeline_step_invalid` |
| `image` | The toolchain image container steps run in. Pin it by digest | none | -- |
| `services` | Named sidecars a step may ask for, each with `image` (required), `env` (plain `NAME: value` configuration, never a secret) and `ready` (a shell probe the runner waits on before the step's command starts) | none | A name that breaks the rule, a service with no image, or an `env` name that is not upper-case letters, digits and underscores starting with a letter or underscore: `pipeline_step_invalid`, scoped `services` or `services/<name>` |
| `caches` | Default caches inherited by steps, such as `go` and `npm` | none | -- |
| `select` | How steps choose what to run. Required once a step names `packages` | none | [Below](#select) |
| `stages` | The stages, in the order they run. At least one | required | Absent or empty: `pipeline_stage_invalid` |

The operator chooses installed delivery policy with
`MEMQL_PIPELINES_POLICY_RESOLVER`. The named pure template receives `repository`,
`ownerUserId` and `pipelineId`. It returns `{}` for portable defaults, or a complete
object of installed template names for these roles:

| Role | Purpose |
|---|---|
| `workflow` | Stage order, bounded parallel work and failure blocking |
| `stageIncluded` | Event, component and schedule selection |
| `packageSelection`, `packageStepSelection`, `bucketIncluded` | Affected package coverage |
| `eventMode`, `versionForEvent` | Event admission, coverage mode and artifact version |
| `notificationCopy`, `notificationOutcome` | Notification text and settlement |
| `artifactRetention` | Retention decisions |

An unset or explicitly empty resolver selects the portable policy. Since
`0.31.1`, a missing optional variable row has the same meaning as an empty
value. A failed settings read, an invalid configured resolver or an incomplete
role map still stops intake before any run or GitHub check is created. If a
verified webhook has no check, inspect its intake journal before redelivering
it; a settings failure is different from a failed test run.

Optional roles are `supersession`, `reconciliation`, `maintenance`,
`evidenceReuse` and, since `0.31.1`, `independentReview`. Their default behavior
preserves affected-PR supersession, cancels an invalidated queue ref, and performs
no repair, evidence reuse or ordinary-PR AI review.
A resolver cannot change merge-group coverage mode away from `full` or grant
repository write access.
Source manifests declare commands and artifacts; they cannot replace this binding.

Every new run saves the complete reachable policy source and its fingerprint
before creating its GitHub check. Redelivery, retry and recovery use that saved
closure, including on another replica with a newer installed bundle. Selection
has no native effects. The stage selector additionally receives `stage`,
`repository`, `sha` and the original `releaseTag`, so independently versioned
components can share a checkout without sharing their publication tags.
An installed stage selector can additionally declare `pullRequest object!` to
opt into provider-verified selection facts. For a PR event the engine reads the
current PR through the source owner's connection and verifies the exact PR
number, open state, repository, branch and head commit before allocating work.
Its argument contains `present: true`, `number`, `state`, `author`, `headRef`,
`headRepository`, `headSha` and `baseSha`. Other events receive only
`{present: false}`. A selector without this argument keeps its existing behavior
and does not require this provider read.
Install engine support before a policy that requires this argument: an older
engine refuses the missing argument instead of presenting unverified facts as a
decision to skip maintenance.

The observation is saved with the run, source owner, credential, installation
and captured policy version under the current driver's lease. Recovery on a
different replica reuses that exact observation. An unavailable provider, moved
PR, mismatched source, cancellation or lost claim refuses selection rather than
turning unknown facts into a passing empty pipeline. These are planning facts;
repository writes still require their own fresh authority and source checks.

Stage selectors may return a boolean, or exactly
`{included: false, reason: "No maintenance is needed for this change."}`. The
reason must be nonempty plain text, at most 512 characters with no control
characters. The engine records it once per stage as a run note, preserving
unrelated notes, and refuses a changed outcome or explanation on recovery. An excluded
stage creates no execution job. Installed DSL decides which authors, branches
or stages apply; source manifests cannot provide verified facts or replace this
policy.
Native code retains leases, current authority, immutable receipts and the final
verdict. See [Pipeline workflows](../build/integration-boundary.md#pipeline-workflows).

### Reusing verification evidence

A command may explicitly declare `verification: true` when its inputs completely
determine the check. Such steps must use digest-pinned cluster containers and
services, and cannot request secrets, repository tokens, artifacts or native
fleet execution. Run-, event- and version-dependent environment variables are
removed. The declaration is an author assertion: network-dependent or otherwise
nondeterministic commands should not use it.

The installed `evidenceReuse` selector can admit a successful full run's directly
completed receipt at the same commit, owner, source, engine revision, policy
fingerprint, command definition and package slice. Evidence older than 24 hours
is refused. Scheduled scans and explicit retries always execute. Artifact
production and publication always retain their own receipts; a reused test does
not stand in for either. Equal trees with different commit hashes are currently
checked separately.

### Recovery and maintenance

Recovery reads GitHub's current queue ref before cancelling work whose merge
group disappeared or moved. An unavailable provider response leaves it running.
Supersession cannot cancel unrelated full runs or releases.

After a failed run, the frozen `maintenance` policy returns `none`, `human`, or
`analysis` with an installed diagnosis template and an explanation. Analysis
opens one durable owner-scoped goal keyed to the failed run; a lost response or
another replica resumes that same request. Failed retries escalate to a person.
Analysis receives facts and ordinary Work authority, never a repository writer
or installation permission. Its state, explanation and linked goal are recorded
on the pipeline run. Installation of a verified release remains a separate,
explicit selection.

`image` and `caches` are passed to the runner as written; what each cache name
mounts is the runner's ([Caches](pipelines-substrate.md#caches)): it knows
`go` and `npm`, and a step declaring any other fails `pipeline_job_rejected`.

### Scheduled scans

Set `pipeline.schedule` to `daily` or `weekly` and opt each audit stage in with
`on: [schedule]`. A stage may override the pipeline cadence with its own
`schedule`. Other declared events remain independent: a weekly audit can also
run on a release or a manual qualification.

The daily scheduler uses UTC. A weekly period starts Monday; if Monday is
missed, the next invocation catches up. The first weekly opening pins that
week's commit, and later main pushes do not repeat it. A failed weekly scan
remains visible until explicitly rerun; its retry keeps the same commit and
period. Daily and weekly checks have `/ daily` and `/ weekly` suffixes, so they
cannot satisfy the required PR or merge-queue check.

### `select`

| Key | Value | Refused when |
|---|---|---|
| `go` | `import-graph`, the one Go selection there is | Any other value: `pipeline_select_invalid`. Absent while a step names `packages`: `pipeline_select_missing` |
| `dbGated` | Repository directories whose Go packages need a database, such as `component/memql`. `.` is the root package alone | An entry that is empty, absolute, holds `..` or a glob character, or names a directory starting with `.` or `_`: `pipeline_select_invalid` |
| `full` | Globs whose change selects every package, beside the built-in ones ([Which packages a step tests](#which-packages-a-step-tests)) | A glob that cannot be read, or a list of only `!` globs: `pipeline_select_invalid` |
| `buckets` | Named lists of globs a step gates on with `when: { bucket: <name> }` | A name that breaks the rule, an empty list, a glob that cannot be read, or a list of only `!` globs: `pipeline_select_invalid` |

### A stage's keys

| Key | Value | Refused when |
|---|---|---|
| `name` | The stage's name | Missing, breaking the rule, or used twice: `pipeline_stage_invalid` |
| `needs` | Earlier stages this one depends on. Recorded rather than scheduled on: stages run in the order written whatever it says | A stage that comes later, the stage itself, or no stage at all: `pipeline_stage_invalid` |
| `schedule` | Override the pipeline cadence with `daily` or `weekly` | Requires a pipeline schedule and `on: [schedule]`; otherwise `pipeline_stage_invalid` |
| `on` | The events (`pull_request`, `merge_group`, `push`, `release`, `schedule`, `manual`) and modes (`affected`, `full`) the stage runs for. Absent means every non-scheduled run, including a manual rehearsal | Anything else: `pipeline_event_unknown` |
| `runAfterFailure` | Boolean, default `false`. Run this stage even when an earlier stage failed; useful for independent security analysis. Does not erase failures or bypass cancellation | Non-boolean values fail manifest parsing |
| `steps` | The stage's steps, which run at once | Both `steps` and `channel`, or neither: `pipeline_stage_invalid` |
| `channel` | Makes the stage a notify stage naming a channel. It carries no steps, and compiles to one step, `<stage>.notify` | A name that breaks the rule: `pipeline_stage_invalid` |

### A step's keys

| Key | Value | Default | Refused when |
|---|---|---|---|
| `name` | The step's name, unique in its stage | required | Missing, breaking the rule, or used twice in the stage: `pipeline_step_invalid` |
| `execution` | `container` or `native`; native uses the host toolchain and ignores the pipeline image | `container` | Native steps with services or shared caches: `pipeline_step_invalid` |
| `githubReadToken` | Give a command `MEMQL_GITHUB_READ_TOKEN`, or an image build the BuildKit secret `github_read_token`, for this repository only | false | Cluster container commands and image builds with an installed source repository. Fixed permissions: contents, checks and pull requests read. It cannot write or reach other installation repositories; queued grants refresh before admission and logs redact them |
| `checkoutHistory` | Set `full` to fetch complete commit ancestry, tags and the comparison base during the trusted checkout | shallow | Cluster container steps only. The command receives history without the installation token; it should scan that local history without fetching again. The existing run deadline and workspace limits bound checkout |
| `memoryMiB` | Explicit memory request and limit for the cluster command container, in MiB; zero keeps namespace defaults | `0` | Nonzero values below 128, above 1048576, or on fleet/native steps are invalid. The Workbench separately refuses values above its operator-configured maximum (8192 MiB by default) before creating a Job |
| `cpuMilli` | Explicit CPU request and limit for the cluster command or image-build container, in millicores; 1000 means one CPU. Zero keeps namespace defaults for commands and the fixed request of one CPU / limit of two CPUs for image builds | `0` | Values below zero, above 256000, or nonzero values on fleet/native steps are invalid. The Workbench separately refuses values above its operator-configured maximum (4000 by default) before creating a Job. Scheduling accounts for the whole request; it does not silently fall back to a smaller allocation |
| `image` | Container image for this step; pin by digest when reproducibility matters | pipeline image | Explicit image on a native step: `pipeline_step_invalid` |
| `placement` | `cluster` or `fleet`, independently of execution | fleet for native execution or host needs, otherwise cluster | Native execution on the cluster, unknown placement, or fleet without consent |
| `platform` | Step OS/architecture; overrides the pipeline platform | pipeline platform | Fleet requires an explicit platform; containers require Linux |
| `imageBuild` | One rootless Dockerfile build: `{ context, dockerfile, target?, args? }`; produces an OCI archive and metadata | none | Requires explicit Linux architecture, cluster placement, memory at least 512 MiB and the operator profile. No command, image override, services, caches, secrets, pull credential, package selection or artifact override; see [Image builds](pipelines-substrate.md#image-builds) |
| `run` | A shell command, run in a fresh working copy of the commit -- the same contract as a deployable's `build.command`. There is no step language | required unless `imageBuild` is present | Blank without `imageBuild`: `pipeline_step_invalid` |
| `packages` | `affected` or `all`: select Go packages into `MEMQL_PACKAGES` | none: the step selects no packages | Another value: `pipeline_step_invalid`. No `select.go`: `pipeline_select_missing` |
| `only` | `db-gated` or `not-db-gated`: keep the selected packages under a `select.dbGated` tree, or the rest | none | Another value, or set without `packages`: `pipeline_step_invalid`. No `select.dbGated`: `pipeline_select_invalid` |
| `shards` | Split the packages over up to this many steps, at most 8. 0 and 1 mean one step | one step | Below 0 or above 8, or above 1 without `packages`: `pipeline_step_invalid` |
| `when` | `{ bucket: <name> }`: skip the step on a pull request that touches nothing in the bucket | always runs | A bucket `select.buckets` does not declare: `pipeline_bucket_unknown` |
| `needs` | `{ <need>: true }`, the need one of `display`, `docker`, `gpu`, `macos_tooling`, `user_files`: a native step needs a fleet machine that offers it. Host needs cannot pass through a container boundary; use `placement: fleet` to request an ordinary Docker container on a worker. A need set `false` is the same as none | runs on the cluster | A need outside the set: `pipeline_need_unknown`. Any need on a pipeline whose compute is `cluster`: `pipeline_fleet_not_consented` |
| `services` | Declared services the step runs beside | none | A name `services` does not declare: `pipeline_service_unknown` |
| `caches` | Overrides the pipeline's cache list; `[]` disables inherited caches, including for a native image build beside container test steps | pipeline caches | Native steps with nonempty effective caches: `pipeline_step_invalid` |
| `timeout` | A duration such as `20m` or `1h30m`, from 1 minute to 6 hours. The run ceiling remains an independent bound. A step still running at its timeout is stopped and fails `pipeline_step_timeout` | `20m` | Not a duration, under `1m` or over `6h`: `pipeline_step_invalid` |
| `artifacts` | Paths the runner saves as Library files owned by the pipeline's owner ([Artifacts](pipelines-substrate.md#artifacts)) | none | -- |
| `secrets` | Names of secrets the step's environment receives, each under its own name ([Secrets](#secrets)) | none | A name that is not upper-case letters, digits and underscores starting with a letter (at most 128 characters), or that begins `MEMQL_`: `pipeline_secret_invalid`. A name the pipeline does not allow: `pipeline_secret_not_allowed` |
| `imagePullSecret` | Name of an owner-allowed Docker config JSON secret for the step and service images; cluster container steps only | none | The same name/consent rules as `secrets`; declaring it as an environment secret or using fleet/native execution is refused |

Two keys share the word `needs`: a stage's names earlier stages, a step's names
what the machine running it must offer.

### Globs

A bucket's globs and `select.full`'s are anchored at the repository root:

- `**`, as a whole path segment, matches zero or more directories. `dsl/**`
  matches `dsl` itself and everything under it; `**/*.md` matches `README.md`
  and `docs/a/b.md`. `**` mixed into a segment is refused.
- `*` matches within one segment, and `?` matches one character within one
  segment. A dot is an ordinary character.
- `[`, `]`, `{`, `}`, `(`, `)`, `+`, `@` and `\` are refused rather than
  guessed at, and so is a `!` anywhere but first.

**A `!` glob means except.** A path is in the list when one of its plain globs
matches it and none of its `!` globs does, in any order:

```yaml
    buckets:
      os: ["clients/**", "!clients/**/*.md"]
```

is every change under `clients/` except its Markdown. A list of only `!` globs
includes nothing and is refused. This is not how `dorny/paths-filter` reads a
list -- it ORs the patterns, so its `!` widens a filter -- which matters when a
bucket is copied from a GitHub Actions workflow: its plain globs mean what they
meant, and its `!` globs now narrow it.

### What a step's command sees

Every runner exports the same environment to a step's command:

| Variable | Value |
|---|---|
| `MEMQL_RUN_ID` | The pipeline run's id |
| `MEMQL_WORK_RUN_ID` | The work run it compiled into |
| `MEMQL_STEP` | The step's key: `<stage>.<step>`, with `#<i>` for a shard |
| `MEMQL_REPOSITORY` | `owner/name` |
| `MEMQL_SHA` | The commit under test |
| `MEMQL_BASE_SHA` | The event's pinned comparison base; empty for an event without a predecessor |
| `MEMQL_MODE` | `affected` or `full` |
| `MEMQL_EVENT` | `pull_request`, `merge_group`, `push` or `release` |
| `MEMQL_VERSION` | The release's tag on a release, the commit otherwise |
| `MEMQL_PACKAGES` | The step's Go import paths, separated by spaces. Empty for a step that selects none |
| `MEMQL_SHARD` | `<i>/<k>` on a shard, absent otherwise |
| `MEMQL_DOMAIN` | The cluster's front-door domain, through which a step reaches `api.<domain>`, `identity.<domain>` and `os.<domain>` from outside, as any client does. Absent when the cluster has none configured |

Each allowed secret is added under its own name. The platform's names win over
a secret of the same name, which the manifest refuses anyway.

---

## How a run is chosen

### Events and modes

The portable policy selects these modes. An operator-bound installed policy may
change event selection within the native trust and coverage constraints.

| GitHub delivers | Opens | Mode |
|---|---|---|
| `pull_request` opened, synchronize or reopened | a run for the pull request's head | affected |
| `merge_group` checks_requested | a run for the merge group's head | full |
| `push` to the default branch | a run for the pushed commit | full |
| `release` published | a run for the commit the tag names | full |
| `check_run` or `check_suite` rerequested | a new attempt of the run it names | the original's mode and event |

*Affected* selects packages and path buckets touched by the change. *Full*
selects every package within each applicable step's declared scope, on the
exact commit being qualified. Event and component selectors still decide which
stages apply; full coverage does not mean every release artifact is published.
Eligible verification receipts may satisfy an opted-in check without executing
it again, as described in [Reusing verification evidence](#reusing-verification-evidence).
A pull request can therefore pass while its merge group fails: the queue checks
the combined commit.

Every other delivery opens nothing: a push to another branch (its pull request
is what runs), a tag push, a deleted branch, a pull request closed or edited, a
review, a ping, an installation event, and any delivery that did not come
through the GitHub App's installation ([Delivery](#delivery-webhook-or-poll)).
Each is ignored, never failed on. What a delivery is comes from the shape
of its signed body. The `X-GitHub-Event` header is not covered by the
signature, so it is a cross-check only, and a delivery whose header disagrees
with its body is ignored.

A re-requested check run re-runs the run that check run reports; a re-requested
check suite re-runs the newest run of the pipeline at that commit.

### The run key

A run is keyed on **(repository, SHA, mode, event)**, written
`owner/name@<sha>:<mode>:<event>`. A second sighting of a key opens nothing:
GitHub redelivering a webhook, or a webhook and the poll seeing the same head,
is one run and one check run. Opening is serialized by repository, SHA and
event before policy selection, so a changed installed mode cannot open a
parallel duplicate for the same delivery; the existing policy snapshot wins.

The event is part of the key because a merge queue's run and the push that
lands its commit share a SHA and a mode. Keyed without the event, the push --
which may carry artifact preparation and notification stages -- would fold
into the queue's run and never execute. Matching verification evidence can be
reused without merging the separate events or their effect receipts.

A re-run is not a second sighting: it opens the key's next attempt, a new run
row with a new check run, and leaves the original as it ended.

### Stages, in order

The portable workflow runs stages one at a time, in the order written, and the
steps of a stage run at once. Every step waits for every step of the stage before
it, whatever `needs` says. An installation can bind its own orchestration recipe;
the captured recipe identifies the behavior that run executes. The first stage that fails stops the run: every later step is skipped
`pipeline_stage_blocked`, and the check run's table says *Not run: an earlier
stage failed* -- except notify stages and stages declaring
`runAfterFailure: true`. Independent analysis can still collect evidence after
failed tests. A passing later stage never turns the run green or unblocks a
subsequent ordinary stage. Cancellation and a lost driver lease still stop it.
The declaration is part of the execution definition checked during recovery.

`on` names events, modes or both. `on: [push]` runs on the default branch's
pushes only; `on: [full]` on the merge queue, pushes and releases;
`on: [pull_request, merge_group]` before a change lands. A stage whose `on`
leaves a run out is absent from that run's plan: it is not a skipped stage, and
the check run does not list it.

### Notify stages

A notify stage announces the run on the channel it names. The agent driving the
run hands the message to the cluster's outbound worker -- one delivery for a
Discord channel, one per recipient for an email channel -- and the step reports
what the delivery's row reports: delivered, failed, or not there yet when its
time ran out. Write the notify stage last:

- **It runs after an earlier stage failed**, to say so; ordinary stages
  after a failure are *Not run*. A stage may explicitly opt in with
  `runAfterFailure: true`.
- **A notify stage that fails fails the run**, and blocks every stage written
  after it, as any failed stage does.
- **A cancel before the hand-over sends nothing.** A cancel after it cannot
  recall a staged notification: the step is cancelled and says the
  notification may still arrive.

The rows the stage stages are the server's: staged by server code and marked
so, they take no write from a client -- no status stamp, no requeue, no
re-stage elsewhere -- so the delivery a step reports is the one the outbound
worker made. The rows are still readable by any signed-in reader
(memql#5804), so a message carries nothing the run page does not.

### Configure a notification channel

In **Pipelines → Settings → Connections → Channels**, add an email or Discord channel.
Use a stable name such as `releases`; the manifest's notify stage refers to
that name. For email, enter one bare email address per line (up to 20). For
Discord, enter the name of the secret holding the webhook URL, not the URL
itself. The secret name must start with a capital letter, contain only capital
letters, digits and underscores, and stay outside the reserved `MEMQL_` prefix.

Save the channel, reopen it, and select the pipelines allowed to send to it.
Both the channel and each selected pipeline must belong to you. Configuration
changes do not send a test message or start a run. The generated SDK exposes
these same operations as `pipelinesChannelSave`, `pipelinesChannelArchive` and
`pipelinesChannelAllow`, under the `app:pipelines/channels` capability.

Archiving keeps the channel and its history, while stopping new notification
admissions. **Save and resume** reactivates the same name. Existing pipeline
grants survive archival; uncheck a pipeline to revoke its grant. Running
pipelines re-read grants and channel status before admitting a notification,
including changes made on another replica. Messages already queued with the
outbound worker may still arrive.

### When a step is skipped

A skip is reserved for work that had nothing to do. It is never a refusal in
disguise, and it fails nothing.

- **`when: { bucket: <name> }`** narrows pull requests only. In an affected
  run the step is skipped `pipeline_not_affected` -- *No change under bucket
  os.* -- when the changed files are known, there is at least one, and none is
  in the bucket. In a full run it always runs, and so it does when the change
  cannot be read.
- **A packages step with nothing selected** is skipped `pipeline_not_affected`:
  *No affected Go packages.*, or *No db-gated packages are affected.* under
  `only: db-gated`.

### Which packages a step tests

`packages: affected`, in an affected run, reads the change and the repository's
Go import graph:

- The changed files are GitHub's compare of the pull request's base with its
  head.
- The graph is read from source at the run's commit. Every `go.mod` names a
  module, every directory of `.go` files is a package of the nearest module
  above it, and every import of a path under one of those modules is an edge --
  under any build tag, test files included, and whether or not a package still
  answers the path. That union is wider than any one build's graph, which is
  the safe direction. Imports of anything else, the standard library and
  dependencies, are not edges. Directories named `testdata` or `vendor`, or
  starting with `.` or `_`, are skipped, as the go tool skips them.
- A changed file selects the package that owns it -- its directory, or the
  nearest package above -- and every package importing one of those,
  transitively.
- A changed file in a directory that holds no package -- where a package was
  until the change deleted or moved it -- also selects every package still
  importing the path that directory would have (its module's path plus the
  directory below the module's root), and their importers, transitively. A
  change that deletes a package therefore tests the packages still importing
  it, which no longer build, rather than selecting nothing and skipping green.
- A change that touches no Go package, in no directory an import names, selects
  none.

The import graph cannot discover tests that read another package's files by
path. Declare those contract checks as explicit steps. Their bucket must cover
every input they inspect, including `.go` and `_test.go` files; omit `when` or
use `["**"]` when that input set is not narrower. The example's
`source-contracts` step covers these reads even when a changed child integration
does not import the parent package containing its source-scanning tests.

It selects **every package** instead whenever the graph cannot say: the change
could not be read (the compare failed, or listed 300 files, where GitHub stops
listing), the change is empty, a `go.mod`, `go.sum`, `go.work` or `go.work.sum`
changed anywhere, `memql-package.yaml` changed, vendored code changed, a path
under `select.full` changed, or a Go file did not parse. Selecting too much
costs minutes; selecting too little merges a red build.

In a full run, `packages: affected` means every package. `packages: all` means
every package in every run.

`packageRoots: [worker]` intersects that selection with packages inside the
repository directory `worker`, including subdirectories. The dependency graph
is followed first: changing a shared library still selects its worker
importers. An empty intersection skips the step before dispatching a host.
This supports a native integration lane for one component while ordinary unit
checks continue to cover the entire selected graph. Non-Go setup scripts that
the graph cannot see belong in `select.full` or an explicit contract lane.

`only: db-gated` then keeps the packages whose directory is a `select.dbGated`
tree or lies under one, on a `/` boundary (`component/database` holds
`component/database/x`, not `component/databasex`); `only: not-db-gated` keeps
the rest.

`shards: k` splits what is left over up to `k` steps, keyed `<stage>.<step>#1`
to `#k`, each with its slice in `MEMQL_PACKAGES` and `MEMQL_SHARD=<i>/<k>`. The
split balances on the pipeline's timing table, heaviest package first, each
into the lightest shard so far; a package the table has not measured weighs
five seconds. Packages that cannot be split -- one, or several measured at
zero -- run as one step under the step's own key. The table learns: after every
successful full run, each passing package's time from its step's `go test`
output (an `ok  <package>  <seconds>s` line; a cached result is not a
measurement) replaces that package's row.

---

## Refused, never run

### Pull requests from forks

A pull request whose head lives in another repository -- a fork, or a head
whose repository GitHub reports deleted -- is **refused, never queued**. The
cluster writes the run row `completed` with conclusion `refused` and code
`pipeline_fork_refused`, and completes the check run with conclusion
**failure**, titled *Refused: pull request from a fork*: a neutral conclusion
would satisfy a required check. No step runs and no secret is resolved. This is
deliberately stricter than GitHub Actions' approval for a first-time
contributor.

To run such a change, run it from a branch of the repository: a maintainer
pushes the contributor's commits to a branch here and opens a pull request from
it, and that pull request's run executes. The commits keep their SHA, so it is
the same run key as the refused run: a fork's refusal answers only a fork, and
the new pull request opens the key's next attempt, whose check run replaces the
failing one. Once a run of this repository's answers the key, a fork's pull
request at that commit is answered by it rather than refused beside it.

### A manifest that cannot compile

A run whose `memql-package.yaml` cannot be read (`package_manifest_invalid`),
has no `pipeline:` block (`pipeline_not_declared`), or does not validate or
compile (a `pipeline_*` code) **fails, typed, before any step runs**. It is
never skipped, because a skipped check reads as green. The run concludes
`failure` carrying the code, its sentence and its scope -- `<stage>/<step>`, a
stage, or a path into the block such as `select/buckets/os` -- and the check run
is titled *Refused: \<what\> (\<where\>)*, its summary carrying the sentence and
what to do. Fix the block and push: the next run reads the fixed manifest.

---

## Publishing retained security reports

`pipelinesPublishSARIF(runId: ..., stepKey: ..., path: ...)` publishes one
retained `.sarif` file from a successful scanner in a native pipeline. The caller
must own the pipeline and still reach its repository through the source's
GitHub connection. The installed GitHub App needs **Code scanning alerts: Read
and write** (`security_events: write`); existing installations must accept that
permission before publication. The App manifest requests it alongside check
reporting permissions. No installation token reaches a scanner container.

The source commit and full Git ref come from the recorded run. The native
publisher checks the successful scanner journal, pins the exact artifact,
and verifies its object version, size and SHA-256 before uploading. The report
must fit the 32 MiB uncompressed bound and GitHub's compressed upload bound.
The result identifies the durable `publicationId`, exact `sha256`, and state:

| State | Meaning |
|---|---|
| `prepared` | Inputs recorded; no upload claimed yet |
| `accepted` | GitHub supplied an upload ID; ingestion is still pending |
| `complete` | GitHub confirmed ingestion |
| `failed` | GitHub rejected the report during ingestion |
| `rejected` | The upload was definitively refused |
| `uncertain` | An uploader is still unresolved, or its reply/receipt was lost |

Use `pipelinesReadSARIF(publicationId: ...)` to observe a pending upload from
any replica. It never uploads. A repeated publish call for the same evidence
also observes the existing receipt. A recorded `accepted` state survives a
status-read failure; a replacement host continues GET requests for that ID.
An unresolved POST is never silently repeated, including after losing the
database write that would have recorded its response. Inspect that operation
before explicitly starting a new scanner run; a timeout is not evidence that
GitHub accepted nothing.

Terminal receipts release this consumer's artifact pin, so normal retention
can reclaim the file; the publication journal remains. Uncertain uploads retain
their evidence for investigation. `complete` proves report delivery, not an
absence of security findings. A scanner must have succeeded, but its enclosing
run may still be active or have failed elsewhere. Cancelled runs cannot publish.

Declare a report step after the stage that retains the scanner's artifact:

```yaml
- name: security-reports
  runAfterFailure: true
  steps:
    - name: codeql-go
      timeout: 5m
      sarif:
        step: analysis.codeql-go
        path: .memql-security/codeql-go/results.sarif
```

Give this stage the same event and schedule selection as its producer. The
producer must be an unsharded command in an earlier stage. The report step
runs in the driver, with no repository command or credentials sent to a worker.
It appears in the normal work journal and check result. The installed
`pipelineSARIFDelivery` recipe publishes once and observes ingestion with
bounded waits. Missing artifacts, failed producers, rejected or uncertain
uploads, and ingestion still pending at the deadline fail the step. An
unrelated failed test does not prevent reports when `runAfterFailure` is set.

A replacement driver recovers the existing publication identity and observes
its receipt without repeating an uncertain write. A new failed-only attempt
runs report producers again: an older attempt's skipped scanner is not proof of
artifact production in the new run. Retrying a delivery therefore differs from
recovering its interrupted driver. Declaring an artifact alone does not enable
automatic upload.

GitHub's [SARIF API](https://docs.github.com/en/rest/code-scanning/code-scanning)
defines the asynchronous ingestion status and required repository permission.

## Secrets

A manifest carries secret **names**, never values:

```yaml
        - name: verify-rollout
          run: ./scripts/verify-rollout.sh
          secrets: [VERIFY_TOKEN]
```

- The name is the cluster's `v1:platform:globalSecret` of that name, and the
  step's environment receives it under the same name.
- It resolves only when the pipeline allows it. `secretNames`, set when the
  pipeline is connected, is the owner's allowlist. A step naming a secret not
  on it refuses the whole run before any step runs
  (`pipeline_secret_not_allowed`), whenever that step's stage is part of the
  run.
- A name beginning `MEMQL_` never resolves: the manifest refuses it
  (`pipeline_secret_invalid`), connect refuses it in `secretNames`, and the
  platform's own variables win over it in any case.
- An allowed name with no value on the cluster fails its step
  `pipeline_secret_missing`.

Values exist only in the request handed to the runner for one step. They are
never written to a pipeline row, a run row, a work step, a log line or the
check run, and the log tails a check run shows have every secret value of four
bytes or more replaced with `***`.

**Allow only what anybody who can push a branch may see.** A pull request can
change its own pipeline, so a branch pushed to the repository can add a step
that reads an allowed secret. A fork cannot: its run is refused before anything
resolves.

### Private container images

A cluster container step can set `imagePullSecret: REGISTRY_AUTH`, where
`REGISTRY_AUTH` is both a stored global secret and a name in the pipeline
owner's `secretNames` allowlist. Its value is a Docker config JSON object:

```json
{"auths":{"registry.example.com":{"username":"reader","password":"<read-only registry credential>"}}}
```

Use an exact registry host, including its port when present. Each host must
match the step image or a declared service image. Inline `auth` containing
base64-encoded `username:password` is also accepted. Credential helpers,
wildcards, URL paths and token-provider configuration are refused. The value
is limited to 64 KiB and eight registries. Fleet and native steps do not
support this field.

The runner gives kubelet a per-attempt `kubernetes.io/dockerconfigjson`
Secret. Commands, sidecars and clone containers receive no mount or environment
variable containing that registry credential. It is cleaned with the owned Job
and guarded by the same receipt/acknowledgment rules. A manifest cannot refer
to an existing Kubernetes Secret or broaden the step service account.
Step and service images request a registry pull on every start, including
when image layers are cached. Use read-only pull credentials: any collaborator
who may edit this pipeline's manifest may also request its allowed global
secrets explicitly as environment secrets in a different step.

## Compute

`compute` is set when the pipeline is connected:

- **`cluster`**, the default: every step runs on the cluster. A step naming a
  need fails the run's compile with `pipeline_fleet_not_consented`, scoped to
  the step -- nothing about somebody's laptop is a default.
- **`cluster_and_fleet`**: a step naming a need may run on a machine in the
  owner's fleet that offers it; every other step stays on the cluster. How the
  step reaches a machine, and what the machine must allow, is
  [the fleet](pipelines-substrate.md#the-fleet).

A need outside the closed set is refused (`pipeline_need_unknown`), never routed
by guesswork.

## Delivery: webhook or poll

`delivery` is set when the pipeline is connected:

- **`webhook`**: GitHub posts each delivery to the cluster's inbound seam,
  `https://api.<domain>/inbound/github`, signed with the app's webhook secret,
  and a shipped automation opens the runs it asks for. The runs are decided by
  the delivery the seam staged and verified, never by anything else: a
  `github` source configured to verify no signature opens no run. A delivery
  opens runs only for pipelines connected under the installation it came from,
  for the repository name they were connected with. **Reinstalling the GitHub
  App on the repository, or renaming the repository, therefore needs a
  reconnect** (the same `pipelinesConnect` call): until then its deliveries
  carry an installation or a name no pipeline matches, and open nothing. A
  delivery through no installation -- a repository webhook, which
  [Packages](packages.md#update-detection) documents for Deployables' update
  feed on the same seam -- opens nothing, and is ignored rather than failed
  on. Setting the webhook up: [GitHub Connect](github-connect.md#the-webhook)
  and [Inbound delivery](inbound-delivery.md).
- **`poll`**: every minute, one agent replica reads the default branch's head
  and the heads of the open pull requests -- the hundred most recently opened
  -- through the owner's connection. It opens a `push` run for a default-branch
  head it has not seen and a `pull_request` run for a new or moved pull request
  head, forks refused as above, and forgets closed pull requests. **The first
  poll records what it sees and opens nothing**, so connecting never floods a
  repository's history with runs; runs start with the next change.

A cluster GitHub cannot reach -- a local cluster above all -- polls. A polled
pipeline sees heads only: a merge group, a published release and a re-run
pressed on GitHub are only ever delivered, so they start nothing there, and
`pipelinesRerun` re-runs from the cluster instead. A pipeline set to poll still
takes a delivery that does arrive, and the run key makes the second sighting of
a head a no-op either way.

## Who drives a run

A run is opened where its trigger fires -- the bff for a webhook, an agent
replica for the poll -- as a `queued` row, and its check run is created
`queued` there. An agent replica claims it under a lease (`driverNodeId`,
renewed every 30 seconds) and drives it: it reads the tree at the commit,
compiles the plan, opens the work goal, hands each step to the runner, and
writes the check run as the run advances. If that replica stops, another agent
replica takes the run over once the lease is 120 seconds stale, and the
every-minute poll claims any queued run no agent picked up. **A cluster with no
agent replica opens runs and never starts them.**

A pipeline's work run belongs to its runner (`triggeredBy: pipeline:<mode>`):
the work dispatcher never executes one, the spine's waiting sweep leaves it
alone, and Nexus's re-run and branch refuse it. Re-run a pipeline run with
`pipelinesRerun`.

## Operator run admission

An installation can hold new CI/CD runs before they acquire a driver lease or
start their execution deadline. Set `MEMQL_PIPELINES_ADMISSION_FILE` on **every
agent replica** to an operator-owned JSON file:

```json
{"allowNewRuns":false,"validUntil":"2026-10-21T23:00:00Z","reason":"Monthly development allowance reserved"}
```

The file contains one object, at most 4 KiB. `allowNewRuns` is a required boolean;
`validUntil` is a required RFC3339 timestamp, strictly in the future and no more
than two hours away. `reason` is optional explanatory text. Unknown fields,
unreadable files, invalid values and expired decisions hold new runs. Unset
configuration preserves ordinary admission. A writer must refresh an allowance
before it expires; stopping the writer cannot leave admission open indefinitely.

An optional boolean `allowTrainRuns` admits [release train](#release-trains)
runs while `allowNewRuns` holds every other run: a train run is the pull
request run of a `train/` branch, and trains are scheduled, so a cost guard can
budget them apart. Without the field, a train run is admitted exactly as any
other run. Install an engine that reads `allowTrainRuns` before any writer
emits it: an older engine refuses the unknown field and holds every run.

```mermaid
flowchart LR
    S[Webhook, poll or manual request] --> Q[Durable queued pipeline run]
    P[Operator decision file on every agent] --> G{Admission allowed?}
    Q --> G
    G -->|Hold or unavailable| Q
    G -->|Allow| C[Claim lease, then start execution clock]
    C --> K[Existing step execution and resource queue]
    R[Recovery of an already-started run] --> K
```

The claim reads the file afresh inside the existing cross-replica run gate.
A Kubernetes installation should project the same ConfigMap as a **directory**
on all agents, read-only and without `subPath`; environment values and subPath
mounts do not follow runtime updates. Projection is eventually consistent, so
allow for propagation delay in the spending reserve. No Azure credential or
Azure-specific cost policy is required in the engine. An external observer may
publish this decision using its own narrowly scoped credentials.

The gate covers new pipeline runs on both cluster and fleet compute, including
release and manual pipeline runs. It does not gate application requests, agent
goals, backups, database work or already-started pipelines. Those pipelines
continue their later steps and can recover on a different replica while new
admission is held. Cancellation remains available, and a held run stays queued
without consuming `MEMQL_PIPELINES_RUN_MAX_MINUTES`. The usual poll retries
admission after an allowance appears; existing deduplication and supersession
rules still apply, so an obsolete PR run may be cancelled by a newer commit.

Kueue capacity and operator pauses are separate controls. Holding individual
Jobs after a run starts still consumes the run deadline. Use run admission for
long budget waits; do not describe a Kueue Job hold as an indefinite pipeline
pause. The unfinished-run recovery page prioritizes started runs before queued
ones, so a backlog of budget-held runs does not hide running work from recovery.

## The check run

Each run writes one check run on its commit. PRs and merge candidates use
**`MemQL / <name>`**, the context a ruleset requires; `<name>` is the manifest's
name when the pipeline was connected. Pushes, releases and manual runs append
`/ push`, `/ release` and `/ manual`, respectively. Scheduled audits append
`/ daily` or `/ weekly`. These events can select different stages, so a passing
release or rehearsal cannot satisfy a failed PR check on the same commit.

A check is `queued` when the run opens, `in_progress`
when an agent starts driving it, and `completed` with the run's conclusion. Its
`external_id` is the run's id, and its details link is the run's page in MemQL
OS, `https://os.<domain>/?pipelineRun=<run id>`.

| State | Title |
|---|---|
| Queued | *Queued* |
| Running | *Preparing the run* until the plan is read, then *Running: \<stage\>* |
| Passed | *Passed: \<n\> stages in \<time\>*, or *Passed: nothing to run* |
| Failed | *Failed at \<stage\>: \<step key\>*, ending *and \<n\> more* when several steps failed; *Failed: \<reason\>* when no step failed and the run has a reason of its own |
| Cancelled | *Cancelled* |
| Refused | *Refused: \<what\> (\<where\>)* |

The summary is one line saying which run, then the stage table, then the failed
steps, each with its message and code:

```text
Mode full · Push to the default branch · Commit 3f9c2ab

| Stage  | Status                           | Steps              | Time   |
| ---    | ---                              | ---                | ---    |
| checks | Passed                           | 1 passed           | 52s    |
| tests  | Failed                           | 8 passed, 1 failed | 7m 40s |
| deploy | Not run: an earlier stage failed | 1 not run          | -      |
| notify | Passed                           | 1 passed           | 3s     |
```

A stage reads *Passed*, *Failed*, *Cancelled*, *Running*, *Waiting*, *Not run*,
*Not run: an earlier stage failed* or *Skipped: \<reason\>*; a stage's time is
its longest step's, and `-` is a time nobody measured. The check run's text is
the last lines of each failed step's output, with secret values masked. A
refused run shows the refusal and what to do instead of a table.

### A run can fail with every step passed

A run's workflow can fail after its steps -- the
[independent review](#independent-review-for-ordinary-pull-requests) reaching
no model, for one -- and a required step can be blocked by the workflow or
never report how it ended. The run is a failure, and no step's row says why.
The driver writes that reason on the run row with the conclusion, as
`failureCode` and `failureMessage`, masked like every text a run reports. The
check run's title is *Failed: \<reason\>*, and its summary adds a
**Run failure** section under the stage table:

```text
Mode affected · Pull request #42 · Commit 3f9c2ab

| Stage  | Status | Steps    | Time   |
| ---    | ---    | ---      | ---    |
| checks | Passed | 2 passed | 52s    |
| tests  | Passed | 8 passed | 7m 40s |

### Run failure

Pipeline workflow failed: every_door_shut: no door to a model is open for this call (`pipeline_executor_error`)
```

A failed step keeps the title, and the run's own reason is listed before the
failed steps. The run page says *The run failed outside its steps* above the
stages, with the same reason. Both read it from the row, so a final report that
recovery republishes from another replica states it too. A refusal is never
written there: it ended the run before any step ran, and stays the whole report.

**A 403 never stops a run.** An installation that has not accepted the checks
permission answers 403 to every check-run write. The run opens, executes and
concludes as usual; it records `checkRunState: refused` and carries the note
`pipeline_check_permission_missing`. Accepting the wider permissions is the
whole repair -- [Upgrading an app registered before pipelines](github-connect.md#upgrading-an-app-registered-before-pipelines)
-- and the next run reports its check. A run that can write no check run at
all, with no app or no token, records `checkRunState: unavailable`.

**A final report that did not land is written again.** A run that concluded
while GitHub would not take its last check-run write -- GitHub down, the token
unobtainable -- records `checkRunState: unavailable`, and its check run still
shows it unfinished, which holds a merge on a required check. The every-minute
recovery on an agent replica republishes the final report of each such run
concluded in the last 24 hours, trying less often while it keeps failing, and
records `written` once it lands. The republished report carries the stage table
and each failed step's message and code, but not the log excerpt, which only
the agent that drove the run held.

**A journal outage stops advancement.** The driver requires a stored intent
before starting a command or staging a notification, a stored receipt before
advancing, and a stored terminal work record before publishing a successful
check. A failed write leaves the pipeline unfinished and stops that drive.
Recovery retries with bounded backoff after storage returns. This prevents a
missing record from being reported as success; it does not make an external
side effect safe to repeat without reconciliation.

## Readiness

Pipelines is an optional item in the cluster's
[readiness](configuration-readiness.md) list, reported by agent nodes. It is
there for the three things a pipeline's checks need: a GitHub App whose
installations hold checks write, a connected repository, and a runner to
execute steps. A repository counts as connected once a pipeline is.

The setup item records configuration: a GitHub App, an active pipeline and an
agent dispatcher. The integration status report calls execution configured
only when those are present and every known workbench reports a
runner with a current isolation proof. The agent reads each replica through
the authenticated mesh. Each report names the replica, namespace, isolation
state, check time and expiry. Missing runners, unanswered requests, failed or
inconclusive proofs, and expired proofs do not count as ready. A substituted
reply from another replica cannot vouch for the one that disappeared.

Pipelines → Settings → Infrastructure → Compute separates the installed dispatcher from each
workbench's observed readiness. Refresh reads the last isolation results; it
does not launch a probe. Missing reports, failed reads and expired observations
never display as ready. The same operator-only report is available through
`builtin pipelinesStatus()` and the generated SDK method.

Reading readiness starts no build or probe. Before the first step proves
isolation, the report explicitly says `not_proven`; the runner proves it
before starting work. Configured GitHub credentials alone do not establish
whether an installation accepted checks write, which is a fact per
installation: a run that could not write its check run says so itself
([The check run](#the-check-run)).
Nothing needs the item, so the first-run wizard does not walk it, and an owner
may answer *Not now*.

**Pipelines → Settings → Infrastructure** is the item, for the cluster owner. Its three
sub-steps are the report's three facts, as the agent nodes that run steps
report them: the GitHub App (registered from Settings through GitHub's
app-manifest flow, so the permissions arrive filled in), a repository
connected, and a runner -- with a line about how many of your machines allow
pipelines. Setup controls remain in this destination; they do not add
unseen-update markers to the OS navigation.

**An app whose permissions grew is named there.** Every existing installation
of the GitHub App keeps the permissions it accepted until the account that
installed it approves the change GitHub sends it, and until then every
check-run write there answers 403. Pipelines → Settings → Infrastructure asks GitHub
(`pipelinesInstallations`, `read app:settings/pipelines`) which installations
lag what the app asks for and links each to the page where the change waits --
before a run finds out by failing to report.

## What runs today

This release is the seam (epic memql#5477), MemQL OS's surfaces for it (epic
memql#5479) and the substrate that executes a run's steps (epic memql#5478):
deliveries, the poll, run keys, the plan, the work goal, the check run, the
Runs tab, run page, Checks and connect flow, and every command step run as a
Kubernetes Job on the cluster, or on one of the owner's machines for a step
naming a need, with its output in the log store and its full log and artifacts
in the owner's Library ([Pipelines substrate](pipelines-substrate.md)). Notify
stages require an active channel and an explicit pipeline grant; a missing or
revoked channel fails the run instead of implying that delivery succeeded.

**Do not make `MemQL / <name>` a required check until a run has passed on this
cluster.** A cluster has to be ready for steps before any runs: on one that does
not enforce network policy -- an AKS cluster as `azure-provision.sh` creates it
-- every step is refused `pipeline_isolation_unenforced`, and a step's images
must be pullable with no registry credential.
[Owner actions](pipelines-substrate.md#owner-actions) lists what to set up
first.

## Refusal codes

A **refusal** ends the run before any step runs; a **failure** fails one
step; a **skip** or a **note** changes no outcome. The scope a code carries
says where: `<stage>/<step>`, a stage, or a path into the block. A failure or
skip code can also be a run's own reason, its `failureCode`, when the run
failed and no step reported why
([A run can fail with every step passed](#a-run-can-fail-with-every-step-passed)).

| Code | Kind | Meaning | What to do |
|---|---|---|---|
| `pipeline_not_declared` | refusal | `memql-package.yaml` has no `pipeline:` block | Add one, or disconnect the pipeline |
| `pipeline_stage_invalid` | refusal | No stages; or a stage missing a name, misnamed or repeated, needing itself, a later stage or no stage, with both or neither of `steps` and `channel`, or naming a misnamed channel | Correct the stage the scope names |
| `pipeline_step_invalid` | refusal | A step missing a name, misnamed or repeated in its stage, with no command, or with a bad `packages`, `only`, `shards` or `timeout`; or a service misnamed, with no image, or with a bad `env` name | Correct the step or service the scope names |
| `pipeline_select_invalid` | refusal | `select` names an unknown Go selection, a `dbGated` entry that is no directory, a misnamed or empty bucket, or a glob that cannot be read; or a step uses `only` with no `dbGated` | Correct the entry the scope names |
| `pipeline_select_missing` | refusal | A step names `packages`, and the block has no `select: { go: import-graph }` | Add it |
| `pipeline_event_unknown` | refusal | A stage's `on` names neither an event nor a mode | Use one the sentence lists |
| `pipeline_bucket_unknown` | refusal | `when.bucket` names a bucket `select.buckets` does not declare | Declare it, or correct the name |
| `pipeline_service_unknown` | refusal | A step names a service `services` does not declare | Declare it, or correct the name |
| `pipeline_need_unknown` | refusal | A step's `needs` names something outside `display`, `docker`, `gpu`, `macos_tooling`, `user_files` | Use one of them, or remove it |
| `pipeline_secret_invalid` | refusal | A secret name is not upper-case letters, digits and underscores, or begins `MEMQL_` | Rename it |
| `pipeline_secret_not_allowed` | refusal | A step uses a secret the pipeline does not allow | Reconnect with it in `secretNames`, or remove it from the step |
| `pipeline_fleet_not_consented` | refusal | A step names a need, and the pipeline's compute is `cluster` | Reconnect with `compute: cluster_and_fleet`, or remove the need |
| `pipeline_fork_refused` | refusal | The pull request's head is in another repository | Push the branch to this repository and open the pull request from it |
| `pipeline_disconnected` | refusal | The pipeline was disconnected before an agent started the run | Connect it again; the next change runs |
| `pipeline_already_connected` | refusal | Connect only: another source's pipeline already runs the repository, and a repository has one | Disconnect that pipeline -- its owner does, with `pipelinesDisconnect` -- or work from that source |
| `pipeline_nothing_to_rerun` | refusal | Re-run failed only: the run has no failed or cancelled step to run again | Re-run it whole |
| `pipeline_runner_unavailable` | failure | No runner could take the step: the cluster is not set up to run steps, or the part that would run it was unreachable | Set the cluster up as [Where a step runs](pipelines-substrate.md#where-a-step-runs) says, then re-run. Nothing in the repository is wrong |
| `pipeline_executor_error` | failure | The runner could not report how the step ended; as a run's `failureCode`, the pipeline workflow failed, perhaps after every step passed | Re-run; if it repeats, look at the runner, or at the reason the check run and the run page state |
| `pipeline_secret_missing` | failure | An allowed secret has no value on this cluster; or a Discord channel's secret has none | Store a value under that name, then re-run |
| `pipeline_channel_missing` | failure | No channel of the name the notify stage gives belongs to the pipeline's owner | Create it, or correct the name, then re-run |
| `pipeline_channel_archived` | failure | The notify stage's channel is archived, and delivers nothing | Name an active channel, then re-run |
| `pipeline_channel_not_allowed` | failure | The channel does not accept deliveries from this pipeline | Allow the pipeline on the channel, then re-run |
| `pipeline_channel_invalid` | failure | The channel cannot deliver as it is set up: a Discord secret name outside `^[A-Z][A-Z0-9_]{0,63}$` or in `MEMQL_`, a secret that holds no Discord webhook URL, or a recipient that is not a bare email address | Correct the channel, then re-run |
| `pipeline_notify_failed` | failure | The outbound worker gave the delivery up, or it could not be handed over | Read the worker's error the sentence quotes, then re-run |
| `pipeline_sarif_delivery_failed` | failure | Scanner evidence, storage, authorization or GitHub's report protocol could not be confirmed | Read the error and inspect the publication receipt before starting another scan |
| `pipeline_sarif_undelivered` | failure | GitHub has not confirmed ingestion, or the upload is rejected or uncertain | Inspect the retained receipt; pending ingestion can finish later, and an uncertain write is never retried automatically |
| `pipeline_notify_undelivered` | failure | The delivery had not arrived when the step's time ran out; the sentence says where it stood | Wait for a delivery still being retried, or re-run, which sends another |
| `pipeline_stage_blocked` | skip | An earlier stage failed | Fix that stage |
| `pipeline_not_affected` | skip | The change touched nothing the step covers | Nothing |
| `pipeline_passed_earlier` | skip | A failed-only re-run carried the step over: the attempt it re-ran passed it with the same packages | Nothing |
| `pipeline_check_permission_missing` | note | GitHub answered 403 to a check-run write; the run went ahead | Accept the app's wider permissions on GitHub |

The runner adds codes of its own -- a step past its timeout
(`pipeline_step_timeout`), an image that will not pull, a clone that fails, a
cluster that cannot prove its steps isolated -- listed with their remedies in
[the substrate's codes](pipelines-substrate.md#codes). Connecting refuses with
Deployables' codes as well as these
([Connecting a pipeline](#connecting-a-pipeline)).

---

## Related

- [Pipelines substrate](pipelines-substrate.md) -- where a step runs, what it
  may reach, its logs and artifacts, how it ends, and what an operator sets up
  first
- [GitHub Connect](github-connect.md) -- the app that delivers a pipeline's
  events and writes its check runs, and the upgrade an older app needs
- [Inbound delivery](inbound-delivery.md) -- the webhook seam and its signatures
- [Packages and deployables](packages.md) -- the source, the manifest, and the
  update feed that reads the same deliveries
- [Deployables](deployables.md) -- the applications a repository produces
- [Configuration readiness](configuration-readiness.md) -- what the Pipelines
  item's marks mean
