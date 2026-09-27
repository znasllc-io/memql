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
	registryEngine
	prompts *memql.PromptRegistry
}

func (e *workPromptEngine) Execute(context.Context, string) (any, error) { return nil, nil }

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
	engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true}}, prompts: registry}
	r := newTestReplier(engine)
	owner := "v1:identity:user:work-prompt-owner"
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: owner})
	msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Save the report as a PDF"}}}
	prepared, err := r.prepareTurn(ctx, msg, time.Now())
	if err != nil {
		t.Fatal(err)
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
	found := false
	for _, tool := range prepared.tools {
		found = found || tool.Name == "composeFile"
	}
	if !found {
		t.Fatal("actual work turn has no file tool")
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
	turn := func(ov *common.StepOverride) (*preparedTurn, error) {
		ctx := auth.ContextWithUserActor(context.Background(), owner)
		ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", StepKey: "draft", OwnerUserId: owner, Override: ov})
		msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Draft the report"}}}
		return r.prepareTurn(ctx, msg, time.Now())
	}

	prepared, err := turn(&common.StepOverride{Level: "reasoning", Model: "app:claude-code:opus", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.routerReq; got.Level != airoute.LevelReasoning || got.ExplicitProvider != "app:claude-code:opus" || got.Effort != "high" {
		t.Fatalf("the re-run turn resolves with level=%q pin=%q effort=%q", got.Level, got.ExplicitProvider, got.Effort)
	}
	// A pinned app is a session door, and the session door hands over a STEP:
	// without the run and step on the request it refuses at resolution.
	if got := prepared.routerReq; got.RunId != "run" || got.StepId != "draft" {
		t.Fatalf("a pinned model's turn must name its step for the session door; run=%q step=%q", got.RunId, got.StepId)
	}

	// The control: the same turn nobody re-ran is the ordinary strong turn.
	plain, err := turn(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.routerReq; got.Level != airoute.LevelStrong || got.ExplicitProvider != "" || got.Effort != "" {
		t.Fatalf("a turn nobody re-ran resolves with level=%q pin=%q effort=%q", got.Level, got.ExplicitProvider, got.Effort)
	}
	if got := plain.routerReq; got.RunId != "" || got.StepId != "" {
		t.Fatalf("default routing of a turn nobody pinned is unchanged; run=%q step=%q", got.RunId, got.StepId)
	}

	// A level no step is re-run at refuses the turn rather than running it at
	// strong and recording that it asked for something else.
	if _, err := turn(&common.StepOverride{Level: "embeddings"}); err == nil {
		t.Fatal("an embeddings override prepared a turn")
	}
}
