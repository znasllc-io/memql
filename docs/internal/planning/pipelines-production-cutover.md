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
workflow API, including generated workflows. **Active delivery workflows remain
until their replacement is qualified.** A replacement needs its trigger, execution, credentials,
outputs, publication and failure evidence, not merely a command in a manifest.

| Engine workflow | Replacement or remaining evidence | Owner |
|---|---|---|
| `ci.yml` | Native package adds editor tests/packaging and the rootless OS Docker stage, retains per-package DB budgets and carries PR bases. Editor hosts plus production PR and exact merge-group proof remain | #5506, #5507 |
| `gitleaks.yml` | Existing current-tree and history scans; production event/cadence evidence | #5506 |
| `govulncheck.yml` | Existing scan; production merge-group/push/schedule evidence | #5506 |
| `codeql.yml` | Existing Go/JS/Python analysis; journaled SARIF upload and receipt still needed | #5480 |
| `sbom.yml` | Native inventory now runs on release, main push, weekly and manual events; installed retained JSON/XML and complete module/lockfile evidence remain. The old workflow only retained artifacts, never attached release assets | #5480 |
| `scorecard.yml` | Scorecard execution, permissions, branch-rule/scheduled triggers and SARIF publication | #5480 |
| `build-engine-images.yml` | Seven native engine builds on the released instance; immutable publication and digest/provenance receipts | #5509 |
| `dispatch-engine-images-on-release.yml` | MemQL release trigger and publication ownership, including duplicate-event recovery | #5509 |
| `recover-engine-image-mirror.yml` | Reconcile immutable source/destination digests after interrupted publication | #5509 |
| `build-toolchain-image.yml` | Build and publish future toolchains without Actions | #5509 |
| `build-db-image.yml` | Native `tests.database-image` selects the verified operand target for changed inputs and full qualification; live native execution and separate registry publication remain | #5509 |
| `mirror-fixture-images.yml` | Digest-verified fixture mirroring without Actions | #5509 |
| `deploy-gate-image.yml` | Native deploy-gate image validation selects command/workspace changes on PRs and retains an unpublished OCI archive; cloud execution evidence remains | #5509 |
| `install-e2e.yml` | Native host lifecycle matrix and cleanup | #5480 |
| `install-cluster-e2e.yml` | Installed-cluster lifecycle and cleanup; preserve actual failure evidence | #5480 |
| `pipelines-live.yml` | Disposable cluster runtime qualification and retained logs | #5480 |
| `proving-live.yml` | Credentialed proving run with explicit authorization and evidence | #5480 |
| `publish-docs-bundle.yml` | Native release-files builds and verifies the exact-version bundle with complete Git history; release attachment and downstream delivery receipt remain | #5508, #5712 |
| `docs-current-check.yml` | Native weekly public docs check declared; installed schedule/failure delivery proof remains | #5508, #5712 |
| `publish-sdk-core.yml` | Native release-files retains the compiled, version-checked npm archive; registry publication and immutable receipt remain | #5480 |
| `publish-vscode-extension.yml` | Native release-files verifies all four native targets plus web; registry publication remains | #5480 |
| `vscode-site.yml` | Native `tests.editor-site` builds and checks the static page, runs the existing Chromium browser suite and retains the page plus screenshots; cloud execution and served-version evidence remain | #5480 |
| `ruleset-drift.yml` | Native daily public reads cover both recorded rulesets; installed schedule/failure delivery proof remains | #5507 |
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
