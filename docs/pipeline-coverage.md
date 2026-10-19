# Cockpit pipeline coverage

`memql-package.yaml` runs through the engine's installed `runPipelineStages`
MemQL automation. The manifest supplies commands, platforms, event filters,
timeouts and artifacts; the automation controls order and failure propagation.
The native worker enforces repository consent, platform matching, cancellation
and workspace ownership. `scripts/ci/prepare.py` only materializes the exact
candidate and `.github/memql-pin` as siblings inside `.memql-ci/`.

This package requires the engine's DSL workflow and `runAfterFailure` contract
(engine PR #5883, merged as `82c37723251d016bca6a8589f50caed67e1dfbca`,
or a compatible descendant). The Cockpit build dependency remains `.github/memql-pin`;
the pipeline engine and the engine source used to compile Cockpit are separate
pins. Neither selecting this manifest nor compiling it grants fleet access.

## Coverage inventory

All legacy workflows remain enabled until installed qualification, observation
and the repository's required-check transition are complete. A compiled command
is not evidence that its host, trigger or publication path has run.

| Legacy workflow | MemQL command coverage | Remaining qualification / external effect |
| --- | --- | --- |
| `ci.yml` | Headless build, race suite, discovered 20-second fuzz targets, gofmt, vet, single engine-pin guard; container Linux and native macOS computer-use build and vet; macOS archive and isolated installer checks | Native Linux amd64 with Docker for the race suite; the published toolchain supplies Linux X11 headers and compilers; macOS arm64 with Xcode. Real computer-use tests remain excluded because they operate a display. |
| `install-scripts-lint.yml` | Bash/POSIX syntax, ShellCheck, installer library/menu tests and rendered-mark/UI assertions on Linux amd64 and macOS arm64 | ShellCheck must already be installed on each consenting host. No host package installation is implicit. |
| `govulncheck.yml` | Exact v1.3.0 scanner, complete module, retained findings | Weekly trigger and installed run; findings and scanner failures remain failures. |
| `gitleaks.yml` | Exact v8.30.1 scanner; introduced commits on PR/push/merge candidates, full candidate history and tags weekly or on demand, redacted coverage receipts | Installed commit-range and full-history runs. A secret introduced and removed inside a PR remains in the selected commit range. |
| `codeql.yml` | Go manual build, pinned CodeQL 2.27.1 bundle with verified SHA-256, **security-and-quality**, retained SARIF | Weekly trigger and driver-owned SARIF publication are declared; installed delivery remains unqualified. Local Linux headless scan passed; other build tags and operating systems need their own coverage. |
| `release.yml` | Headless four-platform build/archives; computer-use builds for darwin/linux × arm64/amd64 using the AMD64 build pool and an Apple Silicon Mac; macOS menu/app archives | Exact release-event/tag qualification, collection of all four computer-use checksum sidecars, complete immutable candidate assembly and GitHub release upload. No upload command or credential is introduced here. |
| `sbom.yml` | CycloneDX v1.10.0 inventories **each built binary**, JSON and XML retained with that build | Cross-step collection and publication of the eight binary inventories; manual release-asset re-inventory trigger. A source dependency list cannot substitute for these binary inventories. |
| `scorecard.yml` | Full 18-check Scorecard 5.5.0 audit, the retiring action's policy, bounded source-verified JSON/SARIF, repository-scoped read grant | Weekly/manual execution and driver-owned SARIF ingestion require engine v0.30.0 and installed qualification. Native reports replace the Actions-only OIDC public-result upload; no GitHub-runner attestation is claimed. |

PR, merge-candidate, push and release selection use the pipeline event contract.
The core workflow retains a failure while running the security stage, and blocks
release builds after any required failure. The manifest never turns a missing
platform, missing tool or denied consent into a successful skipped command.
Linux container commands use the public pinned AMD64 toolchain and explicitly
select Go 1.26.9. The image matches the bounded AKS build pool. Native commands require the exact
declared host platform. The toolchain carries Linux CGO compilers and X11 development
headers. macOS hosts need Xcode command-line tools; Intel artifacts are cross-built on Apple Silicon and run under already-installed Rosetta. Release packaging is
the existing local/ad-hoc signing path; distribution signing credentials and
notarization are not configured by this change.

## Local verification

Run `python3 -m unittest discover -s scripts/ci -p '*_test.py'` for preparation
invariants. It creates fixture Git repositories and proves exact revisions,
workspace containment, ambiguous-pin rejection and stale-workspace refusal.

To execute a command locally, use a fresh committed checkout. Run
`python3 scripts/ci/prepare.py --engine-source=/absolute/local/engine-clone`
to prepare it without a remote fetch, then enter `.memql-ci/memql-cockpit`.
The local clone must contain the exact pin; it is never silently substituted.
Preparation deliberately uses committed source, just as an installed pipeline
does. It does not copy an operator's uncommitted changes or credentials.

The pipeline's Docker test images are explicit and the race lane probes Docker
before testing. Running the suite without those variables is useful unit
coverage, but does not qualify the Docker cases. All-platform publication and
installed execution remain unqualified until their individual evidence exists.

