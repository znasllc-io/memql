package memql

import "testing"

// allCapabilityTools returns a membership set with every tool from every
// registered capability slug present.
func allCapabilityTools() map[string]bool {
	all := map[string]bool{}
	capMu.RLock()
	defer capMu.RUnlock()
	for _, bundle := range capabilitySlugs {
		for _, n := range bundle.tools {
			all[HarnessToolBinding(n)] = true
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

// A worker primitive dropped while its status/scope siblings remain is a
// partial core capability and must still be reported.
func TestVerifyCapabilityToolsRegistered_PartialDrop_Reported(t *testing.T) {
	set := allCapabilityTools()
	delete(set, "worker.workerHost")
	missing := VerifyCapabilityToolsRegistered(func(s string) bool { return set[s] }, testGuardLogger())
	found := false
	for _, m := range missing {
		if m == "workerHost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropping workerHost while its siblings are present must be reported, got %v", missing)
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

// Dropping both execution primitives while keeping core status must report
// the broken bundles. Product presentation tools cannot satisfy this check.
func TestVerifyCapabilityToolsRegistered_ExecutionToolsDropped_Reported(t *testing.T) {
	set := map[string]bool{"worker.workerStatus": true, "canvasPublish": true}
	missing := VerifyCapabilityToolsRegistered(func(s string) bool { return set[s] }, testGuardLogger())
	for _, want := range []string{"workerHost", "workerComputer", "requestComputerUseScope"} {
		seen := false
		for _, m := range missing {
			if m == want {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("expected %q reported missing (workerStatus present so the slug is checked), got %v", want, missing)
		}
	}
}
