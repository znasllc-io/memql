package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// fakes for the planner's four narrow seams
// ---------------------------------------------------------------------------

// stubDecomposer returns a fixed step DAG (or an error to exercise the
// single-step fallback).
type stubDecomposer struct {
	steps []PlanStep
	err   error
}

func (d stubDecomposer) Decompose(_ context.Context, _ string) ([]PlanStep, error) {
	return d.steps, d.err
}

// fakeRoster is an in-memory roster + fitScore source. fitByText maps a
// (stepText-substring -> agentID -> score) so a test can dial in exactly
// which agent fits which step. Any unmatched pair scores 0.
type fakeRoster struct {
	agents []ExistingAgent
	// scoreFn returns the fit score for (text, agentID). Lets a test model
	// route/upgrade/provision/dedup deterministically.
	scoreFn func(text, agentID string) float64
}

func (r *fakeRoster) ListAgents(_ context.Context, _ string) ([]ExistingAgent, error) {
	return r.agents, nil
}

func (r *fakeRoster) FitScores(_ context.Context, text string, agents []ExistingAgent) ([]AgentFit, error) {
	out := make([]AgentFit, 0, len(agents))
	for _, a := range agents {
		s := 0.0
		if r.scoreFn != nil {
			s = r.scoreFn(text, a.ID)
		}
		out = append(out, AgentFit{AgentID: a.ID, Score: s})
	}
	return out, nil
}

// fakePlanWriter records every write so the test can assert the persisted
// plan + step DAG + decision observations.
type fakePlanWriter struct {
	plans     map[string]string // planID -> goal
	steps     map[string]NewStep
	assigned  map[string]string // stepID -> agentID
	decisions []Observation
}

func newFakePlanWriter() *fakePlanWriter {
	return &fakePlanWriter{
		plans:    map[string]string{},
		steps:    map[string]NewStep{},
		assigned: map[string]string{},
	}
}

func (w *fakePlanWriter) CreatePlan(_ context.Context, planID, goal string, _ map[string]any) error {
	w.plans[planID] = goal
	return nil
}

func (w *fakePlanWriter) AddStep(_ context.Context, step NewStep) error {
	w.steps[step.ID] = step
	return nil
}

func (w *fakePlanWriter) AssignAgent(_ context.Context, step NewStep, agentID string) error {
	w.assigned[step.ID] = agentID
	return nil
}

func (w *fakePlanWriter) RecordDecision(_ context.Context, obs Observation) error {
	w.decisions = append(w.decisions, obs)
	return nil
}

// fakeFactory records create/upgrade calls and mints deterministic ids.
type fakeFactory struct {
	created  []ComposedAgent
	upgraded map[string][2][]string // agentID -> {domains, tools}
	nextID   int
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{upgraded: map[string][2][]string{}}
}

func (f *fakeFactory) CreateAgent(_ context.Context, spec ComposedAgent, _, _ string) (string, error) {
	f.nextID++
	f.created = append(f.created, spec)
	return fmt.Sprintf("v1:agents:agent:new-%d", f.nextID), nil
}

func (f *fakeFactory) UpgradeAgent(_ context.Context, agentID string, newDomains, newTools []string, _ string) error {
	f.upgraded[agentID] = [2][]string{newDomains, newTools}
	return nil
}

