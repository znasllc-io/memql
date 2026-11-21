package calls

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func workflowFixture(t *testing.T, name string, insights bool) (*callWorld, accountTestKey, *CommerceConfig, callrules.StartResult) {
	t.Helper()
	cw, k, c := commerceFixture(t, name)
	if cw == nil {
		return nil, k, c, callrules.StartResult{}
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", workflowConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
	commerceVerified(t, cw, k)
	quote, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.KeyID, ParticipantLimit: 4, DurationMinutes: 60, Insights: insights})
	require.NoError(t, err)
	require.NotNil(t, quote.Quote)
	start, err := cw.svc.Start(context.Background(), *k.result.Session, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: quote.Quote.QuoteID, IdempotencyKey: "workflow-host-start"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, start.Join.Outcome)
	return cw, k, c, start
}
func workflowDo(t *testing.T, cw *callWorld, k accountTestKey, req callrules.WorkflowRequest) callrules.WorkflowResult {
	t.Helper()
	out, err := cw.svc.Workflow(context.Background(), cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	return out
}
func workflowCommand(r *callrules.WorkflowRun, action, nonce string) callrules.WorkflowRequest {
	return callrules.WorkflowRequest{Action: action, RunID: r.ID, ExpectedRevision: r.Revision, ExpectedEpoch: r.ControlEpoch, IdempotencyKey: nonce}
}

func TestWorkflowDatabaseDurableClockHoldReplayAndConflict(t *testing.T) {
	cw, k, c, start := workflowFixture(t, "workflow-runtime", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	out := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "start-guidance"})
	require.NotNil(t, out.Run)
	require.Equal(t, "running", out.Run.Status)
	cw.clock.Advance(130 * time.Second)
	replica := allowanceReplica(t, cw)
	replica.commerce = func() CommerceConfig { return *c }
	snapshot, err := replica.Workflow(ctx, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.EqualValues(t, 130, snapshot.Run.ElapsedSeconds)
	require.EqualValues(t, 10, snapshot.Run.OvertimeSeconds)
	require.Empty(t, cw.media.mutes(), "advisory overtime must not mute")
	pause := workflowCommand(snapshot.Run, "pause", "pause-guidance")
	paused := workflowDo(t, cw, k, pause)
	require.Equal(t, "human", paused.Run.Hold)
	cw.clock.Advance(40 * time.Second)
	replay := workflowDo(t, cw, k, pause)
	require.Equal(t, paused.Run.Revision, replay.Run.Revision)
	require.EqualValues(t, 130, replay.Run.ElapsedSeconds)
	stale := workflowCommand(snapshot.Run, "next", "stale-next")
	_, err = replica.Workflow(ctx, cw.ref, k.result.Session.Token, stale)
	require.ErrorContains(t, err, "workflow_revision_conflict")
	resumed := workflowDo(t, cw, k, workflowCommand(replay.Run, "resume", "resume-guidance"))
	require.Empty(t, resumed.Run.Hold)
	cw.clock.Advance(5 * time.Second)
	again := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.EqualValues(t, 135, again.Run.ElapsedSeconds)
	// Competing replicas read one revision: exactly one may move it.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, svc := range []*Service{cw.svc, replica} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			req := workflowCommand(again.Run, "next", fmt.Sprintf("parallel-next-%d", i))
			_, e := svc.Workflow(ctx, cw.ref, k.result.Session.Token, req)
			errs <- e
		}(i, svc)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for e := range errs {
		if e == nil {
			successes++
		} else {
			require.ErrorContains(t, e, "workflow_revision_conflict")
		}
	}
	require.Equal(t, 1, successes)
	latest := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	cancelled := workflowDo(t, cw, k, workflowCommand(latest.Run, "cancel", "cancel-guidance"))
	require.Equal(t, "cancelled", cancelled.Run.Status)
	room, err := cw.svc.store.roomByID(memql.ContextWithFreshRead(ctx), cw.ref.OwnerUserID, start.Call.ID)
	require.NoError(t, err)
	require.True(t, room.open(), "cancel guidance keeps call open")
}
func TestWorkflowDatabaseInsightsAndContinuousMedia(t *testing.T) {
	cw, k, _, start := workflowFixture(t, "workflow-media", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	// Video belongs to the interviewee, not the host/facilitator.
	_, guest, err := cw.join(start.Call.GuestToken, "Interviewee")
	require.NoError(t, err)
	held, _, err := cw.svc.session(ctx, cw.ref, guest.Token, cw.clock.Now())
	require.NoError(t, err)
	started := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: "interview", Version: 1, IdempotencyKey: "video-guidance", Bindings: map[string]string{"interviewee": held.SeatID}})
	require.Equal(t, "blocked", started.Run.Status)
	require.Contains(t, started.Run.Blockers, "camera_required")
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, start.Call.ID)
	require.NoError(t, err)
	seat, err := cw.svc.store.seatByID(ctx, cw.ref.OwnerUserID, started.Run.Bindings["interviewee"])
	require.NoError(t, err)
	camera := MediaTrack{SID: "camera-track", Source: "camera"}
	hostSeat, err := cw.svc.store.seatByID(ctx, cw.ref.OwnerUserID, started.Run.Bindings["facilitator"])
	require.NoError(t, err)
	cw.media.setPresent(room.MediaRoom, person(hostSeat.Identity, camera), person(seat.Identity))
	hostVideo := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Contains(t, hostVideo.Run.Blockers, "camera_required", "the host's camera cannot satisfy interviewee readiness")
	cw.media.setPresent(room.MediaRoom, person(hostSeat.Identity), person(seat.Identity, camera))
	restored := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", restored.Run.Status)
	require.Empty(t, restored.Run.Blockers)
	resumed := workflowDo(t, cw, k, workflowCommand(restored.Run, "resume", "video-resume"))
	require.Equal(t, "running", resumed.Run.Status)
	cw.clock.Advance(3 * time.Second)
	camera.Muted = true
	cw.media.setPresent(room.MediaRoom, person(seat.Identity, camera))
	require.NoError(t, cw.svc.ReconcileWorkflows(ctx))
	blocked := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "blocked", blocked.Run.Status)
	require.EqualValues(t, 3, blocked.Run.ElapsedSeconds)
	cw.clock.Advance(10 * time.Second)
	frozen := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.EqualValues(t, 3, frozen.Run.ElapsedSeconds)
	cw2, k2, _, _ := workflowFixture(t, "workflow-no-insights", false)
	if cw2 == nil {
		return
	}
	_, err = cw2.svc.Workflow(ctx, cw2.ref, k2.result.Session.Token, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "no-insights"})
	require.ErrorContains(t, err, "workflow_insights_required")
}
func workflowActiveRowCount(t *testing.T, cw *callWorld) int {
	t.Helper()
	call, err := langparser.RenderCall("callsWorkflowStates", nil)
	rows, err := cw.svc.store.readAsCluster(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(context.Background())), "active workflow test projection", call, err)
	require.NoError(t, err)
	count := 0
	for _, row := range rows {
		if memql.BareShortId(rowString(row, "siteId")) == cw.ref.SiteID {
			require.Equal(t, "run", rowString(row, "kind"))
			require.True(t, rowBool(row, "active"))
			count++
		}
	}
	return count
}

