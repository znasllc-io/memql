package dbtest

import (
	"os"
	"strings"
	"testing"
)

// live_cluster_guard_3415_test.go covers the blast-radius guard added after
// memql#3415: the db-gated suites silently accept whatever MEMQL_DATABASE_DSN
// names, and their built-in fallback DSN
// (postgres://memql:memql_dev@localhost:5432/memql) is THE LOCAL k3d CLUSTER'S
// DATABASE. Running `go test ./component/memql/...` with no env set therefore
// writes production-shaped rows into a running cluster's identity data. That is
// how a mutation test appended blank-bootstrappedAt versions of the singleton
// clusterSettings row, disabled login for every user, and re-opened the
// unauthenticated ownership wizard.
//
// The guard: a database whose clusterSettings singleton is STAMPED belongs to a
// bootstrapped cluster, not to a test lane. Refuse to run against it unless the
// operator says otherwise. A blank CI database has no such row and is
// unaffected.

func TestLiveClusterGuard3415_RefusesABootstrappedDatabase(t *testing.T) {
	t.Setenv(AllowBootstrappedDBEnv, "")
	err := refuseBootstrappedDatabase("2026-08-09T07:38:20Z", "postgres://memql:memql_dev@localhost:5432/memql")
	if err == nil {
		t.Fatal("a database carrying a bootstrapped clusterSettings row must be refused (memql#3415)")
	}
	// The message has to be actionable and must not leak the password.
	if !strings.Contains(err.Error(), AllowBootstrappedDBEnv) {
		t.Errorf("refusal must name the override env %s; got: %v", AllowBootstrappedDBEnv, err)
	}
	if strings.Contains(err.Error(), "memql_dev") {
		t.Errorf("refusal leaked the DSN password: %v", err)
	}
}

func TestLiveClusterGuard3415_AllowsABlankDatabase(t *testing.T) {
	t.Setenv(AllowBootstrappedDBEnv, "")
	for _, stamp := range []string{"", "   "} {
		if err := refuseBootstrappedDatabase(stamp, "postgres://x@localhost:5432/db"); err != nil {
			t.Errorf("an unbootstrapped database must be allowed (stamp=%q): %v", stamp, err)
		}
	}
}

func TestLiveClusterGuard3415_OverrideOptsBackIn(t *testing.T) {
	t.Setenv(AllowBootstrappedDBEnv, "1")
	if err := refuseBootstrappedDatabase("2026-08-09T07:38:20Z", "postgres://x@localhost:5432/db"); err != nil {
		t.Errorf("%s=1 must opt back in: %v", AllowBootstrappedDBEnv, err)
	}
	// Falsy values must NOT opt in -- a guard that any non-empty string
	// disarms is a guard that disarms by accident.
	for _, v := range []string{"0", "false", "no", ""} {
		os.Setenv(AllowBootstrappedDBEnv, v)
		if err := refuseBootstrappedDatabase("stamp", "postgres://x@localhost:5432/db"); err == nil {
			t.Errorf("%s=%q must not disarm the guard", AllowBootstrappedDBEnv, v)
		}
	}
}
