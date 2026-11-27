package calls

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

type presentationTestBackend struct {
	MediaBackend
	mu          sync.Mutex
	permissions map[string]callrules.GrantSpec
	fail        bool
}

func (b *presentationTestBackend) UpdatePermissions(_ context.Context, _ string, identity string, spec callrules.GrantSpec) (callrules.GrantSpec, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return callrules.GrantSpec{}, errors.New("permission provider unavailable")
	}
	b.permissions[identity] = spec
	return spec, nil
}
func (b *presentationTestBackend) grant(identity string) callrules.GrantSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.permissions[identity]
}
func (b *presentationTestBackend) setFailure(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = fail
}
func (b *presentationTestBackend) RemoveParticipant(ctx context.Context, room, identity string) error {
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	if fail {
		return errors.New("removal provider unavailable")
	}
	return b.MediaBackend.RemoveParticipant(ctx, room, identity)
}

type presentationFixture struct {
	cw                    *callWorld
	key                   accountTestKey
	commerce              *CommerceConfig
	start                 callrules.StartResult
	guest                 callrules.Session
	guestJoin             callrules.JoinResult
	hostLease, guestLease *callrules.MediaSignalAdmission
	backend               *presentationTestBackend
	run                   *callrules.WorkflowRun
}

func newPresentationFixture(t *testing.T, name string, resources ...bool) presentationFixture {
	t.Helper()
	cw, k, commerce, start := workflowFixture(t, name, true)
	if len(resources) > 0 && resources[0] && cw != nil {
		// A new immutable quote selects both capabilities; do not edit an
		// existing room's billing/configuration row to manufacture entitlement.
		require.NoError(t, cw.svc.Leave(context.Background(), *k.result.Session))
		q := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 30, Insights: true, Resources: true})
		var err error
		start, err = cw.svc.Start(context.Background(), *k.result.Session, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.Quote.QuoteID, IdempotencyKey: "presentation-resource-start"})
		require.NoError(t, err)
	}
	f := presentationFixture{cw: cw, key: k, commerce: commerce, start: start}
	if cw == nil {
		return f
	}
	ctx := context.Background()
	base, e := cw.svc.backendFor(ctx, *mustProcessingRoom(t, cw, start.Call.ID))
	require.NoError(t, e)
	f.backend = &presentationTestBackend{MediaBackend: base, permissions: map[string]callrules.GrantSpec{}}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return f.backend, nil }
	f.guestJoin, f.guest, e = cw.join(start.Call.GuestToken, "Audience")
	require.NoError(t, e)
	f.hostLease, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NoError(t, f.hostLease.Confirm(ctx))
	f.guestLease, e = cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, e)
	require.NoError(t, f.guestLease.Confirm(ctx))
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: start.Join.Identity, SID: "PA_host"}, MediaParticipant{Identity: f.guestJoin.Identity, SID: "PA_guest"})
	d := callrules.WorkflowDefinition{ID: "presentation-test", Title: "Presentation", RequiresInsights: true, EntryNodeID: "present", Nodes: []callrules.WorkflowNode{{ID: "present", Type: "presentation", Label: "Present", Visibility: "all", DurationSeconds: 60}, {ID: "finish", Type: "finish", Label: "Done", Visibility: "all"}}, Edges: []callrules.WorkflowEdge{{ID: "finish", Source: "present", Target: "finish"}}}
	workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "publish", Definition: &d, IdempotencyKey: "presentation-publish"})
	f.run = workflowDo(t, cw, k, callrules.WorkflowRequest{Action: "start", SkillID: d.ID, Version: 1, IdempotencyKey: "presentation-start"}).Run
	return f
}
func (f *presentationFixture) snapshot(t *testing.T) callrules.WorkflowResult {
	t.Helper()
	return workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "snapshot"})
}
func (f *presentationFixture) request(t *testing.T, action, nonce, presenter string) callrules.WorkflowRequest {
	t.Helper()
	out := f.snapshot(t)
	req := workflowCommand(out.Run, action, nonce)
	revision := int64(0)
	if out.Presentation != nil {
		revision = out.Presentation.Revision
	}
	req.Presentation = &callrules.PresentationRequest{VisitID: out.Run.VisitID, ExpectedRevision: revision}
	if action == "presentation_set" {
		req.Presentation.PresenterSeatID = presenter
		req.Presentation.Audience = &callrules.PublicationPermissions{}
	}
	return req
}
func (f *presentationFixture) hostSeat(t *testing.T) string {
	t.Helper()
	v, e := f.cw.svc.Session(context.Background(), f.cw.ref, f.key.result.Session.Token)
	require.NoError(t, e)
	return v.SeatID
}
func (f *presentationFixture) desired(t *testing.T, token string) callrules.GrantSpec {
	t.Helper()
	r, _, _, binding, _, e := f.cw.svc.authenticateMediaSignal(context.Background(), token)
	require.NoError(t, e)
	spec, e := f.cw.svc.currentMediaGrant(context.Background(), *r, binding)
	require.NoError(t, e)
	return spec
}
func (f *presentationFixture) present(t *testing.T, identity string) bool {
	t.Helper()
	people, e := f.backend.Participants(context.Background(), f.start.Call.ID)
	require.NoError(t, e)
	for _, p := range people {
		if p.Identity == identity {
			return true
		}
	}
	return false
}
func (f *presentationFixture) reconnectGuest(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	lease, e := f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, e)
	require.NoError(t, lease.Confirm(ctx))
	f.guestLease = lease
	people, e := f.backend.Participants(ctx, f.start.Call.ID)
	require.NoError(t, e)
	people = slices.DeleteFunc(people, func(p MediaParticipant) bool { return p.Identity == f.guestJoin.Identity })
	people = append(people, MediaParticipant{Identity: f.guestJoin.Identity, SID: "PA_" + lease.IntentID})
	f.cw.media.setPresent(f.start.Call.ID, people...)
	workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
}

