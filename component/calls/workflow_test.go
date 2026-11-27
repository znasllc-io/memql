package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func workflowTestDefinition() WorkflowDefinition {
	return WorkflowDefinition{ID: "test", Title: "Test session", RequiresInsights: true, Roles: []WorkflowRole{{ID: "speaker", Label: "Speaker", Required: true}}, EntryNodeID: "turn", Nodes: []WorkflowNode{{ID: "turn", Type: "turn", Label: "Speak", RoleID: "speaker", Visibility: "all", DurationSeconds: 60}, {ID: "done", Type: "finish", Label: "Finish", Visibility: "all"}}, Edges: []WorkflowEdge{{ID: "next", Source: "turn", Target: "done"}}}
}
func TestWorkflowValidationRejectsUnboundedAndUnimplementedGraphs(t *testing.T) {
	valid := workflowTestDefinition()
	require.Empty(t, ValidateWorkflow(valid))
	cases := map[string]func(*WorkflowDefinition){
		"insights": func(d *WorkflowDefinition) { d.RequiresInsights = false },
		"cycle": func(d *WorkflowDefinition) {
			d.Nodes[1].Type = "instruction"
			d.Edges = append(d.Edges, WorkflowEdge{ID: "cycle", Source: "done", Target: "turn"})
		},
		"unreachable": func(d *WorkflowDefinition) {
			d.Nodes = append(d.Nodes, WorkflowNode{ID: "unreachable", Type: "finish", Label: "Hidden", Visibility: "host"})
		},
		"role":          func(d *WorkflowDefinition) { d.Nodes[0].RoleID = "intruder" },
		"supervisor":    func(d *WorkflowDefinition) { d.Nodes[0].Requirements.SupervisorRoleID = "missing" },
		"effect":        func(d *WorkflowDefinition) { d.Nodes[0].Type = "send_email" },
		"parallel":      func(d *WorkflowDefinition) { d.Nodes[0].Type = "parallel" },
		"unboundedtime": func(d *WorkflowDefinition) { d.Nodes[0].DurationSeconds = 14401 },
		"emptyedges":    func(d *WorkflowDefinition) { d.Edges = nil },
		"ambiguous": func(d *WorkflowDefinition) {
			d.Edges = append(d.Edges, WorkflowEdge{ID: "again", Source: "turn", Target: "done"})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := workflowTestDefinition()
			change(&d)
			require.NotEmpty(t, ValidateWorkflow(d))
		})
	}
}
func TestWorkflowClockFreezesPausedAndResistsBackwardClock(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	r := WorkflowRun{Status: "running", AnchorAt: now, ElapsedSeconds: 12, DurationSeconds: 60}
	require.EqualValues(t, 42, WorkflowElapsed(r, now.Add(30*time.Second)))
	require.EqualValues(t, 12, WorkflowElapsed(r, now.Add(-30*time.Second)))
	r.Status = "paused"
	require.EqualValues(t, 12, WorkflowElapsed(r, now.Add(3*time.Hour)))
}
func TestWorkflowConditionRequiresExplicitBranchWithoutCycles(t *testing.T) {
	d := workflowTestDefinition()
	d.Nodes[0].Type = "condition"
	d.Nodes = append(d.Nodes, WorkflowNode{ID: "alternative", Type: "finish", Label: "Alternative", Visibility: "all"})
	d.Edges = append(d.Edges, WorkflowEdge{ID: "otherwise", Source: "turn", Target: "alternative", Label: "Otherwise"})
	require.Empty(t, ValidateWorkflow(d))
	require.Equal(t, []string{"done", "alternative"}, WorkflowNext(d, "turn"))
}

