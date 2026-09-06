package skills

import (
	"reflect"
	"testing"
)

// selection_test.go -- the rules table of spec section C, as tests.
//
// Every case here is falsifiable against a specific line of selection.go:
// delete the pass it names and the test fails. That is the discipline the
// forward-hop test set in this repo, and the reason this file has no engine,
// no database and no clock.

func match(id string, score float64) Candidate {
	return Candidate{SkillID: id, Score: score, Active: true}
}

func committed(from, to string, kind EdgeType) Edge {
	return Edge{FromSkillID: from, ToSkillID: to, Type: kind, Status: EdgeCommitted}
}

func ids(sel []Selected) []string {
	out := make([]string, 0, len(sel))
	for _, s := range sel {
		out = append(out, s.SkillID)
	}
	return out
}

func TestTheMatchIsRankedHighestFirstAndTiesBreakOnId(t *testing.T) {
	got := ids(Select([]Candidate{
		match("zebra", 0.5),
		match("alpha", 0.9),
		match("beta", 0.5),
	}, nil, Options{}))

	want := []string{"alpha", "beta", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
}

// Determinism is a REQUIREMENT, not a nicety: two replicas select for the
// same step and share no state to agree through, so a tie resolved by map
// order would have them bind different skills to one step.
func TestATieIsResolvedIdenticallyEveryTime(t *testing.T) {
	edges := []Edge{committed("a", "b", EdgeDependsOn), committed("c", "b", EdgeDependsOn)}
	first := ids(Select([]Candidate{match("a", 0.5), match("c", 0.5)}, edges, Options{}))
	for i := 0; i < 50; i++ {
		again := ids(Select([]Candidate{match("c", 0.5), match("a", 0.5)}, edges, Options{}))
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("selection is not deterministic: %v then %v", first, again)
		}
	}
}

func TestADependencyIsPulledInAndMarkedAsOne(t *testing.T) {
	sel := Select(
		[]Candidate{match("scripting", 0.9)},
		[]Edge{committed("scripting", "python-runtime", EdgeDependsOn)},
		Options{},
	)

	if got := ids(sel); !reflect.DeepEqual(got, []string{"scripting", "python-runtime"}) {
		t.Fatalf("selection = %v", got)
	}
	if sel[1].Reason != ReasonDependency {
		t.Fatalf("reason = %q, want %q", sel[1].Reason, ReasonDependency)
	}
	if sel[1].Via != "scripting" {
		t.Fatalf("via = %q, want the skill that pulled it in", sel[1].Via)
	}
}

func TestADependencyChainIsFollowedToTheDepthBoundAndNoFurther(t *testing.T) {
	edges := []Edge{
		committed("a", "b", EdgeDependsOn),
		committed("b", "c", EdgeDependsOn),
		committed("c", "d", EdgeDependsOn),
	}

	if got := ids(Select([]Candidate{match("a", 1)}, edges, Options{DependencyDepth: 2})); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("depth 2 = %v, want a, b, c", got)
	}
	if got := ids(Select([]Candidate{match("a", 1)}, edges, Options{DependencyDepth: 3})); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("depth 3 = %v, want the whole chain", got)
	}
}

// A cycle in dependsOn is a authoring mistake, not an impossibility, and an
// unbounded walk over one does not return. The depth bound is what makes
// this terminate; the visited set is what stops it re-adding.
func TestADependencyCycleTerminates(t *testing.T) {
	edges := []Edge{
		committed("a", "b", EdgeDependsOn),
		committed("b", "a", EdgeDependsOn),
	}
	done := make(chan []string, 1)
	go func() { done <- ids(Select([]Candidate{match("a", 1)}, edges, Options{DependencyDepth: 50})) }()
	select {
	case got := <-done:
		if !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("cycle = %v, want each skill once", got)
		}
	case <-timeoutAfterASecond():
		t.Fatal("Select did not return on a dependsOn cycle")
	}
}

func TestTheLowerScoringSideOfAConflictIsDropped(t *testing.T) {
	sel := Select(
		[]Candidate{match("careful", 0.9), match("reckless", 0.8)},
		[]Edge{committed("careful", "reckless", EdgeConflictsWith)},
		Options{},
	)

	if got := ids(sel); !reflect.DeepEqual(got, []string{"careful"}) {
		t.Fatalf("selection = %v, want the winner alone", got)
	}
	want := []Displacement{{SkillID: "reckless", Type: EdgeConflictsWith}}
	if !reflect.DeepEqual(sel[0].Displaced, want) {
		t.Fatalf("displaced = %v, want %v", sel[0].Displaced, want)
	}
}

