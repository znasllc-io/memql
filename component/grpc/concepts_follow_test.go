package memql

// concepts_follow_test.go -- the follow-mode registry-delta stream on
// ConceptsSubscribeMsg (memql#4238), driven through the real handler + a real
// DB-less engine. A concept promoted through one session's engine must reach a
// SECOND session subscribed to the same engine, prove unsubscribe/close stops
// delivery, that the generation increments, and that a stale-generation client
// re-snapshots to a higher generation.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// followTestConceptSrc is a standalone trainable concept (no cross-concept
// binding, so it compiles against the core registry Gate-1 clones).
const followTestConceptSrc = `@version("1.0.0")
@namespace("followns")
@description("A concept trained into a running cluster, for the follow stream")
concept followWidget {
  ownerUserId  string  @required
  label        string
}`

const followTestConceptId = "v1:followns:followWidget"

// followTestEngine boots a DB-less engine with the full embedded DSL tree -- the
// concept promote path is in-memory (registry merge + broadcaster emit) and
// needs no database.
func followTestEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	// The engine binds to the PACKAGE-GLOBAL DefaultRegistry, and a concept
	// promote merges into it -- which would leak across tests in this package
	// (a later test would see the concept with no promotion marker and refuse
	// it as core). Snapshot + restore, mirroring
	// promoteConceptEngineOnTheDefaultRegistry in the engine package.
	before := concept.All()
	t.Cleanup(func() { concept.ReplaceAll(before) })
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("memqlengine.New(nil): %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

func followTestSession(t *testing.T, eng *memqlengine.MemQLEngine) (*streamSession, *captureStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := &service{engine: eng, conceptRegistry: eng.Concepts(), logger: logger}
	cs := newCaptureStream(t)
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: "v1:identity:user:follow"})
	return &streamSession{
		service:      svc,
		stream:       cs,
		logger:       logger,
		closeChan:    make(chan struct{}),
		access:       &auth.AccessContext{UserId: "v1:identity:user:follow", Role: auth.RoleOwner},
		accessLoaded: true,
	}, cs
}

// promoteFollowConcept authors + promotes the fixture concept through the
// exported engine API (no DB), which fires the broadcaster.
func promoteFollowConcept(t *testing.T, eng *memqlengine.MemQLEngine) {
	t.Helper()
	reg := memqlengine.NewAuthoredRuntimeRegistry()
	if _, err := memqlengine.AuthorSessionBundle(reg, "v1:identity:user:follow", followTestConceptSrc, ""); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, ok := reg.Lookup("v1:identity:user:follow", "concept", "followWidget")
	if !ok {
		t.Fatal("session define did not register the concept")
	}
	if err := eng.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("promote concept: %v", err)
	}
}

