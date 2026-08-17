package memql

// staged_transition_test.go -- coverage for the STAGED -> LIVE transition and
// its cross-node propagation (epic memql#3974, task memql#3986).
//
// Internal (package memql) test so it can drive the real promote / transition /
// broadcast paths against fake stores -- no live DB. The db-gated half, which is
// the only thing that can vouch for the mutation actually parsing and writing,
// is staged_transition_db_test.go.
//
// The cases divide into the four things this task decided:
//
//   - THE TRANSITION: both halves come off (durable row + in-memory marker), a
//     restart agrees, and it is idempotent so a retry after a partial write is
//     safe;
//   - RULING (a): a re-promote CARRIES the staging forward instead of silently
//     publishing the rows;
//   - RULING (b): retirement and staging are independent -- training a retired
//     concept works and leaves it retired; a REMOVE demote clears the marker
//     because the concept it names stops existing;
//   - CROSS-NODE: a staged promote on engine A makes engine B withhold, and the
//     transition on A makes engine B publish, both purely over the EXISTING
//     authoring.promote broadcast. That pair is the load-bearing test: each half
//     fails against code that resolves in one direction only.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// --- a shared fake "database" --------------------------------------------------

// sharedAuthoringDB is ONE set of authoring rows that satisfies every store
// interface this file needs: the promote path writes through it, the boot /
// propagation walks read through it, and the transition clears through it.
//
// One object rather than three fakes because that is the production shape --
// every node in the mesh talks to one Postgres -- and the cross-node cases below
// are only honest if engine B reads what engine A actually wrote rather than a
// copy taken at the right moment. It is mutex-guarded for the same reason: B's
// subscriber reads it from a goroutine while A is still writing.
type sharedAuthoringDB struct {
	mu         sync.Mutex
	bundles    []AuthoringBundleRow
	constructs []AuthoringConstructRow
	// trainErr, when set, fails the next TrainConstructConceptData -- the
	// partial-write case.
	trainErr error
	// hideBundles simulates the memql#4036 enumeration returning nothing while
	// the rows are there.
	hideBundles bool
}

func (s *sharedAuthoringDB) CreatePromoteBundle(_ context.Context, bundleId, title, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles = append(s.bundles, AuthoringBundleRow{Id: bundleId, Title: title, Summary: summary, Status: BundleActive})
	return nil
}

func (s *sharedAuthoringDB) CreatePromoteConstruct(ctx context.Context, constructId, bundleId, kind, name, targetNamespace, source, status string) error {
	owner := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		owner = ac.UserId
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.constructs = append(s.constructs, AuthoringConstructRow{
		Id: constructId, OwnerUserId: owner, BundleId: bundleId, Kind: kind, Name: name,
		TargetNamespace: targetNamespace, Source: source, Status: status,
	})
	for i := range s.bundles {
		if s.bundles[i].Id == bundleId {
			s.bundles[i].OwnerUserId = owner
		}
	}
	return nil
}

func (s *sharedAuthoringDB) StageConstructConceptData(_ context.Context, constructId string) error {
	return s.setStaged(constructId, true)
}

func (s *sharedAuthoringDB) TrainConstructConceptData(_ context.Context, _, constructId string) error {
	s.mu.Lock()
	if s.trainErr != nil {
		err := s.trainErr
		s.trainErr = nil
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return s.setStaged(constructId, false)
}

func (s *sharedAuthoringDB) setStaged(constructId string, staged bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.constructs {
		if s.constructs[i].Id == constructId {
			s.constructs[i].ConceptDataStaged = staged
			return nil
		}
	}
	return nil
}

func (s *sharedAuthoringDB) LoadPromotedBundles(context.Context) ([]AuthoringBundleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hideBundles {
		return nil, nil
	}
	out := make([]AuthoringBundleRow, len(s.bundles))
	copy(out, s.bundles)
	return out, nil
}

func (s *sharedAuthoringDB) LoadConstructsForBundle(_ context.Context, _, bundleId string) ([]AuthoringConstructRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuthoringConstructRow
	for _, row := range s.constructs {
		if row.BundleId == bundleId {
			out = append(out, row)
		}
	}
	return out, nil
}

// stampedRows counts the rows currently carrying the staged-data flag -- the
// durable half, read the way a restart reads it.
func (s *sharedAuthoringDB) stampedRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, row := range s.constructs {
		if row.ConceptDataStaged {
			n++
		}
	}
	return n
}

