package safety

import "testing"

// TestWorkbenchExecAllowlistShape pins the basics so a careless
// `delete()` on the curated set in allowlists.go shows up loudly.
// The exact members are intentionally checked by the workbench's
// existing exec_allowlist_test.go (which runs through the
// `EnforceExecAllowlist` helper that now reads from this set).
func TestWorkbenchExecAllowlistShape(t *testing.T) {
	// Must contain the common dev binaries that ship in the
	// curated set; a regression that drops these would fail the
	// workbench's own tests too, but a duplicate guard here catches
	// edits that "tidy" the set without realising the rule layer
	// depends on the same members.
	must := []string{"ls", "cat", "grep", "git", "python3", "node", "jq"}
	for _, b := range must {
		if _, ok := WorkbenchExecAllowlist[b]; !ok {
			t.Errorf("WorkbenchExecAllowlist missing %q", b)
		}
	}
	if len(WorkbenchExecAllowlist) < 40 {
		t.Errorf("WorkbenchExecAllowlist looks truncated: %d entries", len(WorkbenchExecAllowlist))
	}
}

// TestSafeReadOnlyBinariesIsTight pins that the fast-path set stays
// small + read-only. Anything mutating (rm / chmod / curl) here
// would skip the LLM for genuinely-risky commands.
func TestSafeReadOnlyBinariesIsTight(t *testing.T) {
	forbidden := []string{"rm", "chmod", "curl", "wget", "git", "pip", "npm", "go", "tee", "cp", "mv", "tar", "env"}
	for _, b := range forbidden {
		if _, ok := safeReadOnlyBinaries[b]; ok {
			t.Errorf("safeReadOnlyBinaries should not contain %q -- removes LLM oversight on a mutating binary", b)
		}
	}
}