func TestPresentationDatabaseOwnershipHandoffIdempotencyAndOldToken(t *testing.T) {
	f := newPresentationFixture(t, "presentation-handoff")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := f.request(t, "presentation_set", "present-host", f.hostSeat(t))
	out := workflowDo(t, f.cw, f.key, req)
	require.Nil(t, out.Run)
	require.True(t, out.Presentation.Active)
	require.Equal(t, "applied", out.Presentation.Enforcement)
	require.False(t, f.present(t, f.guestJoin.Identity), "narrowing fences and removes the old publisher")
	require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish)
	require.True(t, f.backend.grant(f.start.Join.Identity).CanPublish)
	guest, e := f.cw.svc.Workflow(ctx, f.cw.ref, f.guest.Token, callrules.WorkflowRequest{Action: "presentation_status"})
	require.NoError(t, e)
	require.False(t, guest.Presentation.CanControl)
	require.Empty(t, guest.Presentation.Choices)
	require.Equal(t, "Host", guest.Presentation.PresenterName)
	_, e = f.cw.svc.Workflow(ctx, f.cw.ref, f.guest.Token, req)
	require.ErrorIs(t, e, callrules.ErrForbidden)
	replay := workflowDo(t, f.cw, f.key, req)
	require.Equal(t, out.Presentation.Revision, replay.Presentation.Revision)
	// A valid old provider JWT is reminted against the current native policy.
	reentry, e := f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, e)
	r, server, credential, _, _, e := f.cw.svc.authenticateMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, e)
	_ = r
	_ = server
	claims := rawVideoGrant(t, reentry.UpstreamToken)
	require.Equal(t, false, claims["canPublish"])
	require.NotEmpty(t, credential.Secret)
	require.NoError(t, reentry.Confirm(ctx))
	people, e := f.backend.Participants(ctx, f.start.Call.ID)
	require.NoError(t, e)
	people = append(people, MediaParticipant{Identity: f.guestJoin.Identity, SID: "PA_restricted"})
	f.cw.media.setPresent(f.start.Call.ID, people...)
	req = f.request(t, "presentation_set", "present-guest", f.guest.SeatID)
	out = workflowDo(t, f.cw, f.key, req)
	require.Equal(t, f.guestJoin.Identity, out.Presentation.PresenterIdentity)
	require.True(t, f.backend.grant(f.guestJoin.Identity).CanPublish)
	require.False(t, f.present(t, f.start.Join.Identity))
	require.False(t, f.desired(t, f.start.Join.Pass).CanPublish)
	req = f.request(t, "presentation_clear", "clear-presenter", "")
	out = workflowDo(t, f.cw, f.key, req)
	require.False(t, out.Presentation.Active)
	require.Equal(t, "applied", out.Presentation.Enforcement)
	require.True(t, f.desired(t, f.start.Join.Pass).CanPublish)
	require.False(t, f.present(t, f.start.Join.Identity), "restoration never reconnects or starts a device")
	require.True(t, f.backend.grant(f.guestJoin.Identity).CanPublish)
}

