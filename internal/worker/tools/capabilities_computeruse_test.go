//go:build computeruse

package tools

import (
	"slices"
	"sort"
	"testing"
)

// Capability discovery advertises supported actions, not legacy global helpers.
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
	if want := 11; len(desc.Actions) != want {
		t.Fatalf("supported macOS actions = %v", desc.Actions)
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
	for _, required := range []string{"window_focus", "window_list", "wait", "screenshot", "key_type", "key_combo", "mouse_move", "mouse_click", "mouse_scroll"} {
		if !slices.Contains(desc.Actions, required) {
			t.Errorf("missing supported action %s", required)
		}
	}
	for _, disabled := range []string{"key_hold", "mouse_down", "mouse_up", "mouse_drag"} {
		if slices.Contains(desc.Actions, disabled) {
			t.Errorf("advertised disabled action %s", disabled)
		}
	}
	if desc.ActionContracts["workerComputer.input_target"] != 1 || desc.ActionContracts["workerComputer.window_selection"] != 1 || desc.InputTargetPolicy["focusActivation"] != true {
		t.Fatal("missing target contract", desc)
	}
	linux := computeCapabilities("linux", func(name string) string {
		if name == "DISPLAY" {
			return ":0"
		}
		return ""
	})
	for _, action := range linux.Actions {
		if isComputerInputAction(action) || action == "window_focus" {
			t.Errorf("Linux advertises unsupported input %s", action)
		}
	}
	if len(linux.InputTargetPolicy["supportedInputActions"].([]string)) != 0 {
		t.Fatal("Linux promises targeted backend")
	}
	// displayServer is quartz here, so the descriptor must carry a
	// live display count of at least one (memql-cockpit#165).
	if desc.Displays < 1 {
		t.Errorf("computeruse build with a reachable display server must report displays >= 1; got %d", desc.Displays)
	}
}
