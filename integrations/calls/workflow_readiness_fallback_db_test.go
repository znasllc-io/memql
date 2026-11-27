package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
	"time"
)

func TestWorkflowReadinessDatabaseTimeoutAckReplayAndPrivateHelp(t *testing.T) {
	cw, key, commerce, start := workflowFixture(t, "readiness-outcome", true)
	if cw == nil {
		return
	}
	d := workflowReadinessDefinition()
	d.Nodes[0].Requirements.Acknowledgment = true
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-readiness"})
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-readiness"}).Run
	require.Equal(t, "blocked", r.Status)
	require.NotNil(t, r.Readiness)
	initialWait := *r.Readiness
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	projection, err := cw.svc.Workflow(context.Background(), cw.ref, guest.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Empty(t, projection.Run.Definition.Nodes[0].Readiness.Help)
	_, err = cw.svc.Workflow(context.Background(), cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "too-early"))
	require.ErrorContains(t, err, "workflow_readiness_timeout_pending")
	cw.clock.Advance(11 * time.Second)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *commerce }
	snapshot, err := replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	r = snapshot.Run
	require.Equal(t, "check", r.NodeID)
	require.Equal(t, initialWait, *r.Readiness, "service restart and elapsed timeout do not recreate or execute a command")
	_, err = replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "missing-ack"))
	require.ErrorContains(t, err, "workflow_acknowledgment_required")
	ack := workflowCommand(r, "ack", "ack-before-fallback")
	ack.Acknowledged = true
	r = workflowDo(t, cw, key, ack).Run
	require.Contains(t, r.AllowedActions, "fallback")
	_, err = cw.svc.Workflow(context.Background(), cw.ref, guest.Token, workflowCommand(r, "fallback", "guest-cannot-fallback"))
	require.ErrorIs(t, err, callrules.ErrForbidden)
	command := workflowCommand(r, "fallback", "reviewed-fallback")
	out := workflowDo(t, cw, key, command)
	r = out.Run
	require.Equal(t, "alternative", r.NodeID)
	require.Nil(t, r.Readiness)
	require.Equal(t, "skipped", r.History[0].Outcome)
	require.Equal(t, "fallback", r.History[0].Action)
	replay, err := replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, command)
	require.NoError(t, err)
	require.Equal(t, r.History, replay.Run.History)
	require.Equal(t, r.VisitID, replay.Run.VisitID)
	stale := command
	stale.IdempotencyKey = "new-stale-fallback"
	_, err = replica.Workflow(context.Background(), cw.ref, key.result.Session.Token, stale)
	require.ErrorContains(t, err, "workflow_revision_conflict")
	require.Empty(t, cw.media.mutes())
	cancelled := workflowDo(t, cw, key, workflowCommand(r, "cancel", "cancel-alternative")).Run
	require.Equal(t, "abandoned", cancelled.History[len(cancelled.History)-1].Outcome)
}

func TestWorkflowReadinessDatabaseAgentApprovalTargetReadinessAndHardLimits(t *testing.T) {
	cw, key, _, start := workflowFixture(t, "readiness-agent", true)
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	d := workflowReadinessDefinition()
	d.Nodes[2].Requirements.ScreenShare = true
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-agent-readiness"})
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-agent-readiness"}).Run
	cw.clock.Advance(11 * time.Second)
	grantReq := delegationTestRequest(cw)
	grantReq.Scopes = append(grantReq.Scopes, "skills_read", "skills_control")
	grantReq.RoomIDs = []string{start.Call.ID}
	grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: "working-session", Version: 1}}
	_, denied := delegationTestGrant(t, cw, key, grantReq)
	_, err := cw.svc.Workflow(context.WithValue(context.Background(), agentCallContext{}, denied), cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "unapproved-alternative"))
	require.ErrorContains(t, err, "delegation_skill_version")
	grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: d.ID, Version: 1}}
	grantReq.Label = "Approved readiness agent"
	_, grant := delegationTestGrant(t, cw, key, grantReq)
	agent := context.WithValue(context.Background(), agentCallContext{}, grant)
	_, err = cw.svc.Workflow(agent, cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "target-not-ready"))
	require.ErrorContains(t, err, "workflow_requirements_unavailable")
	host := person(start.Join.Identity, MediaTrack{SID: "screen", Source: "screen_share"})
	host.SID = "PA-host"
	cw.media.setPresent(start.Call.ID, host)
	current, err := cw.svc.Workflow(agent, cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "approved-ready-alternative"))
	require.NoError(t, err)
	require.Equal(t, "alternative", current.Run.NodeID)
	require.Empty(t, cw.media.mutes())
	// Re-enter another blocked run; the call's hard deadline outranks an elapsed wait.
	change := workflowCommand(current.Run, "change", "restart-readiness")
	change.SkillID = d.ID
	change.Version = 1
	r = workflowDo(t, cw, key, change).Run
	require.NotNil(t, r.PreviousRunExit)
	require.Equal(t, "abandoned", r.PreviousRunExit.Outcome)
	cw.clock.Advance(61 * time.Minute)
	_, err = cw.svc.Workflow(context.Background(), cw.ref, key.result.Session.Token, workflowCommand(r, "fallback", "after-hard-call-limit"))
	require.Error(t, err)
	require.Empty(t, cw.media.mutes())
}

