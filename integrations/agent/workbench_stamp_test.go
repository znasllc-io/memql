package agent

import "testing"

// TestInjectAgentContext_WorkbenchHostStampsPlanId is the memql#948 guard.
// workbenchHost declares planId/agentId as @autoInjected, but injectAgentContext
// only stamps tools present in agentContextStamps. Without a workbenchHost entry
// the planId is never injected and the per-Plan workbench dispatch fails
// "missing required arg planId", so the produceArtifact deliverable is never
// written.
func TestInjectAgentContext_WorkbenchHostStampsPlanId(t *testing.T) {
	args := map[string]any{
		"action": "fs_write",
		"args":   map[string]any{"path": "out.md", "content": "hi"},
		// The LLM is told NOT to supply these, but may hallucinate a bogus
		// agentId -- the runtime must overwrite it with the real id.
		"agentId": "agent",
	}
	tc := turnContext{
		AgentId: "v1:agents:agent:abc",
		PlanId:  "v1:planner:plan:p1",
	}

	injectAgentContext("workbenchHost", args, tc)

	if got := args["planId"]; got != "v1:planner:plan:p1" {
		t.Fatalf("planId = %v, want the turn-context plan id (required by the workbench dispatch)", got)
	}
	if got := args["agentId"]; got != "v1:agents:agent:abc" {
		t.Fatalf("agentId = %v, want the runtime id (must overwrite the LLM's bogus value)", got)
	}
}

// TestInjectAgentContext_WorkbenchHostNoPlanIdWhenContextEmpty asserts the
// stamp is a no-op when the turn carries no plan id (a chat-driven workbench
// call, not a plan execution) rather than stamping an empty planId.
func TestInjectAgentContext_WorkbenchHostNoPlanIdWhenContextEmpty(t *testing.T) {
	args := map[string]any{"action": "fs_list", "args": map[string]any{"path": "."}}

	injectAgentContext("workbenchHost", args, turnContext{AgentId: "v1:agents:agent:abc"})

	if _, present := args["planId"]; present {
		t.Fatalf("planId should not be stamped when the turn context carries none, got %v", args["planId"])
	}
	if got := args["agentId"]; got != "v1:agents:agent:abc" {
		t.Fatalf("agentId = %v, want it stamped from the turn context", got)
	}
}
