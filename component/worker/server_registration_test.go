package worker

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// fakeRegistrationStore is a Store double for the upsert path. It
// records every persistence call so tests can assert on exactly what
// the server wrote for new vs. existing registrations.
type fakeRegistrationStore struct {
	existing *RegistrationRow

	created     []RegistrationRow
	refreshed   []RegistrationRow
	lastSeen    []string
	lastSeenAts []time.Time
	lastSeenErr error

	// lastSeenFlushes records the whole flush argument list, not just the
	// registration id -- connectedNodeId and activeCount are the fields
	// memql#4350 added and the ones a regression would drop silently.
	lastSeenFlushes []lastSeenFlush
	// cleared records ClearConnectedNode calls; clearedOwners is checked
	// alongside because an unstamped owner is refused by the write guard.
	cleared       []string
	clearedOwners []string
	// lookupOwners records the owner each WorkerByIdentityId was asked
	// under. An empty one means the real store would have read zero rows.
	lookupOwners []string
}

// lastSeenFlush is one UpdateLastSeen call, recorded whole.
type lastSeenFlush struct {
	RegistrationId  string
	OwnerUserId     string
	At              time.Time
	SourceIP        string
	ConnectedNodeId string
	ActiveCount     int
}

var _ Store = (*fakeRegistrationStore)(nil)

func (f *fakeRegistrationStore) CreateRegistration(ctx context.Context, row RegistrationRow) error {
	f.created = append(f.created, row)
	return nil
}

func (f *fakeRegistrationStore) RefreshRegistration(ctx context.Context, row RegistrationRow) error {
	f.refreshed = append(f.refreshed, row)
	return nil
}

func (f *fakeRegistrationStore) UpdateLastSeen(ctx context.Context, registrationId, ownerUserId string, lastSeenAt time.Time, sourceIP, connectedNodeId string, activeCount int) error {
	if f.lastSeenErr != nil {
		return f.lastSeenErr
	}
	f.lastSeen = append(f.lastSeen, registrationId)
	f.lastSeenAts = append(f.lastSeenAts, lastSeenAt)
	f.lastSeenFlushes = append(f.lastSeenFlushes, lastSeenFlush{
		RegistrationId:  registrationId,
		OwnerUserId:     ownerUserId,
		At:              lastSeenAt,
		SourceIP:        sourceIP,
		ConnectedNodeId: connectedNodeId,
		ActiveCount:     activeCount,
	})
	return nil
}

func (f *fakeRegistrationStore) ClearConnectedNode(ctx context.Context, registrationId, ownerUserId string) error {
	f.cleared = append(f.cleared, registrationId)
	f.clearedOwners = append(f.clearedOwners, ownerUserId)
	return nil
}

func (f *fakeRegistrationStore) RevokeRegistration(ctx context.Context, registrationId, ownerUserId, revokedBy, reason string, at time.Time) error {
	return nil
}

func (f *fakeRegistrationStore) WorkerByIdentityId(ctx context.Context, identityId, ownerUserId string) (*RegistrationRow, error) {
	f.lookupOwners = append(f.lookupOwners, ownerUserId)
	if f.existing != nil && f.existing.IdentityId == identityId {
		cp := *f.existing
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeRegistrationStore) WorkersForUser(ctx context.Context, ownerUserId string) ([]RegistrationRow, error) {
	return nil, nil
}

func (f *fakeRegistrationStore) CreateInvocation(ctx context.Context, row InvocationRow) error {
	return nil
}

func (f *fakeRegistrationStore) IdentityByTokenHash(ctx context.Context, tokenHash string) (*WorkerIdentity, error) {
	return nil, nil
}

// testNodeId is the MEMQL_NODE_ID the test servers claim. Named rather than
// inlined so an assertion reads as "the node that held the stream" instead of
// as a matching string literal.
const testNodeId = "agent-7"

func newUpsertTestServer(store Store, now time.Time) *server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(logger, store, NewRegistry(nil, func() time.Time { return now }), nil, func() time.Time { return now }, testNodeId)
}

func upsertTestIdentity() *WorkerIdentity {
	return &WorkerIdentity{
		IdentityId:  "ident-1",
		OwnerUserId: "user-1",
		Active:      true,
	}
}

func mustDescriptor(t *testing.T, raw string) *CapabilityDescriptor {
	t.Helper()
	d, err := ParseCapabilityDescriptor(raw)
	if err != nil {
		t.Fatalf("parse descriptor: %v", err)
	}
	return d
}

func runUpsert(t *testing.T, store *fakeRegistrationStore, register *memqlv1.Register) RegistrationRow {
	t.Helper()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	srv := newUpsertTestServer(store, now)
	descriptor, err := validateRegister(register)
	if err != nil {
		t.Fatalf("validateRegister: %v", err)
	}
	row, err := srv.upsertRegistration(context.Background(), upsertTestIdentity(), register, descriptor, now, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("upsertRegistration: %v", err)
	}
	return row
}

