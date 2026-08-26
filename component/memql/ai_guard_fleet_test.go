package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// newTestFleetGuard builds a guard with every layer wired to a controllable
// clock, so the four checks can be driven over a stubbed fleet provider.
func newTestFleetGuard(clock *time.Time) *llmGuard {
	return &llmGuard{
		enabled:     true,
		maxRepeat:   2,
		window:      10 * time.Second,
		cooldown:    30 * time.Second,
		rateEnabled: true,
		rateMax:     5,
		rateWindow:  10 * time.Second,
		hits:        map[string][]time.Time{},
		tripped:     map[string]time.Time{},
		scopes:      map[string]*scopeBudget{},
		now:         func() time.Time { return *clock },
		logger:      testGuardLogger(),
	}
}

func fleetReq(prompt string) FleetCallRequest {
	return FleetCallRequest{
		ModelId:  "llama3.1:8b",
		Kind:     FleetKindChat,
		Messages: []common.ChatMessage{{Role: "user", Content: prompt}},
	}
}

// A runaway loop on a free model is still a runaway loop. The identical call,
// repeated, must be stopped by the SAME breaker an identical vendor call hits.
func TestTheLoopBreakerLatchesOnRepeatedFleetCalls(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	g := newTestFleetGuard(&now)
	fp := FleetCallFingerprint(fleetReq("same prompt"))

	for i := 1; i <= 2; i++ {
		if err := g.admitLocalCall(context.Background(), fp); err != nil {
			t.Fatalf("call %d should be admitted: %v", i, err)
		}
	}
	err := g.admitLocalCall(context.Background(), fp)
	if err == nil {
		t.Fatal("the third identical local call must be blocked -- the breaker is the cheap " +
			"catch for a loop that repeats itself byte for byte")
	}
	if got := guardLayerOf(err); got != GuardLayerLoopBreaker {
		t.Fatalf("blocked by %q, want the loop breaker", got)
	}
	if !errors.Is(err, ErrLLMGuardBlocked) {
		t.Fatalf("err = %v, want the guard sentinel", err)
	}
	if IsGuardLatched(err) {
		t.Fatal("the loop breaker self-heals; reporting it as latched would tell a plan not to " +
			"resume from something that drains on its own")
	}
}

// A loop that VARIES its prompt fingerprints differently every time and sails
// past the breaker -- which is exactly what the rate ceiling is for.
func TestTheRateCeilingCatchesAFleetLoopThatVariesItsPrompt(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	g := newTestFleetGuard(&now)

	for i := 1; i <= 5; i++ {
		fp := FleetCallFingerprint(fleetReq(strings.Repeat("x", i)))
		if err := g.admitLocalCall(context.Background(), fp); err != nil {
			t.Fatalf("call %d should be admitted: %v", i, err)
		}
	}
	err := g.admitLocalCall(context.Background(), FleetCallFingerprint(fleetReq("novel again")))
	if err == nil {
		t.Fatal("a varying-prompt local loop must not outrun the rate ceiling")
	}
	if got := guardLayerOf(err); got != GuardLayerRateCeiling {
		t.Fatalf("blocked by %q, want the rate ceiling", got)
	}
}

// The kill-switch is the layer that does NOT drain, and a fleet call must be
// stopped by it exactly as a vendor call is.
func TestAlreadyLatchedKillSwitchStopsFleetCalls(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	g := newTestFleetGuard(&now)
	g.killSwitchEnabled = true
	g.latched = true
	g.latchReason = "cumulative LLM call ceiling reached"

	err := g.admitLocalCall(context.Background(), FleetCallFingerprint(fleetReq("anything")))
	if err == nil {
		t.Fatal("a latched kill-switch must stop a local call too")
	}
	if got := guardLayerOf(err); got != GuardLayerKillSwitch {
		t.Fatalf("blocked by %q, want the kill-switch", got)
	}
	if !IsGuardLatched(err) {
		t.Fatal("the kill-switch does not drain, and a caller has to be able to tell that from " +
			"a layer that does")
	}
}

// THE ASYMMETRY THAT MAKES LOCAL MODELS HONEST: the CALL tally counts a fleet
// call, and the DOLLAR tally does not. Charging money nobody was billed would
// park work over spend that never happened.
func TestFleetCallsCountAgainstCallsButNotDollars(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	g := newTestFleetGuard(&now)
	g.killSwitchEnabled = true
	g.maxTotalCalls = 3
	g.maxTotalCostUSD = 100

	for i := 1; i <= 3; i++ {
		if err := g.admitLocalCall(context.Background(), FleetCallFingerprint(fleetReq(strings.Repeat("y", i)))); err != nil {
			t.Fatalf("call %d should be admitted: %v", i, err)
		}
	}
	if g.totalCalls != 3 {
		t.Fatalf("totalCalls = %d, want 3 -- a runaway on a free model is still a runaway", g.totalCalls)
	}
	if g.totalCostUSD != 0 {
		t.Fatalf("totalCostUSD = %f, want 0 -- nobody was billed for a local call, and charging "+
			"one parks work over money that was never spent", g.totalCostUSD)
	}
	err := g.admitLocalCall(context.Background(), FleetCallFingerprint(fleetReq("fourth")))
	if err == nil || guardLayerOf(err) != GuardLayerBudget {
		t.Fatalf("the fourth call must hit the cumulative CALL ceiling, got %v", err)
	}
}

