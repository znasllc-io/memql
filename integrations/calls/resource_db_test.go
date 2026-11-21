package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type resourceTestBackend struct {
	MediaBackend
	fake *resourceProviderFake
}
type resourceProviderFake struct {
	values           map[string]*ResourceIngress
	created, deleted int
	failDelete       bool
}

func (b *resourceTestBackend) FindResourceIngress(_ context.Context, room, lease string) (*ResourceIngress, error) {
	return b.fake.values[room+":"+lease], nil
}
func (b *resourceTestBackend) CreateResourceIngress(_ context.Context, room, lease, identity string, _ callrules.ResourceSpec) (*ResourceIngress, error) {
	v := &ResourceIngress{ID: lease, Identity: identity, URL: "http://127.0.0.1:8080/whip", StreamKey: "private-" + lease}
	b.fake.values[room+":"+lease] = v
	b.fake.created++
	return v, nil
}
func (b *resourceTestBackend) DeleteResourceIngress(_ context.Context, id string) error {
	if b.fake.failDelete {
		return errors.New("gateway temporarily unavailable")
	}
	for k, v := range b.fake.values {
		if v.ID == id {
			delete(b.fake.values, k)
		}
	}
	b.fake.deleted++
	return nil
}
func installResourceFake(s *Service, f *resourceProviderFake) {
	before := s.media
	s.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		b, e := before(server, credential)
		if e != nil {
			return nil, e
		}
		return &resourceTestBackend{MediaBackend: b, fake: f}, nil
	}
}
func resourceFixture(t *testing.T, name string) (*callWorld, accountTestKey, callrules.StartResult, *resourceProviderFake) {
	t.Helper()
	cw, k, _ := commerceFixture(t, name)
	if cw == nil {
		return nil, k, callrules.StartResult{}, nil
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", resourceConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
	commerceVerified(t, cw, k)
	q := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 30, Resources: true})
	start, err := cw.svc.Start(context.Background(), *k.result.Session, callrules.StartBody{Name: "Source owner", Confirm: confirmed, QuoteID: q.Quote.QuoteID, IdempotencyKey: "resource-room-start"})
	require.NoError(t, err)
	f := &resourceProviderFake{values: map[string]*ResourceIngress{}}
	installResourceFake(cw.svc, f)
	return cw, k, start, f
}
func registerTestResource(t *testing.T, cw *callWorld, k accountTestKey, request string) callrules.ResourceInfo {
	t.Helper()
	spec := resourceTestSpec()
	spec.Label = request
	out, err := cw.svc.Resource(context.Background(), cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "register", RequestID: request, Resource: &spec})
	require.NoError(t, err)
	return out.Resources[len(out.Resources)-1]
}
func TestResourceDurableLeaseSwitchStopAndRecovery(t *testing.T) {
	cw, k, room, provider := resourceFixture(t, "resource-durable")
	if cw == nil {
		return
	}
	ctx := context.Background()
	first := registerTestResource(t, cw, k, "resource-register-0001")
	second := registerTestResource(t, cw, k, "resource-register-0002")
	req := callrules.ResourceRequest{Action: "start", ResourceID: first.ID, RoomID: room.Call.ID, ExpectedRevision: first.Revision, RequestID: "resource-publish-0001"}
	started, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	require.NotNil(t, started.Publication)
	require.Equal(t, "whip", started.Publication.Protocol)
	require.Equal(t, 1, provider.created)
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	installResourceFake(replica, provider)
	replay, err := replica.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	require.Equal(t, started.Publication, replay.Publication)
	require.Equal(t, 1, provider.created)
	req.ResourceID, req.RequestID = second.ID, "resource-publish-0002"
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "presenter_in_use", callrules.ReasonOf(err))
	req.Action = "switch"
	switched, err := replica.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	require.NotEqual(t, started.Publication.StreamKey, switched.Publication.StreamKey)
	require.Equal(t, 1, provider.deleted)
	require.Equal(t, 2, provider.created)
	provider.failDelete = true
	stop := callrules.ResourceRequest{Action: "stop", RoomID: room.Call.ID, ResourceID: second.ID}
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, stop)
	require.Error(t, err)
	status, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "status"})
	require.NoError(t, err)
	require.Equal(t, "revoking", status.Resources[1].Status)
	require.Nil(t, status.Publication)
	provider.failDelete = false
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, stop)
	require.NoError(t, err)
	require.Empty(t, provider.values)
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "resource_lease_ended", callrules.ReasonOf(err))
}
func TestResourceRequiresOwnerBudgetAndExpiresDurably(t *testing.T) {
	cw, k, room, provider := resourceFixture(t, "resource-boundaries")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-0001")
	req := callrules.ResourceRequest{Action: "start", RoomID: room.Call.ID, ResourceID: source.ID, ExpectedRevision: source.Revision, RequestID: "resource-publish-0001"}
	other := accountRegister(t, cw, newSoftwareAuthenticator(t))
	_, err := cw.svc.Resource(ctx, cw.ref, other.result.Session.Token, req)
	require.Error(t, err)
	require.Zero(t, provider.created)
	req.ExpectedRevision = 2
	_, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.Equal(t, "resource_revision_conflict", callrules.ReasonOf(err))
	req.ExpectedRevision = 1
	out, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	require.Equal(t, cw.clock.Now().Add(15*time.Minute), out.Publication.ExpiresAt)
	cw.clock.Advance(15 * time.Minute)
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	require.Empty(t, provider.values)
	state, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "list"})
	require.NoError(t, err)
	require.Equal(t, "registered", state.Resources[0].Status)
}

