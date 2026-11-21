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
