package app

// procedure_mcp_caller_test.go -- a replayed MCP step is decided by every tool
// gate exactly as its recording was (memql#5438).
//
// A recording is an app session's tools/call on the MCP surface, which calls a
// tool as a PERSON: the session's owner, holding the role its credential names
// -- none, for an app session (component/mcp's
// TestAnAppSessionActsAsItsOwnerHoldingNoRole). The replay used to run as the
// owner's reasoning AGENT, so a tool gated on an agent kind admitted a replay
// its recording was refused. Each gate is asked here of the REAL engine, once
// for the recording's caller and once for the context the replay actually
// reaches the registry under.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// recordingCaller is the context the MCP surface calls a tool under for an app
// session: its owner, holding no role, marked a person over MCP -- what
// component/mcp's Server.withActorEnvelope and callMCPTool build from an app
// session's configuration.
func recordingCaller(owner string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner})
	return memql.WithMCPHumanCaller(ctx, "")
}

// capturingProcedureTools is a registry of read tools that records the context
// a replayed call reaches it under.
type capturingProcedureTools struct {
	kinds map[string]string
	ctx   context.Context
}

func (c *capturingProcedureTools) kindOf(name string) (string, bool) {
	kind, ok := c.kinds[name]
	return kind, ok
}

func (c *capturingProcedureTools) execute(ctx context.Context, _ string, _ map[string]any) (string, error) {
	c.ctx = ctx
	return `{"content":[{"type":"text","text":"[]"}],"isError":false}`, nil
}

func TestAReplayedMCPToolIsDecidedAsItsRecordingWas(t *testing.T) {
	engine := procedureTestEngine(t)
	const owner = "v1:identity:user:owner"
	cases := []struct {
		tool     *memql.Tool
		admitted bool // what the recording's caller was answered
	}{
		// The reasoning agent's own kind: the replay used to be that agent.
		{&memql.Tool{Name: "discoverForAssistant", RequiresAgentRole: []string{"assistant"}}, false},
		{&memql.Tool{Name: "discoverForAnyAgent", RequiresAgentRole: []string{"assistant", "specialist"}}, false},
		// A rank floor: the recording's caller held no role.
		{&memql.Tool{Name: "listForReaders", RequiresRank: "reader"}, false},
		// The deprecated form, both kinds of list.
		{&memql.Tool{Name: "legacyAgentList", AllowedRoles: []string{"assistant"}}, false},
		{&memql.Tool{Name: "legacyPersonList", AllowedRoles: []string{"owner", "admin", "developer", "writer", "reader"}}, false},
		// An ungated read runs, as it did.
		{&memql.Tool{Name: "listOpen"}, true},
	}
	tools := &capturingProcedureTools{kinds: map[string]string{}}
	for _, c := range cases {
		tools.kinds[c.tool.Name] = "query"
	}
	d := newTestWorkbenchDispatcher(newFakeWorkbench(), tools)

	for _, c := range cases {
		tools.ctx = nil
		if _, err := d.Dispatch(context.Background(), workbenchStep("mcp", map[string]any{"tool": c.tool.Name})); err != nil {
			t.Fatalf("%s: the replay did not reach the registry: %v", c.tool.Name, err)
		}
		recorded := engine.ToolCallRefusal(recordingCaller(owner), c.tool)
		replayed := engine.ToolCallRefusal(tools.ctx, c.tool)
		if (recorded == nil) != c.admitted {
			t.Fatalf("%s: the recording's caller was answered %v; the fixture expects admitted=%v", c.tool.Name, recorded, c.admitted)
		}
		if (replayed == nil) != (recorded == nil) {
			t.Errorf("%s: the replay was answered %v, but its recording was answered %v", c.tool.Name, replayed, recorded)
		}
	}

	// FOR THE RIGHT REASON. With no principal table a stand-in role resolves
	// no role either, so a rank floor refuses a replay under borrowed
	// authority too -- by accident. The replay must hold the recording's
	// actor itself: the owner, no role, not a stand-in, a person over MCP.
	ac, _ := auth.AccessFromContext(tools.ctx)
	if ac == nil || ac.UserId != owner || ac.Role != "" || ac.RoleStandIn || ac.Unranked {
		t.Errorf("the replay's actor is %+v, want the recording's: the owner, holding no role", ac)
	}
	if caller := memql.ToolCallerFromContext(tools.ctx); caller != (memql.ToolCaller{Kind: memql.ToolCallerMCPHuman}) {
		t.Errorf("the replay called as %+v, want a person over MCP holding no role", caller)
	}
}
