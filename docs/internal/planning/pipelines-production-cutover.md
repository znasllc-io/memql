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
The previous local-only phase has ended; production migration preparation is
authorized. The specific Azure capacity and disruptive networking operation
below remain proposals, not executed changes.

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

## Capacity and networking proposal

Use one Linux amd64 `builds` user pool, D8as_v7 (8 vCPUs, 32 GiB), with
autoscaling 0–1, a 128 GiB managed OS disk and scale-down mode `Delete`.
The subscription's SKU response lists no restrictions for this size in East
US, but that does not reserve allocation capacity. This SKU does not support
ephemeral OS disks. Set both label and matching `NoSchedule` taint to
`memql.io/pipeline-pool=builds`; Workbench already selects and tolerates exactly
that pool. Keep the existing shared cache PVC. Do not move builds onto serving
nodes if capacity is missing.

Request regional and Dasv7 quotas of **32 vCPUs**. Eight existing plus eight
build vCPUs use sixteen normally; a build-node surge and one two-vCPU surge
in each existing pool could require twenty-eight. Quota is headroom, not a
request to run thirty-two vCPUs. The steady build-pool maximum remains one;
an explicitly managed upgrade can temporarily add a surge node.

East US public pay-as-you-go prices observed through the
[Azure Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices)
on October 16: Linux D8as_v7 **$0.363/hour**, P10 LRS 128 GiB OS disk
**$19.71/month**, premium LRS block-blob storage **$0.15/GB-month**. At 730
node-hours, compute plus one OS disk is approximately **$284.70/month**.
At 240 node-hours, an indicative hourly-prorated compute/disk estimate is
**$93.60/month**. A full 50 GB cache adds approximately **$7.50/month** of
stored bytes. These estimates exclude taxes, transactions, network transfer,
temporary surge capacity and retained Library artifacts; actual billing must
be measured. The cache is persistent and its PVC request is not a retention
policy. Use deletion, not deallocation, and verify idle VM and OS-disk removal.
[Azure scale-down behavior](https://learn.microsoft.com/en-us/azure/aks/scale-down-mode).

There is a cold-start prerequisite, [#6132](https://github.com/znasllc-io/memql/issues/6132):
the released isolation probe waits only 90 seconds for a node. The proposed
fix permits ten minutes for its credential-free listener, retains the later
90-second readiness and 60-second network-test windows, and bounds an orphan
probe Job at thirteen minutes plus its one-minute TTL. Ship that fix before
qualifying automatic zero-to-one operation. A manually warmed node can support
the initial controlled rehearsal; it does not prove autoscaling.

The supported networking change is Azure CNI's Cilium dataplane, preserving
the existing overlay CIDRs. Microsoft documents that this operation reimages
**all node pools** and enforcement starts after reimaging finishes. It cannot
be staged one pool at a time. The single database instance means planned
service interruption is possible. A GitOps revert does not undo this Azure
networking operation; no instant reverse migration is assumed.
[Microsoft migration procedure](https://learn.microsoft.com/en-us/azure/aks/update-azure-cni#update-the-data-plane-to-azure-cni-powered-by-cilium).

Before the maintenance decision, verify the current-generation backup and WAL
archive and review the instance's recovery procedure. Audit all existing
NetworkPolicies, since they become active together: the observed policies
cover ArgoCD, konnectivity and pipeline isolation. Preserve the database PVC
and archive. Do not add broad CIDR exceptions to make a failed probe pass.
After reimaging, verify DNS, ingress, identity, mesh, database, ArgoCD, storage
and the pipeline's positive and negative controls before connecting repository
triggers. The instance PR carries the exact configuration and execution order.

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

Keep #5504 (notification) and #5505 (external rollout verification) attached to
their typed implementations and real delivery evidence. Keep #5813 for Azure
cache behavior, #5835 for replay/recovery, and #6041 if source onboarding hits
the reserved-domain failure. The new cold-start defect has its own #6132;
an implementation PR alone cannot close its live AKS qualification.

M1 (#5506) requires the released production instance's check beside
`ci-required` for two weeks, with false-green accounting. M2 (#5507) requires
two weeks of exact merge-queue results before retiring the corresponding
Actions triggers. M3 (#5508) requires three verified releases before replacing
the deployment observer and docs delivery. M4 (#5509) retires engine CI and
image-building last, after independent bootstrap/recovery is proven. These
are the existing issue criteria, retained unless the owner explicitly changes
them. The one-full-pass direction for the prior local phase does not silently
rewrite production observation periods.

Every closure must link a merged implementation and actual acceptance evidence.
A skipped lane, missing artifact, absent status check or fixture-only receipt
is not a passing production check. Record source SHA, executing release,
installation, run/check IDs, output checksums, failure/recovery observations
and rollback version. Re-enable the previous trigger path if a replacement
fails during the observation period, while preventing competing publishers.
