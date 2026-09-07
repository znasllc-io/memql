package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestNew_NilRegistryReturnsNil(t *testing.T) {
	if got := New(nil, nil); got != nil {
		t.Errorf("New(nil, nil) should return nil, got %+v", got)
	}
}

func TestIntegrationName(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	if i.IntegrationName() != "agents" {
		t.Errorf("IntegrationName: got %q want agents", i.IntegrationName())
	}
}

func TestCapabilities_InvokeEnsureForGoalAskSpecialist(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	caps := i.Capabilities()
	// A COUNT PLUS THE NAMES, and the count is the half that catches a
	// capability arriving under a name nobody thought to assert.
	want := []string{"invoke", "runAgentTurn", "ensureForGoal", "askSpecialist", "requestUserFeedback", "produceArtifact"}
	if len(caps) != len(want) {
		got := make([]string, 0, len(caps))
		for _, c := range caps {
			got = append(got, c.Name)
		}
		t.Fatalf("Capabilities count: got %d %v, want %d %v", len(caps), got, len(want), want)
	}
	byName := make(map[string]bool, len(caps))
	for _, c := range caps {
		byName[c.Name] = true
		// Required args schema entries for the async-creates-Plan contract.
		if c.Name == "invoke" {
			for _, key := range []string{"name", "prompt", "partitionId"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("invoke ArgsSchema missing %q", key)
				}
			}
			// Removed args should not appear (old synchronous contract).
			for _, key := range []string{"utterance", "spaceContext", "history"} {
				if _, ok := c.ArgsSchema[key]; ok {
					t.Errorf("invoke ArgsSchema unexpectedly still has %q (old sync contract)", key)
				}
			}
		}
		if c.Name == "ensureForGoal" {
			for _, key := range []string{"goal", "ownerUserId"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("ensureForGoal ArgsSchema missing %q", key)
				}
			}
		}
		if c.Name == "askSpecialist" {
			for _, key := range []string{"role", "query"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("askSpecialist ArgsSchema missing %q", key)
				}
			}
		}
		if c.Name == "requestUserFeedback" {
			for _, key := range []string{"question", "kind", "planId"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("requestUserFeedback ArgsSchema missing %q", key)
				}
			}
		}
		if c.Name == "produceArtifact" {
			for _, key := range []string{"goal", "ownerUserId", "partitionId"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("produceArtifact ArgsSchema missing %q", key)
				}
			}
		}
	}
	for _, name := range want {
		if !byName[name] {
			t.Errorf("missing %q capability", name)
		}
	}
	if !byName["invoke"] {
		t.Error("missing 'invoke' capability")
	}
	if !byName["ensureForGoal"] {
		t.Error("missing 'ensureForGoal' capability")
	}
	if !byName["askSpecialist"] {
		t.Error("missing 'askSpecialist' capability")
	}
	if !byName["requestUserFeedback"] {
		t.Error("missing 'requestUserFeedback' capability")
	}
	if !byName["produceArtifact"] {
		t.Error("missing 'produceArtifact' capability")
	}
}

// handleProduceArtifact's early validation paths are testable without a
// wired engine: missing goal, ownerUserId, and partitionId all fail before the
// engine.Execute call (the engine-nil check sits after arg validation, as in
// requestUserFeedback). The success path (mints a Plan via createPlan)
// needs a wired engine + database and is exercised by the cluster, not here.

// recordingEngine embeds IntegrationEngineAccess (so the few methods the
// handler doesn't touch are nil and would panic on use, surfacing any
// accidental new dependency) and records every Execute call. Used to assert
// the produceArtifact mint path issues EXACTLY ONE createPlan from a
// normal (non-produceArtifact) context (memql#1133 FIX-2 contract).
type recordingEngine struct {
	memql.IntegrationEngineAccess
	calls []string
}

func (e *recordingEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	e.calls = append(e.calls, query)
	return &memql.ExecuteResult{}, nil
}

// fakeWorkGoals records the goals the two entry points open.
type fakeWorkGoals struct {
	opened []DirectGoal
	err    error
}

func (f *fakeWorkGoals) OpenDirectGoal(_ context.Context, g DirectGoal) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.opened = append(f.opened, g)
	return "v1:work:goal:g1", "v1:work:run:r1", nil
}

