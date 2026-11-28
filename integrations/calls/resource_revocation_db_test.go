package calls

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// Independent replicas share only this provider fake and the real database.
// The first DeleteIngress can be suspended without serializing later requests.
type revocationProvider struct {
	mu               sync.Mutex
	values           map[string]ResourceIngress
	created, deleted atomic.Int32
	firstDelete      func(context.Context) error
	firstRoomDelete  func(context.Context) error
	roomDeleted      atomic.Int32
}
type revocationBackend struct {
	MediaBackend
	p *revocationProvider
}

func (b *revocationBackend) FindResourceIngress(_ context.Context, room, lease string) (*ResourceIngress, error) {
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	v, ok := b.p.values[room+":"+lease]
	if !ok {
		return nil, nil
	}
	return &v, nil
}
func (b *revocationBackend) CreateResourceIngress(_ context.Context, room, lease, identity string, _ callrules.ResourceSpec) (*ResourceIngress, error) {
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	v := ResourceIngress{ID: lease, Identity: identity, URL: "http://127.0.0.1:8080/whip", StreamKey: "private-" + lease}
	b.p.values[room+":"+lease] = v
	b.p.created.Add(1)
	return &v, nil
}
func (b *revocationBackend) DeleteResourceIngress(ctx context.Context, id string) error {
	if b.p.deleted.Add(1) == 1 && b.p.firstDelete != nil {
		if err := b.p.firstDelete(ctx); err != nil {
			return err
		}
	}
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	for k, v := range b.p.values {
		if v.ID == id {
			delete(b.p.values, k)
		}
	}
	return nil
}
func (b *revocationBackend) DeleteRoom(ctx context.Context, room string) error {
	if b.p.roomDeleted.Add(1) == 1 && b.p.firstRoomDelete != nil {
		if err := b.p.firstRoomDelete(ctx); err != nil {
			return err
		}
	}
	return b.MediaBackend.DeleteRoom(ctx, room)
}
func installRevocationProvider(s *Service, p *revocationProvider) {
	prior := s.media
	s.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		b, err := prior(server, credential)
		if err != nil {
			return nil, err
		}
		return &revocationBackend{MediaBackend: b, p: p}, nil
	}
}
func revocationFixture(t *testing.T, name string) (*callWorld, accountTestKey, callrules.StartResult, *Service, *revocationProvider) {
	t.Helper()
	cw, k, start, _ := resourceFixture(t, name)
	if cw == nil {
		return nil, k, start, nil, nil
	}
	p := &revocationProvider{values: map[string]ResourceIngress{}}
	installRevocationProvider(cw.svc, p)
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	installRevocationProvider(replica, p)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", mediaSignalConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	return cw, k, start, replica, p
}
func revocationState(t *testing.T, s *Service, cw *callWorld, k accountTestKey) *resourceRow {
	t.Helper()
	d, err := s.store.resourceState(memql.ContextWithFreshRead(context.Background()), cw.owner, cw.site, k.result.Session.KeyID)
	require.NoError(t, err)
	return d
}
func revocationAsync(t *testing.T, resume func(), f func(context.Context) error) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	result, done := make(chan error, 1), make(chan struct{})
	go func() { defer close(done); result <- f(ctx) }()
	t.Cleanup(func() {
		resume()
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("revocation worker did not join")
		}
	})
	return result
}
func awaitRevocation(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteIngress was not reached")
	}
}
func revokeFixtureCredential(t *testing.T, cw *callWorld, k accountTestKey) {
	t.Helper()
	ctx := memql.ContextWithFreshRead(context.Background())
	release, err := cw.svc.gates(ctx, accountGate(k.result.Session.KeyID))
	require.NoError(t, err)
	defer release()
	c, err := cw.svc.store.credentialByID(ctx, cw.owner, k.result.Credentials[0].ID)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, *c, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
}

