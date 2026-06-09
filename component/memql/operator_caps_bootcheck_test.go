package memql

import "testing"

// allCapabilityTools returns a membership set with every tool from every
// capability slug present.
func allCapabilityTools() map[string]bool {
	all := map[string]bool{}
	for _, tools := range capabilitySlugs {
		for _, n := range tools {
			all[n] = true
		}
	}
	return all
}

func TestVerifyCapabilityToolsRegistered_AllPresent_NoMissing(t *testing.T) {
	all := allCapabilityTools()
	missing := VerifyCapabilityToolsRegistered(func(s string) bool { return all[s] }, testGuardLogger())
	if len(missing) != 0 {
		t.Fatalf("a fully-registered set must report nothing missing, got %v", missing)
	}
}

// The memql#1154 shape: workbenchHost dropped while its sibling canvasPublish
// (same workbench-use slug) is still registered -> the partial set is the bug
// and must be reported.
func TestVerifyCapabilityToolsRegistered_PartialDrop_Reported(t *testing.T) {
	set := allCapabilityTools()
	delete(set, "workbenchHost")
	missing := VerifyCapabilityToolsRegistered(func(s string) bool { return set[s] }, testGuardLogger())
	found := false
	for _, m := range missing {
		if m == "workbenchHost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropping workbenchHost while canvasPublish is present must be reported, got %v", missing)
	}
}

// A node that doesn't load the capability surface at all (e.g. voice/identity:
// NONE of a slug's tools registered) must NOT false-alarm -- the slug is
// skipped because no tool of it is present.
func TestVerifyCapabilityToolsRegistered_NoneRegistered_SelfScopesOut(t *testing.T) {
	missing := VerifyCapabilityToolsRegistered(func(string) bool { return false }, testGuardLogger())
	if len(missing) != 0 {
		t.Fatalf("a node with none of the capability tools must not false-alarm, got %v", missing)
	}
}

// Dropping the WHOLE worker set (all of computer-use-headless) but keeping the
// shared canvasPublish: the slug still has canvasPublish present (anyPresent),
// so the dropped worker tools must be reported.
func TestVerifyCapabilityToolsRegistered_WholeWorkerSetDropped_Reported(t *testing.T) {
	set := map[string]bool{"canvasPublish": true} // only the shared tool survives
	missing := VerifyCapabilityToolsRegistered(func(s string) bool { return set[s] }, testGuardLogger())
	for _, want := range []string{"workerHost", "workerStatus", "requestComputerUseScope"} {
		seen := false
		for _, m := range missing {
			if m == want {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("expected %q reported missing (canvasPublish present so the slug is checked), got %v", want, missing)
		}
	}
}
