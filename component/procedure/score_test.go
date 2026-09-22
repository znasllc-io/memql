package procedure

import "testing"

// tmplWith builds a one-step template whose single argument object carries the
// given holes, each with the given class.
func tmplWith(classes ...HoleClass) (Template, []Hole) {
	m := map[string]*Node{"fixed": Lit("k")}
	var holes []Hole
	for i, c := range classes {
		id := "s0." + string(rune('a'+i))
		m[string(rune('a'+i))] = HoleNode(id, "string")
		holes = append(holes, Hole{
			Id: id, StepIndex: 0, Path: []string{string(rune('a' + i))},
			Type: "string", Class: c,
		})
	}
	t := Template{Steps: []TemplateStep{{Tool: "exec", Args: Obj(m)}}, Holes: holes}
	return t, holes
}

func occurrences(n int) []Occurrence {
	out := make([]Occurrence, n)
	for i := range out {
		out[i] = Occurrence{Sequence: i, Positions: []int{0}}
	}
	return out
}

func bigCorpus(n int) [][]Action {
	body := Obj(map[string]*Node{
		"fixed": Lit("k"), "a": Lit("1"), "b": Lit("2"), "c": Lit("3"),
		"nested": Obj(map[string]*Node{"x": Lit("9"), "y": Lit("8")}),
	})
	out := make([][]Action, n)
	for i := range out {
		out[i] = []Action{{Tool: "exec", Args: body.Clone(), Seq: 0}}
	}
	return out
}

// TestScore_APatternWhoseHolesAreAllFreeScoresBelowTheFloor is issue #5405's
// second acceptance criterion, and the record's own failure mode: "a pattern
// whose holes are all free parameters scores below the floor by construction".
func TestScore_APatternWhoseHolesAreAllFreeScoresBelowTheFloor(t *testing.T) {
	tmpl, _ := tmplWith(HoleFree, HoleFree, HoleFree, HoleFree, HoleFree)
	u := Score(tmpl, occurrences(2), bigCorpus(2), DefaultParams())
	if u.Accepted {
		t.Fatalf("a template that is all free parameters is not an abstraction, it is the corpus: %+v", u)
	}
	if u.Reason == "" {
		t.Fatal("a refusal must name its reason -- a bare false says nothing about what was considered")
	}
}

// TestScore_AFreeParameterIsPricedAboveADataFlowHole is D13's pricing rule,
// isolated: the same template scored twice, differing only in one hole's
// class.
func TestScore_AFreeParameterIsPricedAboveADataFlowHole(t *testing.T) {
	free, _ := tmplWith(HoleFree)
	flow, _ := tmplWith(HoleDataFlow)
	uFree := Score(free, occurrences(3), bigCorpus(3), DefaultParams())
	uFlow := Score(flow, occurrences(3), bigCorpus(3), DefaultParams())
	if !(uFree.ArgCost > uFlow.ArgCost) {
		t.Fatalf("a free parameter (%d) must cost MORE than a data-flow hole (%d): the caller has to supply it",
			uFree.ArgCost, uFlow.ArgCost)
	}
	if !(uFlow.Net > uFree.Net) {
		t.Fatalf("net: data-flow %d must beat free %d", uFlow.Net, uFree.Net)
	}
}

func TestScore_AConstantCostsNothing(t *testing.T) {
	konst, _ := tmplWith(HoleConstant)
	flow, _ := tmplWith(HoleDataFlow)
	uK := Score(konst, occurrences(3), bigCorpus(3), DefaultParams())
	uF := Score(flow, occurrences(3), bigCorpus(3), DefaultParams())
	if !(uK.ArgCost < uF.ArgCost) {
		t.Fatalf("a constant is kept literal and costs the caller nothing: %d vs %d", uK.ArgCost, uF.ArgCost)
	}
}