func TestResourceActualTracksAndHumanPresenterHold(t *testing.T) {
	cw, k, started, provider := resourceFixture(t, "resource-presenter-hold")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-0001")
	out, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: started.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-publish-0001"})
	require.NoError(t, err)
	require.Equal(t, "awaiting_source", out.Resources[0].Status)
	cw.media.mu.Lock()
	if cw.media.present == nil {
		cw.media.present = map[string][]MediaParticipant{}
	}
	cw.media.present[started.Call.ID] = []MediaParticipant{{Identity: out.Publication.Identity, Tracks: []MediaTrack{{SID: "source-screen", Source: "screen_share", Muted: true}}}}
	cw.media.mu.Unlock()
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	status, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "list"})
	require.NoError(t, err)
	require.Equal(t, "awaiting_source", status.Resources[0].Status)
	cw.media.mu.Lock()
	cw.media.present[started.Call.ID][0].Tracks[0].Muted = false
	cw.media.mu.Unlock()
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	status, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "list"})
	require.NoError(t, err)
	require.Equal(t, "publishing", status.Resources[0].Status)
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, started.Call.ID)
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.updateRoom(ctx, cw.ref.OwnerUserID, *room, map[string]any{"shareSeatId": room.HostSeatID}, room.Version.Add(time.Microsecond)))
	cw.media.mu.Lock()
	cw.media.present[started.Call.ID] = append(cw.media.present[started.Call.ID], MediaParticipant{Identity: started.Join.Identity, Tracks: []MediaTrack{{SID: "human-screen", Source: "screen_share"}}})
	cw.media.mu.Unlock()
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	require.Empty(t, provider.values)
	status, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "list"})
	require.NoError(t, err)
	require.True(t, status.Resources[0].HumanHold)
	require.Equal(t, "presenter_handoff", status.Resources[0].LastReason)
}

