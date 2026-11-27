package calls

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowEffectNodeFixture(t *testing.T) muteFixture {
	t.Helper()
	f := workflowMuteFixture(t)
	if f.cw == nil {
		return f
	}
	d := callrules.WorkflowDefinition{ID: "explicit-mute", Title: "Reviewed microphone action", RequiresInsights: true, Roles: f.run.Definition.Roles, EntryNodeID: "mute", Nodes: []callrules.WorkflowNode{
		{ID: "mute", Type: "effect", Label: "Mute the assigned speaker", Visibility: "all", RoleID: "speaker", Effect: &callrules.WorkflowEffectSpec{Action: "end_turn_mute"}},
		{ID: "after", Type: "instruction", Label: "Review result", Visibility: "all"},
		{ID: "finish", Type: "finish", Label: "Done", Visibility: "all"},
	}, Edges: []callrules.WorkflowEdge{{ID: "after", Source: "mute", Target: "after"}, {ID: "done", Source: "after", Target: "finish"}}}
	workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-explicit-mute"})
	command := workflowCommand(f.run, "change", "change-explicit-mute")
	command.SkillID = d.ID
	command.Version = 1
	command.Bindings = f.run.Bindings
	f.run = workflowDo(t, f.cw, f.key, command).Run
	return f
}

func TestWorkflowEffectNodeExplicitIntentReplayAndRevisit(t *testing.T) {
	f := workflowEffectNodeFixture(t)
	if f.cw == nil {
		return
	}
	require.Empty(t, f.cw.media.mutes(), "entering an effect node must never dispatch")
	require.Contains(t, f.run.AllowedActions, "end_turn_mute")
	require.NotContains(t, f.run.AllowedActions, "next")
	require.NotContains(t, f.run.AllowedActions, "end_turn")
	for _, action := range []string{"next", "end_turn"} {
		_, err := f.cw.svc.Workflow(context.Background(), f.cw.ref, f.key.result.Session.Token, workflowCommand(f.run, action, "cannot-skip-effect-"+action))
		require.ErrorContains(t, err, "workflow_effect_required")
	}
	original := workflowCommand(f.run, "end_turn_mute", "explicit-mute-once")
	done := workflowDo(t, f.cw, f.key, original)
	require.Equal(t, "completed", done.Effect.Status)
	require.Equal(t, "after", done.Run.NodeID)
	require.Len(t, f.cw.media.mutes(), 1)
	replay := workflowDo(t, f.cw, f.key, original)
	require.Equal(t, done.Run.VisitID, replay.Run.VisitID)
	require.Len(t, f.cw.media.mutes(), 1)
	revisit := workflowCommand(done.Run, "seek", "revisit-without-effect")
	revisit.NodeID = "mute"
	current := workflowDo(t, f.cw, f.key, revisit)
	require.Equal(t, "mute", current.Run.NodeID)
	require.NotEqual(t, f.run.VisitID, current.Run.VisitID)
	require.Len(t, f.cw.media.mutes(), 1, "seek never recreates an effect")
	replay = workflowDo(t, f.cw, f.key, original)
	require.Equal(t, current.Run.VisitID, replay.Run.VisitID)
	require.Equal(t, "mute", replay.Run.NodeID)
	require.Len(t, f.cw.media.mutes(), 1, "old exact receipt cannot advance a new visit")
	fresh := workflowDo(t, f.cw, f.key, workflowCommand(current.Run, "end_turn_mute", "reviewed-fresh-visit"))
	require.Equal(t, "after", fresh.Run.NodeID)
	require.Equal(t, "completed", fresh.Effect.Status)
	require.Len(t, f.cw.media.mutes(), 2, "only a new explicit reviewed intent may dispatch again")
}

func TestWorkflowEffectNodeUnconfirmedMuteDoesNotAdvance(t *testing.T) {
	f := workflowEffectNodeFixture(t)
	if f.cw == nil {
		return
	}
	attempts := 0
	f.cw.svc.media = f.backend(func(context.Context, string, string, string) error {
		attempts++
		return errors.New("local fixture refuses microphone action")
	}, nil)
	original := workflowCommand(f.run, "end_turn_mute", "unconfirmed-effect")
	answer := workflowDo(t, f.cw, f.key, original)
	require.Equal(t, f.run.NodeID, answer.Run.NodeID)
	require.Equal(t, f.run.VisitID, answer.Run.VisitID)
	require.NotEqual(t, "completed", answer.Effect.Status)
	require.Equal(t, 1, attempts)
	retry := workflowDo(t, f.cw, f.key, original)
	require.Equal(t, f.run.VisitID, retry.Run.VisitID)
	require.Equal(t, 1, attempts, "unknown delivery is reconciled without a second send")
	cancelled := workflowDo(t, f.cw, f.key, workflowCommand(retry.Run, "cancel", "cancel-effect-run"))
	require.Equal(t, "cancelled", cancelled.Run.Status)
	require.Equal(t, f.run.NodeID, cancelled.Run.NodeID)
	require.Equal(t, 1, attempts)
}
