// Package safety is the command-classifier foundation: a dependency-
// free package that defines the risk taxonomy, the normalized
// `ActionDescriptor` that every execution surface lowers into, the
// `Classifier` interface, and the central `Gate` choke point that
// later phases plug enforcement, decisioning, audit, and human
// approval into.
//
// Phase 0 (issue #227) ships only the scaffolding -- the gate is
// unwired and the in-tree decision logic always returns `Allow`.
// Live behavior is unchanged. Later phases:
//
//   - #228: deterministic rule engine (a real `Classifier`).
//   - #229: wire the gate into worker / tool / workbench dispatch.
//   - #230: LLM semantic classifier + cache.
//   - #231: replace the always-allow decision stub with the
//     `commandRiskDecision` Policy DSL evaluation.
//   - #232: human-in-the-loop "ask" approval flow.
//   - #233: prompt-injection / output screening.
//   - #234: a `DecisionRecorder` that persists `v1:safety:classification`
//     rows (the slog recorder here is the temporary default).
//   - #235: rollout, fail-safe config, red-team corpus.
//
// The package intentionally imports neither `component/memql` nor
// any `integrations/*` package so it can be imported from both sides
// of the engine without import cycles.
package safety

// RiskTier classifies how dangerous a proposed action is. Ordered
// from least to most severe so callers can compare with `>=` /
// `<=`. `TierNone` means the classifier had no opinion (default
// for unclassified or noop paths).
type RiskTier int

const (
	TierNone RiskTier = iota
	TierLow
	TierMedium
	TierHigh
	TierCritical
)

// String returns the lower-case label used in audit, logs, and the
// policy DSL.
func (t RiskTier) String() string {
	switch t {
	case TierNone:
		return "none"
	case TierLow:
		return "low"
	case TierMedium:
		return "medium"
	case TierHigh:
		return "high"
	case TierCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ParseRiskTier is the inverse of String; unknown labels parse to
// TierNone with ok=false so policy authors can detect typos.
func ParseRiskTier(s string) (RiskTier, bool) {
	switch s {
	case "none":
		return TierNone, true
	case "low":
		return TierLow, true
	case "medium":
		return TierMedium, true
	case "high":
		return TierHigh, true
	case "critical":
		return TierCritical, true
	default:
		return TierNone, false
	}
}

// Category is a non-exclusive label describing WHY an action is
// risky. A single Classification can carry several. Categories are
// the input the decision policy keys off when a specific harm class
// should force a floor (e.g. `credential_access` deny-by-default
// regardless of overall tier).
type Category string

const (
	CategoryDestructive         Category = "destructive"
	CategoryExfiltration        Category = "exfiltration"
	CategoryRemoteCode          Category = "remote_code"
	CategoryPrivilegeEscalation Category = "privilege_escalation"
	CategoryPersistence         Category = "persistence"
	CategoryNetworkEgress       Category = "network_egress"
	CategoryCredentialAccess    Category = "credential_access"
	CategorySandboxEscape       Category = "sandbox_escape"
	CategoryPromptInjection     Category = "prompt_injection"
	CategoryUnknown             Category = "unknown"
)
