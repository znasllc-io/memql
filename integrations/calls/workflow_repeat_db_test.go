package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
	"time"
)

func TestWorkflowRepeatDatabaseReplayRecoveryAndRoundNavigation(t *testing.T) {
	cw, key, commerce, _ := workflowFixture(t, "workflow-repeat", true)
	if cw == nil {
		return
	}
	d := workflowRepeatDefinition()
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-repeat"})
	current := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-repeat"})
	move := func(action, target, nonce string) {
		t.Helper()
		q := workflowCommand(current.Run, action, nonce)
		q.NodeID = target
		q.Acknowledged = action == "ack"
		current = workflowDo(t, cw, key, q)
	}
	move("next", "", "enter-first-round")
	require.Equal(t, 1, current.Run.Repeat.Iteration)
	activation := current.Run.Repeat.VisitID
	move("next", "", "first-review")
	_, err := cw.svc.Workflow(context.Background(), cw.ref, key.result.Session.Token, workflowCommand(current.Run, "next", "cannot-skip-round-ack"))
	require.ErrorContains(t, err, "workflow_acknowledgment_required")
	move("ack", "", "ack-first-round")
	finishRound := workflowCommand(current.Run, "next", "finish-first-round")
	current = workflowDo(t, cw, key, finishRound)
	require.Equal(t, 2, current.Run.Repeat.Iteration)
	require.Equal(t, activation, current.Run.Repeat.VisitID)
	require.Equal(t, "practice", current.Run.NodeID)
	journal := append([]callrules.WorkflowVisit(nil), current.Run.History...)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	replay, err := replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, finishRound)
	require.NoError(t, err)
	require.Equal(t, current.Run.VisitID, replay.Run.VisitID)
	require.Equal(t, journal, replay.Run.History)
	stale := finishRound
	stale.IdempotencyKey = "stale-round-intent"
	_, err = replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, stale)
	require.ErrorContains(t, err, "workflow_revision_conflict")
	for _, target := range []string{"rounds", "review", "after"} {
		q := workflowCommand(current.Run, "seek", "seek-outside-round-"+target)
		q.NodeID = target
		_, err = replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, q)
		require.ErrorContains(t, err, "workflow_repeat_navigation")
	}
	require.Equal(t, []string{"practice"}, current.Run.SeekableNodeIDs)
	cw.clock.Advance(4 * time.Second)
	move("pause", "", "pause-second-round")
	elapsed := current.Run.ElapsedSeconds
	cw.clock.Advance(time.Minute)
	recovered, err := replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, elapsed, recovered.Run.ElapsedSeconds)
	require.Equal(t, 2, recovered.Run.Repeat.Iteration)
	current = recovered
	move("next", "", "second-review-paused")
	require.Equal(t, "paused", current.Run.Status)
	require.Empty(t, current.Run.AcknowledgedSeatIDs)
	move("ack", "", "ack-second-round")
	move("next", "", "leave-final-round")
	require.Nil(t, current.Run.Repeat)
	require.Equal(t, "after", current.Run.NodeID)
	require.Equal(t, "paused", current.Run.Status)
	require.Equal(t, journal, current.Run.History[:len(journal)])
	require.NotContains(t, current.Run.SeekableNodeIDs, "review")
	require.NotContains(t, current.Run.SeekableNodeIDs, "practice")
	require.Contains(t, current.Run.SeekableNodeIDs, "rounds")
	move("previous", "", "revisit-controller")
	require.Equal(t, "rounds", current.Run.NodeID)
	move("next", "", "new-round-activation")
	require.Equal(t, 1, current.Run.Repeat.Iteration)
	require.NotEqual(t, activation, current.Run.Repeat.VisitID)
	require.Empty(t, cw.media.mutes(), "ordinary repeat navigation never dispatches a media effect")
	move("cancel", "", "cancel-repeat")
	require.Nil(t, current.Run.Repeat)
}
func TestWorkflowRepeatCombinedMuteAdvancesOnlyOneRoundWithoutReplay(t *testing.T) {
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return
	}
	cw := f.cw
	d := callrules.WorkflowDefinition{ID: "repeat-mute", Title: "Speaking rounds", RequiresInsights: true, Roles: f.run.Definition.Roles, EntryNodeID: "rounds", Nodes: []callrules.WorkflowNode{
		{ID: "rounds", Type: "repeat", Label: "Two rounds", Visibility: "all", Repeat: &callrules.WorkflowRepeatSpec{Count: 2, UntilNodeID: "turn"}},
		{ID: "turn", Type: "turn", Label: "Speak", Visibility: "all", RoleID: "speaker", DurationSeconds: 60},
		{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []callrules.WorkflowEdge{{ID: "enter", Source: "rounds", Target: "turn"}, {ID: "finish", Source: "turn", Target: "finish"}}}
	workflowDo(t, cw, f.key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-repeat-mute"})
	change := workflowCommand(f.run, "change", "change-repeat-mute")
	change.SkillID = d.ID
	change.Version = 1
	change.Bindings = f.run.Bindings
	current := workflowDo(t, cw, f.key, change)
	current = workflowDo(t, cw, f.key, workflowCommand(current.Run, "next", "begin-speaking-round"))
	require.Equal(t, 1, current.Run.Repeat.Iteration)
	command := workflowCommand(current.Run, "end_turn_mute", "finish-speaking-round-once")
	current = workflowDo(t, cw, f.key, command)
	require.Equal(t, "completed", current.Effect.Status)
	require.Equal(t, "turn", current.Run.NodeID)
	require.Equal(t, 2, current.Run.Repeat.Iteration)
	require.Len(t, cw.media.mutes(), 1)
	replay := workflowDo(t, cw, f.key, command)
	require.Equal(t, "completed", replay.Effect.Status)
	require.Equal(t, current.Run.VisitID, replay.Run.VisitID)
	require.Equal(t, 2, replay.Run.Repeat.Iteration)
	require.Len(t, cw.media.mutes(), 1)
	current = workflowDo(t, cw, f.key, workflowCommand(current.Run, "end_turn", "finish-speaking-without-mute"))
	require.Equal(t, "completed", current.Run.Status)
	require.Nil(t, current.Run.Repeat)
	require.Len(t, cw.media.mutes(), 1)
}
