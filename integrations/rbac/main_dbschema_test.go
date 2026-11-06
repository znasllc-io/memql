package rbac

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain migrates the shared test schema before any Postgres-gated test in
// this package runs, serialized against the other db-gated packages of the
// lane (memql#2551). With no database reachable EnsureSchema is a no-op and
// the database cases self-skip (or fail, under MEMQL_REQUIRE_DB=1).
//
// What made this package db-gated: effectiveAccessForSubject's cases in
// grants_test.go resolve a person's access through the real engine -- their
// memberships, their groups' accounts, the grants on both -- because the
// per-organization half of the answer is a fact about rows, and a fake engine
// would answer whatever the fake said.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
