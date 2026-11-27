package calls

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowResourceFixture(t *testing.T, name string) (*callWorld, accountTestKey, callrules.StartResult, *resourceProviderFake) {
	t.Helper()
	cw, k, _ := commerceFixture(t, name)
	if cw == nil {
		return nil, k, callrules.StartResult{}, nil
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			for _, concept := range []string{workflowConcept, resourceConcept} {
				_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
			}
		}
	})
	commerceVerified(t, cw, k)
	q := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 30, Insights: true, Resources: true})
	start, err := cw.svc.Start(context.Background(), *k.result.Session, callrules.StartBody{Name: "Owner", Confirm: confirmed, QuoteID: q.Quote.QuoteID, IdempotencyKey: "workflow-resource-room"})
	require.NoError(t, err)
	f := &resourceProviderFake{values: map[string]*ResourceIngress{}}
	installResourceFake(cw.svc, f)
	return cw, k, start, f
}
func workflowResourceDefinition() callrules.WorkflowDefinition {
	return callrules.WorkflowDefinition{ID: "bound-demo", Title: "Bound demonstration", RequiresInsights: true, EntryNodeID: "demo", Resources: []callrules.WorkflowResourceSlot{{ID: "display", Label: "Reviewed demonstration", Kind: "synthetic", Source: "screen_share"}}, Nodes: []callrules.WorkflowNode{{ID: "demo", Type: "instruction", Label: "Use the reviewed output", Visibility: "all", DurationSeconds: 60, Requirements: callrules.WorkflowRequirements{ResourceIDs: []string{"display"}}, Readiness: &callrules.WorkflowReadinessSpec{Help: "The owner starts the approved source separately.", TimeoutSeconds: 30}}, {ID: "done", Type: "finish", Label: "Done", Visibility: "all"}}, Edges: []callrules.WorkflowEdge{{ID: "done", Source: "demo", Target: "done"}}}
}
func workflowResourcePublish(t *testing.T, cw *callWorld, k accountTestKey, room string, source callrules.ResourceInfo, nonce string) *callrules.ResourcePublication {
	t.Helper()
	out, err := cw.svc.Resource(context.Background(), cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: room, ResourceID: source.ID, ExpectedRevision: source.Revision, RequestID: nonce})
	require.NoError(t, err)
	require.NotNil(t, out.Publication)
	return out.Publication
}
func workflowResourceRefsFor(source callrules.ResourceInfo) map[string]callrules.WorkflowResourceRef {
	return map[string]callrules.WorkflowResourceRef{"display": {ResourceID: source.ID, Revision: source.Revision}}
}