The integration-authoring boundary is documented in the engine's
[integration boundary](https://github.com/znasllc-io/memql/blob/main/docs/public/build/integration-boundary.md).
Changes to release policy belong in DSL/configuration; native helpers remain
bounded checkout, build, signing, transport or scanner capabilities.

The local port check passed the full Go race suite, the Docker-backed worker
suite with both pinned test images (56.895 s), and the native macOS arm64
computer-use build and vet. Menu/app archive checks and the isolated piped
installer/update/uninstall rehearsal also passed; their service calls are
stubbed and their homes are temporary. The preparation tests additionally
prove that unstaged source and engine-pin edits cannot enter a run of HEAD.
The remaining native platforms and installed release aggregation still require
their own evidence.

The exact declared Linux ARM64 commands at source `7947670` also passed all 13
discovered fuzz targets (20 seconds each), govulncheck v1.3.0, and the headless
four-platform release build. The latter produced four archives and a JSON/XML
CycloneDX inventory for each built binary. Govulncheck reported no reachable or
imported-package vulnerabilities and one advisory in a required module whose
vulnerable code was not called.

CodeQL 2.27.1 completed its declared manual Go build and security-and-quality
suite and retained SARIF. It scanned 172 of 379 Go files in that Linux headless
invocation; it does not qualify other build tags or operating systems. Its one
finding was an unchecked close in the existing Linux GPU-device access probe.
The probe writes no data; it now refuses a failed close. This is a code fix,
not a suppression, and the original scan remains evidence of the original
source rather than a zero-finding claim for the changed source. The final
exact-source scan at `bb81d162c46ff4cb5ad1e2dc54499033e792622b` completed with
zero SARIF findings, again covering 172 of 379 Go files. The affected Linux
inference package tests passed in the same pinned Linux container.

## Current build-pool port

The recipe now selects Linux AMD64 and Go 1.26.9. Native host declarations
remain explicit. The daily schedule selects only opted-in stages; full-history
secret checks and CodeQL run weekly or on manual qualification. Releases retain
the ordinary checks and vulnerability scan; they do not repeat the weekly scans. PR, push and
merge-candidate scans cover every introduced commit, including a secret added
and removed before the final tree. A missing predecessor falls back to full
history; an unreadable nonempty base fails. The bounded scanner and its
regressions are copied from the engine, preserving failure and coverage receipts.
These additions require an engine containing memql#6137's stage cadence; the
Cockpit SARIF delivery step also requires memql#6146. Historical results above
are not qualification of this rebased source or of installed execution.

The Linux ARM64 computer-use release uses the pinned toolchain's AArch64
compiler and target libraries on AMD64. Its completed binary must start under
QEMU before artifact retention. Both Linux architectures use the same bounded
build pool. The Intel Mac release uses the Apple Silicon host with explicit
AMD64 Go and Swift targets, verifies the archives, and requires Rosetta to
already be present; no package installation or license acceptance is implicit.
These recipe changes still require installed qualification with the newly
published toolchain digest before release.

Each computer-use producer retains a `.sha256` sidecar naming its exact raw
binary. The native candidate must include all four binaries and sidecars, plus
the headless archives, Mac app/menu archives and binary SBOMs. This replaces
the legacy post-upload combined computer-use checksum file; the headless
archive checksum manifest and Mac archive checksum names stay unchanged.
Candidate preparation verifies every producer and retained artifact before
approval, and promotion requires every declared asset to have a verified
publication receipt. No build step receives publication credentials.

## Native toolchain pin

The AMD64 toolchain now pins
`sha256:a62b53491fbd9cfd4bbfdf9612bfb1776d9fc9d42aab7ceceac3a0a18a17a8bb`.
Engine native release run `517b203755fd72a42c422493c691ad77` built it in the
existing AKS pool at source `f8ece576d93efdfb899fb3c4133f895c304f7e60`.
The downloaded archive matched its retained SHA-256 and passed the full
matching toolchain smoke script, including its AArch64 compiler and libraries.
Native candidate
`sha256:27c6c1aaaa081a2f8539f8f9190457ad6f8fb1d1e9bf7b1b159df3ab24d9ffa5`
completed reviewed GHCR publication with verified blob and tag readback.
The same manifest is readable anonymously.

This qualifies the image's build, smoke and publication path. Installed
Cockpit checks and release aggregation still require their own runs. The Mac
must explicitly allow this repository in its pipeline policy, and the Intel
Mac release requires already-installed Rosetta. No policy edit or host
installation is performed by these recipes.

## Native repository audit

The weekly/manual Scorecard command audits the current default-branch head and
refuses if it differs from the queued source. Historical-commit mode omits checks
and is not accepted as full coverage. The driver supplies a repository-scoped
read token; the command retains JSON, SARIF, bounded logs, and a source/policy
receipt. It never uploads results or receives writer credentials. The separate
SARIF step publishes retained results through the installed GitHub App.

This replaces the repository audit and code-scanning delivery. The legacy
Scorecard Action's GitHub OIDC attestation and public Scorecard-service upload
are retired with Actions; results remain available in MemQL Library and GitHub
code scanning. A ruleset change can request a manual audit, and weekly scanning
provides the regular cadence. Keep the existing workflow until the native audit
and SARIF ingestion have passed in production.