func TestResourceRevocationDatabaseBlockedDeletePreservesHumanForwardAndCommitment(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-human")
	if cw == nil {
		return
	}
	ctx := context.Background()
	guestKey := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, guestKey)
	guest, _, err := cw.svc.Join(ctx, cw.ref, guestKey.result.Session, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	hostSignal, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	guestSignal, err := replica.BeginMediaSignal(ctx, guest.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(guest.Identity))
	require.NoError(t, hostSignal.Confirm(ctx))
	require.NoError(t, guestSignal.Confirm(ctx))
	source := registerTestResource(t, cw, k, "resource-register-first")
	other := registerTestResource(t, cw, k, "resource-register-other")
	pub := workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			return errors.New("revocation lost its ten-second provider bound")
		}
		return pause(ctx)
	}
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
		return err
	})
	awaitRevocation(t, entered)
	// These actual guarded forwards must finish while the provider is blocked,
	// with the unchanged production frame deadline, on both independent pools.
	var forwarded atomic.Int32
	for _, signal := range []*callrules.MediaSignalAdmission{hostSignal, guestSignal} {
		frame, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = signal.Forward(frame, func(context.Context) error { forwarded.Add(1); return nil })
		cancel()
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, forwarded.Load())
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceStateActive(d))
	require.Len(t, d.State.Leases, 1)
	require.Equal(t, "revoking", d.State.Leases[0].Status)
	seat, err := replica.store.seatByID(ctx, cw.owner, d.State.Leases[0].SeatID)
	require.NoError(t, err)
	require.True(t, seat.occupying(), "unknown provider outcome retains the paid seat")
	require.Equal(t, pub.Identity, d.State.Leases[0].Identity)
	for _, req := range []callrules.ResourceRequest{
		{Action: "start", RoomID: start.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-start-during-stop"},
		{Action: "switch", RoomID: start.Call.ID, ResourceID: other.ID, ExpectedRevision: 1, RequestID: "resource-switch-during-stop"},
		{Action: "unregister", ResourceID: source.ID, ExpectedRevision: 1},
		{Action: "start", RoomID: start.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-publish-first"},
	} {
		_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, req)
		require.Error(t, err)
	}
	require.EqualValues(t, 1, p.created.Load())
	updated := other.ResourceSpec
	updated.Label = "Other source edited during revocation"
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "update", ResourceID: other.ID, ExpectedRevision: 1, Resource: &updated})
	require.NoError(t, err)
	resume()
	require.NoError(t, <-done)
	d = revocationState(t, replica, cw, k)
	require.Equal(t, "stopped", d.State.Leases[0].Status)
	require.True(t, resourceSource(d, source.ID).HumanHold)
	require.Equal(t, updated, resourceSource(d, other.ID).ResourceSpec)
	require.Equal(t, 2, resourceSource(d, other.ID).Revision)
}

func TestResourceRevocationDatabaseLateFinalizerPreservesReplacement(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-late")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	old := workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	stop := callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID}
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, stop)
		return err
	})
	awaitRevocation(t, entered)
	_, err := replica.Resource(ctx, cw.ref, k.result.Session.Token, stop)
	require.NoError(t, err)
	newer, err := replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: start.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-publish-replacement"})
	require.NoError(t, err)
	require.NotNil(t, newer.Publication)
	require.NotEqual(t, old.Identity, newer.Publication.Identity)
	before := revocationState(t, replica, cw, k)
	resume()
	require.NoError(t, <-done)
	after := revocationState(t, replica, cw, k)
	require.Equal(t, before.Version, after.Version, "late finalization must not write a stale registry")
	require.Equal(t, before.State, after.State)
	p.mu.Lock()
	require.Len(t, p.values, 1)
	for _, v := range p.values {
		require.Equal(t, newer.Publication.Identity, v.Identity)
	}
	p.mu.Unlock()
}

