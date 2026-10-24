//go:build computeruse

package tools

import (
	"sort"
	"testing"
)

// TestComputeCapabilities_GUI pins the computeruse-build variant: the
// descriptor advertises exactly the actions the router serves (the
// computerActionHandlers table) -- including window_list /
// window_focus since memql-cockpit#167 -- computerUseAvailable=true, and
// the meta `capabilities` action stays out of the list. CI runs
// headless so this only executes on local computeruse-tagged runs, but it
// keeps the variant honest for anyone building -tags computeruse.
func TestComputeCapabilities_GUI(t *testing.T) {
	desc := computeCapabilities("darwin", func(string) string { return "" })
	if !desc.ComputerUseAvailable {
		t.Error("computeruse build must report computerUseAvailable=true")
	}
	if desc.DisplayServer != "quartz" {
		t.Errorf("darwin computeruse build must report displayServer=quartz; got %q", desc.DisplayServer)
	}
	if !sort.StringsAreSorted(desc.Actions) {
		t.Errorf("actions must be sorted for a stable wire shape; got %v", desc.Actions)
	}
	// The advertised list = the computer-use router table + the build-agnostic
	// set (`wait`, memql-cockpit#166).
	if want := len(computerActionHandlers) + len(buildAgnosticComputerActions); len(desc.Actions) != want {
		t.Errorf("actions length %d must match router table + build-agnostic set (%d)", len(desc.Actions), want)
	}
	agnostic := make(map[string]bool, len(buildAgnosticComputerActions))
	for _, a := range buildAgnosticComputerActions {
		agnostic[a] = true
	}
	for _, a := range desc.Actions {
		if _, ok := computerActionHandlers[a]; !ok && !agnostic[a] {
			t.Errorf("advertised action %q is neither in the router table nor build-agnostic", a)
		}
	}
	for _, a := range desc.Actions {
		if a == "capabilities" {
			t.Error(`"capabilities" must not be advertised in actions (it is build-agnostic and always available)`)
		}
	}
	// window_list / window_focus shipped for real in
	// memql-cockpit#167; wait / key_hold / mouse_down / mouse_up in
	// memql-cockpit#166 -- the computeruse build must advertise them all.
	for _, required := range []string{"window_list", "window_focus", "wait", "key_hold", "mouse_down", "mouse_up"} {
		found := false
		for _, a := range desc.Actions {
			if a == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q must be advertised in actions; got %v", required, desc.Actions)
		}
	}
	// displayServer is quartz here, so the descriptor must carry a
	// live display count of at least one (memql-cockpit#165).
	if desc.Displays < 1 {
		t.Errorf("computeruse build with a reachable display server must report displays >= 1; got %d", desc.Displays)
	}
}
