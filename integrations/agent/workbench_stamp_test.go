package agent

import "testing"

// TestInjectAgentContext_WorkbenchHostStampsRunId is the memql#948 guard.
// workbenchHost declares runId/agentId as @autoInjected, but injectAgentContext
// only stamps tools present in agentContextStamps. Without a workbenchHost entry
// the runId is never injected and the per-Plan workbench dispatch fails
// "missing required arg runId", so the produceArtifact deliverable is never
// written.
func TestInjectAgentContext_WorkbenchHostStampsRunId(t *testing.T) {
	args := map[string]any{
		"action": "fs_write",
		"args":   map[string]any{"path": "out.md", "content": "hi"},
		// The LLM is told NOT to supply these, but may hallucinate a bogus
		// agentId -- the runtime must overwrite it with the real id.
		"agentId": "agent",
	}
	tc := turnContext{
		AgentId: "v1:agents:agent:abc",
		RunId:   "v1:work:run:r1",
	}

	injectAgentContext("workbenchHost", args, tc)

	if got := args["runId"]; got != "v1:work:run:r1" {
		t.Fatalf("runId = %v, want the turn-context run id (required by the workbench dispatch)", got)
	}
	if got := args["agentId"]; got != "v1:agents:agent:abc" {
		t.Fatalf("agentId = %v, want the runtime id (must overwrite the LLM's bogus value)", got)
	}
}

// TestInjectAgentContext_WorkbenchHostNoRunIdWhenContextEmpty asserts the
// stamp is a no-op when the turn carries no run id (a chat-driven workbench
// call, not a plan execution) rather than stamping an empty runId.
func TestInjectAgentContext_WorkbenchHostNoRunIdWhenContextEmpty(t *testing.T) {
	args := map[string]any{"action": "fs_list", "args": map[string]any{"path": "."}}

	injectAgentContext("workbenchHost", args, turnContext{AgentId: "v1:agents:agent:abc"})

	if _, present := args["runId"]; present {
		t.Fatalf("runId should not be stamped when the turn context carries none, got %v", args["runId"])
	}
	if got := args["agentId"]; got != "v1:agents:agent:abc" {
		t.Fatalf("agentId = %v, want it stamped from the turn context", got)
	}
}

// TestInjectAgentContext_CanvasPublishStampsDataPlanId is the memql#1207 guard.
// The published canvasState document card must carry producedByPlanId inside
// its `data` object so the frontend DocumentCard can synthesize a "View task"
// deep-link (frontend#393). canvasPublish has no top-level planId in its
// schema; the provenance rides inside the card data, so the runtime stamps it
// there server-side.
func TestInjectAgentContext_CanvasPublishStampsDataPlanId(t *testing.T) {
	data := map[string]any{"title": "Task done", "source": "# Done\nresult", "producedByPlanId": "hallucinated"}
	args := map[string]any{
		"kind": "document",
		"data": data,
	}
	tc := turnContext{
		AgentId:     "v1:agents:agent:abc",
		PartitionId: "v1:cognition:space:s1",
		RunId:       "v1:planner:plan:p1",
	}

	injectAgentContext("canvasPublish", args, tc)

	gotData, ok := args["data"].(map[string]any)
	if !ok {
		t.Fatalf("data should remain a map, got %T", args["data"])
	}
	// Always overwrite -- the runtime turn-context plan id is the source of
	// truth, not the LLM-supplied value.
	if got := gotData["producedByPlanId"]; got != "v1:planner:plan:p1" {
		t.Fatalf("data.producedByPlanId = %v, want the runtime plan id", got)
	}
	// The actor stamp (canvasPublish also carries StampActor) must still fire.
	if _, present := args["actor"]; !present {
		t.Errorf("expected actor to be stamped on canvasPublish")
	}
}

// TestInjectAgentContext_CanvasPublishCreatesDataWhenMissing asserts the stamp
// creates the data map when the model omitted one, so producedByPlanId always
// lands when a plan id is in context.
func TestInjectAgentContext_CanvasPublishCreatesDataWhenMissing(t *testing.T) {
	args := map[string]any{"kind": "document"}

	injectAgentContext("canvasPublish", args, turnContext{
		AgentId: "v1:agents:agent:abc",
		RunId:   "v1:planner:plan:p1",
	})

	gotData, ok := args["data"].(map[string]any)
	if !ok {
		t.Fatalf("data should have been created as a map, got %T", args["data"])
	}
	if got := gotData["producedByPlanId"]; got != "v1:planner:plan:p1" {
		t.Fatalf("data.producedByPlanId = %v, want the runtime plan id", got)
	}
}

// TestInjectAgentContext_CanvasPublishNoPlanIdWhenContextEmpty asserts a
// chat-driven canvasPublish (no plan in context) does not stamp an empty
// producedByPlanId or fabricate a data map.
func TestInjectAgentContext_CanvasPublishNoPlanIdWhenContextEmpty(t *testing.T) {
	data := map[string]any{"title": "T", "source": "body"}
	args := map[string]any{"kind": "document", "data": data}

	injectAgentContext("canvasPublish", args, turnContext{AgentId: "v1:agents:agent:abc"})

	if _, present := data["producedByPlanId"]; present {
		t.Fatalf("producedByPlanId should not be stamped without a plan id in context, got %v", data["producedByPlanId"])
	}
}
