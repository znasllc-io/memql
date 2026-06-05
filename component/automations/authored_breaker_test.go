package automations_test

// authored_breaker_test.go -- coverage for the per-automation circuit breaker
// (epic memql#954, issue #959, increment 3).
//
// Two layers under test:
//   - AuthoredBreaker in isolation: per-(owner, automation) consecutive-failure
//     tripping, success-reset, fault isolation across keys, re-activation reset.
//   - The breaker wired into AuthoredScheduler: a deliberately-faulting authored
//     automation trips its breaker, auto-pauses (stops firing), and does NOT
//     affect a healthy automation owned by the same OR a different owner -- the
//     no-cascade acceptance criterion.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// --- breaker in isolation ---

// TestBreaker_TripsAfterThreshold: consecutive failures trip the breaker at the
// threshold and not before.
func TestBreaker_TripsAfterThreshold(t *testing.T) {
	b := automations.NewAuthoredBreaker(3, nil)
	if !b.Allow("u", "a") {
		t.Fatal("a fresh breaker must allow")
	}
	if b.RecordFailure("u", "a", "boom") || b.RecordFailure("u", "a", "boom") {
		t.Fatal("breaker tripped before threshold")
	}
	if b.IsOpen("u", "a") {
		t.Fatal("breaker open before threshold")
	}
	// Third failure trips it.
	if !b.RecordFailure("u", "a", "boom") {
		t.Fatal("third failure should trip the breaker")
	}
	if !b.IsOpen("u", "a") || b.Allow("u", "a") {
		t.Fatal("tripped breaker must be open and disallow")
	}
	// A later failure does not re-report a trip.
	if b.RecordFailure("u", "a", "boom") {
		t.Error("an already-open breaker must not re-trip")
	}
}

// TestBreaker_SuccessResets: a success clears a partial failure streak.
func TestBreaker_SuccessResets(t *testing.T) {
	b := automations.NewAuthoredBreaker(3, nil)
	b.RecordFailure("u", "a", "x")
	b.RecordFailure("u", "a", "x")
	b.RecordSuccess("u", "a") // streak cleared
	if b.RecordFailure("u", "a", "x") {
		t.Fatal("success should have reset the streak; one failure must not trip")
	}
}

// TestBreaker_FaultIsolation: failures on one key never trip another key
// (different automation, or different owner).
func TestBreaker_FaultIsolation(t *testing.T) {
	b := automations.NewAuthoredBreaker(2, nil)
	b.RecordFailure("u", "bad", "x")
	b.RecordFailure("u", "bad", "x") // trips "bad"
	if !b.IsOpen("u", "bad") {
		t.Fatal("bad should be open")
	}
	if b.IsOpen("u", "good") {
		t.Error("a different automation must be unaffected")
	}
	if b.IsOpen("other", "bad") {
		t.Error("the same automation name under a different owner must be unaffected")
	}
}

// TestBreaker_ResetReopens: Reset clears a tripped breaker (re-activation path).
func TestBreaker_ResetReopens(t *testing.T) {
	b := automations.NewAuthoredBreaker(1, nil)
	b.RecordFailure("u", "a", "x") // trips immediately at threshold 1
	if !b.IsOpen("u", "a") {
		t.Fatal("should be open")
	}
	b.Reset("u", "a")
	if b.IsOpen("u", "a") || !b.Allow("u", "a") {
		t.Fatal("Reset must clear the trip")
	}
}

// --- breaker wired into the scheduler ---

// faultRunner runs each authored automation. A run faults when its
// (owner, name) is marked faulting -- owner-scoped, read from the author
// envelope on ctx -- so the test can fault ONE owner's automation while an
// identically-named one under another owner stays healthy. Records
// per-(owner, name) run counts.
type faultRunner struct {
	mu       sync.Mutex
	faulting map[string]bool // keyed by owner+"/"+name
	runs     map[string]int  // keyed by owner+"/"+name
}

func newFaultRunner(faultingKeys ...string) *faultRunner {
	fr := &faultRunner{faulting: map[string]bool{}, runs: map[string]int{}}
	for _, k := range faultingKeys {
		fr.faulting[k] = true
	}
	return fr
}

func (fr *faultRunner) run(ctx context.Context, a *automations.Automation, _ *events.Event) error {
	owner := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		owner = ac.UserId
	}
	key := owner + "/" + a.Name
	fr.mu.Lock()
	fr.runs[key]++
	faulting := fr.faulting[key]
	fr.mu.Unlock()
	if faulting {
		return fmt.Errorf("deliberate fault in %s", key)
	}
	return nil
}

