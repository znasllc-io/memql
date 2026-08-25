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

	// The third id is "switch_<expression>", not the authored step name: the
	// compiler derives a switch step's id from what it keys on. That is the
	// shape being asserted -- an authored name here would mean the step is no
	// longer a switch.
	ids := stepIDs(a)
	want := []string{"gate", "verdict", "switch_steps.verdict.result"}
	if len(ids) != len(want) {
		t.Fatalf("installInstance has steps %v, want exactly %v.\n"+
			"A NEW SIBLING STEP IS THE REGRESSION THIS CATCHES: a sibling that reads only the "+
			"event payload carries no data edge, so the topo-sort is free to run it BEFORE the "+
			"render-diff gate -- which means it runs on a version bump the gate was about to "+
			"refuse. New install work belongs inside the gated case, not beside it.", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("installInstance step order = %v, want %v", ids, want)
		}
	}

	install := a.Steps[len(a.Steps)-1]
	if install.Type != automations.StepTypeSwitch {
		t.Fatalf("the install step is a %q, want a switch on the render-diff verdict. "+
			"Flattening it into sibling steps removes the only thing forcing the gate to run "+
			"first.", install.Type)
	}
	if install.Switch == nil {
		t.Fatal("the install step is typed switch but carries no switch config")
	}
	if !strings.Contains(install.Switch.Expression, "verdict") {
		t.Errorf("the install switch keys on %q, want the render-diff verdict step. Switching on "+
			"anything else silently removes the gate while leaving its shape in place.",
			install.Switch.Expression)
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
func TestRepairInstanceChecksBeforeItSyncs(t *testing.T) {
	a := loadInstallAutomation(t, "repairInstance")

	ids := stepIDs(a)
	want := []string{"verify", "verdict", "switch_steps.verdict.result"}
	if len(ids) != len(want) {
		t.Fatalf("repairInstance steps = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("repairInstance step order = %v, want %v -- the check must precede the "+
				"sync, or the sync's success is what the operator reads", ids, want)
		}
	}

	resync := a.Steps[len(a.Steps)-1]
	if resync.Type != automations.StepTypeSwitch {
		t.Fatalf("the resync step is not a switch on the dependency verdict (got %v). "+
			"An unconditional re-sync cannot create a CRD nobody installed, so it changes "+
			"nothing and reports Healthy -- which is the failure this verb exists to stop.",
			resync.Type)
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