// --- fixtures ------------------------------------------------------------------

// promoteTrainedWidget durably promotes the shared trainedWidget concept into eng
// through db, under owner. opts are forwarded, so the same helper builds both a
// staged promote and an ordinary one.
func promoteTrainedWidget(t *testing.T, eng *MemQLEngine, db *sharedAuthoringDB, owner string, opts ...PromoteDurableOption) {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, owner, trainedWidgetSrc, ""); err != nil {
		t.Fatalf("author concept as %s: %v", owner, err)
	}
	c, ok := reg.Lookup(owner, "concept", "trainedWidget")
	if !ok {
		t.Fatalf("session define did not register concept trainedWidget for %s", owner)
	}
	if err := eng.promoteConstructDurableWithStore(context.Background(), db, nil, owner, c, opts...); err != nil {
		t.Fatalf("durable promote as %s: %v", owner, err)
	}
}

// stagedEngine is the common opening: a fresh engine holding trainedWidget with
// its data staged, and the shared rows behind it.
func stagedEngine(t *testing.T) (*MemQLEngine, *sharedAuthoringDB) {
	t.Helper()
	eng := promoteConceptEngine(t)
	db := &sharedAuthoringDB{}
	promoteTrainedWidget(t, eng, db, "owner-1", WithConceptDataStaged())
	if !eng.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("pre-condition: the staged promote did not stage the concept's data")
	}
	if db.stampedRows() != 1 {
		t.Fatalf("pre-condition: expected 1 stamped row, got %d", db.stampedRows())
	}
	return eng, db
}

// countPromoteBroadcasts subscribes to the EXISTING authoring-promote channel and
// returns a reader for how many events landed. The channel is the assertion: this
// task adds no topic, so a transition that propagated over a new one would show
// up here as zero.
func countPromoteBroadcasts(t *testing.T, eng *MemQLEngine) func() int {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	var mu sync.Mutex
	n := 0
	bus.Subscribe(authoringPromotePattern, func(events.Event) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	eng.SetEventBus(bus)
	return func() int {
		// The bus dispatches asynchronously; give it a beat to settle rather
		// than racing it.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// --- the transition ------------------------------------------------------------

// TestTrainConceptData_ClearsBothHalvesAndBroadcasts is the acceptance.
//
// Both halves are asserted because either alone is a different bug wearing the
// same face. The marker without the row means the rows go visible now and hide
// again at the next restart. The row without the marker means the operator is
// told the data is live while this node keeps withholding it -- and its peers do
// not, so the same query answers differently depending on which replica took it.
//
// The broadcast is asserted on the EXISTING authoring.promote channel, which is
// the cross-node design decision made testable: a new topic would have needed a
// new routing rule, and a broadcast with no rule dies silently in the mesh.
func TestTrainConceptData_ClearsBothHalvesAndBroadcasts(t *testing.T) {
	eng, db := stagedEngine(t)
	broadcasts := countPromoteBroadcasts(t, eng)

	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("train concept data: %v", err)
	}
	if !out.Trained || out.RowsCleared != 1 {
		t.Errorf("outcome = %+v, want Trained with 1 row cleared", out)
	}
	if out.ConceptId != trainedWidgetId {
		t.Errorf("outcome names concept %q, want %q -- the declaration name did not resolve to the canonical id", out.ConceptId, trainedWidgetId)
	}
	if out.DurableRecordUnreachable {
		t.Error("the transition reported the durable record unreachable while writing to it")
	}
	if db.stampedRows() != 0 {
		t.Error("the durable stamp survived the transition: the concept comes back STAGED at the next boot, having been reported trained")
	}
	if eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the in-memory marker survived the transition: this node still withholds rows its peers now publish")
	}
	if got := broadcasts(); got != 1 {
		t.Errorf("the transition published %d authoring.promote broadcast(s), want exactly 1 -- no peer learns of a transition that broadcasts nothing", got)
	}

	// A RESTART AGREES. The transition is only real if the fold computes the same
	// answer from the rows it left behind.
	fresh := promoteConceptEngine(t)
	if _, err := fresh.rehydratePromotedNow(context.Background(), db); err != nil {
		t.Fatalf("boot re-hydration after the transition: %v", err)
	}
	staged, err := fresh.applyPersistedConceptDataStaging(context.Background(), db)
	if err != nil {
		t.Fatalf("boot replay after the transition: %v", err)
	}
	if staged != 0 || fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Error("a restart brought the staging back: the durable half of the transition did not land")
	}
}

