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
	if len(caps) != 5 {
		t.Fatalf("Capabilities count: got %d want 5 (invoke + ensureForGoal + askSpecialist + requestUserFeedback + produceArtifact)", len(caps))
	}
	byName := make(map[string]bool, len(caps))
	for _, c := range caps {
		byName[c.Name] = true
		// Required args schema entries for the async-creates-Plan contract.
		if c.Name == "invoke" {
			for _, key := range []string{"name", "prompt", "spaceId"} {
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
			for _, key := range []string{"goal", "ownerUserId", "spaceId"} {
				if _, ok := c.ArgsSchema[key]; !ok {
					t.Errorf("produceArtifact ArgsSchema missing %q", key)
				}
			}
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
// wired engine: missing goal, ownerUserId, and spaceId all fail before the
// engine.Execute call (the engine-nil check sits after arg validation, as in
// requestUserFeedback). The success path (mints a Plan via mutationCreatePlan)
// needs a wired engine + database and is exercised by the cluster, not here.

func TestHandleProduceArtifact_RequiresGoal(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"ownerUserId": "u1",
		"spaceId":     "s1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'goal' is required") {
		t.Fatalf("expected 'goal' is required error, got: %v", err)
	}
}

func TestHandleProduceArtifact_RequiresOwnerUserId(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":    "A markdown file listing 10 birds",
		"spaceId": "s1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'ownerUserId' required") {
		t.Fatalf("expected 'ownerUserId' required error, got: %v", err)
	}
}

func TestHandleProduceArtifact_RequiresSpaceId(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleProduceArtifact(context.Background(), map[string]any{
		"goal":        "A markdown file listing 10 birds",
		"ownerUserId": "u1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "'spaceId' required") {
		t.Fatalf("expected 'spaceId' required error, got: %v", err)
	}
}

// handleRequestUserFeedback's early validation paths are testable
// without a wired engine: missing question, missing/invalid kind, and
// missing planId all fail before the engine.Execute call. The success
// path (which transitions a Plan via mutationRequestPlanFeedback) needs
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
	_, err := i.handleInvoke(context.Background(), map[string]any{"prompt": "hi", "spaceId": "s1"}, 0)
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
		"name":    "missing",
		"prompt":  "hi",
		"spaceId": "s1",
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
