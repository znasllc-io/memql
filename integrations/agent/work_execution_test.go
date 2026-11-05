package agent

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

func TestWorkExecutionCreatesFilesWithoutRedelegating(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner"})
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	r := newTestReplier(&registryEngine{registered: map[string]bool{"composeFile": true}})
	data := map[string]any{"assistant": map[string]any{}, "productionDirective": "delegate everything"}
	names := r.scopeWorkExecution(ctx, data, []string{"produceArtifact", "canvasPublish", "recall"})
	if !slices.Contains(names, "composeFile") || slices.Contains(names, "produceArtifact") || slices.Contains(names, "canvasPublish") || !slices.Contains(names, "recall") {
		t.Fatalf("executor tool surface = %v", names)
	}
	if !strings.Contains(data["productionDirective"].(string), "outputFileId") {
		t.Fatalf("missing instruction to finish the actual file: %v", data)
	}
	if refusal := guardProduceArtifactRedelegation("produceArtifact", turnContext{IsWorkExecution: true}); !strings.Contains(refusal, "composeFile") {
		t.Fatalf("executor can open another goal: %q", refusal)
	}
}

func TestWorkExecutionRequiresThePersistedOwnerContext(t *testing.T) {
	r := newTestReplier(&registryEngine{registered: map[string]bool{"composeFile": true}})
	for _, ctx := range []context.Context{
		context.Background(),
		common.ContextWithRun(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "stranger"}), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"}),
	} {
		data := map[string]any{}
		names := r.scopeWorkExecution(ctx, data, []string{"produceArtifact"})
		if !slices.Equal(names, []string{"produceArtifact"}) || len(data) != 0 {
			t.Fatalf("ordinary or different-owner turn acquired executor tools: %v %v", names, data)
		}
	}
}

// TestOwnedWorkOffersTheEmailCompositionToolsToTheAssistant: Ask's goals are
// owned work, and an assistant's own tools come from its skills, so the two
// email composition tools are offered with the rest of owned work's installed
// tools -- the shipped ones, each once -- and keep their gate: the assistant
// may call them, a specialist and a person over MCP may not.
func TestOwnedWorkOffersTheEmailCompositionToolsToTheAssistant(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	r := newTestReplier(repositoryWorkEngine{native: e})
	base := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleWriter})
	ctx := common.ContextWithRun(base, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	data := map[string]any{"assistant": map[string]any{}}
	names := r.scopeWorkExecution(ctx, data, []string{"startEmailComposition"})
	for _, name := range []string{"emailLayoutCatalogue", "startEmailComposition", "requestUserFeedback"} {
		if count := countToolName(names, name); count != 1 {
			t.Fatalf("owned Work tool %s count=%d in %v", name, count, names)
		}
		if len(e.ToolDefinitionsForNames([]string{name})) != 1 {
			t.Fatalf("%s has no model-visible contract", name)
		}
	}
	for _, name := range []string{"emailLayoutCatalogue", "startEmailComposition"} {
		tool := e.Tools().LookupIndex()[name]
		if tool == nil {
			t.Fatalf("missing shipped tool %s", name)
		}
		if err := e.ToolCallRefusal(memql.WithActingAgentRole(ctx, "assistant"), tool); err != nil {
			t.Fatalf("%s refused the assistant: %v", name, err)
		}
		for _, refused := range []context.Context{memql.WithActingAgentRole(ctx, "specialist"), memql.WithMCPHumanCaller(ctx, "owner")} {
			if err := e.ToolCallRefusal(refused, tool); err == nil {
				t.Fatalf("%s lost its assistant-only gate", name)
			}
		}
	}
	if !slices.Equal(data["assistant"].(map[string]any)["tools"].([]string), names) {
		t.Fatal("rendered assistant does not receive the same tool names")
	}
}
