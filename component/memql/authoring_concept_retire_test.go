package memql

// authoring_concept_retire_test.go -- coverage for what it MEANS to withdraw a
// promoted concept (memql#3756): RETIRE while rows exist, REMOVE only when there
// are none.
//
// Internal (package memql) test so it can drive the real demote path against the
// real engine registries and the unexported store seams. The row COUNT is
// injected (MemQLEngine.conceptRowCount) in the cases below whose subject is the
// decision, so the same decision is exercised on every path that can reach it --
// session demote, durable demote, restart replay -- without each of them writing
// rows into a database to be counted. The count's own behaviour, which is the one
// thing a fake cannot vouch for, is pinned separately against a live Postgres in
// authoring_concept_retire_db_test.go.
//
// The load-bearing cases are the last three. That a demote returns the right
// STRING is easy and would pass against an implementation that decides correctly
// and then applies nothing; so the tests here assert the consequences instead --
// a write refused by name, a name that can be claimed again, and a restart that
// comes back retired rather than active.

import (
	"context"
	"strings"
	"testing"
)

// --- fixtures -------------------------------------------------------------

// A SECOND concept, so the ambiguity + name-freeing cases have something to
// contrast with the shared trainedWidget fixture from
// authoring_promote_concept_test.go.
const retiredGadgetSrc = `@version("1.0.0")
@namespace("trainingns")
@description("A second concept taught to a running cluster")
concept trainedGadget {
  ownerUserId  string  @required
  label        string
}`

const trainedGadgetId = "v1:trainingns:trainedGadget"

// countingRows returns a row counter that always answers n. The decision under
// test is "rows or no rows", so a constant is the whole of what the seam needs
// to say.
func countingRows(n int64) conceptRowCounter {
	return func(context.Context, string) (int64, error) { return n, nil }
}

// writeToConcept attempts a row write through the engine's single write
// chokepoint (executeWrite, shared by insert() and update()) and returns the
// error. On an engine with no database a write that gets PAST the retirement gate
// fails later and differently, which is exactly the distinction these tests need.
func writeToConcept(t *testing.T, e *MemQLEngine, conceptId string) error {
	t.Helper()
	_, err := e.executeInsert(context.Background(), MutationNode{
		Concept:    conceptId,
		ID:         conceptId + ":retire-test-row",
		PayloadRaw: `{"label":"x"}`,
	})
	return err
}

// --- the decision ---------------------------------------------------------

// TestDemoteConcept_WithRowsRetires: the first row of the decision table. Rows
// exist, so the concept STAYS in the registry -- that is what keeps those rows
// readable -- and only writes stop.
//
// The registry assertion is the one that matters. An implementation that
// unregisters the concept and merely reports "retired" passes any test that reads
// the outcome string, and is precisely the implementation this issue exists to
// prevent: it makes data unreachable through an operation whose name says it
// affects a definition.
func TestDemoteConcept_WithRowsRetires(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(412)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	outcome, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget")
	if err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRetired {
		t.Errorf("outcome = %q, want %q", outcome.Outcome, DemoteOutcomeRetired)
	}
	if outcome.ConceptId != trainedWidgetId || outcome.RowCount != 412 {
		t.Errorf("outcome = %+v, want the canonical id + the row count that chose it", outcome)
	}
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr != nil {
		t.Fatal("a retired concept was UNREGISTERED: every row ever written under it is now addressed by a name the engine does not know")
	}
	if !e.conceptIsRetired(trainedWidgetId) {
		t.Error("the concept is registered but not marked retired, so it still accepts writes")
	}
	// Still author-promoted: the marker is what a later demote (and the catalog)
	// finds it by. Clearing it would strand the concept as un-demotable.
	if promoted, _ := e.promotedConstruct(ConstructKindConcept, trainedWidgetId); !promoted {
		t.Error("retiring dropped the promotion marker; the concept can no longer be demoted or reported as promoted")
	}
}

