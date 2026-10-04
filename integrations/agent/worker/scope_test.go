//go:build agent

package worker

import (
	"testing"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// TestActionRequiredScope_Table pins the full (tool, action) ->
// (capability, scope) policy table. Every admitted action appears
// here so a classification change (a new action, a tier move) is a
// reviewable diff against this table, not a silent behavior change
// in dispatch.
func TestActionRequiredScope_Table(t *testing.T) {
	headless := workerservice.CapabilityHeadless
	computerUse := workerservice.CapabilityComputerUse

	cases := []struct {
		tool   string
		action string
		want   ScopeRequirement
	}{
		// --- workerHost (unchanged) ---
		{"workerHost", "fs_read", ScopeRequirement{Capability: headless, Scope: "observe"}},
		{"workerHost", "fs_list", ScopeRequirement{Capability: headless, Scope: "observe"}},
		{"workerHost", "fs_stat", ScopeRequirement{Capability: headless, Scope: "observe"}},
		{"workerHost", "http_fetch", ScopeRequirement{Capability: headless, Scope: "observe"}},
		{"workerHost", "exec", ScopeRequirement{Capability: headless, Scope: "full"}},
		{"workerHost", "fs_write", ScopeRequirement{Capability: headless, Scope: "full"}},

		// --- workerComputer introspection / timing (cockpit #162 /
		// #177): served by BOTH cockpit builds, so they gate on
		// HEADLESS (mandatory on every registration) -- requiring
		// COMPUTERUSE would make the router filter out a headless-only
		// worker that can actually answer.
		{"workerComputer", "capabilities", ScopeRequirement{Capability: headless, Scope: "observe"}},
		{"workerComputer", "wait", ScopeRequirement{Capability: headless, Scope: "observe"}},

		// --- workerComputer read-only observation (unchanged) ---
		{"workerComputer", "screenshot", ScopeRequirement{Capability: computerUse, Scope: "observe"}},
		{"workerComputer", "cursor_position", ScopeRequirement{Capability: computerUse, Scope: "observe"}},
		{"workerComputer", "display_info", ScopeRequirement{Capability: computerUse, Scope: "observe"}},
		{"workerComputer", "window_list", ScopeRequirement{Capability: computerUse, Scope: "observe"}},

		// --- workerComputer input actions: full tier (the interact
		// tier is retired). Pre-existing set unchanged...
		{"workerComputer", "mouse_move", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "mouse_click", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "mouse_drag", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "mouse_scroll", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "key_type", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "key_combo", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "window_focus", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		// ...plus the computer-use v2 additions (cockpit #166).
		{"workerComputer", "mouse_down", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "mouse_up", ScopeRequirement{Capability: computerUse, Scope: "full"}},
		{"workerComputer", "key_hold", ScopeRequirement{Capability: computerUse, Scope: "full"}},

		// --- unknown actions / tools resolve to the zero value so
		// preDispatchCheck denies them as unknown_action ---
		{"workerComputer", "format_disk", ScopeRequirement{}},
		{"workerHost", "wait", ScopeRequirement{}},
		{"workerHost", "capabilities", ScopeRequirement{}},
		{"someOtherTool", "exec", ScopeRequirement{}},
	}

	for _, tc := range cases {
		got := actionRequiredScope(tc.tool, tc.action)
		if got != tc.want {
			t.Errorf("%s.%s: got %+v, want %+v", tc.tool, tc.action, got, tc.want)
		}
	}
}

// TestActionRequiredScope_NewActionsAdmitObserveOrFull asserts the
// tier semantics end to end through scopeAllows: an observe-scoped
// agent may call capabilities + wait but none of the new input
// actions; a full-scoped agent may call all of them.
func TestActionRequiredScope_NewActionsAdmitObserveOrFull(t *testing.T) {
	observeOK := []string{"capabilities", "wait"}
	fullOnly := []string{"key_hold", "mouse_down", "mouse_up"}

	for _, action := range observeOK {
		req := actionRequiredScope("workerComputer", action)
		if !scopeAllows("observe", req.Scope) {
			t.Errorf("%s: observe scope must admit, requires %q", action, req.Scope)
		}
	}
	for _, action := range fullOnly {
		req := actionRequiredScope("workerComputer", action)
		if scopeAllows("observe", req.Scope) {
			t.Errorf("%s: observe scope must NOT admit input action", action)
		}
		if !scopeAllows("full", req.Scope) {
			t.Errorf("%s: full scope must admit, requires %q", action, req.Scope)
		}
		// Legacy `interact` rows read as full (scopeRank upgrade).
		if !scopeAllows("interact", req.Scope) {
			t.Errorf("%s: legacy interact scope must read as full", action)
		}
	}
}

// TestDockerNeedMapsToTheDockerLabel -- epic memql#5478 (#5494).
//
// A step that needs Docker has to reach a machine that has it, and fleet labels
// match EXACTLY: there is no "any value" form and no prefix match, so the
// requirement is the literal pair docker=true or it routes nowhere. Before
// this, an unrecognised need produced no label at all -- the dispatch would
// have gone to the first machine that answered, with or without a daemon.
//
// The scope half is the ladder's ordinary answer for a need that runs commands
// on the machine: full, never the observe tier user_files alone earns.
func TestDockerNeedMapsToTheDockerLabel(t *testing.T) {
	if got := EnvironmentNeedsLabels([]string{"docker"}, ""); len(got) != 1 || got["docker"] != "true" {
		t.Fatalf("labels = %v, want exactly map[docker:true]", got)
	}
	// Beside an os the hint named, both requirements survive: a Linux machine
	// without Docker and a Docker machine on the wrong OS are both wrong.
	if got := EnvironmentNeedsLabels([]string{"docker"}, "linux"); len(got) != 2 || got["docker"] != "true" || got["os"] != "linux" {
		t.Fatalf("labels = %v, want exactly map[docker:true os:linux]", got)
	}
	if got := EnvironmentNeedsScope([]string{"docker"}); got != "full" {
		t.Fatalf("scope = %q, want full -- running containers on somebody's machine is not a read", got)
	}
}
