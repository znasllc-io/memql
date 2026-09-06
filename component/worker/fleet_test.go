package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// -----------------------------------------------------------------------------
// Design D3: the owner's fields survive the machine that carries them
// -----------------------------------------------------------------------------

// TestRefreshRegistration_PreservesOperatorLabelsAndDisplayName is the design-D3
// regression, and it is written as an assertion about ABSENCE because that is
// what the design is: update{} is a read-merge, so operatorLabels and
// displayName survive a reconnect precisely by not appearing in the refresh
// call. A "complete the field list" edit to EngineStore.RefreshRegistration is
// the change this catches -- it would compile, pass every other test, and erase
// an operator's tags on the next lid close.
//
// The row deliberately carries both fields AND a fresh `labels` map, so the
// test also pins the other half: `labels` IS overwritten from the Register
// message, which is why the operator's tags cannot live there.
func TestRefreshRegistration_PreservesOperatorLabelsAndDisplayName(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	row := RegistrationRow{
		ID:             "reg-1",
		OwnerUserId:    "user-1",
		Name:           "hostname-from-cockpit",
		DisplayName:    "Jose's laptop",
		Capabilities:   []string{CapabilityHeadless},
		Labels:         map[string]string{"os": "darwin"},
		OperatorLabels: map[string]string{"team": "platform"},
		LastSeenAt:     time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := store.RefreshRegistration(context.Background(), row); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(eng.queries) != 1 {
		t.Fatalf("expected 1 engine call, got %d", len(eng.queries))
	}
	q := eng.queries[0]
	for _, forbidden := range []string{"operatorLabels", "displayName"} {
		if strings.Contains(q, forbidden) {
			t.Fatalf("refresh must NOT write %s -- update{} is a read-merge and that is how the owner's value survives a reconnect:\n%s", forbidden, q)
		}
	}
	// Spelled as NAMED ARGUMENTS since memql#5004. These assertions used to
	// read `"labels":{...}` because the store rendered the retired
	// object-literal wrapper, which the parser refuses -- so they were
	// asserting the content of a statement that never ran. The content is
	// what they mean; render_parses_test.go owns the form.
	if !strings.Contains(q, `labels: {"os":"darwin"}`) {
		t.Fatalf("refresh must still overwrite the cockpit's own labels:\n%s", q)
	}
	if !strings.Contains(q, `name: "hostname-from-cockpit"`) {
		t.Fatalf("refresh must still overwrite the cockpit's own name:\n%s", q)
	}
}

// TestUpsertRegistration_NeverWritesOperatorLabelsOrDisplayName covers the same
// prohibition one layer up: the row the server hands the store must not carry
// values for either field, whatever the Register message said. The Register
// proto has no field for them today; this fails if one is ever plumbed in.
func TestUpsertRegistration_NeverWritesOperatorLabelsOrDisplayName(t *testing.T) {
	store := &fakeRegistrationStore{existing: existingRow(nil)}
	runUpsert(t, store, &memqlv1.Register{
		Name:         "mbp",
		Capabilities: []string{CapabilityHeadless},
		Labels:       map[string]string{"os": "darwin"},
	})
	if len(store.refreshed) != 1 {
		t.Fatalf("expected 1 refresh, got %d", len(store.refreshed))
	}
	got := store.refreshed[0]
	if len(got.OperatorLabels) != 0 {
		t.Fatalf("register must not populate operatorLabels, got %v", got.OperatorLabels)
	}
	if got.DisplayName != "" {
		t.Fatalf("register must not populate displayName, got %q", got.DisplayName)
	}
}

// -----------------------------------------------------------------------------
// connectedNodeId: stamped on register, re-asserted on flush, cleared on close
// -----------------------------------------------------------------------------

// TestUpsertRegistration_StampsConnectedNodeId: whichever replica holds the
// stream must be named on the row, on the create path and on the refresh path
// alike. A machine whose row names no node is one a router cannot forward to.
func TestUpsertRegistration_StampsConnectedNodeId(t *testing.T) {
	fresh := &fakeRegistrationStore{}
	runUpsert(t, fresh, &memqlv1.Register{Name: "mbp", Capabilities: []string{CapabilityHeadless}})
	if len(fresh.created) != 1 || fresh.created[0].ConnectedNodeId != testNodeId {
		t.Fatalf("create must stamp connectedNodeId=%q, got %+v", testNodeId, fresh.created)
	}

	reconnect := &fakeRegistrationStore{existing: existingRow(nil)}
	runUpsert(t, reconnect, &memqlv1.Register{Name: "mbp", Capabilities: []string{CapabilityHeadless}})
	if len(reconnect.refreshed) != 1 || reconnect.refreshed[0].ConnectedNodeId != testNodeId {
		t.Fatalf("refresh must stamp connectedNodeId=%q, got %+v", testNodeId, reconnect.refreshed)
	}
}

// TestHandleHeartbeat_FlushCarriesConnectedNodeAndActiveCount: the flush is
// what keeps connectedNodeId true over the life of a stream (a row written at
// register and never re-asserted goes wrong the moment anything rebalances),
// and it is the only writer of activeCount.
func TestHandleHeartbeat_FlushCarriesConnectedNodeAndActiveCount(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	session.handleHeartbeat(&memqlv1.Heartbeat{
		Ts:               timestamppb.New(t0),
		ActiveCallsTotal: 3,
	}, "10.0.0.1:1234")

	if len(store.lastSeenFlushes) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(store.lastSeenFlushes))
	}
	got := store.lastSeenFlushes[0]
	if got.ConnectedNodeId != testNodeId {
		t.Fatalf("flush must carry connectedNodeId=%q, got %q", testNodeId, got.ConnectedNodeId)
	}
	if got.ActiveCount != 3 {
		t.Fatalf("flush must carry the worker's reported active count, got %d", got.ActiveCount)
	}
	if got.OwnerUserId != "user-1" {
		t.Fatalf("flush must name the row owner (the write guard checks it), got %q", got.OwnerUserId)
	}
}