func TestPresentationDatabasePendingForwardFenceAndLateConfirm(t *testing.T) {
	f := newPresentationFixture(t, "presentation-race")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	pending, e := f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, e)
	req := f.request(t, "presentation_set", "fence-pending", f.hostSeat(t))
	out := workflowDo(t, f.cw, f.key, req)
	require.Equal(t, "pending", out.Presentation.Enforcement)
	sent := false
	require.ErrorContains(t, pending.Forward(ctx, func(context.Context) error { sent = true; return nil }), "media_permissions_changed")
	require.False(t, sent)
	// A previously sent handshake can still reply late. Confirm must remove
	// the broad participant without releasing its response to the client.
	require.ErrorContains(t, pending.Confirm(ctx), "media_permissions_changed")
	require.False(t, f.present(t, f.guestJoin.Identity))
	out = workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
	require.Equal(t, "applied", out.Presentation.Enforcement)
}

func TestPresentationDatabaseNarrowingOrdersAnAlreadyForwardingSignal(t *testing.T) {
	f := newPresentationFixture(t, "presentation-write-order")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := f.request(t, "presentation_set", "narrow-after-forward", f.hostSeat(t))
	entered, release := make(chan struct{}), make(chan struct{})
	forwarded := make(chan error, 1)
	go func() {
		forwarded <- f.guestLease.Forward(ctx, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	type result struct {
		out callrules.WorkflowResult
		err error
	}
	changed := make(chan result, 1)
	go func() {
		out, err := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
		changed <- result{out, err}
	}()
	select {
	case r := <-changed:
		t.Fatalf("policy bypassed the in-flight native write: %v", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-forwarded)
	r := <-changed
	require.NoError(t, r.err)
	require.Equal(t, "applied", r.out.Presentation.Enforcement)
	require.Error(t, f.guestLease.Forward(ctx, func(context.Context) error { t.Fatal("old publisher forwarded after strict narrowing"); return nil }))
	f.reconnectGuest(t)
	require.NoError(t, f.guestLease.Forward(ctx, func(context.Context) error { require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish); return nil }))
}

func TestPresentationDatabaseReconnectCannotHideBroadParticipantDuringPolicyCommitGap(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "guarded", true: "legacy"}[legacy], func(t *testing.T) {
			f := newPresentationFixture(t, "presentation-reconnect-gap")
			if f.cw == nil {
				return
			}
			ctx := context.Background()
			r := mustProcessingRoom(t, f.cw, f.start.Call.ID)
			hostID := f.hostSeat(t)
			// Persist exactly the desired-policy half of the mutation. A second
			// replica may admit a reconnect before the provider half runs.
			e := f.cw.svc.mediaSignalGated(ctx, *r, mediaSignalBinding{}, func(ctx context.Context, room roomRow) error {
				v, e := f.cw.svc.store.workflowState(ctx, f.cw.owner, workflowStateID(room.SiteID, "run", room.ID))
				if e != nil {
					return e
				}
				host, e := f.cw.svc.store.seatByID(ctx, f.cw.owner, hostID)
				if e != nil {
					return e
				}
				bound, _, e := f.cw.svc.presentationAdmission(ctx, room, *host)
				if e != nil {
					return e
				}
				v.Presentation = &presentationIntent{RunID: v.Run.ID, VisitID: v.Run.VisitID, Revision: 1, Active: true, Pending: true, Presenter: bound}
				if legacy {
					j, e := f.cw.svc.store.mediaSignals(ctx, room)
					if e != nil {
						return e
					}
					j.remove(f.guestLease.IntentID)
					if e := f.cw.svc.store.saveMediaSignals(ctx, room, j, f.cw.clock.Now()); e != nil {
						return e
					}
				}
				return f.cw.svc.store.saveWorkflow(ctx, f.cw.owner, v, f.cw.clock.Now())
			})
			require.NoError(t, e)
			pending, e := f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
			require.NoError(t, e)
			require.Equal(t, false, rawVideoGrant(t, pending.UpstreamToken)["canPublish"])
			if !legacy {
				require.False(t, f.present(t, f.guestJoin.Identity), "Begin must remove the old broad connection before replacing its journal")
			}
			require.NoError(t, f.cw.svc.reconcilePresentation(ctx, *r))
			require.False(t, f.present(t, f.guestJoin.Identity), "a pending narrow token is not permission evidence for an existing legacy publisher")
			require.Error(t, f.guestLease.Forward(ctx, func(context.Context) error { t.Fatal("old publisher forwarded"); return nil }))
		})
	}
}