// TestDemoteConcept_RetiredRefusesWritesByName: the refusal has to SAY it is a
// retirement.
//
// A retired concept is fully registered, so every downstream check passes and the
// write would otherwise land. Refusing it with a schema-shaped or not-found error
// would send the operator to look at their payload; the message has to name the
// state and the way out of it, because "the definition was withdrawn" is not
// something they can discover from anywhere else.
func TestDemoteConcept_RetiredRefusesWritesByName(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(1)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget"); err != nil {
		t.Fatalf("demote concept: %v", err)
	}

	err := writeToConcept(t, e, trainedWidgetId)
	if err == nil {
		t.Fatal("a write to a RETIRED concept was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "RETIRED") || !strings.Contains(msg, trainedWidgetId) {
		t.Errorf("refusal = %q, want it to name the concept and the retirement", msg)
	}
	if !strings.Contains(msg, "re-promote") && !strings.Contains(msg, "Re-promote") {
		t.Errorf("refusal = %q, want it to name the way out (re-promote)", msg)
	}
}

// TestDemoteConcept_WithZeroRowsRemovesAndFreesTheName: the second row of the
// decision table, and the reason the table has two rows at all. A concept
// promoted by typo, with nothing ever written to it, has to be cleanly
// withdrawable -- otherwise the name is taken forever on that cluster and the
// only repair is a redeploy.
//
// The freeing is asserted by CLAIMING the name again, not by reading the
// registry: "the entry is gone" and "the name can be promoted again" are
// different claims, and only the second is the one that matters. The promote path
// refuses a name a core concept owns by looking it up, so a half-removal (entry
// gone, marker left, or the reverse) is caught here.
func TestDemoteConcept_WithZeroRowsRemovesAndFreesTheName(t *testing.T) {
	e := promoteConceptEngineOnTheDefaultRegistry(t)
	e.conceptRowCount = countingRows(0)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	outcome, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget")
	if err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRemoved || outcome.RowCount != 0 {
		t.Fatalf("outcome = %+v, want removed with a zero row count", outcome)
	}
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr == nil {
		t.Error("the concept is still registered after a zero-row demote")
	}
	if idx := e.schemaIndex(); idx != nil {
		if _, ok := idx.concepts[trainedWidgetId]; ok {
			t.Error("the removed concept is still in the derived schema index")
		}
	}
	if e.conceptIsRetired(trainedWidgetId) {
		t.Error("a REMOVED concept is marked retired; the two outcomes must not both apply")
	}

	// The name is claimable again.
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("re-promoting the removed name was refused: %v -- the name is not free, which is the whole point of removing it", err)
	}
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr != nil {
		t.Errorf("re-promote reported success but the concept is not registered: %v", gerr)
	}
}

// TestRePromoteConcept_UnRetiresAndWritesResume: re-promoting is the un-retire
// path, and it is the ONLY one -- there is no separate API, so a promote can
// never half-lift a retirement.
//
// Asserted through the write path rather than through conceptIsRetired alone: the
// state is only worth anything if the gate reading it agrees. On an engine with
// no database the resumed write fails for its own reasons; what this pins is that
// it is no longer refused AS A RETIREMENT.
func TestRePromoteConcept_UnRetiresAndWritesResume(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(7)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget"); err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	if err := writeToConcept(t, e, trainedWidgetId); err == nil || !strings.Contains(err.Error(), "RETIRED") {
		t.Fatalf("pre-condition: a write to the retired concept should be refused as a retirement, got %v", err)
	}

	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("re-promote: %v", err)
	}
	if e.conceptIsRetired(trainedWidgetId) {
		t.Error("the concept is still retired after a re-promote")
	}
	if err := writeToConcept(t, e, trainedWidgetId); err != nil && strings.Contains(err.Error(), "RETIRED") {
		t.Errorf("writes are still refused as a retirement after a re-promote: %v", err)
	}
}

// --- the safety gate ------------------------------------------------------

