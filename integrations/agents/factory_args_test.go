package agents

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// TestBuildCreateAgentArgs_StampsKindSpecialist pins the contract for
// memql#398 + memql#399: every agent the factory creates lands with
// kind="specialist" regardless of caller. The factory only ever
// creates specialists -- the assistant + system buckets come from
// their own seed materializers.
func TestBuildCreateAgentArgs_StampsKindSpecialist(t *testing.T) {
	role := roleSnapshot{
		Slug:            "it-support",
		Name:            "IT Support",
		Tier:            "A",
		LockedSkillIds:  []string{"workbench-baseline"},
		DefaultSkillIds: []string{"engineering-baseline"},
	}
	decision := factoryDecision{Action: "create", RoleSlug: "it-support", Reasoning: "fits"}

	args := buildCreateAgentArgs("agent-abc", "user-xyz", decision, role, factoryRun{})

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

// TestBuildCreateAgentArgs_NoRunNoOriginatingRun pins the lineage shape for
// a creation outside any run: createdBy bucket is "user" and no
// lineage.originatingRunId is stamped -- there is no run to point at.
func TestBuildCreateAgentArgs_NoRunNoOriginatingRun(t *testing.T) {
	role := roleSnapshot{Slug: "it-support", Name: "IT Support", Tier: "A"}
	decision := factoryDecision{Action: "create", RoleSlug: "it-support"}

	args := buildCreateAgentArgs("a1", "u1", decision, role, factoryRun{})

	lineage, ok := args["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %v", args["lineage"])
	}
	if got := lineage["createdBy"]; got != "user" {
		t.Errorf("lineage.createdBy: got %v want \"user\" (no run)", got)
	}
	if _, has := lineage["originatingRunId"]; has {
		t.Errorf("lineage.originatingRunId stamped on a creation outside any run: %v", lineage["originatingRunId"])
	}
	if _, has := lineage["originatingPlanId"]; has {
		t.Errorf("lineage.originatingPlanId is a field v1:agents:agent no longer declares: %v", lineage["originatingPlanId"])
	}
}

// TestBuildCreateAgentArgs_RunOnTheCallIsRecorded pins memql#5436: an agent
// the ensureAgent tool creates while a run is being worked records that run
// in lineage.originatingRunId -- the field agentsForRun reads, which nothing
// wrote before -- and keeps the "user" bucket, because no caller NAMED the
// run.
func TestBuildCreateAgentArgs_RunOnTheCallIsRecorded(t *testing.T) {
	role := roleSnapshot{Slug: "it-support", Name: "IT Support", Tier: "A"}
	decision := factoryDecision{Action: "create", RoleSlug: "it-support"}

	args := buildCreateAgentArgs("a1", "u1", decision, role, factoryRun{Id: "v1:work:run:r7"})

	lineage, ok := args["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %v", args["lineage"])
	}
	if got := lineage["originatingRunId"]; got != "v1:work:run:r7" {
		t.Errorf("lineage.originatingRunId: got %v want \"v1:work:run:r7\"", got)
	}
	if got := lineage["createdBy"]; got != "user" {
		t.Errorf("lineage.createdBy: got %v want \"user\" (the tool path keeps its bucket)", got)
	}
	if _, has := lineage["originatingPlanId"]; has {
		t.Errorf("lineage.originatingPlanId is a field v1:agents:agent no longer declares: %v", lineage["originatingPlanId"])
	}
}

