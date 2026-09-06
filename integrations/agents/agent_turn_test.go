package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// agent_turn_test.go -- runAgentTurn (memql#5048).
//
// The property that matters is the one a registered-but-unwired plug-in gets
// wrong: a capability that RESOLVES is not a capability that works. Every case
// here drives the handler, not the registration.

type fakeTurnRunner struct {
	reply string
	err   error
	saw   []*memqlv1.AgentGenerateTurnMsg
}

func (f *fakeTurnRunner) RunTurn(_ context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	f.saw = append(f.saw, msg)
	return f.reply, f.err
}

func TestRunAgentTurn_ReturnsTheReply(t *testing.T) {
	i := &Integration{}
	runner := &fakeTurnRunner{reply: "the answer"}
	i.SetAgentTurnRunner(runner)

	nodes, err := i.handleRunAgentTurn(context.Background(), map[string]any{
		"agentId": "v1:agents:agent:a1", "prompt": "summarize this", "scopeId": "s1",
	}, 0)
	if err != nil {
		t.Fatalf("handleRunAgentTurn: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want one envelope, got %d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if payload["reply"] != "the answer" {
		t.Errorf("reply = %v", payload["reply"])
	}
	if payload["agentId"] != "v1:agents:agent:a1" {
		t.Errorf("agentId = %v", payload["agentId"])
	}

	// The turn the runtime actually saw. `agent()` mints a Plan and lets the
	// planner build this message; here it is built directly, so the shape is
	// this handler's responsibility.
	if len(runner.saw) != 1 {
		t.Fatalf("the runtime saw %d turns, want 1", len(runner.saw))
	}
	msg := runner.saw[0]
	if msg.AgentId != "v1:agents:agent:a1" || msg.ScopeId != "s1" {
		t.Errorf("turn addressed wrongly: agentId=%q scopeId=%q", msg.AgentId, msg.ScopeId)
	}
	if len(msg.History) != 1 || msg.History[0].Role != "user" || msg.History[0].Content != "summarize this" {
		t.Errorf("the prompt did not reach the turn as a user message: %+v", msg.History)
	}
	if strings.TrimSpace(msg.RequestId) == "" {
		t.Error("the turn carries no request id, so nothing downstream can address it")
	}
}

// A CAPABILITY THAT RESOLVES IS NOT A CAPABILITY THAT WORKS. integrations/agents
// is a CORE plug-in -- it loads on identity, edge, mcp and every other node
// type, none of which has an agent runtime. The refusal has to NAME that,
// because an empty reply is indistinguishable from an agent that had nothing
// to say.
func TestRunAgentTurn_WithNoRuntimeRefusesAndSaysWhy(t *testing.T) {
	i := &Integration{}
	_, err := i.handleRunAgentTurn(context.Background(), map[string]any{
		"agentId": "v1:agents:agent:a1", "prompt": "x",
	}, 0)
	if err == nil {
		t.Fatal("a node with no agent runtime answered a turn")
	}
	if !strings.Contains(err.Error(), "agent node") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

func TestRunAgentTurn_RefusesAnIncompleteCall(t *testing.T) {
	i := &Integration{}
	i.SetAgentTurnRunner(&fakeTurnRunner{reply: "x"})
	for _, args := range []map[string]any{
		{"prompt": "x"},
		{"agentId": "a1"},
		{"agentId": "  ", "prompt": "x"},
	} {
		if _, err := i.handleRunAgentTurn(context.Background(), args, 0); err == nil {
			t.Errorf("accepted an incomplete call: %+v", args)
		}
	}
}

// A runtime error reaches the caller rather than becoming an empty reply --
// the step has to fail so the run's journal records that it did.
func TestRunAgentTurn_ARuntimeErrorIsNotAnEmptyReply(t *testing.T) {
	i := &Integration{}
	boom := errors.New("no provider available")
	i.SetAgentTurnRunner(&fakeTurnRunner{err: boom})
	_, err := i.handleRunAgentTurn(context.Background(), map[string]any{
		"agentId": "a1", "prompt": "x",
	}, 0)
	if err == nil {
		t.Fatal("a failed turn returned no error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the runtime's error did not reach the caller: %v", err)
	}
}

// The capability has to be REGISTERED, or the builtin resolves to nothing and
// a work step fails with "function not found" -- which reads like a DSL
// problem rather than a wiring one.
func TestRunAgentTurnIsRegistered(t *testing.T) {
	i := &Integration{}
	for _, c := range i.Capabilities() {
		if c.Name == "runAgentTurn" {
			if c.Handler == nil {
				t.Fatal("runAgentTurn is registered with a nil handler")
			}
			for _, arg := range []string{"agentId", "prompt"} {
				if _, ok := c.ArgsSchema[arg]; !ok {
					t.Errorf("the capability does not declare %q", arg)
				}
			}
			return
		}
	}
	t.Fatal("runAgentTurn is not in Capabilities(); the DSL builtin would resolve to nothing")
}