// registryDeltas returns every ConceptsRegistryDelta captured so far, in order.
func registryDeltas(cs *captureStream) []*memqlv1.ConceptsRegistryDelta {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []*memqlv1.ConceptsRegistryDelta
	for _, m := range cs.sent {
		if d := m.GetConceptsRegistryDelta(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// waitForDeltaAdding polls until a delta whose Added contains conceptId arrives,
// or fails. Returns that delta.
func waitForDeltaAdding(t *testing.T, cs *captureStream, conceptId string) *memqlv1.ConceptsRegistryDelta {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range registryDeltas(cs) {
			if d.GetReset_() {
				continue
			}
			for _, a := range d.GetAdded() {
				if a.GetId() == conceptId {
					return d
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no incremental delta adding %q arrived within 3s", conceptId)
	return nil
}

// TestConceptsFollow_SnapshotThenLiveDeltaAcrossTwoSessions is the core test: a
// promote through the shared engine reaches BOTH follow sessions, and the
// snapshot precedes the live delta on each.
func TestConceptsFollow_SnapshotThenLiveDeltaAcrossTwoSessions(t *testing.T) {
	eng := followTestEngine(t)

	sessA, csA := followTestSession(t, eng)
	sessB, csB := followTestSession(t, eng)
	t.Cleanup(func() { close(sessA.closeChan); close(sessB.closeChan) })

	envA := &memqlv1.MemqlClientMessage{MessageId: "mA"}
	envB := &memqlv1.MemqlClientMessage{MessageId: "mB"}
	if err := sessA.handleConceptsSubscribe(envA, &memqlv1.ConceptsSubscribeMsg{RequestId: "rA", Follow: true}); err != nil {
		t.Fatalf("follow subscribe A: %v", err)
	}
	if err := sessB.handleConceptsSubscribe(envB, &memqlv1.ConceptsSubscribeMsg{RequestId: "rB", Follow: true}); err != nil {
		t.Fatalf("follow subscribe B: %v", err)
	}

	// Each session's first message is the snapshot: reset=true, carrying the core
	// registry, and NOT yet containing the fixture concept.
	for _, cs := range []*captureStream{csA, csB} {
		deltas := registryDeltas(cs)
		if len(deltas) == 0 {
			t.Fatal("no snapshot delta was sent")
		}
		snap := deltas[0]
		if !snap.GetReset_() {
			t.Fatal("the first delta must be a reset snapshot")
		}
		if len(snap.GetAdded()) == 0 {
			t.Fatal("the snapshot should carry the current concept set")
		}
		if snap.GetSubscriptionId() == "" {
			t.Fatal("the snapshot must carry a subscription_id for unsubscribe")
		}
		for _, a := range snap.GetAdded() {
			if a.GetId() == followTestConceptId {
				t.Fatal("the fixture concept is in the snapshot before it was promoted")
			}
		}
	}

	// Promote once; both sessions receive an incremental Added delta.
	promoteFollowConcept(t, eng)
	dA := waitForDeltaAdding(t, csA, followTestConceptId)
	dB := waitForDeltaAdding(t, csB, followTestConceptId)

	// The delta descriptor carries the ConceptInfo projection (domain parsed).
	if dA.GetAdded()[0].GetDomain() != "followns" {
		t.Fatalf("delta ConceptInfo not projected: domain=%q", dA.GetAdded()[0].GetDomain())
	}
	// Generation strictly greater than the snapshot's on each session.
	if dA.GetGeneration() <= registryDeltas(csA)[0].GetGeneration() {
		t.Fatalf("live delta generation %d not past snapshot generation %d", dA.GetGeneration(), registryDeltas(csA)[0].GetGeneration())
	}
	_ = dB
}

// TestConceptsFollow_UnsubscribeStopsDelivery: after an UnsubscribeMsg for the
// snapshot's subscription_id, a later promote produces no further delta on that
// session, while a still-subscribed session keeps receiving.
func TestConceptsFollow_UnsubscribeStopsDelivery(t *testing.T) {
	eng := followTestEngine(t)
	sessA, csA := followTestSession(t, eng)
	sessB, csB := followTestSession(t, eng)
	t.Cleanup(func() { close(sessB.closeChan) })

	envA := &memqlv1.MemqlClientMessage{MessageId: "mA"}
	envB := &memqlv1.MemqlClientMessage{MessageId: "mB"}
	if err := sessA.handleConceptsSubscribe(envA, &memqlv1.ConceptsSubscribeMsg{RequestId: "rA", Follow: true}); err != nil {
		t.Fatalf("follow subscribe A: %v", err)
	}
	if err := sessB.handleConceptsSubscribe(envB, &memqlv1.ConceptsSubscribeMsg{RequestId: "rB", Follow: true}); err != nil {
		t.Fatalf("follow subscribe B: %v", err)
	}

	subIdA := registryDeltas(csA)[0].GetSubscriptionId()
	if err := sessA.handleUnsubscribe(&memqlv1.MemqlClientMessage{MessageId: "u"}, &memqlv1.UnsubscribeMsg{SubscriptionId: subIdA}); err != nil {
		t.Fatalf("unsubscribe A: %v", err)
	}
	// The unsubscribe closed the broadcaster channel; let the forwarder exit.
	time.Sleep(50 * time.Millisecond)
	beforeA := len(registryDeltas(csA))

	promoteFollowConcept(t, eng)

	// B still gets it; A does not.
	waitForDeltaAdding(t, csB, followTestConceptId)
	time.Sleep(100 * time.Millisecond)
	if afterA := len(registryDeltas(csA)); afterA != beforeA {
		t.Fatalf("session A received %d delta(s) after unsubscribing (had %d)", afterA-beforeA, beforeA)
	}
}

// TestConceptsFollow_GenerationIncrementsAndStaleReSnapshots: two promotes bump
// the generation by one each, and a client that reconnects (a fresh follow)
// gets a snapshot at the LATEST generation -- the re-snapshot recovery path.
func TestConceptsFollow_GenerationIncrementsAndStaleReSnapshots(t *testing.T) {
	eng := followTestEngine(t)
	sess, cs := followTestSession(t, eng)
	t.Cleanup(func() { close(sess.closeChan) })

	if err := sess.handleConceptsSubscribe(&memqlv1.MemqlClientMessage{MessageId: "m"}, &memqlv1.ConceptsSubscribeMsg{RequestId: "r", Follow: true}); err != nil {
		t.Fatalf("follow subscribe: %v", err)
	}
	snapGen := registryDeltas(cs)[0].GetGeneration()

	promoteFollowConcept(t, eng)
	d1 := waitForDeltaAdding(t, cs, followTestConceptId)
	if d1.GetGeneration() != snapGen+1 {
		t.Fatalf("first live delta generation = %d, want %d", d1.GetGeneration(), snapGen+1)
	}

	// Demote (zero rows -> remove), then re-promote, so the generation advances
	// twice more. The engine has no DB, so demote's row count is unknowable and
	// would fail closed; stub it to zero rows via the engine's exported knob is
	// not available here, so instead promote a SECOND distinct concept to bump
	// the generation deterministically.
	promoteSecondFollowConcept(t, eng)
	d2 := waitForDeltaAdding(t, cs, "v1:followns:followWidgetTwo")
	if d2.GetGeneration() != d1.GetGeneration()+1 {
		t.Fatalf("second live delta generation = %d, want %d", d2.GetGeneration(), d1.GetGeneration()+1)
	}

	// A stale client reconnects: a fresh follow returns a snapshot whose
	// generation is the latest, and whose Added now includes both concepts.
	sess2, cs2 := followTestSession(t, eng)
	t.Cleanup(func() { close(sess2.closeChan) })
	if err := sess2.handleConceptsSubscribe(&memqlv1.MemqlClientMessage{MessageId: "m2"}, &memqlv1.ConceptsSubscribeMsg{RequestId: "r2", Follow: true}); err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	snap2 := registryDeltas(cs2)[0]
	if !snap2.GetReset_() {
		t.Fatal("re-follow first message must be a reset snapshot")
	}
	if snap2.GetGeneration() != d2.GetGeneration() {
		t.Fatalf("re-snapshot generation = %d, want the latest %d", snap2.GetGeneration(), d2.GetGeneration())
	}
	var sawOne, sawTwo bool
	for _, a := range snap2.GetAdded() {
		if a.GetId() == followTestConceptId {
			sawOne = true
		}
		if a.GetId() == "v1:followns:followWidgetTwo" {
			sawTwo = true
		}
	}
	if !sawOne || !sawTwo {
		t.Fatalf("re-snapshot missing a promoted concept (one=%v two=%v)", sawOne, sawTwo)
	}
}

const followTestConceptTwoSrc = `@version("1.0.0")
@namespace("followns")
@description("A second trained concept")
concept followWidgetTwo {
  ownerUserId  string  @required
  label        string
}`

func promoteSecondFollowConcept(t *testing.T, eng *memqlengine.MemQLEngine) {
	t.Helper()
	reg := memqlengine.NewAuthoredRuntimeRegistry()
	if _, err := memqlengine.AuthorSessionBundle(reg, "v1:identity:user:follow", followTestConceptTwoSrc, ""); err != nil {
		t.Fatalf("author second concept: %v", err)
	}
	c, ok := reg.Lookup("v1:identity:user:follow", "concept", "followWidgetTwo")
	if !ok {
		t.Fatal("session define did not register the second concept")
	}
	if err := eng.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("promote second concept: %v", err)
	}
}
