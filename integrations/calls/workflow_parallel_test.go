package calls

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowParallelDefinition() callrules.WorkflowDefinition {
	return callrules.WorkflowDefinition{ID: "parallel-guidance", Title: "Parallel guidance", RequiresInsights: true, EntryNodeID: "fork", Roles: []callrules.WorkflowRole{{ID: "speaker", Label: "Speaker"}}, Nodes: []callrules.WorkflowNode{
		{ID: "fork", Type: "parallel", Label: "Together", Visibility: "all", Parallel: &callrules.WorkflowParallelSpec{Branches: []callrules.WorkflowParallelBranchSpec{{ID: "first", EntryNodeID: "a", UntilNodeID: "a2"}, {ID: "second", EntryNodeID: "b", UntilNodeID: "b"}}}},
		{ID: "a", Type: "instruction", Label: "Private instructions", Instructions: "Host secret", Visibility: "host", DurationSeconds: 10},
		{ID: "a2", Type: "acknowledgment", Label: "Confirm", Visibility: "all", DurationSeconds: 20},
		{ID: "b", Type: "turn", Label: "Speak", Visibility: "all", RoleID: "speaker", DurationSeconds: 40},
		{ID: "after", Type: "instruction", Label: "Together again", Visibility: "all"},
		{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []callrules.WorkflowEdge{{ID: "fa", Source: "fork", Target: "a"}, {ID: "fb", Source: "fork", Target: "b"}, {ID: "aa", Source: "a", Target: "a2"}, {ID: "ad", Source: "a2", Target: "after"}, {ID: "bd", Source: "b", Target: "after"}, {ID: "end", Source: "after", Target: "finish"}}}
}
func TestWorkflowParallelClocksProjectionAndStateValidation(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "parallel-run", Definition: workflowParallelDefinition(), Revision: 1, ControlEpoch: 1, Bindings: map[string]string{"speaker": "guest"}}
	require.Empty(t, callrules.ValidateWorkflow(r.Definition))
	workflowEnter(&r, "fork", now)
	require.Len(t, r.Parallel.Branches, 2)
	require.NotEqual(t, r.Parallel.Branches[0].VisitID, r.Parallel.Branches[1].VisitID)
	require.True(t, workflowValidParallel(&workflowState{Kind: "run", Run: &r}))
	require.Empty(t, workflowForwardTargets(&r))
	before, _ := json.Marshal(r)
	guest := workflowProjection(r, seatRow{ID: "guest"}, false, now.Add(5*time.Second))
	require.Equal(t, int64(5), guest.Parallel.Branches[0].ElapsedSeconds)
	require.Contains(t, guest.Parallel.Branches[1].AllowedActions, "end_turn")
	require.Empty(t, guest.Parallel.Branches[0].AllowedActions)
	after, _ := json.Marshal(r)
	require.Equal(t, before, after, "projection must not mutate persisted branch slices or clocks")
	projected, _ := json.Marshal(guest)
	require.NotContains(t, string(projected), "Host secret")
	require.NotContains(t, string(projected), "Private instructions")
	r.Hold = "human"
	agent := workflowProjection(r, seatRow{ID: "agent", Host: true}, true, now)
	require.Empty(t, agent.AllowedActions)
	for _, branch := range agent.Parallel.Branches {
		require.Empty(t, branch.AllowedActions, "human hold fences every delegated branch control")
	}
	r.Hold = ""
	workflowFreeze(&r, now.Add(7*time.Second))
	r.Status = "paused"
	require.Equal(t, int64(7), workflowBranchElapsed(&r, r.Parallel.Branches[0], now.Add(time.Hour)))
	r.Status = "running"
	workflowParallelResume(&r, now.Add(time.Hour))
	require.Equal(t, int64(9), workflowBranchElapsed(&r, r.Parallel.Branches[0], now.Add(time.Hour+2*time.Second)))
	workflowParallelResume(&r, now.Add(time.Hour+2*time.Second))
	require.Equal(t, int64(10), workflowBranchElapsed(&r, r.Parallel.Branches[0], now.Add(time.Hour+3*time.Second)), "resuming another branch cannot reset a running clock")
	for _, mutate := range []func(*callrules.WorkflowRun){
		func(r *callrules.WorkflowRun) { r.Parallel = nil },
		func(r *callrules.WorkflowRun) { r.Parallel.ControllerVisitID = "invented" },
		func(r *callrules.WorkflowRun) { r.Parallel.Branches[0].VisitID = r.Parallel.Branches[1].VisitID },
		func(r *callrules.WorkflowRun) { r.Parallel.Branches[0].NodeID = "b" },
		func(r *callrules.WorkflowRun) { r.Parallel.Branches[0].Status = "joined" },
	} {
		var copy callrules.WorkflowRun
		raw, _ := json.Marshal(r)
		require.NoError(t, json.Unmarshal(raw, &copy))
		mutate(&copy)
		require.False(t, workflowValidParallel(&workflowState{Kind: "run", Run: &copy}))
	}
}
