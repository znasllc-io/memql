package memql

// tool_gate_shipped_test.go -- the SHIPPED tree's tools, through the engine's
// real gate (memql#5438): no tool is left on the deprecated @allowedRoles, the
// agent-kind tools are never offered to a person over MCP, and the forge tools
// are offered by rank.

import (
	"strings"
	"testing"
)

func TestTheShippedToolsFollowTheAxes(t *testing.T) {
	registry := loadedConceptRegistry(t)
	eng := newQuietEngine(t)
	if err := eng.Init(registry); err != nil {
		t.Fatalf("Init over the embedded tree: %v", err)
	}

	agentKind := map[string]bool{}
	for _, tool := range eng.Tools().List() {
		if len(tool.AllowedRoles) > 0 {
			t.Errorf("shipped tool %s still gates on the deprecated @allowedRoles %v", tool.Name, tool.AllowedRoles)
		}
		if len(tool.RequiresAgentRole) > 0 {
			agentKind[tool.Name] = true
		}
	}
	// The work tools were gated on "system-planner", a roleSlug no agent's role
	// ever is, so the planner -- a specialist -- could not call the tools its
	// own work runs offer it. They name the agent kinds now.
	for _, name := range []string{"discoverCapabilities", "executeCapability", "navigateOS", "recallWorkHistory"} {
		tool, err := eng.Tools().Get(name)
		if err != nil {
			t.Fatalf("work tool %s is not registered: %v", name, err)
		}
		if err := eng.ToolCallRefusal(agentFor("specialist", "writer"), tool); err != nil &&
			!strings.Contains(err.Error(), "no handler") {
			t.Errorf("a specialist (the planner's role) is refused %s: %v", name, err)
		}
	}
	if !agentKind["ensureAgent"] || !agentKind["discoverCapabilities"] {
		t.Fatalf("the shipped agent-kind tools were not found (%d found); this test would measure nothing", len(agentKind))
	}
	// Assistant-only stays assistant-only: the distinction the delegation
	// baseline depends on.
	ensure, err := eng.Tools().Get("ensureAgent")
	if err != nil {
		t.Fatal(err)
	}
	if eng.ToolCallRefusal(agentFor("specialist", "owner"), ensure) == nil {
		t.Error("a specialist may call ensureAgent, the assistant-only agent factory")
	}

	developerTier := []string{"forgeRegisterProject", "forgeRequestHistory", "forgeValidationQueue",
		"forgeValidateRequest", "forgeApprovalQueue", "forgeApproveRequest", "forgeRequestChanges"}
	teamTier := []string{"forgeActiveProjects", "forgeResolveProject", "forgeSubmitRequest", "forgeMyRequests",
		"forgeRequestById", "forgeRecordMentoring", "forgeAttachToRequest"}
	for _, role := range []string{"owner", "developer", "admin", "writer", "reader"} {
		ctx := personOverMCP(role)
		for _, tool := range eng.Tools().List() {
			if agentKind[tool.Name] && eng.ToolListed(ctx, tool) {
				t.Errorf("%s over MCP is offered agent-kind tool %s", role, tool.Name)
			}
		}
		for _, name := range teamTier {
			tool, err := eng.Tools().Get(name)
			if err != nil {
				t.Fatalf("forge tool %s: %v", name, err)
			}
			if !eng.ToolListed(ctx, tool) {
				t.Errorf("%s over MCP is not offered team tool %s", role, name)
			}
		}
		for _, name := range developerTier {
			tool, err := eng.Tools().Get(name)
			if err != nil {
				t.Fatalf("forge tool %s: %v", name, err)
			}
			if got, want := eng.ToolListed(ctx, tool), role != "reader"; got != want {
				t.Errorf("%s over MCP offered developer tool %s = %v, want %v", role, name, got, want)
			}
		}
	}
}
