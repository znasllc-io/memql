package memql

import (
	"testing"
)

// TestUnavailableSlugsAreReportedNotSwallowed is the memql#4692 boot half.
//
// A slug with none of its tools registered stays a NON-error -- workbenchHost
// is pack-owned, so an engine-only cluster missing it is correct and failing
// boot would fail every such cluster. What must change is that it stops being
// INVISIBLE: "this node provides the capability" and "this node does not" used
// to produce byte-identical output, while an agent whose role locks the slug
// carried the tool name into the model's prompt either way.
func TestUnavailableSlugsAreReportedNotSwallowed(t *testing.T) {
	unavailable := UnavailableCapabilitySlugs(func(string) bool { return false })
	if len(unavailable) == 0 {
		t.Fatal("a registry containing NOTHING reports no unavailable capability slugs; " +
			"the check cannot distinguish 'this node does not load that pack' from " +
			"'everything is fine' (memql#4692)")
	}
	found := false
	for _, s := range unavailable {
		if s == "workbench-use" {
			found = true
		}
	}
	if !found {
		t.Errorf("workbench-use is the concrete case in the issue and is not reported: %v", unavailable)
	}
	// Still not an error: the missing-set must stay empty, or every engine-only
	// deployment fails its boot self-check.
	if missing := VerifyCapabilityToolsRegistered(func(string) bool { return false }, testGuardLogger()); len(missing) != 0 {
		t.Errorf("a fully-absent slug must not be reported as MISSING (that is the partial-parse bug), got %v", missing)
	}
}

// TestFullyRegisteredNodeReportsNothingUnavailable is the reachable positive:
// without it, the assertion above passes on a checker that reports every slug
// unconditionally.
func TestFullyRegisteredNodeReportsNothingUnavailable(t *testing.T) {
	all := allCapabilityToolNames()
	if len(all) == 0 {
		t.Fatal("no capability slugs registered; the test proves nothing")
	}
	present := map[string]bool{}
	for _, n := range all {
		present[n] = true
	}
	if got := UnavailableCapabilitySlugs(func(s string) bool { return present[s] }); len(got) != 0 {
		t.Errorf("with every tool registered, no slug is unavailable; got %v", got)
	}
}

// TestPartialAndAbsentAreDifferentOutcomes pins the split itself. Collapsing
// them either fails every engine-only boot (treating absent as missing) or
// re-hides the parse bug (treating missing as absent).
func TestPartialAndAbsentAreDifferentOutcomes(t *testing.T) {
	names := CapabilityToolNames("workbench-use")
	if len(names) < 2 {
		t.Skipf("workbench-use expands to %d tool(s); the partial case needs at least 2", len(names))
	}
	onlyFirst := func(s string) bool { return s == names[0] }

	missing := VerifyCapabilityToolsRegistered(onlyFirst, testGuardLogger())
	if len(missing) == 0 {
		t.Error("a PARTIALLY registered slug must be reported as missing -- that is the parse bug")
	}
	if got := UnavailableCapabilitySlugs(onlyFirst); len(got) != 0 {
		for _, s := range got {
			if s == "workbench-use" {
				t.Error("a partially-registered slug must NOT be reported as merely unavailable")
			}
		}
	}
}

// TestCapabilityToolNamesDoesNotAliasTheRegistry: the getter hands out a copy,
// so a caller cannot mutate the process-wide slug table by accident.
func TestCapabilityToolNamesDoesNotAliasTheRegistry(t *testing.T) {
	first := CapabilityToolNames("workbench-use")
	if len(first) == 0 {
		t.Skip("workbench-use not registered")
	}
	first[0] = "clobbered"
	if second := CapabilityToolNames("workbench-use"); second[0] == "clobbered" {
		t.Error("CapabilityToolNames returns the registry's own slice; a caller can corrupt it")
	}
}

func allCapabilityToolNames() []string {
	var out []string
	for _, slug := range []string{"computer-use-headless", "computer-use-embodied", "workbench-use"} {
		out = append(out, CapabilityToolNames(slug)...)
	}
	return out
}