// TestTrainConceptData_IsIdempotent: a second transition is a successful no-op,
// not an error and not a second broadcast.
//
// It has to be, because the durable half writes N rows and can fail at row K.
// The recovery for that is to run it again, which is only a recovery if running
// it again on the rows already cleared is allowed. A transition that errored on
// "nothing to do" would make its own partial failure unrecoverable.
func TestTrainConceptData_IsIdempotent(t *testing.T) {
	eng, db := stagedEngine(t)
	if _, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget"); err != nil {
		t.Fatalf("first train: %v", err)
	}
	broadcasts := countPromoteBroadcasts(t, eng)

	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", trainedWidgetId)
	if err != nil {
		t.Fatalf("second train must succeed as a no-op, got: %v", err)
	}
	if out.Trained || out.RowsCleared != 0 {
		t.Errorf("second train reported %+v, want an unchanged no-op", out)
	}
	if got := broadcasts(); got != 0 {
		t.Errorf("a no-op transition published %d broadcast(s): every peer re-folds the whole durable record for nothing", got)
	}
}

// TestTrainConceptData_PartialWriteIsRetryable: a store failure part-way through
// reports the error AND how far it got, and re-running finishes the job.
//
// The outcome travelling with the error is the point. Rows cleared before the
// failure stay cleared, so a caller told only "it failed" would not know whether
// the concept is now half-published -- and half-published is the state the retry
// exists to leave.
func TestTrainConceptData_PartialWriteIsRetryable(t *testing.T) {
	eng, db := stagedEngine(t)
	promoteTrainedWidget(t, eng, db, "owner-1", WithConceptDataStaged())
	if db.stampedRows() != 2 {
		t.Fatalf("pre-condition: expected 2 stamped rows, got %d", db.stampedRows())
	}

	db.trainErr = errFakeTrainWrite
	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err == nil {
		t.Fatal("a failing durable write must surface as an error")
	}
	if out.ConceptId != trainedWidgetId {
		t.Errorf("the failure outcome does not name the concept: %+v", out)
	}
	if eng.conceptDataIsStaged(trainedWidgetId) != true {
		t.Error("a FAILED transition cleared the in-memory marker: this node publishes rows the durable record still withholds, and a restart hides them again")
	}

	// The retry finishes it.
	out, err = eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("retry after a partial write: %v", err)
	}
	if !out.Trained || db.stampedRows() != 0 {
		t.Errorf("the retry left %d stamped row(s) (outcome %+v): a partial transition is not recoverable", db.stampedRows(), out)
	}
}

// errFakeTrainWrite is the injected store failure for the partial-write case.
var errFakeTrainWrite = errFakeStore("fake store: the durable write failed")

type errFakeStore string

func (e errFakeStore) Error() string { return string(e) }

