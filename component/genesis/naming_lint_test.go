package genesis

import (
	"strings"
	"testing"
)

// knownExternalRegistryNames mirrors the envscan external denylist
// (cmd/envscan/scan.go `external` + `externalPrefixes`): env keys that
// are NOT memQL-owned configuration (CI / build / OS / runtime-platform).
// A registry entry whose name is external is exempt from the MEMQL_
// prefix rule. Kept here (rather than imported) so this test in the
// genesis package does not take a dependency on the envscan command.
var knownExternalRegistryNames = map[string]bool{
	"GITHUB_STEP_SUMMARY": true,
	"K_REVISION":          true,
	"VERSION":             true,
	"HOME":                true,
	"PATH":                true,
	"PWD":                 true,
	"USER":                true,
	"TMPDIR":              true,
	"TERM":                true,
	"LANG":                true,
	"TZ":                  true,
	"CI":                  true,
	// LiveKit runtime-platform var (see cmd/envscan/scan.go `external`).
	"LIVEKIT_PUBLIC_URL": true,
}

var knownExternalRegistryPrefixes = []string{"GITHUB_", "RUNNER_", "GO"}

func isExternalRegistryName(name string) bool {
	if knownExternalRegistryNames[name] {
		return true
	}
	for _, p := range knownExternalRegistryPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// legacyAliasNames is the set of pre-7.3 legacy names (the VALUES of
// LegacyAliases). A registry entry with one of these names is the
// deliberately-registered deprecated alias and is exempt from the
// MEMQL_ prefix rule.
func legacyAliasNames() map[string]bool {
	out := make(map[string]bool, len(LegacyAliases))
	for _, legacy := range LegacyAliases {
		out[legacy] = true
	}
	return out
}

// TestOwnedVarsArePrefixed is the Epic 7.3 (memql#2106) enforcement: every
// memQL-owned registry entry name must start with MEMQL_. The only
// exemptions are (a) a registered legacy alias (a VALUE in LegacyAliases,
// kept for back-compat and resolved by ApplyLegacyEnvAliases) and (b) a
// known-external var (CI / build / OS / runtime-platform, the envscan
// denylist). A newly-added non-conforming owned var fails this test.
func TestOwnedVarsArePrefixed(t *testing.T) {
	m, err := LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
	if err != nil {
		t.Fatalf("load embedded manifest: %v", err)
	}
	legacy := legacyAliasNames()

	var violations []string
	for _, e := range m.AllEntries() {
		name := e.Name
		if strings.HasPrefix(name, "MEMQL_") {
			continue
		}
		if legacy[name] {
			continue // deprecated alias, intentionally registered
		}
		if isExternalRegistryName(name) {
			continue // CI / build / OS var, not memQL-owned
		}
		violations = append(violations, name)
	}

	if len(violations) > 0 {
		t.Fatalf("registry entries are memQL-owned but not MEMQL_-prefixed "+
			"(and are neither a legacy alias nor an external var): %s\n"+
			"Owned env vars must use the MEMQL_ convention (Epic 7.3 / memql#2106). "+
			"If this is a legacy alias add it to LegacyAliases; if external, add it to "+
			"the envscan denylist.", strings.Join(violations, ", "))
	}
}

// TestLegacyAliasesCount guards the rename map against accidental edits: every
// legacy alias must map to a distinct MEMQL_ new name.
//
// The count went 90 -> 89 in memql#3453, which REMOVED
// MEMQL_POLYPHON_BRIDGE_AGENT_URL along with the Bridge Agent code that read
// it. An alias whose target no longer exists is worse than no alias: it keeps
// a retired name resolvable and tells an operator the variable still means
// something. Shrinking this number is legitimate only when the target variable
// is going away too -- a bare decrement with the target still live would be
// dropping a migration path operators depend on.
//
// 89 -> 95 in memql#3831, which renamed the six PRE-CONVENTION vars that never
// carried a MEMQL_ prefix at all (CACHE_MAX_TTL, the four MEMORY_ENGINE_*, and
// SERVER_PUBLIC_PATH). Growing the number is the safe direction -- each new
// entry ADDS a migration path rather than removing one -- but it still belongs
// in this guard, because the entries are what make an operator's existing
// configuration keep working and a silent deletion would be invisible.
func TestLegacyAliasesCount(t *testing.T) {
	if len(LegacyAliases) != 95 {
		t.Fatalf("LegacyAliases has %d entries, want 95 (the Epic 7.3 rename map, minus the memql#3453 removal, plus the six pre-convention renames in memql#3831)", len(LegacyAliases))
	}
	seenLegacy := map[string]bool{}
	for newName, legacy := range LegacyAliases {
		if !strings.HasPrefix(newName, "MEMQL_") {
			t.Errorf("alias new name %q must start with MEMQL_", newName)
		}
		if seenLegacy[legacy] {
			t.Errorf("duplicate legacy alias value %q", legacy)
		}
		seenLegacy[legacy] = true
	}
}