func TestWorkflowSimulationFollowsLongestLegalRouteRegardlessOfArrayOrder(t *testing.T) {
	d := WorkflowDefinition{
		ID: "routes", Title: "Choose a route", RequiresInsights: true, EntryNodeID: "choice",
		Nodes: []WorkflowNode{
			{ID: "finish", Type: "finish", Label: "Done", Visibility: "all", DurationSeconds: 100},
			{ID: "join", Type: "instruction", Label: "Together", Visibility: "all", DurationSeconds: 5},
			{ID: "short", Type: "instruction", Label: "Short", Visibility: "all", DurationSeconds: 10},
			{ID: "choice", Type: "condition", Label: "Choose", Visibility: "all", DurationSeconds: 2},
			{ID: "long", Type: "instruction", Label: "Long", Visibility: "all", DurationSeconds: 30},
		},
		Edges: []WorkflowEdge{
			{ID: "join-finish", Source: "join", Target: "finish"},
			{ID: "short-join", Source: "short", Target: "join"},
			{ID: "choice-short", Source: "choice", Target: "short"},
			{ID: "long-join", Source: "long", Target: "join"},
			{ID: "choice-long", Source: "choice", Target: "long"},
		},
	}
	require.Empty(t, ValidateWorkflow(d))
	nodes, edges := append([]WorkflowNode(nil), d.Nodes...), append([]WorkflowEdge(nil), d.Edges...)
	simulation := SimulateWorkflow(d)
	require.Equal(t, []string{"choice", "long", "join", "finish"}, simulation.NodeIDs)
	require.EqualValues(t, 37, simulation.MaximumSeconds, "alternative branches do not run together and finish has no running clock")
	require.Equal(t, nodes, d.Nodes, "simulation must not rearrange the saved graph")
	require.Equal(t, edges, d.Edges)

	// A zero-duration shared suffix still has a cached result; ties are stable
	// across arrays, so moving nodes or connections cannot change the example.
	d.Nodes[1].DurationSeconds, d.Nodes[2].DurationSeconds = 0, 30
	first := SimulateWorkflow(d)
	for i, j := 0, len(d.Nodes)-1; i < j; i, j = i+1, j-1 {
		d.Nodes[i], d.Nodes[j] = d.Nodes[j], d.Nodes[i]
	}
	for i, j := 0, len(d.Edges)-1; i < j; i, j = i+1, j-1 {
		d.Edges[i], d.Edges[j] = d.Edges[j], d.Edges[i]
	}
	require.Equal(t, first, SimulateWorkflow(d))
	require.Equal(t, []string{"choice", "long", "join", "finish"}, first.NodeIDs)
	require.EqualValues(t, 32, first.MaximumSeconds)
}

func TestWorkflowSimulationRefusesInvalidGraphsAndHandlesImmediateFinish(t *testing.T) {
	d := workflowTestDefinition()
	d.Nodes[1].Type = "instruction"
	d.Edges = append(d.Edges, WorkflowEdge{ID: "cycle", Source: "done", Target: "turn"})
	invalid := SimulateWorkflow(d)
	require.Empty(t, invalid.NodeIDs)
	require.Zero(t, invalid.MaximumSeconds)

	d = WorkflowDefinition{ID: "done", Title: "Already done", RequiresInsights: true, EntryNodeID: "finish", Nodes: []WorkflowNode{{ID: "finish", Type: "finish", Label: "Done", Visibility: "all", DurationSeconds: 100}}}
	simulation := SimulateWorkflow(d)
	require.Equal(t, []string{"finish"}, simulation.NodeIDs)
	require.Zero(t, simulation.MaximumSeconds)
}

func TestWorkflowCombinedMuteDiscoveryHasOneExactBoundedOperation(t *testing.T) {
	var found map[string]any
	for _, value := range workflowUTCPTools("https://calls.example.test/agent") {
		tool := value.(map[string]any)
		if tool["name"] == "callkey_workflow_end_turn_mute" {
			found = tool
		}
	}
	require.NotNil(t, found)
	require.Contains(t, found["description"], "BOTH skills_control and control")
	require.Contains(t, found["description"], "never mute again")
	request := found["inputs"].(map[string]any)["properties"].(map[string]any)["request"].(map[string]any)
	workflow := request["properties"].(map[string]any)["workflow"].(map[string]any)
	require.Equal(t, false, workflow["additionalProperties"])
	require.ElementsMatch(t, []string{"action", "runId", "expectedRevision", "expectedEpoch", "idempotencyKey"}, workflow["required"])
	require.Len(t, workflow["properties"], 6)
}