// TestDemoteConcept_RefusesACoreConcept: a demote can never unregister a sealed
// core concept.
//
// The gate is the promotion marker, exactly as for every other kind: a core
// concept can never be shadowed by a promote, so it never carries one. This is
// the case where getting it wrong is unrecoverable -- removing a core concept
// takes the whole tree's queries with it -- so it is pinned against a REAL core
// concept id out of the loaded tree rather than a stub.
func TestDemoteConcept_RefusesACoreConcept(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(0) // even with nothing to strand

	const coreConcept = "v1:authoring:construct"
	if _, err := e.concepts.Get(coreConcept); err != nil {
		t.Fatalf("fixture: %q is not in the loaded core tree: %v", coreConcept, err)
	}

	_, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", coreConcept)
	if err == nil {
		t.Fatal("demoting a CORE concept was allowed")
	}
	if !strings.Contains(err.Error(), "not an author-promoted construct") {
		t.Errorf("refusal = %q, want the author-promoted-only safety gate wording (the cross-node demote treats it as its idempotency signal)", err)
	}
	if _, gerr := e.concepts.Get(coreConcept); gerr != nil {
		t.Fatal("a refused demote removed the core concept anyway")
	}
}

// TestDemoteConcept_RefusesWhenTheRowCountIsUnknowable: with no way to count, the
// demote refuses rather than guessing.
//
// The two outcomes are not symmetric, which is what makes fail-closed the only
// defensible choice: guessing "retired" costs a name that stays claimed, guessing
// "removed" costs the readability of every row that may exist. An engine with no
// database has no evidence either way.
func TestDemoteConcept_RefusesWhenTheRowCountIsUnknowable(t *testing.T) {
	e := promoteConceptEngine(t) // no database configured
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	_, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget")
	if err == nil {
		t.Fatal("a concept demote chose an outcome with no way to count the rows")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("refusal = %q, want it to name the missing count", err)
	}
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr != nil {
		t.Error("a refused demote removed the concept anyway")
	}
	if e.conceptIsRetired(trainedWidgetId) {
		t.Error("a refused demote retired the concept anyway")
	}
}

// TestDemoteConcept_ResolvesTheDeclarationNameToItsCanonicalId: the demote
// arrives naming `trainedWidget`; the registry and the promotion marker are keyed
// `v1:trainingns:trainedWidget`.
//
// The two identities are the standing trap on this path (the same one that made a
// promoted concept report as `core` in memql#3749, filed under the wrong key). A
// demote is also the one operation that never compiles the source -- that is what
// lets a construct whose source has rotted still be withdrawn -- so the id cannot
// be re-derived at demote time and has to be resolved from what the engine holds.
func TestDemoteConcept_ResolvesTheDeclarationNameToItsCanonicalId(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(0)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := promoteConceptSource(t, e, retiredGadgetSrc, "trainedGadget"); err != nil {
		t.Fatalf("promote second concept: %v", err)
	}

	// By declaration name.
	outcome, err := e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", "trainedWidget")
	if err != nil {
		t.Fatalf("demote by declaration name: %v", err)
	}
	if outcome.ConceptId != trainedWidgetId {
		t.Errorf("resolved concept id = %q, want %q", outcome.ConceptId, trainedWidgetId)
	}
	// By canonical id -- a caller holding one must not have to take it apart.
	outcome, err = e.demoteAuthoredConstructWithOutcome(context.Background(), "concept", trainedGadgetId)
	if err != nil {
		t.Fatalf("demote by canonical id: %v", err)
	}
	if outcome.ConceptId != trainedGadgetId {
		t.Errorf("resolved concept id = %q, want %q", outcome.ConceptId, trainedGadgetId)
	}
	// The sibling was not touched by the first demote.
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr == nil {
		t.Error("trainedWidget should have been removed by its own demote")
	}
}

// --- durable + restart ----------------------------------------------------