// Per-scope budgets apply too: a plan driving fleet calls parks on its call
// budget exactly as it would on metered ones.
func TestPerScopeCallBudgetsCountFleetCalls(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	g := newTestFleetGuard(&now)
	g.scopeEnabled = true
	g.scopeMaxCalls = 2
	ctx := ContextWithBudgetScope(context.Background(), "plan:p1")

	for i := 1; i <= 2; i++ {
		if err := g.admitLocalCall(ctx, FleetCallFingerprint(fleetReq(strings.Repeat("z", i)))); err != nil {
			t.Fatalf("call %d should be admitted: %v", i, err)
		}
	}
	if err := g.admitLocalCall(ctx, FleetCallFingerprint(fleetReq("third"))); err == nil {
		t.Fatal("a plan's cumulative call budget must count local calls")
	}
	// A different plan is unaffected -- the scopes are independent.
	other := ContextWithBudgetScope(context.Background(), "plan:p2")
	if err := g.admitLocalCall(other, FleetCallFingerprint(fleetReq("elsewhere"))); err != nil {
		t.Fatalf("another plan's budget must be independent: %v", err)
	}
}

// The fingerprint keys on what makes two calls the SAME call. A plan id or a
// purpose varies across a genuine loop, and folding one in would make every
// repetition look novel -- defeating the cheap catch entirely.
func TestTheFingerprintIgnoresAttributionAndKeysOnThePrompt(t *testing.T) {
	a := fleetReq("hello")
	a.PlanId, a.TaskId, a.Purpose = "plan-1", "task-1", "planner"
	b := fleetReq("hello")
	b.PlanId, b.TaskId, b.Purpose = "plan-2", "task-9", "conductor"
	if FleetCallFingerprint(a) != FleetCallFingerprint(b) {
		t.Fatal("two identical prompts must fingerprint the same; folding attribution in would " +
			"make every repetition of a loop look novel")
	}

	c := fleetReq("hello there")
	if FleetCallFingerprint(a) == FleetCallFingerprint(c) {
		t.Fatal("different prompts must fingerprint differently, or legitimate traffic throttles")
	}

	d := fleetReq("hello")
	d.ModelId = "qwen2.5:7b"
	if FleetCallFingerprint(a) == FleetCallFingerprint(d) {
		t.Fatal("the same prompt on a different model is a different call")
	}

	e := fleetReq("hello")
	e.Schema = &common.StructuredSchema{Name: "s", Schema: []byte(`{"type":"object"}`)}
	if FleetCallFingerprint(a) == FleetCallFingerprint(e) {
		t.Fatal("a structured call is not the same call as a prose one")
	}
}

// A guard with every layer switched off admits everything, so a deployment
// that has deliberately disabled the breaker is not broken by this path.
func TestAFullyDisabledGuardAdmitsFleetCalls(t *testing.T) {
	g := &llmGuard{now: time.Now, logger: testGuardLogger()}
	if err := g.admitLocalCall(context.Background(), "fp"); err != nil {
		t.Fatalf("a disabled guard must admit: %v", err)
	}
}

// The guard sits at the provider seam, so a call placed through the registered
// fleet provider passes it -- which is the property that makes "no fleet call
// path bypasses ai_guard.go" true rather than aspirational.
func TestTheFleetProviderItselfPassesTheGuard(t *testing.T) {
	prev := sharedLLMGuard
	t.Cleanup(func() { sharedLLMGuard = prev })

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	sharedLLMGuard = newTestFleetGuard(&now)

	stub := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: "ok"}
	r := newProviderRegistry("")
	r.SetFleetInference(stub)
	entry, _ := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	chat := entry.Client.(common.ChatAIProvider)

	msgs := []common.ChatMessage{{Role: "user", Content: "identical"}}
	for i := 1; i <= 2; i++ {
		if _, err := chat.CallChat(userCtx("alice"), msgs); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := chat.CallChat(userCtx("alice"), msgs); err == nil {
		t.Fatal("the third identical call through the PROVIDER must be blocked -- a fleet call " +
			"has no *http.Client, so guardedTransport never sees it and this seam is the only gate")
	} else if !errors.Is(err, ErrLLMGuardBlocked) {
		t.Fatalf("err = %v, want the guard sentinel", err)
	}
}
