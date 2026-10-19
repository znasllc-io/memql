---
title: Production pipeline cutover
audience: internal
status: draft
area: planning
sinceVersion: 0.29.3
owner: znas
---

# Production pipeline cutover

The owner requested production adoption after the local full-suite proof and
asked that existing issues track the work through closure. This continues
[epic #5480](https://github.com/znasllc-io/memql/issues/5480), using its
existing milestones rather than opening another epic. The implementation
contracts in [self-hosted delivery](pipelines-self-hosting.md) still apply.
The owner authorized implementation, testing, merge, production deployment,
cleanup and retirement of Actions. Keep one consecutive full qualification pass
as the acceptance target, with targeted failure/recovery checks; do not repeat
three expensive full passes. Close issues only when their actual replacement
and production evidence are complete.

## Active cutover, October 19, 2026

Production currently serves **v0.29.35**, through instance commit
`ccd6a45d9f8ba5dc82eedda585aa7ce6b86f60f9`. Its Argo operation completed
Healthy/Synced at 12:13 UTC. No native runs remained active during the rollout.
An independent public check verified the API, Identity and OS against the
pinned edge image; website documentation remains a separate pending check.
Engine main is `771c15a01` (v0.29.35). The v0.29.31 full native pass
below remains valid historical acceptance evidence; it is no longer the serving
release. Engine **v0.30.0** is prepared in #6200 and is not deployed. The
independent v0.29.33 rollout at 06:56 UTC replaced Agent while the editor
qualification still had two steps unfinished. Its resulting
`pipeline_node_lost` failures are not a successful qualification. The cutover
branches incorporate that release and require fresh checks on their new heads.

Instance #237 passed all nine native steps with retained logs and artifacts.
Its Agent artifact receiver fix is now deployed with the already configured
2 GiB operator bound; the fleet transport retains its separate 256 MiB limit.
Instance #232 and #239 are also deployed. Both Agent and Workbench replicas
are ready. No CPU, memory, node or quota increase accompanies these changes.
The independent instance #243 deployment paused pipeline intake again; the
12:14 UTC whole-run read found no active pipelines, unfinished runs or Jobs.
All four original webhook connections were restored afterward, confirmed
through the authenticated production UI at 12:45 UTC. Native #6213 now tests
head `d46962e79f7c148f2731f5cbf84cd36828eec00b` in run
`88d6b0e12539b876d1ca0c886fd9b9c4`; it includes the deployed reconnect fix.
Keep controllers stable until every whole run, including fleet work, finishes.

Instance #240's current head `36a635936d6eda0359dd53572ee9f91bdf85a344`
passed all ten native steps in run `e7cb192d9779fc8de3b8eb3f3ccf16ba`,
with ten retained logs, sixteen Library file records and no artifact notes.
Its public documentation acceptance remains pending #6171 and website
publication. Four clean, merged instance worktrees and their exact matching
remote/local branches were removed after ancestry and process checks
(#224, #232, #237 and #239); active work and shared caches were preserved.

At 13:59 UTC, the remaining competing publisher triggers were disabled:
engine SDK, docs, database, fixture mirror and toolchain publication; template
and instance image publication; and the instance's hourly docs sync. None had
an active run. This prevents a legacy writer racing the native candidate;
it does not claim the remaining native publication paths are qualified.
Required checks and their App binding were unchanged. Workflow definitions
remain until their replacement acceptance is complete.

The preceding `a4a8118e140616dd02a4e5b4ee3d9005` run finished with 34
completed steps, one failure and seven skips. All eight Linux/macOS editor-host
cases passed. Packaging stopped after 2,839 of 2,840 unit cases passed: the log
redaction case timed out waiting for its real run recorder to finish filesystem
writes. Log `beb4936a-6484-4ad7-a6c0-8852a73c5b10` was retained and read back.
A four-second delayed terminal receipt reproduces that false timeout. The
repair uses a ten-second elapsed deadline and ten-millisecond polling; both
the delayed case and all 2,840 unit tests pass in the pinned toolchain with two
CPUs and 4 GiB, without skips. Fresh native qualification remains required.

The final runtime also preserves a failed command's exit code when packaging
never produces its outputs. Cluster and Fleet report artifact collection as a
secondary note, retain the command log and reject unverified files. A successful
command still fails when a required artifact is missing or cannot be verified.
The reproducing cluster regression fails before the repair; the complete
pipeline executor race suite, including Fleet and false-green controls, passes.

The independent instance rollout at 08:31–08:32 UTC restarted Agent and
Workbench while qualification was active, despite unchanged executor images.
Configuration annotations also change pod templates. Final run
`350954a64844ed427080eeaec5322992` therefore failed its macOS web-host step
with `pipeline_execution_uncertain`. It was cancelled at 09:22 UTC to release
capacity for prerequisites, retaining 23 completed steps and the failure.
It is not a complete pass. Future controller rollouts must drain whole runs,
including fleet work, rather than checking only current Kubernetes Jobs.
The declared event priority classes are installed, but automatic priority
labels require the v0.30.0 runtime. Backfilled priorities on owned queued
checks preserve the same quotas and never preempt running work.

The Mac now permits the instance repository alongside the engine and Cockpit.
The exact repository permission change has its own native maintenance receipt.
Native run `c61e098d69c66c97310147d942760171` then passed all 15 instance
Bash compatibility tests on the actual Mac, with no skips. Its report and log
are retained; temporary maintenance PRs #6242 and instance #241 were closed
without merge and their branches and worktrees removed. Instance execution
now permits cluster and fleet work.

Cockpit v0.20.0 remains unpublished after a full release attempt encountered
Docker's temporarily unavailable memory statistics. Cockpit #554 merged as `babaf7752cbb4cbe315ecea1be7ad89c1104c180`
at 09:51 UTC and prepares v0.20.1: three bounded sample attempts, fresh daemon identity checks and
fail-closed admission when measurement remains unavailable. The complete local
race suite, including real Docker isolation, passes. Native run
`3a01154b260a42500e89ccf89e5749b4` passed all 12 steps with retained logs
and eight artifacts, including the actual Mac tests. Both Cockpit rulesets
were independently verified before merge. The immutable v0.20.0 tag is not
reused.

Cockpit's second v0.20.1 release attempt,
`12385a6647a7194e58f0df7214644c96`, passed all 16 steps with 16 retained
logs, from work run `4f28f2665bc7bb48ee7e002bc15254b5`. It ran at the exact
merged source above. Independent production readback confirmed this complete
pass after Chrome access was restored. The redundant third attempt opened
before that discovery was cancelled; another full Cockpit build is unnecessary.
Candidate `sha256:8cc989db242c4d16502fa923ceeeb72d675c119596bfa5289ece4093d52b7464`
is approved, with 18 of 41 file publications complete in draft release
417220182. It is not public. The engine upgrade changed approval identity;
#6244 adds an explicitly reviewed renewal of unchanged file drafts. After that
runtime is deployed, renew under fresh approval, finish the retained release,
and verify public downloads. Preserve existing assets and publication receipts.
The renewal and its private test-engine cache cleanup are integrated into the
held v0.30.0 branch. Combined release, npm and OCI race suites pass against a
required real database; documentation and embedded-file inventory checks pass.
The temporary database and editor test containers were removed. The redundant
image-read-grant worktree was removed after both patches were verified in the
final branch; its branch references remain until that branch merges.

The v0.20.1 release run `a120d1d5b0f32c30f307b06d47bcdc12` ended with
14 completed steps and two failed Mac builds. The earlier #6213 run
`6f39cbd8bd068fba838d148823b7d0aa` ended with 32 completed steps,
three failed Mac editor steps and seven skips. All five failures were
`pipeline_node_lost`; neither run qualifies. Completed logs were retained and
neither failed run supplied an approved candidate. The Linux worker recorded a network timeout
and DNS failures at 10:16 UTC; it and the Mac registered new connections around
10:17. Older streams remained open until 10:32. Agent and Workbench did not
restart during that interval. The Mac's client-side network cause remains
unconfirmed, so a shared network interruption is an inference.

Same-replica registration replacement now terminates the predecessor, matching
the existing cross-replica supersede policy. The RPC receive loop also observes
session cancellation: cancelling its derived context alone did not interrupt
gRPC's blocked receive. Real gRPC regression cases exercise pending work,
same-replica replacement, remote-holder events and revocation while idle. The
successor remains usable. This reports interrupted work promptly; it does not
replay a command whose execution is uncertain. Both transport regressions
failed against the previous implementation; the affected worker race suite
passes locally. Independent #6243 carried the same runtime repair through
native run `bf6d6d27baad43e524b7496c0c3560c6`, merged at 11:36 UTC,
and is deployed in v0.29.35. The final branch adopts that implementation and
retains the additional real-gRPC cases. Fresh engine qualification is running;
Cockpit's separate complete pass is recorded above. Deploying this repair does
not turn the earlier failed runs into passes.

The final runtime now includes bounded repository maintenance: retained JSON
proposals, exact operator-owned repository/file grants, fresh signed-actor and
PR-author checks for dependency updates, and deterministic Git write/readback
recovery. Commands receive no writer credentials. Successful Git writes retain
their evidence until the work receipt is durable; conclusion releases the
matching pins and repeats cleanup after a process loss. A second agent replica
recovers a simulated journal failure after the write without another commit.
The complete compiler, driver and GitHub protocol race suites pass. The eight
proposal-generator tests and generated model/schema/SDK checks pass. Recipe
activation and production qualification remain separate from this runtime work.
The production GitHub App, “MemQL for ZNAS LLC” (4822538), now has accepted
`pull_requests:write` on installation 158882932. The installed permission page
was verified at 12:51 UTC; existing checks, contents, security-event, metadata
and merge-queue permissions were preserved. No further GitHub access prompt is
pending for this repository-maintenance capability.

The repaired full local `make test` passed, including the combined maintenance
runtime before its final pin-lifecycle refinement; database-backed cases skipped
without PostgreSQL. The final lifecycle changes pass the complete affected race
suites. These local results do not replace fresh native qualification. The old
#6200 head's native run was cancelled after local conformance failures were
found; it is not a passing release.

## Historical acceptance evidence: October 18, 2026

At this checkpoint, installed production ran **v0.29.31**, built natively from
`1d087ac973e5fd1e620f99f93ef42dba1f3a3642`. Release run
`b0643687e51532322e50eb0ff6014ab3` / Work run
`af5b4ab480781f3df9a1eefc90ecb511` passed **57 of 57 steps**, with no skips
or missing logs, from 10:23:29 to 12:00:50 UTC. The Library retained 57 logs
and 136 artifacts. This includes all five database groups, the editor host
matrix and 5,905 OS tests. Four steps capped their displayed log previews;
their full logs were retained. The earlier unsuccessful releases below are
history, not the serving release or a reason to repeat this complete pass.

All seven engine images were independently checked for source, release,
role and digest before instance
[PR #226](https://github.com/znasllc-io/memql-znas/pull/226) pinned them.
ArgoCD reports Healthy/Synced, with all configured serving deployments ready.
The MCP deployment remains intentionally scaled to zero. Instance PRs
[#227](https://github.com/znasllc-io/memql-znas/pull/227) and
[#228](https://github.com/znasllc-io/memql-znas/pull/228) preserve bounded
background admission and runtime capacity during deployment sync.

The build pool remains one autoscaled Standard_D8s_v6 node, maximum one.
Kueue bounds background work to 6,500 millicores, 22,528 MiB memory,
65,536 MiB ephemeral storage and six pods. The namespace's 128-Job object
quota is queue capacity, not concurrent execution capacity. This cutover
adds no serving nodes or paid fleet machines.

The proven native PR check is now required, bound to App 4822538, alongside
the existing `ci-required` context. The queue builds one candidate at a time
with a 180-minute response ceiling. See the exact decision and PR proof in
[the ruleset baseline](../ops/ruleset-baseline.md#native-required-check-october-18-2026).
Native merge-candidate success is still pending. The queue rejected the
non-successful native candidate described below even after legacy CI passed;
the legacy context and remaining workflows have not been retired.

Storage PR #6171 passed its native head check at 16:29:23 UTC: 35 completed
steps, 14 expected path skips, 35 retained logs and 29 artifacts. It entered
the ordinary merge queue at 16:29:48, without owner bypass. Native full-mode
merge-group run `edc9f8dee667f4951fe021b9b6a404ae` / Work
`572f56714d0b952207a526e0b5f952a7` started at 16:30:08 on exact candidate
`1d2dcc4fde54822e218dcf2def35470afef9ed67`, based on
`f0af5373d980491c0f215c9a4233100310898808`. It did not qualify: the
campaign signup burst assertion rejected a valid partial write, and a macOS
editor host timed out while repeatedly downloading VS Code. Its 325 unit tests
passed; the desktop host never started. Repairs are
[#6212](https://github.com/znasllc-io/memql/pull/6212) and
[#6213](https://github.com/znasllc-io/memql/pull/6213). The former adds a
cross-engine retry regression without changing signup runtime behavior. The
latter distinguishes progressing downloads from stalls and bounds all transport
attempts by one twenty-minute ceiling, without retrying tests.

The unsuccessful queue attempt was cancelled at 18:48:58 UTC to release its
remaining work: 35 completed, two failed and eleven cancelled steps; 35 successful
logs and 93 artifacts retained. Its conclusion is **cancelled**, not successful
or a completed failure aggregate. Legacy `ci-required` check 117730646444 had
passed at 16:43:11. Native App 4822538's check 117727730439 reported cancelled
at 18:48:58, and `github-merge-queue[bot]` removed #6171 at 18:49:31 without
merging it. This proves non-success rejection by the actual queue; it does not
change the run's conclusion to failure. The driver regression suite separately
covers failed steps, early returns and fabricated skips.

Signup repair #6212 merged as `4f9d6a799c6191fb50491d13a1ead5c10b7099d6`
at 19:43:09 after native run `9c037d5b6ddacb1fe495d1ae4cd8a427` passed
all 33 selected steps, with 14 expected path skips, 33 logs, 29 artifacts and
no retention notes. Mac download repair #6213 still has native qualification
in progress. Both repairs are included in #6200. The repaired exact ordinary
merge-queue success remains required before removing the legacy context.
Its native run exposed an unrelated error-page test-fixture collision: released
public/probe ports could be reused by the fixture's deliberately occupied TLS
socket. Reserving all three distinct ports before startup preserves the strict
listener-cleanup assertion. The original failed eight of 50,000 short iterations;
the repaired fixture passed all 50,000 and the complete error-page suite with
the race detector. This repair is also included in #6213 and #6200.
The cutover branch prepares engine version 0.29.33; 0.29.32 is reserved by the
independent Ask/install-recovery PR #6199. Neither version was deployed at this checkpoint; the active status above supersedes it.
Storage PR #6171's head gate received a one-time Kueue priority of 1, allowing it to
run alongside the existing database job. No running job was preempted and
no quota was increased; all declared commands and assertions ran unchanged.

The owner subsequently authorized the Mac repository permission change.
One-time native maintenance run `c2d53a13e477da5ffb7755f329f9d285` passed,
and the live worker re-advertised both `znasllc-io/memql` and
`znasllc-io/memql-cockpit`. Its retained receipt proves the policy edit and
reload; it is not application CI qualification. Temporary
[PR #6204](https://github.com/znasllc-io/memql/pull/6204) was closed without
merge, and its remote branch, local branch and worktree were removed.
Cockpit [PR #523](https://github.com/znasllc-io/memql-cockpit/pull/523)
now has native qualification in progress.

Cockpit candidate `41615a5beb4def7da7f2c3f9597a41b51d0947bd` follows native
Linux race-suite and macOS computer-use build, vet, app/menu packaging and
isolated installer successes. Its predecessor's fuzz lane hit the upstream
[Go duration-shutdown race](https://github.com/golang/go/issues/75804), without
a failing input or property assertion. Only fuzzing now uses Go 1.27.2; all
13 discovered targets passed locally at 20 seconds each, without retries or
suppressed failures. Native run `3c7831256ca179ee9bc672d0e8fac12d` is still
in progress; this is not a published Cockpit release.

The workflow-retirement branch now reads native configuration for the
database partition, timeout budgets, tag coverage, node roles, path coverage,
gate inputs, editor dependencies, OS checks, cache declarations and service
image pins. Its database guard compiles the real source graph through the
installed policy, verifies every provisioned package reaches a shard once,
and passes those shards through the actual budget runner's command builder.
The obsolete Actions parser, aggregate, token-permission and cache-writer
guards have been removed. Their replacement failure behavior is covered by
the native trigger and driver tests, including early-return and fabricated-
skip rejection. The core Actions workflow itself remains until the exact
merge-candidate production proof is complete.

Pending implementation [PR #6200](https://github.com/znasllc-io/memql/pull/6200)
combines standalone DB test budgets, channel management and fresh notification
grants, and full automatic toolchain smoke checks before native artifact export.
It supersedes [#6202](https://github.com/znasllc-io/memql/pull/6202) and
[#6203](https://github.com/znasllc-io/memql/pull/6203) to avoid repeated full
validation of sibling PRs in the bounded build pool. Their local validation is
recorded on the PRs; an open PR is not a deployed feature. SDK publication,
public docs delivery, installer lifecycle, remaining
audits and companion workflow retirement still need their own acceptance
receipts before #5480 is complete.

Native installer migration keeps the cheap no-cluster round trip for installer
changes and moves the five full cluster cases to weekly or manual execution:
round trip, repair, upgrade, from-source owner claim and pipelines substrate.
The candidate scripts retain the existing graph/receipt assertions and use a
fresh inner Docker daemon on an opted-in Linux fleet machine. The cheap case
is capped at two CPUs and 4 GiB; the full cases at four CPUs and 16 GiB. No
host home, Docker socket, directory or network namespace enters the fixture.
The privileged fixture runs trusted installer test code; it is not a shared
Workbench or an AKS step. A 64 GiB free-disk preflight is a check, not a disk
quota. This adds no cloud instance or production capacity.

The local fixture proof reached a real nested container with its cgroup limits
enforced. A second proof killed the parent process with SIGKILL: a separate
supervisor, bound by a kernel pidfd and resource ownership labels, retained
evidence and removed the container, volume and image. Its first attempt found
a missing recovered container ID; the corrected proof passed. This is fixture
and cleanup evidence, not successful install/repair/upgrade qualification.
The existing installer Actions remain until the native lifecycle receipts pass.

The unused VS Code Marketplace/Open VSX publishing workflow is retired while
native builds and installable downloads remain. Its historical successful run
was a dry run, and its only publishing attempt failed without a token. Neither
registry listed the extension on October 18. First-time marketplace publishing
is separate from migrating the working delivery paths.

The retirement branch also adds receipt-bound npm publication to the existing
release candidate service. Local protocol and real-database tests cover exact
archive readback, conflicts, uncertain outcomes, durable version reservation
and recovery on another host. The release review exposes the package version
and dist-tag and preserves separate approval. This is implemented and locally
tested, not a production npm publication receipt. Component tag triggers,
registry credentials and a live publication still need qualification before
the SDK workflow can be removed.
GitHub Packages already contains SDK 0.8.0, published on July 15. The retirement
branch prepares 0.9.0 and keeps its workspace lockfiles aligned; its package
dry run passed. This does not publish or replace an existing registry version.

## Native infrastructure publication preparation, October 18

PR #6200 includes OCI index publication and an independent
`deploy/db-image/VERSION` of `16.15.1` (PostgreSQL 16.15, operand revision 1).
The installed CNPG webhook accepted this tag in a server dry run; the running
database image was not changed. Native database publication is still pending.

The existing TimescaleDB fixture index contains five architectures and its
children total 2,687,287,862 compressed bytes, beyond the unchanged 2 GiB
artifact limit. The new manual-only `fixtures.timescaledb` step explicitly
selects Linux AMD64 and ARM64, the native container runner's supported platforms,
and retains both provenance manifests. The old five-platform mirror and service
pin remain untouched. Local anonymous download and complete verification passed:
1,081,885,184-byte archive `sha256:4ffb9bb37e693b0a6371ad9a3c9ba42204bb22c61a9c9ba95324cb0d95d80ca9`,
derived index `sha256:dea8fe7100c13d6473659b9d4be3fdb83f045e716b165155b9b6de5271fbf11a`.
The metadata distinguishes that derived index from the pinned upstream index.

The publisher verifies every child and blob and preserves durable intent,
immutable tags and independent-host readback. Race-enabled protocol tests cover
missing/corrupted children, unrelated provenance, duplicate platforms, tag
conflicts, read-only transport bounds and fresh-host replay. Compiled native
selection and version guards pass. These are local qualification results;
production execution, approved publication and anonymous pull receipts are
still required before the database and fixture Actions workflows are removed.

## Remaining native operation support, October 18

PR #6200 adds repository-scoped read credentials for explicitly opted-in cluster
commands. They have contents/checks/pull-requests read only, required Secret
references, queue refresh and log redaction. Fleet, image and report-only steps
refuse the command grant. The full compiler/runner race suites and agent/app
scope tests pass.

The standalone Scorecard scanner preserves the retiring action's policy and
analyzes all 18 checks (two are unenforced by that upstream policy). It validates
source identity, complete coverage and real SARIF output; an upstream zero exit
without a report is not success. A read-only rehearsal at main `f0af5373` completed
all 18 with no unscored checks, producing 13 findings and SARIF digest
`3c322886cf2b1792002a2f6f21565fe81df700c92e9a311dd5f72b035578eaa5`.
Weekly activation is held in commit `6d56703fb` until the new `githubReadToken`
manifest field is deployed. Native App-token execution and receipt-bound SARIF
publication still require qualification; the old workflow remains meanwhile.

An append-only publication configuration preview passes the actual candidate
parser: 11 assembly plans, 38 OCI targets, 49 release files and one npm target.
It adds independent SDK/database/fixture version sources and 41 Cockpit files,
including all eight binaries, platform archives, Mac apps, checksums and
per-binary inventories. No credential was read and no publication occurred.
The eventual Cockpit tag commit and current configuration must be refreshed
before application; npm/index settings require the new runtime first.

Manual maintenance can now be requested without disconnecting automatic CI in
PR #6200. Existing event keys deduplicate repeated requests across replicas;
the separate `/ manual` check cannot satisfy PR or merge-candidate requirements.
Connected pipeline settings offer Run with a default-branch choice. Compiler and
run-driver race tests, 14 UI behavior tests, typechecking and SDK drift checks
pass. The browser fixture verified desktop/narrow layouts in light/dark,
keyboard focus, cancellation, submission and retained connection state. This is
not yet deployed.

The production disk-packing change is instance PR #233, revision
`ad95cfcbc6c6457c6245da4b4ae357f26ca27c83`, Healthy/Synced. New command/container
workspace budgets are 15 GiB; old Jobs retain their declared budgets. The overall
64 GiB disk quota, CPU/memory/pod ceilings and maximum of one build node remain.
Instance PR #232 preserves that change while adding docs delivery and declared
non-preempting priorities. Temporary priority changes for the two migration
repairs affect only pinned, suspended, owned attempts and admit at free capacity;
they neither preempt work nor increase quotas.

## Observed baseline, October 16, 2026 UTC

The instance repository is `znasllc-io/memql-znas`, revision
`53684643847ea46d536ff13ce5aa3635a11ae743`. Its engine release is **v0.29.3**,
built from `3a91aa24081fa26954d008e39f139489dbd21266`. The seven live engine
digests match `deploy/releases/engine-0.29.3-manifest.json`. This includes the
completed pipeline fixes; the older release and ARM-only observations on the
issues are superseded.

| Item | Measured state | Remaining work |
|---|---|---|
| Kubernetes | AKS 1.35.7; Ubuntu 24.04.5; containerd 2.3.3 | Prove the fixed rootless image-builder profile on this runtime |
| Serving topology | Three D2as_v7 mesh nodes, one D2as_v7 database node; two Workbench replicas | Add independent build capacity |
| Workbench identity | `memql-engine-workbench` | Verify its required cloud federations, without granting step pods engine authority |
| Networking | Azure CNI Overlay, Azure dataplane, no policy engine | Supported Cilium migration and positive/negative live probes |
| Pipeline capacity | One Job; 8 GiB workspace; no dedicated selector; default two-hour ceiling | Rendered instance patch: two Jobs, 20 GiB workspace, `builds`, eight hours on agent and Workbench |
| Cache | Bound, 50 GiB RWX, Azure Blob NFS | #5813: permissions and concurrent Go use across different nodes, then retention |
| GitOps | Engine and product applications Synced/Healthy | Verify again after each infrastructure and application change |
| Database | One ready CNPG instance; recent Barman plugin backups completed | Fresh backup, archive verification and recovery readiness before node reimaging |
| Branch protection | `ci-required` required; merge queue enabled | Add the actual production MemQL check only after it reports reliably |
| Azure quota | Regional and Dasv7-family vCPUs both 8 used / 10 allowed | Increase both before allocating a build node or simultaneous pool surges |

The local merged-main run on `d74ddfc3ec85da7fbe0e6431a91199aa43693a42`
passed 41 steps in 7h19m59s, with 121 retained files read back and verified.
Its [evidence](https://github.com/znasllc-io/memql/issues/6018#issuecomment-6206687900)
qualifies local portable execution. It does not qualify production execution,
release publication, deployment, notification, or complete Actions coverage.

## Applied infrastructure and capacity decision

The instance PR [memql-znas#209](https://github.com/znasllc-io/memql-znas/pull/209)
carries the current configuration, measurements and maintenance receipts. The
initial baseline above is historical; read live state before each rollout.

Azure now has a `builds` user pool, **Standard_D8s_v6, 8 vCPUs / 32 GiB**,
Ubuntu, managed 128 GiB OS disk, autoscale **0–1**, scale-down mode `Delete`,
maxSurge=0/maxUnavailable=1. It was created at zero nodes. The label and matching
NoSchedule taint are `memql.io/pipeline-pool=builds`. The Dasv7 quota increase
was refused; the existing Dsv6 quota is 10 vCPUs and regional quota is now 48.
No additional serving or user-workbench pool was purchased.

Heavy pipeline steps reserve three CPUs, leaving room for two steps, sidecars
and system overhead on the bounded pool. Live allocation, cold start, mixed-load
throughput and idle deletion are still acceptance checks. The indicative
compute-plus-OS-disk rate is $0.430/node-hour; the $150/month CI target is not
a forecast or billing cap. With a provisional $10 storage allowance it permits
about 326 active hours/month. Measure rather than assuming that demand.

The pre-maintenance CNPG backup completed at 05:58:56 UTC on October 16;
its isolated restore verified a writable 20.46 GB database and 1,603,793
MemoryNodes. Temporary restore resources were removed; the backup and original
PVCs remain. The Cilium migration completed successfully. Postgres shut down cleanly
at 06:30:27 and accepted connections at 06:39:00. The OS returned HTTP 200 and
database monitoring reported healthy with no WAL archive backlog. All nodes
returned Ready and the endpoint probe passed 24/24 requests. The temporary DB
pool maintenance settings were restored to maxSurge=10%/maxUnavailable=0.

Instance [PR #211](https://github.com/znasllc-io/memql-znas/pull/211) replaced
the disposable Blob NFS cache with a 64 GiB Premium SSD claim. Two concurrent
small Go builds measured 163/166 seconds cold and 28.3 seconds warm on NFS,
versus 18.3/18.3 seconds cold and 0.88/0.93 seconds warm on SSD, producing
identical binaries. The SSD is $10.207/month at the observed East US list price,
within the provisional storage allowance. It supports concurrent Jobs on the
single build node; revisit the RWO topology before increasing the pool maximum.
Live ownership/isolation checks passed after Argo reconciled the replacement.
This fixture does not establish full-suite throughput or an overall bill.

The `v0.29.7` bootstrap release at `5a0bcabf14abbec61caca405fd988a5161516e50`
started through MemQL at 09:03:35 UTC on October 16. Its
[check](https://github.com/znasllc-io/memql/runs/116897094312) records pipeline
run `4aae632a393d46284de90ff27b811f50`. Local MemQL controls this bootstrap;
Jobs run on the dedicated AKS pool through a temporary namespace-scoped grant.
That is distinct from installed-production qualification. The serving release
remains v0.29.5 until verified native image publication and GitOps deployment.

The cold-probe fix [#6133](https://github.com/znasllc-io/memql/pull/6133) and
Workbench routing/capacity [#6136](https://github.com/znasllc-io/memql/pull/6136)
are merged. They require deployment and production verification. The cadence
change adds daily dependency audits and weekly history/CodeQL, with separate
check names and cross-replica weekly deduplication. PR/queue/push secret scans
cover every newly introduced commit; saved comparison bases also close the
authoring-surface parity gap. No scheduled scan may satisfy a required PR check.

## Workflow retirement ledger

Inventory uses the engine tree at the source revision above and GitHub's live
workflow API, including generated workflows. The retirement state below was
rechecked on October 18. A replacement needs its trigger, execution, credentials,
outputs, publication and failure evidence, not merely a command in a manifest.

| Engine workflow | Replacement or remaining evidence | Owner |
|---|---|---|
| `ci.yml` | Native package adds editor tests/packaging and the rootless OS Docker stage, retains per-package DB budgets and carries PR bases. Editor hosts plus production PR and exact merge-group proof remain | #5506, #5507 |
| `gitleaks.yml` | Existing current-tree and history scans; production event/cadence evidence | #5506 |
| `govulncheck.yml` | Disabled on October 18; definition removed by this change. Native `vulnerabilities.go-vulnerabilities` passed in release `b0643687e51532322e50eb0ff6014ab3` and scheduled run `3443e867a582c3ee5053aa23511dcf41`, retaining a log and all three reports in each. The pinned v1.7.0 scanner still covers every workspace module and fails on findings. Compiled PR/push/queue/release/schedule selection remains tested | #5506 |
| `codeql.yml` | Existing Go/JS/Python analysis; journaled SARIF upload and receipt still needed | #5480 |
| `sbom.yml` | Disabled on October 18; definition removed by this change. Native `inventory.sbom` passed in release `b0643687e51532322e50eb0ff6014ab3`, retained 19 files and log `94d6ed5d-4a52-489c-a39e-f1e7703a4c66`. Summary file `06ffedd8baf50f1ff95b42134bf9a8bcd266addea103092afbe8b291da76266e` reports complete coverage; Go JSON/XML and npm lockfile reports are readable in the Library. Release/main-push/weekly/manual selection and missing-module/package refusal tests pass. The old workflow only retained artifacts; it never attached release assets | #5480 |
| `scorecard.yml` | Scorecard execution, permissions, branch-rule/scheduled triggers and SARIF publication | #5480 |
| `build-engine-images.yml` | Disabled and definition removed. Native release `b0643687e51532322e50eb0ff6014ab3` qualified all seven images; exact source, role, release and registry digests were verified before production rollout. Runtime source/version stamping, immutable publication and per-role build guards remain | #5509 |
| `dispatch-engine-images-on-release.yml` | Disabled and definition removed. MemQL pins explicit release requests to the tag commit and deduplicates webhook delivery; native release tests cover both paths | #5509 |
| `recover-engine-image-mirror.yml` | Disabled and definition removed. Reconcile immutable digests through the native publication journal; the Actions-only recovery wrapper is removed; native OCI verification and publication tests retain the provenance, digest and conflict checks | #5509 |
| `build-toolchain-image.yml` | Build and publish future toolchains without Actions | #5509 |
| `build-db-image.yml` | Native `tests.database-image` selects the verified operand target for changed inputs and full qualification; live native execution and separate registry publication remain | #5509 |
| `mirror-fixture-images.yml` | Digest-verified fixture mirroring without Actions | #5509 |
| `deploy-gate-image.yml` | Removed after installed-production run `3f3af7ca78ce6e9529aafa08f4bfa593` passed the unchanged build and retained its log, OCI archive and metadata | #5509 |
| `install-e2e.yml` | Native host lifecycle matrix and cleanup | #5480 |
| `install-cluster-e2e.yml` | Installed-cluster lifecycle and cleanup; preserve actual failure evidence | #5480 |
| `pipelines-live.yml` | Removed unused manual placeholder: zero historical runs, command only refuses missing prerequisites; no runtime coverage lost | #5480 |
| `proving-live.yml` | Removed unused manual placeholder: zero historical runs, live provider remains unconfigured; no provider qualification claimed | #5480 |
| `publish-docs-bundle.yml` | Native release-files builds and verifies the exact-version bundle with complete Git history; release attachment and downstream delivery receipt remain | #5508, #5712 |
| `docs-current-check.yml` | Native weekly public docs check declared; installed schedule/failure delivery proof remains | #5508, #5712 |
| `publish-sdk-core.yml` | Native release-files retains the compiled, version-checked npm archive; registry publication and immutable receipt remain | #5480 |
| `publish-vscode-extension.yml` | Retired unused Marketplace publishing. Its only successful historical run, `37052150601`, was a dry run; `38112601591` failed every publishing target with an empty `VSCE_PAT`. Neither registry listed `znasllc.memql` on October 18. Native `release-files.packages` retains and verifies Linux x64/ARM64, macOS x64/ARM64 and web VSIX archives. First-time Marketplace/Open VSX account setup and publication are separate; no registry publication is claimed | #5480 |
| `vscode-site.yml` | Retired after native release `b0643687e51532322e50eb0ff6014ab3` passed `tests.editor-site` in 13 seconds: build, self-check, complete Chromium suite, 25 retained artifacts and readable log `73f4664f-80ab-4a7b-a1c0-9705832da015`. The live page at `vscode.memql.io` matches the native HTML SHA-256 `6d7fd2c949a1c7e8f6ffb6f9fa9895a2991a3f2060387af76e6b0a3503f45c05` after the edge's standard refresh-script injection. Source is the deployed `1d087ac973e5fd1e620f99f93ef42dba1f3a3642` | #5480 |
| `ruleset-drift.yml` | Removed after installed daily run `3443e867a582c3ee5053aa23511dcf41` passed both baselines, retained both logs and reported separate check `MemQL / memql / daily`; negative controls pass ([receipt](https://github.com/znasllc-io/memql/issues/5507#issuecomment-6245341749)) | #5507 |
| `dependabot-tidy.yml` | Trusted dependency-PR maintenance, without exposing write authority to PR code | #5480 |
| `clustere2e-compile.yml` API entry | Disabled October 17 at 14:05 UTC: file absent at `f8ece576d`, only two historical runs and no active execution. Current CI and native `checks.build-clustere2e` retain compilation. Required `ci-required` and run history preserved | #5480 |
| Generated `github-code-scanning/codeql` | Reconcile GitHub default setup with advanced scanning and replacement publication | #5480 |
| Generated `github-code-quality/codeql` | GitHub Code Quality is distinct from security CodeQL; equivalent coverage or an explicit owner decision is required | #5480 |
| Generated Dependabot updates / graph | Retain useful GitHub dependency services per the design; explicitly resolve their managed Actions execution before claiming zero Actions dependency | #5480 |

Companion inventories are part of the cutover, rather than an implicit engine
success. The live API reports nine instance workflows, ten Cockpit workflows
(including two generated Dependabot entries), and seven template workflows.

| Repository | Workflows still on Actions | Tracking |
|---|---|---|
| `memql-znas` | `ci`, `bash32`, `gitleaks`, `instance-ci`, `package-config`, `publish-images`, `engine-version-watch`, `template-drift`, `docs-sync` | #5480/#5508; instance #164 for automatic site delivery |
| `memql-cockpit` | `ci`, `codeql`, `gitleaks`, `govulncheck`, `install-scripts-lint`, `release`, `sbom`, `scorecard`, generated Dependabot updates/graph | #5480; native host consent/signing remains a separate qualification |
| `memql-project` | `ci`, `deployment-observer-tests`, `engine-version-watch`, `gitleaks`, `publish-images`, `template-ci`, `template-drift` | #5480/#5508; change shared operational files in the template first |

## Portable coverage additions

The native `editor-packages` step runs LSP drift guards, all VS Code unit tests,
the hosted browser editor build/typecheck/tests, Productivity unit tests and
Linux VSIX packaging. The exact command passed in the pinned toolchain as uid
1000: 2,840 extension tests, 14 browser tests and 325 Productivity tests, plus a
36.91 MB VSIX whose 55 staged scripts retain executable modes. This is portable
command evidence; it does not qualify the separate desktop/web host matrices.
The first local harness attempt failed because its bind mount created a
root-owned parent cache directory; the corrected harness mounts the Go cache
under `/cache`, like the actual runner.

The package also declares the eight cases from the existing editor host
matrix: VS Code desktop at 1.91.0 and stable on Linux, plus Productivity web
at stable and desktop at both versions on Linux and macOS. Linux cases stay
in bounded cluster containers. The three macOS cases explicitly require a
native `darwin/arm64` Fleet worker with `macos_tooling`; the pipeline connection
must authorize cluster and Fleet execution, and the machine's local repository
allowlist must admit this repository. Accessibility is not needed for these
command-line host tests.

`scripts/ci/editor-host.sh` builds the shared SDKs and extensions in each
checkout, stages the language server for VS Code tests, and preserves the
selected host, runtime, exit status and failure diagnostics. macOS caches and
browser downloads stay in the attempt checkout. A checksum-pinned official
Node 22 macOS archive is also unpacked there, so a missing or different system
Node does not require installing software into the owner's environment.
Dependency downloads have
bounded retries; test assertions run once. The manifest regression test
compares all eight cases with the retained workflow and checks changed-source,
exact merge candidate, release and scheduled-audit selection.

Both Linux VS Code versions passed locally with 20 host assertions each.
The twelve credentialed live-cluster cases were skipped, as in the existing
uncredentialed host lane; this is not live-cluster proof. The Productivity
Chromium host and both Linux desktop versions passed; the browser lane also
packaged its VSIX. Each Productivity case ran all 325 unit tests first. Native production execution and macOS host evidence remain
required before retiring the corresponding Actions lanes.

`os-image` uses the fixed rootless image builder for Dockerfile's `spa-build`
stage, retaining the OCI archive and metadata. Its path bucket includes the
Dockerfile and the OS sources. Its production execution remains required.

The DB step retains the exact package selection and five isolated Postgres
services, and groups each shard's packages by the CI planner's budget classes.
Each class gets a fresh database within its shard's service, then drops only
that successfully created database after its tests exit. Fixture rows and
planner statistics from one class cannot consume another class's test budget.
All invocations stay serial and uncached; fast suites keep 180 seconds,
packages 300 seconds, and engine/campaigns 600 seconds. An empty selection,
missing required-database setting or failing test refuses. The class table
remains in the checked-in workflow while both definitions are retained; reading
it requires no Actions execution. Tests exercise failure propagation and every
current class, and the command passed against a real database.

The release-only `release-files.packages` step builds the documentation bundle,
SDK npm package, and Linux x64/ARM64, macOS x64/ARM64 and web VSIX archives.
It checks the pinned commit and engine version before building, fills shallow
history for page dates, and retains seven verified files plus a checksum
manifest. Documentation metadata and npm package identity must match the source.
Native VSIX files must contain exactly their target's executable LSP, verified
from ELF/Mach-O headers; web archives must carry a browser entry point and no
native binaries or installer scripts. This packages macOS binaries by cross
compilation; it does not replace running the macOS host tests.

The complete command passed as uid 1000 in the pinned toolchain at
`5a0bcabf14abbec61caca405fd988a5161516e50`: 130 documentation pages, SDK 0.8.0
and all five editor 0.6.11 archives. It held no publication credentials and
published nothing. Future native execution and approved publication are still
required. Separate SDK/editor tag triggers and registry receipts remain in the
retirement scope; an engine release's package build is not their replacement.

The database operand now has a `verified` Docker target that runs the same
smoke script used against loaded local images. It starts a disposable Postgres
as uid 26, verifies TimescaleDB Community, pgvector, continuous aggregates,
compression, retention and the previous-to-current extension upgrade. The
script and database are temporary build mounts and do not enter an image
layer. The default development target remains the operand, preserving the
existing local smoke-test option. Native `tests.database-image` builds the
verified target when its inputs change and retains the unpublished OCI archive.

Local validation passed against the existing operand and inside the exact new
Docker target. A wrong expected current version fails; the resulting verified
image remains uid 26 and contains no test script or database. Manifest selection,
version drift, Bash syntax and ShellCheck pass. This is preparation, not native
cloud or publication evidence. The Actions database publisher stays active
until independent version selection, verified native execution and registry
publication receipts replace it. No production database image is changed.

## Publication qualification, October 17, 2026 UTC

The installed v0.29.20 instance completed the native v0.29.22 release run
`3f3af7ca78ce6e9529aafa08f4bfa593`, Work run
`5e3ee9731c745417a39e3b8fef3101ee`, from
`7801fddd1a29c15f509a66e076362e1f6633b959`. All 46 steps passed between
07:45:23 and 09:39:58 UTC: **114m35s elapsed**, including capacity waiting.
The GitHub summary's stage-duration sum is not the wall-clock duration.
All 46 logs and 66 artifact references were retained. Three logs reached
their configured size cap; none reported missing artifact paths or storage
ownership refusal. The Files app returned the DB shard log, including Work's
130.607-second pass, and the release manifest whose seven file checksums
match their retained Library rows.

Native candidate assembly and separate approval succeeded. Publication then
stopped with a durable pending intent at the first Azure and GHCR images.
Neither has a verified completion receipt. Empty upload-session probes found
the protocol differences: Azure preserves a `_nouploadcache=false` query
parameter, and GHCR returns a singular `/blobs/upload/<id>` session path.
Azure documents its flag in the
[Start Upload contract](https://learn.microsoft.com/en-us/rest/api/registry-dataplane/container-registry-blob/start-upload).
The original transport rejects both; regression tests reproduce the same
uncertain-publication outcome. The fix accepts those exact session forms
while retaining origin, repository, digest, method and query bounds.

The 578 MiB toolchain archive also exposed a download failure. Both HTTP
hops use a 15-second default write deadline. Slow-transfer tests reproduce
truncation at the serving handler and across the Edge proxy. The repair
uses a 30-second idle write deadline and a 30-minute total ceiling on this
download path, preserves earlier caller cancellation, and propagates
`http.ErrAbortHandler` without appending JSON to partial file bytes. The
browser keeps its download iframe until the body finishes, then cleans it up.
These tests pass with the repair and fail against the previous implementation.

The repair merged through [#6177](https://github.com/znasllc-io/memql/pull/6177)
at `f8ece576d93efdfb899fb3c4133f895c304f7e60`, tagged v0.29.23. Its required
CI passed. At 13:56:45 UTC the rebuilt local MemQL controller started native
release run `517b203755fd72a42c422493c691ad77`, Work run
`4e28aba0e2460a1a6d482900f6d092f8`, against that exact tag. Compilation uses
the existing AKS build pool and two-Job quota. A temporary namespace-scoped
runner grant expires at 21:54 UTC; automatic delivery on this local connection
remains disconnected. This repairs publication without an Actions image build
or an operator image push. The bootstrap is not installed-production
qualification. Its temporary grant and configuration must be removed after
publication and native Job cleanup.

The v0.29.22 files candidate published all eight GitHub draft assets with
verified native receipts. Draft release `415819034` remains unpublished.
Neither failed image publication has a completion receipt. Candidate
verification binds the controller's engine commit, so upgrading it does not
by itself make an old pending intent recoverable. Preserve the old candidate,
configuration and journal; do not rewrite reservations, manufacture receipts
or relabel uncertainty as success. A reviewed cross-version recovery path is
still needed to resolve those old intents. The fresh v0.29.23 candidate uses
its own source, tag and release identities.

Deployment and live publication/download verification remain outstanding.
The serving instance remains v0.29.20, and active Actions retirement is pending.

## Native database class isolation, October 17, 2026 UTC

Release v0.29.24 run `ab281c223557cd329b72c6aa17b9cf3c`, source
`b191f80df04033ce47f188edfbb9d7bda08f5b3c`, failed `tests.db-tests#2`.
The engine suite passed in 515.507 seconds, then Library exceeded its unchanged
180-second package limit. The native shard reused one database across budget
classes, unlike the CI planner's separate Library service.

The sequence reproduces locally with two Go threads, `GOMEMLIMIT=512MiB` and
`GOGC=50`: Library passes in 127.414 seconds, but exceeds 180 seconds after the
engine suite populates that database. With class isolation, the same Library
suite passes in 118.806 seconds even when the service's original database
still contains those engine fixtures. No test selection, assertion, package
timeout or resource limit changes. Database creation, extension setup, test
failure and cleanup all fail closed; only databases created by this invocation
are removed. Connection validation admits the disposable loopback service and
rejects libpq routing overrides.

Prepare v0.29.25 with this runner repair and the already-merged Ask evidence,
research reuse and query-pressure fixes. Its full native release, publication
receipts and production verification remain required. The failed v0.29.24 run
is diagnostic evidence, not a release candidate.

## Azure publication readback, October 17, 2026 UTC

The v0.29.23 native bootstrap run `517b203755fd72a42c422493c691ad77`,
Work run `4e28aba0e2460a1a6d482900f6d092f8`, passed all 46 steps from
source `f8ece576d93efdfb899fb3c4133f895c304f7e60`. Its controller ran in
local k3d and its build jobs used the existing AKS builds pool. This is
bootstrap evidence, not qualification on the installed production controller.
All 46 logs and 66 artifact references were retained without missing paths or
storage-owner failures. The 606,256,128-byte toolchain archive downloaded
through the local Files app, matched SHA-256
`052e2cbf771cc158fd0b660afb0956abdb0f8b15d65656bd11bf48a8bfc2b550`,
and passed OCI verification and the toolchain's actual-image smoke script.
Production download verification remains outstanding.

Native GHCR publication completed and verified the agent image. Azure accepted
that image's immutable manifest, but verification stopped before its version
tag was written. Azure's signed blob URL contains an empty path segment
(`//`), which the reader incorrectly rejects as a noncanonical filesystem
path. Storage object keys must retain those segments and the signed query.
The repair leaves exact HTTPS origins, credential isolation, rejection of dot
and escaped path segments, redirect bounds and content hashes enforced.
Publication/replay and read-only availability tests fail on the old reader
and pass with this change; the complete registry suite passes with the race
detector. The repaired native reader also verified all 20 config/layer blobs
of the actual Azure agent image through its credential-free storage redirects.
Prepare v0.29.26 with this readback repair and the merged database class
isolation. Existing pending publication intents remain pending until a
native operation verifies and records completion; never manufacture receipts
or reuse their immutable tags for different bytes.

## Native toolchain publication, October 17, 2026 UTC

The v0.29.23 bootstrap published and verified all seven GHCR engine images
through candidate
`sha256:184bc0ea33d995d1d82b3feb86d81c106d77fe3ce8744899f5fabf9e9b2990da`.
Each approved effect has a native completion receipt. Its Azure agent intent
remains pending; this does not claim a completed cloud release.

Toolchain candidate
`sha256:27c6c1aaaa081a2f8539f8f9190457ad6f8fb1d1e9bf7b1b159df3ab24d9ffa5`,
approval `b3ca06b3-dc1c-4889-a62e-2aeaa2cc6043`, completed native publication
to GHCR with immutable tag `0.29.23` and image digest
`sha256:a62b53491fbd9cfd4bbfdf9612bfb1776d9fc9d42aab7ceceac3a0a18a17a8bb`.
The 13-layer manifest is also readable anonymously and matches that digest.
The pipeline now pins this image, whose downloaded archive passed the full
matching smoke script. Five Linux editor host commands have passed their
local execution checks; all eight native host cases still require installed
pipeline execution, including the three Mac cases. Legacy host checks remain
until that qualification completes.

The Azure reader repair is merged in #6180. The later v0.29.26 run failed
artifact capture and did not qualify a release; the subsequent repair and
qualification are recorded below.

## Artifact transfer integrity and urgent cutover, October 18, 2026 UTC

Native v0.29.27 run `e60862099a9ba0108681d5be1311b4d2`, source
`90e82d606e7620596a4be83242a4d80ad2f4de74`, ended with 36 successful
steps, one failure and nine skips. `tests.os-image` exited zero, but its
required artifact failed with `unexpected EOF`. It is not a release candidate.
Production remains v0.29.20; the bootstrap controller and AKS jobs are not an
installed production qualification.

PR #6187 reproduces a reader accepting 1,024 bytes as a complete file when
exec returned success and closed normally. This does not prove the exact cause
of the live failure, whose producer has already been cleaned up. Its repair
measures the immutable source length through a framed, bounded, read-only stat
command, requires every range's expected length and retries incomplete ranges
within the existing three-attempt bound. Required artifacts still fail the
step when capture fails. Tar errors now identify the file and received versus
expected bytes. Prepare v0.29.28 from this repair and #6185.

The short-success regression fails before the repair and passes after it;
permanent truncation fails closed. Both complete affected package suites pass.
The local Kubernetes fixture transfers and hashes 128 MiB through the resumable
reader, parses its tar snapshot and rejects oversized or missing files. Its
optional object-storage leg was not configured and skipped. An independent
integrity investigation reproduced the same truncation defect and passed a real
16 MiB export through two collectors; its additional checksum changes are
preserved locally, not part of this release. Reuse #6187 rather than merge two
competing repairs.

The native #6187 check failed with `no space left on device` in the shared build
cache, as did unrelated PRs. Its required repository checks passed; neither
that result nor local transfer tests substitute for a successful native release.
Recover the disposable build cache without changing serving allocations, then
verify sufficient bytes and inodes before admitting another release. The
prepared quiescent cache-GC configuration must ship with queue-aware runners;
do not delete caches while old Jobs can still use them.

The explicit-release retry repair in #6185 passed native run
`d1c444b15d6c90120c26f60f0e57d207` (31 successful steps, four expected
skips) and merged as `59613d576bb71ee40873bc1655076bc0394fbc51`.
It preserves disconnected automatic delivery while allowing an owner's
explicit release retry. It is not deployed yet.

Prioritize the native release path: qualify these repairs, build and publish
one complete native release, pin its verified digests, and verify production
rollout and large artifact download. In parallel, complete #6181's real Mac
editor hosts and Cockpit platform qualification. Then qualify the exact merge
candidate, change required checks to MemQL, retire each proven-replaced Actions
workflow and close issues with the receipts below. Keep the existing single
build node maximum and three-Job quota; queue excess builds and preserve user
workbench headroom. Do not increase serving resources, bypass failed checks or
require three consecutive releases. Keep the current production pins available
for rollback and retire temporary bootstrap grants only after owned work ends.

## Acceptance and issue closure

The owner merge bypass now preserves a required check's configured GitHub App
binding. It reads every check page for the exact PR head and uses the newest
attempt from that App; an unavailable read, same-name check from another App,
wrong commit or cancelled latest attempt cannot authorize a merge. Behavioral
regressions reproduce the old false acceptance and pass with the repair. The
whole development-script suite passes. The live native check is `MemQL / memql`,
reported by App 4822538 (`MemQL for ZNAS LLC`), verified on check run
117531817586. Bind the eventual required native check to that App through
[`integration_id`](https://docs.github.com/en/rest/repos/rules), after the
production and exact-queue qualification below; the ruleset still requires
`ci-required` at this point.

The editor landing page now has a native validation declaration covering its
source, themes, shared brand, canonical research example and runner scripts.
It uses the native-published browser-capable toolchain and an isolated browser
download, with two CPUs, 4 GiB and fifteen minutes. The unchanged browser suite
passes locally in that exact image as uid 1000 with no extra capabilities,
including five viewport widths, theme persistence, keyboard/clipboard behavior,
contrast, CSP and the no-JavaScript fallback. The built 19-file page and review
screenshots are retained. This is local command evidence, not installed cloud
qualification; keep the existing site workflow until its native run passes.

The public repository audits now have native declarations: both ruleset
baselines run daily in separate steps, and documentation freshness runs weekly.
They run after failures and remain absent from PR, merge-group, push and release
checks. Public reads use no stored token, refuse redirects and incomplete tag
inventories, and retain the scripts' existing missing-release and stale-site
failure behavior. Fixture tests cover those failures and the public transport.
Both live ruleset reads passed on October 17. The live docs audit correctly
failed: v0.29.26 still had no published docs asset, and memql.io served v0.24.0.
These are command checks, not installed scheduler or notification qualification;
the corresponding Actions workflows stay active until those receipts exist.

The cadence change removes the multi-hour full-history scan from ordinary
merge groups and main pushes. Validate the remaining exact-tree functional
work against the merge queue's sixty-minute budget on the actual build pool.
Neither skipping required coverage nor extending the timeout alone demonstrates
usable throughput. Daily and weekly audit results stay separate from required
PR/queue status, and failed audits remain visible until repaired.

Keep #5504 (notification) and #5505 (external rollout verification) attached to
their typed implementations and real delivery evidence. Keep #5813 for Azure
cache behavior, #5835 for replay/recovery, and #6041 if source onboarding hits
the reserved-domain failure. The new cold-start defect has its own #6132;
an implementation PR alone cannot close its live AKS qualification.

M1 (#5506) requires a real production PR check, retained logs/artifacts and
false-green controls. M2 (#5507) requires the exact merge candidate, a required
MemQL context and rejection of a failing candidate. M3 (#5508) requires one
complete release/deploy/docs delivery and targeted rollback/recovery evidence.
M4 (#5509) retires remaining Actions only after native image build/publication
and independent bootstrap/recovery work. This follows the owner's current
one-pass direction in place of the earlier two-week/two-week/three-release
observation proposal. Coverage and failure evidence remain required.

Every closure must link a merged implementation and actual acceptance evidence.
A skipped lane, missing artifact, absent status check or fixture-only receipt
is not a passing production check. Record source SHA, executing release,
installation, run/check IDs, output checksums, failure/recovery observations
and rollback version. Re-enable the previous trigger path if a replacement
fails during the observation period, while preventing competing publishers.


## v0.29.29 qualification candidate

The next native release combines the complete editor-host matrix, the verified
DB-image target, the editor-site browser suite and release/main/weekly SBOM
inventory. Both core Linux host versions pass with the temporary HOME guard
preserved; the short profile path has a real Unix-socket regression. Native
macOS execution must pass before this candidate qualifies.

The failed v0.29.28 release was cancelled without a candidate or publication.
Its Work package hit the 180-second package alarm. A focused unchanged run on
the same bounded AKS runner passed all 809 cases in 176.217 seconds, with no
skips, OOM events or material CPU throttling; its retained report was read back
and checked by size and SHA-256. Local baseline, source-scan memoization and a
third CPU measured 93.590, 93.087 and 94.298 seconds respectively. The latter
two changes are not adopted. Only Work's package ceiling becomes 300 seconds;
its operation deadlines, every test, fresh database, resource reservations and
overall lane deadline remain. Passing execution exits immediately.

Four already-disabled Actions definitions are removed here: deploy-gate image
validation and the two daily ruleset audits have installed native receipts;
the two live placeholders had no runs and only refused absent prerequisites.
No live-provider qualification is claimed. Required `ci-required` protection
remains until the native App's exact PR and merge-candidate evidence qualifies
its replacement. Consolidate this candidate before one full release pass;
verify publication receipts, GitOps health and public behavior before closing
the cutover issues or removing the remaining workflows.
