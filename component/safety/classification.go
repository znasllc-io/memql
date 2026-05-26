package safety

import "time"

// Source records which layer produced a Classification. Lets audit +
// observability distinguish a rule-engine block ("a deny rule fired,
// here's the rule id") from a model verdict ("the LLM judged it
// risky") from a cache hit. Disabled means the classifier was off
// (mode=off) so no verdict was actually computed.
type Source string

const (
	SourceRule     Source = "rule"
	SourceModel    Source = "model"
	SourceCache    Source = "cache"
	SourceNoop     Source = "noop"
	SourceDisabled Source = "disabled"
)

// Classification is what a Classifier returns for an
// ActionDescriptor. Carries the tier + categories the decision
// policy keys off, the reason/explanation that lands in audit + the
// "why was I asked to approve this?" approval UI, and the
// provenance (source / rule id / latency / confidence) that
// observability needs to tune the system.
type Classification struct {
	Tier       RiskTier
	Categories []Category
	// Reason is a short human-readable explanation -- which specific
	// pattern fired, what the model flagged. Lands in audit + the
	// approval surface; keep it short and quotable.
	Reason string
	// RuleID identifies the rule (or model prompt) that produced
	// this verdict, e.g. `shell.destructive` or `model.classify_v1`.
	// Empty when SourceNoop / SourceDisabled. Stable + greppable --
	// used by audit and the red-team corpus to track per-rule
	// behaviour over time.
	RuleID string
	// Confidence is the classifier's self-reported confidence in
	// [0.0, 1.0]. Rule verdicts are 1.0 by convention; model
	// verdicts carry the model's own estimate. The decision policy
	// uses it to treat low-confidence high-tier conservatively.
	Confidence float64
	Source     Source
	LatencyMs  int64
}

// Decision is the gate's verdict for the dispatch path: execute,
// surface an approval request, or refuse. Maps 1:1 onto how the
// caller treats the result -- there is no fourth state.
type Decision string

const (
	// DecisionAllow proceeds with normal execution.
	DecisionAllow Decision = "allow"
	// DecisionAsk parks the action and surfaces an approval request
	// (#232). The caller treats this as "pending" -- not an error,
	// not an execution.
	DecisionAsk Decision = "ask"
	// DecisionDeny refuses the action. The caller emits the
	// structured refusal back to the agent loop + an audit event.
	DecisionDeny Decision = "deny"
)

// since returns the milliseconds elapsed since `start`, capped at the
// max int64 -- defensive for very long-running classifiers, but in
// practice classifications are sub-second.
func since(start time.Time) int64 {
	d := time.Since(start)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}