func (fr *faultRunner) count(owner, name string) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.runs[owner+"/"+name]
}

func eventAutomationSrc(name string) string {
	return `@enabled
@trigger(event="node.created", concept="v1:identity:user")
automation ` + name + ` {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`
}

// TestBreaker_FaultingAutomationTripsWithoutCascade is the increment-3 north
// star: a deliberately-faulting authored automation trips its breaker and
// auto-pauses (stops firing) WITHOUT affecting a healthy automation -- same
// owner or different owner -- or the rest of the runtime.
func TestBreaker_FaultingAutomationTripsWithoutCascade(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	// Only user-a's badAuto faults; user-b's same-named automation is healthy.
	runner := newFaultRunner("user-a/badAuto")

	// Hold the scheduler so the breaker's onTrip can deactivate the offender.
	var sched *automations.AuthoredScheduler
	breaker := automations.NewAuthoredBreaker(3, func(info automations.BreakerTripInfo) {
		// Auto-pause ONLY the offending automation.
		sched.Deactivate(info.Owner, info.Automation)
	})

	loader := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()})
	s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   loader,
		EventBus: bus,
		Run:      runner.run,
		Breaker:  breaker,
	})
	if err != nil {
		t.Fatalf("NewAuthoredScheduler: %v", err)
	}
	sched = s
	defer s.Stop()

	// Owner-a has a FAULTING automation + a HEALTHY one; owner-b has a healthy
	// one of the same name as the faulting one (to prove owner-scoped isolation).
	if err := s.Activate(authoredAutomationConstruct("user-a", "badAuto", eventAutomationSrc("badAuto"))); err != nil {
		t.Fatalf("activate badAuto: %v", err)
	}
	if err := s.Activate(authoredAutomationConstruct("user-a", "goodAuto", eventAutomationSrc("goodAuto"))); err != nil {
		t.Fatalf("activate goodAuto: %v", err)
	}
	if err := s.Activate(authoredAutomationConstruct("user-b", "badAuto", eventAutomationSrc("badAuto"))); err != nil {
		t.Fatalf("activate user-b badAuto: %v", err)
	}

	// Fire the shared trigger enough times to trip user-a's badAuto breaker
	// (threshold 3). Each PublishSync delivers to all three subscriptions.
	for i := 0; i < 5; i++ {
		bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "x"}))
	}

	// user-a's badAuto tripped + auto-paused: it ran exactly threshold (3) times
	// then stopped (the 4th/5th events found it deactivated).
	if !breaker.IsOpen("user-a", "badAuto") {
		t.Error("user-a badAuto breaker should be open")
	}
	if s.IsActive("user-a", "badAuto") {
		t.Error("user-a badAuto should be auto-paused after tripping")
	}
	// user-a's badAuto ran exactly the threshold (3) times, then auto-paused so
	// the 4th + 5th events found it deactivated.
	if got := runner.count("user-a", "badAuto"); got != 3 {
		t.Errorf("user-a badAuto should have run exactly 3 times (threshold) before auto-pausing, got %d", got)
	}

	// NO CASCADE: user-a's healthy automation kept firing on every event (5x),
	// and user-b's same-named automation never tripped (still active, ran 5x).
	if got := runner.count("user-a", "goodAuto"); got != 5 {
		t.Errorf("healthy goodAuto must keep firing on every event (no cascade); got %d, want 5", got)
	}
	if breaker.IsOpen("user-b", "badAuto") || !s.IsActive("user-b", "badAuto") {
		t.Error("user-b badAuto (healthy) must be unaffected by user-a's trip")
	}
	if got := runner.count("user-b", "badAuto"); got != 5 {
		t.Errorf("user-b's healthy automation must keep firing on every event; got %d, want 5", got)
	}

	// Re-activation gives the offender a fresh start (breaker reset, fires again).
	runner.mu.Lock()
	delete(runner.faulting, "user-a/badAuto") // pretend the author fixed it
	runner.mu.Unlock()
	if err := s.Activate(authoredAutomationConstruct("user-a", "badAuto", eventAutomationSrc("badAuto"))); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if breaker.IsOpen("user-a", "badAuto") {
		t.Error("re-activation must reset the breaker")
	}
	before := runner.count("user-a", "badAuto")
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "y"}))
	if runner.count("user-a", "badAuto") <= before {
		t.Error("re-activated automation must fire again")
	}
}