func TestResourceRevocationDatabaseSwitchRechecksCallerAfterDelete(t *testing.T) {
	cw, k, start, _, p := revocationFixture(t, "revoke-switch")
	if cw == nil {
		return
	}
	source := registerTestResource(t, cw, k, "resource-register-first")
	other := registerTestResource(t, cw, k, "resource-register-other")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		out, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "switch", RoomID: start.Call.ID, ResourceID: other.ID, ExpectedRevision: 1, RequestID: "resource-switch-next"})
		if out.Publication != nil {
			return errors.New("switch returned a publisher after revocation")
		}
		return err
	})
	awaitRevocation(t, entered)
	revokeFixtureCredential(t, cw, k)
	resume()
	require.Error(t, <-done)
	require.EqualValues(t, 1, p.created.Load())
	d := revocationState(t, cw.svc, cw, k)
	require.Equal(t, "stopped", d.State.Leases[0].Status)
	require.False(t, resourceStateActive(d))
}

func TestResourceRevocationDatabaseSwitchCannotUndoLaterStop(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-switch-stop")
	if cw == nil {
		return
	}
	source := registerTestResource(t, cw, k, "resource-register-first")
	other := registerTestResource(t, cw, k, "resource-register-other")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "switch", RoomID: start.Call.ID, ResourceID: other.ID, ExpectedRevision: 1, RequestID: "resource-switch-next"})
		return err
	})
	awaitRevocation(t, entered)
	_, err := replica.Resource(context.Background(), cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
	require.NoError(t, err)
	resume()
	require.ErrorContains(t, <-done, "resource_state_changed")
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceSource(d, source.ID).HumanHold)
	require.False(t, resourceStateActive(d))
	require.EqualValues(t, 1, p.created.Load())
}

func TestResourceRevocationDatabaseEndFencesConcurrentSwitchAndLateStop(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-end")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	other := registerTestResource(t, cw, k, "resource-register-other")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	// An earlier non-closing finalizer must not clear the later room fence.
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		_, err := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
		return err
	})
	awaitRevocation(t, entered)
	host, err := replica.hostOf(ctx, *k.result.Session)
	require.NoError(t, err)
	closing, err := replica.prepareResourceRoomEnd(ctx, host)
	require.NoError(t, err)
	require.NotNil(t, closing)
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "switch", RoomID: start.Call.ID, ResourceID: other.ID, ExpectedRevision: 1, RequestID: "resource-switch-after-end"})
	require.ErrorContains(t, err, "resource_room_closing")
	resume()
	require.ErrorContains(t, <-done, "resource_state_changed")
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceStateActive(d))
	require.NotNil(t, d.State.Leases[0].RoomClosing)
	r, err := replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.True(t, r.open())
	// A different replica resumes the already-authorized close, never creates
	// a replacement between stopping the old lease and deleting its room.
	require.NoError(t, replica.ReconcileResources(ctx))
	r, err = replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.False(t, r.open())
	d = revocationState(t, replica, cw, k)
	require.False(t, resourceStateActive(d))
	require.EqualValues(t, 1, p.created.Load())
}

func TestResourceRevocationDatabaseBlockedEndCanResumeAfterCallerRevocation(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-end-caller")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	other := registerTestResource(t, cw, k, "resource-register-other")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		return cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd})
	})
	awaitRevocation(t, entered)
	_, err := replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "switch", RoomID: start.Call.ID, ResourceID: other.ID, ExpectedRevision: 1, RequestID: "resource-switch-after-end"})
	require.ErrorContains(t, err, "resource_room_closing")
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceStateActive(d))
	require.Equal(t, "revoking", d.State.Leases[0].Status)
	require.Len(t, d.State.ClosingRooms, 1)
	revokeFixtureCredential(t, cw, k)
	// The second replica can complete the same durable narrowing operation
	// while the initiating provider reply is still delayed.
	require.NoError(t, replica.ReconcileResources(ctx))
	r, err := replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.False(t, r.open())
	resume()
	require.NoError(t, <-done)
	d = revocationState(t, replica, cw, k)
	require.False(t, resourceStateActive(d))
	require.Empty(t, d.State.ClosingRooms)
	require.EqualValues(t, 1, p.created.Load())
}

type resourceEndWriteFailure struct {
	Engine
	failed atomic.Bool
}

var errResourceEndWrite = errors.New("injected final native room write failure")