func TestPresentationDatabaseSpeakerExpiryAndFailureSurviveReplica(t *testing.T) {
	f := newPresentationFixture(t, "presentation-expiry")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := f.request(t, "presentation_set", "speaker-temporary", f.hostSeat(t))
	req.Presentation.SpeakerSeatID = f.guest.SeatID
	req.Presentation.SpeakerSeconds = 5
	out := workflowDo(t, f.cw, f.key, req)
	require.False(t, f.present(t, f.guestJoin.Identity))
	f.reconnectGuest(t)
	require.Equal(t, []string{"microphone"}, f.backend.grant(f.guestJoin.Identity).Sources)
	require.NotNil(t, out.Presentation.SpeakerExpiresAt)
	f.backend.setFailure(true)
	f.cw.clock.Advance(6 * time.Second)
	replica := allowanceReplica(t, f.cw)
	replica.commerce = func() CommerceConfig { return *f.commerce }
	replica.media = f.cw.svc.media
	require.NoError(t, replica.ReconcileWorkflows(ctx), "expiry is native lifecycle work with no client polling")
	status, e := replica.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, callrules.WorkflowRequest{Action: "presentation_status"})
	require.NoError(t, e)
	require.Equal(t, "pending", status.Presentation.Enforcement)
	require.Empty(t, status.Presentation.SpeakerSeatID)
	require.Error(t, f.guestLease.Forward(ctx, func(context.Context) error {
		t.Fatal("expired permission forwarded before failed provider readback")
		return nil
	}))
	f.backend.setFailure(false)
	require.NoError(t, replica.ReconcileWorkflows(ctx))
	require.False(t, f.present(t, f.guestJoin.Identity))
	require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish)
	status, e = replica.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, callrules.WorkflowRequest{Action: "presentation_status"})
	require.NoError(t, e)
	require.Equal(t, "applied", status.Presentation.Enforcement)
}