// TestTrainConceptData_ClearsEveryStampedRowIncludingAnotherOwners: visibility is
// a property of the CONCEPT, so the transition is too.
//
// A concept accumulates one construct row per promote and those promotes need not
// share an owner. A transition that stopped at the caller's own bundles would
// leave a stamped row behind, the "a live row wins" fold would resolve the
// concept STAGED at the next boot, and the operator would have been told it was
// trained. The demote walk crosses owners for the same reason and writes each row
// under its own bundle's owner; this asserts the transition does too.
func TestTrainConceptData_ClearsEveryStampedRowIncludingAnotherOwners(t *testing.T) {
	eng, db := stagedEngine(t)
	promoteTrainedWidget(t, eng, db, "owner-2", WithConceptDataStaged())
	if db.stampedRows() != 2 {
		t.Fatalf("pre-condition: expected 2 stamped rows across two owners, got %d", db.stampedRows())
	}

	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("train concept data: %v", err)
	}
	if out.RowsCleared != 2 {
		t.Errorf("cleared %d row(s), want 2 -- a row left stamped under another owner brings the staging back at the next boot", out.RowsCleared)
	}

	fresh := promoteConceptEngine(t)
	if _, err := fresh.rehydratePromotedNow(context.Background(), db); err != nil {
		t.Fatalf("boot re-hydration: %v", err)
	}
	staged, err := fresh.applyPersistedConceptDataStaging(context.Background(), db)
	if err != nil {
		t.Fatalf("boot replay: %v", err)
	}
	if staged != 0 {
		t.Error("the concept came back staged after a transition that reported success")
	}
}

// TestTrainConceptData_RefusesAConceptThatIsNotAuthorPromoted: the gate.
//
// A CORE concept carries no promotion marker, so it can never have been staged --
// the marker is only ever set by the durable promote path. Routing the resolution
// through the demote path's gate is what makes that structural rather than
// assumed: a name that is not author-promoted is refused before any row is read,
// and the refusal names the operation the caller actually ran.
func TestTrainConceptData_RefusesAConceptThatIsNotAuthorPromoted(t *testing.T) {
	eng := promoteConceptEngine(t)
	db := &sharedAuthoringDB{}

	for _, name := range []string{"v1:identity:user", "noSuchConceptAnywhere"} {
		_, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", name)
		if err == nil {
			t.Fatalf("training %q was allowed: a concept nobody promoted has no staged data to publish", name)
		}
		if !strings.Contains(err.Error(), "not an author-promoted construct") {
			t.Errorf("the refusal for %q does not name the reason: %v", name, err)
		}
		if !strings.Contains(err.Error(), "train") {
			t.Errorf("the refusal for %q names the wrong operation, which sends the operator to look at a demote they did not run: %v", name, err)
		}
	}

	// The two arguments every entry point checks.
	if _, err := eng.trainConceptDataWithStore(context.Background(), db, "", "trainedWidget"); err == nil {
		t.Error("an unauthenticated transition was allowed")
	}
	if _, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "  "); err == nil {
		t.Error("a nameless transition was allowed")
	}
}

// TestTrainConceptData_ReportsAnUnreachableDurableRecord: memql#4036, made loud.
//
// The durable-promote bundle enumeration matches no rows on a live cluster today
// (see staged_transition.go's header), so the transition's row walk finds
// nothing and reports "0 rows cleared" -- which is the SAME sentence a concept
// whose data was already live produces. The operator running the transition is
// exactly the person who needs those two told apart, so a walk that enumerates
// zero bundles while holding a promotion marker for the concept says so.
func TestTrainConceptData_ReportsAnUnreachableDurableRecord(t *testing.T) {
	eng, db := stagedEngine(t)
	db.hideBundles = true

	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("the transition must still succeed with an unreachable durable record: %v", err)
	}
	if !out.DurableRecordUnreachable {
		t.Error("a transition that enumerated ZERO durable bundles for an author-promoted concept reported nothing: memql#4036 is indistinguishable from `already live`")
	}
	if eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the marker survived: with no durable record to appeal to, the epic's asymmetry says fail toward VISIBLE -- a withholding nothing records is indistinguishable from data loss")
	}
}

