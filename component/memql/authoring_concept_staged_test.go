package memql

// authoring_concept_staged_test.go -- coverage for the STAGED DATA tier (epic
// memql#3974, task memql#3980).
//
// Internal (package memql) test so it can drive the real durable-promote path
// and the unexported store seams with fakes -- no live DB. The tier is INERT at
// this point: nothing filters a read, so there is no "the rows disappeared"
// assertion to make yet. What CAN be pinned now is everything that has to be
// true before such a filter would be safe to add, and each case below is one of
// those:
//
//   - the default is LIVE, on the row and in memory, so shipping this cannot
//     hide anything that exists today;
//   - a staged promote stamps BOTH halves, because the marker without the row is
//     forgotten at the next restart and the row without the marker does nothing
//     until one;
//   - the marker comes back after a restart, from the row alone;
//   - and the fold resolves LIVE whenever any row says live -- asserted in both
//     directions, so an inverted fold fails rather than passing on the case it
//     happens to agree about.

import (
	"context"
	"testing"
)

// --- fixtures -------------------------------------------------------------

// promoteConceptForDataStaging authors + durably promotes trainedWidget into e
// under owner-1, returning the fake store holding the persisted rows. opts are
// forwarded verbatim, which is the whole point: the same call with and without
// WithConceptDataStaged() is what the default-is-live cases contrast.
func promoteConceptForDataStaging(t *testing.T, e *MemQLEngine, opts ...PromoteDurableOption) *fakePromoteStore {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc, ""); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "concept", "trainedWidget")
	if !ok {
		t.Fatal("session define did not register concept trainedWidget")
	}
	persist := &fakePromoteStore{}
	// A nil gate is the ordinary strict-classification promote (memql#3757).
	if err := e.promoteConstructDurableWithStore(context.Background(), persist, nil, "owner-1", c, opts...); err != nil {
		t.Fatalf("durable promote: %v", err)
	}
	return persist
}

// rehydrateStoreFrom converts the rows a durable promote persisted into the
// boot-walk's view of the graph, which is where the replay reads them from.
// Several promotes can be folded into one store, which is how the two-row cases
// below are built.
func rehydrateStoreFrom(persists ...*fakePromoteStore) *fakeRehydrateStore {
	store := &fakeRehydrateStore{constructs: map[string][]AuthoringConstructRow{}}
	for _, p := range persists {
		for _, b := range p.bundles {
			b.OwnerUserId = "owner-1"
			b.Status = BundleActive
			store.bundles = append(store.bundles, b)
		}
		for _, row := range p.constructs {
			row.OwnerUserId = "owner-1"
			store.constructs[row.BundleId] = append(store.constructs[row.BundleId], row)
		}
	}
	return store
}

// --- the default ----------------------------------------------------------

// TestConceptDataStaged_OrdinaryPromoteLeavesDataLive: the default is LIVE, and
// this is the case that decides whether the tier is safe to ship at all.
//
// Every construct row that already exists on every installation carries no
// marker, and every promote that does not ask for staging writes none. A default
// that read as "staged" would make an entire installation's data invisible the
// moment a read filter is wired -- the failure isNotDeleted already made once,
// and the first risk this epic's brief names. So both halves are asserted: the
// persisted row AND the in-memory marker the read seam will consult.
func TestConceptDataStaged_OrdinaryPromoteLeavesDataLive(t *testing.T) {
	e := promoteConceptEngine(t)
	persist := promoteConceptForDataStaging(t, e)

	if len(persist.constructs) != 1 {
		t.Fatalf("expected one persisted construct row, got %d", len(persist.constructs))
	}
	if persist.constructs[0].ConceptDataStaged {
		t.Error("an ordinary promote stamped conceptDataStaged on the row: every existing installation's data would come back staged at the next boot")
	}
	if e.conceptDataIsStaged(trainedWidgetId) {
		t.Error("an ordinary promote marked the concept's data staged in memory: its rows would be withheld from every read that consults the marker")
	}
}

// TestConceptDataStaged_PromoteWithTheOptionStampsBothHalves: a staged promote
// writes the durable flag AND sets the in-memory marker, and neither alone is
// the tier.
//
// The marker without the row is forgotten by the next restart, which turns a
// deliberate withholding into a temporary one nobody is told expired. The row
// without the marker does nothing until a restart, so the rows the author
// staged are visible for the whole life of the process that staged them. The two
// failures look nothing alike and both present as "staging didn't work".
func TestConceptDataStaged_PromoteWithTheOptionStampsBothHalves(t *testing.T) {
	e := promoteConceptEngine(t)
	persist := promoteConceptForDataStaging(t, e, WithConceptDataStaged())

	if len(persist.constructs) != 1 || !persist.constructs[0].ConceptDataStaged {
		t.Errorf("the durable half is missing: row = %+v", persist.constructs)
	}
	if !e.conceptDataIsStaged(trainedWidgetId) {
		t.Error("the in-memory half is missing: the concept's rows are visible on the node that just staged them")
	}
	// The concept itself is REGISTERED, resolvable and unretired -- which is the
	// whole difference between this tier and both of its neighbours. The
	// memql#3928 construct tier would not have registered it shared at all, and a
	// memql#3756 retirement would have closed it to writes.
	if _, err := e.concepts.Get(trainedWidgetId); err != nil {
		t.Fatalf("staging the DATA unregistered the concept: %v -- its rows are now unreadable rather than staged", err)
	}
	if e.conceptIsRetired(trainedWidgetId) {
		t.Error("staging the data also retired the concept to writes: the two markers are aliasing")
	}
}