func newTestPlanner(t *testing.T, d Decomposer, r AgentRoster, w PlanWriter, f AgentFactory) *Planner {
	t.Helper()
	p, err := NewPlanner(d, r, w, f, nil, PlannerConfig{})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// acceptance-criteria tests
// ---------------------------------------------------------------------------

// AC: a goal is decomposed into a persisted plan with a step DAG.
func TestPlan_PersistsPlanAndDAG(t *testing.T) {
	dec := stubDecomposer{steps: []PlanStep{
		{Key: "s1", Title: "research", Goal: "research X", RoleHint: "researcher"},
		{Key: "s2", Title: "build", Goal: "build Y", RoleHint: "builder", DependsOn: []string{"s1"}},
	}}
	w := newFakePlanWriter()
	p := newTestPlanner(t, dec, &fakeRoster{}, w, newFakeFactory())

	res, err := p.Plan(context.Background(), "owner-1", "do X then Y", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(w.plans) != 1 {
		t.Fatalf("want 1 plan persisted, got %d", len(w.plans))
	}
	if len(w.steps) != 2 {
		t.Fatalf("want 2 steps persisted, got %d", len(w.steps))
	}
	// The DAG edge must reference the persisted step id, not the local key.
	var s2 NewStep
	for _, s := range w.steps {
		if s.Title == "build" {
			s2 = s
		}
	}
	if len(s2.DependsOn) != 1 {
		t.Fatalf("s2 should depend on one step, got %v", s2.DependsOn)
	}
	if _, ok := w.steps[s2.DependsOn[0]]; !ok {
		t.Fatalf("s2 dependsOn %q is not a persisted step id", s2.DependsOn[0])
	}
	if res.PlanID == "" {
		t.Fatalf("PlanResult.PlanID empty")
	}
}

// AC: high-fit routes, low-fit provisions, partial-fit upgrades.
func TestPlan_RouteUpgradeProvision(t *testing.T) {
	// Three steps, one for each action.
	dec := stubDecomposer{steps: []PlanStep{
		{Key: "route", Title: "route step", Goal: "g-route"},
		{Key: "upg", Title: "upgrade step", Goal: "g-upg", RoleHint: "builder"},
		{Key: "prov", Title: "provision step", Goal: "g-prov", RoleHint: "builder"},
	}}
	roster := &fakeRoster{
		agents: []ExistingAgent{
			{ID: "agent-route", RoleSlug: "researcher", KnowledgeDomains: []string{"research-methods"}, Tools: []string{"workbenchHost"}},
			{ID: "agent-upg", RoleSlug: "builder", KnowledgeDomains: []string{"workbench"}, Tools: []string{"workbenchHost"}},
		},
		scoreFn: func(text, agentID string) float64 {
			// agent-route fits the route step highly; agent-upg fits the
			// upgrade step partially; nobody fits the provision step.
			switch {
			case strings.Contains(text, "g-route") && agentID == "agent-route":
				return 0.95
			case strings.Contains(text, "g-upg") && agentID == "agent-upg":
				return 0.70
			default:
				return 0.10
			}
		},
	}
	w := newFakePlanWriter()
	f := newFakeFactory()
	p := newTestPlanner(t, dec, roster, w, f)

	res, err := p.Plan(context.Background(), "owner-1", "mixed goal", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	byTitle := map[string]StepPlanResult{}
	for _, s := range res.Steps {
		byTitle[s.Title] = s
	}
	if got := byTitle["route step"]; got.Action != ActionRoute || got.AssignedAgent != "agent-route" {
		t.Fatalf("route step: got action=%q agent=%q", got.Action, got.AssignedAgent)
	}
	if got := byTitle["upgrade step"]; got.Action != ActionUpgrade || got.AssignedAgent != "agent-upg" {
		t.Fatalf("upgrade step: got action=%q agent=%q", got.Action, got.AssignedAgent)
	}
	if got := byTitle["provision step"]; got.Action != ActionProvision {
		t.Fatalf("provision step: got action=%q", got.Action)
	}

	// Upgrade attached the builder role's missing domain (software-engineering)
	// to agent-upg.
	up, ok := f.upgraded["agent-upg"]
	if !ok {
		t.Fatalf("agent-upg was not upgraded")
	}
	if !sliceContains(up[0], "software-engineering") {
		t.Fatalf("upgrade did not add software-engineering domain: %v", up[0])
	}

	// Provision created exactly one new agent (the builder for prov step).
	if len(f.created) != 1 {
		t.Fatalf("want 1 provisioned agent, got %d", len(f.created))
	}
	if f.created[0].RoleSlug != "builder" {
		t.Fatalf("provisioned role = %q, want builder", f.created[0].RoleSlug)
	}

	// All steps got assigned + a decision recorded.
	if len(w.assigned) < 2 {
		t.Fatalf("expected route + upgrade + provision assignments, got %d", len(w.assigned))
	}
}

// AC: provisioning a near-duplicate merges into the existing agent (dedup),
// preventing sprawl -- no new agent row is created.
func TestPlan_DedupMergeOnProvision(t *testing.T) {
	dec := stubDecomposer{steps: []PlanStep{
		{Key: "s1", Title: "novel step", Goal: "g-novel", RoleHint: "researcher"},
	}}
	// The step does not fit the existing agent (forces a provision
	// decision), BUT the freshly-composed agent's capability text is a
	// near-duplicate of the existing agent (forces a dedup merge).
	roster := &fakeRoster{
		agents: []ExistingAgent{
			{ID: "agent-dup", RoleSlug: "researcher", KnowledgeDomains: []string{"research-methods"}, Tools: []string{"workbenchHost"}},
		},
		scoreFn: func(text, agentID string) float64 {
			// Step text scores low (provision); composed-agent capability
			// text scores high (dedup merge). The composed researcher's
			// capability text contains "role: researcher".
			if strings.Contains(text, "role: researcher") {
				return 0.97 // near-duplicate
			}
			return 0.05 // step doesn't fit -> provision
		},
	}
	w := newFakePlanWriter()
	f := newFakeFactory()
	p := newTestPlanner(t, dec, roster, w, f)

	res, err := p.Plan(context.Background(), "owner-1", "novel goal", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("want 1 step result, got %d", len(res.Steps))
	}
	// Provision decision, but merged into the existing agent -- NO new row.
	if got := res.Steps[0]; got.Action != ActionProvision {
		t.Fatalf("action = %q, want provision", got.Action)
	}
	if len(f.created) != 0 {
		t.Fatalf("dedup should have merged -- no agent created, got %d", len(f.created))
	}
	if w.assigned[w.stepIDForTitle("novel step")] != "agent-dup" {
		t.Fatalf("step should be assigned to the merge target agent-dup, got %q",
			w.assigned[w.stepIDForTitle("novel step")])
	}
}

// AC: decisions (route/upgrade/provision) are recorded as `decision`
// observations for auditability.
func TestPlan_RecordsDecisionObservations(t *testing.T) {
	dec := stubDecomposer{steps: []PlanStep{
		{Key: "s1", Title: "one", Goal: "g1"},
		{Key: "s2", Title: "two", Goal: "g2"},
	}}
	w := newFakePlanWriter()
	p := newTestPlanner(t, dec, &fakeRoster{}, w, newFakeFactory())

	if _, err := p.Plan(context.Background(), "owner-1", "goal", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(w.decisions) != 2 {
		t.Fatalf("want 2 decision observations, got %d", len(w.decisions))
	}
	for _, obs := range w.decisions {
		if obs.Kind != "decision" {
			t.Fatalf("observation kind = %q, want decision", obs.Kind)
		}
		if obs.Data["choice"] == nil || obs.Data["rationale"] == nil {
			t.Fatalf("decision observation missing choice/rationale: %+v", obs.Data)
		}
	}
}

// The single-step fallback runs when the decomposer fails.
func TestPlan_DecomposeFailureFallsBack(t *testing.T) {
	dec := stubDecomposer{err: fmt.Errorf("LLM down")}
	w := newFakePlanWriter()
	p := newTestPlanner(t, dec, &fakeRoster{}, w, newFakeFactory())

	res, err := p.Plan(context.Background(), "owner-1", "salvage this goal", nil)
	if err != nil {
		t.Fatalf("Plan should not error on decompose failure: %v", err)
	}
	if len(w.steps) != 1 {
		t.Fatalf("fallback should persist exactly one step, got %d", len(w.steps))
	}
	if len(res.Steps) != 1 {
		t.Fatalf("want 1 step result, got %d", len(res.Steps))
	}
}

func TestNewPlanner_RequiresDeps(t *testing.T) {
	_, err := NewPlanner(nil, &fakeRoster{}, newFakePlanWriter(), newFakeFactory(), nil, PlannerConfig{})
	if err == nil {
		t.Fatalf("expected error when decomposer is nil")
	}
}

// stepIDForTitle finds the persisted step id for a given title (test helper).
func (w *fakePlanWriter) stepIDForTitle(title string) string {
	for id, s := range w.steps {
		if s.Title == title {
			return id
		}
	}
	return ""
}

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
