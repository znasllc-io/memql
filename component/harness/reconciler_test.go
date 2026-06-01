package harness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeGraph is an in-memory model of the harness graph that satisfies
// StepReader, StepClaimer, and Writer. It enforces the same latest-per-id
// + claim-once semantics the real engine guard provides, so the
// controller's logic is exercised end to end without a database.
type fakeGraph struct {
	mu           sync.Mutex
	steps        map[string]StepView
	planStatus   string
	planView     PlanView
	observations []Observation
	claimCalls   int32
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		steps:      map[string]StepView{},
		planStatus: PlanStatusOpen,
		planView:   PlanView{ID: "plan-1", OwnerUserId: "u1", Goal: "g", Status: PlanStatusOpen},
	}
}

func (g *fakeGraph) addStep(s StepView) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.steps[s.ID] = s
}

// --- StepReader ---

func (g *fakeGraph) StepsForPlan(_ context.Context, planID string) ([]StepView, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]StepView, 0, len(g.steps))
	for _, s := range g.steps {
		if s.PlanID == planID {
			out = append(out, s)
		}
	}
	return out, "default", nil
}

func (g *fakeGraph) PlanStatus(_ context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.planStatus, nil
}

func (g *fakeGraph) PlanView(_ context.Context, _ string) (PlanView, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := g.planView
	v.Status = g.planStatus
	return v, true, nil
}

// --- StepClaimer (atomic ready->running, claim-once) ---

func (g *fakeGraph) ClaimStep(_ context.Context, _ string, stepID string, _ int) (bool, error) {
	atomic.AddInt32(&g.claimCalls, 1)
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.steps[stepID]
	if !ok {
		return false, fmt.Errorf("unknown step %q", stepID)
	}
	if s.Status != StepStatusReady {
		// Lost the race / invalid transition -- mirrors the engine guard.
		return false, nil
	}
	s.Status = StepStatusRunning
	s.Attempt++
	g.steps[stepID] = s
	return true, nil
}

// --- Writer ---

func (g *fakeGraph) MarkStepReady(_ context.Context, step StepView) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[step.ID]
	s.Status = StepStatusReady
	g.steps[step.ID] = s
	return nil
}

func (g *fakeGraph) StartStep(_ context.Context, _ string) error { return nil }

func (g *fakeGraph) CompleteStep(_ context.Context, stepID string, _ map[string]any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[stepID]
	s.Status = StepStatusDone
	g.steps[stepID] = s
	return nil
}

func (g *fakeGraph) FailStep(_ context.Context, stepID, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[stepID]
	s.Status = StepStatusFailed
	g.steps[stepID] = s
	return nil
}

func (g *fakeGraph) RecordObservation(_ context.Context, obs Observation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observations = append(g.observations, obs)
	return nil
}

func (g *fakeGraph) SetPlanStatus(_ context.Context, _ PlanView, status, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.planStatus = status
	return nil
}

// countDispatcher counts dispatches per step + total, succeeding always.
type countDispatcher struct {
	mu       sync.Mutex
	perStep  map[string]int
	total    int32
	failFor  map[string]bool
	toolCall int
}

func newCountDispatcher() *countDispatcher {
	return &countDispatcher{perStep: map[string]int{}, failFor: map[string]bool{}, toolCall: 1}
}

func (d *countDispatcher) Dispatch(_ context.Context, step StepView) (map[string]any, int, error) {
	atomic.AddInt32(&d.total, 1)
	d.mu.Lock()
	d.perStep[step.ID]++
	fail := d.failFor[step.ID]
	d.mu.Unlock()
	if fail {
		return nil, d.toolCall, fmt.Errorf("synthetic failure for %s", step.ID)
	}
	return map[string]any{"ok": step.ID}, d.toolCall, nil
}

