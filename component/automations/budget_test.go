package automations

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// newTestBudget builds an automation budget with a controllable clock so the
// window behavior is deterministic.
func newTestBudget(globalMax, perAutoMax int, window time.Duration, clock *time.Time) *automationBudget {
	return &automationBudget{
		enabled:    true,
		globalMax:  globalMax,
		perAutoMax: perAutoMax,
		window:     window,
		perAuto:    map[string]*windowCount{},
		now:        func() time.Time { return *clock },
	}
}

func TestAutomationBudget_GlobalCeiling_AdmitsNThenBlocks(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(5, 0, time.Minute, &now) // global 5, per-auto unlimited
	// 5 admitted across (possibly different) automations.
	for i := 0; i < 5; i++ {
		if ok, _ := b.admit("autoA"); !ok {
			t.Fatalf("execution %d should be admitted (<= global 5)", i)
		}
	}
	// The 6th -- even for a brand-new automation -- is blocked by the global cap.
	if ok, reason := b.admit("autoB"); ok || reason == "" {
		t.Fatalf("6th execution must be globally blocked with an alert reason (ok=%v reason=%q)", ok, reason)
	}
}

func TestAutomationBudget_PerAutomationCeiling_Isolated(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(0, 3, time.Minute, &now) // global unlimited, per-auto 3
	for i := 0; i < 3; i++ {
		if ok, _ := b.admit("storm"); !ok {
			t.Fatalf("storm execution %d should be admitted (<= per-auto 3)", i)
		}
	}
	// 4th of the SAME automation is blocked.
	if ok, _ := b.admit("storm"); ok {
		t.Fatalf("4th execution of 'storm' must be blocked by the per-automation cap")
	}
	// A DIFFERENT automation is unaffected -- the cap is per-automation.
	if ok, _ := b.admit("calm"); !ok {
		t.Fatalf("a different automation must not be throttled by another's storm")
	}
}

func TestAutomationBudget_WindowRolls_ReadmitsAfterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(2, 0, 60*time.Second, &now)
	b.admit("a")
	b.admit("a")
	if ok, _ := b.admit("a"); ok {
		t.Fatalf("3rd within the window must be blocked")
	}
	now = now.Add(61 * time.Second) // roll the window
	if ok, _ := b.admit("a"); !ok {
		t.Fatalf("after the window rolls the budget must admit again")
	}
}

func TestAutomationBudget_Disabled_NeverBlocks(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(1, 1, time.Minute, &now)
	b.enabled = false
	for i := 0; i < 1000; i++ {
		if ok, _ := b.admit("x"); !ok {
			t.Fatalf("disabled budget must never block (call %d)", i)
		}
	}
}

func TestAutomationBudget_AlertReason_ThrottledOncePerWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBudget(1, 0, 60*time.Second, &now)
	b.admit("a") // fills the global cap of 1
	// First block in the window carries the alert reason...
	if ok, reason := b.admit("a"); ok || reason == "" {
		t.Fatalf("first block must carry an alert reason")
	}
	// ...subsequent blocks in the SAME window are still blocked but silent.
	for i := 0; i < 10; i++ {
		if ok, reason := b.admit("a"); ok || reason != "" {
			t.Fatalf("subsequent blocks in-window must stay blocked AND silent (ok=%v reason=%q)", ok, reason)
		}
	}
	// After the window rolls, a fresh block alerts again.
	now = now.Add(61 * time.Second)
	b.admit("a") // refills the cap
	if ok, reason := b.admit("a"); ok || reason == "" {
		t.Fatalf("after the window rolls, a new block must alert again")
	}
}

// --- cluster-guard fail-open bound (memql#1142) --------------------------

// TestClusterGuard_FailOpenBounded: with no DB the guard fails OPEN, but only
// up to the unguarded budget; past it, it fails CLOSED (skips) so a DB outage
// cannot become an unbounded double-fire multiplier.
func TestClusterGuard_FailOpenBounded(t *testing.T) {
	g := NewClusterExecutionGuard(func() *bun.DB { return nil }, nil)
	clock := time.Unix(2000, 0)
	g.now = func() time.Time { return clock }
	g.unguardedMax = 3
	g.unguardedWindow = 60 * time.Second

	// First 3 unguarded executions are admitted (fail-open).
	for i := 0; i < 3; i++ {
		if !g.Claim(context.Background(), "stormAutomation", "key") {
			t.Fatalf("unguarded execution %d should fail OPEN (<= budget 3)", i)
		}
	}
	// The 4th fails CLOSED -- the fail-open path is now bounded.
	if g.Claim(context.Background(), "stormAutomation", "key") {
		t.Fatalf("4th unguarded execution must fail CLOSED once the budget is exhausted")
	}
	// After the window rolls, fail-open resumes.
	clock = clock.Add(61 * time.Second)
	if !g.Claim(context.Background(), "stormAutomation", "key") {
		t.Fatalf("after the window rolls, the fail-open budget must replenish")
	}
}

// A cap of 0 preserves the pre-#1142 pure fail-open behavior (unlimited).
func TestClusterGuard_FailOpenUnlimitedWhenCapZero(t *testing.T) {
	g := NewClusterExecutionGuard(func() *bun.DB { return nil }, nil)
	g.unguardedMax = 0
	for i := 0; i < 500; i++ {
		if !g.Claim(context.Background(), "a", "k") {
			t.Fatalf("with cap 0 the fail-open path must be unlimited (call %d)", i)
		}
	}
}
