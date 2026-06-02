package planner

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
)

// testLogger returns a discard slog logger satisfying plannerLogger so
// the loop's direct r.logger.X calls don't panic in tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustCron parses a standard cron expression or fails the test.
func mustCron(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		t.Fatalf("parse cron %q: %v", expr, err)
	}
	return sched
}

// rowsEnvelope wraps flat projected rows in the {output: [...]} shape
// MaterializeRows reads (mirrors trainPlanRow in the dispatcher test).
func rowsEnvelope(rows ...map[string]any) any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}
}

// --- C1: due-check ---------------------------------------------------------

func TestReactiveLoop_RecurringDue_NeverEvaluatedFires(t *testing.T) {
	r := NewReactiveLoop(&fakeEngine{}, testLogger())
	now := time.Date(2026, 6, 1, 9, 5, 0, 0, time.UTC) // Monday 09:05
	row := map[string]any{
		"id":              "v1:planner:responsibility:r1",
		"trigger":         "recurring",
		"schedule":        "0 9 * * 1", // Monday 09:00
		"lastEvaluatedAt": "",          // never evaluated
	}
	if !r.isDue("recurring", row, now) {
		t.Fatalf("never-evaluated recurring row past its 09:00 occurrence should be due")
	}
}

func TestReactiveLoop_RecurringNotDue_AlreadyEvaluated(t *testing.T) {
	r := NewReactiveLoop(&fakeEngine{}, testLogger())
	now := time.Date(2026, 6, 1, 9, 5, 0, 0, time.UTC) // Monday 09:05
	row := map[string]any{
		"id":       "v1:planner:responsibility:r1",
		"trigger":  "recurring",
		"schedule": "0 9 * * 1",
		// evaluated at 09:01, after the 09:00 occurrence -> not due again
		"lastEvaluatedAt": "2026-06-01T09:01:00Z",
	}
	if r.isDue("recurring", row, now) {
		t.Fatalf("recurring row already evaluated after its occurrence should NOT be due")
	}
}

func TestReactiveLoop_RecurringBadCron_NotDue(t *testing.T) {
	r := NewReactiveLoop(&fakeEngine{}, testLogger())
	row := map[string]any{
		"id":       "r1",
		"trigger":  "recurring",
		"schedule": "not a cron",
	}
	if r.isDue("recurring", row, time.Now().UTC()) {
		t.Fatalf("unparseable cron must not be treated as due")
	}
}

func TestReactiveLoop_StandingNeverDueOnHeartbeat(t *testing.T) {
	r := NewReactiveLoop(&fakeEngine{}, testLogger())
	row := map[string]any{"id": "r1", "trigger": "standing"}
	if r.isDue("standing", row, time.Now().UTC()) {
		t.Fatalf("standing rows are always-on context, never due on the heartbeat")
	}
}

// --- C1: dedup -------------------------------------------------------------

func TestReactiveLoop_HasLivePlan(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryPlansForResponsibility") {
				return rowsEnvelope(map[string]any{"id": "p1", "status": "running"}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	if !r.hasLivePlan(context.Background(), "r1") {
		t.Fatalf("a running plan for the responsibility must count as live")
	}
}

func TestReactiveLoop_NoLivePlan_AllTerminal(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryPlansForResponsibility") {
				return rowsEnvelope(
					map[string]any{"id": "p1", "status": "succeeded"},
					map[string]any{"id": "p2", "status": "cancelled"},
				), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	if r.hasLivePlan(context.Background(), "r1") {
		t.Fatalf("only terminal plans exist -> no live plan")
	}
}

// --- C2: routing -----------------------------------------------------------

func TestReactiveLoop_RouteAssistant_ResolvesGA(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryAssistantAgentForUser") {
				return rowsEnvelope(map[string]any{"id": "v1:agents:agent:ga1"}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":         "r1",
		"targetKind": "assistant",
		"statement":  "keep the tracker tidy",
		"trigger":    "recurring",
	}
	agentId, err := r.routeResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row)
	if err != nil {
		t.Fatalf("route assistant: %v", err)
	}
	if agentId != "v1:agents:agent:ga1" {
		t.Fatalf("expected GA id, got %q", agentId)
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationAssignResponsibility") == 0 {
		t.Fatalf("assistant routing should persist the assignment")
	}
}

