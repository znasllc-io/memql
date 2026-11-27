package calls

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func parallelDefinition() (WorkflowDefinition, map[string]WorkflowParallelSpec) {
	d := WorkflowDefinition{ID: "parallel-test", Title: "Parallel", RequiresInsights: true, EntryNodeID: "fork", Nodes: []WorkflowNode{
		{ID: "fork", Type: "parallel", Label: "Together", Visibility: "all"},
		{ID: "a", Type: "instruction", Label: "First", Visibility: "all", DurationSeconds: 10},
		{ID: "a2", Type: "acknowledgment", Label: "Ack", Visibility: "all", DurationSeconds: 20},
		{ID: "b", Type: "turn", Label: "Second", Visibility: "all", DurationSeconds: 40},
		{ID: "done", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []WorkflowEdge{
		{ID: "fa", Source: "fork", Target: "a"}, {ID: "fb", Source: "fork", Target: "b"},
		{ID: "aa", Source: "a", Target: "a2"}, {ID: "ad", Source: "a2", Target: "done"}, {ID: "bd", Source: "b", Target: "done"},
	}}
	s := map[string]WorkflowParallelSpec{"fork": {Branches: []WorkflowParallelBranchSpec{{ID: "first", EntryNodeID: "a", UntilNodeID: "a2"}, {ID: "second", EntryNodeID: "b", UntilNodeID: "b"}}}}
	return d, s
}

func TestWorkflowParallelRegionsRequireDisjointBoundedJoinAll(t *testing.T) {
	d, specs := parallelDefinition()
	regions, issues := workflowBuildParallels(d, specs)
	require.Empty(t, issues)
	require.Len(t, regions, 1)
	require.Equal(t, "done", regions["fork"].AfterID)
	require.Equal(t, map[string]bool{"a": true, "a2": true}, regions["fork"].Branches[0].Body)
	for _, tc := range []struct {
		name string
		edit func(*WorkflowDefinition, map[string]WorkflowParallelSpec)
	}{
		{"one branch", func(_ *WorkflowDefinition, s map[string]WorkflowParallelSpec) {
			s["fork"] = WorkflowParallelSpec{Branches: s["fork"].Branches[:1]}
		}},
		{"duplicate id", func(_ *WorkflowDefinition, s map[string]WorkflowParallelSpec) { s["fork"].Branches[1].ID = "first" }},
		{"duplicate entry", func(_ *WorkflowDefinition, s map[string]WorkflowParallelSpec) {
			s["fork"].Branches[1].EntryNodeID = "a"
		}},
		{"missing entry edge", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Edges[1].Target = "a" }},
		{"different joins", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Edges[4].Target = "a2" }},
		{"early exit", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) {
			d.Edges = append(d.Edges, WorkflowEdge{ID: "early", Source: "a", Target: "done"})
		}},
		{"external entry", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) {
			d.Edges = append(d.Edges, WorkflowEdge{ID: "cross", Source: "b", Target: "a2"})
		}},
		{"cycle", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Edges[2].Target = "a" }},
		{"controller timer", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[0].DurationSeconds = 1 }},
		{"controller readiness", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[0].Requirements.Video = true }},
		{"presentation", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[1].Type = "presentation" }},
		{"effect", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[1].Type = "effect" }},
		{"nested repeat", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[1].Type = "repeat" }},
		{"nested parallel", func(d *WorkflowDefinition, _ map[string]WorkflowParallelSpec) { d.Nodes[1].Type = "parallel" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, s := parallelDefinition()
			tc.edit(&d, s)
			_, issues := workflowBuildParallels(d, s)
			require.NotEmpty(t, issues)
		})
	}
}

func TestWorkflowParallelRegionAllowsConditionalGuidanceWithinOneBranch(t *testing.T) {
	d, s := parallelDefinition()
	d.Nodes[1].Type = "condition"
	d.Nodes = append(d.Nodes, WorkflowNode{ID: "optional", Type: "instruction", Label: "Optional", Visibility: "role", RoleID: "helper"})
	d.Edges = append(d.Edges, WorkflowEdge{ID: "ao", Source: "a", Target: "optional"}, WorkflowEdge{ID: "oa", Source: "optional", Target: "a2"})
	regions, issues := workflowBuildParallels(d, s)
	require.Empty(t, issues)
	require.True(t, regions["fork"].Branches[0].Body["optional"])
	// Ordinary edge order cannot change the declared branch identities/order.
	slices.Reverse(d.Edges)
	again, issues := workflowBuildParallels(d, s)
	require.Empty(t, issues)
	require.Equal(t, regions, again)
}

func TestWorkflowParallelSimulationUsesMaximumTimeAndSummedVisits(t *testing.T) {
	d, specs := parallelDefinition()
	spec := specs["fork"]
	d.Nodes[0].Parallel = &spec
	require.Empty(t, ValidateWorkflow(d))
	result := SimulateWorkflow(d)
	require.Equal(t, int64(40), result.MaximumSeconds)
	require.Equal(t, 5, result.MaximumVisits)
	require.Equal(t, []string{"fork", "a", "a2", "b", "done"}, result.NodeIDs)
	d.Nodes[0].Parallel.Branches = append(d.Nodes[0].Parallel.Branches, d.Nodes[0].Parallel.Branches[0])
	require.NotEmpty(t, ValidateWorkflow(d))
}