func (e *resourceEndWriteFailure) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "updateCallsRoom(") && strings.Contains(query, "endedAt") && e.failed.CompareAndSwap(false, true) {
		return nil, errResourceEndWrite
	}
	return e.Engine.Execute(ctx, query)
}
func TestResourceRevocationDatabaseEndRecoversAfterNativeWriteAndCallerRevocation(t *testing.T) {
	cw, k, start, replica, _ := revocationFixture(t, "revoke-recover")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	original := cw.svc.store.engine
	fault := &resourceEndWriteFailure{Engine: original}
	cw.svc.store.engine = fault
	t.Cleanup(func() { cw.svc.store.engine = original })
	err := cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd})
	require.ErrorIs(t, err, errResourceEndWrite)
	require.True(t, fault.failed.Load())
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceStateActive(d))
	require.Equal(t, "revoking", d.State.Leases[0].Status)
	require.NotNil(t, d.State.Leases[0].RoomClosing)
	r, err := replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.True(t, r.open(), "the injected failure occurs after provider deletion but before the room's final write")
	seats, err := replica.store.seatsForRoom(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	for _, seat := range seats {
		require.False(t, seat.placed(), "endRows already left seats before its final room write")
	}
	revokeFixtureCredential(t, cw, k)
	require.NoError(t, replica.ReconcileResources(ctx))
	r, err = replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.False(t, r.open())
	d = revocationState(t, replica, cw, k)
	require.False(t, resourceStateActive(d))
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: start.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-reopen-after-end"})
	require.Error(t, err)
}

func TestResourceRevocationDatabaseEndFencesFirstStartWithoutIngress(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-empty")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	entered, resume, pause := admissionPause(t)
	p.firstRoomDelete = pause
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		return cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd})
	})
	awaitRevocation(t, entered)
	d := revocationState(t, replica, cw, k)
	require.Empty(t, d.State.Leases)
	require.Len(t, d.State.ClosingRooms, 1)
	require.True(t, resourceStateActive(d), "the no-lease end must remain in the bounded recovery inventory")
	r, err := replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	intent := resourceRoomEnd{Room: *r, Close: d.State.ClosingRooms[0]}
	_, err = replica.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: start.Call.ID, ResourceID: source.ID, ExpectedRevision: 1, RequestID: "resource-first-start-during-end"})
	require.ErrorContains(t, err, "resource_room_closing")
	require.Zero(t, p.created.Load())
	resume()
	require.NoError(t, <-done)
	require.NoError(t, replica.resumeResourceRoomEnd(ctx, intent), "the exact completed no-lease end is idempotent")
	d = revocationState(t, replica, cw, k)
	require.Empty(t, d.State.ClosingRooms)
	require.False(t, resourceStateActive(d))
	require.Zero(t, p.deleted.Load(), "no resource gateway operation is necessary without an ingress")
}

func TestResourceRevocationDatabaseNoLeaseEndRecoveryNeedsNoResourceGateway(t *testing.T) {
	cw, k, start, replica, _ := revocationFixture(t, "revoke-no-gateway")
	if cw == nil {
		return
	}
	ctx := context.Background()
	// Keep ordinary media support but deliberately hide ResourceMediaBackend.
	for _, s := range []*Service{cw.svc, replica} {
		prior := s.media
		s.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
			b, err := prior(server, credential)
			return &struct{ MediaBackend }{b}, err
		}
	}
	original := cw.svc.store.engine
	fault := &resourceEndWriteFailure{Engine: original}
	cw.svc.store.engine = fault
	t.Cleanup(func() { cw.svc.store.engine = original })
	require.ErrorIs(t, cw.svc.Host(ctx, *k.result.Session, callrules.HostActionRequest{Action: callrules.HostEnd}), errResourceEndWrite)
	d := revocationState(t, replica, cw, k)
	require.Empty(t, d.State.Leases)
	require.Len(t, d.State.ClosingRooms, 1)
	require.True(t, resourceStateActive(d))
	revokeFixtureCredential(t, cw, k)
	require.NoError(t, replica.ReconcileResources(ctx))
	r, err := replica.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.False(t, r.open())
	d = revocationState(t, replica, cw, k)
	require.Empty(t, d.State.ClosingRooms)
	require.False(t, resourceStateActive(d))
}

