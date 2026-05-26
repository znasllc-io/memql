package safety

import "strings"

// DecisionPolicy turns a Classification + the caller's context into
// the Decision the Gate will honour. The default implementation
// (DefaultDecisionPolicy) encodes the risk × scope × categories
// matrix from issue #231; app boot can override via
// `GateOptions.DecisionPolicy` -- the expected production swap is a
// thin wrapper that delegates to a DSL-authored `commandRiskDecision`
// policy via `engine.EvaluatePolicy`, once the procedural-policy DSL
// path is exercised end-to-end (today no procedural policy exists in
// the tree, so #231 ships the matrix in Go and the DSL migration is
// a follow-up).
//
// The interface is intentionally narrow + stateless so a custom
// impl can be a single struct + one Decide call -- no lifecycle, no
// dependencies on the engine.
type DecisionPolicy interface {
	Decide(in DecisionInput) Decision
}

// DecisionInput is everything DecisionPolicy.Decide sees: the
// classifier verdict plus the descriptor's caller + surface fields.
// Plain value type so a custom impl can pattern-match easily.
type DecisionInput struct {
	Classification Classification
	Surface        Surface
	// Scope is the effective scope from the descriptor's
	// CallerContext.Scope (already resolved by the existing
	// scope/kill-switch gate -- the decision policy doesn't
	// re-derive it). "observe" / "interact" / "full" / "".
	Scope string
	// Capability is the agent-side capability that already
	// authorised this surface (workerHost / workerComputer /
	// workbench_use / ""). Read-only; informational for the policy.
	Capability string
}

// DefaultDecisionPolicy implements the #231 matrix:
//
// Tier mapping:
//   - none / low        -> Allow
//   - medium            -> Allow if scope >= interact, else Ask
//   - high              -> Ask (regardless of scope)
//   - critical          -> Deny (only an explicit elevated approval
//     via #232 may override)
//
// Category floors (applied BEFORE the tier mapping; force a minimum
// tier when these categories appear -- they're the harm classes
// where the rule layer / model verdict alone shouldn't be enough):
//   - remote_code                       -> floor=Critical
//   - credential_access / exfiltration  -> floor=High
//   - sandbox_escape                    -> floor=High
//
// Surface strictness: `computer_use_headless` and
// `computer_use_embodied` are one notch stricter than the sandboxed
// surfaces (workbench / tool_*). The blast radius is the user's
// real machine, so a "medium" verdict that would Allow-on-scope in
// the workbench escalates to Ask on computer-use regardless of
// scope. We don't bump Ask -> Deny without user input -- that would
// strand the agent silently; #232's approval flow is where the
// user actually decides.
//
// Confidence is currently not consumed here -- low-confidence
// high/critical handling is a deliberate Phase-3 / #235 concern
// once the red-team corpus tells us the right threshold.
type DefaultDecisionPolicy struct{}

// Decide applies the matrix.
func (DefaultDecisionPolicy) Decide(in DecisionInput) Decision {
	tier := effectiveTier(in.Classification.Tier, in.Classification.Categories)
	switch tier {
	case TierNone, TierLow:
		return DecisionAllow
	case TierMedium:
		if isComputerUseSurface(in.Surface) {
			// One notch stricter on the user's real machine: a
			// medium-tier action needs user approval regardless of
			// scope, since "interact" on computer_use is doing
			// real things to the user's system.
			return DecisionAsk
		}
		if scopeAtLeastInteract(in.Scope) {
			return DecisionAllow
		}
		return DecisionAsk
	case TierHigh:
		return DecisionAsk
	case TierCritical:
		return DecisionDeny
	default:
		// Defensive: an out-of-range tier (shouldn't happen --
		// String/Parse cover the enum) falls through to Allow so
		// shadow-mode telemetry surfaces it without the surface
		// blocking.
		return DecisionAllow
	}
}

// effectiveTier applies the category floors to the raw tier from
// the classifier. The verdict for an action carrying a
// "credential_access" category is never below High no matter what
// the classifier said about overall tier -- the harm class is the
// dominant signal.
func effectiveTier(t RiskTier, cats []Category) RiskTier {
	floor := TierNone
	for _, c := range cats {
		switch c {
		case CategoryRemoteCode:
			if TierCritical > floor {
				floor = TierCritical
			}
		case CategoryCredentialAccess,
			CategoryExfiltration,
			CategorySandboxEscape:
			if TierHigh > floor {
				floor = TierHigh
			}
		}
	}
	if floor > t {
		return floor
	}
	return t
}

// scopeAtLeastInteract returns true when the caller's effective
// scope is `interact` or `full`. Case-insensitive + whitespace-
// tolerant since the scope comes from request payloads + DB rows.
// The known scope ladder today: observe < interact < full.
func scopeAtLeastInteract(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "interact", "full":
		return true
	default:
		return false
	}
}

// isComputerUseSurface reports whether the surface is one of the
// two computer-use variants (highest blast radius). Centralised so
// "what counts as computer-use" lives in one place.
func isComputerUseSurface(s Surface) bool {
	return s == SurfaceComputerUseHeadless || s == SurfaceComputerUseEmbodied
}