// --- RULING (a): a re-promote carries the staging forward -----------------------

// TestConceptDataStaged_RePromoteCarriesTheStagingForward is RULING (a).
//
// Before this task an ordinary re-promote un-staged the concept, and not as a
// decision: the promote appends an unstamped row and "a live row wins" resolves
// it live. So an author who fixed a typo in a staged concept's schema published
// every row they had accumulated -- a disclosure caused by an operation named
// after a schema, silent, and not undoable.
//
// Both halves are asserted, and the durable one is what matters most: carrying
// the marker forward while writing an unstamped row would look correct until the
// next restart, at which point the fold publishes everything.
func TestConceptDataStaged_RePromoteCarriesTheStagingForward(t *testing.T) {
	eng, db := stagedEngine(t)

	// The iteration loop: promote the same concept again, saying NOTHING about
	// staging.
	promoteTrainedWidget(t, eng, db, "owner-1")

	if !eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("an ordinary re-promote un-staged the concept in memory: editing a schema published the rows")
	}
	if db.stampedRows() != 2 {
		t.Errorf("the re-promote wrote an UNSTAMPED row (%d of 2 stamped): the marker survives this process and the next restart publishes everything", db.stampedRows())
	}

	fresh := promoteConceptEngine(t)
	if _, err := fresh.rehydratePromotedNow(context.Background(), db); err != nil {
		t.Fatalf("boot re-hydration: %v", err)
	}
	staged, err := fresh.applyPersistedConceptDataStaging(context.Background(), db)
	if err != nil {
		t.Fatalf("boot replay: %v", err)
	}
	if staged != 1 || !fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the concept came back LIVE after a re-promote that never asked to publish it")
	}

	// AND THE TRANSITION STILL WORKS over the carried-forward rows -- the point of
	// the ruling is that there is exactly one door out, not that there is none.
	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("train after a carry-forward re-promote: %v", err)
	}
	if out.RowsCleared != 2 || db.stampedRows() != 0 {
		t.Errorf("the transition cleared %d of 2 carried-forward rows: %+v", out.RowsCleared, out)
	}
}

// TestConceptDataStaged_TrainThenRePromoteStaysLive: the carry-forward reads the
// CURRENT state rather than remembering the original intent.
//
// This is the case that separates "carry forward" from "sticky forever". After
// the transition the concept is live, so a later re-promote must leave it live --
// otherwise training would be undone by the next schema edit and the transition
// would not be one-way after all, merely one-way until somebody promoted.
func TestConceptDataStaged_TrainThenRePromoteStaysLive(t *testing.T) {
	eng, db := stagedEngine(t)
	if _, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget"); err != nil {
		t.Fatalf("train concept data: %v", err)
	}

	promoteTrainedWidget(t, eng, db, "owner-1")

	if eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("a re-promote after the transition re-staged the concept: training is undone by the next schema edit")
	}
	if db.stampedRows() != 0 {
		t.Errorf("a re-promote after the transition wrote a stamped row (%d): the next restart hides data the cluster has published", db.stampedRows())
	}
}

// --- RULING (b): retirement and staging are independent -------------------------

// TestTrainConceptData_LeavesTheRetirementAlone is RULING (b), forward direction.
//
// The two markers answer different questions -- may rows be WRITTEN, may rows be
// READ -- and the transition speaks only to the second. The concrete case is the
// one that decides it: an author stages a concept, accumulates rows, then demotes
// it (rows exist, so it RETIRES). If training were refused there, the only route
// to data they already hold would be a re-promote -- re-opening the concept to
// WRITES purely in order to READ. That is the retire path's own rule 1 arriving
// from the other side.
func TestTrainConceptData_LeavesTheRetirementAlone(t *testing.T) {
	eng, db := stagedEngine(t)
	eng.markConceptRetired(trainedWidgetId)

	out, err := eng.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget")
	if err != nil {
		t.Fatalf("training a RETIRED concept's staged data must be allowed: %v", err)
	}
	if !out.Trained {
		t.Error("training a retired concept changed nothing")
	}
	if eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the rows stayed staged")
	}
	if !eng.conceptIsRetired(trainedWidgetId) {
		t.Error("publishing the rows also re-opened the concept to WRITES: the two axes are coupled, and one named operation moved the other silently")
	}
}

