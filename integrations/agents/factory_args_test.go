package agents

import (
	"testing"
)

// TestBuildCreateAgentArgs_StampsKindSpecialist pins the contract for
// memql#398 + memql#399: every agent the factory creates lands with
// kind="specialist" regardless of caller. The factory only ever
// creates specialists -- the assistant + system buckets come from
// their own seed materializers.
func TestBuildCreateAgentArgs_StampsKindSpecialist(t *testing.T) {
	role := roleSnapshot{
		Slug:                  "it-support",
		Name:                  "IT Support",
		Tier:                  "A",
		LockedSkillIds:        []string{"workbench-baseline"},
		DefaultSkillIds:       []string{"engineering-baseline"},
		RecommendedPolicySlug: "balancedChat",
	}
	decision := factoryDecision{Action: "create", RoleSlug: "it-support", Reasoning: "fits"}

	args := buildCreateAgentArgs("agent-abc", "user-xyz", decision, role, "")

	kind, ok := args["kind"].(string)
	if !ok || kind != "specialist" {
		t.Fatalf("kind: got %v want \"specialist\"", args["kind"])
	}
	role2, ok := args["role"].(string)
	if !ok || role2 != "specialist" {
		t.Errorf("role: got %v want \"specialist\"", args["role"])
	}
	if got := args["roleSlug"]; got != "it-support" {
		t.Errorf("roleSlug: got %v want \"it-support\"", got)
	}
	if got := args["ownerUserId"]; got != "user-xyz" {
		t.Errorf("ownerUserId: got %v want \"user-xyz\"", got)
	}
	if got := args["agentId"]; got != "agent-abc" {
		t.Errorf("agentId: got %v want \"agent-abc\"", got)
	}
}

// TestBuildCreateAgentArgs_GADrivenLineage pins the lineage shape for
// the GA-driven path (planId empty): createdBy bucket is "user",
// originatingPlanId is NOT stamped. The GA's ensureAgent tool calls
// the factory from a user turn; the resulting specialist has no Plan
// back-pointer.
func TestBuildCreateAgentArgs_GADrivenLineage(t *testing.T) {
	role := roleSnapshot{Slug: "it-support", Name: "IT Support", Tier: "A"}
	decision := factoryDecision{Action: "create", RoleSlug: "it-support"}

	args := buildCreateAgentArgs("a1", "u1", decision, role, "")

	lineage, ok := args["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %v", args["lineage"])
	}
	if got := lineage["createdBy"]; got != "user" {
		t.Errorf("lineage.createdBy: got %v want \"user\" (GA-driven path)", got)
	}
	if _, has := lineage["originatingPlanId"]; has {
		t.Errorf("lineage.originatingPlanId unexpectedly stamped on GA-driven create: %v", lineage["originatingPlanId"])
	}
}

// TestBuildCreateAgentArgs_PlannerDrivenLineage pins the planner
// auto-provision contract (memql#399). When ensureSpecialistForPlan
// passes a non-empty planId, the factory stamps:
//   - lineage.createdBy = "planner"
//   - lineage.originatingPlanId = the plan id
//
// The Tasks-page Plan/agent attribution UI reads these to render
// "created for Plan X" on planner-spawned specialists.
func TestBuildCreateAgentArgs_PlannerDrivenLineage(t *testing.T) {
	role := roleSnapshot{Slug: "data-analysis", Name: "Data Analysis", Tier: "A"}
	decision := factoryDecision{Action: "create", RoleSlug: "data-analysis"}

	args := buildCreateAgentArgs("a1", "u1", decision, role, "plan-42")

	lineage, ok := args["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %v", args["lineage"])
	}
	if got := lineage["createdBy"]; got != "planner" {
		t.Errorf("lineage.createdBy: got %v want \"planner\" (planner-driven path)", got)
	}
	if got := lineage["originatingPlanId"]; got != "plan-42" {
		t.Errorf("lineage.originatingPlanId: got %v want \"plan-42\"", got)
	}
	// Kind invariant holds across both code paths.
	if got := args["kind"]; got != "specialist" {
		t.Errorf("kind: got %v want \"specialist\" (planner-driven path)", got)
	}
}

// TestBuildCreateAgentArgs_SkillUnion pins the skill-composition
// behavior: the args map's capabilities.skillIds is the union of the
// role's locked + default sets plus the analysis-supplied additions,
// deduplicated. Regression guard against silent breakage from the
// role catalog evolving.
func TestBuildCreateAgentArgs_SkillUnion(t *testing.T) {
	role := roleSnapshot{
		Slug:            "engineering",
		Name:            "Engineering",
		Tier:            "A",
		LockedSkillIds:  []string{"workbench-baseline", "go-backend-engineering"},
		DefaultSkillIds: []string{"engineering-baseline"},
	}
	decision := factoryDecision{
		Action:   "create",
		RoleSlug: "engineering",
		SkillIds: []string{"copresent-ui", "go-backend-engineering"}, // overlap with locked
	}

	args := buildCreateAgentArgs("a1", "u1", decision, role, "")

	caps, ok := args["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing or wrong type: %v", args["capabilities"])
	}
	skillIds, ok := caps["skillIds"].([]string)
	if !ok {
		t.Fatalf("capabilities.skillIds missing or wrong type: %v", caps["skillIds"])
	}
	want := map[string]bool{
		"workbench-baseline":     true,
		"go-backend-engineering": true,
		"engineering-baseline":   true,
		"copresent-ui":           true,
	}
	if len(skillIds) != len(want) {
		t.Errorf("skillIds count: got %d want %d (got=%v)", len(skillIds), len(want), skillIds)
	}
	for _, sid := range skillIds {
		if !want[sid] {
			t.Errorf("skillIds includes unexpected id %q", sid)
		}
	}
}
