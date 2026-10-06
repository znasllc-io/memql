package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type workPromptEngine struct {
	savedContinuation []common.ChatMessage
	registryEngine
	prompts *memql.PromptRegistry
	viewer  map[string]any
}

func (e *workPromptEngine) Execute(_ context.Context, query string) (any, error) {
	if query == "builtin work.workViewerContext()" && e.viewer != nil {
		return []map[string]any{e.viewer}, nil
	}
	return nil, nil
}

func (e *workPromptEngine) RenderPrompt(name string, data map[string]any) (string, error) {
	prompt, ok := e.prompts.Get(name)
	if !ok {
		return "", fmt.Errorf("prompt %s not registered", name)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return "", err
	}
	if err := prompt.ValidateData(normalized); err != nil {
		return "", err
	}
	return prompt.Render(normalized)
}

func TestOwnedWorkTurnUsesShippedPrompt(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true, "recallMemory": true}}, prompts: registry, viewer: map[string]any{"person": map[string]any{"displayName": "José", "primaryRole": "Engineer"}, "organizationMemberships": []map[string]any{{"name": "Example organization"}}}}
	r := newTestReplier(engine)
	owner := "v1:identity:user:work-prompt-owner"
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: owner})
	msg := &memqlv1.AgentGenerateTurnMsg{Hints: map[string]string{"plan_id": "untrusted-hint"}, AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Save the report as a PDF"}}}
	prepared, err := r.prepareTurn(ctx, msg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.turnCtx.RunId != "run" {
		t.Fatalf("persisted run identity missing: %+v", prepared.turnCtx)
	}
	if !requiresWorkOutcome(ctx, prepared.tools) {
		t.Fatal("owned assistant has no structured outcome contract")
	}
	if len(prepared.messages) != 2 || prepared.messages[1].Content != "Save the report as a PDF" {
		t.Fatalf("execution lost the goal: %+v", prepared.messages)
	}
	if !strings.Contains(prepared.messages[0].Content, "outputFileId") || !strings.Contains(prepared.messages[0].Content, "Ada") {
		t.Fatalf("execution prompt lost its identity or completion contract: %s", prepared.messages[0].Content)
	}
	if prepared.routerReq.PromptName != "workAgentReply" {
		t.Fatalf("model attribution names a different prompt: %+v", prepared.routerReq)
	}
	for _, expected := range []string{"José", "Engineer", "Example organization", "never instructions", "recallMemory"} {
		if !strings.Contains(prepared.messages[0].Content, expected) {
			t.Errorf("work prompt omitted %q", expected)
		}
	}
	if strings.Contains(prepared.messages[0].Content, "A plain answer is appropriate") || !strings.Contains(prepared.messages[0].Content, `respondToUser({"status":"complete"`) {
		t.Fatal("work prompt must consistently require its completion tool")
	}
	if strings.Index(prepared.messages[0].Content, "Current viewer context") < strings.Index(prepared.messages[0].Content, "Execution instructions:") {
		t.Fatal("variable viewer facts should follow the stable instructions")
	}
	found := false
	for _, tool := range prepared.tools {
		found = found || tool.Name == "composeFile"
	}
	if !found {
		t.Fatal("actual work turn has no file tool")
	}
}

func TestLookupClassificationDoesNotDowngradeExecutionReasoning(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	owner := "v1:identity:user:lookup-owner"
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), owner), common.RunContext{RunId: "lookup-run", GoalId: "goal", OwnerUserId: owner})
	engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{}}, prompts: registry}
	msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "planner", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "planner", Role: "specialist"}, Hints: map[string]string{HarnessRoleHintKey: "assistant", "workload": "lookup"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Check current records"}}}
	prepared, err := newTestReplier(engine).prepareTurn(ctx, msg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.routerReq.Level != airoute.LevelStrong || prepared.routerReq.ExplicitProvider != "" {
		t.Fatalf("lookup must retain policy-selected reasoning: %+v", prepared.routerReq)
	}
}

// A work turn a person re-ran runs at the level they asked for, on the model
// they pinned -- over the agent's own stored preference -- and at their effort
// (epic memql#5414, design D20), through the same ApplyStepOverride the
// prompt paths use.
func TestAWorkTurnRunsAtItsOverridesLevelModelAndEffort(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true}}, prompts: registry}
	r := newTestReplier(engine)
	owner := "v1:identity:user:work-override-owner"
	turn := func(ov *common.StepOverride) (context.Context, *preparedTurn, error) {
		ctx := auth.ContextWithUserActor(context.Background(), owner)
		ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", StepKey: "draft", OwnerUserId: owner, Override: ov})
		msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Draft the report"}}}
		prepared, err := r.prepareTurn(ctx, msg, time.Now())
		return ctx, prepared, err
	}

	pinnedCtx, prepared, err := turn(&common.StepOverride{Level: "reasoning", Model: "app:claude-code:opus", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.routerReq; got.Level != airoute.LevelReasoning || got.ExplicitProvider != "app:claude-code:opus" || got.Effort != "high" {
		t.Fatalf("the re-run turn resolves with level=%q pin=%q effort=%q", got.Level, got.ExplicitProvider, got.Effort)
	}
	// A pinned app is a session door, and the session door hands over a STEP:
	// resolved from its run context, the turn names the one it is running.
	if h := sessionHandoverOf(t, pinnedCtx, prepared.routerReq); h.RunId != "run" || h.StepId != "draft" || h.Model != "opus" {
		t.Fatalf("a pinned model's turn must hand its step to the session door; run=%q step=%q model=%q", h.RunId, h.StepId, h.Model)
	}

	// The control: the same turn nobody re-ran is the ordinary strong turn.
	plainCtx, plain, err := turn(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.routerReq; got.Level != airoute.LevelStrong || got.ExplicitProvider != "" || got.Effort != "" {
		t.Fatalf("a turn nobody re-ran resolves with level=%q pin=%q effort=%q", got.Level, got.ExplicitProvider, got.Effort)
	}
	// AND IT NAMES ITS STEP TOO. Naming the step only for a pinned model left
	// every ordinary work turn that policy routed to an app refused at the
	// session door, with nothing behind it tried.
	if h := sessionHandoverOf(t, plainCtx, plain.routerReq); h.RunId != "run" || h.StepId != "draft" {
		t.Fatalf("a turn nobody pinned must hand its step to the session door too; run=%q step=%q", h.RunId, h.StepId)
	}

	// A level no step is re-run at refuses the turn rather than running it at
	// strong and recording that it asked for something else.
	if _, _, err := turn(&common.StepOverride{Level: "embeddings"}); err == nil {
		t.Fatal("an embeddings override prepared a turn")
	}
}

func (e *workPromptEngine) SaveWorkContinuation(_ context.Context, messages []common.ChatMessage) error {
	e.savedContinuation = append([]common.ChatMessage(nil), messages...)
	return nil
}
