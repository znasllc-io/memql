// Package actiontrust implements Phase 4 of the action library (#1739, epic
// #1734): the surface-aware trust gate and the reliability lifecycle that
// decides when a minted action is safe to offer for replay.
//
// The rule, from the doc (§9.2): a `read` on any surface, or a `write`/`exec`
// in the sandboxed workbench, auto-promotes silently (the confidence signal is
// free -- every replay is fingerprint-verified). A `write`/`exec` on a
// real-machine surface (computer-use) or an external MCP surface always
// suggests + confirms: the system flags "this looks stable" after N verified
// runs and a human confirms with one click.
//
// It is pure: callers supply (sideEffectClass, surface, reliability,
// reinforceCount, age) and this package returns the gate decision, the mint
// status, the decayed reliability, and the deprecate verdict. The recording
// lifecycle (a scheduled consolidation loop, the #586 sibling) and the confirm
// UI drive these; the math lives here where it is testable.
package actiontrust

import (
	"math"
	"strings"
)

// GateDecision is how a minted action is promoted onto the replay ladder.
type GateDecision string

const (
	// AutoPromote: safe to offer for replay immediately (mints active).
	AutoPromote GateDecision = "auto"
	// SuggestConfirm: held off the replay ladder until a human confirms
	// (mints candidate); the system suggests it once it looks stable.
	SuggestConfirm GateDecision = "confirm"
)

// Defaults for the lifecycle thresholds. Callers may override.
const (
	DefaultDeprecateFloor = 0.2  // below this reliability an action is deprecated
	DefaultSuggestRuns    = 3    // verified replays before a confirm is suggested
	DefaultSuggestRel     = 0.75 // reliability before a confirm is suggested
)

// Gate decides how to promote a freshly minted action, keyed on its side
// effect class and the surface it runs on. surfaceKind is one of "workbench",
// "computer-use", "mcp" (empty defaults to the sandbox workbench).
func Gate(sideEffectClass, surfaceKind string) GateDecision {
	if strings.EqualFold(sideEffectClass, "read") {
		return AutoPromote // reads are reversible everywhere
	}
	// write / exec: real-machine and external surfaces always confirm.
	switch strings.ToLower(strings.TrimSpace(surfaceKind)) {
	case "computer-use", "mcp":
		return SuggestConfirm
	default:
		return AutoPromote // sandboxed workbench (or unset default)
	}
}

// InitialStatus maps a gate decision to the action's mint status: an
// auto-promoted action mints `active` (offered for replay immediately); a
// confirm-gated action mints `candidate` (held until a human confirms).
func InitialStatus(d GateDecision) string {
	if d == SuggestConfirm {
		return "candidate"
	}
	return "active"
}

// readCapabilityHints are capability-name fragments that indicate a read-only
// operation (no side effect). Anything else is treated as write/exec.
var readCapabilityHints = []string{
	"read", "list", "stat", "get", "fetch", "query", "search", "head", "describe",
}

// ClassifySideEffect infers a coarse side-effect class from a capability verb.
// Conservative: only clearly read-shaped verbs are "read"; everything else is
// "write" (the gate then errs toward confirming real-machine effects).
func ClassifySideEffect(capability string) string {
	c := strings.ToLower(capability)
	for _, h := range readCapabilityHints {
		if strings.Contains(c, h) {
			return "read"
		}
	}
	return "write"
}

// Decay returns reliability after ageSeconds of disuse under an exponential
// half-life (mirrors the semanticMemory decay sweep). Non-positive age or
// half-life leaves reliability unchanged.
func Decay(reliability, ageSeconds, halfLifeSeconds float64) float64 {
	if halfLifeSeconds <= 0 || ageSeconds <= 0 {
		return reliability
	}
	r := reliability * math.Pow(0.5, ageSeconds/halfLifeSeconds)
	if r < 0 {
		return 0
	}
	return r
}

// ShouldDeprecate reports whether reliability has fallen below the floor, in
// which case the action is deprecated and no longer offered to the planner.
func ShouldDeprecate(reliability, floor float64) bool {
	return reliability < floor
}

// ReadyToSuggest reports whether a confirm-gated candidate has accrued enough
// verified replays + reliability to surface a "this looks stable" suggestion.
func ReadyToSuggest(reinforceCount int, reliability float64, minRuns int, minReliability float64) bool {
	return reinforceCount >= minRuns && reliability >= minReliability
}