func TestWorkflowDatabaseImmutablePublishAndAcknowledgment(t *testing.T) {
	cw, k, _, _ := workflowFixture(t, "workflow-publish", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	d := callrules.WorkflowDefinition{ID: "my-meeting", Title: "Private meeting", RequiresInsights: true, Roles: []callrules.WorkflowRole{{ID: "facilitator", Label: "Host", Required: true}}, EntryNodeID: "consent", Nodes: []callrules.WorkflowNode{{ID: "consent", Type: "acknowledgment", Label: "Consent", Visibility: "all", RoleID: "facilitator"}, {ID: "finish", Type: "finish", Label: "Done", Visibility: "all"}}, Edges: []callrules.WorkflowEdge{{ID: "next", Source: "consent", Target: "finish"}}}
	pub := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-first"})
	require.NotNil(t, pub.Definition)
	require.Equal(t, 1, pub.Definition.Version)
	require.Zero(t, workflowActiveRowCount(t, cw), "skill libraries must not consume reconciliation pages")
	run := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-private"})
	require.Equal(t, 1, workflowActiveRowCount(t, cw))
	_, err := cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, workflowCommand(run.Run, "next", "skip-ack"))
	require.ErrorContains(t, err, "workflow_acknowledgment_required")
	edited := *pub.Definition
	edited.Title = "Changed meeting"
	second := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &edited, ExpectedRevision: pub.Definition.Revision, IdempotencyKey: "publish-second"})
	require.Equal(t, 2, second.Definition.Version)
	snapshot := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "Private meeting", snapshot.Run.Definition.Title)
	require.Equal(t, 1, snapshot.Run.DefinitionVersion)
	ack := workflowCommand(snapshot.Run, "ack", "own-ack")
	ack.Acknowledged = true
	acknowledged := workflowDo(t, cw, k, ack)
	complete := workflowDo(t, cw, k, workflowCommand(acknowledged.Run, "next", "finish-private"))
	require.Equal(t, "completed", complete.Run.Status)
	require.Zero(t, workflowActiveRowCount(t, cw), "completed runs must not fill reconciliation pages")
}

