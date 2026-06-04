package planner

import (
	"context"
	"log/slog"
	"testing"
)

// findCall returns the first recorded engine call containing sub, or ""
// when none match.
func findCall(calls []string, sub string) string {
	for _, c := range calls {
		if containsAll(c, sub) {
			return c
		}
	}
	return ""
}

// agentRow builds a queryAgentById response in the MaterializeRows
// envelope shape (a flat row under `output`), carrying the agent's
// capabilities.domains.
func agentRow(agentId string, domains ...string) any {
	d := make([]any, 0, len(domains))
	for _, s := range domains {
		d = append(d, s)
	}
	return map[string]any{
		"output": []any{
			map[string]any{
				"id":           agentId,
				"capabilities": map[string]any{"domains": d},
			},
		},
	}
}

// approvedTrainPlanRow is an approved userGoal plan with an owner agent
// already stamped (by a prior createSpecialist iteration).
func approvedTrainPlanRow(planId, ownerAgentId string) any {
	return map[string]any{
		"output": []any{
			map[string]any{
				"id":           planId,
				"kind":         "userGoal",
				"status":       "running",
				"goal":         "stand up a payroll specialist",
				"spaceId":      "v1:cognition:space:s1",
				"requestedBy":  "v1:identity:user:u1",
				"ownerAgentId": ownerAgentId,
				"metrics":      map[string]any{"specialistApproved": true},
			},
		},
	}
}

// memql#852 Gap 2: an approved spawnTrainingPlan mints a kind=trainSpecialist
// child plan targeting the specialist's primary attached domain, and marks
// the originating plan succeeded -- no fallback domain creation.
func TestDispatchApprovedTrainingPlan_ReusesSpecialistDomain(t *testing.T) {
	const specialistId = "v1:agents:agent:payroll"
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		switch {
		case containsAll(query, "queryPlanById"):
			return approvedTrainPlanRow("plan-1", specialistId), nil
		case containsAll(query, "queryAgentById"):
			return agentRow(specialistId, "financial-data", "employee-records"), nil
		default:
			return nil, nil
		}
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	d := &plannerDecision{Action: "spawnTrainingPlan", SpecialistId: specialistId, Topic: "payroll", Mode: "initial"}
	if err := l.dispatchApprovedTrainingPlan(context.Background(), "plan-1", d); err != nil {
		t.Fatalf("dispatchApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()

	if got := countContains(exec, `"kind": "trainSpecialist"`); got != 1 {
		t.Fatalf("must mint exactly one trainSpecialist plan, got %d\ncalls: %v", got, exec)
	}
	mint := findCall(exec, `"kind": "trainSpecialist"`)
	if !containsAll(mint, `"domainId":"financial-data"`) {
		t.Fatalf("trainSpecialist input.domainId must be the specialist's primary attached domain (financial-data)\nmint: %s", mint)
	}
	if countContains(exec, "mutationCreateKnowledgeDomain") != 0 {
		t.Fatalf("must NOT mint a fallback domain when the specialist already has one, got %d", countContains(exec, "mutationCreateKnowledgeDomain"))
	}
	if countContains(exec, `status:"succeeded"`) != 1 {
		t.Fatalf("must mark the originating plan succeeded, got %d", countContains(exec, `status:"succeeded"`))
	}
	// The minted plan carries the specialist + mode the dispatcher reads.
	if countContains(exec, specialistId) < 2 { // queryAgentById + the mint input
		t.Fatalf("trainSpecialist input must carry the specialistId")
	}
}

// When the decision omits specialistId, the handler falls back to the
// Plan's ownerAgentId (stamped by the createSpecialist iteration).
func TestDispatchApprovedTrainingPlan_SpecialistFromOwnerAgent(t *testing.T) {
	const ownerAgentId = "v1:agents:agent:fromowner"
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		switch {
		case containsAll(query, "queryPlanById"):
			return approvedTrainPlanRow("plan-2", ownerAgentId), nil
		case containsAll(query, "queryAgentById"):
			return agentRow(ownerAgentId, "customer-relations"), nil
		default:
			return nil, nil
		}
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	d := &plannerDecision{Action: "spawnTrainingPlan", Topic: "support"} // no SpecialistId
	if err := l.dispatchApprovedTrainingPlan(context.Background(), "plan-2", d); err != nil {
		t.Fatalf("dispatchApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, `"kind": "trainSpecialist"`) != 1 {
		t.Fatalf("must mint a trainSpecialist plan using the Plan's ownerAgentId\ncalls: %v", exec)
	}
	if countContains(exec, ownerAgentId) < 2 {
		t.Fatalf("must resolve + train the ownerAgentId specialist")
	}
}

// memql#852 Gap 2 fallback: a specialist with no attached domain mints an
// empty knowledge domain (for the Trainer to seed) before the train plan.
func TestDispatchApprovedTrainingPlan_FallbackMintsDomain(t *testing.T) {
	const specialistId = "v1:agents:agent:bare"
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		switch {
		case containsAll(query, "queryPlanById"):
			return approvedTrainPlanRow("plan-3", specialistId), nil
		case containsAll(query, "queryAgentById"):
			return agentRow(specialistId), nil // zero domains
		default:
			return nil, nil
		}
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	d := &plannerDecision{Action: "spawnTrainingPlan", SpecialistId: specialistId, Topic: "logistics", Mode: "initial"}
	if err := l.dispatchApprovedTrainingPlan(context.Background(), "plan-3", d); err != nil {
		t.Fatalf("dispatchApprovedTrainingPlan: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationCreateKnowledgeDomain") != 1 {
		t.Fatalf("a domainless specialist must mint one fallback domain, got %d\ncalls: %v", countContains(exec, "mutationCreateKnowledgeDomain"), exec)
	}
	if countContains(exec, `"kind": "trainSpecialist"`) != 1 {
		t.Fatalf("must still mint the trainSpecialist plan after the fallback domain, got %d", countContains(exec, `"kind": "trainSpecialist"`))
	}
}

// "Declining mints nothing": an UNAPPROVED spawnTrainingPlan parks at the
// gate and never reaches the mint path -- no trainSpecialist plan, no
// training spend. (The approved path is exercised above.)
func TestSpawnTrainingPlan_UnapprovedDoesNotMint(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "plan-4", "kind": "userGoal", "status": "running",
			"goal": "stand up a specialist", "spaceId": "v1:cognition:space:s1",
			"requestedBy": "v1:identity:user:u1",
			// no metrics.specialistApproved -> gate blocks
		}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "queryPlanById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	handled, err := l.gateSpecialistAction(context.Background(), "plan-4", "spawnTrainingPlan")
	if err != nil || !handled {
		t.Fatalf("unapproved spawnTrainingPlan must be gated (parked): handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "trainSpecialist") != 0 {
		t.Fatalf("an unapproved training request must mint NO trainSpecialist plan, got %d", countContains(exec, "trainSpecialist"))
	}
	if countContains(exec, "specialist_approval_required") != 1 {
		t.Fatalf("must park for approval, got %d", countContains(exec, "specialist_approval_required"))
	}
}
