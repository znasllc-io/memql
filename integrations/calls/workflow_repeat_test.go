package calls

import (
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
	"time"
)

func workflowRepeatDefinition() callrules.WorkflowDefinition {
	return callrules.WorkflowDefinition{ID: "practice-rounds", Title: "Practice rounds", RequiresInsights: true, EntryNodeID: "rounds", Nodes: []callrules.WorkflowNode{
		{ID: "rounds", Type: "repeat", Label: "Two practice rounds", Visibility: "all", Repeat: &callrules.WorkflowRepeatSpec{Count: 2, UntilNodeID: "review"}},
		{ID: "practice", Type: "instruction", Label: "Practice", Visibility: "all", DurationSeconds: 10},
		{ID: "review", Type: "acknowledgment", Label: "Review", Visibility: "all", DurationSeconds: 5},
		{ID: "after", Type: "instruction", Label: "Wrap up", Visibility: "all", DurationSeconds: 7},
		{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []callrules.WorkflowEdge{{ID: "enter", Source: "rounds", Target: "practice"}, {ID: "review", Source: "practice", Target: "review"}, {ID: "leave", Source: "review", Target: "after"}, {ID: "done", Source: "after", Target: "finish"}}}
}
func TestWorkflowRepeatRuntimeRoundsVisitsAndNavigation(t *testing.T) {
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "repeat-run", Definition: workflowRepeatDefinition(), Revision: 1, ControlEpoch: 1}
	require.Empty(t, callrules.ValidateWorkflow(r.Definition))
	workflowEnter(&r, "rounds", now)
	require.True(t, workflowMove(&r, "practice", true, now.Add(time.Second)))
	activation := r.Repeat.VisitID
	require.Equal(t, 1, r.Repeat.Iteration)
	require.False(t, workflowCanSeek(&r, "rounds"))
	require.False(t, workflowCanSeek(&r, "after"))
	require.True(t, workflowMove(&r, "review", true, now.Add(5*time.Second)))
	r.AcknowledgedSeatIDs = []string{"host"}
	firstJournal := append([]callrules.WorkflowVisit(nil), r.History...)
	before := r.VisitID
	require.False(t, workflowMove(&r, "after", true, now.Add(10*time.Second)))
	require.Equal(t, before, r.VisitID)
	require.True(t, workflowMove(&r, "practice", true, now.Add(10*time.Second)))
	require.Equal(t, 2, r.Repeat.Iteration)
	require.Equal(t, activation, r.Repeat.VisitID)
	require.NotEqual(t, before, r.VisitID)
	require.Empty(t, r.AcknowledgedSeatIDs)
	require.Empty(t, workflowPreviousNode(&r))
	require.False(t, workflowCanSeek(&r, "review"))
	require.Equal(t, firstJournal, r.History[:len(firstJournal)])
	require.Equal(t, 1, r.History[len(r.History)-1].RepeatIteration)
	projected := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now.Add(10*time.Second))
	require.Equal(t, []string{"practice"}, projected.SeekableNodeIDs)
	guest := workflowProjection(r, seatRow{ID: "guest"}, false, now.Add(10*time.Second))
	require.Empty(t, guest.SeekableNodeIDs)
	require.True(t, workflowMove(&r, "review", true, now.Add(12*time.Second)))
	require.Equal(t, "practice", workflowPreviousNode(&r))
	require.True(t, workflowMove(&r, "after", true, now.Add(15*time.Second)))
	require.Nil(t, r.Repeat)
	require.Equal(t, 2, r.History[len(r.History)-1].RepeatIteration)
	require.False(t, workflowCanSeek(&r, "practice"))
	require.False(t, workflowCanSeek(&r, "review"))
	require.True(t, workflowCanSeek(&r, "rounds"))
	require.Equal(t, "rounds", workflowPreviousNode(&r))
	require.True(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &r}))
	require.True(t, workflowMove(&r, "rounds", false, now.Add(16*time.Second)))
	require.True(t, workflowMove(&r, "practice", true, now.Add(17*time.Second)))
	require.Equal(t, 1, r.Repeat.Iteration)
	require.NotEqual(t, activation, r.Repeat.VisitID)
}
func TestWorkflowRepeatStateRejectsInventedRoundAuthority(t *testing.T) {
	now := time.Now()
	r := callrules.WorkflowRun{ID: "repeat-run", Definition: workflowRepeatDefinition(), Revision: 1, ControlEpoch: 1}
	workflowEnter(&r, "rounds", now)
	require.True(t, workflowMove(&r, "practice", true, now))
	require.True(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &r}))
	for name, mutate := range map[string]func(*callrules.WorkflowRun){
		"missing-frame":       func(r *callrules.WorkflowRun) { r.Repeat = nil },
		"wrong-controller":    func(r *callrules.WorkflowRun) { r.Repeat.NodeID = "practice" },
		"wrong-count":         func(r *callrules.WorkflowRun) { r.Repeat.Count = 8 },
		"exhausted":           func(r *callrules.WorkflowRun) { r.Repeat.Iteration = 3 },
		"invented-activation": func(r *callrules.WorkflowRun) { r.Repeat.VisitID = r.VisitID },
		"outside-body":        func(r *callrules.WorkflowRun) { r.NodeID = "after" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := r
			frame := *r.Repeat
			copy.Repeat = &frame
			mutate(&copy)
			require.False(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &copy}))
		})
	}
}

func TestWorkflowRepeatRetainedVisitsNeverInventEarlierRounds(t *testing.T) {
	now := time.Now()
	r := callrules.WorkflowRun{ID: "repeat-run", Definition: workflowRepeatDefinition(), Revision: 1, ControlEpoch: 1}
	workflowEnter(&r, "rounds", now)
	require.True(t, workflowMove(&r, "practice", true, now))
	activation := r.Repeat.VisitID
	for range 140 {
		require.True(t, workflowMove(&r, "review", true, now))
		require.True(t, workflowMove(&r, "practice", false, now))
	}
	require.Len(t, r.History, 256)
	require.NotEqual(t, activation, r.History[0].VisitID)
	require.True(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &r}))
	require.True(t, workflowMove(&r, "review", true, now))
	require.True(t, workflowMove(&r, "practice", true, now))
	require.Equal(t, 2, r.Repeat.Iteration)
	require.Empty(t, workflowPreviousNode(&r))
	require.False(t, workflowCanSeek(&r, "review"), "retained visits from a prior round cannot authorize a current-round seek")
	require.True(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &r}))
	// Retaining a body visit does not let it masquerade as the controller activation.
	copy := r
	frame := *r.Repeat
	copy.Repeat = &frame
	copy.Repeat.VisitID = r.History[len(r.History)-1].VisitID
	require.False(t, workflowValidRepeat(&workflowState{Kind: "run", Run: &copy}))
}
