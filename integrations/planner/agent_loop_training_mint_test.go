package planner

import (
	"context"
	"log/slog"
	"testing"
)

// trainingFakeEngine returns a fakeEngine that answers the reads
// mintApprovedTrainingPlan makes: the parent plan, the specialist agent
// (with the given skillIds), and the active skill catalog (skill -> domains).
func trainingFakeEngine(parent, agent map[string]any, skills []map[string]any) *fakeEngine {
	return &fakeEngine{execResponder: func(query string) (any, error) {
		switch {
		case containsAll(query, "queryAgentById"):
			return rowsEnvelope(agent), nil
		case containsAll(query, "queryActiveSkillsFull"):
			return rowsEnvelope(skills...), nil
		case containsAll(query, "queryPlanById"):
			return rowsEnvelope(parent), nil
		default:
			return nil, nil
		}
	}}
}

func newTestLoop(fe *fakeEngine) *PlannerAgentLoop {
	return &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
}

// TestMintApprovedTrainingPlan_MintsAndSucceeds is the memql#852 Gap 2
// happy path: an approved spawnTrainingPlan whose specialist has an
// attached domain mints a kind=trainSpecialist child Plan (carrying the
// resolved domainId) and marks the parent Plan succeeded.
func TestMintApprovedTrainingPlan_MintsAndSucceeds(t *testing.T) {
	parent := map[string]any{
		"id": "plan-1", "kind": "userGoal", "status": "running",
		"goal": "become an expert on French employment law",
		"partitionId": "v1:cognition:space:s1", "requestedBy": "v1:identity:user:u1",
	}
	agent := map[string]any{
		"id": "v1:agents:agent:spec-1",
		"capabilities": map[string]any{
			"skillIds": []any{"v1:agents:skill:law-1"},
		},
	}
	skills := []map[string]any{
		{"id": "v1:agents:skill:law-1", "domainIds": []any{"v1:knowledge:knowledgeDomain:fr-law"}},
	}
	fe := trainingFakeEngine(parent, agent, skills)
	l := newTestLoop(fe)

	d := plannerDecision{Action: "spawnTrainingPlan", SpecialistId: "v1:agents:agent:spec-1", Topic: "French employment law", Mode: "initial"}
	if err := l.mintApprovedTrainingPlan(context.Background(), "plan-1", d); err != nil {
		t.Fatalf("mintApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()

	if countContains(exec, "mutationCreatePlan") != 1 {
		t.Fatalf("must mint exactly one Plan, got %d mutationCreatePlan calls", countContains(exec, "mutationCreatePlan"))
	}
	if countContains(exec, "trainSpecialist") < 1 {
		t.Fatalf("minted Plan must be kind=trainSpecialist")
	}
	if countContains(exec, "v1:knowledge:knowledgeDomain:fr-law") < 1 {
		t.Fatalf("minted Plan input must carry the resolved domainId from the specialist's skill")
	}
	if countContains(exec, `"succeeded"`) != 1 {
		t.Fatalf("parent Plan must be marked succeeded after dispatching the training child, got %d", countContains(exec, `"succeeded"`))
	}
	if countContains(exec, "awaitingFeedback") != 0 {
		t.Fatalf("an approved + resolvable training request must not escalate")
	}
}

// TestMintApprovedTrainingPlan_NoDomainEscalates: an approved training
// request whose specialist has no domain-bearing skill must NOT mint a
// (dispatcher-rejecting) Plan -- it escalates for feedback instead.
func TestMintApprovedTrainingPlan_NoDomainEscalates(t *testing.T) {
	parent := map[string]any{
		"id": "plan-2", "goal": "x", "partitionId": "v1:cognition:space:s1", "requestedBy": "v1:identity:user:u1",
	}
	agent := map[string]any{
		"id":           "v1:agents:agent:spec-2",
		"capabilities": map[string]any{"skillIds": []any{"v1:agents:skill:bare"}},
	}
	// skill exists but bundles no domains
	skills := []map[string]any{{"id": "v1:agents:skill:bare", "domainIds": []any{}}}
	fe := trainingFakeEngine(parent, agent, skills)
	l := newTestLoop(fe)

	d := plannerDecision{Action: "spawnTrainingPlan", SpecialistId: "v1:agents:agent:spec-2", Topic: "t", Mode: "initial"}
	if err := l.mintApprovedTrainingPlan(context.Background(), "plan-2", d); err != nil {
		t.Fatalf("mintApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationCreatePlan") != 0 {
		t.Fatalf("must NOT mint a training Plan when no domain resolves, got %d", countContains(exec, "mutationCreatePlan"))
	}
	if countContains(exec, "awaitingFeedback") != 1 {
		t.Fatalf("must escalate awaitingFeedback when no domain resolves, got %d", countContains(exec, "awaitingFeedback"))
	}
}

// TestMintApprovedTrainingPlan_NoSpecialistEscalates: a malformed decision
// with no specialistId escalates without touching the agent/skill reads.
func TestMintApprovedTrainingPlan_NoSpecialistEscalates(t *testing.T) {
	parent := map[string]any{"id": "plan-3", "goal": "x", "partitionId": "s", "requestedBy": "u"}
	fe := trainingFakeEngine(parent, map[string]any{}, nil)
	l := newTestLoop(fe)

	d := plannerDecision{Action: "spawnTrainingPlan", Topic: "t"} // SpecialistId empty
	if err := l.mintApprovedTrainingPlan(context.Background(), "plan-3", d); err != nil {
		t.Fatalf("mintApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "queryAgentById") != 0 {
		t.Fatalf("must not look up a specialist when none was named")
	}
	if countContains(exec, "mutationCreatePlan") != 0 || countContains(exec, "awaitingFeedback") != 1 {
		t.Fatalf("no specialist -> escalate, no mint")
	}
}

// TestResolveSpecialistPrimaryDomain returns the first domain in skillIds
// order, skipping skills that bundle no domains.
func TestResolveSpecialistPrimaryDomain(t *testing.T) {
	agent := map[string]any{
		"id": "v1:agents:agent:spec-1",
		"capabilities": map[string]any{
			"skillIds": []any{"v1:agents:skill:empty", "v1:agents:skill:has-dom"},
		},
	}
	skills := []map[string]any{
		{"id": "v1:agents:skill:empty", "domainIds": []any{}},
		{"id": "v1:agents:skill:has-dom", "domainIds": []any{"v1:knowledge:knowledgeDomain:d2", "v1:knowledge:knowledgeDomain:d3"}},
	}
	fe := trainingFakeEngine(map[string]any{}, agent, skills)
	l := newTestLoop(fe)

	got := l.resolveSpecialistPrimaryDomain(context.Background(), "v1:agents:agent:spec-1")
	if got != "v1:knowledge:knowledgeDomain:d2" {
		t.Fatalf("expected first domain of the first domain-bearing skill, got %q", got)
	}
}