func TestReactiveLoop_RouteUnassigned_RunsFactory(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "ensureAgentForGoal") {
				return rowsEnvelope(map[string]any{"agentId": "v1:agents:agent:spec1", "action": "create"}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":         "r1",
		"targetKind": "unassigned",
		"statement":  "summarize marketing performance",
		"trigger":    "recurring",
	}
	agentId, err := r.routeResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row)
	if err != nil {
		t.Fatalf("route unassigned: %v", err)
	}
	if agentId != "v1:agents:agent:spec1" {
		t.Fatalf("expected minted specialist id, got %q", agentId)
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "ensureAgentForGoal") == 0 {
		t.Fatalf("unassigned routing must run the factory")
	}
	if countContains(exec, "mutationAssignResponsibility") == 0 {
		t.Fatalf("specialist routing must persist the assignment")
	}
}

func TestReactiveLoop_RouteSpecialistAlreadyBound_Idempotent(t *testing.T) {
	eng := &fakeEngine{}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":              "r1",
		"targetKind":      "specialist",
		"assignedAgentId": "v1:agents:agent:spec1",
		"statement":       "review PRs",
		"trigger":         "recurring",
	}
	agentId, err := r.routeResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row)
	if err != nil {
		t.Fatalf("route bound specialist: %v", err)
	}
	if agentId != "v1:agents:agent:spec1" {
		t.Fatalf("bound specialist should be returned verbatim, got %q", agentId)
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "ensureAgentForGoal") != 0 {
		t.Fatalf("an already-bound specialist must not re-run the factory (idempotent)")
	}
}

// --- C3: honor -------------------------------------------------------------

func TestReactiveLoop_HonorRecurring_CreatesPlan(t *testing.T) {
	eng := &fakeEngine{}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":        "v1:planner:responsibility:r1",
		"trigger":   "recurring",
		"statement": "summarize last week",
	}
	res, err := r.honorResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row, "v1:agents:agent:spec1")
	if err != nil {
		t.Fatalf("honor recurring: %v", err)
	}
	if !strings.Contains(res, "Plan") {
		t.Fatalf("recurring honor should report a spawned Plan, got %q", res)
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationCreatePlan") == 0 {
		t.Fatalf("recurring honor must create a Plan")
	}
	// The plan must carry the responsibilityId back-pointer for dedup.
	found := false
	for _, c := range exec {
		if strings.Contains(c, "mutationCreatePlan") && strings.Contains(c, "responsibilityId") {
			found = true
		}
	}
	if !found {
		t.Fatalf("created plan must stamp input.responsibilityId for the dedup query")
	}
}

func TestReactiveLoop_HonorStanding_InjectsContextNoPlan(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryAgentById") {
				return rowsEnvelope(map[string]any{
					"id":      "v1:agents:agent:spec1",
					"lineage": map[string]any{"extensionGoals": []any{"existing goal"}},
				}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":        "v1:planner:responsibility:r1",
		"trigger":   "standing",
		"statement": "comment every private function",
	}
	_, err := r.honorResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row, "v1:agents:agent:spec1")
	if err != nil {
		t.Fatalf("honor standing: %v", err)
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationCreatePlan") != 0 {
		t.Fatalf("standing honor must NOT create a Plan")
	}
	if countContains(exec, "mutationUpdateAgent") == 0 {
		t.Fatalf("standing honor must inject the directive via mutationUpdateAgent")
	}
	// The injected payload must append the directive to extensionGoals.
	found := false
	for _, c := range exec {
		if strings.Contains(c, "mutationUpdateAgent") &&
			strings.Contains(c, "extensionGoals") &&
			strings.Contains(c, "comment every private function") {
			found = true
		}
	}
	if !found {
		t.Fatalf("standing inject must append the directive into lineage.extensionGoals")
	}
}

func TestReactiveLoop_StandingInject_Idempotent(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryAgentById") {
				return rowsEnvelope(map[string]any{
					"id":      "v1:agents:agent:spec1",
					"lineage": map[string]any{"extensionGoals": []any{"comment every private function"}},
				}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":        "r1",
		"trigger":   "standing",
		"statement": "comment every private function",
	}
	r.maybeInjectStanding(ownerActorContext(context.Background(), "u1"), row, "v1:agents:agent:spec1")
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationUpdateAgent") != 0 {
		t.Fatalf("a directive already present must not be re-written (idempotent)")
	}
}