// TestConceptDemote_LeavesStagedDataAloneWhenItRetires is RULING (b), reverse
// direction: withdrawing a definition must not publish the data it withheld.
//
// A demote of a concept with rows under it RETIRES it, and the rows -- staged or
// not -- stay exactly as visible as they were. Clearing the staging here would
// publish them, which is the opposite of anything "withdraw this definition" can
// be read to authorise.
func TestConceptDemote_LeavesStagedDataAloneWhenItRetires(t *testing.T) {
	eng, _ := stagedEngine(t)
	eng.conceptRowCount = func(context.Context, string) (int64, error) { return 412, nil }

	out, err := eng.demoteConceptFromLiveRegistry(context.Background(), "trainedWidget")
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if out.Outcome != DemoteOutcomeRetired {
		t.Fatalf("a concept with rows was %q, want %q", out.Outcome, DemoteOutcomeRetired)
	}
	if !eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("retiring the concept PUBLISHED its staged rows: withdrawing a definition disclosed the data it was withholding")
	}
}

// TestConceptDemote_ClearsStagedDataWhenItRemoves is the one place the two states
// stop being independent, and why that is not an exception to the ruling.
//
// A demote with ZERO rows REMOVES the concept from the registry: the name is free
// again and nothing resolves the canonical id. A marker left behind names a
// concept that no longer exists, which is precisely the stranded state
// memql#3980's replay declines to create when it skips status-retired rows. And
// there is no data at stake by definition -- reaching here requires a concept
// nobody ever wrote a row to.
func TestConceptDemote_ClearsStagedDataWhenItRemoves(t *testing.T) {
	eng, _ := stagedEngine(t)
	eng.conceptRowCount = func(context.Context, string) (int64, error) { return 0, nil }

	out, err := eng.demoteConceptFromLiveRegistry(context.Background(), "trainedWidget")
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if out.Outcome != DemoteOutcomeRemoved {
		t.Fatalf("a concept with no rows was %q, want %q", out.Outcome, DemoteOutcomeRemoved)
	}
	if eng.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the staged-data marker outlived the concept it names: a read seam consulting it is misled by state whose subject was withdrawn")
	}
}

// --- the resolve ----------------------------------------------------------------

// TestResolveConceptDataStaging_MovesTheMarkerInBOTHDirections.
//
// The boot replay only ever MARKS, which is right for a fresh engine whose marker
// map is empty. A running node needs the other half too: the transition is
// exactly a marker that has to come OFF, and a resolve that only marked would
// leave every peer withholding rows the cluster had already published.
//
// Both directions are asserted from one fixture so a resolve wired in one
// direction fails rather than passing on the case it happens to agree about.
func TestResolveConceptDataStaging_MovesTheMarkerInBOTHDirections(t *testing.T) {
	author, db := stagedEngine(t)
	_ = author

	// A node that knows nothing: the rows say staged, so the resolve marks.
	peer := promoteConceptEngine(t)
	if _, err := peer.rehydratePromotedNow(context.Background(), db); err != nil {
		t.Fatalf("peer boot re-hydration: %v", err)
	}
	marked, cleared, err := peer.resolveConceptDataStagingFromStore(context.Background(), db)
	if err != nil {
		t.Fatalf("resolve (mark direction): %v", err)
	}
	if marked != 1 || cleared != 0 || !peer.conceptDataIsStaged(trainedWidgetId) {
		t.Fatalf("resolve marked=%d cleared=%d: a peer that never handled the staged promote is publishing its rows", marked, cleared)
	}

	// The rows now say live. The SAME resolve must take the marker off.
	if _, err := author.trainConceptDataWithStore(context.Background(), db, "owner-1", "trainedWidget"); err != nil {
		t.Fatalf("train: %v", err)
	}
	marked, cleared, err = peer.resolveConceptDataStagingFromStore(context.Background(), db)
	if err != nil {
		t.Fatalf("resolve (clear direction): %v", err)
	}
	if cleared != 1 || marked != 0 || peer.conceptDataIsStaged(trainedWidgetId) {
		t.Fatalf("resolve marked=%d cleared=%d: the transition never reaches a peer, which keeps withholding rows every other node publishes", marked, cleared)
	}

	// Idempotent: a second resolve over unchanged rows moves nothing, so the
	// per-broadcast log line is evidence rather than noise.
	if marked, cleared, err = peer.resolveConceptDataStagingFromStore(context.Background(), db); err != nil || marked != 0 || cleared != 0 {
		t.Errorf("a re-resolve over unchanged rows moved marked=%d cleared=%d (err=%v)", marked, cleared, err)
	}
}