// TestConceptDataStaged_OptionLeavesNonConceptKindsAlone: a bundle promoted with
// the option stages its concepts and stamps nothing else.
//
// Not an optimisation and not a silent drop: a query, a mutation, a spec and a
// trait own no rows, so there is nothing about them whose visibility could be
// withheld. Stamping one would put a concept-only flag on a row that no fold
// ever reads for that kind, which is the sort of state that survives long enough
// to be believed.
func TestConceptDataStaged_OptionLeavesNonConceptKindsAlone(t *testing.T) {
	e := promoteConceptEngine(t)
	persist := &fakePromoteStore{}
	if _, err := e.promoteBundleDurableWithStore(context.Background(), persist, "owner-1", sessionSpecSrc, "", false, WithConceptDataStaged()); err != nil {
		t.Fatalf("promote spec bundle: %v", err)
	}
	if len(persist.constructs) != 1 {
		t.Fatalf("expected one persisted construct row, got %d", len(persist.constructs))
	}
	if persist.constructs[0].ConceptDataStaged {
		t.Errorf("a %s row was stamped with the concept-only staged-data flag: %+v", persist.constructs[0].Kind, persist.constructs[0])
	}
}

// --- the accessors --------------------------------------------------------

// TestConceptDataStaged_MarkAndClearRoundTrip: the three accessors, and the
// namespace separation that lets a concept be in both concept states at once.
//
// A concept CAN be retired to new writes while the rows it already holds are
// still staged, so the two markers are separate keys rather than two values of
// one. Asserting the independence here is what stops a later "simplification"
// from folding them into a single marker whose second setter silently clears the
// first.
func TestConceptDataStaged_MarkAndClearRoundTrip(t *testing.T) {
	e := promoteConceptEngine(t)

	if e.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("pre-condition: a concept nothing has staged reads as staged")
	}
	e.markConceptDataStaged(trainedWidgetId)
	if !e.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("mark did not take")
	}
	if e.conceptIsRetired(trainedWidgetId) {
		t.Error("marking the DATA staged also read as a write retirement: the two key namespaces alias")
	}

	e.markConceptRetired(trainedWidgetId)
	if !e.conceptDataIsStaged(trainedWidgetId) {
		t.Error("retiring the concept to writes cleared the staged-data marker: the two states must coexist")
	}

	e.clearConceptDataStaging(trainedWidgetId)
	if e.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("clear did not take: there is no way back from staged")
	}
	if !e.conceptIsRetired(trainedWidgetId) {
		t.Error("making the data live also un-retired the concept to writes: the two key namespaces alias")
	}

	// Empty ids are inert in all three directions -- the read seam memql#3983
	// will wire this into runs on every row, including ones whose concept is
	// unresolved.
	if e.conceptDataIsStaged("   ") {
		t.Error("a blank concept id reads as staged")
	}
	e.markConceptDataStaged("")
	if e.conceptDataIsStaged("") {
		t.Error("a blank concept id was markable")
	}
}

// --- the boot replay ------------------------------------------------------

// TestConceptDataStaged_SurvivesARestart: the durability acceptance.
//
// Stage a concept's data, then simulate a restart -- a FRESH engine running the
// boot walk against the persisted rows. The concept must come back REGISTERED
// (its rows are addressed by it and staging is not deletion) and its data must
// come back STAGED. Both halves fail in opposite directions: a row skipped like
// a status-retired one comes back absent, and a row re-hydrated without the
// replay comes back with its staged rows visible.
func TestConceptDataStaged_SurvivesARestart(t *testing.T) {
	old := promoteConceptEngine(t)
	persist := promoteConceptForDataStaging(t, old, WithConceptDataStaged())
	store := rehydrateStoreFrom(persist)

	fresh := promoteConceptEngine(t)
	res, err := fresh.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("boot re-hydration: %v", err)
	}
	if res.Rehydrated != 1 {
		t.Fatalf("re-hydrate result = %+v, want the concept re-registered", res)
	}
	if _, gerr := fresh.concepts.Get(trainedWidgetId); gerr != nil {
		t.Fatalf("the concept did not come back registered: %v", gerr)
	}
	if fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("pre-condition: the walk alone should not have staged the data; the replay is what does")
	}

	staged, err := fresh.applyPersistedConceptDataStaging(context.Background(), store)
	if err != nil {
		t.Fatalf("replay concept staged data: %v", err)
	}
	if staged != 1 {
		t.Errorf("replayed %d staged concept(s), want 1", staged)
	}
	if !fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Fatal("the concept's data came back LIVE after a restart: rows their author staged became visible with nothing to say they had")
	}
}

