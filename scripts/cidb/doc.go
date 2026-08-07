// Package cidb holds the CI drift gate for the db-tests lane (memql#2886).
//
// It has no runtime code. The package exists so an ordinary, untagged test can
// compare two sources of truth that nothing previously reconciled: what
// .github/workflows/ci.yml's `db-tests` job actually configures, and what the
// db-gated suites in the tree actually require in order to assert anything.
//
// The lane it guards is the one several PRs rest their only end-to-end evidence
// on, and it had two ways to report `ok` having verified nothing:
//
//   - It set MEMQL_DATABASE_DSN but not MEMQL_REQUIRE_DB, so every db-gated
//     test self-skipped when Postgres was slow, unhealthy, or unpullable, and
//     the suite degraded to green skips instead of a red build.
//   - Nothing tied the job's package selector to the packages that actually
//     carry db-gated tests, so a rename, a move, or a deletion could leave the
//     lane provisioning a database and then executing zero tests against it --
//     the same silent-nothing failure one level up.
//
// It also guards the ways the lane can report a non-failure while executing
// nothing at all -- a step-level env override of MEMQL_REQUIRE_DB (Actions
// gives step env precedence over job env), a `-run` selector that matches
// nothing, a step-level `if:`, or `continue-on-error` -- because ci-required
// treats a skipped or continue-on-error job as a pass, so each is silent.
//
// Both are now executable assertions rather than prose. The precedent is
// scripts/citags, which does exactly this job for build-tagged suites, and
// scripts/lib/capability_contract_test.go: a test-only gate beside the thing it
// gates, under scripts/ rather than cmd/ because cmd/ means "a binary".
//
// # Why the import, not the filename
//
// ci.yml's hand-maintained comment bound the selector to "packages that carry
// *_db_test.go files". That is under-specified: FOUR of the seven packages
// holding db-gated tests carry no such file at all (component/grpc,
// integrations/cognition, integrations/planner and examples/referencepack).
// All four are in the selector as of memql#3030, so the filename convention
// would now miss every one of them -- the parenthetical here used to single
// referencepack out as the only one covered, which stopped being true in that
// change. So this gate keys on the thing that actually makes a test db-gated:
// an import of component/database/dbtest from a _test.go file.
//
// Membership of the lane keys on something stricter still: a TestMain calling
// dbtest.EnsureSchema. A package earns one only so it can migrate the shared
// schema and join this lane's parallel run (memql#2551), so having one and not
// being in the selector is always drift -- and unlike a per-argument check,
// that catches a selector entry being DELETED.
//
// # The tie is asserted in BOTH directions (memql#3095)
//
// That rule used to be one-way, and this doc used to say so -- a gap, honestly
// documented, not a false claim. It was still wide enough to drive the change
// through: deleting every main_dbschema_test.go added by memql#3091 while
// leaving the selector listing their packages left the gate fully green
// (`ok  scripts/cidb`). The central artifact of that PR could be removed
// wholesale and the gate that tightens it said nothing.
//
// coverageFindings now also asserts the inverse: a package COVERED by the
// selector that carries db-gated tests must have an EnsureSchema TestMain. The
// rule keys on the packages that actually carry db-gated tests, not on the
// selector's whole expansion -- the selector reaches 13 packages of which 7
// touch the database, and the other 6 have no business owning a migration
// TestMain. scripts/cidb itself is exempt for the reason selfPkg records.
//
// Measured on the tree when it landed: deleting any ONE of the seven
// main_dbschema_test.go files reds `go test ./scripts/cidb/...` with exactly
// one finding, naming that package and its remedy -- seven for seven. Deleting
// all seven yields seven findings. Zero false positives across all 13 packages
// the selector expands to.
//
// What this direction protects is the memql#2551 invariant itself: the lane
// runs per-package binaries as parallel processes against ONE database, and a
// package without the TestMain races the shared migration and fails
// INTERMITTENTLY with `relation "MemoryNodes" does not exist`. An intermittent
// red is the worst shape of failure this lane can produce, which is why the
// invariant is worth an assertion rather than a convention.
//
// # Full coverage IS asserted (memql#3030)
//
// The uncovered-set finding was a log line rather than an assertion while four
// packages were outside the lane (component/automations, component/grpc,
// integrations/cognition, integrations/planner) -- deliberately, so the tree
// was not red over a defect the change that added this gate did not fix. Their
// 19 DB assertions had never run in CI: each package printed `ok` while
// executing none of them, because nothing asserted the selector reached them.
//
// memql#3030 gave each of those a TestMain calling dbtest.EnsureSchema
// (memql#2551 -- required because the lane runs per-package binaries as
// parallel processes against ONE database) and added them to the selector, so
// the exemption has no subject left and the log became the failure it was
// always meant to be. A db-gated package added outside the selector now reds
// immediately.
//
// The counts remain a coarse proxy and still feed no pass/fail decision: the
// log over-counts non-DB tests that happen to sit in a dbtest-importing file,
// and under-counts db-gated tests that reach dbtest through a helper in a
// sibling file. What is asserted is per-package coverage, which is unaffected
// by either.
//
// # What it can and cannot prove
//
// Be precise about this, because a gate that overstates its guarantee is worse
// than no gate: it invites people to stop looking.
//
//   - AIRTIGHT: MEMQL_REQUIRE_DB is present and truthy at the step that runs
//     the suite. That is a YAML field read, with Actions' step-over-job
//     precedence modelled, and it is what makes an unreachable database a
//     failure instead of a green skip. This is the issue's actual fix.
//   - STRUCTURAL: every package with an EnsureSchema TestMain is in the
//     selector. Pure Go AST on both sides, no shell involved.
//   - BEST-EFFORT: that the lane's command actually executes those packages.
//     This is read off `run:` text, and text cannot prove execution. The gate
//     narrows the gap by REFUSING anything it cannot read plainly -- shell
//     control flow, exit-status suppression (`|| true`, backgrounding, an
//     unguarded pipe), `cd`/`exec`/`alias`, a working-directory, a `./...`
//     wildcard, a zero-execution flag on the command line or in GOFLAGS -- so
//     the residual is exotic rather than everyday. It is not a sandbox, and a
//     determined edit can still defeat it.
//
// The authoritative version of the third bullet is a RUNTIME count: pipe `go
// test -json` through a checker that fails when zero db-gated tests actually
// ran. That observes execution instead of reasoning about text, and it is the
// natural next step if this ever proves insufficient. It is deliberately not
// done here -- the static form catches the drift that actually happens
// (a renamed package, an emptied selector, a deleted entry, a dropped env key)
// and runs on EVERY pull request, including the many where the db-tests lane
// itself is skipped.
//
// Untagged on purpose: it must run in the default `go test ./...`, which the
// `ci` path filter reaches on any .github/workflows/** edit -- so a PR that
// removes the env key it guards is the PR that goes red.
package cidb