func TestPresentationDatabasePresenterDepartureAndCancelRestore(t *testing.T) {
	f := newPresentationFixture(t, "presentation-restore")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "guest-presenter", f.guest.SeatID))
	require.NoError(t, f.cw.svc.Leave(ctx, f.guest))
	require.NoError(t, f.cw.svc.ReconcileWorkflows(ctx))
	status := workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
	require.False(t, status.Presentation.Active)
	require.Equal(t, "presentation_presenter_unavailable", status.Presentation.Reason)
	require.True(t, f.desired(t, f.start.Join.Pass).CanPublish)
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "host-presenter-again", f.hostSeat(t)))
	snapshot := f.snapshot(t)
	workflowDo(t, f.cw, f.key, workflowCommand(snapshot.Run, "cancel", "cancel-policy-run"))
	require.NoError(t, f.cw.svc.ReconcileWorkflows(ctx))
	status = workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
	require.False(t, status.Presentation.Active)
	require.Equal(t, "applied", status.Presentation.Enforcement)
	v, e := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", f.start.Call.ID))
	require.NoError(t, e)
	require.False(t, workflowActive(v), "finished policy leaves lifecycle inventory after confirmed restoration")
}

func TestPresentationDatabaseCurrentVisitAndScopes(t *testing.T) {
	f := newPresentationFixture(t, "presentation-authority")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := f.request(t, "presentation_set", "stale-presentation", f.hostSeat(t))
	req.Presentation.VisitID = "older-visit"
	_, e := f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorContains(t, e, "workflow_revision_conflict")
	cleanupDelegationTestRows(t, f.cw)
	grantReq := delegationTestRequest(f.cw)
	grantReq.Scopes = []string{"skills_read", "skills_control"}
	grantReq.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: f.run.DefinitionID, Version: 1}}
	grantReq.RoomIDs = []string{f.start.Call.ID}
	_, grantID := delegationTestGrant(t, f.cw, f.key, grantReq)
	actor := context.WithValue(ctx, agentCallContext{}, grantID)
	req = f.request(t, "presentation_set", "agent-control-required", f.hostSeat(t))
	_, e = f.cw.svc.Workflow(actor, f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorContains(t, e, "delegation_scope")
	d, e := f.cw.svc.store.delegation(ctx, f.cw.owner, grantID)
	require.NoError(t, e)
	d.State.Request.Scopes = append(d.State.Request.Scopes, "control")
	require.NoError(t, f.cw.svc.store.saveDelegation(ctx, f.cw.owner, d, f.cw.clock.Now()))
	out, e := f.cw.svc.Workflow(actor, f.cw.ref, f.key.result.Session.Token, req)
	require.NoError(t, e)
	require.True(t, out.Presentation.Active)
	d, e = f.cw.svc.store.delegation(ctx, f.cw.owner, grantID)
	require.NoError(t, e)
	d.State.Request.Scopes = slices.DeleteFunc(d.State.Request.Scopes, func(scope string) bool { return scope == "control" })
	require.NoError(t, f.cw.svc.store.saveDelegation(ctx, f.cw.owner, d, f.cw.clock.Now()))
	_, e = f.cw.svc.Workflow(actor, f.cw.ref, f.key.result.Session.Token, req)
	require.ErrorContains(t, e, "delegation_scope", "a retry never outlives revoked control scope")
}

type presentationResourceBackend struct {
	*resourceTestBackend
	permission *presentationTestBackend
}