// TestHandleHeartbeat_ActiveCountFallsBackToRegistrySum: a cockpit build that
// predates Heartbeat.active_calls_total sends 0. The server's own count of
// admitted-and-not-released dispatches is the only other view, so it is what
// gets persisted rather than a flat zero.
func TestHandleHeartbeat_ActiveCountFallsBackToRegistrySum(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	// Add() is what allocates activePerCap; Acquire is the real accounting.
	session.server.registry.Add(session.worker)
	for i := 0; i < 2; i++ {
		if err := session.worker.Acquire(context.Background(), CapabilityHeadless); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if got := session.worker.ActiveCount(); got != 2 {
		t.Fatalf("registry sum should be 2, got %d", got)
	}

	session.handleHeartbeat(&memqlv1.Heartbeat{Ts: timestamppb.New(t0)}, "10.0.0.1:1234")
	if len(store.lastSeenFlushes) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(store.lastSeenFlushes))
	}
	if got := store.lastSeenFlushes[0].ActiveCount; got != 2 {
		t.Fatalf("a heartbeat reporting no count must fall back to the registry sum, got %d", got)
	}
}

// TestStreamSessionClose_ClearsConnectedNode: without this the row keeps naming
// a replica that no longer holds the stream, and a dispatch forwarded there
// fails in a way that reads as a mesh fault rather than as an offline laptop.
//
// The session context is already cancelled by the time close runs -- that is
// exactly the trap clearConnectedNode derives a fresh context to avoid -- so
// this also cancels first and asserts the write still happens.
func TestStreamSessionClose_ClearsConnectedNode(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })

	session.cancel() // the disconnect path's real starting condition
	session.close()

	if len(store.cleared) != 1 || store.cleared[0] != "reg-1" {
		t.Fatalf("close must clear connectedNodeId for the registration, got %v", store.cleared)
	}
	if store.clearedOwners[0] != "user-1" {
		t.Fatalf("clear must name the row owner (the write guard checks it), got %q", store.clearedOwners[0])
	}

	// close is once-only; a second call must not write again.
	session.close()
	if len(store.cleared) != 1 {
		t.Fatalf("close must be idempotent, got %d clears", len(store.cleared))
	}
}

// -----------------------------------------------------------------------------
// The borrowed actor
// -----------------------------------------------------------------------------

