package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func TestWorkflowPlayerDatabaseFailedChangeRetainsPausedCheckpoint(t *testing.T) {
	cw, key, commerce, start := workflowFixture(t, "change-checkpoint", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "checkpoint-start"}).Run
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	forbidden := workflowCommand(r, "change", "guest-change")
	forbidden.SkillID, forbidden.Version = "missing", 1
	_, err = cw.svc.Workflow(ctx, cw.ref, guest.Token, forbidden)
	require.ErrorIs(t, err, callrules.ErrForbidden)
	unchanged := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, r.Revision, unchanged.Revision)
	require.Equal(t, "running", unchanged.Status)
	cw.clock.Advance(15 * time.Second)
	change := workflowCommand(r, "change", "missing-change")
	change.SkillID, change.Version = "missing", 1
	out, err := cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, change)
	require.ErrorContains(t, err, "workflow_published_version_required")
	require.Equal(t, "paused", out.Run.Status)
	require.Equal(t, r.ID, out.Run.ID)
	require.EqualValues(t, 15, out.Run.ElapsedSeconds)
	require.Greater(t, out.Run.Revision, r.Revision)
	require.Greater(t, out.Run.ControlEpoch, r.ControlEpoch)
	require.Equal(t, "human", out.Run.Hold)
	cw.clock.Advance(30 * time.Second)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	recovered, err := replica.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Equal(t, "paused", recovered.Run.Status)
	require.EqualValues(t, 15, recovered.Run.ElapsedSeconds)
	_, err = replica.Workflow(ctx, cw.ref, key.result.Session.Token, change)
	require.ErrorContains(t, err, "workflow_revision_conflict", "exact stale retry cannot install a replacement")
	invalid := workflowCommand(recovered.Run, "change", "binding-change")
	invalid.SkillID, invalid.Version = "working-session", 1
	invalid.Bindings = map[string]string{"facilitator": "missing-seat"}
	out, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, invalid)
	require.ErrorContains(t, err, "workflow_invalid_binding")
	require.Equal(t, "paused", out.Run.Status)
	replace := workflowCommand(out.Run, "change", "reviewed-change")
	replace.SkillID, replace.Version = "working-session", 1
	changed := workflowDo(t, cw, key, replace).Run
	require.NotEqual(t, r.ID, changed.ID)
	require.Equal(t, "running", changed.Status)
	replay := workflowDo(t, cw, key, replace).Run
	require.Equal(t, changed.ID, replay.ID)
	require.Equal(t, changed.Revision, replay.Revision)
	require.Empty(t, cw.media.mutes())
}

func TestWorkflowPlayerDatabaseExtensionRespectsCallDeadline(t *testing.T) {
	cw, key, _, start := workflowFixture(t, "extend-deadline", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room, err := cw.svc.store.roomByID(memql.ContextWithFreshRead(ctx), cw.ref.OwnerUserID, start.Call.ID)
	require.NoError(t, err)
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "extend-start"}).Run
	cw.clock.Advance(5 * time.Second)
	r = workflowDo(t, cw, key, workflowCommand(r, "pause", "extend-pause")).Run
	remaining := int64(room.FundingDeadline.Sub(cw.clock.Now()) / time.Second)
	limit := remaining - r.DurationSeconds + r.ElapsedSeconds
	require.Greater(t, limit, int64(0))
	require.Less(t, limit, int64(3600))
	rejected := workflowCommand(r, "extend", "beyond-deadline")
	rejected.Seconds = limit + 1
	_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, rejected)
	require.ErrorContains(t, err, "workflow_extension_exceeds_call_deadline")
	accepted := workflowCommand(r, "extend", "at-deadline")
	accepted.Seconds = limit
	r = workflowDo(t, cw, key, accepted).Run
	require.Equal(t, remaining, r.DurationSeconds-r.ElapsedSeconds)
	replay := workflowDo(t, cw, key, accepted).Run
	require.Equal(t, r.DurationSeconds, replay.DurationSeconds)
	require.Equal(t, r.Revision, replay.Revision)
	// The same deadline rule applies to the isolated branch clock.
	d := workflowParallelDefinition()
	d.Roles = nil
	d.Nodes[3].RoleID = ""
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "extend-parallel-publish"})
	change := workflowCommand(r, "change", "extend-parallel-start")
	change.SkillID, change.Version = d.ID, 1
	r = workflowDo(t, cw, key, change).Run
	b := r.Parallel.Branches[0]
	remaining = int64(room.FundingDeadline.Sub(cw.clock.Now()) / time.Second)
	limit = remaining - b.DurationSeconds + b.ElapsedSeconds
	rejected = parallelCommand(r, "first", "extend", "branch-beyond-deadline")
	rejected.Seconds = limit + 1
	_, err = cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, rejected)
	require.ErrorContains(t, err, "workflow_extension_exceeds_call_deadline")
	accepted = parallelCommand(r, "first", "extend", "branch-at-deadline")
	accepted.Seconds = limit
	r = workflowDo(t, cw, key, accepted).Run
	require.Equal(t, remaining, r.Parallel.Branches[0].DurationSeconds-r.Parallel.Branches[0].ElapsedSeconds)
	require.EqualValues(t, 40, r.Parallel.Branches[1].DurationSeconds, "sibling clock is independent")
}
