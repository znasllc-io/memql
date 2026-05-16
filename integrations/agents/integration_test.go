package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/visionarys-io/memql/component/memql"
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

func TestCapabilities_OneInvokeCapability(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	caps := i.Capabilities()
	if len(caps) != 1 {
		t.Fatalf("Capabilities count: got %d want 1", len(caps))
	}
	if caps[0].Name != "invoke" {
		t.Errorf("capability name: got %q want invoke", caps[0].Name)
	}
	// Required args schema entries.
	for _, key := range []string{"name", "utterance", "spaceContext", "history"} {
		if _, ok := caps[0].ArgsSchema[key]; !ok {
			t.Errorf("ArgsSchema missing %q", key)
		}
	}
}

func TestHandleInvoke_RequiresName(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleInvoke(context.Background(), map[string]any{"utterance": "hi"}, 0)
	if err == nil {
		t.Fatal("expected error when name is missing")
	}
	if !strings.Contains(err.Error(), "'name' argument is required") {
		t.Errorf("error should mention 'name', got: %v", err)
	}
}

func TestHandleInvoke_RequiresUtterance(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleInvoke(context.Background(), map[string]any{"name": "foo"}, 0)
	if err == nil {
		t.Fatal("expected error when utterance is missing")
	}
	if !strings.Contains(err.Error(), "'utterance' argument is required") {
		t.Errorf("error should mention 'utterance', got: %v", err)
	}
}

func TestHandleInvoke_UnregisteredAgent(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	_, err := i.handleInvoke(context.Background(), map[string]any{
		"name":      "missing",
		"utterance": "hi",
	}, 0)
	if err == nil {
		t.Fatal("expected error for unregistered agent name")
	}
	if !strings.Contains(err.Error(), "no agent registered") {
		t.Errorf("error should mention 'no agent registered', got: %v", err)
	}
}

func TestHandleInvoke_ResolvedAgent_ReturnsEnvelope(t *testing.T) {
	reg := memql.NewAgentRegistry()
	if err := reg.Upsert(&memql.AgentDefinition{
		Name:     "alpha",
		Role:     "general_assistant",
		RoleSlug: "general_assistant",
		Scope:    "perUser",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	i := New(reg, nil)

	nodes, err := i.handleInvoke(context.Background(), map[string]any{
		"name":      "alpha",
		"utterance": "ping",
	}, 0)
	if err != nil {
		t.Fatalf("handleInvoke: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 MemoryNode, got %d", len(nodes))
	}
	node := nodes[0]
	if node.Concept != "integration:agents:envelope" {
		t.Errorf("Concept: got %q", node.Concept)
	}

	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	resp, _ := payload["response"].(string)
	if !strings.Contains(resp, "alpha") || !strings.Contains(resp, "ping") {
		t.Errorf("response should reference agent name + utterance; got %q", resp)
	}
	// citations is an empty list (Phase 4 placeholder).
	cits, _ := payload["citations"].([]any)
	if len(cits) != 0 {
		t.Errorf("citations: got %d, want 0", len(cits))
	}
	// agent metadata sub-object.
	agentMeta, _ := payload["agent"].(map[string]any)
	if agentMeta["name"] != "alpha" || agentMeta["role"] != "general_assistant" {
		t.Errorf("agent metadata: %+v", agentMeta)
	}
}
