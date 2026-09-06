package work

import "testing"

// The headline test of spec section J: a goal that fully matches the
// catalog makes ZERO provider calls. Decide is pure, so "zero calls" is
// provable as a property of the decision rather than by counting.
func TestDecide_ExactCatalogHitReachesNoModel(t *testing.T) {
	d := Decide(CompileInput{
		Statement: "Summarise yesterday's support tickets",
		InputKeys: []string{"day"},
		Exact:     []CatalogCandidate{{ConstructId: "c1", Name: "summariseTickets", Signature: "sig"}},
	})
	if d.Route != RouteCatalogExact {
		t.Fatalf("route = %q, want catalogExact", d.Route)
	}
	if d.NeedsModel {
		t.Fatal("an exact catalog hit must reach no model -- this is the spec's headline claim")
	}
	if d.NeedsTriage {
		t.Fatal("an exact hit must not even run the cheap triage classifier: triage is a model call")
	}
	if d.Candidate == nil || d.Candidate.ConstructId != "c1" {
		t.Fatalf("candidate = %+v", d.Candidate)
	}
	if len(d.Gaps) != 0 {
		t.Errorf("an exact match has no gaps, got %v", d.Gaps)
	}
}

// Order is the whole design: exact BEFORE near, near BEFORE triage.
// A near match that outranks an exact one would make the cheap path
// unreachable exactly when it is most valuable.
func TestDecide_ExactBeatsNear(t *testing.T) {
	d := Decide(CompileInput{
		Statement:     "x",
		Exact:         []CatalogCandidate{{ConstructId: "exact"}},
		Near:          []CatalogCandidate{{ConstructId: "near", Similarity: 0.99}},
		NearThreshold: 0.82,
	})
	if d.Candidate.ConstructId != "exact" {
		t.Fatalf("near match outranked an exact one: %+v", d.Candidate)
	}
}

func TestDecide_NearMatchCarriesTheGapListAndStillSkipsTriage(t *testing.T) {
	d := Decide(CompileInput{
		Statement:     "Summarise yesterday's tickets by team",
		InputKeys:     []string{"day", "team"},
		Near:          []CatalogCandidate{{ConstructId: "c2", Similarity: 0.9, MissingArgs: []string{"team"}}},
		NearThreshold: 0.82,
	})
	if d.Route != RouteCatalogNear {
		t.Fatalf("route = %q, want catalogNear", d.Route)
	}
	if d.NeedsTriage {
		t.Fatal("a near match above threshold is already a decision; triage would be a wasted call")
	}
	if len(d.Gaps) != 1 || d.Gaps[0] != "team" {
		t.Fatalf("gaps = %v, want [team]", d.Gaps)
	}
	if !d.NeedsModel {
		t.Error("closing a gap is reasoning, so a near match does reach a model -- just not to author from scratch")
	}
}

func TestDecide_NearBelowThresholdFallsThroughToTriage(t *testing.T) {
	d := Decide(CompileInput{
		Statement:     "something new",
		Near:          []CatalogCandidate{{ConstructId: "c3", Similarity: 0.5}},
		NearThreshold: 0.82,
	})
	if d.Route != RouteUnknown || !d.NeedsTriage {
		t.Fatalf("a weak near match must fall through to the cheap triage classifier; got %+v", d)
	}
}

func TestDecide_TriageRoutes(t *testing.T) {
	for _, tc := range []struct {
		complexity  string
		sectionable bool
		want        Route
	}{
		{"trivial", false, RouteTrivial},
		{"moderate", true, RouteSectionable},
		{"complex", true, RouteSectionable},
		{"moderate", false, RouteAuthor},
		{"complex", false, RouteAuthor},
	} {
		d := Decide(CompileInput{Statement: "x", Complexity: tc.complexity, Sectionable: tc.sectionable, NearThreshold: 0.82})
		if d.Route != tc.want {
			t.Errorf("complexity=%s sectionable=%v -> %q, want %q", tc.complexity, tc.sectionable, d.Route, tc.want)
		}
		if d.NeedsTriage {
			t.Errorf("complexity=%s: triage already ran; asking again is a second call", tc.complexity)
		}
	}
}

// The sectionable generator is deterministic after the shared triage call,
// so it must not be marked as reaching a model again.
func TestDecide_SectionableIsDeterministicAfterTriage(t *testing.T) {
	d := Decide(CompileInput{Statement: "x", Complexity: "moderate", Sectionable: true, NearThreshold: 0.82})
	if d.NeedsModel {
		t.Fatal("the sectionable generator is deterministic after the one shared triage call")
	}
}

func TestGoalSignature_NormalisesStatementAndInputShape(t *testing.T) {
	a := GoalSignature("  Summarise   Yesterday's TICKETS! ", []string{"day", "team"})
	b := GoalSignature("summarise yesterday's tickets", []string{"team", "day"})
	if a != b {
		t.Fatalf("signature must be insensitive to case, punctuation, spacing and arg ORDER:\n a=%s\n b=%s", a, b)
	}
	if GoalSignature("summarise tickets", []string{"day"}) == a {
		t.Fatal("a different input shape is a different signature: the same words with different arguments is a different template")
	}
	if GoalSignature("", nil) == "" {
		t.Fatal("a signature is always computable; an empty goal still hashes")
	}
}

func TestGoalSignature_IsStableAcrossCalls(t *testing.T) {
	for i := 0; i < 8; i++ {
		if GoalSignature("do the thing", []string{"b", "a", "c"}) != GoalSignature("do the thing", []string{"c", "b", "a"}) {
			t.Fatal("signature is not stable; a catalog keyed on it would miss its own entries")
		}
	}
}