// TestCreateRegistration_StampsOwnerActorAndSendsNoOwnerArg is the memql#4350
// authorization contract in one test. Two halves, and they are inseparable:
//
//   - createWorkerRegistration no longer DECLARES ownerUserId (the concept
//     marks it @serverSet), so sending one would be an undeclared argument;
//   - the mutation stamps it from actor.userId, and a worker authenticates as
//     worker:<id>, so without the borrowed actor the row would land owned by
//     nobody -- or the write would be refused outright.
func TestCreateRegistration_StampsOwnerActorAndSendsNoOwnerArg(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	row := RegistrationRow{
		ID:           "reg-1",
		OwnerUserId:  "user-1",
		IdentityId:   "ident-1",
		Name:         "mbp",
		Capabilities: []string{CapabilityHeadless},
		RegisteredAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		LastSeenAt:   time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := store.CreateRegistration(context.Background(), row); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(eng.queries) != 1 {
		t.Fatalf("expected 1 engine call, got %d", len(eng.queries))
	}
	if strings.Contains(eng.queries[0], "ownerUserId") {
		t.Fatalf("createWorkerRegistration no longer declares ownerUserId -- it is stamped from the actor:\n%s", eng.queries[0])
	}
	if got := actorUserId(eng.ctxs[0]); got != "user-1" {
		t.Fatalf("create must run under the owner's actor, got %q", got)
	}
}

// TestRegistrationWrites_RefuseABlankOwner: auth.ContextWithUserActor returns
// the context UNCHANGED for a blank id, so a silent fallthrough would produce
// an actor-less call the engine refuses (or, on a read, one that returns
// nothing) far from where the value went missing. Every entry point refuses it
// instead.
func TestRegistrationWrites_RefuseABlankOwner(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}
	ctx := context.Background()

	cases := map[string]func() error{
		"create":       func() error { return store.CreateRegistration(ctx, RegistrationRow{ID: "reg-1"}) },
		"refresh":      func() error { return store.RefreshRegistration(ctx, RegistrationRow{ID: "reg-1"}) },
		"updateSeen":   func() error { return store.UpdateLastSeen(ctx, "reg-1", "", time.Now(), "", "n1", 0) },
		"clearNode":    func() error { return store.ClearConnectedNode(ctx, "reg-1", "") },
		"revoke":       func() error { return store.RevokeRegistration(ctx, "reg-1", "", "admin", "why", time.Now()) },
		"byIdentityId": func() error { _, err := store.WorkerByIdentityId(ctx, "ident-1", ""); return err },
		"forUser":      func() error { _, err := store.WorkersForUser(ctx, ""); return err },
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Fatalf("%s must refuse a blank ownerUserId", name)
		}
	}
	if len(eng.queries) != 0 {
		t.Fatalf("nothing may reach the engine without an owner, got %v", eng.queries)
	}
}

// TestReadsRunUnderTheOwnersActor: the READ gate has no internal-origin bypass,
// so an owned-tier row with no actor in context is DENIED -- these two calls
// return zero rows rather than an error, which is the failure mode that made
// the register handshake insert a duplicate row on every reconnect.
func TestReadsRunUnderTheOwnersActor(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	if _, err := store.WorkerByIdentityId(context.Background(), "ident-1", "user-1"); err != nil {
		t.Fatalf("byIdentityId: %v", err)
	}
	if _, err := store.WorkersForUser(context.Background(), "user-2"); err != nil {
		t.Fatalf("forUser: %v", err)
	}
	if len(eng.ctxs) != 2 {
		t.Fatalf("expected 2 engine calls, got %d", len(eng.ctxs))
	}
	if got := actorUserId(eng.ctxs[0]); got != "user-1" {
		t.Fatalf("workerByIdentityId must run under the owner's actor, got %q", got)
	}
	if got := actorUserId(eng.ctxs[1]); got != "user-2" {
		t.Fatalf("workersForUser must run under the owner's actor, got %q", got)
	}
}

// TestUpsertRegistration_LooksUpUnderTheOwner: the handshake resolves the
// identity (which names the owner) BEFORE the lookup, so there is no excuse
// for an unstamped read here. An empty owner would read as "no registration"
// and insert a second row for the same machine.
func TestUpsertRegistration_LooksUpUnderTheOwner(t *testing.T) {
	store := &fakeRegistrationStore{existing: existingRow(nil)}
	runUpsert(t, store, &memqlv1.Register{Name: "mbp", Capabilities: []string{CapabilityHeadless}})
	if len(store.lookupOwners) != 1 || store.lookupOwners[0] != "user-1" {
		t.Fatalf("lookup must carry the identity's owner, got %v", store.lookupOwners)
	}
}