func (b *presentationResourceBackend) UpdatePermissions(ctx context.Context, room, identity string, spec callrules.GrantSpec) (callrules.GrantSpec, error) {
	return b.permission.UpdatePermissions(ctx, room, identity, spec)
}
func TestPresentationDatabaseResourceOwnerLeaseAndStopFirstHandoff(t *testing.T) {
	f := newPresentationFixture(t, "presentation-resource", true)
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	provider := &resourceProviderFake{values: map[string]*ResourceIngress{}}
	f.cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) {
		return &presentationResourceBackend{resourceTestBackend: &resourceTestBackend{MediaBackend: f.backend, fake: provider}, permission: f.backend}, nil
	}
	first := registerTestResource(t, f.cw, f.key, "presentation-source-first")
	second := registerTestResource(t, f.cw, f.key, "presentation-source-second")
	_, e := f.cw.svc.Resource(ctx, f.cw.ref, f.key.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: f.start.Call.ID, ResourceID: first.ID, ExpectedRevision: first.Revision, RequestID: "presentation-source-publish"})
	require.NoError(t, e)
	d, e := f.cw.svc.store.resourceState(ctx, f.cw.owner, f.cw.site, f.key.result.Session.KeyID)
	require.NoError(t, e)
	require.Len(t, d.State.Leases, 1)
	lease := d.State.Leases[0]
	f.cw.media.setPresent(f.start.Call.ID, MediaParticipant{Identity: f.start.Join.Identity, SID: "PA_host"}, MediaParticipant{Identity: f.guestJoin.Identity, SID: "PA_guest"}, MediaParticipant{Identity: lease.Identity, SID: "PA_source"})
	_, e = f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, f.request(t, "presentation_set", "human-before-stop", f.hostSeat(t)))
	require.ErrorContains(t, e, "presentation_stop_other_resources")
	out := workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "resource-presenter", lease.SeatID))
	require.Equal(t, first.ID, out.Presentation.ResourceID)
	require.Equal(t, "applied", out.Presentation.Enforcement)
	require.Equal(t, []string{"screen_share", "screen_share_audio"}, f.backend.grant(lease.Identity).Sources)
	_, e = f.cw.svc.Resource(ctx, f.cw.ref, f.key.result.Session.Token, callrules.ResourceRequest{Action: "switch", RoomID: f.start.Call.ID, ResourceID: second.ID, ExpectedRevision: second.Revision, RequestID: "presentation-competing-source"})
	require.ErrorContains(t, e, "presentation_stop_other_resources")
	provider.failDelete = true
	_, e = f.cw.svc.Resource(ctx, f.cw.ref, f.key.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: f.start.Call.ID, ResourceID: first.ID})
	require.Error(t, e)
	cleanup, e := f.cw.svc.ReconcileMediaSignals(ctx, f.start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "pending", cleanup.State)
	require.Equal(t, "resource_cleanup_pending", cleanup.Reason, "empty provider inventory does not revoke an ingress key")
	_, e = f.cw.svc.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, f.request(t, "presentation_set", "human-after-failed-delete", f.hostSeat(t)))
	require.ErrorContains(t, e, "presentation_stop_other_resources", "a departed resource seat cannot hide a revoking ingress lease")
	provider.failDelete = false
	_, e = f.cw.svc.Resource(ctx, f.cw.ref, f.key.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: f.start.Call.ID, ResourceID: first.ID})
	require.NoError(t, e)
	out = workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
	require.False(t, out.Presentation.Active)
	require.Equal(t, "presentation_presenter_unavailable", out.Presentation.Reason)
	require.Empty(t, provider.values, "ownership release cannot restart the owner's source")
}

func TestPresentationDatabaseEntitlementLossRestoresOnlyWorkflowRestriction(t *testing.T) {
	f := newPresentationFixture(t, "presentation-entitlement")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "present-with-insights", f.hostSeat(t)))
	r, e := f.cw.svc.store.roomByID(ctx, f.cw.owner, f.start.Call.ID)
	require.NoError(t, e)
	v, e := f.cw.svc.store.workflowState(ctx, f.cw.owner, workflowStateID(f.cw.site, "run", r.ID))
	require.NoError(t, e)
	copy := *r
	cfg, e := decodeUsageConfig(copy.UsageConfig)
	require.NoError(t, e)
	cfg.Insights = false
	raw, e := json.Marshal(cfg)
	require.NoError(t, e)
	copy.UsageConfig = string(raw)
	active, reason := f.cw.svc.presentationEffective(ctx, copy, v)
	require.False(t, active)
	require.Equal(t, "presentation_insights_unavailable", reason)
	mode := f.commerce.Mode
	f.commerce.Mode = providers.Disabled
	require.NoError(t, f.cw.svc.ReconcileWorkflows(ctx))
	require.True(t, f.desired(t, f.guestJoin.Pass).CanPublish)
	f.commerce.Mode = mode
	out := workflowDo(t, f.cw, f.key, callrules.WorkflowRequest{Action: "presentation_status"})
	require.False(t, out.Presentation.Active)
	require.Equal(t, "presentation_insights_unavailable", out.Presentation.Reason)
}
