package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ensure_schema_env_test.go -- memql#3096.
//
// EnsureSchema wrote MEMQL_DATABASE_DSN into the process environment BEFORE it
// pinged, and unconditionally when the variable was empty. Every db-gated test
// file reads that env first and falls back to its own literal only when it is
// EMPTY -- so the moment EnsureSchema published a defaultDSN that could not
// connect, each test's own perfectly good fallback became unreachable dead
// code.
//
// EnsureSchema converted its own failure into every downstream test's failure.
// Measured on one throwaway TimescaleDB, same tree, only the credential
// differing: the wrong password gave 19 SKIP, the right one 0 SKIP / 929 PASS
// -- and under MEMQL_REQUIRE_DB it failed the package outright rather than
// skipping.
//
// CI cannot catch this class: ci.yml sets the DSN at job level, so the fallback
// branch never executes in the lane. That is why the wrong credential survived,
// and why these tests drive the branch directly.

// unreachableDSN points at a port nothing listens on. Port 1 is privileged and
// reserved (tcpmux); a bind there would need root and nothing in this project
// does it.
const unreachableDSN = "postgres://memql:memql_dev@127.0.0.1:1/memql?sslmode=disable"

// withDefaultDSN swaps the package fallback for one test and restores it.
func withDefaultDSN(t *testing.T, dsn string) {
	t.Helper()
	prev := defaultDSN
	defaultDSN = dsn
	t.Cleanup(func() { defaultDSN = prev })
}

// THE REGRESSION GUARD. An unreachable default must leave the env ALONE.
//
// t.Setenv("") is how "unset" is expressed here: EnsureSchema TrimSpaces the
// value and treats empty as absent, which is the same condition every db-gated
// test file uses to reach its own fallback.
func TestEnsureSchemaDoesNotPublishAnUnreachableDefault(t *testing.T) {
	t.Setenv("MEMQL_DATABASE_DSN", "")
	t.Setenv(RequireDBEnv, "")
	withDefaultDSN(t, unreachableDSN)

	reachable, err := EnsureSchema(context.Background())
	if err != nil {
		t.Fatalf("without %s an unreachable DB must degrade quietly, got: %v", RequireDBEnv, err)
	}
	if reachable {
		t.Fatal("control broken: something IS listening on the unreachable DSN, so this test " +
			"proves nothing")
	}

	if got := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN")); got != "" {
		t.Errorf("EnsureSchema published an UNREACHABLE dsn into the environment: %q.\n"+
			"Every db-gated test file falls back to its own literal only when this variable is "+
			"EMPTY, so publishing a dead DSN makes each of those fallbacks unreachable and turns "+
			"one helper's failure into the whole package's (memql#3096).", SafeDSN(got))
	}
}

// The same property under MEMQL_REQUIRE_DB, where EnsureSchema returns an
// error instead of degrading. The env must still be untouched -- an errored
// run that has already poisoned the environment is the worse version of this
// bug, because the caller may keep going.
func TestEnsureSchemaDoesNotPublishAnUnreachableDefaultUnderRequireDB(t *testing.T) {
	t.Setenv("MEMQL_DATABASE_DSN", "")
	t.Setenv(RequireDBEnv, "1")
	withDefaultDSN(t, unreachableDSN)

	if _, err := EnsureSchema(context.Background()); err == nil {
		t.Fatal("control broken: an unreachable DB under MEMQL_REQUIRE_DB must be an error")
	}
	if got := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN")); got != "" {
		t.Errorf("EnsureSchema published %q despite failing; the environment must be untouched "+
			"on the failure path (memql#3096)", SafeDSN(got))
	}
}

// An EXPLICIT env is never overwritten, reachable or not -- the caller's choice
// wins. Guards the fix from the other side: a version that always deferred the
// Setenv could still clobber an operator-supplied DSN.
func TestEnsureSchemaNeverOverwritesAnExplicitDSN(t *testing.T) {
	const explicit = "postgres://someone:else@example.invalid:5432/other?sslmode=disable"
	t.Setenv("MEMQL_DATABASE_DSN", explicit)
	t.Setenv(RequireDBEnv, "")
	withDefaultDSN(t, unreachableDSN)

	if _, err := EnsureSchema(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := os.Getenv("MEMQL_DATABASE_DSN"); got != explicit {
		t.Errorf("an explicitly supplied DSN was modified: got %q, want it untouched", SafeDSN(got))
	}
}

// The defaultDSN credential must match what the project documents everywhere
// else. It once read `memql_local_dev`, which matches nothing in this project
// -- and because CI sets the DSN at job level, nothing could catch it.
//
// Pinned as a literal rather than by parsing: the point is that this exact
// string is the one CLAUDE.md, the README, the quickstart, the Makefile,
// scripts/k3d/seed-secrets.sh and ci.yml all use.
func TestDefaultDSNUsesTheProjectCredential(t *testing.T) {
	for _, want := range []string{"memql:memql_dev@", "localhost:5432", "/memql?"} {
		if !strings.Contains(defaultDSN, want) {
			t.Errorf("defaultDSN = %q, which does not contain %q.\n"+
				"It must match the credential the project documents everywhere else "+
				"(CLAUDE.md, README, quickstart, Makefile, scripts/k3d/seed-secrets.sh, ci.yml). "+
				"CI cannot catch drift here -- the lane sets the DSN at job level, so this "+
				"fallback never executes there (memql#3096).", SafeDSN(defaultDSN), want)
		}
	}
}