func TestResourceRevocationDatabaseProviderIdentityMismatchRetainsCommitment(t *testing.T) {
	cw, k, start, _, p := revocationFixture(t, "revoke-identity")
	if cw == nil {
		return
	}
	source := registerTestResource(t, cw, k, "resource-register-first")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	p.mu.Lock()
	for key, v := range p.values {
		v.Identity = "unrelated-provider-identity"
		p.values[key] = v
	}
	p.mu.Unlock()
	_, err := cw.svc.Resource(context.Background(), cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
	require.ErrorContains(t, err, "resource_gateway_ambiguous")
	require.Zero(t, p.deleted.Load())
	d := revocationState(t, cw.svc, cw, k)
	require.True(t, resourceStateActive(d))
	require.Equal(t, "revoking", d.State.Leases[0].Status)
}

func TestResourceRevocationDatabaseCanceledDeleteRetainsFenceAndRecovers(t *testing.T) {
	cw, k, start, replica, p := revocationFixture(t, "revoke-cancel")
	if cw == nil {
		return
	}
	ctx := context.Background()
	signal, err := replica.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity))
	require.NoError(t, signal.Confirm(ctx))
	source := registerTestResource(t, cw, k, "resource-register-first")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	entered, resume, pause := admissionPause(t)
	p.firstDelete = pause
	cancelReady := make(chan context.CancelFunc, 1)
	done := revocationAsync(t, resume, func(ctx context.Context) error {
		caller, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelReady <- cancel
		_, err := cw.svc.Resource(caller, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: start.Call.ID, ResourceID: source.ID})
		return err
	})
	awaitRevocation(t, entered)
	(<-cancelReady)()
	require.ErrorIs(t, <-done, context.Canceled)
	d := revocationState(t, replica, cw, k)
	require.True(t, resourceStateActive(d))
	require.Equal(t, "revoking", d.State.Leases[0].Status)
	seat, err := replica.store.seatByID(ctx, cw.owner, d.State.Leases[0].SeatID)
	require.NoError(t, err)
	require.True(t, seat.occupying())
	frame, cancel := context.WithTimeout(ctx, 5*time.Second)
	var forwarded bool
	err = signal.Forward(frame, func(context.Context) error { forwarded = true; return nil })
	cancel()
	require.NoError(t, err)
	require.True(t, forwarded)
	require.NoError(t, replica.ReconcileResources(ctx))
	d = revocationState(t, replica, cw, k)
	require.Equal(t, "stopped", d.State.Leases[0].Status)
	require.False(t, resourceStateActive(d))
}

func TestResourceRevocationDatabaseRoomCloseFloorsReplicaClockOnlyOnce(t *testing.T) {
	cw, k, start, replica, _ := revocationFixture(t, "revoke-clock")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "resource-register-first")
	workflowResourcePublish(t, cw, k, start.Call.ID, source, "resource-publish-first")
	before := revocationState(t, replica, cw, k)
	cw.clock.Advance(-time.Second)
	host, err := replica.hostOf(ctx, *k.result.Session)
	require.NoError(t, err)
	intent, err := replica.prepareResourceRoomEnd(ctx, host)
	require.NoError(t, err)
	require.Equal(t, before.State.Leases[0].CreatedAt, intent.Close.RequestedAt)
	// A later retry must join the immutable original intent, not restamp it.
	cw.clock.Advance(3 * time.Second)
	retry, err := cw.svc.prepareResourceRoomEnd(ctx, host)
	require.NoError(t, err)
	require.Equal(t, intent.Close, retry.Close)
	require.NoError(t, replica.ReconcileResources(ctx))
	d := revocationState(t, replica, cw, k)
	require.Empty(t, d.State.ClosingRooms)
	require.False(t, resourceStateActive(d))
}

