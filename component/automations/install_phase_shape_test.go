package automations_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// install_phase_shape_test.go -- epic memql#4490.
//
// installInstance was a one-step alias for argoSync. It now owns the eleven
// ordered steps between a provisioned substrate and a sync that means
// something, and TWO structural claims are load-bearing enough to assert
// rather than believe:
//
//  1. THE INSTALL IS INSIDE A GATED SWITCH, not a row of sibling steps. The
//     scheduler runs steps in dependency topo-sort order, so siblings that all
//     read only the event payload keep authored order -- but the render-diff
//     gate creates a data edge, and a sibling step without one would sort
//     AHEAD of it. Nesting the whole install inside the gated case makes the
//     sequence structural instead of incidental.
//
//  2. THE SEQUENCE ENDS WITH argoSync, NOT STARTS WITH IT. Every operator and
//     every credential has to exist first; a sync against a cluster with none
//     of them syncs nothing at all, and reports success doing it.
//
// Neither is checkable by reading a rendered plan, because there is no plan to
// read until something fires -- which on this verb means a real cloud
// subscription.

func loadInstallAutomation(t *testing.T, name string) *automations.Automation {
	t.Helper()
	t.Setenv(memql.AllowSkipsEnvVar, "")
	loader := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)})
	all, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, a := range all {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("automation %q did not load; the shipped tree has %d automations", name, len(all))
	return nil
}

// stepIDs is the authored order as the loader produced it.
func stepIDs(a *automations.Automation) []string {
	out := make([]string, 0, len(a.Steps))
	for _, s := range a.Steps {
		out = append(out, s.ID)
	}
	return out
}

// TestInstallInstanceGatesTheWholeInstall pins claim 1.
func TestInstallInstanceGatesTheWholeInstall(t *testing.T) {
	a := loadInstallAutomation(t, "installInstance")
	if !a.IsStatementBody() {
		t.Fatal("installInstance did not load as a statement body")
	}

	// The render-diff gate, then its verdict, then the install -- which is
	// the switch on the verdict, flattened: each install step carries the
	// verdict in its condition.
	ids := stepIDs(a)
	if len(ids) < 3 || ids[0] != "gate" || ids[1] != "verdict" {
		t.Fatalf("installInstance steps = %v, want the render-diff gate, then its verdict, then the install", ids)
	}
	for _, s := range a.Steps[2:] {
		if !strings.Contains(s.Condition, "verdict") {
			t.Errorf("installInstance's %s runs whatever the render-diff verdict (condition %q).\n"+
				"A NEW SIBLING STATEMENT IS THE REGRESSION THIS CATCHES: written beside the switch "+
				"instead of inside its gated case, it runs on a version bump the gate was about to "+
				"refuse. New install work belongs inside the gated case.", s.ID, s.Condition)
		}
	}
}

// TestInstallInstanceSyncsLast pins claim 2, and pins the eleven steps by the
// capability ids they actually reach -- so a step deleted in a refactor fails
// here rather than on a cloud subscription.
func TestInstallInstanceSyncsLast(t *testing.T) {
	a := loadInstallAutomation(t, "installInstance")

	body := automationBodyText(t, a)

	// The order the eleven steps must appear in. Listed, not discovered: the
	// failure worth catching is one of them being dropped, and discovery
	// would wave exactly that through.
	sequence := []string{
		"installClusterOperators",
		"seedInstanceSecrets",
		"wireExternalSecrets",
		"registerGitOpsRepo",
		"argoSync",
		"settleAfterSync",
		"verifyInstallDependencies",
	}
	at := -1
	for _, name := range sequence {
		idx := strings.Index(body, name)
		if idx < 0 {
			t.Fatalf("installInstance no longer reaches %q. That action exists BECAUSE the step "+
				"it performs failed silently when nobody did it: read its header before deciding "+
				"it is redundant.", name)
		}
		if idx < at {
			t.Errorf("%q appears before a step that must precede it. The order is: %v",
				name, sequence)
		}
		at = idx
	}

	// The specific inversion this whole epic is about.
	if strings.Index(body, "argoSync") < strings.Index(body, "installClusterOperators") {
		t.Error("argoSync runs before the operators are installed. A sync against a cluster with " +
			"no ArgoCD, no cert-manager and no CRDs syncs NOTHING, and that was the original " +
			"defect: substrate, then a wish.")
	}
}

// TestRepairInstanceChecksBeforeItSyncs -- a repair that only re-syncs declares
// success on a cluster whose CRDs were never installed, because nothing is
// unhealthy when the objects were never created.
//
// WHAT THIS PIN IS FOR, and what it is not. The load-bearing claim is the
// ORDER: verify, then a verdict, then a sync that is CONDITIONAL on it. The
// exact step list is pinned only so that order cannot be quietly rearranged.
//
// So adding a step is a legitimate change and updating `want` is the right
// response to it -- what must never be updated away is the check-before-sync
// relation asserted below. `version` is the first such addition (memql#4486):
// a REPORT of the declared/rendered/running refs, deliberately a sibling of the
// gate rather than nested under it, because a refused repair is exactly when an
// operator needs to know what is executing.
func TestRepairInstanceChecksBeforeItSyncs(t *testing.T) {
	a := loadInstallAutomation(t, "repairInstance")

	ids := stepIDs(a)
	want := []string{"version", "verify", "verdict", "argoSync"}
	if len(ids) != len(want) {
		t.Fatalf("repairInstance steps = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("repairInstance step order = %v, want %v -- the check must precede the "+
				"sync, or the sync's success is what the operator reads", ids, want)
		}
	}

	// The relation the step list exists to protect, asserted directly so it
	// survives any future re-ordering of the list above.
	verifyAt, verdictAt := indexOfStep(ids, "verify"), indexOfStep(ids, "verdict")
	syncAt := indexOfStep(ids, "argoSync")
	if verifyAt < 0 || verdictAt < 0 || syncAt < 0 {
		t.Fatalf("repairInstance lost one of verify/verdict/the conditional sync: %v", ids)
	}
	if !(verifyAt < verdictAt && verdictAt < syncAt) {
		t.Errorf("repairInstance order is %v. The check must precede the verdict and the verdict "+
			"must precede the sync: a sync cannot create a CRD nobody installed, so an "+
			"unconditional one changes nothing and reports Healthy.", ids)
	}

	if sync := a.Steps[syncAt]; !strings.Contains(sync.Condition, "verdict") {
		t.Fatalf("the re-sync runs whatever the dependency verdict (condition %q). "+
			"An unconditional re-sync cannot create a CRD nobody installed, so it changes "+
			"nothing and reports Healthy -- which is the failure this verb exists to stop.",
			sync.Condition)
	}
}

// automationBodyText renders the loaded automation back to JSON so the ORDER
// the actions appear in can be read. Reading the compiled form rather than the
// .memql source is what makes this a test of what the engine will run, rather
// than of what the file says.
func automationBodyText(t *testing.T, a *automations.Automation) string {
	t.Helper()
	b, err := json.Marshal(a.Steps)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	return string(b)
}

// indexOfStep returns the position of id in ids, or -1.
func indexOfStep(ids []string, id string) int {
	for i, got := range ids {
		if got == id {
			return i
		}
	}
	return -1
}
