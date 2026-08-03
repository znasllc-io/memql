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
// Both are now executable assertions rather than prose. The precedent is
// scripts/citags, which does exactly this job for build-tagged suites, and
// scripts/lib/capability_contract_test.go: a test-only gate beside the thing it
// gates, under scripts/ rather than cmd/ because cmd/ means "a binary".
//
// # Why the import, not the filename
//
// ci.yml's hand-maintained comment binds the selector to "packages that carry
// *_db_test.go files". That is under-specified -- three of the seven packages
// holding db-gated tests keep them in files not matching that name -- so this
// gate keys on the thing that actually makes a test db-gated: an import of
// component/database/dbtest from a _test.go file.
//
// # What this gate deliberately does NOT assert
//
// It does not require every db-gated package to be in the lane. Four are not
// (component/automations, component/grpc, integrations/cognition,
// integrations/planner), hiding 19 assertions that have never run in CI. That
// is real, and it is tracked separately, because closing it is per-package work
// rather than a ci.yml edit: each package needs a TestMain calling
// dbtest.EnsureSchema before it can share the lane's one database (memql#2551).
// Asserting full coverage here would red the tree on a defect this change does
// not fix.
//
// Untagged on purpose: it must run in the default `go test ./...`, which the
// `ci` path filter reaches on any .github/workflows/** edit -- so a PR that
// removes the env key it guards is the PR that goes red.
package cidb