// seedPromotedConcept promotes a concept durably into e and returns a
// fakeDemoteStore reflecting the persisted rows, so a demote can find them. The
// mirror of seedPromotedSpec.
func seedPromotedConcept(t *testing.T, e *MemQLEngine, owner, source, name string) *fakeDemoteStore {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, owner, source); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, ok := reg.Lookup(owner, "concept", name)
	if !ok {
		t.Fatalf("session define did not register concept %q", name)
	}
	persist := &fakePromoteStore{}
	// A nil gate is the ordinary strict-classification promote (memql#3757).
	// This is a seed, so there is no prior version to diff against.
	if err := e.promoteConstructDurableWithStore(context.Background(), persist, nil, owner, c); err != nil {
		t.Fatalf("seed durable promote: %v", err)
	}
	bundleId := persist.bundles[0].Id
	row := persist.constructs[0]
	row.OwnerUserId = owner
	row.Status = string(BundleActive)
	return &fakeDemoteStore{
		bundles:    []AuthoringBundleRow{{Id: bundleId, OwnerUserId: owner, Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{bundleId: {row}},
	}
}

// TestDemoteConceptDurable_StampsTheRowAndLeavesItActive: the persistence shape
// of a retirement, and the one place the two withdrawal states are easiest to
// confuse.
//
// A retired concept's row must NOT go to status "retired". That status is the
// REMOVE outcome's marker and it makes every re-hydration walk SKIP the row --
// which for a concept with rows under it means never registering it again, which
// means the rows it was retired to protect become unreadable at the next restart.
// So the retirement is a separate flag, and the row (and its bundle) stay active.
func TestDemoteConceptDurable_StampsTheRowAndLeavesItActive(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(5)
	store := seedPromotedConcept(t, e, "owner-1", trainedWidgetSrc, "trainedWidget")

	outcome, err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget")
	if err != nil {
		t.Fatalf("durable demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRetired {
		t.Fatalf("outcome = %+v, want retired", outcome)
	}
	if len(store.stampedCs) != 1 {
		t.Errorf("expected 1 construct row stamped conceptRetired, got %d", len(store.stampedCs))
	}
	if len(store.retiredCs) != 0 {
		t.Error("a RETIRED concept's row was flipped to status retired; the boot walk will skip it and its rows become unreadable")
	}
	if len(store.retiredBs) != 0 {
		t.Error("the owning bundle was retired; it must stay active so the boot walk still reaches the concept row")
	}
}

// TestDemoteConceptDurable_ByCanonicalIdStillFindsItsRows: a demote addressed by
// canonical id must still write the withdrawal onto the persisted rows, which are
// named by the DECLARATION name.
//
// The failure this pins is silent and delayed: the in-memory retirement applies,
// the call returns success, no row is touched, and the next restart brings the
// concept back accepting writes. Nothing between the demote and that restart looks
// wrong.
func TestDemoteConceptDurable_ByCanonicalIdStillFindsItsRows(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(2)
	store := seedPromotedConcept(t, e, "owner-1", trainedWidgetSrc, "trainedWidget")

	outcome, err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", trainedWidgetId)
	if err != nil {
		t.Fatalf("durable demote by canonical id: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRetired {
		t.Fatalf("outcome = %+v, want retired", outcome)
	}
	if len(store.stampedCs) != 1 {
		t.Fatal("a demote by canonical id stamped no row: the retirement is in memory only and the next restart undoes it")
	}
}

// TestDemoteConceptDurable_ZeroRowsRetiresTheRowLikeAnyOtherKind: the remove
// outcome takes the ordinary path -- row retired, bundle retired once empty --
// because there is nothing to come back for.
func TestDemoteConceptDurable_ZeroRowsRetiresTheRowLikeAnyOtherKind(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(0)
	store := seedPromotedConcept(t, e, "owner-1", trainedWidgetSrc, "trainedWidget")

	outcome, err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget")
	if err != nil {
		t.Fatalf("durable demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRemoved {
		t.Fatalf("outcome = %+v, want removed", outcome)
	}
	if len(store.retiredCs) != 1 || len(store.retiredBs) != 1 {
		t.Errorf("expected the row and its emptied bundle retired, got %d row(s) + %d bundle(s)", len(store.retiredCs), len(store.retiredBs))
	}
	if len(store.stampedCs) != 0 {
		t.Error("a REMOVED concept's row was stamped conceptRetired; it would come back registered-and-retired at the next boot")
	}
}

// TestDemoteBundleDurable_ReportsPerConstructOutcomes: the bundle-level result
// carries what happened to each member, structured.
//
// A bundle demote of a noun plus its verbs produces two DIFFERENT withdrawals in
// one call -- the verbs removed, the noun retired -- and ok=true is true of both.
// Without the per-construct outcome the caller cannot tell them apart at all, and
// would have to parse a sentence to find out whether the name it just withdrew is
// claimable again.
func TestDemoteBundleDurable_ReportsPerConstructOutcomes(t *testing.T) {
	e := promoteConceptEngineOnTheDefaultRegistry(t)
	e.conceptRowCount = countingRows(9)
	bundle := trainedWidgetSrc + "\n\n" + trainedWidgetMutationSrc

	persist := &fakePromoteStore{}
	// allowBreaking=false is the ordinary promote (memql#3757); this bundle has
	// no prior version to break against.
	if _, err := e.promoteBundleDurableWithStore(context.Background(), persist, "owner-1", bundle, false); err != nil {
		t.Fatalf("promote bundle: %v", err)
	}
	store := &fakeDemoteStore{constructs: map[string][]AuthoringConstructRow{}}

	res, err := e.demoteBundleDurableWithStore(context.Background(), store, "owner-1", bundle)
	if err != nil {
		t.Fatalf("demote bundle: %v", err)
	}
	if !res.OK || len(res.Outcomes) != 2 {
		t.Fatalf("bundle demote = OK %v with %d outcome(s), want ok + one per construct: %+v", res.OK, len(res.Outcomes), res.Outcomes)
	}
	if len(res.Outcomes) != len(res.Demoted) {
		t.Errorf("outcomes (%d) and demoted (%d) disagree; they are built from one slice and must not", len(res.Outcomes), len(res.Demoted))
	}
	byName := map[string]DemoteOutcome{}
	for _, o := range res.Outcomes {
		byName[o.Name] = o
	}
	if got := byName["trainedWidget"]; got.Outcome != DemoteOutcomeRetired || got.RowCount != 9 || got.ConceptId != trainedWidgetId {
		t.Errorf("concept outcome = %+v, want retired with its row count + canonical id", got)
	}
	if got := byName["mutationCreateTrainedWidget"]; got.Outcome != DemoteOutcomeRemoved {
		t.Errorf("mutation outcome = %+v, want removed", got)
	}
	// The intended end state: the verbs are gone, the noun is still readable.
	if got, _ := e.functions.Get("mutationCreateTrainedWidget"); got != nil {
		t.Error("the bundle demote left the mutation callable")
	}
	if _, gerr := e.concepts.Get(trainedWidgetId); gerr != nil {
		t.Error("the bundle demote unregistered the concept its rows are still addressed by")
	}
}

// TestDemoteConcept_SurvivesARestartStillRetired: the durability acceptance.
//
// Demote a promoted concept (rows exist, so it retires), then simulate a restart
// -- a FRESH engine running the boot re-hydration against the persisted rows.
// The concept must come back REGISTERED (its rows are still readable) and still
// RETIRED (writes still refused). Both halves matter and they fail in opposite
// directions: a row skipped like an ordinary retired construct comes back absent,
// and a row re-hydrated without the replay comes back accepting writes.
func TestDemoteConcept_SurvivesARestartStillRetired(t *testing.T) {
	old := promoteConceptEngine(t)
	old.conceptRowCount = countingRows(3)
	store := seedPromotedConcept(t, old, "owner-1", trainedWidgetSrc, "trainedWidget")
	if _, err := old.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget"); err != nil {
		t.Fatalf("durable demote: %v", err)
	}

	// The restart.
	fresh := promoteConceptEngine(t)
	res, err := fresh.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("boot re-hydration: %v", err)
	}
	if res.Rehydrated != 1 {
		t.Fatalf("re-hydrate result = %+v, want the concept re-registered (its rows are still addressed by it)", res)
	}
	if _, gerr := fresh.concepts.Get(trainedWidgetId); gerr != nil {
		t.Fatalf("the retired concept did not come back at all: %v -- every row under it is now unreadable", gerr)
	}
	if fresh.conceptIsRetired(trainedWidgetId) {
		t.Fatal("pre-condition: the walk alone should not have retired it; the replay is what does")
	}

	retired, err := fresh.applyPersistedConceptRetirements(context.Background(), store)
	if err != nil {
		t.Fatalf("replay concept retirements: %v", err)
	}
	if retired != 1 {
		t.Errorf("replayed %d retirement(s), want 1", retired)
	}
	if !fresh.conceptIsRetired(trainedWidgetId) {
		t.Fatal("the concept came back ACTIVE after a restart: a demote that survives as a readable concept still accepting writes is not a demote")
	}
	if err := writeToConcept(t, fresh, trainedWidgetId); err == nil || !strings.Contains(err.Error(), "RETIRED") {
		t.Errorf("writes are not refused after the restart: %v", err)
	}
}

// --- cross-node -----------------------------------------------------------

// TestConceptRetirement_PropagatesAcrossNodes: multi-node is the default, so a
// demote handled on node A has to take effect on node B within seconds -- and
// for a concept "take effect" means the RIGHT one of the two outcomes, not
// merely "gone".
//
// The interesting part is what the broadcast does NOT carry. The
// authoring.demote.<bundleId> event names a bundle and an owner and nothing
// else, and this design deliberately leaves it that way: every node re-derives
// the outcome from the SAME shared Postgres, so the decision cannot diverge
// across the mesh, and a node that joins later reaches the same answer without
// having heard the event at all. Putting the outcome in the payload would have
// created a second source of truth for it.
//
// This drives the receiving side directly (demoteBundleFromRegistryNow, what the
// subscriber calls) with node B's own row counter -- which is the point: B counts
// for itself.
func TestConceptRetirement_PropagatesAcrossNodes(t *testing.T) {
	nodeA := promoteConceptEngine(t)
	nodeA.conceptRowCount = countingRows(6)
	store := seedPromotedConcept(t, nodeA, "owner-1", trainedWidgetSrc, "trainedWidget")

	// Node B is a DIFFERENT engine carrying the same promotion (as if it had
	// received the original promote broadcast).
	nodeB := promoteConceptEngine(t)
	nodeB.conceptRowCount = countingRows(6)
	if err := promoteConceptSource(t, nodeB, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("seed the promotion on node B: %v", err)
	}

	if _, err := nodeA.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget"); err != nil {
		t.Fatalf("demote on node A: %v", err)
	}
	bundleId := store.bundles[0].Id

	removed, err := nodeB.demoteBundleFromRegistryNow(context.Background(), store, "owner-1", bundleId)
	if err != nil {
		t.Fatalf("demote propagation on node B: %v", err)
	}
	if removed != 1 {
		t.Fatalf("node B applied %d withdrawal(s), want 1", removed)
	}
	if _, gerr := nodeB.concepts.Get(trainedWidgetId); gerr != nil {
		t.Fatal("node B UNREGISTERED a concept that still has rows: its reads now fail while node A's succeed")
	}
	if !nodeB.conceptIsRetired(trainedWidgetId) {
		t.Fatal("node B still accepts writes to a concept node A retired")
	}
}

// TestConceptRemoval_PropagatesAcrossNodes: the other outcome across the mesh. A
// concept with no rows is removed on the receiving node too, so the name is free
// everywhere rather than on the one node that handled the demote.
func TestConceptRemoval_PropagatesAcrossNodes(t *testing.T) {
	nodeA := promoteConceptEngine(t)
	nodeA.conceptRowCount = countingRows(0)
	store := seedPromotedConcept(t, nodeA, "owner-1", trainedWidgetSrc, "trainedWidget")

	nodeB := promoteConceptEngine(t)
	nodeB.conceptRowCount = countingRows(0)
	if err := promoteConceptSource(t, nodeB, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("seed the promotion on node B: %v", err)
	}

	if _, err := nodeA.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget"); err != nil {
		t.Fatalf("demote on node A: %v", err)
	}
	if _, err := nodeB.demoteBundleFromRegistryNow(context.Background(), store, "owner-1", store.bundles[0].Id); err != nil {
		t.Fatalf("demote propagation on node B: %v", err)
	}
	if _, gerr := nodeB.concepts.Get(trainedWidgetId); gerr == nil {
		t.Error("node B still has the removed concept registered; the name is claimable on A and taken on B")
	}
}