func TestWorkflowResourcesDatabaseExactLeaseSourceAndExplicitRenewal(t *testing.T) {
	cw, k, start, provider := workflowResourceFixture(t, "workflow-source-exact")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "source-reviewed-registration")
	d := workflowResourceDefinition()
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-resource-skill"})
	req := callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-resource-skill"}
	_, err := cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "workflow_resource_binding_required", callrules.ReasonOf(err))
	req.ResourceBindings = workflowResourceRefsFor(source)
	r := workflowDo(t, cw, k, req).Run
	require.Equal(t, "blocked", r.Status)
	require.Contains(t, r.Blockers, "bound_resource_lease_unavailable")
	require.Zero(t, provider.created, "a Skill cannot start an owner publisher")
	pub := workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-first-publication")
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity, MediaTrack{SID: "unrelated-screen", Source: "screen_share"}), person(pub.Identity, MediaTrack{SID: "wrong-source", Source: "camera"}))
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Empty(t, r.ResourceBindings["display"].LeaseID, "observation must not acquire a new lease")
	_, err = cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, workflowCommand(r, "resume", "wrong-source-resume"))
	require.Equal(t, "workflow_requirements_unavailable", callrules.ReasonOf(err))
	cw.media.setPresent(start.Call.ID, person(pub.Identity, MediaTrack{SID: "reviewed-screen", Source: "screen_share"}))
	r = workflowDo(t, cw, k, workflowCommand(r, "resume", "reviewed-source-resume")).Run
	require.Equal(t, "running", r.Status)
	require.Nil(t, r.Readiness)
	pinned := r.ResourceBindings["display"]
	require.NotEmpty(t, pinned.LeaseID)
	require.Equal(t, pub.Identity, pinned.Identity)
	backend := cw.svc.media
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) {
		return nil, errors.New("provider unavailable")
	}
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, "blocked", r.Status)
	require.Contains(t, r.Blockers, "media_readiness_unavailable")
	require.Equal(t, pinned, r.ResourceBindings["display"])
	cw.svc.media = backend
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	r = workflowDo(t, cw, k, workflowCommand(r, "resume", "provider-restored")).Run
	_, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
	require.NoError(t, err)
	replacement := workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-second-publication")
	require.NotEqual(t, pub.Identity, replacement.Identity)
	cw.media.setPresent(start.Call.ID, person(replacement.Identity, MediaTrack{SID: "new-reviewed-screen", Source: "screen_share"}))
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Contains(t, r.Blockers, "bound_resource_lease_unavailable")
	require.Equal(t, pinned, r.ResourceBindings["display"], "replacement publication cannot silently satisfy an old visit")
	require.Contains(t, r.AllowedActions, "resume", "the authoritative UI must expose the explicit lease check")
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, start.Call.ID)
	require.NoError(t, err)
	require.NoError(t, cw.svc.workflowPinResourceLeases(ctx, cw.ref, *room, r, r.NodeID))
	require.Equal(t, pinned, r.ResourceBindings["display"], "ordinary branch/step navigation preserves the shared slot")
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	installResourceFake(replica, provider)
	out, err := replica.Workflow(ctx, cw.ref, k.result.Session.Token, workflowCommand(r, "resume", "reviewed-replacement"))
	require.NoError(t, err)
	r = out.Run
	require.Equal(t, replacement.Identity, r.ResourceBindings["display"].Identity)
	require.Equal(t, "running", r.Status)
	require.Equal(t, 2, provider.created)
	cw.clock.Advance(16 * time.Minute)
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Contains(t, r.Blockers, "bound_resource_lease_unavailable", "expired lease cannot satisfy readiness even while provider inventory lingers")
	require.Empty(t, cw.media.mutes())
}