// --- cross-node -----------------------------------------------------------------

// TestStagedDataPropagation_CrossNode is the load-bearing cross-node test, and it
// asserts BOTH transitions of the tier over the mesh.
//
// Two SEPARATE engines with independent concept registries, bridged by forwarding
// authoring.promote.* from A's bus onto B's -- the node EventBridge plus the
// single broadcast routing rule that already exists in component/node/routing.go,
// which is why this task adds no topic. They share one fake database, as the mesh
// shares one Postgres.
//
//  1. A promotes the concept STAGED. B must register it AND withhold its rows.
//     Fails against code that propagates registration only, which is what the
//     promote broadcast did before this task: B came away resolving the concept
//     and believing its rows were live.
//  2. A TRAINS it. B must publish. Fails against a resolve that only marks, and
//     against a transition that clears its marker without broadcasting.
//
// Neither half alone would catch a resolve wired in one direction.
func TestStagedDataPropagation_CrossNode(t *testing.T) {
	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })

	// Bridge A -> B, stamping an OriginNodeId so it reads as peer-bridged --
	// exactly what the node EventBridge does with a forwarded broadcast.
	busA.Subscribe(authoringPromotePattern, func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	engA := promoteConceptEngine(t)
	engA.SetEventBus(busA)
	engB := promoteConceptEngine(t)
	engB.SetEventBus(busB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := &sharedAuthoringDB{}
	engB.startAuthoringPromoteSubscriberWithStore(ctx, db)

	// 1. THE STAGED PROMOTE CROSSES.
	promoteTrainedWidget(t, engA, db, "owner-1", WithConceptDataStaged())
	if !eventually(3*time.Second, func() bool {
		_, err := engB.concepts.Get(trainedWidgetId)
		return err == nil && engB.conceptDataIsStaged(trainedWidgetId)
	}) {
		registered := false
		if _, err := engB.concepts.Get(trainedWidgetId); err == nil {
			registered = true
		}
		t.Fatalf("CROSS-NODE FAILURE (staged promote): engine B registered=%v staged=%v -- a peer that registers the concept without its visibility publishes every row the author staged",
			registered, engB.conceptDataIsStaged(trainedWidgetId))
	}

	// 2. THE TRANSITION CROSSES, over the SAME channel.
	if _, err := engA.trainConceptDataWithStore(ctx, db, "owner-1", "trainedWidget"); err != nil {
		t.Fatalf("train on engine A: %v", err)
	}
	if !eventually(3*time.Second, func() bool { return !engB.conceptDataIsStaged(trainedWidgetId) }) {
		t.Fatal("CROSS-NODE FAILURE (transition): engine B still withholds the concept's rows " +
			"-- the staged -> live transition never propagated, so the same query answers differently depending on which replica took it")
	}
}