func TestReactiveLoop_ProcessStanding_InjectsNoPlanNoDueCheck(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryAgentById") {
				return rowsEnvelope(map[string]any{
					"id":      "v1:agents:agent:spec1",
					"lineage": map[string]any{"extensionGoals": []any{}},
				}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	row := map[string]any{
		"id":              "v1:planner:responsibility:r1",
		"trigger":         "standing",
		"targetKind":      "specialist",
		"assignedAgentId": "v1:agents:agent:spec1",
		"statement":       "comment every private function",
	}
	honored := r.processResponsibility(ownerActorContext(context.Background(), "u1"), "u1", row, time.Now().UTC())
	if !honored {
		t.Fatalf("a standing row bound to a specialist should be honored (injected)")
	}
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationCreatePlan") != 0 {
		t.Fatalf("standing row must not create a Plan")
	}
	if countContains(exec, "queryPlansForResponsibility") != 0 {
		t.Fatalf("standing row must not run the live-plan dedup query (not Plan-driven)")
	}
	if countContains(exec, "mutationUpdateAgent") == 0 {
		t.Fatalf("standing row must inject via mutationUpdateAgent")
	}
	if countContains(exec, "mutationRecordResponsibilityEvaluation") == 0 {
		t.Fatalf("standing row should stamp lastResult/lastEvaluatedAt")
	}
}

// --- C4: convergence -------------------------------------------------------

func TestParseConvergence_Silent(t *testing.T) {
	d, err := parseConvergence(`{"actions": [], "silentReason": "all on track"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Actions) != 0 {
		t.Fatalf("expected no actions")
	}
	if d.SilentReason != "all on track" {
		t.Fatalf("silentReason mismatch: %q", d.SilentReason)
	}
}

func TestParseConvergence_FencedAction(t *testing.T) {
	raw := "```json\n{\"actions\": [{\"kind\": \"surfaceNudge\", \"statement\": \"goal X at 60%\", \"confidence\": 0.8}]}\n```"
	d, err := parseConvergence(raw)
	if err != nil {
		t.Fatalf("parse fenced: %v", err)
	}
	if len(d.Actions) != 1 || d.Actions[0].Kind != "surfaceNudge" {
		t.Fatalf("expected one surfaceNudge action, got %+v", d.Actions)
	}
	if d.Actions[0].Confidence != 0.8 {
		t.Fatalf("confidence parse mismatch: %v", d.Actions[0].Confidence)
	}
}

func TestReactiveLoop_ConvergenceLowConfidenceDropped(t *testing.T) {
	eng := &fakeEngine{}
	r := NewReactiveLoop(eng, testLogger())
	r.dispatchConvergenceAction(ownerActorContext(context.Background(), "u1"), "u1",
		convergenceAction{Kind: "createPlan", Statement: "x", Confidence: 0.4},
		time.Now().UTC())
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationCreatePlan") != 0 {
		t.Fatalf("low-confidence convergence actions must be dropped (quiet guard)")
	}
}

func TestReactiveLoop_ConvergenceDedup(t *testing.T) {
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			if strings.Contains(q, "queryAssistantAgentForUser") {
				return rowsEnvelope(map[string]any{"id": "ga1"}), nil
			}
			return nil, nil
		},
	}
	r := NewReactiveLoop(eng, testLogger())
	now := time.Now().UTC()
	act := convergenceAction{Kind: "createPlan", ResponsibilityId: "r1", Statement: "x", Confidence: 0.9}
	r.dispatchConvergenceAction(ownerActorContext(context.Background(), "u1"), "u1", act, now)
	r.dispatchConvergenceAction(ownerActorContext(context.Background(), "u1"), "u1", act, now.Add(time.Minute))
	exec, _, _ := eng.snapshot()
	if countContains(exec, "mutationCreatePlan") != 1 {
		t.Fatalf("the dedup guard should let the same action fire exactly once within the guard window, got %d", countContains(exec, "mutationCreatePlan"))
	}
}

// --- helpers ---------------------------------------------------------------

// latestOccurrenceBefore sanity: a daily 09:00 schedule observed at
// 09:05 yields the 09:00 occurrence of the same day.
func TestLatestOccurrenceBefore_Daily(t *testing.T) {
	sched := mustCron(t, "0 9 * * *")
	now := time.Date(2026, 6, 1, 9, 5, 0, 0, time.UTC)
	got := latestOccurrenceBefore(sched, now)
	want := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("latest occurrence: got %v want %v", got, want)
	}
}