func newReconcilerFor(g *fakeGraph, d Dispatcher, cfg Config) *Reconciler {
	r, err := New(g, g, d, g, nil, nil, cfg)
	if err != nil {
		panic(err)
	}
	return r
}

// reconcileToFixpoint runs Reconcile repeatedly until the plan settles or
// the cap is hit -- this is the event-sourced loop: each observation write
// would re-fire a tick; here we drive the ticks directly.
func reconcileToFixpoint(t *testing.T, r *Reconciler, g *fakeGraph, planID string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if err := r.Reconcile(context.Background(), planID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		st, _ := g.PlanStatus(context.Background(), planID)
		if isTerminalPlanStatus(st) {
			return
		}
	}
}

// AC: a 3-step DAG runs to completion with observations recorded.
func TestReconcile_ThreeStepDAGCompletes(t *testing.T) {
	g := newFakeGraph()
	// a -> b -> c (linear DAG). a starts ready; b,c pending on deps.
	g.addStep(StepView{ID: "a", PlanID: "plan-1", Status: StepStatusReady})
	g.addStep(StepView{ID: "b", PlanID: "plan-1", Status: StepStatusPending, DependsOn: []string{"a"}})
	g.addStep(StepView{ID: "c", PlanID: "plan-1", Status: StepStatusPending, DependsOn: []string{"b"}})

	d := newCountDispatcher()
	r := newReconcilerFor(g, d, Config{})
	reconcileToFixpoint(t, r, g, "plan-1")

	for _, id := range []string{"a", "b", "c"} {
		if got := g.steps[id].Status; got != StepStatusDone {
			t.Fatalf("step %s not done: %s", id, got)
		}
	}
	if st, _ := g.PlanStatus(context.Background(), "plan-1"); st != PlanStatusDone {
		t.Fatalf("plan not done: %s", st)
	}
	// One tool_result observation per step (at minimum).
	if len(g.observations) < 3 {
		t.Fatalf("expected >=3 observations, got %d", len(g.observations))
	}
}

// AC: concurrent observation events do not double-execute a step.
func TestReconcile_ClaimPreventsDoubleExec(t *testing.T) {
	g := newFakeGraph()
	g.addStep(StepView{ID: "solo", PlanID: "plan-1", Status: StepStatusReady})
	d := newCountDispatcher()
	r := newReconcilerFor(g, d, Config{})

	// Fire many concurrent reconciles for the same plan -- the per-plan
	// lock serializes them, and claim-once guarantees a single dispatch.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Reconcile(context.Background(), "plan-1")
		}()
	}
	wg.Wait()

	if got := d.perStep["solo"]; got != 1 {
		t.Fatalf("step dispatched %d times, want exactly 1", got)
	}
	if g.steps["solo"].Status != StepStatusDone {
		t.Fatalf("solo step not done: %s", g.steps["solo"].Status)
	}
}

// AC: budget limits halt a runaway plan and mark it failed with a reason.
func TestReconcile_BudgetHaltsAndFailsPlan(t *testing.T) {
	g := newFakeGraph()
	// Two ready steps but MaxSteps budget of 0-effective via a high
	// already-dispatched count: simulate a runaway by pre-marking many
	// steps as done so StepsDispatched >= MaxSteps on the first tick.
	for i := 0; i < 5; i++ {
		g.addStep(StepView{ID: fmt.Sprintf("done-%d", i), PlanID: "plan-1", Status: StepStatusDone})
	}
	g.addStep(StepView{ID: "next", PlanID: "plan-1", Status: StepStatusReady})

	d := newCountDispatcher()
	r := newReconcilerFor(g, d, Config{Budget: PlanBudget{MaxSteps: 3}})

	if err := r.Reconcile(context.Background(), "plan-1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st, _ := g.PlanStatus(context.Background(), "plan-1"); st != PlanStatusFailed {
		t.Fatalf("plan not failed on budget: %s", st)
	}
	// A decision observation naming the limit must have been recorded.
	found := false
	for _, o := range g.observations {
		if o.Kind == "decision" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a decision observation recording the budget halt")
	}
	// The runaway step must NOT have dispatched (budget tripped first).
	if d.perStep["next"] != 0 {
		t.Fatalf("budget should have halted before dispatch, got %d", d.perStep["next"])
	}
}