func TestWorkflowResourcesDatabaseOwnerCohostAndDelegatedScopes(t *testing.T) {
	cw, k, start, _ := workflowResourceFixture(t, "workflow-source-authority")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	source := registerTestResource(t, cw, k, "private-resource-label")
	other := registerTestResource(t, cw, k, "other-private-resource")
	d := workflowResourceDefinition()
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-source-authority"})
	pub := workflowResourcePublish(t, cw, k, start.Call.ID, source, "source-authority-publication")
	cw.media.setPresent(start.Call.ID, person(pub.Identity, MediaTrack{SID: "source", Source: "screen_share"}))
	r := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, ResourceBindings: workflowResourceRefsFor(source), IdempotencyKey: "start-source-authority"}).Run
	_, cohost, err := cw.joinBy(callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Co-host"})
	require.NoError(t, err)
	view, err := cw.svc.Workflow(ctx, cw.ref, cohost.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Len(t, view.ResourceOptions, 1)
	require.Equal(t, source.ID, view.ResourceOptions[0].ResourceID)
	require.Equal(t, "Reviewed demonstration", view.ResourceOptions[0].Label)
	req := workflowCommand(view.Run, "change", "cohost-cannot-expand")
	req.SkillID = d.ID
	req.Version = 1
	req.ResourceBindings = workflowResourceRefsFor(other)
	_, err = cw.svc.Workflow(ctx, cw.ref, cohost.Token, req)
	require.Equal(t, "workflow_resource_owner_required", callrules.ReasonOf(err))
	view, err = cw.svc.Workflow(ctx, cw.ref, cohost.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	req = workflowCommand(view.Run, "change", "cohost-reuses-approved")
	req.SkillID = d.ID
	req.Version = 1
	req.ResourceBindings = workflowResourceRefsFor(source)
	view, err = cw.svc.Workflow(ctx, cw.ref, cohost.Token, req)
	require.NoError(t, err)
	r = view.Run
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	guestView, err := cw.svc.Workflow(ctx, cw.ref, guest.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Empty(t, guestView.ResourceOptions)
	require.Nil(t, guestView.Run.ResourceBindings)
	r = workflowDo(t, cw, k, workflowCommand(r, "handoff", "owner-reviewed-handoff")).Run
	grantReq := delegationTestRequest(cw)
	grantReq.Scopes = append(grantReq.Scopes, "skills_read", "skills_control")
	grantReq.RoomIDs = []string{start.Call.ID}
	grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: d.ID, Version: 1}}
	_, missing := delegationTestGrant(t, cw, k, grantReq)
	denied := context.WithValue(ctx, agentCallContext{}, missing)
	agentView, err := cw.svc.Workflow(denied, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Empty(t, agentView.ResourceOptions)
	require.Nil(t, agentView.Run.ResourceBindings)
	require.NotContains(t, agentView.Run.AllowedActions, "next")
	// Non-host privacy strips the map before agent action projection; the stored
	// references still fence participant/branch actions for an unscoped agent.
	participantRun := workflowProjection(*r, seatRow{ID: "agent-seat", Kind: "agent"}, true, cw.clock.Now())
	participantRun.AllowedActions = []string{"ack", "end_turn"}
	participantResult := callrules.WorkflowResult{Run: &participantRun}
	cw.svc.workflowResourceProjection(denied, cw.ref, k.result.Account.KeyID, k.result.Account.KeyID, start.Call.ID, "", &participantResult, workflowResourceRefs(r.ResourceBindings))
	require.Empty(t, participantResult.Run.AllowedActions)
	_, err = cw.svc.Workflow(denied, cw.ref, k.result.Session.Token, workflowCommand(r, "next", "agent-without-resource-read"))
	require.Equal(t, "delegation_scope", callrules.ReasonOf(err))
	grantReq.Scopes = append(grantReq.Scopes, "resources_read")
	grantReq.ResourceIDs = []string{other.ID}
	grantReq.Label = "Wrong resource"
	_, wrong := delegationTestGrant(t, cw, k, grantReq)
	_, err = cw.svc.Workflow(context.WithValue(ctx, agentCallContext{}, wrong), cw.ref, k.result.Session.Token, workflowCommand(r, "next", "agent-wrong-resource"))
	require.Equal(t, "delegation_resource_scope", callrules.ReasonOf(err))
	grantReq.ResourceIDs = []string{source.ID}
	grantReq.Label = "Approved resource reader"
	_, grant := delegationTestGrant(t, cw, k, grantReq)
	agent := context.WithValue(ctx, agentCallContext{}, grant)
	agentView, err = cw.svc.Workflow(agent, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
	require.NoError(t, err)
	require.Len(t, agentView.ResourceOptions, 1)
	require.NotEmpty(t, agentView.Run.ResourceBindings)
	extend := workflowCommand(agentView.Run, "extend", "bounded-resource-reader-control")
	extend.Seconds = 30
	agentView, err = cw.svc.Workflow(agent, cw.ref, k.result.Session.Token, extend)
	require.NoError(t, err)
	require.EqualValues(t, 90, agentView.Run.DurationSeconds)
	r = agentView.Run
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: grant})
	require.NoError(t, err)
	_, err = cw.svc.Workflow(agent, cw.ref, k.result.Session.Token, workflowCommand(r, "next", "revoked-resource-reader"))
	require.Equal(t, "delegation_inactive", callrules.ReasonOf(err))
}

func TestWorkflowResourcesDatabaseScheduledRevisionRecheckAndHardGates(t *testing.T) {
	cw, k, _, provider := workflowResourceFixture(t, "workflow-source-booking")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "scheduled-reviewed-resource")
	d := workflowResourceDefinition()
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-booked-resource"})
	require.NoError(t, cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd}))
	q := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 10, Insights: true, Resources: true})
	book, err := cw.svc.Schedule(ctx, *k.result.Session, callrules.ScheduleRequest{Start: cw.clock.Now().Add(10 * time.Minute), Minutes: 10, QuoteID: q.Quote.QuoteID, IdempotencyKey: "book-source-guidance"})
	require.NoError(t, err)
	binding := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "bind_booking", BookingID: book.ID, SkillID: d.ID, Version: 1, ResourceBindings: workflowResourceRefsFor(source), IdempotencyKey: "bind-scheduled-source"}).Binding
	require.Equal(t, workflowResourceRefsFor(source), binding.ResourceBindings)
	require.Zero(t, provider.created)
	spec := source.ResourceSpec
	spec.Label = "Reviewed source changed"
	updated, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "update", ResourceID: source.ID, ExpectedRevision: source.Revision, Resource: &spec, RequestID: "revise-scheduled-source"})
	require.NoError(t, err)
	require.Equal(t, 2, updated.Resources[0].Revision)
	cw.clock.Advance(10 * time.Minute)
	joined, _, err := cw.svc.Join(ctx, cw.ref, k.result.Session, callrules.JoinBody{HostToken: book.HostToken, Name: "Scheduled owner", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	req := callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "start-stale-booked-source"}
	_, err = cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "workflow_resource_revision_changed", callrules.ReasonOf(err))
	req.ResourceBindings = workflowResourceRefsFor(updated.Resources[0])
	req.IdempotencyKey = "substitute-booked-source"
	_, err = cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "workflow_booking_resources_required", callrules.ReasonOf(err))
	snapshot := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Nil(t, snapshot.Run)
	require.Equal(t, 1, snapshot.Binding.ResourceBindings["display"].Revision)
	require.Zero(t, provider.created)
}

