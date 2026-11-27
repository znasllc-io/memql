package calls

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func repeatDefinition() (WorkflowDefinition, map[string]WorkflowRepeatSpec) {
	d := WorkflowDefinition{ID: "repeat-test", Title: "Practice rounds", RequiresInsights: true, EntryNodeID: "rounds", Nodes: []WorkflowNode{
		{ID: "rounds", Type: "repeat", Label: "Practice", Visibility: "all"},
		{ID: "choice", Type: "condition", Label: "Choose", Visibility: "all", DurationSeconds: 2},
		{ID: "short", Type: "instruction", Label: "Short exercise", Visibility: "all", DurationSeconds: 10},
		{ID: "long", Type: "instruction", Label: "Long exercise", Visibility: "all", DurationSeconds: 20},
		{ID: "review", Type: "acknowledgment", Label: "Review", Visibility: "all", DurationSeconds: 5},
		{ID: "after", Type: "instruction", Label: "Wrap up", Visibility: "all", DurationSeconds: 7},
		{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []WorkflowEdge{
		{ID: "enter", Source: "rounds", Target: "choice"},
		{ID: "short", Source: "choice", Target: "short"},
		{ID: "long", Source: "choice", Target: "long"},
		{ID: "short-review", Source: "short", Target: "review"},
		{ID: "long-review", Source: "long", Target: "review"},
		{ID: "leave", Source: "review", Target: "after"},
		{ID: "finish", Source: "after", Target: "finish"},
	}}
	return d, map[string]WorkflowRepeatSpec{"rounds": {Count: 3, UntilNodeID: "review"}}
}

func TestWorkflowRepeatRegionsAndWorstCaseRoute(t *testing.T) {
	d, specs := repeatDefinition()
	regions, issues := workflowBuildRepeats(d, specs)
	require.Empty(t, issues)
	require.Len(t, regions["rounds"].Body, 4)
	plan := workflowPlan(d, regions)
	require.EqualValues(t, (2+20+5)*3+7, plan.seconds)
	require.Equal(t, 12, plan.visits)
	require.Equal(t, []string{"rounds", "choice", "long", "review", "choice", "long", "review", "choice", "long", "review", "after", "finish"}, plan.nodes)
	require.Empty(t, workflowRepeatBounds(d, regions))
	// A zero-time alternative can have more visits than the maximum-time route.
	d.Nodes = append(d.Nodes, WorkflowNode{ID: "extra", Type: "instruction", Label: "Extra", Visibility: "all"})
	d.Edges[3].Target = "extra"
	d.Edges = append(d.Edges, WorkflowEdge{ID: "extra-review", Source: "extra", Target: "review"})
	regions, issues = workflowBuildRepeats(d, specs)
	require.Empty(t, issues)
	plan = workflowPlan(d, regions)
	require.EqualValues(t, (2+20+5)*3+7, plan.seconds)
	require.Equal(t, 15, plan.visits)
}

func TestWorkflowRepeatRejectsUnstructuredAndUnboundedRegions(t *testing.T) {
	for name, change := range map[string]func(*WorkflowDefinition, map[string]WorkflowRepeatSpec){
		"zero": func(_ *WorkflowDefinition, specs map[string]WorkflowRepeatSpec) {
			specs["rounds"] = WorkflowRepeatSpec{Count: 0, UntilNodeID: "review"}
		},
		"unbounded": func(_ *WorkflowDefinition, specs map[string]WorkflowRepeatSpec) {
			specs["rounds"] = WorkflowRepeatSpec{Count: 9, UntilNodeID: "review"}
		},
		"missing-exit": func(_ *WorkflowDefinition, specs map[string]WorkflowRepeatSpec) {
			specs["rounds"] = WorkflowRepeatSpec{Count: 2, UntilNodeID: "missing"}
		},
		"early-exit": func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Edges[3].Target = "after" },
		"external-entry": func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) {
			d.Edges = append(d.Edges, WorkflowEdge{ID: "wrong-entry", Source: "after", Target: "short"})
		},
		"cycle":            func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Edges[3].Target = "choice" },
		"finish-in-body":   func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[2].Type = "finish" },
		"parallel-in-body": func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[2].Type = "parallel" },
		"nested": func(d *WorkflowDefinition, specs map[string]WorkflowRepeatSpec) {
			d.Nodes[2].Type = "repeat"
			specs["short"] = WorkflowRepeatSpec{Count: 2, UntilNodeID: "review"}
		},
		"controller-timer":           func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[0].DurationSeconds = 1 },
		"controller-readiness":       func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[0].Requirements.Video = true },
		"controller-role":            func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[0].RoleID = "speaker" },
		"controller-role-visibility": func(d *WorkflowDefinition, _ map[string]WorkflowRepeatSpec) { d.Nodes[0].Visibility = "role" },
		"overlapping-controllers": func(d *WorkflowDefinition, specs map[string]WorkflowRepeatSpec) {
			d.Nodes = append(d.Nodes, WorkflowNode{ID: "other-rounds", Type: "repeat", Label: "Other rounds", Visibility: "all"})
			d.Edges = append(d.Edges, WorkflowEdge{ID: "overlap", Source: "other-rounds", Target: "choice"})
			specs["other-rounds"] = WorkflowRepeatSpec{Count: 2, UntilNodeID: "review"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, specs := repeatDefinition()
			change(&d, specs)
			_, issues := workflowBuildRepeats(d, specs)
			require.NotEmpty(t, issues)
		})
	}
	d, specs := repeatDefinition()
	d.Nodes[3].DurationSeconds = 14400
	specs["rounds"] = WorkflowRepeatSpec{Count: 8, UntilNodeID: "review"}
	regions, issues := workflowBuildRepeats(d, specs)
	require.Empty(t, issues)
	require.NotEmpty(t, workflowRepeatBounds(d, regions))
}

func TestWorkflowRepeatVisitBoundIsIndependentOfTime(t *testing.T) {
	d, specs := repeatDefinition()
	specs["rounds"] = WorkflowRepeatSpec{Count: 8, UntilNodeID: "review"}
	d.Edges[4].Target = "extra-0"
	for i := 0; i < 32; i++ {
		id, target := fmt.Sprintf("extra-%d", i), fmt.Sprintf("extra-%d", i+1)
		if i == 31 {
			target = "review"
		}
		d.Nodes = append(d.Nodes, WorkflowNode{ID: id, Type: "instruction", Label: "Extra", Visibility: "all"})
		d.Edges = append(d.Edges, WorkflowEdge{ID: id + "-next", Source: id, Target: target})
	}
	regions, issues := workflowBuildRepeats(d, specs)
	require.Empty(t, issues)
	plan := workflowPlan(d, regions)
	require.Less(t, plan.seconds, int64(86400))
	require.Greater(t, plan.visits, 256)
	require.NotEmpty(t, workflowRepeatBounds(d, regions))
}