// TestBuildCreateAgentArgs_PlannerDrivenLineage pins the planner
// auto-provision contract (memql#399, in run vocabulary since memql#5436).
// When a caller names its run, the factory stamps:
//   - lineage.createdBy = "planner"
//   - lineage.originatingRunId = the run id
func TestBuildCreateAgentArgs_PlannerDrivenLineage(t *testing.T) {
	role := roleSnapshot{Slug: "data-analysis", Name: "Data Analysis", Tier: "A"}
	decision := factoryDecision{Action: "create", RoleSlug: "data-analysis"}

	args := buildCreateAgentArgs("a1", "u1", decision, role, factoryRun{Id: "run-42", RunDriven: true})

	lineage, ok := args["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %v", args["lineage"])
	}
	if got := lineage["createdBy"]; got != "planner" {
		t.Errorf("lineage.createdBy: got %v want \"planner\" (planner-driven path)", got)
	}
	if got := lineage["originatingRunId"]; got != "run-42" {
		t.Errorf("lineage.originatingRunId: got %v want \"run-42\"", got)
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
		SkillIds: []string{"exampleapp-ui", "go-backend-engineering"}, // overlap with locked
	}

	args := buildCreateAgentArgs("a1", "u1", decision, role, factoryRun{})

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
		"exampleapp-ui":          true,
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
// extend audit contract (memql#405, run vocabulary since memql#5436): a
// planner-driven extend stamps the per-user Planner Agent id as
// actorAgentId, carries the run as runId, and leaves actorUserId unset.
func TestBuildSkillChangeEventArgs_PlannerDriven(t *testing.T) {
	before := map[string]any{"domainIds": []string{"d1"}}
	after := map[string]any{"domainIds": []string{"d1", "d2"}}

	args := buildSkillChangeEventArgs("ev-1", "agent-7", "skill-new", "v1:identity:user:jose", factoryRun{Id: "run-42", RunDriven: true}, before, after)

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
	if got := args["runId"]; got != "run-42" {
		t.Errorf("runId: got %v want \"run-42\"", got)
	}
	if _, has := args["planId"]; has {
		t.Errorf("planId is an argument createSkillChangeEvent no longer declares: %v", args["planId"])
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

// TestBuildSkillChangeEventArgs_GADriven pins the GA-driven extend audit
// contract (memql#405): the ensureAgent tool path stamps actorUserId from
// the caller and leaves actorAgentId unset. Outside any run no runId is
// written; inside one, the run is recorded without changing attribution.
func TestBuildSkillChangeEventArgs_GADriven(t *testing.T) {
	args := buildSkillChangeEventArgs("ev-2", "agent-9", "skill-x", "v1:identity:user:dana", factoryRun{}, map[string]any{}, map[string]any{})

	if got := args["actorUserId"]; got != "v1:identity:user:dana" {
		t.Errorf("actorUserId: got %v want \"v1:identity:user:dana\" (GA-driven)", got)
	}
	if _, has := args["actorAgentId"]; has {
		t.Errorf("actorAgentId unexpectedly set on GA-driven extend: %v", args["actorAgentId"])
	}
	for _, key := range []string{"runId", "planId"} {
		if _, has := args[key]; has {
			t.Errorf("%s written for an extend outside any run: %v", key, args[key])
		}
	}

	inRun := buildSkillChangeEventArgs("ev-3", "agent-9", "skill-x", "v1:identity:user:dana", factoryRun{Id: "v1:work:run:r7"}, map[string]any{}, map[string]any{})
	if got := inRun["runId"]; got != "v1:work:run:r7" {
		t.Errorf("runId: got %v want \"v1:work:run:r7\" (the run the extend happened under)", got)
	}
	if got := inRun["actorUserId"]; got != "v1:identity:user:dana" {
		t.Errorf("actorUserId: got %v; a run on the call must not move the attribution to the planner", got)
	}
}

// TestRunForFactory pins where the factory learns its run (memql#5436): an
// explicit runId argument, which is the planner-driven signal; else the work
// run the call carries on its context, which is not; else none.
func TestRunForFactory(t *testing.T) {
	inRun := common.ContextWithRun(context.Background(), common.RunContext{RunId: "v1:work:run:ctx", OwnerUserId: "u1"})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		args map[string]any
		want factoryRun
	}{
		{"no run anywhere", context.Background(), map[string]any{}, factoryRun{}},
		{"the run on the context", inRun, map[string]any{}, factoryRun{Id: "v1:work:run:ctx"}},
		{"a named run", context.Background(), map[string]any{"runId": "run-42"}, factoryRun{Id: "run-42", RunDriven: true}},
		{"a named run wins over the context", inRun, map[string]any{"runId": "run-42"}, factoryRun{Id: "run-42", RunDriven: true}},
		{"a blank name is no name", inRun, map[string]any{"runId": "  "}, factoryRun{Id: "v1:work:run:ctx"}},
	} {
		if got := runForFactory(tc.ctx, tc.args); got != tc.want {
			t.Errorf("%s: runForFactory = %+v, want %+v", tc.name, got, tc.want)
		}
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