func TestWorkflowResourcesDatabaseTariffAndExpiredFundingRemainRequired(t *testing.T) {
	cw, k, _, start := workflowFixture(t, "workflow-source-tariff", true)
	if cw == nil {
		return
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", resourceConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "registered-no-tariff")
	d := workflowResourceDefinition()
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-no-resource-tariff"})
	_, err := cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, ResourceBindings: workflowResourceRefsFor(source), IdempotencyKey: "refuse-no-resource-tariff"})
	require.Equal(t, "resource_tariff_required", callrules.ReasonOf(err))
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, start.Call.ID)
	require.NoError(t, err)
	cfg, err := decodeUsageConfig(room.UsageConfig)
	require.NoError(t, err)
	family := *room
	cfg.ParticipationMode = familyMode
	cfg.Insights = false
	cfg.RateVersion = callrules.PreviewRateVersion
	family.UsageConfig = encodeUsageConfig(cfg)
	require.Equal(t, "workflow_family_unavailable", callrules.ReasonOf(cw.svc.workflowEntitlement(ctx, cw.ref, family)))
	room.FundingDeadline = cw.clock.Now()
	require.Equal(t, "workflow_call_deadline", callrules.ReasonOf(cw.svc.workflowEntitlement(ctx, cw.ref, *room)))
}

func TestWorkflowResourcesDatabaseMuteClaimRechecksStoppedTargetLease(t *testing.T) {
	cw, k, start, _ := workflowResourceFixture(t, "workflow-source-mute")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "reviewed-effect-source")
	pub := workflowResourcePublish(t, cw, k, start.Call.ID, source, "effect-source-publication")
	d := workflowResourceDefinition()
	d.EntryNodeID = "turn"
	d.Roles = []callrules.WorkflowRole{{ID: "facilitator", Label: "Host", Required: true}}
	d.Nodes = append([]callrules.WorkflowNode{{ID: "turn", Type: "turn", Label: "Review before muting", Visibility: "all", RoleID: "facilitator"}}, d.Nodes...)
	d.Edges = append(d.Edges, callrules.WorkflowEdge{ID: "demo", Source: "turn", Target: "demo"})
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-effect-resource"})
	r := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, ResourceBindings: workflowResourceRefsFor(source), IdempotencyKey: "start-effect-resource"}).Run
	require.Empty(t, r.ResourceBindings["display"].LeaseID)
	host := person(start.Join.Identity, MediaTrack{SID: "host-microphone", Source: "microphone"})
	host.SID = "PA-host"
	cw.media.setPresent(start.Call.ID, host, person(pub.Identity, MediaTrack{SID: "source-screen", Source: "screen_share"}))
	held, _, err := cw.svc.session(ctx, cw.ref, k.result.Session.Token, cw.clock.Now())
	require.NoError(t, err)
	seat, room, err := cw.svc.seatAndRoom(ctx, cw.ref.OwnerUserID, held)
	require.NoError(t, err)
	v, err := cw.svc.store.workflowState(ctx, cw.ref.OwnerUserID, workflowStateID(cw.ref.SiteID, "run", start.Call.ID))
	require.NoError(t, err)
	req := workflowCommand(r, "end_turn_mute", "reviewed-resource-effect")
	require.NoError(t, cw.svc.workflowPrepareMute(ctx, cw.ref, k.result.Session.Token, *held, *seat, *room, v, req))
	require.NotEmpty(t, v.Run.ResourceBindings["display"].LeaseID, "reviewed preparation pins the target's first lease")
	_, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
	require.NoError(t, err)
	reserved, err := cw.svc.workflowReserveMute(ctx, cw.ref, v)
	require.False(t, reserved)
	require.Equal(t, "workflow_requirements_unavailable", callrules.ReasonOf(err))
	require.Empty(t, cw.media.mutes())
	current := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"})
	require.Equal(t, "turn", current.Run.NodeID)
	require.Nil(t, current.Run.Effect)
}

