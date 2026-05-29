package agents

import (
	"reflect"
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

// TestBuildSkillChangeEventArgs_PlannerDriven pins the planner-driven
// extend audit contract (memql#405): a planner-driven extend (planId
// set) stamps the per-user Planner Agent id as actorAgentId, carries
// the originating planId, and leaves actorUserId unset. The skillId +
// targetAgentId + changeKind=attached are load-bearing for the Tasks
// "extended for Plan X" attribution UI.
func TestBuildSkillChangeEventArgs_PlannerDriven(t *testing.T) {
	before := map[string]any{"domainIds": []string{"d1"}}
	after := map[string]any{"domainIds": []string{"d1", "d2"}}

	args := buildSkillChangeEventArgs("ev-1", "agent-7", "skill-new", "v1:identity:user:jose", "plan-42", before, after)

	if got := args["targetAgentId"]; got != "agent-7" {
		t.Errorf("targetAgentId: got %v want \"agent-7\"", got)
	}
	if got := args["skillId"]; got != "skill-new" {
		t.Errorf("skillId: got %v want \"skill-new\"", got)
	}
	if got := args["skillChangeEventId"]; got != "ev-1" {
		t.Errorf("skillChangeEventId: got %v want \"ev-1\"", got)
	}
	if got := args["changeKind"]; got != "attached" {
		t.Errorf("changeKind: got %v want \"attached\"", got)
	}
	// Planner attribution: actorAgentId = plannerAgent-<userShortId>,
	// canonical user prefix stripped.
	if got := args["actorAgentId"]; got != "plannerAgent-jose" {
		t.Errorf("actorAgentId: got %v want \"plannerAgent-jose\" (planner-driven)", got)
	}
	if got := args["planId"]; got != "plan-42" {
		t.Errorf("planId: got %v want \"plan-42\"", got)
	}
	if _, has := args["actorUserId"]; has {
		t.Errorf("actorUserId unexpectedly set on planner-driven extend: %v", args["actorUserId"])
	}
	if !reflect.DeepEqual(args["before"], before) {
		t.Errorf("before snapshot not carried verbatim: got %v want %v", args["before"], before)
	}
	if !reflect.DeepEqual(args["after"], after) {
		t.Errorf("after snapshot not carried verbatim: got %v want %v", args["after"], after)
	}
}

// TestBuildSkillChangeEventArgs_GADriven pins the GA-driven extend
// audit contract (memql#405): the ensureAgent tool path (planId empty)
// stamps actorUserId from the caller, leaves actorAgentId unset, and
// carries an empty planId (no Plan back-pointer).
func TestBuildSkillChangeEventArgs_GADriven(t *testing.T) {
	args := buildSkillChangeEventArgs("ev-2", "agent-9", "skill-x", "v1:identity:user:dana", "", map[string]any{}, map[string]any{})

	if got := args["actorUserId"]; got != "v1:identity:user:dana" {
		t.Errorf("actorUserId: got %v want \"v1:identity:user:dana\" (GA-driven)", got)
	}
	if _, has := args["actorAgentId"]; has {
		t.Errorf("actorAgentId unexpectedly set on GA-driven extend: %v", args["actorAgentId"])
	}
	if got, ok := args["planId"].(string); !ok || got != "" {
		t.Errorf("planId: got %v want \"\" (GA-driven has no Plan back-pointer)", args["planId"])
	}
}

// TestDiffStrings_NetNewOnly pins the net-new computation the extend
// audit relies on: exactly the skills present post-extend that were NOT
// present pre-extend get an event row -- existing skills do not
// re-emit (memql#405). One event per net-new skill, dedup-safe.
func TestDiffStrings_NetNewOnly(t *testing.T) {
	preExtend := []string{"a", "b"}
	merged := []string{"a", "b", "c", "d"}

	netNew := diffStrings(merged, preExtend)

	want := []string{"c", "d"}
	if !reflect.DeepEqual(netNew, want) {
		t.Errorf("net-new skills: got %v want %v", netNew, want)
	}

	// No additions -> no event rows.
	if got := diffStrings([]string{"a", "b"}, []string{"a", "b"}); len(got) != 0 {
		t.Errorf("expected zero net-new when nothing added, got %v", got)
	}
	// Empties + dups are skipped.
	if got := diffStrings([]string{"a", "", "c", "c"}, []string{"a"}); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("diffStrings did not skip empties/dups: got %v want [c]", got)
	}
}

// TestPlannerAgentId_StripsCanonicalPrefix pins the per-user Planner
// Agent id derivation -- it must match the seed materializer's
// `<seedName>-<userShortId>` form (memql#405).
func TestPlannerAgentId_StripsCanonicalPrefix(t *testing.T) {
	if got := plannerAgentId("v1:identity:user:jose"); got != "plannerAgent-jose" {
		t.Errorf("plannerAgentId(canonical): got %q want \"plannerAgent-jose\"", got)
	}
	if got := plannerAgentId("jose"); got != "plannerAgent-jose" {
		t.Errorf("plannerAgentId(bare): got %q want \"plannerAgent-jose\"", got)
	}
}
