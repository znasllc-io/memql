package calls

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestWorkflowProviderOutageFreezesAndRestorationRequiresResume(t *testing.T) {
	cw, key, _, _ := workflowPrivateMediaFixture(t)
	if cw == nil {
		return
	}
	ctx := context.Background()
	before := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	cw.clock.Advance(3 * time.Second)
	cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		backend, err := cw.media.backend(server, credential)
		return delayedHostInventory{MediaBackend: backend, pause: func(context.Context) error { return errors.New("synthetic inventory outage") }}, err
	}
	require.NoError(t, cw.svc.ReconcileWorkflows(ctx))
	blocked := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", blocked.Run.Status)
	require.Contains(t, blocked.Run.Blockers, "media_readiness_unavailable")
	require.Greater(t, blocked.Run.ControlEpoch, before.Run.ControlEpoch)
	require.EqualValues(t, 3, blocked.Run.ElapsedSeconds)
	cw.clock.Advance(10 * time.Second)
	again := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, blocked.Run.Revision, again.Run.Revision, "persistent outage must not write repeated state/events")
	require.EqualValues(t, 3, again.Run.ElapsedSeconds)
	cw.svc.media = cw.media.backend
	restored := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", restored.Run.Status)
	require.Empty(t, restored.Run.Blockers)
	require.Equal(t, before.Run.VisitID, restored.Run.VisitID, "restoration must not replay the visit")
	require.NotContains(t, restored.Run.AllowedActions, "next")
	_, err := cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, workflowCommand(before.Run, "next", "before-outage-next"))
	require.ErrorContains(t, err, "workflow_revision_conflict")
	resumed := workflowDo(t, cw, key, workflowCommand(restored.Run, "resume", "after-outage-resume"))
	require.Equal(t, "running", resumed.Run.Status)
	require.EqualValues(t, 3, resumed.Run.ElapsedSeconds)
	require.Empty(t, cw.media.mutes(), "readiness is guidance, not forced media control")
}

func TestWorkflowScreenShareReadinessBelongsToTheAssignedPresenter(t *testing.T) {
	cw, key, _, started := workflowFixture(t, "workflow-share-readiness", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	guest, _, err := cw.join(started.Call.GuestToken, "Other sharer")
	require.NoError(t, err)
	share := MediaTrack{SID: "assigned-share", Source: "screen_share"}
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(guest.Identity, share))
	active := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "demonstration", Version: 1, IdempotencyKey: "share-required-start"})
	require.Equal(t, "blocked", active.Run.Status)
	require.Contains(t, active.Run.Blockers, "presentation_source_required", "another seat's share must not satisfy the facilitator requirement")
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity, share), person(guest.Identity))
	restored := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", restored.Run.Status)
	require.Empty(t, restored.Run.Blockers)
	running := workflowDo(t, cw, key, workflowCommand(restored.Run, "resume", "share-ready-resume"))
	cw.clock.Advance(2 * time.Second)
	share.Muted = true
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity, share), person(guest.Identity))
	require.NoError(t, cw.svc.ReconcileWorkflows(ctx))
	blocked := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", blocked.Run.Status)
	require.Contains(t, blocked.Run.Blockers, "presentation_source_required")
	require.EqualValues(t, 2, blocked.Run.ElapsedSeconds)
	require.Greater(t, blocked.Run.ControlEpoch, running.Run.ControlEpoch)
	require.Empty(t, cw.media.mutes())
}

func TestWorkflowSupervisorMustRemainConnectedAndRestoreExplicitly(t *testing.T) {
	cw, key, _, started := workflowFixture(t, "workflow-supervisor-readiness", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, _, err := cw.svc.session(ctx, cw.ref, key.result.Session.Token, cw.clock.Now())
	require.NoError(t, err)
	guest, _, err := cw.join(started.Call.GuestToken, "Adult learner fixture")
	require.NoError(t, err)
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(guest.Identity))
	running := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "supervised-learning", Version: 1, IdempotencyKey: "supervisor-start", Bindings: map[string]string{"supervisor": host.SeatID}})
	require.Equal(t, "running", running.Run.Status)
	cw.clock.Advance(4 * time.Second)
	// An admitted database seat alone cannot stand in for the connected supervisor.
	cw.media.setPresent(started.Call.ID, person(guest.Identity))
	require.NoError(t, cw.svc.ReconcileWorkflows(ctx))
	blocked := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", blocked.Run.Status)
	require.Contains(t, blocked.Run.Blockers, "supervisor_not_connected")
	require.EqualValues(t, 4, blocked.Run.ElapsedSeconds)
	cw.clock.Advance(5 * time.Second)
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity), person(guest.Identity))
	require.NoError(t, cw.svc.ReconcileWorkflows(ctx))
	restored := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", restored.Run.Status)
	require.Empty(t, restored.Run.Blockers)
	require.Equal(t, running.Run.VisitID, restored.Run.VisitID)
	require.EqualValues(t, 4, restored.Run.ElapsedSeconds)
	resumed := workflowDo(t, cw, key, workflowCommand(restored.Run, "resume", "supervisor-returned"))
	require.Equal(t, "running", resumed.Run.Status)
	require.Empty(t, cw.media.mutes())
}