// TestConceptDataStagedReplay_ALiveRowWins: the fold's direction, asserted in
// BOTH directions so an inversion cannot pass.
//
// A concept accumulates rows -- each promote of the same source appends one --
// so after a staged promote followed by an ordinary one it has two active rows
// that disagree. Resolving them per-row while walking would make the outcome
// depend on which bundle the walk reached first. The fold is over canonical ids
// and A LIVE ROW WINS.
//
// The asymmetry is deliberate and it is the direction whose failure is VISIBLE:
// a stale stamp resolving to LIVE discloses rows the owner can withhold again
// and can see happening, where a stale stamp resolving to STAGED hides the rows
// of a concept its owner has already trained -- intact, addressable, absent from
// every read, and indistinguishable from data loss.
//
// The staged-only case below is the control, and it is not padding: a fold
// inverted to "a staged row wins" passes the two-row case and fails here, while
// a fold that simply dropped the live check passes here and fails there. One
// case alone cannot tell an inversion from a correct implementation.
//
// THE LIVE ROW IS WRITTEN BY A SECOND ENGINE (memql#3986). It used to be a plain
// re-promote on the SAME engine, which no longer produces one: a re-promote now
// CARRIES THE STAGING FORWARD rather than publishing the rows by omission (see
// RULING (a) in staged_transition.go), so that construction would stamp its row
// too. A promote on a node whose marker does not know about the staging still
// writes an unstamped row -- that is the bounded window carry-forward leaves,
// which the marker-based intent cannot close and which this fixture now pins
// rather than assumes away. The ASSERTION is unchanged; only how the two
// disagreeing rows come to exist is.
func TestConceptDataStagedReplay_ALiveRowWins(t *testing.T) {
	e := promoteConceptEngine(t)
	stagedPromote := promoteConceptForDataStaging(t, e, WithConceptDataStaged())

	// CONTROL: the stamped row alone resolves as staged.
	onlyStaged := promoteConceptEngine(t)
	if _, err := onlyStaged.rehydratePromotedNow(context.Background(), rehydrateStoreFrom(stagedPromote)); err != nil {
		t.Fatalf("boot re-hydration (staged only): %v", err)
	}
	staged, err := onlyStaged.applyPersistedConceptDataStaging(context.Background(), rehydrateStoreFrom(stagedPromote))
	if err != nil {
		t.Fatalf("replay (staged only): %v", err)
	}
	if staged != 1 || !onlyStaged.conceptDataIsStaged(trainedWidgetId) {
		t.Fatalf("a lone stamped row resolved as LIVE (%d replayed): the fold is inverted, and every staged concept is visible after a restart", staged)
	}

	// THE CASE: a later ordinary promote, on a node that never saw the staging,
	// appends a second, unstamped row.
	livePromote := promoteConceptForDataStaging(t, promoteConceptEngine(t))
	both := rehydrateStoreFrom(stagedPromote, livePromote)

	fresh := promoteConceptEngine(t)
	if _, err := fresh.rehydratePromotedNow(context.Background(), both); err != nil {
		t.Fatalf("boot re-hydration (both rows): %v", err)
	}
	replayed, err := fresh.applyPersistedConceptDataStaging(context.Background(), both)
	if err != nil {
		t.Fatalf("replay (both rows): %v", err)
	}
	if replayed != 0 || fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Errorf("the concept came back STAGED despite a live row (%d replayed): a stale stamp is hiding rows the owner has already trained, which is indistinguishable from having lost them", replayed)
	}
}

// TestConceptDataStagedReplay_SkipsRetiredRows: a status-retired row never
// re-registered its concept, so it has no visibility to replay.
//
// Folding it in would mark a concept that is not in the registry, leaving a
// marker keyed to a name nothing resolves -- harmless today, and exactly the
// kind of stranded state a later read seam would consult and be misled by.
func TestConceptDataStagedReplay_SkipsRetiredRows(t *testing.T) {
	e := promoteConceptEngine(t)
	persist := promoteConceptForDataStaging(t, e, WithConceptDataStaged())
	store := rehydrateStoreFrom(persist)
	for bid := range store.constructs {
		for i := range store.constructs[bid] {
			store.constructs[bid][i].Status = string(BundleRetired)
		}
	}

	fresh := promoteConceptEngine(t)
	staged, err := fresh.applyPersistedConceptDataStaging(context.Background(), store)
	if err != nil {
		t.Fatalf("replay over a retired row: %v", err)
	}
	if staged != 0 || fresh.conceptDataIsStaged(trainedWidgetId) {
		t.Errorf("a status-retired row was folded in (%d replayed): the concept it names was never registered", staged)
	}
}