func TestWorkflowDatabaseGuestPrivacyAndHumanOverrideAgentHold(t *testing.T) {
	cw, k, _, start := workflowFixture(t, "workflow-authority", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	d := callrules.WorkflowDefinition{ID: "host-notes", Title: "Guided session", Instructions: "Host rubric: confidential", RequiresInsights: true, Roles: []callrules.WorkflowRole{{ID: "facilitator", Label: "Host", Required: true}}, EntryNodeID: "private", Nodes: []callrules.WorkflowNode{{ID: "private", Type: "discussion", Label: "Confidential rubric", Instructions: "Private facilitator notes", Visibility: "host", RoleID: "facilitator", DurationSeconds: 60}, {ID: "finish", Type: "finish", Label: "Done", Visibility: "all"}}, Edges: []callrules.WorkflowEdge{{ID: "next", Source: "private", Target: "finish"}}}
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-host-notes"})
	active := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-host-notes"})
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	guestView, err := cw.svc.Workflow(ctx, cw.ref, guest.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.NotNil(t, guestView.Run)
	require.Empty(t, guestView.Run.Definition.Instructions)
	require.Empty(t, guestView.Run.Definition.Nodes[0].Instructions)
	require.Equal(t, "Private step", guestView.Run.Definition.Nodes[0].Label)
	require.Empty(t, guestView.Run.Events)
	_, err = cw.svc.Workflow(ctx, cw.ref, guest.Token, workflowCommand(active.Run, "next", "guest-next"))
	require.ErrorIs(t, err, callrules.ErrForbidden)
	grantReq := delegationTestRequest(cw)
	grantReq.Scopes = append(grantReq.Scopes, "skills_read", "skills_control")
	grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: d.ID, Version: 1}}
	grantReq.RoomIDs = []string{start.Call.ID}
	_, grant := delegationTestGrant(t, cw, k, grantReq)
	agentCtx := context.WithValue(ctx, agentCallContext{}, grant)
	paused := workflowDo(t, cw, k, workflowCommand(active.Run, "pause", "human-pause"))
	_, err = cw.svc.Workflow(agentCtx, cw.ref, k.result.Session.Token, workflowCommand(paused.Run, "resume", "agent-resume"))
	require.ErrorContains(t, err, "workflow_human_control_hold")
	// Even the newest revision/epoch cannot silently take back human control.
	fresh, err := cw.svc.Workflow(agentCtx, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Empty(t, fresh.Run.AllowedActions)
	handed := workflowDo(t, cw, k, workflowCommand(paused.Run, "handoff", "human-handoff"))
	agentPaused, err := cw.svc.Workflow(agentCtx, cw.ref, k.result.Session.Token, workflowCommand(handed.Run, "pause", "agent-pause"))
	require.NoError(t, err)
	require.Empty(t, agentPaused.Run.Hold)
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: grant})
	require.NoError(t, err)
	// Simulate a request retaining its internal session after the outer dispatch.
	_, err = cw.svc.Workflow(agentCtx, cw.ref, k.result.Session.Token, workflowCommand(agentPaused.Run, "resume", "revoked-resume"))
	require.ErrorContains(t, err, "delegation_inactive")
}

func TestWorkflowDatabaseScheduledBindingPinsVersionAndCancellation(t *testing.T) {
	cw, k, _, _ := workflowFixture(t, "workflow-booking", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	require.NoError(t, cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd}))
	quote, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.KeyID, ParticipantLimit: 4, DurationMinutes: 10, Insights: true})
	require.NoError(t, err)
	book, err := cw.svc.Schedule(ctx, *k.result.Session, callrules.ScheduleRequest{Start: cw.clock.Now().Add(10 * time.Minute), Minutes: 10, QuoteID: quote.Quote.QuoteID, IdempotencyKey: "workflow-book-guidance"})
	require.NoError(t, err)
	binding := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "bind_booking", BookingID: book.ID, SkillID: "working-session", Version: 1, IdempotencyKey: "bind-guidance"})
	require.NotNil(t, binding.Binding)
	require.EqualValues(t, 1, binding.Binding.Revision)
	snapshot := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot_binding", BookingID: book.ID})
	require.Equal(t, binding.Binding, snapshot.Binding)
	cw.clock.Advance(10 * time.Minute)
	joined, _, err := cw.svc.Join(ctx, cw.ref, k.result.Session, callrules.JoinBody{HostToken: book.HostToken, Name: "Scheduled host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	live := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Nil(t, live.Run, "binding never silently launches an agent or workflow")
	require.NotNil(t, live.Binding)
	_, err = cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "start", SkillID: "one-on-one", Version: 1, IdempotencyKey: "wrong-binding"})
	require.ErrorContains(t, err, "workflow_booking_version_required")
	started := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: live.Binding.SkillID, Version: live.Binding.Version, IdempotencyKey: "start-bound-guidance"})
	require.Equal(t, "working-session", started.Run.DefinitionID)
	require.NoError(t, cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd}))
	q2, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.KeyID, ParticipantLimit: 2, DurationMinutes: 10, Insights: true})
	require.NoError(t, err)
	cancelled, err := cw.svc.Schedule(ctx, *k.result.Session, callrules.ScheduleRequest{Start: cw.clock.Now().Add(10 * time.Minute), Minutes: 10, QuoteID: q2.Quote.QuoteID, IdempotencyKey: "workflow-book-to-cancel"})
	require.NoError(t, err)
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "bind_booking", BookingID: cancelled.ID, SkillID: "one-on-one", Version: 1, IdempotencyKey: "bind-to-cancel"})
	require.NoError(t, cw.svc.Cancel(ctx, *k.result.Session, cancelled.ID))
	gone := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot_binding", BookingID: cancelled.ID})
	require.Nil(t, gone.Binding)
}