// The edge is symmetric in MEANING, so it must be symmetric in effect: which
// row happens to be the `from` is an authoring accident.
func TestAConflictDropsTheSameSideWhicheverWayTheEdgeIsWritten(t *testing.T) {
	forward := ids(Select(
		[]Candidate{match("careful", 0.9), match("reckless", 0.8)},
		[]Edge{committed("careful", "reckless", EdgeConflictsWith)},
		Options{},
	))
	backward := ids(Select(
		[]Candidate{match("careful", 0.9), match("reckless", 0.8)},
		[]Edge{committed("reckless", "careful", EdgeConflictsWith)},
		Options{},
	))
	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("edge direction changed the answer: %v vs %v", forward, backward)
	}
}

func TestDuplicatesCollapseToTheHigherScoringRow(t *testing.T) {
	sel := Select(
		[]Candidate{match("web-research", 0.7), match("web-search", 0.95)},
		[]Edge{committed("web-research", "web-search", EdgeDuplicates)},
		Options{},
	)
	if got := ids(sel); !reflect.DeepEqual(got, []string{"web-search"}) {
		t.Fatalf("selection = %v, want the higher-scoring row alone", got)
	}
	if sel[0].Displaced[0].Type != EdgeDuplicates {
		t.Fatalf("displacement type = %q", sel[0].Displaced[0].Type)
	}
}

// The specialist wins even when the general scored HIGHER. That is the one
// place score does not decide, and it is deliberate: the general is by
// definition the less precise answer to the same request, so a higher
// similarity to a vaguer description is not evidence it is the better skill.
func TestASpecialistDisplacesItsGeneralEvenOnALowerScore(t *testing.T) {
	sel := Select(
		[]Candidate{match("research", 0.95), match("legal-research", 0.6)},
		[]Edge{committed("legal-research", "research", EdgeSpecializes)},
		Options{},
	)
	if got := ids(sel); !reflect.DeepEqual(got, []string{"legal-research"}) {
		t.Fatalf("selection = %v, want the specialist alone", got)
	}
}

// A specialist that did not match is NOT invented. Pulling one in because it
// specializes something that matched would answer a request nobody made.
func TestASpecialistThatDidNotMatchIsNotInvented(t *testing.T) {
	got := ids(Select(
		[]Candidate{match("research", 0.95)},
		[]Edge{committed("legal-research", "research", EdgeSpecializes)},
		Options{},
	))
	if !reflect.DeepEqual(got, []string{"research"}) {
		t.Fatalf("selection = %v, want the general alone", got)
	}
}

// The propose-then-commit protocol's whole point: one compile's hypothesis
// must not reshape what every later selection sees.
func TestAProposedEdgeDoesNotSteerSelection(t *testing.T) {
	proposed := Edge{FromSkillID: "careful", ToSkillID: "reckless", Type: EdgeConflictsWith, Status: EdgeProposed}
	got := ids(Select([]Candidate{match("careful", 0.9), match("reckless", 0.8)}, []Edge{proposed}, Options{}))
	if !reflect.DeepEqual(got, []string{"careful", "reckless"}) {
		t.Fatalf("a proposed edge steered selection: %v", got)
	}
}

// A dependency the answer already refuses is the case that would otherwise
// re-admit exactly what the conflict edge exists to keep out.
func TestADependencyAConflictRefusesIsNotPulledIn(t *testing.T) {
	got := ids(Select(
		[]Candidate{match("careful", 0.9), match("scripting", 0.8)},
		[]Edge{
			committed("scripting", "reckless", EdgeDependsOn),
			committed("careful", "reckless", EdgeConflictsWith),
		},
		Options{},
	))
	if !reflect.DeepEqual(got, []string{"careful", "scripting"}) {
		t.Fatalf("selection = %v, want the conflicting dependency left out", got)
	}
}

// A dropped skill must not drag its dependencies in behind it -- which is
// what running the dependency pass before the displacement pass would do.
func TestTheDependenciesOfADisplacedSkillDoNotArrive(t *testing.T) {
	got := ids(Select(
		[]Candidate{match("winner", 0.9), match("loser", 0.8)},
		[]Edge{
			committed("winner", "loser", EdgeDuplicates),
			committed("loser", "loser-dep", EdgeDependsOn),
		},
		Options{},
	))
	if !reflect.DeepEqual(got, []string{"winner"}) {
		t.Fatalf("selection = %v, want no trace of the displaced skill", got)
	}
}

func TestAnInactiveSkillIsNeverADirectMatchButIsStillReachableAsADependency(t *testing.T) {
	retired := Candidate{SkillID: "retired", Score: 0.99, Active: false}

	if got := ids(Select([]Candidate{retired, match("live", 0.1)}, nil, Options{})); !reflect.DeepEqual(got, []string{"live"}) {
		t.Fatalf("an inactive skill was offered as a match: %v", got)
	}

	got := ids(Select([]Candidate{retired, match("live", 0.1)}, []Edge{committed("live", "retired", EdgeDependsOn)}, Options{}))
	if !reflect.DeepEqual(got, []string{"live", "retired"}) {
		t.Fatalf("selection = %v -- a dependency something still declares must stay reachable", got)
	}
}