// AC: a blocked step becomes ready and runs once its blocker completes.
func TestReconcile_BlockedBecomesReadyAfterBlocker(t *testing.T) {
	g := newFakeGraph()
	g.addStep(StepView{ID: "blocker", PlanID: "plan-1", Status: StepStatusReady})
	g.addStep(StepView{ID: "blocked", PlanID: "plan-1", Status: StepStatusBlocked, DependsOn: []string{"blocker"}})

	d := newCountDispatcher()
	r := newReconcilerFor(g, d, Config{})
	reconcileToFixpoint(t, r, g, "plan-1")

	if g.steps["blocked"].Status != StepStatusDone {
		t.Fatalf("blocked step never ran: %s", g.steps["blocked"].Status)
	}
	if d.perStep["blocked"] != 1 {
		t.Fatalf("blocked step dispatch count = %d, want 1", d.perStep["blocked"])
	}
}

// A failed step marks the plan failed (terminal verdict path).
func TestReconcile_FailedStepFailsPlan(t *testing.T) {
	g := newFakeGraph()
	g.addStep(StepView{ID: "boom", PlanID: "plan-1", Status: StepStatusReady})
	d := newCountDispatcher()
	d.failFor["boom"] = true
	r := newReconcilerFor(g, d, Config{})
	reconcileToFixpoint(t, r, g, "plan-1")

	if g.steps["boom"].Status != StepStatusFailed {
		t.Fatalf("boom not failed: %s", g.steps["boom"].Status)
	}
	if st, _ := g.PlanStatus(context.Background(), "plan-1"); st != PlanStatusFailed {
		t.Fatalf("plan not failed: %s", st)
	}
	// An error observation must have been recorded for the failed step.
	foundErr := false
	for _, o := range g.observations {
		if o.Kind == "error" && o.StepID == "boom" {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatalf("expected error observation for failed step")
	}
}

// AC: killing the process mid-plan and restarting resumes correctly with
// no duplicate side effects. We model a "crash" as discarding the
// reconciler (its in-memory clock) but keeping the graph, then building a
// fresh reconciler and reconciling again. A step already done must not
// re-dispatch.
func TestReconcile_CrashRecoveryNoDoubleExec(t *testing.T) {
	g := newFakeGraph()
	g.addStep(StepView{ID: "a", PlanID: "plan-1", Status: StepStatusReady})
	g.addStep(StepView{ID: "b", PlanID: "plan-1", Status: StepStatusPending, DependsOn: []string{"a"}})

	d := newCountDispatcher()

	// First controller: run a single tick (a runs + b promotes).
	r1 := newReconcilerFor(g, d, Config{})
	if err := r1.Reconcile(context.Background(), "plan-1"); err != nil {
		t.Fatalf("r1 reconcile: %v", err)
	}
	if g.steps["a"].Status != StepStatusDone {
		t.Fatalf("a not done after first tick: %s", g.steps["a"].Status)
	}

	// "Crash": drop r1, keep the graph. Fresh controller resumes.
	r2 := newReconcilerFor(g, d, Config{})
	reconcileToFixpoint(t, r2, g, "plan-1")

	if d.perStep["a"] != 1 {
		t.Fatalf("step a re-executed after restart: %d dispatches", d.perStep["a"])
	}
	if g.steps["b"].Status != StepStatusDone {
		t.Fatalf("b not done after recovery: %s", g.steps["b"].Status)
	}
	if st, _ := g.PlanStatus(context.Background(), "plan-1"); st != PlanStatusDone {
		t.Fatalf("plan not done after recovery: %s", st)
	}
}
