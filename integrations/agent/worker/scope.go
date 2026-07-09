//go:build agent

package worker

import (
	workerservice "github.com/znasllc-io/memql/component/worker"
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
