package memql

// tool_gate_test.go -- the two tool gates and the caller kinds they read
// (memql#5438). The engine here has no database, so its role ladder is the
// compiled base ladder (owner 400, developer 300, admin 200, writer 100,
// reader 50) -- the ladder a first boot validates against.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// agentFor is an agent's tool-loop context: the acting agent's role, acting
// for a person holding personRole.
func agentFor(agentRole, personRole string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:u1", Role: auth.Role(personRole)})
	return WithActingAgentRole(ctx, agentRole)
}

// personOverMCP is an authenticated person calling over MCP.
func personOverMCP(role string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:u1", Role: auth.Role(role)})
	return WithMCPHumanCaller(ctx, role)
}

func TestToolCallerFromContextNamesTheTwoKinds(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want ToolCaller
	}{
		{"nothing stamped", context.Background(), ToolCaller{}},
		{"an agent", WithActingAgentRole(context.Background(), "assistant"),
			ToolCaller{Kind: ToolCallerAgent, AgentRole: "assistant", LegacyRole: "assistant"}},
		{"a person over MCP", WithMCPHumanCaller(context.Background(), "owner"),
			ToolCaller{Kind: ToolCallerMCPHuman, LegacyRole: "owner"}},
		// An agent loop started inside an MCP call stamps its own agent, and
		// it is that agent calling.
		{"an agent inside an MCP call", WithActingAgentRole(WithMCPHumanCaller(context.Background(), "owner"), "specialist"),
			ToolCaller{Kind: ToolCallerAgent, AgentRole: "specialist", LegacyRole: "specialist"}},
		{"an empty agent role is no agent", WithActingAgentRole(context.Background(), "  "), ToolCaller{}},
	}
	for _, tc := range cases {
		if got := ToolCallerFromContext(tc.ctx); got != tc.want {
			t.Errorf("%s: ToolCallerFromContext = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestToolCallRefusalAppliesEachGateToItsOwnAxis is the matrix: which agent is
// calling decides @requiresAgentRole, the person the call is for decides
// @requiresRank, and a person over MCP is never an agent kind.
func TestToolCallRefusalAppliesEachGateToItsOwnAxis(t *testing.T) {
	eng := newQuietEngine(t)
	assistantOnly := &Tool{Name: "delegateGoal", RequiresAgentRole: []string{"assistant"}}
	anyAgent := &Tool{Name: "discoverThings", RequiresAgentRole: []string{"assistant", "specialist"}}
	writerFloor := &Tool{Name: "fileRequest", RequiresRank: "writer"}
	developerFloor := &Tool{Name: "validateRequest", RequiresRank: "developer"}
	ungated := &Tool{Name: "openTicket"}

	cases := []struct {
		name    string
		ctx     context.Context
		tool    *Tool
		refused string // "" = admitted; otherwise text the refusal carries
	}{
		// @requiresAgentRole: which AGENT.
		{"assistant calls an assistant tool", agentFor("assistant", "reader"), assistantOnly, ""},
		{"specialist calls an assistant tool", agentFor("specialist", "owner"), assistantOnly, `not allowed for caller role "specialist"`},
		{"specialist calls an any-agent tool", agentFor("specialist", "reader"), anyAgent, ""},
		{"a person over MCP is not an agent kind", personOverMCP("owner"), anyAgent, "a person calling over MCP is not an agent"},
		// The old MCP surface stamped a person's role as the acting agent's, so
		// a role string that happened to read "assistant" passed an
		// assistant-only gate. A person is not an agent however it is spelled.
		{"a person whose role string reads like an agent kind", personOverMCP("assistant"), assistantOnly, "a person calling over MCP is not an agent"},

		// @requiresRank: the PERSON the call is for, whoever is calling.
		{"owner over MCP clears a writer floor", personOverMCP("owner"), writerFloor, ""},
		{"writer over MCP clears a writer floor", personOverMCP("writer"), writerFloor, ""},
		{"reader over MCP is below a writer floor", personOverMCP("reader"), writerFloor, `requires the "writer" role or above`},
		{"an agent acting for a writer is below a developer floor", agentFor("assistant", "writer"), developerFloor, `requires the "developer" role or above`},
		{"an agent acting for a developer clears it", agentFor("assistant", "developer"), developerFloor, ""},
		{"an agent's own kind does not rank", WithActingAgentRole(context.Background(), "assistant"), writerFloor, "carries no caller identity"},

		// The caller-kind rule itself.
		{"an agent calls an ungated tool", agentFor("specialist", "reader"), ungated, ""},
		{"a person over MCP calls an ungated tool", personOverMCP("reader"), ungated, ""},
		{"nobody is calling", context.Background(), ungated, "tools are agent-only"},
		{"a person over MCP with no identity", WithMCPHumanCaller(context.Background(), "owner"), ungated, "no authenticated identity"},
		{"an anonymous actor is no identity", WithMCPHumanCaller(auth.ContextWithAccess(context.Background(), auth.AnonymousActor()), "reader"), ungated, "no authenticated identity"},
	}
	for _, tc := range cases {
		err := eng.ToolCallRefusal(tc.ctx, tc.tool)
		switch {
		case tc.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refused != "" && err == nil:
			t.Errorf("%s: admitted; want a refusal carrying %q", tc.name, tc.refused)
		case tc.refused != "" && !strings.Contains(err.Error(), tc.refused):
			t.Errorf("%s: refusal %q does not carry %q", tc.name, err, tc.refused)
		}
		// Every refusal must read as a PERMISSION refusal to the agent loop:
		// its repeat-failure breaker and the model both key on the class.
		if err != nil {
			if got := ClassifyToolError(err).Type; got != ToolErrorPermission {
				t.Errorf("%s: the refusal classifies as %q, want %q: %v", tc.name, got, ToolErrorPermission, err)
			}
		}
	}
}

// TestDeprecatedAllowedRolesKeepsItsBehaviourInItsWindow: @allowedRoles still
// compares ONE string -- the agent's role for an agent, the person's cluster
// role over MCP -- so the tools that used it keep working until they migrate.
func TestDeprecatedAllowedRolesKeepsItsBehaviourInItsWindow(t *testing.T) {
	eng := newQuietEngine(t)
	agentKinds := &Tool{Name: "agentKinds", AllowedRoles: []string{"assistant"}}
	humanRoles := &Tool{Name: "humanRoles", AllowedRoles: []string{"owner", "admin"}}
	specialistDefault := &Tool{Name: "specialistDefault", AllowedRoles: []string{"specialist"}}

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		tool  *Tool
		admit bool
	}{
		{"assistant agent, agent-kind list", agentFor("assistant", "reader"), agentKinds, true},
		{"specialist agent, agent-kind list", agentFor("specialist", "reader"), agentKinds, false},
		{"owner over MCP, human-role list", personOverMCP("owner"), humanRoles, true},
		{"writer over MCP, human-role list", personOverMCP("writer"), humanRoles, false},
		// The list is compared as a string, which is why the form is leaving:
		// a person whose role were spelled "assistant" would pass it.
		{"an agent kind in a human-role list", agentFor("assistant", "owner"), humanRoles, false},
		// A person with NO role was refused outright before (the surface
		// stamped nothing), so the "" -> specialist default must not admit
		// them now.
		{"a person over MCP with no role", personOverMCP(""), specialistDefault, false},
	} {
		err := eng.ToolCallRefusal(tc.ctx, tc.tool)
		if (err == nil) != tc.admit {
			t.Errorf("%s: admitted=%v, want %v (%v)", tc.name, err == nil, tc.admit, err)
		}
	}
}

// TestAToolIsListedExactlyWhenItIsCallable: listing and calling are one
// decision asked twice, so a caller is never offered a tool it cannot call
// nor refused one it was offered.
func TestAToolIsListedExactlyWhenItIsCallable(t *testing.T) {
	eng := newQuietEngine(t)
	tools := []*Tool{
		{Name: "a", RequiresAgentRole: []string{"assistant"}},
		{Name: "b", RequiresAgentRole: []string{"assistant", "specialist"}},
		{Name: "c", RequiresRank: "writer"},
		{Name: "d", RequiresRank: "owner", RequiresAgentRole: []string{"specialist"}},
		{Name: "e", AllowedRoles: []string{"assistant"}},
		{Name: "f", AllowedRoles: []string{"owner", "writer"}},
		{Name: "g"},
	}
	callers := map[string]context.Context{
		"assistant for reader":  agentFor("assistant", "reader"),
		"specialist for owner":  agentFor("specialist", "owner"),
		"specialist for writer": agentFor("specialist", "writer"),
		"owner over MCP":        personOverMCP("owner"),
		"writer over MCP":       personOverMCP("writer"),
		"reader over MCP":       personOverMCP("reader"),
		"assistant over MCP (a person whose role string is an agent kind)": personOverMCP("assistant"),
	}
	for name, ctx := range callers {
		for _, tool := range tools {
			listed := eng.ToolListed(ctx, tool)
			callable := eng.ToolCallRefusal(ctx, tool) == nil
			if listed != callable {
				t.Errorf("%s / tool %s: listed=%v but callable=%v", name, tool.Name, listed, callable)
			}
		}
	}
}

// TestExecuteToolEnforcesTheGatesBeforeAnyHandler: the gate is ExecuteTool's,
// so every path that calls a tool -- an agent loop, CallToolMsg, MCP
// tools/call, the engine's own tool loop -- is held to it. The tool has no
// handler, so an admitted call answers "no handler" and a refused one never
// gets that far.
func TestExecuteToolEnforcesTheGatesBeforeAnyHandler(t *testing.T) {
	eng := newQuietEngine(t)
	tool := &Tool{Name: "assistantOnlyNoHandler", RequiresAgentRole: []string{"assistant"}}

	if _, err := eng.ExecuteTool(personOverMCP("owner"), tool, nil); err == nil ||
		!strings.Contains(err.Error(), "a person calling over MCP is not an agent") {
		t.Fatalf("a person over MCP reached an agent-kind tool: %v", err)
	}
	if _, err := eng.ExecuteTool(agentFor("specialist", "owner"), tool, nil); err == nil {
		t.Fatal("a specialist reached an assistant-only tool")
	}
	res, err := eng.ExecuteTool(agentFor("assistant", "reader"), tool, nil)
	if err != nil {
		t.Fatalf("the assistant was refused its own tool: %v", err)
	}
	if res == nil || !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "no handler") {
		t.Fatalf("an admitted call should reach the handler check, got %+v", res)
	}
}

// TestAnUnresolvableToolFloorRefusesEveryone: a floor the ladder does not know
// fails closed at call time for EVERY caller, the owner included -- the
// runtime backstop behind the load refusal.
func TestAnUnresolvableToolFloorRefusesEveryone(t *testing.T) {
	eng := newQuietEngine(t)
	tool := &Tool{Name: "typoFloor", RequiresRank: "superuser"}
	err := eng.ToolCallRefusal(personOverMCP("owner"), tool)
	if err == nil || !strings.Contains(err.Error(), "names no role") {
		t.Fatalf("an unresolvable floor admitted the owner: %v", err)
	}
	if eng.ToolListed(personOverMCP("owner"), tool) {
		t.Fatal("an unresolvable floor listed the tool")
	}
}

// TestToolGateRefusalsAreNotSentinelErrors guards the classification test
// above against passing on a wrapped nil: the rank refusal wraps
// refuseBelowRequiredRank's error, which must survive the wrap.
func TestToolGateRefusalsAreNotSentinelErrors(t *testing.T) {
	eng := newQuietEngine(t)
	err := eng.ToolCallRefusal(personOverMCP("reader"), &Tool{Name: "x", RequiresRank: "admin"})
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("the rank refusal should wrap the floor's own refusal, got %v", err)
	}
}
