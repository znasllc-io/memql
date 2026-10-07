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
| `ci.yml` | Headless build, race suite, discovered 20-second fuzz targets, gofmt, vet, single engine-pin guard; native Linux/macOS computer-use build and vet; macOS archive and isolated installer checks | Native Linux amd64 with Docker and X11 headers; macOS arm64 with Xcode. Real computer-use tests remain excluded because they operate a display. |
| `install-scripts-lint.yml` | Bash/POSIX syntax, ShellCheck, installer library/menu tests and rendered-mark/UI assertions on Linux amd64 and macOS arm64 | ShellCheck must already be installed on each consenting host. No host package installation is implicit. |
| `govulncheck.yml` | Exact v1.3.0 scanner, complete module, retained findings | Weekly trigger and installed run; findings and scanner failures remain failures. |
| `gitleaks.yml` | Exact v8.30.1 scanner, candidate history and tags, redacted report | Installed full-history run. Cockpit's legacy PR scan is also full-history; this port preserves it. |
| `codeql.yml` | Go manual build, pinned CodeQL 2.27.1 bundle with verified SHA-256, **security-and-quality**, retained SARIF | Weekly trigger and SARIF publication remain unqualified. Local Linux headless scan passed; other build tags and operating systems need their own coverage. |
| `release.yml` | Headless four-platform build/archives; native computer-use builds on darwin/linux × arm64/amd64; macOS menu/app archives | Exact release-event/tag qualification, collection of all four computer-use checksums, immutable cross-step artifact aggregation and GitHub release upload. No upload command or credential is introduced here. |
| `sbom.yml` | CycloneDX v1.10.0 inventories **each built binary**, JSON and XML retained with that build | Cross-step collection and publication of the eight binary inventories; manual release-asset re-inventory trigger. A source dependency list cannot substitute for these binary inventories. |
| `scorecard.yml` | No equivalent declared | Scorecard's branch-protection event, weekly trigger, identity-token attestation, public result publication and SARIF upload require an installed authorized equivalent. This row cannot be marked passed by another scan. |

PR, merge-candidate, push and release selection use the pipeline event contract.
The core workflow retains a failure while running the security stage, and blocks
release builds after any required failure. The manifest never turns a missing
platform, missing tool or denied consent into a successful skipped command.
Linux container commands use the public pinned ARM64 toolchain and explicitly
select Go 1.26.6, matching the legacy lanes. Native commands require the exact
declared platform; Linux hosts also need CGO's compiler and X11 development
headers, and macOS hosts need Xcode command-line tools. Release packaging is
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
