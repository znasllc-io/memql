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

// TestLegacyAliasesCount guards the 90-rule rename map against accidental
// edits: every legacy alias must map to a distinct MEMQL_ new name.
func TestLegacyAliasesCount(t *testing.T) {
	if len(LegacyAliases) != 90 {
		t.Fatalf("LegacyAliases has %d entries, want 90 (the Epic 7.3 rename map)", len(LegacyAliases))
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