// TestConceptRetirement_PropagationIsIdempotentOnANodeThatNeverHadIt: a node that
// never handled the original promote has nothing to withdraw, and that is not a
// failure -- the broadcast reaches EVERY node, including ones that booted after
// the promote or never registered it. The receiving path recognises the
// author-promoted-only refusal as "already absent", which is why the concept
// branch keeps that exact wording.
func TestConceptRetirement_PropagationIsIdempotentOnANodeThatNeverHadIt(t *testing.T) {
	nodeA := promoteConceptEngine(t)
	nodeA.conceptRowCount = countingRows(4)
	store := seedPromotedConcept(t, nodeA, "owner-1", trainedWidgetSrc, "trainedWidget")
	if _, err := nodeA.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget"); err != nil {
		t.Fatalf("demote on node A: %v", err)
	}

	stranger := promoteConceptEngine(t) // never promoted anything
	removed, err := stranger.demoteBundleFromRegistryNow(context.Background(), store, "owner-1", store.bundles[0].Id)
	if err != nil {
		t.Fatalf("demote propagation on a node that never had the concept: %v -- it must be a harmless no-op", err)
	}
	if removed != 0 {
		t.Errorf("a node that never had the concept reported %d withdrawal(s)", removed)
	}
}

// TestConceptRetirementReplay_ALiveRowWins: the ordering property that makes the
// replay safe without an un-retire write.
//
// After demote-then-re-promote a concept has TWO active rows: the stamped one the
// demote wrote, and the fresh unstamped one the re-promote appended. Resolving
// them per-row while walking would make the outcome depend on which bundle the
// walk reached first. The fold is over canonical ids and a live row wins, so the
// re-promote survives the restart -- and the asymmetry is deliberate: a stale
// stamp resolving to "retired" would silently refuse writes to a concept its owner
// has already re-promoted.
func TestConceptRetirementReplay_ALiveRowWins(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(2)
	store := seedPromotedConcept(t, e, "owner-1", trainedWidgetSrc, "trainedWidget")
	if _, err := e.demoteConstructDurableWithStore(context.Background(), store, "owner-1", "concept", "trainedWidget"); err != nil {
		t.Fatalf("durable demote: %v", err)
	}

	// The re-promote appends a second bundle + row, unstamped.
	repromoted := seedPromotedConcept(t, e, "owner-1", trainedWidgetSrc, "trainedWidget")
	store.bundles = append(store.bundles, repromoted.bundles...)
	for id, rows := range repromoted.constructs {
		store.constructs[id] = rows
	}

	fresh := promoteConceptEngine(t)
	if _, err := fresh.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("boot re-hydration: %v", err)
	}
	retired, err := fresh.applyPersistedConceptRetirements(context.Background(), store)
	if err != nil {
		t.Fatalf("replay concept retirements: %v", err)
	}
	if retired != 0 || fresh.conceptIsRetired(trainedWidgetId) {
		t.Errorf("the concept came back retired despite a live re-promote row (%d replayed): a stale stamp is refusing writes the owner re-enabled", retired)
	}
}