// -----------------------------------------------------------------------------
// Invocation routing
// -----------------------------------------------------------------------------

// TestEngineStoreCreateInvocation_RoutingWireShape pins how the routing record
// reaches createWorkerInvocation, including the nil case. The mutation writes
// `routing: args.routing ?? {}`, and ?? is BLANK-coalescing rather than
// null-coalescing -- its exact behaviour on a JSON null is easier to check than
// to reason about, so this checks that a nil map does serialize as null and
// therefore hits that coalesce rather than arriving as some other shape.
func TestEngineStoreCreateInvocation_RoutingWireShape(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	row := InvocationRow{
		ID:          "inv-1",
		OwnerUserId: "user-1",
		WorkerId:    "reg-1",
		AgentId:     "agent-1",
		Tool:        "workerHost",
		Action:      "exec",
		Outcome:     "success",
		StartedAt:   time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Routing: map[string]any{
			"policyId": "pol-1",
			"strategy": "roundRobin",
		},
	}
	if err := store.CreateInvocation(context.Background(), row); err != nil {
		t.Fatalf("create invocation: %v", err)
	}
	if !strings.Contains(eng.queries[0], `"strategy":"roundRobin"`) {
		t.Fatalf("routing record missing from the invocation write:\n%s", eng.queries[0])
	}

	row.Routing = nil
	if err := store.CreateInvocation(context.Background(), row); err != nil {
		t.Fatalf("create invocation without routing: %v", err)
	}
	if !strings.Contains(eng.queries[1], `routing: null`) {
		t.Fatalf("a nil routing map must serialize as null for the mutation's ?? {} to fire:\n%s", eng.queries[1])
	}
}

// -----------------------------------------------------------------------------
// Registry index
// -----------------------------------------------------------------------------

