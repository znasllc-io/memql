package memql

import "testing"

// staged_read_predicate_3984_test.go -- epic memql#3974, task memql#3984.
//
// The direct-SQL read sites in integrations/ and component/harness cannot reach
// the engine, so they take the staged-DATA question as an injected
// `func(conceptId string) bool` wired from ConceptDataIsStaged. That indirection
// is where a SECOND source of truth would grow if anyone ever "helpfully"
// reimplemented the lookup on the far side, and a second one is the failure this
// tier cannot tolerate: two derivations agree on the day they are written, and
// the direction of the later disagreement that PUBLISHES rows is invisible from
// outside.
//
// So this pins the one property that keeps them one thing: the exported method
// and the tier's own lookup are the same answer, always, including through a
// transition.

// TestConceptDataIsStagedIsTheSameAnswerAsTheTier walks the full lifecycle and
// asserts the exported accessor never diverges from the unexported one.
//
// Both directions of the transition are exercised deliberately. A delegation
// that had been replaced by a cached copy would pass a single-state check and
// fail here, at the point where the cache went stale -- which is exactly the
// shape the divergence would take in production.
func TestConceptDataIsStagedIsTheSameAnswerAsTheTier(t *testing.T) {
	const conceptId = "v1:trained:widget"
	e := &MemQLEngine{}

	agree := func(when string) {
		t.Helper()
		if got, want := e.ConceptDataIsStaged(conceptId), e.conceptDataIsStaged(conceptId); got != want {
			t.Fatalf("%s: ConceptDataIsStaged=%v but the tier says %v -- the exported accessor has "+
				"stopped being a delegation, which is a second source of truth for row visibility",
				when, got, want)
		}
	}

	agree("before anything is staged")
	if e.ConceptDataIsStaged(conceptId) {
		t.Fatal("a concept nothing has staged reads as staged; the default must be live, or shipping " +
			"this tier hides every row in the installation")
	}

	e.markConceptDataStaged(conceptId)
	agree("after the promote-side mark")
	if !e.ConceptDataIsStaged(conceptId) {
		t.Error("a staged concept reads as live through the exported accessor; every direct-SQL read " +
			"site is wired to this answer")
	}

	e.clearConceptDataStaging(conceptId)
	agree("after the staged -> live transition")
	if e.ConceptDataIsStaged(conceptId) {
		t.Error("a concept whose data was made live still reads as staged")
	}
}

// TestConceptDataIsStagedIsSafeOnAnUnbuiltEngine: a nil receiver and a blank id
// answer "not staged" rather than panicking.
//
// Not defensive tidying. app/'s wiring hands this method out as a closure
// before the engine is fully built, and the plug-in factories that receive it
// may run in a binary that never brings an engine up at all. A panic there
// would take a node down at boot; the honest answer for "no engine, so nothing
// has been promoted" is that nothing is staged.
func TestConceptDataIsStagedIsSafeOnAnUnbuiltEngine(t *testing.T) {
	var nilEngine *MemQLEngine
	if nilEngine.ConceptDataIsStaged("v1:trained:widget") {
		t.Error("a nil engine reported a staged concept")
	}
	e := &MemQLEngine{}
	if e.ConceptDataIsStaged("   ") {
		t.Error("a blank concept id reported as staged")
	}
}
