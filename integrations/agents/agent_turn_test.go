package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
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

func (f *fakeTurnRunner) ResolveOwnerAgent(_ context.Context, owner string) (string, error) {
	return "reasoning-" + owner, nil
}

func TestRunAgentTurnDefaultsOnlyToItsAuthenticatedRunOwner(t *testing.T) {
	runner := &fakeTurnRunner{reply: "evidence"}
	i := &Integration{}
	i.SetAgentTurnRunner(runner)
	ctx := auth.ContextWithUserActor(context.Background(), "owner")
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "research"})
	if _, err := i.handleRunAgentTurn(ctx, map[string]any{"prompt": "Collect evidence"}, 0); err != nil {
		t.Fatal(err)
	}
	if runner.saw[0].AgentId != "reasoning-owner" {
		t.Fatal("did not use the owner's shared reasoning agent")
	}
	ctx = auth.ContextWithUserActor(ctx, "another-owner")
	if _, err := i.handleRunAgentTurn(ctx, map[string]any{"prompt": "Collect evidence"}, 0); err == nil {
		t.Fatal("borrowed another owner's reasoning agent")
	}
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

type receiptEngine struct {
	memql.IntegrationEngineAccess
	rows  []map[string]any
	err   error
	query string
	owner string
}

func (e *receiptEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.query = query
	ac, _ := auth.AccessFromContext(ctx)
	if ac != nil {
		e.owner = ac.UserId
	}
	return memql.NewResultWithOutput(e.rows), e.err
}

func TestRunAgentTurnRequiresAnOwnedReadyFileOnlyWhenRequested(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "owner")
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "assemble"})
	for _, tc := range []struct {
		name    string
		row     map[string]any
		failure bool
	}{
		{name: "plain success is insufficient", failure: true},
		{name: "ready owned output", row: map[string]any{"id": "v1:library:file:file", "ownerUserId": "v1:identity:user:owner", "producedByRunId": "v1:work:run:run", "producedByStepKey": "assemble", "status": "ready", "blobUrl": "https://files/output"}},
		{name: "earlier branch file", row: map[string]any{"id": "file", "ownerUserId": "owner", "producedByRunId": "run", "producedByStepKey": "sections.one", "status": "ready", "blobUrl": "https://files/output"}, failure: true},
		{name: "missing step receipt", row: map[string]any{"id": "file", "ownerUserId": "owner", "producedByRunId": "run", "status": "ready", "blobUrl": "https://files/output"}, failure: true},
		{name: "different run", row: map[string]any{"id": "file", "ownerUserId": "owner", "producedByRunId": "other", "status": "ready", "blobUrl": "https://files/output"}, failure: true},
		{name: "different owner", row: map[string]any{"id": "file", "ownerUserId": "other", "producedByRunId": "run", "status": "ready", "blobUrl": "https://files/output"}, failure: true},
		{name: "not ready", row: map[string]any{"id": "file", "ownerUserId": "owner", "producedByRunId": "run", "status": "processing", "blobUrl": "https://files/output"}, failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &receiptEngine{}
			if tc.row != nil {
				engine.rows = []map[string]any{tc.row}
			}
			i := &Integration{engine: engine}
			i.SetAgentTurnRunner(&fakeTurnRunner{reply: "Done"})
			nodes, err := i.handleRunAgentTurn(ctx, map[string]any{"agentId": "agent", "prompt": "Save a file", "requireFile": true}, 0)
			if tc.failure {
				if err == nil {
					t.Fatal("accepted a completion without an owned ready file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["outputFileId"] != "file" || engine.owner != "owner" || !strings.Contains(engine.query, "libraryFilesForOwner") {
				t.Fatalf("missing owned file receipt: %v query=%s owner=%s", payload, engine.query, engine.owner)
			}
		})
	}
}

func TestRunAgentTurnFileReceiptRequiresAStepBeforeStarting(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "owner")
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	engine := &receiptEngine{rows: []map[string]any{{"id": "file", "ownerUserId": "owner", "producedByRunId": "run", "status": "ready", "blobUrl": "https://files/output"}}}
	i := &Integration{engine: engine}
	runner := &fakeTurnRunner{reply: "Done"}
	i.SetAgentTurnRunner(runner)
	if _, err := i.handleRunAgentTurn(ctx, map[string]any{"agentId": "agent", "prompt": "save", "requireFile": true}, 0); err == nil || len(runner.saw) != 0 {
		t.Fatalf("missing step dispatched a file turn: err=%v calls=%d", err, len(runner.saw))
	}
}

func TestRunAgentTurnReplayFileReceiptUsesSameGoalAndStep(t *testing.T) {
	for _, tc := range []struct {
		name, sourceGoal, producedStep string
		wantError                      bool
	}{
		{name: "same source effect", sourceGoal: "v1:work:goal:goal", producedStep: "assemble"},
		{name: "foreign source goal", sourceGoal: "other", producedStep: "assemble", wantError: true},
		{name: "earlier source branch", sourceGoal: "goal", producedStep: "sections.partial", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.ContextWithUserActor(context.Background(), "owner")
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "replay", GoalId: "goal", OwnerUserId: "owner", StepKey: "assemble", Mode: common.RunModeReplay, SourceRunId: "v1:work:run:source", SourceGoalId: tc.sourceGoal})
			engine := &receiptEngine{rows: []map[string]any{{"id": "file", "ownerUserId": "owner", "producedByRunId": "source", "producedByStepKey": tc.producedStep, "status": "ready", "blobUrl": "https://files/output"}}}
			i := &Integration{engine: engine}
			i.SetAgentTurnRunner(&fakeTurnRunner{reply: "Done"})
			_, err := i.handleRunAgentTurn(ctx, map[string]any{"agentId": "agent", "prompt": "save", "requireFile": true}, 0)
			if (err != nil) != tc.wantError {
				t.Fatalf("replay receipt err=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