func TestResourceGenericHostStopShareRevokesWithoutOwnership(t *testing.T) {
	cw, k, started, provider := resourceFixture(t, "resource-cohost-stop")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-0001")
	_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: started.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-publish-0001"})
	require.NoError(t, err)
	d, err := cw.svc.store.resourceState(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, k.result.Session.KeyID)
	require.NoError(t, err)
	require.Len(t, d.State.Leases, 1)
	cohost := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, cohost)
	join, _, err := cw.svc.Join(ctx, cw.ref, cohost.result.Session, callrules.JoinBody{HostToken: started.Call.HostToken, Name: "Authorized co-host", Confirm: confirmed})
	require.NoError(t, err)
	require.True(t, join.Host)
	require.NoError(t, cw.svc.Host(ctx, *cohost.result.Session, callrules.HostActionRequest{Action: callrules.HostStopShare, Seat: d.State.Leases[0].SeatID}))
	require.Empty(t, provider.values)
	status, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "list"})
	require.NoError(t, err)
	require.True(t, status.Resources[0].HumanHold)
	require.Equal(t, "host_stopped", status.Resources[0].LastReason)
	_, err = cw.svc.Resource(ctx, cw.ref, cohost.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: started.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-cohost-restart"})
	require.ErrorIs(t, err, callrules.ErrForbidden)
}

func TestResourceReconciliationIgnoresHistoricalAccountsAndMigratesLegacyLease(t *testing.T) {
	cw, k, started, provider := resourceFixture(t, "resource-projection")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-0001")
	request := callrules.ResourceRequest{Action: "start", RoomID: started.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-projection-start"}
	_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, request)
	require.NoError(t, err)
	history, err := json.Marshal(resourceState{OwnerUserID: cw.ref.OwnerUserID, Registrations: map[string]string{}})
	require.NoError(t, err)
	// More historical accounts than the old full-table limit must not consume
	// the active page or prevent this current lease from being revoked.
	for i := 0; i < 1001; i++ {
		key := fmt.Sprintf("history-%s-%04d", cw.suffix, i)
		cw.insert(resourceConcept, resourceStateID(cw.site, key), map[string]any{"siteId": siteConcept + ":" + cw.site, "ownerUserId": cw.ref.OwnerUserID, "keyId": key, "stateData": string(history), "active": false})
	}
	active, err := cw.svc.store.resourceStatePage(ctx, "callsResourceStates")
	require.NoError(t, err)
	require.Len(t, active, 1)
	cw.clock.Advance(15 * time.Minute)
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	require.Empty(t, provider.values)
	request.RequestID = "resource-legacy-start"
	_, err = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, request)
	require.NoError(t, err)
	d, err := cw.svc.store.resourceState(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, k.result.Session.KeyID)
	require.NoError(t, err)
	for i := range d.State.Leases {
		if d.State.Leases[i].active() {
			d.State.Leases[i].ExpiresAt = cw.clock.Now().Add(time.Second)
		}
	}
	legacy, err := json.Marshal(d.State)
	require.NoError(t, err)
	// Pre-release writes omitted the projection. They must enter a migration
	// queue, not disappear from cleanup when the new active filter is deployed.
	oldPayload, oldVersion := latestPayload(t, cw.db, resourceConcept, resourceConcept+":"+d.ID)
	delete(oldPayload, "active")
	oldPayload["stateData"] = string(legacy)
	oldVersion.Payload, err = json.Marshal(oldPayload)
	require.NoError(t, err)
	// The fixture clock advanced; stamp this legacy-format version after its
	// latest native version, rather than an earlier wall-clock insert.
	oldVersion.CreatedAt = d.Version.Add(time.Microsecond)
	_, err = cw.db.NewInsert().Model(&oldVersion).Exec(ctx)
	require.NoError(t, err)
	unprojected, err := cw.svc.store.resourceStatePage(ctx, "callsResourceLegacyStates")
	require.NoError(t, err)
	require.Len(t, unprojected, 1)
	cw.clock.Advance(2 * time.Second)
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	require.Empty(t, provider.values)
	d, err = cw.svc.store.resourceState(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, k.result.Session.KeyID)
	require.NoError(t, err)
	require.True(t, d.ProjectionKnown)
	require.False(t, resourceStateActive(d))
	unprojected, err = cw.svc.store.resourceStatePage(ctx, "callsResourceLegacyStates")
	require.NoError(t, err)
	require.Empty(t, unprojected)
}