func TestAScoreBelowTheFloorIsNotAMatch(t *testing.T) {
	got := ids(Select([]Candidate{match("strong", 0.8), match("weak", 0.2)}, nil, Options{MinScore: 0.5}))
	if !reflect.DeepEqual(got, []string{"strong"}) {
		t.Fatalf("selection = %v", got)
	}
}

func TestTheLimitCapsTheAnswer(t *testing.T) {
	got := Select([]Candidate{match("a", 0.9), match("b", 0.8), match("c", 0.7)}, nil, Options{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestAnUnrecognisedEdgeTypeIsIgnoredRatherThanGuessedAt(t *testing.T) {
	future := Edge{FromSkillID: "a", ToSkillID: "b", Type: EdgeType("supersedes"), Status: EdgeCommitted}
	got := ids(Select([]Candidate{match("a", 0.9), match("b", 0.8)}, []Edge{future}, Options{}))
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("selection = %v -- a type this engine does not know must cost a refinement, never a drop", got)
	}
}

// ---------------------------------------------------------------------------
// Propose and commit
// ---------------------------------------------------------------------------

func TestARunProposesDependsOnFromItsOwnOrdering(t *testing.T) {
	got := ProposeFromRun("run-1", []StepBinding{
		{Key: "fetch", SkillIDs: []string{"http"}},
		{Key: "parse", SkillIDs: []string{"csv"}, DependsOn: []string{"fetch"}},
	})

	if len(got) != 1 {
		t.Fatalf("proposals = %+v, want exactly one", got)
	}
	e := got[0]
	if e.FromSkillID != "csv" || e.ToSkillID != "http" || e.Type != EdgeDependsOn {
		t.Fatalf("proposal = %+v", e)
	}
	if e.Status != EdgeProposed {
		t.Fatalf("status = %q, want %q", e.Status, EdgeProposed)
	}
	if len(e.Evidence) != 1 || e.Evidence[0].RunID != "run-1" || e.Evidence[0].StepKey != "parse" {
		t.Fatalf("evidence = %+v -- an edge nobody can trace back is one nobody can argue with", e.Evidence)
	}
}

func TestTwoSkillsOwningOneConstructProposeDuplicates(t *testing.T) {
	got := ProposeFromRun("run-1", []StepBinding{
		{Key: "s", SkillIDs: []string{"beta", "alpha"}, ConstructRefs: []string{"sendEmail"}},
	})
	if len(got) != 1 || got[0].Type != EdgeDuplicates {
		t.Fatalf("proposals = %+v", got)
	}
	// Ordered by id so the symmetric fact is stored once rather than twice.
	if got[0].FromSkillID != "alpha" || got[0].ToSkillID != "beta" {
		t.Fatalf("pair = %s -> %s, want the id order", got[0].FromSkillID, got[0].ToSkillID)
	}
}

// The two types a run cannot honestly witness. Co-occurrence is not conflict,
// and nothing in a trace says one skill MEANS a sharper form of another.
func TestARunProposesNeitherConflictNorSpecialization(t *testing.T) {
	for _, e := range ProposeFromRun("run-1", []StepBinding{
		{Key: "a", SkillIDs: []string{"x", "y"}},
		{Key: "b", SkillIDs: []string{"z"}, DependsOn: []string{"a"}, ConstructRefs: []string{"c"}},
	}) {
		if e.Type == EdgeConflictsWith || e.Type == EdgeSpecializes {
			t.Fatalf("a run proposed %q, which no trace can witness", e.Type)
		}
	}
}

func TestASucceededRunCommitsItsProposalsAndAFailedOneLeavesThemProposed(t *testing.T) {
	proposals := ProposeFromRun("run-1", []StepBinding{
		{Key: "fetch", SkillIDs: []string{"http"}},
		{Key: "parse", SkillIDs: []string{"csv"}, DependsOn: []string{"fetch"}},
	})

	committedEdges := CommitProposals(proposals, true)
	if len(committedEdges) != 1 || committedEdges[0].Status != EdgeCommitted {
		t.Fatalf("commit on success = %+v", committedEdges)
	}
	if got := CommitProposals(proposals, false); len(got) != 0 {
		t.Fatalf("commit on failure = %+v, want nothing promoted", got)
	}
	// The proposals themselves are untouched: a proposal that keeps being
	// made is the signal a person needs, and erasing it erases the evidence.
	if proposals[0].Status != EdgeProposed {
		t.Fatalf("the proposal was mutated to %q", proposals[0].Status)
	}
}

func TestProposeIsDeterministic(t *testing.T) {
	steps := []StepBinding{
		{Key: "a", SkillIDs: []string{"q", "p"}, ConstructRefs: []string{"c1"}},
		{Key: "b", SkillIDs: []string{"z", "y"}, DependsOn: []string{"a"}},
	}
	first := ProposeFromRun("run-1", steps)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, ProposeFromRun("run-1", steps)) {
			t.Fatal("ProposeFromRun is not deterministic")
		}
	}
}
