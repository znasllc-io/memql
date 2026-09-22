package worker

// The failure paths (epic memql#5327, design D16).
//
// ===========================================================================
// EVERY TEST HERE FAILS AGAINST THE CODE IT REPLACES
// ===========================================================================
// The 2026-09-13 audit's coverage table is a list of untested failure paths --
// revocation while connected, two replicas holding one machine, a dead pod's
// stamp, a clock that disagrees. The happy paths and the refusal WORDING were
// well covered; what was not covered was anything that had never worked.
//
// So the assertions below are deliberately about the OLD behaviour's absence:
// a stream that stayed open through a revoke, a lastSeenAt taken from the
// machine's own clock, a hold wiped from under a successor. Each one would
// have passed before this epic only by asserting nothing.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/workertoken"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// D1 -- the registration watcher
// ---------------------------------------------------------------------------

// watcherFixture is a registry holding one machine's stream, plus a recorder
// for the reason it was ended.
type watcherFixture struct {
	registry *Registry
	worker   *Worker
	reasons  []string
}

func newWatcherFixture(t *testing.T) *watcherFixture {
	t.Helper()
	f := &watcherFixture{registry: NewRegistry(nil, time.Now)}
	f.worker = &Worker{RegistrationId: "reg-1", OwnerUserId: "user-1"}
	f.worker.SetTerminateFunc(func(reason string) { f.reasons = append(f.reasons, reason) })
	f.registry.Add(f.worker)
	return f
}

func quietWatcher(registry *Registry, selfNode string) *RegistrationWatcher {
	return NewRegistrationWatcher(registry, selfNode, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestARevokedRegistrationEndsTheStreamThisReplicaHolds(t *testing.T) {
	// THE WHOLE OF H-4. Revoking a registration excluded it from ROUTING and
	// left the stream open, so a stolen laptop kept receiving dispatches until
	// it reconnected -- which for a machine nobody is touching is never.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  "reg-1",
		RevokedAt:       time.Now(),
		ConnectedNodeId: "agent-a",
	})

	if got != DisconnectReasonRevoked {
		t.Fatalf("a revoked registration must end its stream as %q, got %q", DisconnectReasonRevoked, got)
	}
	if len(f.reasons) != 1 || f.reasons[0] != DisconnectReasonRevoked {
		t.Fatalf("the session must be terminated once, naming the reason: %v", f.reasons)
	}
}

func TestRevocationOutranksSupersedeAsTheReason(t *testing.T) {
	// A revoked row may ALSO name another node -- a machine that reconnected
	// elsewhere and was then revoked. Reporting that stream as merely
	// superseded would file a security decision as a routine handover.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  "reg-1",
		RevokedAt:       time.Now(),
		ConnectedNodeId: "agent-b",
	})

	if got != DisconnectReasonRevoked {
		t.Fatalf("revocation must outrank supersede, got %q", got)
	}
}

func TestAnotherReplicaTakingOverEndsThisOne(t *testing.T) {
	// THE SUPERSEDE HALF OF M-4. Two live streams for one machine on two pods
	// flapped the row, each re-stamping connectedNodeId at itself on every
	// heartbeat flush, with nothing telling either one to stop.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  "reg-1",
		ConnectedNodeId: "agent-b",
	})

	if got != DisconnectReasonSuperseded {
		t.Fatalf("a hold taken by another replica must end this one as %q, got %q", DisconnectReasonSuperseded, got)
	}
}

func TestOurOwnStampNeverEndsOurOwnStream(t *testing.T) {
	// EVERY HEARTBEAT FLUSH RE-ASSERTS connectedNodeId, so this event arrives
	// every fifteen seconds for every connected machine in the cluster. A
	// watcher that acted on it would drain every stream it holds, one beat
	// after opening it.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	if got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  "reg-1",
		ConnectedNodeId: "agent-a",
	}); got != "" {
		t.Fatalf("our own hold must not end our own stream, got %q", got)
	}
	if len(f.reasons) != 0 {
		t.Fatalf("nothing should have been terminated: %v", f.reasons)
	}
}

