package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func parallelCommand(r *callrules.WorkflowRun, branch, action, nonce string) callrules.WorkflowRequest {
	req := workflowCommand(r, action, nonce)
	req.BranchID = branch
	for _, b := range r.Parallel.Branches {
		if b.ID == branch {
			req.BranchVisitID = b.VisitID
		}
	}
	return req
}

func TestWorkflowParallelDatabaseJoinAllExactVisitsAndActivationHistory(t *testing.T) {
	cw, key, commerce, start := workflowFixture(t, "parallel-join", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, guest, err := cw.join(start.Call.GuestToken, "Guest speaker")
	require.NoError(t, err)
	d := workflowParallelDefinition()
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "parallel-publish"})
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, Bindings: map[string]string{"speaker": guest.SeatID}, IdempotencyKey: "parallel-start"}).Run
	require.NotNil(t, r.Parallel)
	require.NotContains(t, r.AllowedActions, "next")
	_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, workflowCommand(r, "next", "early-join"))
	require.ErrorContains(t, err, "workflow_parallel_join_required")
	staleOtherBranch := parallelCommand(r, "second", "end_turn", "stale-second")
	firstMove := parallelCommand(r, "first", "next", "first-advance")
	r = workflowDo(t, cw, key, firstMove).Run
	require.Equal(t, "a2", r.Parallel.Branches[0].NodeID)
	_, err = cw.svc.Workflow(ctx, cw.ref, guest.Token, staleOtherBranch)
	require.ErrorContains(t, err, "workflow_revision_conflict")
	wrongVisit := parallelCommand(r, "second", "end_turn", "wrong-branch-visit")
	wrongVisit.BranchVisitID = r.Parallel.Branches[0].VisitID
	_, err = cw.svc.Workflow(ctx, cw.ref, guest.Token, wrongVisit)
	require.ErrorContains(t, err, "workflow_branch_visit_conflict")
	other, err := cw.svc.Workflow(ctx, cw.ref, guest.Token, parallelCommand(r, "second", "end_turn", "second-join"))
	require.NoError(t, err)
	r = other.Run
	require.Equal(t, "joined", r.Parallel.Branches[1].Status)
	require.NotContains(t, r.AllowedActions, "next")
	// A participant cannot acknowledge a host-owned branch with no role.
	guestAck := parallelCommand(r, "first", "ack", "guest-wrong-branch")
	guestAck.Acknowledged = true
	_, err = cw.svc.Workflow(ctx, cw.ref, guest.Token, guestAck)
	require.ErrorIs(t, err, callrules.ErrForbidden)
	r = workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"}).Run
	ack := parallelCommand(r, "first", "ack", "first-ack")
	ack.Acknowledged = true
	r = workflowDo(t, cw, key, ack).Run
	oldVisit := r.Parallel.Branches[0].VisitID
	r = workflowDo(t, cw, key, parallelCommand(r, "first", "previous", "first-previous")).Run
	require.Equal(t, "a", r.Parallel.Branches[0].NodeID)
	require.NotEqual(t, oldVisit, r.Parallel.Branches[0].VisitID)
	require.Equal(t, "joined", r.Parallel.Branches[1].Status, "branch navigation cannot reopen a sibling")
	cross := parallelCommand(r, "first", "seek", "cross-branch-seek")
	cross.NodeID = "b"
	_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, cross)
	require.ErrorContains(t, err, "workflow_seek_requires_visited_step")
	seek := parallelCommand(r, "first", "seek", "first-seek")
	seek.NodeID = "a2"
	r = workflowDo(t, cw, key, seek).Run
	require.Empty(t, r.Parallel.Branches[0].AcknowledgedSeatIDs, "revisiting requires a fresh acknowledgment")
	ack = parallelCommand(r, "first", "ack", "first-revisit-ack")
	ack.Acknowledged = true
	r = workflowDo(t, cw, key, ack).Run
	join := parallelCommand(r, "first", "next", "first-join")
	r = workflowDo(t, cw, key, join).Run
	require.Contains(t, r.AllowedActions, "next")
	activation := r.VisitID
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	replay, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, join)
	require.NoError(t, err)
	require.Equal(t, r.Revision, replay.Run.Revision)
	r = workflowDo(t, cw, key, workflowCommand(r, "next", "all-joined")).Run
	require.Equal(t, "after", r.NodeID)
	require.Nil(t, r.Parallel)
	require.True(t, workflowValidParallel(&workflowState{Kind: "run", Run: r}))
	for _, target := range []string{"a", "a2", "b"} {
		req := workflowCommand(r, "seek", "escape-"+target)
		req.NodeID = target
		_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, req)
		require.ErrorContains(t, err, "workflow_seek_requires_visited_step")
	}
	before := append([]callrules.WorkflowVisit(nil), r.History...)
	r = workflowDo(t, cw, key, workflowCommand(r, "previous", "revisit-controller")).Run
	require.Equal(t, "fork", r.NodeID)
	require.NotEqual(t, activation, r.VisitID)
	require.Empty(t, r.Parallel.Branches[0].AcknowledgedSeatIDs)
	require.Equal(t, before, r.History[:len(before)])
	replayed, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, firstMove)
	require.NoError(t, err)
	require.Equal(t, "a", replayed.Run.Parallel.Branches[0].NodeID, "an old exact retry must not execute in the new activation")
	require.Equal(t, r.Revision, replayed.Run.Revision)
	cancel := workflowCommand(r, "cancel", "parallel-cancel")
	r = workflowDo(t, cw, key, cancel).Run
	require.Nil(t, r.Parallel)
	recovered, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, "cancelled", recovered.Run.Status)
	require.True(t, workflowValidParallel(&workflowState{Kind: "run", Run: recovered.Run}))
}

