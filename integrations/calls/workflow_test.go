package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestWorkflowCatalogIsDeclarativeAndValid(t *testing.T) {
	catalog, err := workflowCatalog()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(catalog), 14)
	seen := map[string]bool{}
	for _, d := range catalog {
		require.False(t, seen[d.ID])
		seen[d.ID] = true
		require.Empty(t, callrules.ValidateWorkflow(d))
		require.Equal(t, "published", d.Status)
	}
}
func TestWorkflowProjectionRedactsPrivateContentAndKeepsOneOvertimeItem(t *testing.T) {
	now := time.Now().UTC()
	catalog, err := workflowCatalog()
	require.NoError(t, err)
	d := catalog[0]
	d.Nodes[0].Instructions = "private assessment"
	d.Nodes[0].Visibility = "host"
	r := callrules.WorkflowRun{ID: "run", Definition: d, Bindings: map[string]string{"facilitator": "host"}, Revision: 1, ControlEpoch: 1, Status: "running"}
	workflowEnter(&r, d.EntryNodeID, now)
	r.DurationSeconds = 2
	guest := seatRow{ID: "guest"}
	projected := workflowProjection(r, guest, false, now.Add(5*time.Second))
	require.Empty(t, projected.Definition.Instructions)
	require.Empty(t, projected.Definition.Nodes[0].Instructions)
	require.Equal(t, "Private step", projected.Definition.Nodes[0].Label)
	require.Empty(t, projected.Events)
	require.Empty(t, projected.AllowedActions)
	require.Equal(t, "private assessment", r.Definition.Nodes[0].Instructions, "redaction must not mutate durable state")
	host := seatRow{ID: "host", Host: true}
	a := workflowProjection(r, host, false, now.Add(5*time.Second))
	b := workflowProjection(r, host, false, now.Add(6*time.Second))
	require.Equal(t, a.Events[len(a.Events)-1].ID, b.Events[len(b.Events)-1].ID)
	require.EqualValues(t, 3, a.OvertimeSeconds)
	require.EqualValues(t, 4, b.OvertimeSeconds)
	require.Equal(t, "running", r.Status, "overtime must never force a mute or advancement")
	r.Hold = "human"
	require.Empty(t, workflowProjection(r, host, true, now).AllowedActions)
}
func TestWorkflowIdempotencyRejectsChangedPayloadAndSeparatesActors(t *testing.T) {
	v := &workflowState{}
	req := callrules.WorkflowRequest{Action: "pause", IdempotencyKey: "op-001", ExpectedRevision: 4, ExpectedEpoch: 2}
	seen, err := workflowOperationSeen(v, req, "human")
	require.NoError(t, err)
	require.False(t, seen)
	workflowRecord(v, req, "human", 5)
	seen, err = workflowOperationSeen(v, req, "human")
	require.NoError(t, err)
	require.True(t, seen)
	changed := req
	changed.Action = "resume"
	_, err = workflowOperationSeen(v, changed, "human")
	require.ErrorContains(t, err, "workflow_idempotency_conflict")
	seen, err = workflowOperationSeen(v, req, "agent")
	require.NoError(t, err)
	require.False(t, seen)
}
func TestWorkflowRevisitCreatesNewVisitWithoutUndo(t *testing.T) {
	now := time.Now().UTC()
	catalog, _ := workflowCatalog()
	d := catalog[0]
	r := callrules.WorkflowRun{ID: "run", Definition: d}
	workflowEnter(&r, d.Nodes[0].ID, now)
	first := r.VisitID
	workflowEnter(&r, d.Nodes[1].ID, now.Add(4*time.Second))
	workflowEnter(&r, d.Nodes[0].ID, now.Add(6*time.Second))
	require.NotEqual(t, first, r.VisitID)
	require.Len(t, r.History, 2)
	require.EqualValues(t, 4, r.History[0].ElapsedSeconds)
	require.Len(t, r.Events, 3)
}

func TestWorkflowJournalCeilingStillAllowsHostToStop(t *testing.T) {
	v := &workflowState{Operations: make([]workflowOperation, 1000)}
	_, err := workflowOperationSeen(v, callrules.WorkflowRequest{Action: "next", IdempotencyKey: "new-next"}, "host")
	require.ErrorContains(t, err, "workflow_operation_limit")
	req := callrules.WorkflowRequest{Action: "cancel", IdempotencyKey: "emergency-cancel"}
	_, err = workflowOperationSeen(v, req, "host")
	require.NoError(t, err)
	workflowRecord(v, req, "host", 1001)
	require.Len(t, v.Operations, 1000)
	replay, err := workflowOperationSeen(v, req, "host")
	require.NoError(t, err)
	require.True(t, replay)
}