// TestRegistryWorkerById returns the live handle for a registration this
// replica holds, and nil for everything else -- including during a drain, when
// the streams are being cancelled and a handle taken now is one whose dispatch
// is about to fail.
func TestRegistryWorkerById(t *testing.T) {
	reg := NewRegistry(nil, func() time.Time { return time.Unix(0, 0).UTC() })
	w := &Worker{RegistrationId: "reg-1", OwnerUserId: "user-1"}
	reg.Add(w)

	if got := reg.WorkerById("reg-1"); got != w {
		t.Fatalf("WorkerById must return the live handle, got %v", got)
	}
	if got := reg.WorkerById("reg-2"); got != nil {
		t.Fatalf("WorkerById must return nil for a registration this replica does not hold, got %v", got)
	}
	if got := reg.WorkerById(""); got != nil {
		t.Fatalf("WorkerById must return nil for an empty id, got %v", got)
	}

	reg.Drain()
	if got := reg.WorkerById("reg-1"); got != nil {
		t.Fatalf("WorkerById must return nil while draining, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// The online rule
// -----------------------------------------------------------------------------

// TestIsOnline is the rule stated as cases. The two that matter most are the
// two-missed-beats boundary (which is what the Fleet page shows) and the
// revoked machine (a decision that outranks any heartbeat).
func TestIsOnline(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Hour)

	cases := []struct {
		name       string
		lastSeenAt time.Time
		revokedAt  time.Time
		want       bool
	}{
		{"just beat", now, time.Time{}, true},
		{"one missed beat", now.Add(-HeartbeatBatchInterval), time.Time{}, true},
		{"exactly at the window", now.Add(-OnlineWindow), time.Time{}, true},
		{"two missed beats plus a tick", now.Add(-OnlineWindow - time.Second), time.Time{}, false},
		{"long gone", now.Add(-time.Hour), time.Time{}, false},
		{"never seen", time.Time{}, time.Time{}, false},
		{"revoked but beating", now, revoked, false},
		{"revoked and stale", now.Add(-time.Hour), revoked, false},
		{"clock skew -- lastSeenAt in the future", now.Add(time.Minute), time.Time{}, true},
	}
	for _, tc := range cases {
		if got := IsOnline(tc.lastSeenAt, tc.revokedAt, now); got != tc.want {
			t.Errorf("%s: IsOnline = %v, want %v", tc.name, got, tc.want)
		}
		row := RegistrationRow{LastSeenAt: tc.lastSeenAt, RevokedAt: tc.revokedAt}
		if got := row.IsOnline(now); got != tc.want {
			t.Errorf("%s: RegistrationRow.IsOnline = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsOnline_FlipsAfterTwoMissedHeartbeats walks the flag over time the way a
// worker actually goes quiet: beating, one beat missed (still online, because a
// late flush must not flap the indicator), two missed (offline).
func TestIsOnline_FlipsAfterTwoMissedHeartbeats(t *testing.T) {
	lastSeen := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	if !IsOnline(lastSeen, time.Time{}, lastSeen.Add(HeartbeatBatchInterval)) {
		t.Fatalf("one missed heartbeat must NOT read as offline -- a late flush would flap the indicator")
	}
	if !IsOnline(lastSeen, time.Time{}, lastSeen.Add(OnlineWindow)) {
		t.Fatalf("the window boundary itself is still online")
	}
	if IsOnline(lastSeen, time.Time{}, lastSeen.Add(OnlineWindow+time.Millisecond)) {
		t.Fatalf("past two missed heartbeats the machine must read as offline")
	}
}

// TestOnlineWindowIsTwoHeartbeatIntervals pins the derivation rather than the
// number: OnlineWindow is a function of the flush cadence, and changing
// HeartbeatBatchInterval must move it rather than leave a stale literal behind.
func TestOnlineWindowIsTwoHeartbeatIntervals(t *testing.T) {
	if OnlineWindow != 2*HeartbeatBatchInterval {
		t.Fatalf("OnlineWindow must stay 2 x HeartbeatBatchInterval, got %v vs %v", OnlineWindow, HeartbeatBatchInterval)
	}
	if HeartbeatBatchInterval != 15*time.Second {
		t.Fatalf("HeartbeatBatchInterval is the freshness budget of `online`; got %v", HeartbeatBatchInterval)
	}
}

// TestEngineStoreCreateInvocation_BorrowsTheOwnersAuthority pins the half of
// memql#4406 that is silent when it breaks.
//
// v1:worker:invocation declares @rowAuthz(owner="ownerUserId", clusterOwner)
// and marks the field @serverSet, so createWorkerInvocation stamps
// actor.userId. A worker authenticates as `worker:<id>`, never as its owner --
// so if this store does not borrow the owner's authority, the row lands with an
// EMPTY ownerUserId and is readable by nobody, including the operator later
// asking why the machine shows no activity. There is no error on that path: the
// mutation succeeds and writes a row nothing can see.
//
// Asserted on the CONTEXT rather than through an engine, because a fake
// executor never reaches the row-authz gate -- the property to check is that
// the store stamped, not what the gate then did with it.
func TestEngineStoreCreateInvocation_BorrowsTheOwnersAuthority(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	row := InvocationRow{
		ID:          "inv-owner-1",
		OwnerUserId: "user-42",
		WorkerId:    "reg-1",
		AgentId:     "agent-1",
		Tool:        "workerHost",
		Action:      "exec",
		Outcome:     "success",
		StartedAt:   time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := store.CreateInvocation(context.Background(), row); err != nil {
		t.Fatalf("create invocation: %v", err)
	}
	if got := actorUserId(eng.ctxs[0]); got != "user-42" {
		t.Fatalf("the invocation write ran as actor %q, want %q. An unstamped write lands a row with "+
			"an empty ownerUserId -- readable by nobody, and with no error to say so.", got, "user-42")
	}
	// ownerUserId must NOT also travel as data: the construct no longer
	// declares it, and an undeclared argument is silently discarded
	// (memql#3626), so passing it would look like belt-and-braces and be
	// nothing at all.
	if strings.Contains(eng.queries[0], "ownerUserId") {
		t.Fatalf("the invocation write still passes ownerUserId as an argument:\n%s", eng.queries[0])
	}

	// A blank owner is REFUSED rather than passed through.
	// auth.ContextWithUserActor returns ctx UNCHANGED for a blank id, so a
	// fallthrough here would produce exactly the unreadable row above.
	row.OwnerUserId = "  "
	if err := store.CreateInvocation(context.Background(), row); err == nil {
		t.Fatal("a blank ownerUserId was accepted; the write would land a row nobody can read")
	}
	if len(eng.queries) != 1 {
		t.Fatalf("the refused call still reached the engine (%d queries)", len(eng.queries))
	}
}