func TestAClearedHoldEndsNothing(t *testing.T) {
	// A BLANK connectedNodeId is what the disconnect path and the stale-hold
	// sweep both write, and our own clear is the most likely author of this
	// very event. Reading it as "somebody else holds this" would make a
	// reconnect race end the stream that had just been established.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	if got := w.Observe(context.Background(), RegistrationEvent{RegistrationId: "reg-1"}); got != "" {
		t.Fatalf("a cleared hold must end nothing, got %q", got)
	}
}

func TestAReplicaThatDoesNotKnowItsOwnIdActsOnNothing(t *testing.T) {
	// It cannot tell "somebody else holds this" from "I do". Acting would
	// drain every stream on the node on the first event that arrived.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "")

	if got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId:  "reg-1",
		ConnectedNodeId: "agent-b",
	}); got != "" {
		t.Fatalf("a replica with no id must act on nothing, got %q", got)
	}
}

func TestAnEventForAMachineThisReplicaDoesNotHoldIsSilent(t *testing.T) {
	// THE ORDINARY ANSWER. This runs on every agent replica for every
	// registration write in the cluster, and all but one of them hold no
	// stream for the machine named.
	f := newWatcherFixture(t)
	w := quietWatcher(f.registry, "agent-a")

	if got := w.Observe(context.Background(), RegistrationEvent{
		RegistrationId: "reg-somebody-else",
		RevokedAt:      time.Now(),
	}); got != "" {
		t.Fatalf("a machine we do not hold must be silent, got %q", got)
	}
	if len(f.reasons) != 0 {
		t.Fatalf("nothing should have been terminated: %v", f.reasons)
	}
}

// ---------------------------------------------------------------------------
// D3 -- the live credential re-check
// ---------------------------------------------------------------------------

func recheckSession(t *testing.T, store *fakeRegistrationStore, now time.Time) *streamSession {
	t.Helper()
	s := newHeartbeatTestSession(store, func() time.Time { return now })
	s.worker.IdentityId = "ident-1"
	return s
}

func TestARevokedCredentialEndsALiveStream(t *testing.T) {
	// THE BACKSTOP FOR WHAT THE BROADCAST CANNOT CARRY: a token revoked
	// through a path that writes no registration row. Before this the token
	// was resolved once, at stream open, and never looked at again.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{identity: &WorkerIdentity{IdentityId: "ident-1", OwnerUserId: "user-1", Active: false}}
	s := recheckSession(t, store, t0)
	defer s.cancel()

	beatAt(s, t0)

	if len(store.identityLookups) != 1 {
		t.Fatalf("the first beat of a stream must re-check, got %d lookups", len(store.identityLookups))
	}
	if s.drainReason != DisconnectReasonRevoked {
		t.Fatalf("a revoked credential must end the stream as %q, got %q", DisconnectReasonRevoked, s.drainReason)
	}
	if len(store.lastSeenFlushes) != 0 {
		t.Fatal("a revoked machine's last beat must not also refresh the row that says it is online")
	}
}

func TestAnExpiredCredentialEndsALiveStreamUnderItsOwnReason(t *testing.T) {
	// EXPIRY ANNOUNCES ITSELF WITH NO WRITE AT ALL, which is why the broadcast
	// cannot carry it and this check has to exist. The reason is its own
	// because "somebody revoked this" and "the clock ran out" lead a reader to
	// different places.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{identity: &WorkerIdentity{
		IdentityId: "ident-1", OwnerUserId: "user-1", Active: true,
		ExpiresAt: t0.Add(-time.Minute),
	}}
	s := recheckSession(t, store, t0)
	defer s.cancel()

	beatAt(s, t0)

	if s.drainReason != DisconnectReasonTokenExpired {
		t.Fatalf("an expired credential must end the stream as %q, got %q", DisconnectReasonTokenExpired, s.drainReason)
	}
}