func TestWorkflowReadinessDatabaseRepeatAndParallelFallbackStayScoped(t *testing.T) {
	cw, key, _, _ := workflowFixture(t, "structured-fallback", true)
	if cw == nil {
		return
	}
	d := workflowReadinessDefinition()
	d.ID = "repeat-readiness"
	d.EntryNodeID = "rounds"
	d.Nodes = append([]callrules.WorkflowNode{{ID: "rounds", Type: "repeat", Label: "Two rounds", Visibility: "all", Repeat: &callrules.WorkflowRepeatSpec{Count: 2, UntilNodeID: "join"}}}, d.Nodes...)
	d.Nodes = append(d.Nodes, callrules.WorkflowNode{ID: "join", Type: "instruction", Label: "Round done", Visibility: "all"})
	d.Edges[2].Target = "join"
	d.Edges[3].Target = "join"
	d.Edges = append(d.Edges, callrules.WorkflowEdge{ID: "enter", Source: "rounds", Target: "check"}, callrules.WorkflowEdge{ID: "join-done", Source: "join", Target: "done"})
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-repeat-wait"})
	r := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-repeat-wait"}).Run
	r = workflowDo(t, cw, key, workflowCommand(r, "next", "enter-readiness")).Run
	require.NotNil(t, r.Readiness)
	require.Equal(t, 1, r.Repeat.Iteration)
	cw.clock.Advance(11 * time.Second)
	r = workflowDo(t, cw, key, workflowCommand(r, "fallback", "repeat-alternative")).Run
	require.Equal(t, 1, r.Repeat.Iteration)
	r = workflowDo(t, cw, key, workflowCommand(r, "next", "round-join")).Run
	r = workflowDo(t, cw, key, workflowCommand(r, "next", "second-round")).Run
	require.Equal(t, 2, r.Repeat.Iteration)
	require.NotNil(t, r.Readiness)
	require.NotContains(t, r.AllowedActions, "fallback", "each visit needs its own fresh timeout")
	p := workflowReadinessDefinition()
	p.ID = "parallel-readiness"
	p.EntryNodeID = "fork"
	p.Nodes = append([]callrules.WorkflowNode{{ID: "fork", Type: "parallel", Label: "Two groups", Visibility: "all", Parallel: &callrules.WorkflowParallelSpec{Branches: []callrules.WorkflowParallelBranchSpec{{ID: "first", EntryNodeID: "check", UntilNodeID: "join"}, {ID: "second", EntryNodeID: "other", UntilNodeID: "other"}}}}}, p.Nodes...)
	p.Nodes = append(p.Nodes, callrules.WorkflowNode{ID: "join", Type: "instruction", Label: "First joined", Visibility: "all"}, callrules.WorkflowNode{ID: "other", Type: "instruction", Label: "Other group", Visibility: "all"})
	p.Edges[2].Target = "join"
	p.Edges[3].Target = "join"
	p.Edges = append(p.Edges, callrules.WorkflowEdge{ID: "first", Source: "fork", Target: "check"}, callrules.WorkflowEdge{ID: "second", Source: "fork", Target: "other"}, callrules.WorkflowEdge{ID: "join-done", Source: "join", Target: "done"}, callrules.WorkflowEdge{ID: "other-done", Source: "other", Target: "done"})
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &p, IdempotencyKey: "publish-parallel-wait"})
	change := workflowCommand(r, "change", "parallel-readiness-start")
	change.SkillID = p.ID
	change.Version = 1
	r = workflowDo(t, cw, key, change).Run
	require.NotNil(t, r.Parallel.Branches[0].Readiness)
	otherVisit := r.Parallel.Branches[1].VisitID
	cw.clock.Advance(11 * time.Second)
	command := parallelCommand(r, "first", "fallback", "branch-alternative")
	r = workflowDo(t, cw, key, command).Run
	require.Equal(t, "alternative", r.Parallel.Branches[0].NodeID)
	require.Equal(t, otherVisit, r.Parallel.Branches[1].VisitID)
	require.Equal(t, "skipped", r.Parallel.Branches[0].History[0].Outcome)
	require.Empty(t, cw.media.mutes())
}
