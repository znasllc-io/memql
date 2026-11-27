package calls

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func readinessDefinition() WorkflowDefinition {
	return WorkflowDefinition{ID: "readiness-test", Title: "Camera alternatives", RequiresInsights: true, EntryNodeID: "check", Nodes: []WorkflowNode{
		{ID: "check", Type: "readiness", Label: "Check camera", Visibility: "all", Requirements: WorkflowRequirements{Video: true}, Readiness: &WorkflowReadinessSpec{TimeoutSeconds: 30, Help: "Choose a camera, or review the approved verbal alternative after waiting.", FallbackNodeID: "alternative"}},
		{ID: "primary", Type: "instruction", Label: "Visual route", Visibility: "all"},
		{ID: "alternative", Type: "instruction", Label: "Verbal route", Visibility: "all"},
		{ID: "done", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []WorkflowEdge{{ID: "primary", Source: "check", Target: "primary"}, {ID: "alternative", Source: "check", Target: "alternative"}, {ID: "p-done", Source: "primary", Target: "done"}, {ID: "a-done", Source: "alternative", Target: "done"}}}
}
func TestWorkflowReadinessClosedGraphAndFallbackContract(t *testing.T) {
	d := readinessDefinition()
	require.Empty(t, ValidateWorkflow(d))
	require.Equal(t, []string{"primary"}, WorkflowOrdinaryNext(d, "check"))
	require.EqualValues(t, 30, SimulateWorkflow(d).MaximumSeconds, "planning includes one bounded readiness wait per visit")
	for name, mutate := range map[string]func(*WorkflowDefinition){
		"zero-timeout":      func(d *WorkflowDefinition) { d.Nodes[0].Readiness.TimeoutSeconds = 0 },
		"unbounded-timeout": func(d *WorkflowDefinition) { d.Nodes[0].Readiness.TimeoutSeconds = 3601 },
		"missing-help":      func(d *WorkflowDefinition) { d.Nodes[0].Readiness.Help = "" },
		"oversized-help":    func(d *WorkflowDefinition) { d.Nodes[0].Readiness.Help = strings.Repeat("x", 1001) },
		"hidden-control":    func(d *WorkflowDefinition) { d.Nodes[0].Readiness.Help = "hidden\x00instruction" },
		"no-condition":      func(d *WorkflowDefinition) { d.Nodes[0].Requirements = WorkflowRequirements{} },
		"undeclared-target": func(d *WorkflowDefinition) { d.Nodes[0].Readiness.FallbackNodeID = "done" },
		"cycle":             func(d *WorkflowDefinition) { d.Nodes[0].Readiness.FallbackNodeID = "check" },
		"only-fallback":     func(d *WorkflowDefinition) { d.Edges = d.Edges[1:] },
		"effect-bypass": func(d *WorkflowDefinition) {
			d.Nodes[0].Type = "effect"
			d.Nodes[0].Effect = &WorkflowEffectSpec{Action: "end_turn_mute"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := readinessDefinition()
			mutate(&copy)
			require.NotEmpty(t, ValidateWorkflow(copy))
		})
	}
	req := WorkflowRequest{Action: "fallback", RunID: "run", ExpectedRevision: 1, ExpectedEpoch: 1, IdempotencyKey: "fallback-once"}
	require.True(t, ValidWorkflowFallbackRequest(req))
	req.NodeID = "alternative"
	require.False(t, ValidWorkflowFallbackRequest(req))
	req.NodeID = ""
	req.Seconds = 10
	require.False(t, ValidWorkflowFallbackRequest(req))
}

func TestWorkflowReadinessFallbackDiscoveryIsClosedAndBranchPaired(t *testing.T) {
	var found map[string]any
	for _, raw := range workflowUTCPTools("https://calls.example.test/agent") {
		tool := raw.(map[string]any)
		if tool["name"] == "callkey_workflow_fallback" {
			found = tool
		}
	}
	require.NotNil(t, found)
	request := found["inputs"].(map[string]any)["properties"].(map[string]any)["request"].(map[string]any)
	workflow := request["properties"].(map[string]any)["workflow"].(map[string]any)
	require.Equal(t, false, workflow["additionalProperties"])
	require.Len(t, workflow["properties"], 7)
	require.ElementsMatch(t, []string{"action", "runId", "expectedRevision", "expectedEpoch", "idempotencyKey"}, workflow["required"])
	require.Equal(t, map[string]any{"branchId": []string{"branchVisitId"}, "branchVisitId": []string{"branchId"}}, workflow["dependentRequired"])
	require.Contains(t, found["description"], "never dispatches media effects")
}