func TestWorkflowResourcesDatabaseParallelRenewalPreservesOtherBranchState(t *testing.T) {
	cw, k, start, _ := workflowResourceFixture(t, "workflow-source-parallel")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "parallel-reviewed-source")
	pub := workflowResourcePublish(t, cw, k, start.Call.ID, source, "parallel-resource-first")
	cw.media.setPresent(start.Call.ID, person(pub.Identity, MediaTrack{SID: "shared-output", Source: "screen_share"}))
	d := workflowParallelDefinition()
	d.Resources = workflowResourceDefinition().Resources
	d.Roles = nil
	for i := range d.Nodes {
		if d.Nodes[i].ID == "a" || d.Nodes[i].ID == "a2" || d.Nodes[i].ID == "b" {
			d.Nodes[i].Requirements.ResourceIDs = []string{"display"}
			d.Nodes[i].RoleID = ""
		}
		if d.Nodes[i].ID == "b" {
			d.Nodes[i].Type = "instruction"
		}
	}
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "publish-parallel-resource"})
	r := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, ResourceBindings: workflowResourceRefsFor(source), IdempotencyKey: "start-parallel-resource"}).Run
	r = workflowDo(t, cw, k, parallelCommand(r, "first", "next", "first-resource-advance")).Run
	ack := parallelCommand(r, "first", "ack", "first-resource-ack")
	ack.Acknowledged = true
	r = workflowDo(t, cw, k, ack).Run
	cw.clock.Advance(3 * time.Second)
	_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
	require.NoError(t, err)
	replacement := workflowResourcePublish(t, cw, k, start.Call.ID, source, "parallel-resource-second")
	cw.media.setPresent(start.Call.ID, person(replacement.Identity, MediaTrack{SID: "new-shared-output", Source: "screen_share"}))
	r = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, "running", r.Status)
	require.Equal(t, "blocked", r.Parallel.Branches[0].Status)
	require.Equal(t, "blocked", r.Parallel.Branches[1].Status)
	require.Contains(t, r.AllowedActions, "resume")
	pinned := r.ResourceBindings["display"]
	before := r.Parallel.Branches
	_, err = cw.svc.Workflow(ctx, cw.ref, k.result.Session.Token, parallelCommand(r, "first", "next", "no-branch-repin"))
	require.Equal(t, "workflow_requirements_unavailable", callrules.ReasonOf(err))
	unchanged := workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "snapshot"}).Run
	require.Equal(t, pinned, unchanged.ResourceBindings["display"])
	r = workflowDo(t, cw, k, workflowCommand(unchanged, "resume", "explicit-global-resource-renewal")).Run
	require.Equal(t, replacement.Identity, r.ResourceBindings["display"].Identity)
	for i, b := range r.Parallel.Branches {
		require.Equal(t, "running", b.Status)
		require.Equal(t, before[i].VisitID, b.VisitID)
		require.Equal(t, before[i].ElapsedSeconds, b.ElapsedSeconds)
		require.Equal(t, before[i].AcknowledgedSeatIDs, b.AcknowledgedSeatIDs)
	}
	require.NotEmpty(t, r.Parallel.Branches[0].AcknowledgedSeatIDs)
	require.Empty(t, cw.media.mutes())
}