func TestAnUnreadableCredentialLookupKeepsTheStream(t *testing.T) {
	// FAIL OPEN, deliberately, and the asymmetry with component/node's
	// open-time gate is the ruling. That one runs at stream OPEN, where a
	// wrong refusal costs one retry a second later. This runs on live
	// connections: failing closed on a database hiccup disconnects every
	// paired machine in the cluster, each of which reconnects into it.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{identityErr: errors.New("connection refused")}
	s := recheckSession(t, store, t0)
	defer s.cancel()

	beatAt(s, t0)

	if s.drainReason != "" {
		t.Fatalf("an unreadable lookup must keep the stream, got %q", s.drainReason)
	}
	if len(store.lastSeenFlushes) != 1 {
		t.Fatal("and the beat must still persist -- the machine is there and said so")
	}
}

func TestACredentialThatCannotBeFoundKeepsTheStream(t *testing.T) {
	// THE RULING WORTH ARGUING. Revocation here is a SOFT flag and nothing in
	// the tree deletes an identity row, so absence is not a decision anybody
	// made -- it is a read that did not see what is there. Reading it as a
	// revoke would disconnect every machine in the cluster the first time a
	// paginated query came back empty.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{} // identity nil, err nil
	s := recheckSession(t, store, t0)
	defer s.cancel()

	beatAt(s, t0)

	if s.drainReason != "" {
		t.Fatalf("a credential that was not found must keep the stream, got %q", s.drainReason)
	}
}

func TestTheCredentialIsRecheckedOnAnIntervalRatherThanEveryBeat(t *testing.T) {
	// It is a database read on the recv goroutine of every connected machine
	// in the cluster. The question it answers changes at human speed.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{identity: &WorkerIdentity{IdentityId: "ident-1", OwnerUserId: "user-1", Active: true}}
	s := recheckSession(t, store, t0)
	defer s.cancel()

	beatAt(s, t0)
	beatAt(s, t0.Add(HeartbeatBatchInterval))
	beatAt(s, t0.Add(2*HeartbeatBatchInterval))
	if len(store.identityLookups) != 1 {
		t.Fatalf("beats inside the interval must not re-check, got %d lookups", len(store.identityLookups))
	}

	beatAt(s, t0.Add(IdentityRecheckInterval))
	if len(store.identityLookups) != 2 {
		t.Fatalf("the first beat at or past the interval must re-check, got %d lookups", len(store.identityLookups))
	}
}

// ---------------------------------------------------------------------------
// D6 -- the server's clock
// ---------------------------------------------------------------------------

func TestLastSeenIsTheServersClockAndNotTheMachines(t *testing.T) {
	// M-3 WHOLE. This took hb.GetTs() -- the cockpit's own timestamppb.Now()
	// -- and persisted it, while IsOnline compares against the SERVER's now.
	// A machine 45s slow read offline while it was connected and beating.
	serverNow := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clientNow := serverNow.Add(-45 * time.Second)
	store := &fakeRegistrationStore{}
	s := newHeartbeatTestSession(store, func() time.Time { return serverNow })
	defer s.cancel()

	beatAtWithClientClock(s, serverNow, clientNow)

	if len(store.lastSeenFlushes) != 1 {
		t.Fatalf("expected one flush, got %d", len(store.lastSeenFlushes))
	}
	if got := store.lastSeenFlushes[0].LastSeenAt; !got.Equal(serverNow) {
		t.Fatalf("lastSeenAt must be the server's clock (%s), got %s", serverNow, got)
	}
}

