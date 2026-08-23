//go:build agent

package worker

import (
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
	// The need vocabulary is imported rather than restated. integrations/workbench
	// owns it -- it declares the constants, parses the hint and decides the
	// mismatch -- and a second copy here would be one more place for the closed
	// set to drift out of step with the thing that produces it.
	"github.com/znasllc-io/memql/integrations/workbench"
)

// ScopeRequirement describes the capability + scope tier an action
// requires.
type ScopeRequirement struct {
	Capability string
	Scope      string
}

// scopeRank gives an ordering on scopes: empty < observe < full.
//
// The earlier `interact` tier (mouse / keyboard / scroll without
// shell exec or filesystem writes) was retired -- in practice the
// agent kept hitting cases where a shell command was the cleaner
// path even after committing to interact, and the user wanted a
// simpler two-tier model: read-only observation, or full machine
// access. Legacy data carrying `interact` is upgraded to `full` on
// read so existing agentAuthorization rows keep working without a
// migration.
var scopeRank = map[string]int{
	"":         0,
	"observe":  1,
	"interact": 2, // legacy: treated as full on the read path
	"full":     2,
}

// scopeAllows reports whether the agent's effective scope satisfies
// the action's required scope tier.
func scopeAllows(have, need string) bool {
	return scopeRank[have] >= scopeRank[need]
}

// scopeIsNarrowerOrEqual reports whether `child` is a narrower-or-
// equal scope to `parent`. Used to enforce that a Plan-level scope
// override never widens the agent's standing scope.
func scopeIsNarrowerOrEqual(child, parent string) bool {
	return scopeRank[child] <= scopeRank[parent]
}

// actionRequiredScope maps a (tool, action) pair to its required
// capability + scope tier. Centralized here so the policy table is
// reviewable in one place.
func actionRequiredScope(tool, action string) ScopeRequirement {
	switch tool {
	case "workerHost":
		return workerHostScope(action)
	case "workerComputer":
		return workerComputerScope(action)
	}
	return ScopeRequirement{}
}

func workerHostScope(action string) ScopeRequirement {
	switch action {
	case "fs_read", "fs_list", "fs_stat":
		return ScopeRequirement{Capability: workerservice.CapabilityHeadless, Scope: "observe"}
	case "http_fetch":
		// Observe allows GET-only; the action itself does not gate
		// method, the policy engine on the cockpit side does. We
		// still admit at observe so dispatch lets the call through.
		return ScopeRequirement{Capability: workerservice.CapabilityHeadless, Scope: "observe"}
	case "exec", "fs_write":
		return ScopeRequirement{Capability: workerservice.CapabilityHeadless, Scope: "full"}
	}
	return ScopeRequirement{}
}

func workerComputerScope(action string) ScopeRequirement {
	switch action {
	case "capabilities", "wait":
		// Observe-tier, but gated on HEADLESS rather than COMPUTERUSE.
		// `capabilities` is pure introspection and `wait` is a no-op
		// timer; the cockpit serves both in its headless AND
		// computer-use builds (cockpit #162 / #177). HEADLESS is
		// mandatory on every registration (COMPUTERUSE workers
		// advertise it too), so requiring CapabilityHeadless admits
		// every worker. Requiring CapabilityComputerUse here would make
		// PickWorker skip a headless-only worker and wrongly return
		// no_worker_available for an introspection / timing call the
		// worker can actually serve.
		return ScopeRequirement{Capability: workerservice.CapabilityHeadless, Scope: "observe"}
	case "screenshot", "cursor_position", "display_info", "window_list":
		return ScopeRequirement{Capability: workerservice.CapabilityComputerUse, Scope: "observe"}
	case "mouse_move", "mouse_click", "mouse_drag", "mouse_scroll",
		"mouse_down", "mouse_up", "key_type", "key_combo", "key_hold",
		"window_focus":
		// Required scope is full -- the legacy `interact` tier was
		// retired (see scopeRank). Drives + shell are the two
		// machine-touching surfaces; we don't ask the user to
		// distinguish "input-driving without shell" anymore.
		return ScopeRequirement{Capability: workerservice.CapabilityComputerUse, Scope: "full"}
	}
	return ScopeRequirement{}
}

// -- workbench needs -> the fleet (memql#4353) --------------------------------
//
// THE MAPPING LIVES HERE, beside the scope ladder it reads, and not in the
// tool loop. The tool loop's job is to notice a mismatch and act on it; deciding
// that "this action needs the user's files" implies a particular tier of access
// to the user's machine is a scope judgment, and scope judgments have one home.
// Written in the loop it would be a second copy of the ladder, and the two
// would drift in the direction that reads as safe -- a loop that asks for
// `observe` where the ladder says `full` produces a card the user approves for
// something narrower than what then runs.

// EnvironmentNeedsScope returns the scope tier a set of unmet workbench needs
// implies on the user's own machine.
//
// `user_files` alone is a READ: the workbench could not see the file, and the
// machine is being asked to look at it. Everything else -- a display to drive,
// a GPU to use, macOS tooling to run, or simply a different operating system,
// which in practice means running commands there -- is `full`. An empty or
// unrecognised set yields `full`, the conservative direction: an unknown need
// asking for the narrower tier is how a card gets approved for less than what
// runs.
func EnvironmentNeedsScope(needs []string) string {
	if len(needs) == 0 {
		return "full"
	}
	for _, need := range needs {
		if need != workbench.NeedUserFiles {
			return "full"
		}
	}
	return "observe"
}

// EnvironmentNeedsLabels turns unmet needs into the routing requirement the
// dispatch carries, so the router picks a machine that can actually do the work
// rather than the first one that answers.
//
// requestedOS, when the hint named one, becomes `os=<goos>` -- the single most
// load-bearing label, because "the workbench is Linux and this needs macOS" is
// the common case and picking another Linux box would fail identically.
func EnvironmentNeedsLabels(needs []string, requestedOS string) map[string]string {
	out := map[string]string{}
	if os := strings.TrimSpace(requestedOS); os != "" {
		out["os"] = os
	}
	for _, need := range needs {
		switch need {
		case workbench.NeedDisplay:
			out["display"] = "true"
		case workbench.NeedGPU:
			out["gpu"] = "true"
		case workbench.NeedMacOSTooling:
			// A need for macOS tooling IS a need for macOS. Stated as the os
			// label rather than a second one, so a machine tagged only
			// `os=darwin` by the cockpit still matches -- an owner should not
			// have to hand-tag their Mac to make Xcode work.
			out["os"] = "darwin"
		case workbench.NeedUserFiles:
			// No label. "The files are on the user's machine" is true of every
			// machine the user owns, so requiring anything here would narrow
			// the candidate set for no reason.
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
