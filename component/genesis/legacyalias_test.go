package genesis

import (
	"os"
	"testing"
)

func TestApplyLegacyEnvAliases_BridgesLegacy(t *testing.T) {
	const newName = "MEMQL_DATABASE_DSN"
	const legacy = "MEMORY_NODES_DATABASE_DSN"
	t.Setenv(newName, "")
	t.Setenv(legacy, "postgres://legacy")
	// Ensure new is truly unset (t.Setenv with "" still sets it to empty;
	// ApplyLegacyEnvAliases treats empty as unset via os.Getenv != "").
	os.Unsetenv(newName)

	ApplyLegacyEnvAliases(nil)

	if got := os.Getenv(newName); got != "postgres://legacy" {
		t.Fatalf("new name not bridged from legacy: got %q", got)
	}
}

func TestApplyLegacyEnvAliases_NewWins(t *testing.T) {
	const newName = "MEMQL_OPENAI_API_KEY"
	const legacy = "OPENAI_API_KEY"
	t.Setenv(newName, "new-value")
	t.Setenv(legacy, "legacy-value")

	ApplyLegacyEnvAliases(nil)

	if got := os.Getenv(newName); got != "new-value" {
		t.Fatalf("new value should win, got %q", got)
	}
}

func TestApplyLegacyEnvAliases_Idempotent(t *testing.T) {
	const newName = "MEMQL_ANAM_API_KEY"
	const legacy = "ANAM_API_KEY"
	os.Unsetenv(newName)
	t.Setenv(legacy, "k1")

	ApplyLegacyEnvAliases(nil)
	first := os.Getenv(newName)
	// A second legacy value must NOT clobber the now-present new name.
	t.Setenv(legacy, "k2")
	ApplyLegacyEnvAliases(nil)
	second := os.Getenv(newName)

	if first != "k1" || second != "k1" {
		t.Fatalf("not idempotent: first=%q second=%q", first, second)
	}
}

func TestPresentWithLegacy(t *testing.T) {
	have := map[string]bool{
		"MEMORY_NODES_DATABASE_DSN": true, // legacy only
		"MEMQL_OPENAI_API_KEY":      true, // new only
	}
	if !PresentWithLegacy(have, "MEMQL_DATABASE_DSN") {
		t.Error("legacy alias MEMORY_NODES_DATABASE_DSN should satisfy MEMQL_DATABASE_DSN")
	}
	if !PresentWithLegacy(have, "MEMQL_OPENAI_API_KEY") {
		t.Error("new name present should satisfy")
	}
	if PresentWithLegacy(have, "MEMQL_ANTHROPIC_API_KEY") {
		t.Error("neither new nor legacy present should be false")
	}
}

func TestFindMissingWithLegacy_LegacyCovers(t *testing.T) {
	// Floor asks for the new names; .env carries the legacy names.
	required := []string{"MEMQL_OPENAI_API_KEY", "MEMQL_DATABASE_DSN"}
	entries := []EnvEntry{
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
