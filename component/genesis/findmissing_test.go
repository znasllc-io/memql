package genesis

// findmissing_test.go -- moved here with its subject (memql#3963).
//
// FindMissing / FindMissingWithLegacy answer the SEAL FLOOR's question, so
// they left component/envregistry with the envelope. The test came with them:
// a test living in the package that no longer declares the function is not a
// smaller change, it is a build failure.

import (
	"testing"

	"github.com/znasllc-io/memql/component/envregistry"
)

func TestFindMissingWithLegacy_LegacyCovers(t *testing.T) {
	// Floor asks for the new names; .env carries the legacy names.
	required := []string{"MEMQL_OPENAI_API_KEY", "MEMQL_DATABASE_DSN"}
	entries := []envregistry.EnvEntry{
		{Name: "OPENAI_API_KEY", Value: "x"},
		{Name: "MEMORY_NODES_DATABASE_DSN", Value: "y"},
	}
	if missing := FindMissingWithLegacy(entries, required); len(missing) != 0 {
		t.Fatalf("legacy names should cover the floor, missing=%v", missing)
	}
	// Plain FindMissing would flag both as missing.
	if missing := FindMissing(entries, required); len(missing) != 2 {
		t.Fatalf("plain FindMissing should flag both, got %v", missing)
	}
}