func TestTheMachinesOwnStampIsKeptAsMeasuredSkew(t *testing.T) {
	// The client's timestamp is not discarded -- it is recorded as the only
	// thing it can honestly measure, so a machine whose clock is wrong is
	// diagnosable rather than merely absent from the page.
	serverNow := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	s := newHeartbeatTestSession(store, func() time.Time { return serverNow })
	defer s.cancel()

	beatAtWithClientClock(s, serverNow, serverNow.Add(-45*time.Second))

	flush := store.lastSeenFlushes[0]
	if !flush.ClockSkewSeen {
		t.Fatal("a beat that carried a timestamp must record the skew as SEEN")
	}
	if flush.ClockSkewMs != -45000 {
		t.Fatalf("a machine 45s behind must record -45000 ms, got %d", flush.ClockSkewMs)
	}
}

func TestABeatWithNoTimestampReportsNoSkewRatherThanZero(t *testing.T) {
	// ZERO IS A REAL ANSWER HERE -- the clocks agree -- so silence must not
	// be written as one. This is the distinction ClockSkewSeen exists for.
	serverNow := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	s := newHeartbeatTestSession(store, func() time.Time { return serverNow })
	defer s.cancel()

	s.handleHeartbeat(&memqlv1.Heartbeat{}, "10.0.0.1:1234")

	if store.lastSeenFlushes[0].ClockSkewSeen {
		t.Fatal("a beat with no timestamp measured nothing, and must not claim a zero skew")
	}
}

func TestAMeasuredZeroSkewIsReportedAsMeasured(t *testing.T) {
	serverNow := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	s := newHeartbeatTestSession(store, func() time.Time { return serverNow })
	defer s.cancel()

	s.handleHeartbeat(&memqlv1.Heartbeat{Ts: timestamppb.New(serverNow)}, "10.0.0.1:1234")

	flush := store.lastSeenFlushes[0]
	if !flush.ClockSkewSeen || flush.ClockSkewMs != 0 {
		t.Fatalf("clocks that agree is an ANSWER: want seen=true skew=0, got seen=%v skew=%d",
			flush.ClockSkewSeen, flush.ClockSkewMs)
	}
}

// ---------------------------------------------------------------------------
// D7 -- when a hold is stale
// ---------------------------------------------------------------------------

