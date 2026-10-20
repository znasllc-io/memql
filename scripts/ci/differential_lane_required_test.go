package ci

// The language differential must fail disagreements and unreachable databases,
// on both PRs and merge candidates (memql#5386). Its own seeded suite remains
// separate from the database-package partition.

import (
	"slices"
	"strings"
	"testing"
)

func TestDifferentialLaneRunsInRequiredMode(t *testing.T) {
	step := nativeRequiredStep(t, "mcp-conformance")
	for _, command := range []string{
		"export MEMQL_DIFFERENTIAL_REQUIRED=1",
		"go test -count=1 -timeout=600s -v ./test/conformance/...",
	} {
		if !strings.Contains(step.Run, command+"\n") {
			t.Errorf("conformance lacks required command %q", command)
		}
	}
}

func TestConformanceJobRequiresReachableDatabase(t *testing.T) {
	step := nativeRequiredStep(t, "mcp-conformance")
	if !slices.Contains(step.Services, "postgres") || !strings.Contains(step.Run, "export MEMQL_REQUIRE_DB=1\n") {
		t.Fatal("conformance can silently skip its database-backed differential")
	}
	for _, command := range []string{
		"export MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable'",
		"CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;",
		"CREATE EXTENSION IF NOT EXISTS vector;",
	} {
		if !strings.Contains(step.Run, command+"\n") {
			t.Errorf("conformance lacks isolated database setup %q", command)
		}
	}
}
