package agent

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

// Only the definition-read seam is needed by scopeWorkExecution. The native
// registry supplies it; no fake permissive tool or handler is installed.
type repositoryWorkEngine struct {
	MemQLEngine
	native *memql.MemQLEngine
}

func (e repositoryWorkEngine) ToolDefinitionsForNames(names []string) []common.ToolDefinition {
	return e.native.ToolDefinitionsForNames(names)
}

// Load the actual shipped tools, not aliases invented by the test. Owned Work
// must have these bounded request contracts even without a software skill.
func TestOwnedWorkOffersShippedRepositoryToolsWithTheirRoleGates(t *testing.T) {
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
	base := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	ctx := common.ContextWithRun(base, common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	data := map[string]any{"assistant": map[string]any{}}
	names := r.scopeWorkExecution(ctx, data, []string{"requestRepositoryChange"})
	for _, name := range []string{"inspectRepositoryAuthority", "requestRepositoryChange", "repositoryChangeRequestStatus", "listRepositoryChangeRequests"} {
		if count := countToolName(names, name); count != 1 {
			t.Fatalf("owned Work tool %s count=%d in %v", name, count, names)
		}
		if len(e.ToolDefinitionsForNames([]string{name})) != 1 {
			t.Fatalf("%s has no actual model-visible contract", name)
		}
		tool := e.Tools().LookupIndex()[name]
		if tool == nil {
			t.Fatalf("missing shipped tool %s", name)
		}
		for _, role := range []string{"assistant", "specialist"} {
			if err := e.ToolCallRefusal(memql.WithActingAgentRole(ctx, role), tool); err != nil {
				t.Fatalf("%s role %s: %v", name, role, err)
			}
		}
		for _, refused := range []context.Context{ctx, memql.WithActingAgentRole(ctx, "operator"), memql.WithMCPHumanCaller(ctx, "owner")} {
			if err := e.ToolCallRefusal(refused, tool); err == nil {
				t.Fatalf("%s lost its declared role restriction", name)
			}
		}
	}
	if !slices.Equal(data["assistant"].(map[string]any)["tools"].([]string), names) {
		t.Fatal("rendered assistant does not receive the same tool names")
	}
}

func TestRepositoryWorkToolsRequireOwnedRunAndInstalledContracts(t *testing.T) {
	names := []string{"inspectRepositoryAuthority", "requestRepositoryChange", "repositoryChangeRequestStatus", "listRepositoryChangeRequests"}
	registered := map[string]bool{}
	for _, name := range names {
		registered[name] = true
	}
	r := newTestReplier(&registryEngine{registered: registered})
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"ordinary", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner"})},
		{"foreign", common.ContextWithRun(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "stranger"}), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})},
		{"missing-goal", common.ContextWithRun(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner"}), common.RunContext{RunId: "run", OwnerUserId: "owner"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			got := r.scopeWorkExecution(tc.ctx, data, []string{"recall"})
			if !slices.Equal(got, []string{"recall"}) || len(data) != 0 {
				t.Fatalf("unowned context acquired Work tools: %v %v", got, data)
			}
		})
	}
	ctx := common.ContextWithRun(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner"}), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	delete(registered, "requestRepositoryChange")
	got := r.scopeWorkExecution(ctx, map[string]any{}, nil)
	if slices.Contains(got, "requestRepositoryChange") || !slices.Contains(got, "inspectRepositoryAuthority") {
		t.Fatalf("uninstalled contract advertised or installed read omitted: %v", got)
	}
}

func countToolName(names []string, target string) int {
	n := 0
	for _, name := range names {
		if name == target {
			n++
		}
	}
	return n
}