func TestHoldIsStale(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		what   string
		holder string
		seen   time.Time
		want   bool
	}{
		{"no stamp, nothing to clear", "", now.Add(-time.Hour), false},
		{"a fresh beat", "agent-a", now.Add(-time.Second), false},
		{
			// THE SLACK IS LOAD-BEARING. The flush is throttled, so a row can
			// legitimately sit one interval behind a perfectly healthy stream;
			// a sweep on the online window alone would race the machine's own
			// next write and blank a live hold.
			"one interval behind a healthy stream", "agent-a",
			now.Add(-(OnlineWindow + time.Second)), false,
		},
		{"past the window and its slack", "agent-a", now.Add(-(StaleHoldWindow + time.Second)), true},
		{
			// A stamp with no heartbeat at all can only have come from a
			// register that never reached its first flush: a pod that died
			// inside one interval.
			"a stamp from a machine never heard from", "agent-a", time.Time{}, true,
		},
	}
	for _, c := range cases {
		if got := HoldIsStale(c.holder, c.seen, now); got != c.want {
			t.Errorf("%s: want stale=%v, got %v", c.what, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// D9 -- reclaim revokes the credential it displaced
// ---------------------------------------------------------------------------

func TestReclaimingAMachineKeyRevokesTheCredentialItDisplaced(t *testing.T) {
	// M-4's second half. Reclaim rebinds the row's identityId to the token
	// that just connected; leaving the previous one active gives one physical
	// machine two live credentials, and the machine key is what BOTH of them
	// reclaim on -- so the old cockpit takes the row straight back and the two
	// flap, each re-pointing connectedNodeId at itself.
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{
		byUser: []RegistrationRow{{
			ID:          "reg-1",
			OwnerUserId: "user-1",
			IdentityId:  "ident-OLD",
			Labels:      map[string]string{"machineId": "mac-123"},
		}},
	}
	srv := newUpsertTestServer(store, now)

	_, err := srv.upsertRegistration(context.Background(),
		&WorkerIdentity{IdentityId: "ident-NEW", OwnerUserId: "user-1", Active: true},
		&memqlv1.Register{
			Capabilities: []string{CapabilityHeadless},
			Labels:       map[string]string{"machineId": "mac-123"},
		},
		nil, now, "10.0.0.1:1234",
	)
	if err != nil {
		t.Fatalf("reclaim must succeed: %v", err)
	}

	if len(store.revokedIdentities) != 1 || store.revokedIdentities[0] != "ident-OLD" {
		t.Fatalf("reclaim must revoke the credential it displaced, got %v", store.revokedIdentities)
	}
}

func TestAnOrdinaryRefreshRevokesNothing(t *testing.T) {
	// SCOPED TO RECLAIM. A refresh under the same identityId displaces
	// nothing, and revoking there would kill the credential of every machine
	// that merely reconnected.
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{
		existing: &RegistrationRow{ID: "reg-1", OwnerUserId: "user-1", IdentityId: "ident-1"},
	}
	srv := newUpsertTestServer(store, now)

	if _, err := srv.upsertRegistration(context.Background(), upsertTestIdentity(),
		&memqlv1.Register{Capabilities: []string{CapabilityHeadless}}, nil, now, "10.0.0.1:1234"); err != nil {
		t.Fatalf("refresh must succeed: %v", err)
	}

	if len(store.revokedIdentities) != 0 {
		t.Fatalf("an ordinary refresh must revoke nothing, got %v", store.revokedIdentities)
	}
}

func TestAFailedCredentialRevokeDoesNotFailTheHandshake(t *testing.T) {
	// BEST EFFORT, and deliberately after the refresh. The registration is the
	// record the cluster routes on; failing the handshake because a cleanup
	// write did not land would take a working machine offline over a
	// credential nobody is using.
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{
		revokeIdentityErr: errors.New("write refused"),
		byUser: []RegistrationRow{{
			ID: "reg-1", OwnerUserId: "user-1", IdentityId: "ident-OLD",
			Labels: map[string]string{"machineId": "mac-123"},
		}},
	}
	srv := newUpsertTestServer(store, now)

	row, err := srv.upsertRegistration(context.Background(),
		&WorkerIdentity{IdentityId: "ident-NEW", OwnerUserId: "user-1", Active: true},
		&memqlv1.Register{
			Capabilities: []string{CapabilityHeadless},
			Labels:       map[string]string{"machineId": "mac-123"},
		}, nil, now, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("the handshake must survive a failed credential revoke: %v", err)
	}
	if row.ID != "reg-1" {
		t.Fatalf("the reclaim itself must still have happened, got %q", row.ID)
	}
}

// ---------------------------------------------------------------------------
// D4 / D5 -- the credential's deadline, and renewing it
// ---------------------------------------------------------------------------

func TestTheCredentialsDeadlineIsMirroredOntoTheRegistration(t *testing.T) {
	// Without this the Fleet page cannot warn before a machine disconnects
	// itself, and D4 would have created a new silent failure: in ninety days
	// the machine stops connecting with nothing having led up to it.
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	expires := now.Add(90 * 24 * time.Hour)
	store := &fakeRegistrationStore{}
	srv := newUpsertTestServer(store, now)

	row, err := srv.upsertRegistration(context.Background(),
		&WorkerIdentity{IdentityId: "ident-1", OwnerUserId: "user-1", Active: true, ExpiresAt: expires},
		&memqlv1.Register{Capabilities: []string{CapabilityHeadless}}, nil, now, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !row.CredentialExpiresAt.Equal(expires) {
		t.Fatalf("the registration must carry the credential's deadline, got %s", row.CredentialExpiresAt)
	}
	if len(store.created) != 1 || !store.created[0].CredentialExpiresAt.Equal(expires) {
		t.Fatal("and it must reach the store on the create")
	}
}

func TestARotationAnswersWithAFreshTokenAndMovesTheDeadline(t *testing.T) {
	// handleRotationRequest replied with an empty RotationResponse -- an MVP
	// stub -- so a cockpit that asked for rotation got an acknowledgment and
	// no token, forever.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	expires := t0.Add(90 * 24 * time.Hour)
	store := &fakeRegistrationStore{rotatedToken: "mql_wkr_fresh", rotatedExpiry: expires}
	s := recheckSession(t, store, t0)
	defer s.cancel()
	sent := captureSends(s)

	s.handleRotationRequest(context.Background(), &memqlv1.RotationRequest{})

	if len(store.rotations) != 1 {
		t.Fatalf("the rotation must reach the store, got %d", len(store.rotations))
	}
	msg := sent.last(t)
	res := msg.GetRotationResponse()
	if res == nil || res.GetNewToken() != "mql_wkr_fresh" {
		t.Fatalf("the reply must carry the new plaintext, got %#v", res)
	}
	if got := res.GetNewTokenExpiresAt().AsTime(); !got.Equal(expires) {
		t.Fatalf("the reply must carry the new deadline, got %s", got)
	}
	if !s.credentialExpiresAt.Equal(expires) {
		t.Fatal("and the session must hold it, so the next heartbeat flush mirrors it onto the row")
	}
}

func TestAFailedRotationStillAnswers(t *testing.T) {
	// EVERY FAILURE ANSWERS. The reply carries an empty token, which is what
	// the pre-D5 stub always sent -- so a cockpit that cannot tell the two
	// apart is no worse off, and one that can keeps the credential it has.
	// Answering NOTHING would park a cockpit on a message never coming.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{rotateErr: errors.New("the credential is revoked")}
	s := recheckSession(t, store, t0)
	defer s.cancel()
	sent := captureSends(s)

	s.handleRotationRequest(context.Background(), &memqlv1.RotationRequest{})

	res := sent.last(t).GetRotationResponse()
	if res == nil {
		t.Fatal("a failed rotation must still send a RotationResponse")
	}
	if res.GetNewToken() != "" {
		t.Fatalf("a failed rotation must not hand out a token, got %q", res.GetNewToken())
	}
	if !s.credentialExpiresAt.IsZero() {
		t.Fatal("and it must not move the deadline the session believes in")
	}
}

// ---------------------------------------------------------------------------
// A stream double, for the two rotation tests
// ---------------------------------------------------------------------------

// recordingStream is the narrowest WorkerService_StreamServer a session needs
// to SEND. Recv is never called here -- these tests drive handlers directly --
// so it answers io.EOF rather than pretending to be a wire.
type recordingStream struct {
	memqlv1.WorkerService_StreamServer
	sent []*memqlv1.WorkerServerMessage
}

func (r *recordingStream) Send(m *memqlv1.WorkerServerMessage) error {
	r.sent = append(r.sent, m)
	return nil
}

func (r *recordingStream) Recv() (*memqlv1.WorkerClientMessage, error) { return nil, io.EOF }
func (r *recordingStream) Context() context.Context                    { return context.Background() }

func (r *recordingStream) last(t *testing.T) *memqlv1.WorkerServerMessage {
	t.Helper()
	if len(r.sent) == 0 {
		t.Fatal("nothing was sent -- the handler returned without answering, which parks the cockpit")
	}
	return r.sent[len(r.sent)-1]
}

func captureSends(s *streamSession) *recordingStream {
	rec := &recordingStream{}
	s.stream = rec
	return rec
}

// ---------------------------------------------------------------------------
// D8 -- the clear names the holder it expects
// ---------------------------------------------------------------------------

func TestTheDisconnectClearNamesTheHolderItExpects(t *testing.T) {
	// M-13. The session guards by read-then-write, and clearWorkerConnectedNode
	// wrote UNCONDITIONALLY -- so a sibling replica that stamped its own hold
	// between the two was wiped, and StreamHeld read false for a machine that
	// was connected the whole time.
	//
	// The guard cannot live in the mutation: a .memql body carries no filter,
	// and an args field it never references is refused at load, so an
	// expectedNodeId argument there could only be decoration. What this pins
	// is that the expectation REACHES the store, which is where it can be
	// enforced.
	store := &fakeRegistrationStore{}
	store.byUser = []RegistrationRow{{ID: "reg-1", OwnerUserId: "owner", ConnectedNodeId: "agent-dying"}}
	srv := &server{
		store:    store,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		nodeId:   "agent-dying",
		registry: NewRegistry(nil, time.Now),
	}
	w := &Worker{RegistrationId: "reg-1", OwnerUserId: "owner"}
	session := newStreamSession(srv, nil, w, context.Background(), func() {})

	session.clearConnectedNode()

	if len(store.clearedExpectations) != 1 {
		t.Fatalf("expected one clear, got %d", len(store.clearedExpectations))
	}
	if store.clearedExpectations[0] != "agent-dying" {
		t.Fatalf("the clear must name the node it expects to still hold the row, got %q -- an "+
			"unconditional clear wipes a successor's hold", store.clearedExpectations[0])
	}
}

// ---------------------------------------------------------------------------
// D4 -- the default lifetime
// ---------------------------------------------------------------------------

func TestTheDefaultTokenLifetimeIsTheOneTheConceptDocuments(t *testing.T) {
	// v1:identity:identity's worker_token variant has documented 90-day
	// rotation since the credential shipped, and nothing implemented it:
	// pairing minted time.Time{}, which resolveWorkerToken reads as
	// non-expiring. This pins the figure to the documentation rather than
	// leaving two numbers in two places.
	if workertoken.DefaultTTL != 90*24*time.Hour {
		t.Fatalf("the documented lifetime is 90 days, got %s", workertoken.DefaultTTL)
	}
	// And the grace is a reconnect-and-retry window rather than a second
	// credential: two indefinitely valid hashes for one machine is the
	// duplicate-credential state design D9 exists to revoke.
	if workertoken.RotationGrace > time.Hour {
		t.Fatalf("the rotation grace must stay short, got %s", workertoken.RotationGrace)
	}
}

func TestReconnectingUnderTheSameCredentialSpeltDifferentlyRevokesNothing(t *testing.T) {
	// THE WORST BUG THIS FUNCTION COULD HAVE, and a raw `!=` is all it takes.
	//
	// The engine bare-ifies ids on egress, so the identityId stored on a
	// registration row and the one the interceptor resolved from a presented
	// token are routinely the SAME credential written two ways. Compared
	// rawly, reclaim reads "displaced" for a machine that merely reconnected
	// and revokes the very token that just authenticated it -- so the machine
	// connects and immediately loses its credential, every time, and the
	// symptom is a laptop that pairs and then cannot come back.
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{
		byUser: []RegistrationRow{{
			ID:          "reg-1",
			OwnerUserId: "user-1",
			// Canonical on the row...
			IdentityId: "v1:identity:identity:wkr-abc",
			Labels:     map[string]string{"machineId": "mac-123"},
		}},
	}
	srv := newUpsertTestServer(store, now)

	_, err := srv.upsertRegistration(context.Background(),
		// ...and bare from the resolver.
		&WorkerIdentity{IdentityId: "wkr-abc", OwnerUserId: "user-1", Active: true},
		&memqlv1.Register{
			Capabilities: []string{CapabilityHeadless},
			Labels:       map[string]string{"machineId": "mac-123"},
		}, nil, now, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	if len(store.revokedIdentities) != 0 {
		t.Fatalf("the same credential in two spellings must not read as a displaced one: revoked %v",
			store.revokedIdentities)
	}
}
