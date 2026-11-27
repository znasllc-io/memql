package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestWorkflowDatabasePreviousSeekReplayAndAcknowledgment(t *testing.T) {
	cw, key, commerce, _ := workflowFixture(t, "workflow-navigation", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	d := workflowNavigationDefinition()
	d.Nodes[1].Requirements.Acknowledgment = true
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-navigation"})
	current := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-navigation"})
	move := func(action, target, nonce string) {
		t.Helper()
		cw.clock.Advance(time.Second)
		req := workflowCommand(current.Run, action, nonce)
		req.NodeID = target
		req.Acknowledged = action == "ack"
		current = workflowDo(t, cw, key, req)
	}
	move("next", "a", "choose-a")
	move("ack", "", "ack-a")
	move("next", "", "a-to-join")
	move("next", "", "join-to-later")
	move("pause", "", "pause-later")
	journal := append([]callrules.WorkflowVisit(nil), current.Run.History...)

	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	refreshed, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, current.Run.VisitID, refreshed.Run.VisitID)
	previous := workflowCommand(refreshed.Run, "previous", "previous-join")
	current = workflowDo(t, cw, key, previous)
	require.Equal(t, "join", current.Run.NodeID)
	require.Equal(t, "paused", current.Run.Status)
	require.Equal(t, "human", current.Run.Hold)
	replay, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, previous)
	require.NoError(t, err)
	require.Equal(t, current.Run.Revision, replay.Run.Revision)
	require.Equal(t, current.Run.History, replay.Run.History, "an exact replay cannot append another visit")

	move("previous", "", "previous-a")
	require.Equal(t, "a", current.Run.NodeID)
	require.Empty(t, current.Run.AcknowledgedSeatIDs, "revisiting starts a new acknowledgment requirement")
	require.EqualValues(t, 20, current.Run.DurationSeconds)
	require.Zero(t, current.Run.ElapsedSeconds)
	_, err = replica.Workflow(ctx, cw.ref, key.result.Session.Token, workflowCommand(current.Run, "previous", "cannot-skip-ack"))
	require.ErrorContains(t, err, "workflow_acknowledgment_required")
	move("ack", "", "ack-a-revisit")
	move("previous", "", "previous-choice")
	require.Equal(t, "choice", current.Run.NodeID)
	require.Equal(t, "paused", current.Run.Status)
	require.NotContains(t, current.Run.AllowedActions, "previous")
	require.Contains(t, current.Run.AllowedActions, "seek")
	require.Equal(t, journal, current.Run.History[:len(journal)], "the original visits survive every revisit")
	_, err = replica.Workflow(ctx, cw.ref, key.result.Session.Token, workflowCommand(current.Run, "previous", "no-earlier-visit"))
	require.ErrorContains(t, err, "workflow_no_previous")

	// Seek is explicit and can revisit a later step. Previous from that step
	// follows the latest visited ancestor, not the forward step just left.
	move("seek", "join", "seek-join")
	move("previous", "", "seek-back-to-choice")
	require.Equal(t, "choice", current.Run.NodeID)
	move("next", "b", "choose-b")
	move("next", "", "b-to-join")
	move("previous", "", "previous-b")
	require.Equal(t, "b", current.Run.NodeID)
	move("previous", "", "branch-back-to-choice")
	require.Equal(t, "choice", current.Run.NodeID)
	require.Empty(t, cw.media.mutes(), "navigation never repeats media effects")
	move("cancel", "", "cancel-navigation")
}