func TestResourceRoomCloseRejectsMalformedNarrowingAuthority(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := resourceLease{ID: "lease", ResourceID: "source", RoomID: "room", SeatID: "seat", Identity: "identity", CreatedAt: now, ExpiresAt: now.Add(time.Minute), Status: "revoking", RoomClosing: &resourceRoomClose{OwnerUserID: "owner", SiteID: "site", KeyID: "key", RoomID: "room", MediaServerID: "server", MediaRoom: "media", RequestedAt: now}}
	for _, test := range []struct {
		name   string
		change func(*resourceLease)
		valid  bool
	}{
		{"valid", func(*resourceLease) {}, true},
		{"legacy", func(l *resourceLease) { l.RoomClosing = nil }, true},
		{"completed", func(l *resourceLease) { l.Status = "stopped" }, true},
		{"publishing", func(l *resourceLease) { l.Status = "publishing" }, false},
		{"preparing", func(l *resourceLease) { l.Status = "preparing" }, false},
		{"owner", func(l *resourceLease) { l.RoomClosing.OwnerUserID = "other" }, false},
		{"site", func(l *resourceLease) { l.RoomClosing.SiteID = "other" }, false},
		{"key", func(l *resourceLease) { l.RoomClosing.KeyID = "other" }, false},
		{"room", func(l *resourceLease) { l.RoomClosing.RoomID = "other" }, false},
		{"server", func(l *resourceLease) { l.RoomClosing.MediaServerID = "" }, false},
		{"media", func(l *resourceLease) { l.RoomClosing.MediaRoom = "" }, false},
		{"time", func(l *resourceLease) { l.RoomClosing.RequestedAt = now.Add(-time.Second) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := base
			c := *base.RoomClosing
			l.RoomClosing = &c
			test.change(&l)
			state := resourceState{OwnerUserID: "owner", Leases: []resourceLease{l}}
			if l.RoomClosing != nil && l.active() {
				state.ClosingRooms = []resourceRoomClose{*l.RoomClosing}
			}
			data, err := json.Marshal(state)
			require.NoError(t, err)
			_, err = resourceFromRow(map[string]any{"id": resourceStateID("site", "key"), "siteId": "site", "keyId": "key", "stateData": string(data)})
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "resource_state_unavailable")
			}
		})
	}
}

func TestResourceRoomCloseRegistryBoundsAndExactLeaseJoin(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	close := resourceRoomClose{OwnerUserID: "owner", SiteID: "site", KeyID: "key", RoomID: "room", MediaServerID: "server", MediaRoom: "media", RequestedAt: now}
	lease := resourceLease{ID: "lease", ResourceID: "source", RoomID: "room", SeatID: "seat", Identity: "identity", CreatedAt: now, ExpiresAt: now.Add(time.Minute), Status: "revoking", RoomClosing: &close}
	for _, tc := range []struct {
		name  string
		state resourceState
		valid bool
	}{
		{"no_lease", resourceState{ClosingRooms: []resourceRoomClose{close}}, true},
		{"duplicate", resourceState{ClosingRooms: []resourceRoomClose{close, close}}, false},
		{"missing_intent", resourceState{Leases: []resourceLease{lease}}, false},
		{"unmarked_lease", resourceState{ClosingRooms: []resourceRoomClose{close}, Leases: []resourceLease{func() resourceLease { l := lease; l.RoomClosing = nil; return l }()}}, false},
		{"different_intent", resourceState{ClosingRooms: []resourceRoomClose{func() resourceRoomClose { c := close; c.RequestedAt = now.Add(time.Second); return c }()}, Leases: []resourceLease{lease}}, false},
		{"overflow", resourceState{ClosingRooms: make([]resourceRoomClose, callrules.MaxResourceLeases+1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.OwnerUserID = "owner"
			data, err := json.Marshal(tc.state)
			require.NoError(t, err)
			d, err := resourceFromRow(map[string]any{"id": resourceStateID("site", "key"), "siteId": "site", "keyId": "key", "stateData": string(data)})
			if tc.valid {
				require.NoError(t, err)
				require.True(t, resourceStateActive(d))
			} else {
				require.ErrorContains(t, err, "resource_state_unavailable")
			}
		})
	}
}
