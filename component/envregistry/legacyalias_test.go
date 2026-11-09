package envregistry

import (
	"os"
	"testing"
)

func TestApplyLegacyEnvAliases_BridgesLegacy(t *testing.T) {
	preserveShimTargets(t)
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
	preserveShimTargets(t)
	// The fixtures below name aliases that are LIVE in LegacyAliases. That is
	// load-bearing rather than incidental: a fixture naming a retired alias
	// leaves the new name untouched, so "new wins" and "neither is present"
	// both pass for the wrong reason and the mechanism goes untested. The
	// vendor-API-key pairs these two used to name were retired in epic
	// memql#5088 -- and TestApplyLegacyEnvAliases_Idempotent, which asserts a
	// value rather than an absence, is what noticed.
	const newName = "MEMQL_AI_OPENAI_PROJECT_ID"
	const legacy = "MEMQL_SI_OPENAI_PROJECT_ID"
	t.Setenv(newName, "new-value")
	t.Setenv(legacy, "legacy-value")

	ApplyLegacyEnvAliases(nil)

	if got := os.Getenv(newName); got != "new-value" {
		t.Fatalf("new value should win, got %q", got)
	}
}

func TestApplyLegacyEnvAliases_Idempotent(t *testing.T) {
	preserveShimTargets(t)
	const newName = "MEMQL_EMAIL_SENDER"
	const legacy = "EMAIL_SENDER"
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
		"MEMORY_NODES_DATABASE_DSN":  true, // legacy only
		"MEMQL_AI_OPENAI_PROJECT_ID": true, // new only
	}
	if !PresentWithLegacy(have, "MEMQL_DATABASE_DSN") {
		t.Error("legacy alias MEMORY_NODES_DATABASE_DSN should satisfy MEMQL_DATABASE_DSN")
	}
	if !PresentWithLegacy(have, "MEMQL_AI_OPENAI_PROJECT_ID") {
		t.Error("new name present should satisfy")
	}
	// A name that HAS a live alias, neither half of which is present -- so the
	// false answer comes from the lookup rather than from an absent map entry.
	if PresentWithLegacy(have, "MEMQL_EMAIL_SENDER") {
		t.Error("neither new nor legacy present should be false")
	}
}
