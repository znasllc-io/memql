package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowNavigationDefinition() callrules.WorkflowDefinition {
	return callrules.WorkflowDefinition{
		ID: "navigation", Title: "Navigation", RequiresInsights: true, EntryNodeID: "choice",
		Nodes: []callrules.WorkflowNode{
			{ID: "choice", Type: "condition", Label: "Choose", Visibility: "all", DurationSeconds: 10},
			{ID: "a", Type: "instruction", Label: "First branch", Visibility: "all", DurationSeconds: 20},
			{ID: "b", Type: "instruction", Label: "Second branch", Visibility: "all", DurationSeconds: 30},
			{ID: "join", Type: "instruction", Label: "Together", Visibility: "all", DurationSeconds: 40},
			{ID: "later", Type: "instruction", Label: "Later", Visibility: "all", DurationSeconds: 50},
			{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
		},
		Edges: []callrules.WorkflowEdge{
			{ID: "choice-a", Source: "choice", Target: "a"},
			{ID: "choice-b", Source: "choice", Target: "b"},
			{ID: "a-join", Source: "a", Target: "join"},
			{ID: "b-join", Source: "b", Target: "join"},
			{ID: "join-later", Source: "join", Target: "later"},
			{ID: "later-finish", Source: "later", Target: "finish"},
		},
	}
}

func TestWorkflowPreviousWalksAncestorsWithoutRewritingVisits(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "run", Definition: workflowNavigationDefinition()}
	require.Empty(t, callrules.ValidateWorkflow(r.Definition))
	for i, node := range []string{"choice", "a", "join", "later"} {
		workflowEnter(&r, node, now.Add(time.Duration(i)*time.Second))
	}
	journal := append([]callrules.WorkflowVisit(nil), r.History...)
	events := append([]callrules.WorkflowEvent(nil), r.Events...)
	for i, expected := range []string{"join", "a", "choice"} {
		require.Equal(t, expected, workflowPreviousNode(&r))
		previousVisit := r.VisitID
		workflowEnter(&r, expected, now.Add(time.Duration(i+4)*time.Second))
		require.NotEqual(t, previousVisit, r.VisitID)
		require.Zero(t, r.ElapsedSeconds, "a revisit starts its own advisory allowance")
	}
	require.Empty(t, workflowPreviousNode(&r), "Previous must not become redo at the entry")
	require.Equal(t, journal, r.History[:len(journal)])
	require.Equal(t, events, r.Events[:len(events)])
	require.Len(t, r.History, 6)
	projection := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now.Add(7*time.Second))
	require.NotContains(t, projection.AllowedActions, "previous")
	require.Contains(t, projection.AllowedActions, "seek", "previously visited forward steps remain explicitly seekable")
}

func TestWorkflowPreviousUsesTheVisitedBranchAfterSeekAndHistoryExpiry(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "run", Definition: workflowNavigationDefinition()}
	for i, node := range []string{"choice", "a", "join", "choice", "b", "join", "later"} {
		workflowEnter(&r, node, now.Add(time.Duration(i)*time.Second))
	}
	workflowEnter(&r, "join", now.Add(7*time.Second))
	require.Equal(t, "b", workflowPreviousNode(&r), "the latest visited branch leads into the join")
	workflowEnter(&r, "a", now.Add(8*time.Second)) // seek to a previously visited alternative
	require.Equal(t, "choice", workflowPreviousNode(&r), "the other branch and its descendants are not predecessors")
	for i := 0; i < 256; i++ {
		workflowEnter(&r, "a", now.Add(time.Duration(i+9)*time.Second)) // revisiting the same step is legal
	}
	require.Len(t, r.History, 256)
	require.Empty(t, workflowPreviousNode(&r), "an expired ancestor visit is not reconstructed from topology")
	projection := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now.Add(266*time.Second))
	require.NotContains(t, projection.AllowedActions, "previous")
}