// TestScore_OneUseIsRefusedWithTheReasonNamed is the NEGATIVE CONTROL for
// D14's two-uses floor.
func TestScore_OneUseIsRefusedWithTheReasonNamed(t *testing.T) {
	tmpl, _ := tmplWith(HoleDataFlow)
	u := Score(tmpl, occurrences(1), bigCorpus(1), DefaultParams())
	if u.Accepted {
		t.Fatal("one use is not an abstraction")
	}
	if u.Reason != ReasonOneUse {
		t.Fatalf("Reason = %q, want %q", u.Reason, ReasonOneUse)
	}
}

func TestScore_ARepeatedBodyWithOneDataFlowHoleIsAccepted(t *testing.T) {
	tmpl, _ := tmplWith(HoleDataFlow)
	u := Score(tmpl, occurrences(4), bigCorpus(4), DefaultParams())
	if !u.Accepted {
		t.Fatalf("four uses of a substantial body must clear the floor: %+v", u)
	}
}

func TestScore_TooManyFreeParametersIsRefusedByTheCeiling(t *testing.T) {
	p := DefaultParams()
	p.MaxArgs = 2
	tmpl, _ := tmplWith(HoleFree, HoleFree, HoleFree)
	u := Score(tmpl, occurrences(10), bigCorpus(10), p)
	if u.Accepted {
		t.Fatal("three free parameters must not clear a ceiling of two")
	}
	if u.Reason != ReasonTooManyArgs {
		t.Fatalf("Reason = %q, want %q", u.Reason, ReasonTooManyArgs)
	}
}

// TestSelect_RewritesTheCorpusSoTheSecondAbstractionCanReferenceTheFirst is
// D14's headline claim stated as a test: "the library grows one abstraction at
// a time, the corpus is rewritten, and the loop repeats, so abstractions
// reference earlier ones and the hierarchy emerges".
func TestSelect_RewritesTheCorpusSoTheSecondAbstractionCanReferenceTheFirst(t *testing.T) {
	corpus := bigCorpus(4)
	first, _ := tmplWith(HoleDataFlow)
	second, _ := tmplWith(HoleDataFlow)
	second.Steps[0].Tool = "fs_write"

	accepted := Select([]Candidate{
		{Template: first, Occurrences: occurrences(4)},
		{Template: second, Occurrences: occurrences(4)},
	}, corpus, DefaultParams())

	if len(accepted) == 0 {
		t.Fatal("at least the max-utility candidate must be accepted")
	}
	if accepted[0].Order != 0 {
		t.Fatalf("the first acceptance must carry order 0; got %d", accepted[0].Order)
	}
	for i := 1; i < len(accepted); i++ {
		if accepted[i].Order != i {
			t.Fatalf("acceptance %d carries order %d; the order IS the hierarchy", i, accepted[i].Order)
		}
		if accepted[i].Utility.Saved > accepted[i-1].Utility.Saved {
			t.Fatalf("a later acceptance saved MORE (%d) than an earlier one (%d); the corpus was not "+
				"rewritten between rounds", accepted[i].Utility.Saved, accepted[i-1].Utility.Saved)
		}
	}
}

func TestSelect_IsDeterministicUnderATie(t *testing.T) {
	corpus := bigCorpus(4)
	a, _ := tmplWith(HoleDataFlow)
	b, _ := tmplWith(HoleDataFlow)
	cands := []Candidate{
		{Template: a, Occurrences: occurrences(4)},
		{Template: b, Occurrences: occurrences(4)},
	}
	first := Select(cands, corpus, DefaultParams())
	second := Select(cands, bigCorpus(4), DefaultParams())
	if len(first) != len(second) {
		t.Fatalf("%d accepted then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Utility.Net != second[i].Utility.Net {
			t.Fatalf("acceptance %d differs between runs", i)
		}
	}
}

func TestSelect_AcceptsNothingWhenNothingClearsTheFloor(t *testing.T) {
	tmpl, _ := tmplWith(HoleFree, HoleFree, HoleFree, HoleFree, HoleFree)
	got := Select([]Candidate{{Template: tmpl, Occurrences: occurrences(2)}}, bigCorpus(2), DefaultParams())
	if len(got) != 0 {
		t.Fatalf("got %d acceptances, want 0", len(got))
	}
}
