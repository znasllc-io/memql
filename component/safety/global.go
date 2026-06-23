package safety

import (
	"sync"
	"sync/atomic"
)

// global.go is the dispatch-side surface other packages use to reach
// the Gate without each one wiring its own. There is one
// process-wide Gate, lazily constructed with the default rule
// classifier + slog recorder + mode-from-env. App boot and tests
// override via SetDefaultGate.

var (
	defaultGate   atomic.Pointer[Gate]
	defaultGateMu sync.Mutex
)

// DefaultGate returns the process-wide command-classifier Gate.
// First call lazily constructs one with:
//
//   - Classifier: ChainClassifier(RuleClassifier(DefaultRules()...),
//     NoopClassifier{}). The rule layer (#228) handles the
//     deterministic cases; the noop is the placeholder for the LLM
//     classifier (#230) that app boot can swap in via SetDefaultGate.
//   - Recorder: SlogRecorder against slog.Default().
//   - Mode: ModeFromEnv (defaults to ModeShadow).
//
// Concurrency-safe via double-checked-locking on an atomic pointer:
// reads are lock-free on the hot dispatch path, the mutex only
// serializes the one-shot initialisation. SetDefaultGate(nil) is the
// supported "reset to lazy" hook used by tests.
func DefaultGate() *Gate {
	if g := defaultGate.Load(); g != nil {
		return g
	}
	defaultGateMu.Lock()
	defer defaultGateMu.Unlock()
	if g := defaultGate.Load(); g != nil {
		return g
	}
	g := NewGate(GateOptions{
		Classifier: NewChainClassifier(
			NewRuleClassifier(DefaultRules()...),
			NoopClassifier{},
		),
	})
	defaultGate.Store(g)
	return g
}

// SetDefaultGate replaces the process-wide Gate. Intended for:
//
//   - App boot (`app/`) injecting a fully-configured gate that
//     appends `NewWebhookHostAllowlistRule(cfg.WebhookAllowedHosts)`
//     to the rule list and wires the LLM classifier (#230).
//   - Tests that want a deterministic verdict (a stub gate that
//     always denies, etc.) without poking env vars.
//
// A subsequent DefaultGate call returns the gate set here; the
// lazy-init path is bypassed for the rest of the process lifetime.
func SetDefaultGate(g *Gate) {
	defaultGate.Store(g)
}

// EnforceDecision maps the Gate's Evaluate result -- (Decision,
// Classification, error) -- into a (proceed, refusalReason) outcome
// each surface acts on:
//
//   - proceed=true, refusalReason="": surface dispatches normally.
//   - proceed=false: surface emits its audit event for the block
//     and returns a structured refusal to the caller.
//
// Per-surface fail-open vs fail-closed is the caller's call -- pass
// failClosedOnError=true for high-blast-radius surfaces
// (computer-use) and false for bounded ones (workbench / tool exec).
// **In ModeShadow the fail-closed-on-error path is suppressed** so
// the rollout-safety invariant holds: shadow mode is observation-
// only end-to-end (decisions AND classifier failures get logged
// but never block live traffic). The recorder still captures the
// error so ops see it.
//
// In Phase 1 the gate's decide() always returns Allow; #231 swapped
// in the real commandRiskDecision matrix so Deny/Ask returns fire;
// #232 plugged the ApprovalSink into the Gate's Evaluate path so the
// Ask branch here only fires when the sink couldn't approve; #235
// added the surface param so the mode lookup honours per-surface
// env overrides (MEMQL_SAFETY_<SURFACE>_MODE) -- without it,
// EnforceDecision would consult the construction-time g.mode while
// Evaluate was already running in per-surface enforce, breaking
// the fail-closed posture.
func (g *Gate) EnforceDecision(surface Surface, decision Decision, cls Classification, err error, failClosedOnError bool) (proceed bool, refusalReason string) {
	mode := resolveModeForSurface(surface, g.mode)
	if err != nil {
		// Fail-closed only applies in enforce mode -- shadow and off
		// are observation-only by design (the rollout-safety
		// invariant) so a classifier error gets logged + swallowed
		// rather than blocking live traffic.
		if failClosedOnError && mode == ModeEnforce {
			return false, "command classifier error (failing closed): " + err.Error()
		}
		return true, ""
	}
	switch decision {
	case DecisionDeny:
		if cls.Reason != "" {
			return false, "blocked by safety classifier: " + cls.Reason
		}
		return false, "blocked by safety classifier"
	case DecisionAsk:
		// Gate's Evaluate already consulted the ApprovalSink in
		// enforce mode (#232); we only land here when the sink
		// returned Pending or Unconfigured. cls.Reason carries the
		// approvalRequest id when one was created -- callers /
		// approvers can resolve via resolveApprovalRequest.
		if cls.Reason != "" {
			return false, "requires user approval: " + cls.Reason
		}
		return false, "requires user approval"
	default:
		return true, ""
	}
}
