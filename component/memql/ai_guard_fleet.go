package memql

// The guards, applied to LOCAL model calls (epic memql#4676, task memql#4680).
//
// A RUNAWAY LOOP ON A FREE MODEL IS STILL A RUNAWAY LOOP. Read
// docs/public/ai/llm-cost-control.md before touching this file or
// ai_guard.go; every layer described there is load-bearing, and this file
// exists because one assumption underneath all of them stopped being true.
//
// THE ASSUMPTION. `guardedHTTPClient` wraps the *http.Client of all four chat
// provider builds, so "every chat/messages completion leaves the process
// through one guardedTransport" was a complete statement -- the chokepoint was
// path-agnostic precisely because there was exactly one way out of the
// process. A fleet call has no *http.Client at all: it leaves over the
// WorkerService stream, and would have passed no gate whatever.
//
// SO THE CHOKEPOINT MOVES UP, from the transport to the PROVIDER SEAM. This
// file supplies the same four checks in the same order for a caller that has
// no HTTP request: latch, loop breaker, rate ceiling, cumulative accounting.
// It does not thin any layer and it does not duplicate the state -- both paths
// read and write the SAME *llmGuard, which is what stops the two from
// disagreeing about how many calls a runaway has made.
//
// WHAT DIFFERS, AND ONLY THIS: the dollar figure is ZERO. Nobody was billed,
// so charging a local call would park work over money that was never spent.
// The CALL tallies are unchanged -- see recordAndMaybeLatchCost for why the
// two caps want opposite answers.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrLLMGuardBlocked is returned when a local model call is stopped by one of
// the guards.
//
// It is a plain error rather than a synthetic 429, and the difference is
// deliberate: the HTTP guards return a status code because the VENDOR SDKS
// need one to raise their own rate-limit type. Nothing sits between this call
// and its caller, so a status code here would be a costume worn for an
// audience that is not present.
var ErrLLMGuardBlocked = errors.New("blocked by the LLM guard")

// LLMGuardBlock says which layer stopped the call and why. `Latched` marks the
// non-draining kill-switch, which a caller must not retry -- the other two
// self-heal, and a plan parked on them is worth resuming.
type LLMGuardBlock struct {
	Layer   string
	Reason  string
	Latched bool
}

func (b *LLMGuardBlock) Error() string {
	if b == nil {
		return ErrLLMGuardBlocked.Error()
	}
	return fmt.Sprintf("%s: %s (%s)", ErrLLMGuardBlocked.Error(), b.Reason, b.Layer)
}

func (b *LLMGuardBlock) Unwrap() error { return ErrLLMGuardBlocked }

// Guard layer names, as they appear on the block and in logs.
const (
	GuardLayerKillSwitch  = "kill-switch"
	GuardLayerLoopBreaker = "loop-breaker"
	GuardLayerRateCeiling = "rate-ceiling"
	GuardLayerBudget      = "cumulative-budget"
)

// GuardLocalModelCall runs a local model call through the same guards a vendor
// HTTP call passes, in the same order.
//
// `fingerprint` must be derived from what would make two calls IDENTICAL --
// the model and the exact prompt. FleetCallFingerprint builds it; a caller
// that supplies an empty one skips the loop breaker only, because a breaker
// that fingerprints everything to the same key would trip on unrelated traffic
// and a breaker that fingerprints nothing is not a breaker.
func GuardLocalModelCall(ctx context.Context, fingerprint string) error {
	return sharedLLMGuard.admitLocalCall(ctx, fingerprint)
}

// admitLocalCall is GuardLocalModelCall against a specific guard. The split is
// what lets a test drive the four layers over a stubbed fleet provider without
// mutating process-wide state that every other test in the package shares.
func (g *llmGuard) admitLocalCall(ctx context.Context, fingerprint string) error {
	if g == nil {
		return nil
	}
	if !g.enabled && !g.rateEnabled && !g.killSwitchEnabled && !g.scopeEnabled {
		return nil
	}

	// 0. KILL-SWITCH first. If the cumulative breaker has already latched,
	// hard-stop with nothing else attempted -- the process-wide budget is
	// exhausted and it does not drain.
	if reason, dead := g.killed(); dead {
		return &LLMGuardBlock{Layer: GuardLayerKillSwitch, Reason: reason, Latched: true}
	}

	// 1. Per-fingerprint loop breaker. admit() self-gates on g.enabled.
	if fingerprint != "" {
		if open, repeats := g.admit(fingerprint); open {
			return &LLMGuardBlock{
				Layer: GuardLayerLoopBreaker,
				Reason: fmt.Sprintf(
					"the identical local model call was repeated %d times within the loop window; "+
						"blocked to stop a runaway loop. It will be admitted again after the cooldown.",
					repeats),
			}
		}
	}

	// 2. The per-lane rate ceiling. The lane is read off the context exactly
	// as the transport reads it off the request context, so a background
	// executor's local calls count against the background bucket.
	if open, calls := g.admitRate(backgroundLaneFromContext(ctx)); open {
		return &LLMGuardBlock{
			Layer: GuardLayerRateCeiling,
			Reason: fmt.Sprintf(
				"the local model rate ceiling was reached (%d calls in the window); blocked to stop "+
					"a runaway loop. It will be admitted again as the window drains.", calls),
		}
	}

	// 3. Cumulative accounting LAST, for a call that has cleared the cheaper
	// guards and is about to reach a machine -- so the tallies stay honest.
	// COST ZERO: nobody was billed. The CALL still counts.
	if reason, blocked := g.recordAndMaybeLatchCost(budgetScopesFromContext(ctx), 0); blocked {
		return &LLMGuardBlock{Layer: GuardLayerBudget, Reason: reason, Latched: true}
	}
	return nil
}

// FleetCallFingerprint derives the loop-breaker key for a local model call.
//
// It hashes what makes two calls the same call: the model, and the exact
// conversation. Not the plan id, not the purpose, not a timestamp -- those
// vary across a genuine loop and would make every repetition look novel, which
// is precisely the failure the rate ceiling exists to backstop and the breaker
// exists to catch cheaply.
func FleetCallFingerprint(req FleetCallRequest) string {
	h := sha256.New()
	h.Write([]byte(req.ModelId))
	h.Write([]byte{0})
	h.Write([]byte(req.Kind))
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte{1})
		h.Write([]byte(m.Content))
	}
	for _, in := range req.EmbeddingInput {
		h.Write([]byte{2})
		h.Write([]byte(in))
	}
	if req.Schema != nil {
		h.Write([]byte{3})
		h.Write([]byte(req.Schema.Name))
		h.Write(req.Schema.Schema)
	}
	return "fleet:" + hex.EncodeToString(h.Sum(nil))
}

// IsGuardLatched reports whether an error is the non-draining kill-switch, so
// a caller can tell "wait and it will work" from "this process is done making
// model calls".
func IsGuardLatched(err error) bool {
	var block *LLMGuardBlock
	if errors.As(err, &block) {
		return block.Latched
	}
	return false
}

// guardLayerOf reports which layer blocked a call, for tests and logs.
func guardLayerOf(err error) string {
	var block *LLMGuardBlock
	if errors.As(err, &block) {
		return block.Layer
	}
	return ""
}