func existingRow(descriptor *CapabilityDescriptor) *RegistrationRow {
	return &RegistrationRow{
		ID:                   "reg-1",
		IdentityId:           "ident-1",
		OwnerUserId:          "user-1",
		Name:                 "mbp",
		Capabilities:         []string{CapabilityHeadless},
		CapabilityDescriptor: descriptor,
		Version:              "0.8.0",
		RegisteredAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// TestUpsertRegistration_NewWorkerCreates is the baseline: no
// existing row means a fresh CreateRegistration, no refresh.
func TestUpsertRegistration_NewWorkerCreates(t *testing.T) {
	store := &fakeRegistrationStore{}
	row := runUpsert(t, store, &memqlv1.Register{
		Name:                     "mbp",
		Capabilities:             []string{CapabilityHeadless, CapabilityComputerUse},
		CapabilityDescriptorJson: validDescriptorJSON,
	})
	if len(store.created) != 1 || len(store.refreshed) != 0 {
		t.Fatalf("expected 1 create / 0 refresh, got %d / %d", len(store.created), len(store.refreshed))
	}
	if row.ID == "" || row.CapabilityDescriptor == nil {
		t.Fatalf("created row incomplete: %+v", row)
	}
}

// TestUpsertRegistration_RefreshesChangedCapabilitiesAndDescriptor is
// the memql#1332 regression: a reconnect with a different
// capabilities list and a different descriptor must persist BOTH on
// the existing row, not just bump lastSeenAt.
func TestUpsertRegistration_RefreshesChangedCapabilitiesAndDescriptor(t *testing.T) {
	oldDesc := mustDescriptor(t, `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":["screenshot"],"schemaVersion":1}`)
	store := &fakeRegistrationStore{existing: existingRow(oldDesc)}

	newDescJSON := `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":["screenshot","mouse_move","key_type"],"schemaVersion":1}`
	row := runUpsert(t, store, &memqlv1.Register{
		Name:                     "mbp",
		Capabilities:             []string{CapabilityHeadless, CapabilityComputerUse},
		CapabilityDescriptorJson: newDescJSON,
		Version:                  "0.9.0",
	})

	if len(store.created) != 0 || len(store.refreshed) != 1 {
		t.Fatalf("expected 0 create / 1 refresh, got %d / %d", len(store.created), len(store.refreshed))
	}
	got := store.refreshed[0]
	if got.ID != "reg-1" {
		t.Fatalf("refresh must target the existing row, got id %q", got.ID)
	}
	if !got.RegisteredAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("registeredAt must be preserved, got %v", got.RegisteredAt)
	}
	if len(got.Capabilities) != 2 {
		t.Fatalf("capabilities not refreshed: %v", got.Capabilities)
	}
	if got.CapabilityDescriptor == nil || len(got.CapabilityDescriptor.Actions) != 3 {
		t.Fatalf("descriptor not refreshed: %+v", got.CapabilityDescriptor)
	}
	if got.Version != "0.9.0" {
		t.Fatalf("version not refreshed: %q", got.Version)
	}
	if row.ID != "reg-1" || row.CapabilityDescriptor == nil || len(row.CapabilityDescriptor.Actions) != 3 {
		t.Fatalf("returned row stale: %+v", row)
	}
	if len(store.lastSeen) != 0 {
		t.Fatalf("upsert must not also call UpdateLastSeen, got %v", store.lastSeen)
	}
}

// TestUpsertRegistration_DescriptorAddedWhereThereWasNone covers the
// cockpit-upgrade direction: an old row without a descriptor gains
// one when the upgraded worker reconnects.
func TestUpsertRegistration_DescriptorAddedWhereThereWasNone(t *testing.T) {
	store := &fakeRegistrationStore{existing: existingRow(nil)}
	runUpsert(t, store, &memqlv1.Register{
		Name:                     "mbp",
		Capabilities:             []string{CapabilityHeadless, CapabilityComputerUse},
		CapabilityDescriptorJson: validDescriptorJSON,
	})
	if len(store.refreshed) != 1 {
		t.Fatalf("expected 1 refresh, got %d", len(store.refreshed))
	}
	got := store.refreshed[0].CapabilityDescriptor
	if got == nil || got.DisplayServer != "quartz" || len(got.Actions) != 4 {
		t.Fatalf("descriptor not persisted on refresh: %+v", got)
	}
}

// TestUpsertRegistration_DescriptorOmittedClearsPersisted locks the
// omission semantics: when a re-registration no longer carries a
// descriptor, the refresh row carries nil so the store CLEARS the
// persisted one rather than keeping the stale copy.
func TestUpsertRegistration_DescriptorOmittedClearsPersisted(t *testing.T) {
	oldDesc := mustDescriptor(t, validDescriptorJSON)
	store := &fakeRegistrationStore{existing: existingRow(oldDesc)}
	row := runUpsert(t, store, &memqlv1.Register{
		Name:         "mbp",
		Capabilities: []string{CapabilityHeadless},
	})
	if len(store.refreshed) != 1 {
		t.Fatalf("expected 1 refresh, got %d", len(store.refreshed))
	}
	if store.refreshed[0].CapabilityDescriptor != nil {
		t.Fatalf("omitted descriptor must clear the persisted one, got %+v", store.refreshed[0].CapabilityDescriptor)
	}
	if row.CapabilityDescriptor != nil {
		t.Fatalf("returned row must drop the descriptor, got %+v", row.CapabilityDescriptor)
	}
}

// TestUpsertRegistration_LegacyWorkerUnchanged: a worker that never
// sent a descriptor keeps registering with none -- before and after.
func TestUpsertRegistration_LegacyWorkerUnchanged(t *testing.T) {
	store := &fakeRegistrationStore{existing: existingRow(nil)}
	row := runUpsert(t, store, &memqlv1.Register{
		Name:         "mbp",
		Capabilities: []string{CapabilityHeadless},
	})
	if len(store.refreshed) != 1 {
		t.Fatalf("expected 1 refresh, got %d", len(store.refreshed))
	}
	got := store.refreshed[0]
	if got.CapabilityDescriptor != nil || row.CapabilityDescriptor != nil {
		t.Fatalf("legacy worker must stay descriptor-less: %+v", got.CapabilityDescriptor)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != CapabilityHeadless {
		t.Fatalf("legacy capabilities changed: %v", got.Capabilities)
	}
	if got.ID != "reg-1" {
		t.Fatalf("refresh must keep the existing row id, got %q", got.ID)
	}
}

// TestUpsertRegistration_RevokedRowRejected: a revoked registration
// must not be refreshed back to life.
func TestUpsertRegistration_RevokedRowRejected(t *testing.T) {
	revoked := existingRow(nil)
	revoked.RevokedAt = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{existing: revoked}

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	srv := newUpsertTestServer(store, now)
	register := &memqlv1.Register{Name: "mbp", Capabilities: []string{CapabilityHeadless}}
	descriptor, err := validateRegister(register)
	if err != nil {
		t.Fatalf("validateRegister: %v", err)
	}
	if _, err := srv.upsertRegistration(context.Background(), upsertTestIdentity(), register, descriptor, now, ""); err == nil {
		t.Fatalf("expected revoked-worker error")
	}
	if len(store.refreshed) != 0 || len(store.created) != 0 {
		t.Fatalf("revoked row must not be written: %d created / %d refreshed", len(store.created), len(store.refreshed))
	}
}

// fakeEngineExecutor captures the raw queries EngineStore emits, and the
// CONTEXT each was executed on. The context matters as much as the query
// string here: v1:worker:registration is owner-tiered, so a call on an
// actor-less context is refused by the write guard (or silently reads nothing)
// no matter how correct its arguments are.
type fakeEngineExecutor struct {
	queries []string
	ctxs    []context.Context
}

func (f *fakeEngineExecutor) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	f.ctxs = append(f.ctxs, ctx)
	return nil, nil
}

// actorUserId reads back the actor the store stamped, or "" when it stamped
// none.
func actorUserId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	return ac.UserId
}