// TestHandleProduceArtifact_OpensExactlyOneGoal is the memql#1133 FIX-2
// counterpart to the agent-side re-delegation guard, re-pointed onto the work
// spine (memql#5048): a produceArtifact invocation from a NORMAL context opens
// exactly ONE goal. The depth-1 cap lives on the agent turn loop (it refuses a
// SECOND produceArtifact from within a produceArtifact executor turn), so the
// path itself is unconditional -- this pins that the first delegation is
// unaffected.
func TestHandleProduceArtifact_OpensExactlyOneGoal(t *testing.T) {
	eng := &recordingEngine{}
	i := New(memql.NewAgentRegistry(), eng)
	goals := &fakeWorkGoals{}
	i.SetWorkGoals(goals)

	nodes, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":        "A markdown file listing 10 beautiful birds",
		"ownerUserId": "u1",
		"partitionId": "s1",
	}, 0)
	if err != nil {
		t.Fatalf("handleProduceArtifact (normal context): unexpected error: %v", err)
	}
	if len(goals.opened) != 1 {
		t.Fatalf("expected EXACTLY ONE goal opened, got %d: %+v", len(goals.opened), goals.opened)
	}
	g := goals.opened[0]
	if g.AutomationName != templateProduceArtifact {
		t.Errorf("goal named automation %q, want %q", g.AutomationName, templateProduceArtifact)
	}
	if g.OwnerUserId != "u1" {
		t.Errorf("goal owner %q, want u1", g.OwnerUserId)
	}
	// NO Plan write, and no graph write at all: opening the goal is the work
	// integration's job, through the seam.
	if len(eng.calls) != 0 {
		t.Errorf("the handler wrote to the engine directly: %v", eng.calls)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one ack envelope node, got %d", len(nodes))
	}
}

// TestHandleProduceArtifact_RefusesWithNoGoalSurface: the failure this whole
// change exists to end is returning an id for work nothing executed. An ack
// for a goal that was never opened is that failure with a new name, so the
// handler refuses instead.
func TestHandleProduceArtifact_RefusesWithNoGoalSurface(t *testing.T) {
	i := New(memql.NewAgentRegistry(), &recordingEngine{})
	// deliberately no SetWorkGoals

	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":        "A markdown file listing 10 birds",
		"ownerUserId": "u1",
		"partitionId": "s1",
	}, 0)
	if err == nil {
		t.Fatal("handleProduceArtifact acked with no work-goal surface installed")
	}
	if !strings.Contains(err.Error(), "work-goal surface") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

func TestHandleProduceArtifact_RequiresGoal(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"ownerUserId": "u1",
		"partitionId": "s1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'goal' is required") {
		t.Fatalf("expected 'goal' is required error, got: %v", err)
	}
}

func TestHandleProduceArtifact_RequiresOwnerUserId(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":        "A markdown file listing 10 birds",
		"partitionId": "s1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'ownerUserId' required") {
		t.Fatalf("expected 'ownerUserId' required error, got: %v", err)
	}
}

func TestHandleProduceArtifact_RequiresPartitionId(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":        "A markdown file listing 10 birds",
		"ownerUserId": "u1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'partitionId' required") {
		t.Fatalf("expected 'partitionId' required error, got: %v", err)
	}
}

// handleRequestUserFeedback's early validation paths are testable
// without a wired engine: missing question, missing/invalid kind, and
// missing planId all fail before the engine.Execute call. The success
// path (which transitions a Plan via requestPlanFeedback) needs
// a wired engine + database and is exercised by the planner
// integration tests, not here.

func TestHandleRequestUserFeedback_RequiresQuestion(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleRequestUserFeedback(context.Background(), map[string]any{
		"kind":   "text",
		"planId": "plan-1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'question' is required") {
		t.Fatalf("expected 'question' is required error, got: %v", err)
	}
}

func TestHandleRequestUserFeedback_RejectsBadKind(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleRequestUserFeedback(context.Background(), map[string]any{
		"question": "Which quarter?",
		"kind":     "freeform",
		"planId":   "plan-1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "must be choice / text / multi") {
		t.Fatalf("expected bad-kind error, got: %v", err)
	}
}

func TestHandleRequestUserFeedback_RequiresPlanId(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleRequestUserFeedback(context.Background(), map[string]any{
		"question": "Which quarter?",
		"kind":     "text",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'planId' required") {
		t.Fatalf("expected missing-planId error, got: %v", err)
	}
}

// The handler's three early error paths are testable without a real
// engine handle: registry-nil, missing required args, and missing
// agent registration. The success path (which mints a Plan via
// engine.Execute) needs a wired engine + database; that lives behind
// an integration test that the planner integration's tests will pick
// up, not here. Keeping the unit tests focused on contract-shape
// assertions keeps them fast and free of database fixture overhead.

func TestHandleInvoke_RequiresName(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleInvoke(context.Background(), map[string]any{"prompt": "hi", "partitionId": "s1"}, 0)
	if err == nil {
		t.Fatal("expected error when name is missing")
	}
	// Missing name fails before the engine-handle check, so this is
	// directly the 'name' arg error.
	if !strings.Contains(err.Error(), "'name' argument is required") &&
		!strings.Contains(err.Error(), "engine handle missing") {
		t.Errorf("error should mention 'name' or engine missing, got: %v", err)
	}
}

func TestHandleInvoke_UnregisteredAgent(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleInvoke(context.Background(), map[string]any{
		"name":        "missing",
		"prompt":      "hi",
		"partitionId": "s1",
	}, 0)
	if err == nil {
		t.Fatal("expected error for unregistered agent name or missing engine")
	}
	// With engine=nil the handler errors at the engine-handle check
	// before getting to the registry lookup. Either failure shape is
	// acceptable here; the assertion is that we DO return an error.
	if !strings.Contains(err.Error(), "engine handle missing") &&
		!strings.Contains(err.Error(), "no agent registered") {
		t.Errorf("error should mention engine missing or 'no agent registered', got: %v", err)
	}
}
