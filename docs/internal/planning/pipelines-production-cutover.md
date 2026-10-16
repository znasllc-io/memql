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
PVCs remain. The Cilium migration is in progress. Postgres shut down cleanly
at 06:30:27 and accepted connections at 06:39:00. The OS returned HTTP 200 and
database monitoring reported healthy with no WAL archive backlog. Complete all
node and serving checks before introducing CI load. Restore the temporary DB
pool maxSurge=0/maxUnavailable=1 to its prior 10%/0 after migration.

The cold-probe fix [#6133](https://github.com/znasllc-io/memql/pull/6133) and
Workbench routing/capacity [#6136](https://github.com/znasllc-io/memql/pull/6136)
are merged. They require deployment and production verification. The cadence
change adds daily dependency audits and weekly history/CodeQL, with separate
check names and cross-replica weekly deduplication. PR/queue/push secret scans
cover every newly introduced commit; saved comparison bases also close the
authoring-surface parity gap. No scheduled scan may satisfy a required PR check.

## Workflow retirement ledger

Inventory uses the engine tree at the source revision above and GitHub's live
workflow API, including generated workflows. **None of these rows is approved
for retirement yet.** A replacement needs its trigger, execution, credentials,
outputs, publication and failure evidence, not merely a command in a manifest.

| Engine workflow | Replacement or remaining evidence | Owner |
|---|---|---|
| `ci.yml` | Existing 41-step package; close editor-host, OS Docker-stage, PR-base and DB-plan gaps; production PR and exact merge-group proof | #5506, #5507 |
| `gitleaks.yml` | Existing current-tree and history scans; production event/cadence evidence | #5506 |
| `govulncheck.yml` | Existing scan; production merge-group/push/schedule evidence | #5506 |
| `codeql.yml` | Existing Go/JS/Python analysis; journaled SARIF upload and receipt still needed | #5480 |
| `sbom.yml` | Existing portable generation; release asset publication and matching event coverage | #5480 |
| `scorecard.yml` | Scorecard execution, permissions, branch-rule/scheduled triggers and SARIF publication | #5480 |
| `build-engine-images.yml` | Seven native engine builds on the released instance; immutable publication and digest/provenance receipts | #5509 |
| `dispatch-engine-images-on-release.yml` | MemQL release trigger and publication ownership, including duplicate-event recovery | #5509 |
| `recover-engine-image-mirror.yml` | Reconcile immutable source/destination digests after interrupted publication | #5509 |
| `build-toolchain-image.yml` | Build and publish future toolchains without Actions | #5509 |
| `build-db-image.yml` | Build and publish the database image without Actions | #5509 |
| `mirror-fixture-images.yml` | Digest-verified fixture mirroring without Actions | #5509 |
| `deploy-gate-image.yml` | Build-stage and image evidence for changed deploy inputs | #5509 |
| `install-e2e.yml` | Native host lifecycle matrix and cleanup | #5480 |
| `install-cluster-e2e.yml` | Installed-cluster lifecycle and cleanup; preserve actual failure evidence | #5480 |
| `pipelines-live.yml` | Disposable cluster runtime qualification and retained logs | #5480 |
| `proving-live.yml` | Credentialed proving run with explicit authorization and evidence | #5480 |
| `publish-docs-bundle.yml` | Versioned docs bundle, release attachment and downstream delivery receipt | #5508, #5712 |
| `docs-current-check.yml` | Scheduled public docs freshness check and failure notification | #5508, #5712 |
| `publish-sdk-core.yml` | Exact-version npm publication and immutable receipt | #5480 |
| `publish-vscode-extension.yml` | Editor build and registry publication; web artifact delivery | #5480 |
| `vscode-site.yml` | Static editor-site build and its deployed version proof | #5480 |
| `ruleset-drift.yml` | Scheduled policy audit; retain code-owner and required-check enforcement | #5507 |
| `dependabot-tidy.yml` | Trusted dependency-PR maintenance, without exposing write authority to PR code | #5480 |
| `clustere2e-compile.yml` API entry | File absent at this revision; verify no independent active execution before disabling the stale registration | #5480 |
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

## Acceptance and issue closure

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
