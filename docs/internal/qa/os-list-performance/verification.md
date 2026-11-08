---
title: "Final local verification"
audience: internal
status: stable
area: build
sinceVersion: 0.31.23
owner: znas
---

# Final local verification

Evidence audited November 7, 2026. All recorded automated checks pass, including
the final DSL conformance and language compatibility gates. These are local
checks against source and synthetic fixtures; they do not establish production
latency or deployment readiness by themselves. Independent final review is
complete with no remaining findings; repository finalization is tracked
separately. No instance resources changed.

## Completed checks

| Check | Result | Retained evidence |
| --- | --- | --- |
| Full MemQL OS tests | 6,885 passed, 0 failed, across 484 files; 2 snapshots matched | [`verification.json`](verification.json), extracted from the complete Vitest JSON |
| Full TypeScript SDK tests | 481 passed; 0 failed, skipped or cancelled | [`final-sdk-summary.log`](final-sdk-summary.log) |
| OS source and test typecheck | Passed | [`final-typecheck.log`](../../../../clients/os/qa/evidence/final-typecheck.log) |
| Isolated engine database class | Main engine and 8 test-bearing subpackages passed; `baseloader` has no test files | [`final-engine.log`](final-engine.log) |
| Isolated native database classes | All 9 selected packages passed | [`final-native-db.log`](final-native-db.log) |
| Complementary Go package sweep | 242 selected: initial 201 passed, 36 had no tests, 5 failed; all 5 failures now pass complete-package reruns, yielding 206 passed and 36 without tests | [`verification.json`](verification.json), [`final-unit-repairs.log`](final-unit-repairs.log), [`final-dsl-conformance.log`](final-dsl-conformance.log) |
| Final DSL gates | Full conformance package passed; 375 DSL files linted without diagnostics; no language compatibility breaks; generated SDK has no drift | [`final-dsl-gates.log`](final-dsl-gates.log), [`final-sdk-generation.log`](final-sdk-generation.log) |
| Final targeted database checks | Pipeline browse/mutation shared traits across replicas; Accounts membership/PII and outsider/anonymous boundaries passed | [`final-pipeline-traits-db.log`](final-pipeline-traits-db.log), [`final-accounts-pii-scope.log`](final-accounts-pii-scope.log) |
| Mounted browser matrix | 48 of 48 flows passed: 12 directories, widths 1280/390, light/dark | Four linked JSON matrices below |
| Matched production builds | Both baseline and changed source built successfully against the same current SDK and installed dependencies | [Raw builds and exact bytes](../../../../clients/os/qa/evidence/startup-build-comparison.json) |

Vitest reports 1,857 suites because that counter includes nested suites; the
file count is 484. Its stderr includes jsdom notices for unsupported
`window.open` and navigation, with no failed assertions. The compact summary
retains the raw source files' hashes instead of duplicating the 2.2 MB test
report and 97 KB SDK output. Retained console logs normalize trailing whitespace
and indentation where needed for the repository whitespace check; test results
and benchmark samples are unchanged.

The initial Go sweep failed in LSP, LSP grammar, integrations, CI scripts and
DSL conformance. Complete-package reruns close all five failures. The
report preserves that history and does not describe the initial sweep as an
all-green run. Database classes used fresh disposable databases with
`MEMQL_REQUIRE_DB=1`, so unavailable database infrastructure could not silently
skip those checks. Execution times in their logs are test durations, not
performance benchmarks.

## Browser and build audit

Every browser flow records 10 initial rows, 20 after Load more, one older server
search match and 20 restored rows; no horizontal overflow, horizontal row
dividers, boxed row metadata or browser errors:

- [Files and Materializer: 16 flows](../../../../clients/os/qa/evidence/resource-browser-checks.json).
- [Nexus: 16 flows](../../../../clients/os/qa/evidence/nexus-browser-checks.json).
- [Accounts, Users and Fleet: 12 flows](../../../../clients/os/qa/evidence/identity-fleet-browser-checks.json).
- [Concepts: 4 flows](../../../../clients/os/qa/evidence/concepts-browser-checks.json).

The evidence directory retains 32 matrix-specific screenshots for
Accounts/Users/Fleet/Nexus/Concepts and three additional representative or
exploratory screenshots. All 48 flows have JSON results; screenshots are not
claimed for every flow. These mounted browser checks use synthetic local data.

| Initial build asset | Baseline `8b7b3a483` | Changed |
| --- | ---: | ---: |
| JavaScript | 3,348.84 kB | 1,363.30 kB |
| JavaScript gzip, Vite output | 953.72 kB | 351.34 kB |
| CSS | 344.81 kB | 279.43 kB |

Exact initial JavaScript falls from 3,348,838 to 1,363,296 bytes, **59.29%**.
Both HTML entries reference one initial JavaScript asset and no additional
static module-preload JavaScript. Deferred app and voice chunks still load
when requested. The exact-byte JSON also gives a separate gzip-level-6 result,
which differs slightly from Vite's compressor. Build sizes measure download
and parsing exposure, not the user's production response time.

For measured server behavior and its limits, see the [same-fixture database
benchmark](README.md). For implementation coverage, intentional finite-catalog
exceptions, demand behavior and reducer scan limits, see the [resource-list
inventory](../../../../clients/os/qa/resource-lists.md).

## Final gate audit

The final conformance package passed in 15.106 seconds. DSL lint reports 375
files and no diagnostics; its namespace-collision notice means that the
freshly rebuilt embedded tree supplied those same source definitions. Language
compatibility reports no breaks against the checked-in 2026 surface, and SDK
generation reports no drift. The final shared-trait database checks passed
after the conformance repairs. `git diff --check` also passed. No gate failure
was waived or hidden by the compact report.

## Final lifecycle review

The independent review found one missing recovery path for component-owned
readers using `cacheReleased: false`. The shared continuity observer now
forwards stream gaps to those collections and replaces their duplicate
reconnect listener. The [regression summary](../../../../clients/os/qa/evidence/noncached-gap-regression.json)
records a reproduced failure before the fix and 85 passing focused tests
afterward, including StrictMode, disconnect/resume deduplication and cleanup.
The complete 6,885-test OS suite, typecheck and production build were rerun on
that final source and passed. The earlier 6,884-test report hash remains in the
machine-readable history; the final report identifies
`memql-os-recovery-verified.json` as current. No SDK or backend code changed in
this final lifecycle repair, and the independent review has no remaining
findings.