// TestEngineStoreRefreshRegistration_WireShape asserts the engine
// mutation EngineStore.RefreshRegistration emits: the right mutation
// name, the refreshed fields, and `"capabilityDescriptor":null` when
// the row carries no descriptor (which the mutation coalesces to {}
// -- the clear path).
func TestEngineStoreRefreshRegistration_WireShape(t *testing.T) {
	eng := &fakeEngineExecutor{}
	store := &EngineStore{Engine: eng}

	desc := mustDescriptor(t, validDescriptorJSON)
	row := RegistrationRow{
		ID: "reg-1",
		// The owner is not decoration: ownerActor refuses a blank one, so a
		// row without it never reaches the engine at all.
		OwnerUserId:          "user-1",
		Name:                 "mbp",
		Capabilities:         []string{CapabilityHeadless, CapabilityComputerUse},
		CapabilityDescriptor: desc,
		Version:              "0.9.0",
		BuildTag:             "computeruse",
		LastSeenAt:           time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		LastConnectedFromIP:  "10.0.0.1:1234",
	}
	if err := store.RefreshRegistration(context.Background(), row); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(eng.queries) != 1 {
		t.Fatalf("expected 1 engine call, got %d", len(eng.queries))
	}
	q := eng.queries[0]
	for _, want := range []string{
		"refreshWorkerRegistration(",
		`"registrationId":"reg-1"`,
		`"displayServer":"quartz"`,
		`"version":"0.9.0"`,
		`"buildTag":"computeruse"`,
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("query missing %q:\n%s", want, q)
		}
	}

	// Nil descriptor serializes as null -> coalesced to {} by the
	// mutation, clearing the persisted descriptor.
	row.CapabilityDescriptor = nil
	if err := store.RefreshRegistration(context.Background(), row); err != nil {
		t.Fatalf("refresh without descriptor: %v", err)
	}
	if !strings.Contains(eng.queries[1], `"capabilityDescriptor":null`) {
		t.Fatalf("nil descriptor must serialize as null:\n%s", eng.queries[1])
	}
}
