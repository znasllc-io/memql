package mcp

// agent_role_axis_test.go -- the MCP surface calls tools as a PERSON, never as
// an agent (memql#5438).
//
// The surface used to stamp the person's cluster role as the acting AGENT's
// role, which is how one @allowedRoles list came to gate agent kinds on some
// tools and human roles on others. It now marks the call as a person's over
// MCP: a tool gated on an agent kind (@requiresAgentRole) is neither listed nor
// callable for them, whatever their role is spelled, and a tool's rank floor
// (@requiresRank) is judged against their own rank.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func agentKindFixtures() *fakeRegistry {
	return &fakeRegistry{tools: []*memql.Tool{
		{Name: "delegateGoal", Description: "d", RequiresAgentRole: []string{"assistant"}},
		{Name: "discoverThings", Description: "d", RequiresAgentRole: []string{"assistant", "specialist"}},
		{Name: "openTicket", Description: "d"},
	}}
}

// A person over MCP is not an agent kind, however their role is spelled -- a
// role string that happens to read "assistant" included.
func TestAnAgentKindToolIsNeitherListedNorCallableOverMCP(t *testing.T) {
	eng := &fakeEngine{reg: agentKindFixtures()}
	for _, role := range []string{"owner", "developer", "reader", "assistant", "specialist"} {
		names := toolNames(listMCPTools(asPerson(role), eng, role, TierSealed, ""))
		if names["delegateGoal"] || names["discoverThings"] {
			t.Errorf("role %q: an agent-kind tool was listed to a person over MCP: %v", role, names)
		}
		if !names["openTicket"] {
			t.Errorf("role %q: the ungated tool is missing from the surface: %v", role, names)
		}
		res := callMCPTool(asPerson(role), eng, role, TierSealed, "", "discoverThings", nil)
		if !isError(res) || !strings.Contains(resultText(res), "a person calling over MCP is not an agent") {
			t.Errorf("role %q: calling an agent-kind tool over MCP = %v; want the not-an-agent refusal", role, res)
		}
	}
}

// Nothing the surface does stamps an acting agent: a reflected call reaches
// the engine as a person, with the role the session carries.
func TestTheMCPSurfaceNeverStampsAnActingAgent(t *testing.T) {
	eng := &fakeEngine{reg: agentKindFixtures()}
	res := callMCPTool(asPerson("owner"), eng, "owner", TierSealed, "", "openTicket", nil)
	if isError(res) {
		t.Fatalf("an owner's call to an ungated tool was refused: %v", res)
	}
	if eng.roleSeen != "" {
		t.Errorf("the call reached the engine with acting agent role %q", eng.roleSeen)
	}
	if eng.callerSeen.Kind != memql.ToolCallerMCPHuman || eng.callerSeen.LegacyRole != "owner" {
		t.Errorf("the call reached the engine as %+v, want a person over MCP holding owner", eng.callerSeen)
	}
}

// A tool is called by an agent or by an AUTHENTICATED person over MCP: a
// session with no identity -- stdio without MEMQL_MCP_USER -- runs none.
func TestAnMCPSessionWithNoIdentityRunsNoTool(t *testing.T) {
	eng := &fakeEngine{reg: agentKindFixtures()}
	res := callMCPTool(context.Background(), eng, "owner", TierSealed, "", "openTicket", nil)
	if !isError(res) || !strings.Contains(resultText(res), "MEMQL_MCP_USER") {
		t.Fatalf("a session with no identity ran a tool: %v", res)
	}
}
