package calls

import (
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
	"time"
)

func workflowReadinessDefinition() callrules.WorkflowDefinition {
	return callrules.WorkflowDefinition{ID: "reviewed-readiness", Title: "Camera alternatives", RequiresInsights: true, EntryNodeID: "check", Nodes: []callrules.WorkflowNode{
		{ID: "check", Type: "readiness", Label: "Check camera", Visibility: "host", Requirements: callrules.WorkflowRequirements{Video: true}, Readiness: &callrules.WorkflowReadinessSpec{TimeoutSeconds: 10, Help: "Private facilitator help: offer the approved verbal route if a camera is unavailable.", FallbackNodeID: "alternative"}},
		{ID: "primary", Type: "instruction", Label: "Visual route", Visibility: "all"},
		{ID: "alternative", Type: "instruction", Label: "Verbal route", Visibility: "all"},
		{ID: "done", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []callrules.WorkflowEdge{{ID: "primary", Source: "check", Target: "primary"}, {ID: "alternative", Source: "check", Target: "alternative"}, {ID: "p-done", Source: "primary", Target: "done"}, {ID: "a-done", Source: "alternative", Target: "done"}}}
}
func TestWorkflowReadinessWaitDeadlineRecoveryPrivacyAndOutcomes(t *testing.T) {
	now := time.Now().UTC()
	r := callrules.WorkflowRun{ID: "wait-run", Definition: workflowReadinessDefinition(), Revision: 1, ControlEpoch: 1}
	workflowEnter(&r, "check", now)
	workflowApplyReadiness(&r, []string{"camera_required"}, now)
	wait := *r.Readiness
	require.Equal(t, now.Add(10*time.Second), wait.Deadline)
	_, err := workflowFallbackTarget(&r, now.Add(9*time.Second))
	require.ErrorContains(t, err, "workflow_readiness_timeout_pending")
	require.False(t, workflowReadinessObserve(r.Definition, r.NodeID, r.VisitID, r.Blockers, &r.Readiness, now.Add(5*time.Second)))
	require.Equal(t, wait, *r.Readiness)
	target, err := workflowFallbackTarget(&r, now.Add(11*time.Second))
	require.NoError(t, err)
	require.Equal(t, "alternative", target)
	require.Equal(t, "check", r.NodeID, "deadline never advances by itself")
	host := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now.Add(11*time.Second))
	require.Contains(t, host.AllowedActions, "fallback")
	guest := workflowProjection(r, seatRow{ID: "guest"}, false, now.Add(11*time.Second))
	require.NotContains(t, guest.AllowedActions, "fallback")
	require.Empty(t, guest.Definition.Nodes[0].Readiness.Help)
	require.NotEmpty(t, r.Definition.Nodes[0].Readiness.Help)
	require.True(t, workflowReadinessObserve(r.Definition, r.NodeID, r.VisitID, nil, &r.Readiness, now.Add(12*time.Second)))
	require.Nil(t, r.Readiness)
	workflowReadinessObserve(r.Definition, r.NodeID, r.VisitID, r.Blockers, &r.Readiness, now.Add(15*time.Second))
	require.Equal(t, now.Add(25*time.Second), r.Readiness.Deadline)
	workflowEnter(&r, target, now.Add(26*time.Second), "fallback")
	require.Nil(t, r.Readiness)
	require.Equal(t, "skipped", r.History[0].Outcome)
	require.Equal(t, "fallback", r.History[0].Action)
	workflowEnter(&r, "alternative", now.Add(27*time.Second), "seek")
	require.Equal(t, "revisited", r.History[1].Outcome)
	workflowEnter(&r, "primary", now.Add(28*time.Second), "seek")
	require.Equal(t, "skipped", r.History[2].Outcome)
	workflowAbandonVisit(&r, "cancel", now.Add(29*time.Second))
	require.Equal(t, "abandoned", r.History[3].Outcome)
	require.True(t, workflowValidReadiness(&workflowState{Run: &r}))
	r.History[0].Outcome = "proven_success"
	require.False(t, workflowValidReadiness(&workflowState{Run: &r}))
}
func TestWorkflowReadinessRejectsForgedVisitAndGlobalEligibilityBypass(t *testing.T) {
	now := time.Now()
	r := callrules.WorkflowRun{ID: "wait-run", Definition: workflowReadinessDefinition(), Revision: 1, ControlEpoch: 1}
	workflowEnter(&r, "check", now)
	workflowApplyReadiness(&r, []string{"camera_required"}, now)
	r.Readiness.VisitID = "other-visit"
	require.False(t, workflowValidReadiness(&workflowState{Run: &r}))
	_, err := workflowFallbackTarget(&r, now.Add(time.Hour))
	require.ErrorContains(t, err, "workflow_fallback_unavailable")
	r.Readiness.VisitID = r.VisitID
	r.Blockers = append(r.Blockers, "insights_or_account_unavailable")
	_, err = workflowFallbackTarget(&r, now.Add(time.Hour))
	require.ErrorContains(t, err, "workflow_fallback_unavailable")
}
