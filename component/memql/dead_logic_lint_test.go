package memql

import "testing"

// The dead-logic lint replaced the @entrypoint registration audit (memql#2216).
// It runs over the real tree and reports orphan logics as warnings. This test
// also guards the @entrypoint retirement: the contrived serviceVersionProbe
// (whose only purpose was exercising @entrypoint) must be gone.
func TestDeadLogicLint(t *testing.T) {
	dead := DeadLogicNames(nil)
	for _, n := range dead {
		t.Logf("WARNING: dead logic %q (no logic/query/tool/automation references it)", n)
		if n == "serviceVersionProbe" {
			t.Errorf("serviceVersionProbe should have been retired with @entrypoint")
		}
	}
	// Sanity: a referenced logic must NOT be reported dead. registerNode is
	// invoked by the cluster registerNode automation.
	for _, n := range dead {
		if n == "registerNode" {
			t.Errorf("registerNode is referenced by an automation; must not be flagged dead")
		}
	}
}
