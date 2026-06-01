package harness

// replay_graph.go is the in-memory harness graph used by replay (and the
// eval harness) to re-drive the reconciler WITHOUT a database, event bus,
// or LLM. It enforces the same latest-per-id + claim-once + state-machine
// semantics the real engine guard provides, so the reconciler's decision
// core runs exactly as it would in production -- only the storage and the
// dispatch are faked.
//
// The dispatcher is a deterministic no-op that marks every step succeeded
// (plus a configurable per-step result), so replay measures the
// reconciler's ordering, not tool behavior. The eval harness swaps in a
// scripted dispatcher to model task success / failure.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ReplayDispatch is the dispatcher seam for the in-memory graph. It returns
// the step's result + tool-call count + error, modeling tool behavior
// deterministically. The default (nil) marks every step a success.
type ReplayDispatch func(ctx context.Context, step StepView) (result map[string]any, toolCalls int, err error)

// replayGraph is the in-memory StepReader + StepClaimer + Writer +
// Dispatcher used by Replay and the eval harness.
type replayGraph struct {
	mu         sync.Mutex
	steps      map[string]StepView
	stepOrder  []string // first-seen step ids, for stable iteration
	planView   PlanView
	planStatus string
	dispatch   ReplayDispatch

	dispatched []string // step ids in claim order (the dispatch sequence)
	nDispatch  int
}

// newReplayGraph seeds an in-memory graph from a ReplaySpec: the plan in
// status=open and every step in status=pending (the reconciler promotes
// them through the state machine just like production).
func newReplayGraph(spec ReplaySpec) *replayGraph {
	g := &replayGraph{
		steps:      make(map[string]StepView, len(spec.Steps)),
		planStatus: PlanStatusOpen,
		planView: PlanView{
			ID:          spec.PlanID,
			OwnerUserId: spec.OwnerUserId,
			Goal:        spec.Goal,
			Status:      PlanStatusOpen,
		},
	}
	for _, s := range spec.Steps {
		g.steps[s.ID] = StepView{
			ID:             s.ID,
			PlanID:         spec.PlanID,
			Status:         StepStatusPending,
			DependsOn:      s.DependsOn,
			Attempt:        0,
			IdempotencyKey: s.ID,
			OwnerUserId:    spec.OwnerUserId,
			Title:          s.Title,
			Input:          s.Input,
		}
		g.stepOrder = append(g.stepOrder, s.ID)
	}
	return g
}

// withDispatch sets the dispatcher seam (used by the eval harness to script
// per-step outcomes).
func (g *replayGraph) withDispatch(fn ReplayDispatch) *replayGraph {
	g.dispatch = fn
	return g
}

func (g *replayGraph) dispatchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nDispatch
}

func (g *replayGraph) dispatchOrder() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.dispatched))
	copy(out, g.dispatched)
	return out
}

// --- StepReader ---

func (g *replayGraph) StepsForPlan(_ context.Context, planID string) ([]StepView, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]StepView, 0, len(g.stepOrder))
	for _, id := range g.stepOrder {
		s := g.steps[id]
		if s.PlanID == planID {
			out = append(out, s)
		}
	}
	return out, "default", nil
}

func (g *replayGraph) PlanStatus(_ context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.planStatus, nil
}

func (g *replayGraph) PlanView(_ context.Context, _ string) (PlanView, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := g.planView
	v.Status = g.planStatus
	return v, true, nil
}

// --- StepClaimer (atomic ready->running, claim-once) ---

func (g *replayGraph) ClaimStep(_ context.Context, _ string, stepID string, _ int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.steps[stepID]
	if !ok {
		return false, fmt.Errorf("replay: unknown step %q", stepID)
	}
	if s.Status != StepStatusReady {
		return false, nil // lost the race / invalid transition
	}
	s.Status = StepStatusRunning
	g.steps[stepID] = s
	g.dispatched = append(g.dispatched, stepID)
	g.nDispatch++
	return true, nil
}

// --- Writer ---

func (g *replayGraph) MarkStepReady(_ context.Context, step StepView) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[step.ID]
	s.Status = StepStatusReady
	g.steps[step.ID] = s
	return nil
}

func (g *replayGraph) StartStep(_ context.Context, _ string) error { return nil }

func (g *replayGraph) CompleteStep(_ context.Context, stepID string, result map[string]any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[stepID]
	s.Status = StepStatusDone
	g.steps[stepID] = s
	return nil
}

func (g *replayGraph) FailStep(_ context.Context, stepID, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.steps[stepID]
	s.Status = StepStatusFailed
	g.steps[stepID] = s
	return nil
}

func (g *replayGraph) RecordObservation(_ context.Context, _ Observation) error { return nil }

func (g *replayGraph) SetPlanStatus(_ context.Context, _ PlanView, status, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.planStatus = status
	return nil
}

// --- Dispatcher ---

func (g *replayGraph) Dispatch(ctx context.Context, step StepView) (map[string]any, int, error) {
	if g.dispatch != nil {
		return g.dispatch(ctx, step)
	}
	// Default: deterministic success with no tool calls.
	return map[string]any{"replayed": true, "at": time.Now().UTC().Format(time.RFC3339Nano)}, 0, nil
}
