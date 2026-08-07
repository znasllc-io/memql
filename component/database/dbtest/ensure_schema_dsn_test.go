package dbtest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// ensure_schema_dsn_test.go -- EnsureSchema must not amplify its own failure
// (memql#3096, #3148).
//
// The defect these pin: EnsureSchema wrote its built-in fallback into
// MEMQL_DATABASE_DSN *before* pinging it, and unconditionally. Every db-gated
// test reads that env first and falls back to its own literal only when the
// variable is EMPTY -- so an unreachable fallback published by this helper made
// every downstream test's own perfectly good fallback unreachable dead code.
// One wrong credential in one const turned into 19 skips across the tree
// (memql#3030), and under MEMQL_REQUIRE_DB into "0 tests ran, package FAIL".
//
// unreachableDSN points at port 1, which is reserved (tcpmux) and never bound
// here. Connection-refused is immediate, so these tests need no database and
// cost no wall-clock -- and unlike a bad credential they do not depend on a
// server being present to reject them.
const unreachableDSN = "postgres://memql:memql_dev@127.0.0.1:1/memql?sslmode=disable"

// dsnEnvSandbox isolates a test from the ambient MEMQL_DATABASE_DSN /
// MEMQL_REQUIRE_DB, restoring both afterwards. It returns with both UNSET, so
// each test states the environment it means to exercise.
func dsnEnvSandbox(t *testing.T) {
	t.Helper()
	for _, key := range []string{dsnEnv, RequireDBEnv} {
		prev, had := os.LookupEnv(key)
		t.Cleanup(func() {
			if had {
				os.Setenv(key, prev)
			} else {
				os.Unsetenv(key)
			}
		})
		os.Unsetenv(key)
	}
}

// TestEnsureSchema_UnreachableDefaultLeavesEnvUntouched is the core assertion
// of memql#3096: the env is written only for a DSN that answered.
func TestEnsureSchema_UnreachableDefaultLeavesEnvUntouched(t *testing.T) {
	dsnEnvSandbox(t)

	reachable, err := ensureSchema(context.Background(), unreachableDSN)
	if err != nil {
		t.Fatalf("unreachable fallback with %s unset must not error, got %v", RequireDBEnv, err)
	}
	if reachable {
		t.Fatal("unreachable fallback reported reachable=true")
	}
	if got, ok := os.LookupEnv(dsnEnv); ok {
		t.Fatalf("%s was published as %q despite the DSN never answering -- "+
			"every downstream test now inherits this helper's failure and its own "+
			"fallback is dead code (memql#3096)", dsnEnv, got)
	}
}

// TestEnsureSchema_UnreachableDefaultUnderRequireDBFallsThrough pins the
// memql#3148 decision recorded in ensureSchema's ping-failure branch: when the
// operator named NO DSN, an unreachable built-in fallback is this helper's
// problem, not a verdict on the run. Returning (false, nil) is what lets every
// main_dbschema_test.go reach m.Run() -- each exits only on err != nil.
func TestEnsureSchema_UnreachableDefaultUnderRequireDBFallsThrough(t *testing.T) {
	dsnEnvSandbox(t)
	os.Setenv(RequireDBEnv, "1")

	reachable, err := ensureSchema(context.Background(), unreachableDSN)
	if err != nil {
		t.Fatalf("under %s=1 an unreachable BUILT-IN fallback must fall through so the "+
			"package's own DSN resolution and per-test gate get their turn; got err=%v", RequireDBEnv, err)
	}
	if reachable {
		t.Fatal("unreachable fallback reported reachable=true")
	}
	if got, ok := os.LookupEnv(dsnEnv); ok {
		t.Fatalf("%s published as %q on the %s path too", dsnEnv, got, RequireDBEnv)
	}
}

// TestEnsureSchema_UnreachableNamedDSNUnderRequireDBIsAnError is the other half
// of that decision, and the half memql#2680 bought: a DSN the operator NAMED
// that does not answer is a hard error. This is the path ci.yml's db-tests lane
// always takes -- it sets MEMQL_DATABASE_DSN at job level -- so the lane's
// loud-failure guarantee is untouched by the gate above.
func TestEnsureSchema_UnreachableNamedDSNUnderRequireDBIsAnError(t *testing.T) {
	dsnEnvSandbox(t)
	os.Setenv(dsnEnv, unreachableDSN)
	os.Setenv(RequireDBEnv, "1")

	reachable, err := ensureSchema(context.Background(), defaultDSN)
	if err == nil {
		t.Fatalf("a NAMED %s that does not answer must fail under %s=1 (memql#2680)", dsnEnv, RequireDBEnv)
	}
	if reachable {
		t.Fatal("unreachable named DSN reported reachable=true")
	}
	if !strings.Contains(err.Error(), RequireDBEnv) {
		t.Errorf("error must name %s so the reader knows which switch produced it: %v", RequireDBEnv, err)
	}
	// The message must be safe to print in CI output.
	if strings.Contains(err.Error(), "memql_dev") {
		t.Errorf("error leaked the password: %v", err)
	}
	// And the env the operator set must survive untouched.
	if got := os.Getenv(dsnEnv); got != unreachableDSN {
		t.Errorf("%s = %q, want the operator's value %q -- EnsureSchema must never "+
			"rewrite a DSN the caller supplied", dsnEnv, got, unreachableDSN)
	}
}

// TestEnsureSchema_UnreachableNamedDSNWithoutRequireDBIsSilent keeps the
// green-by-skip behaviour a laptop `go test ./...` depends on.
func TestEnsureSchema_UnreachableNamedDSNWithoutRequireDBIsSilent(t *testing.T) {
	dsnEnvSandbox(t)
	os.Setenv(dsnEnv, unreachableDSN)

	reachable, err := ensureSchema(context.Background(), defaultDSN)
	if err != nil {
		t.Fatalf("with %s unset an unreachable DSN must degrade to skips, not error: %v", RequireDBEnv, err)
	}
	if reachable {
		t.Fatal("unreachable named DSN reported reachable=true")
	}
}

// TestEnsureSchema_ReachableDefaultPublishesEnv is the positive control: the
// Setenv was moved, not deleted. Its stated purpose -- aligning
// NewMemoryNodesDatabase, which reads the env, with the DSN the tests fall back
// to -- is only meaningful on the reachable path, and it must still happen
// there. DB-gated: skips (or fails under MEMQL_REQUIRE_DB) without Postgres.
func TestEnsureSchema_ReachableDefaultPublishesEnv(t *testing.T) {
	dsnEnvSandbox(t)

	reachable, err := ensureSchema(context.Background(), defaultDSN)
	if err != nil {
		t.Fatalf("ensureSchema against the built-in default: %v", err)
	}
	if !reachable {
		Unreachable(t, "EnsureSchema reachable-path env publication", defaultDSN,
			errNoPingResponse)
		return
	}
	if got := os.Getenv(dsnEnv); got != defaultDSN {
		t.Fatalf("%s = %q after a SUCCESSFUL ping, want %q -- NewMemoryNodesDatabase reads "+
			"the env and would target a different database than the tests", dsnEnv, got, defaultDSN)
	}
}

// errNoPingResponse is the cause reported by the skip above. ensureSchema
// deliberately does not surface the ping error on the non-required path -- that
// silence is what keeps a DB-less run green -- so there is no underlying error
// to forward, and saying so beats inventing one.
var errNoPingResponse = errors.New("ensureSchema reported reachable=false (ping error not surfaced on this path)")