func TestWorkflowParallelDatabaseReadinessFreezesOnlyAffectedBranch(t *testing.T) {
	cw, key, commerce, start := workflowFixture(t, "parallel-readiness", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	joined, guest, err := cw.join(start.Call.GuestToken, "Speaker")
	require.NoError(t, err)
	d := workflowParallelDefinition()
	d.Nodes[1].RoleID, d.Nodes[1].Requirements.Video = "speaker", true
	camera := MediaParticipant{Identity: joined.Identity, SID: "parallel-speaker", Tracks: []MediaTrack{{SID: "camera", Source: "camera"}}}
	cw.media.setPresent(start.Call.ID, camera)
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "parallel-ready-publish"})
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, Bindings: map[string]string{"speaker": guest.SeatID}, IdempotencyKey: "parallel-ready-start"}).Run
	cw.clock.Advance(5 * time.Second)
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: joined.Identity, SID: camera.SID})
	r = workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, "running", r.Status)
	require.Equal(t, "blocked", r.Parallel.Branches[0].Status)
	require.Equal(t, "running", r.Parallel.Branches[1].Status)
	require.Equal(t, int64(5), r.Parallel.Branches[0].ElapsedSeconds)
	cw.clock.Advance(6 * time.Second)
	r = workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, int64(5), r.Parallel.Branches[0].ElapsedSeconds)
	require.Equal(t, int64(11), r.Parallel.Branches[1].ElapsedSeconds)
	_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, parallelCommand(r, "first", "next", "blocked-branch"))
	require.ErrorContains(t, err, "workflow_requirements_unavailable")
	cw.media.setPresent(start.Call.ID, camera)
	r = workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, "blocked", r.Parallel.Branches[0].Status)
	require.Empty(t, r.Parallel.Branches[0].Blockers)
	require.Contains(t, r.AllowedActions, "resume")
	r = workflowDo(t, cw, key, workflowCommand(r, "resume", "resume-ready-branch")).Run
	require.Equal(t, "running", r.Parallel.Branches[0].Status)
	cw.clock.Advance(2 * time.Second)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	out, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, int64(7), out.Run.Parallel.Branches[0].ElapsedSeconds)
	require.Equal(t, int64(13), out.Run.Parallel.Branches[1].ElapsedSeconds)
	r = workflowDo(t, cw, key, workflowCommand(out.Run, "pause", "pause-all-branches")).Run
	cw.clock.Advance(20 * time.Second)
	out, err = replica.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, r.Parallel.Branches[0].ElapsedSeconds, out.Run.Parallel.Branches[0].ElapsedSeconds)
	require.Equal(t, r.Parallel.Branches[1].ElapsedSeconds, out.Run.Parallel.Branches[1].ElapsedSeconds)
}
