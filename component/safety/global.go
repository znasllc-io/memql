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

// EnforceDecision maps a Gate.Evaluate result -- (Decision,
// Classification, error) -- into a (proceed, refusalReason) outcome
// each surface acts on:
//
//   - proceed=true, refusalReason="": surface dispatches normally.
//   - proceed=false: surface emits its audit event for the block
//     and returns a structured refusal to the caller.
//
// The Gate itself stays neutral on classifier error so per-surface
// fail-open vs fail-closed is the caller's call -- pass
// failClosedOnError=true for high-blast-radius surfaces
// (computer-use) and false for bounded ones (workbench / tool exec).
//
// In Phase 1 the gate's decide() always returns Allow, so the only
// way this currently returns blocked is the fail-closed-on-error
// path. #231 replaces decide() with the real commandRiskDecision
// policy and the Deny/Ask returns start firing. The DecisionAsk
// path here treats Ask as a deny with a "pending #232" reason --
// approval is wired by #232 and replaces this branch then.
func EnforceDecision(decision Decision, cls Classification, err error, failClosedOnError bool) (proceed bool, refusalReason string) {
	if err != nil {
		if failClosedOnError {
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
		// #232 wires the approval surface; until then surface Ask
		// as a refusal so the agent loop returns a structured "I
		// need approval first" rather than parking the call. The
		// reason text + RuleID still land in audit via the gate's
		// recorder so we can measure how often Ask would fire.
		if cls.Reason != "" {
			return false, "requires user approval (not yet implemented; pending #232): " + cls.Reason
		}
		return false, "requires user approval (not yet implemented; pending #232)"
	default:
		return true, ""
	}
}
