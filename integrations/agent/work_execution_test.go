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

// roleGatedWorkEngine is repositoryWorkEngine answering, as app's engine
// adapter does, the agent roles each tool admits.
type roleGatedWorkEngine struct{ repositoryWorkEngine }

func (e roleGatedWorkEngine) ToolAgentRoles(name string) []string {
	tool, err := e.native.Tools().Get(name)
	if err != nil || tool == nil {
		return nil
	}
	return tool.RequiresAgentRole
}

// TestOwnedWorkOffersTheEmailCompositionToolsToTheAssistant: Ask's goals are
// owned work, and an assistant's own tools come from its skills, so the two
// email composition tools are offered with the rest of owned work's installed
// tools -- the shipped ones, each once -- on a turn whose acting agent may call
// them. They are the assistant's: a specialist's turn is offered neither,
// while a tool both roles may call is offered on both. A turn that names no
// acting role keeps the list as it was; the tool gate judges each call.
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
	r := newTestReplier(roleGatedWorkEngine{repositoryWorkEngine{native: e}})
	base := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleWriter})
	ctx := common.ContextWithRun(base, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	email := []string{"emailLayoutCatalogue", "startEmailComposition"}
	for _, tc := range []struct {
		role  string
		email int // how many times each email tool is offered
	}{
		{"assistant", 1},
		{"specialist", 0},
		{"", 1},
	} {
		assistant := map[string]any{}
		if tc.role != "" {
			assistant["role"] = tc.role
		}
		data := map[string]any{"assistant": assistant}
		names := r.scopeWorkExecution(ctx, data, []string{"recall"})
		for _, name := range email {
			if count := countToolName(names, name); count != tc.email {
				t.Errorf("acting role %q: %s offered %d times in %v, want %d", tc.role, name, count, names, tc.email)
			}
		}
		for _, name := range []string{"requestUserFeedback", "discoverCapabilities", "recall"} {
			if count := countToolName(names, name); count != 1 {
				t.Errorf("acting role %q: %s offered %d times in %v, want once", tc.role, name, count, names)
			}
		}
		if !slices.Equal(assistant["tools"].([]string), names) {
			t.Errorf("acting role %q: rendered assistant does not receive the same tool names", tc.role)
		}
	}
	// An assistant whose own tools already hold one is offered it once.
	names := r.scopeWorkExecution(ctx, map[string]any{"assistant": map[string]any{"role": "assistant"}}, []string{"startEmailComposition"})
	if countToolName(names, "startEmailComposition") != 1 {
		t.Errorf("startEmailComposition offered %d times in %v, want once", countToolName(names, "startEmailComposition"), names)
	}
	for _, name := range email {
		if len(e.ToolDefinitionsForNames([]string{name})) != 1 {
			t.Fatalf("%s has no model-visible contract", name)
		}
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
}
