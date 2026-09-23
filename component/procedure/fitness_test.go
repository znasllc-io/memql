package procedure

import (
	"math"
	"testing"
)

func leaf(s string) *ProcessTree { return &ProcessTree{Op: OpLeaf, Symbol: s} }

func tree(op TreeOp, kids ...*ProcessTree) *ProcessTree {
	return &ProcessTree{Op: op, Children: kids}
}

func seqOf(symbols ...string) *ProcessTree {
	kids := make([]*ProcessTree, len(symbols))
	for i, s := range symbols {
		kids[i] = leaf(s)
	}
	return tree(OpSeq, kids...)
}

// retryModel is the tree Structure mines from a retried action -- the same
// corpus structure_test.go pins to seq(start,loop(try,fail),done).
func retryModel(t *testing.T) *ProcessTree {
	t.Helper()
	m := Structure([][]string{
		{"start", "try", "fail", "try", "fail", "try", "done"},
		{"start", "try", "fail", "try", "done"},
		{"start", "try", "done"},
	}, DefaultParams())
	if got := treeString(m); got != "seq(start,loop(try,fail),done)" {
		t.Fatalf("fixture: Structure gave %s", got)
	}
	return m
}

// TestAFittingSequenceHasFitnessOne also pins the token accounting, because
// the fitness is only as meaningful as its counts: the initial token is
// produced and the final one consumed, so a three-step sequence produces and
// consumes four.
func TestAFittingSequenceHasFitnessOne(t *testing.T) {
	got := TokenReplay(seqOf("a", "b", "c"), []string{"a", "b", "c"})
	if !got.Fits || got.Fitness != 1 || got.FirstDeviation != -1 {
		t.Fatalf("a trace the model allows must fit with fitness 1; got %+v", got)
	}
	if got.Produced != 4 || got.Consumed != 4 || got.Missing != 0 || got.Remaining != 0 {
		t.Fatalf("token counts = %+v, want produced 4, consumed 4, missing 0, remaining 0", got)
	}
}

// TestAnExtraEventLowersFitnessAndNamesItsIndex: the index is the point. A
// replay that learns only "it did not fit" cannot say where it stopped.
func TestAnExtraEventLowersFitnessAndNamesItsIndex(t *testing.T) {
	model := seqOf("a", "b", "c")
	for _, c := range []struct {
		name  string
		trace []string
		at    int
	}{
		{"an event the model never names", []string{"a", "x", "b", "c"}, 1},
		{"a step taken twice", []string{"a", "b", "b", "c"}, 2},
	} {
		got := TokenReplay(model, c.trace)
		if got.Fits || !(got.Fitness < 1) {
			t.Errorf("%s: an extra event must lower the fitness; got %+v", c.name, got)
		}
		if got.FirstDeviation != c.at {
			t.Errorf("%s: FirstDeviation = %d, want %d", c.name, got.FirstDeviation, c.at)
		}
	}
}

// TestAMissingEventLeavesRemainingTokens: skipping b leaves the token b would
// have consumed stranded (remaining) and makes c consume one nobody produced
// (missing) -- and the index names c, the first event that could not follow.
func TestAMissingEventLeavesRemainingTokens(t *testing.T) {
	got := TokenReplay(seqOf("a", "b", "c"), []string{"a", "c"})
	if got.Fits {
		t.Fatal("a trace that skipped a step must not fit")
	}
	if got.Missing != 1 || got.Remaining != 1 || got.FirstDeviation != 1 {
		t.Fatalf("got %+v, want missing 1, remaining 1, first deviation 1", got)
	}
	if want := 2.0 / 3.0; math.Abs(got.Fitness-want) > 1e-9 {
		t.Fatalf("Fitness = %v, want %v (0.5*(1-1/3) + 0.5*(1-1/3))", got.Fitness, want)
	}
}

// TestATraceThatStopsEarlyDoesNotFitButNamesNoEvent: nothing the trace did was
// wrong -- it did not finish. The end of the trace is what cannot reach the
// final marking, so no event index is blamed.
func TestATraceThatStopsEarlyDoesNotFitButNamesNoEvent(t *testing.T) {
	got := TokenReplay(seqOf("a", "b", "c"), []string{"a", "b"})
	if got.Fits || got.FirstDeviation != -1 || got.Remaining != 1 || got.Missing != 1 {
		t.Fatalf("got %+v, want not fitting, no event blamed, one token stranded, the final one missing", got)
	}
}

// TestALoopReplaysAnyNumberOfIterations is the reason Structure makes loops:
// a retry recorded twice must accept a run that needed four attempts, and
// the model must still reject a round that did not start with the body.
func TestALoopReplaysAnyNumberOfIterations(t *testing.T) {
	model := retryModel(t)
	for _, trace := range [][]string{
		{"start", "try", "done"},
		{"start", "try", "fail", "try", "done"},
		{"start", "try", "fail", "try", "fail", "try", "fail", "try", "fail", "try", "done"},
	} {
		if got := TokenReplay(model, trace); !got.Fits {
			t.Errorf("%v: a loop must replay any number of iterations; got %+v", trace, got)
		}
	}
	got := TokenReplay(model, []string{"start", "fail", "done"})
	if got.Fits || got.FirstDeviation != 1 {
		t.Fatalf("a redo with no body before it must deviate at 1; got %+v", got)
	}
	if got := TokenReplay(model, []string{"start", "try", "fail", "done"}); got.Fits {
		t.Fatal("a loop may not END on its redo: the body must run again after it")
	}
}

func TestAChoiceAcceptsEitherBranch(t *testing.T) {
	model := Structure([][]string{
		{"start", "left", "end"},
		{"start", "right", "end"},
	}, DefaultParams())
	for _, trace := range [][]string{{"start", "left", "end"}, {"start", "right", "end"}} {
		if got := TokenReplay(model, trace); !got.Fits {
			t.Errorf("%v: either branch of a choice must fit %s; got %+v", trace, treeString(model), got)
		}
	}
	got := TokenReplay(model, []string{"start", "left", "right", "end"})
	if got.Fits || got.FirstDeviation != 2 {
		t.Fatalf("taking both branches of a choice must deviate at the second; got %+v", got)
	}
}

// TestALoopInsideAChoiceDoesNotLeakIntoTheOtherBranch guards the one subtle
// place in the net construction. A loop's redo must return to the loop's OWN
// entry, not to the place the loop was entered from: inside a choice that
// place is shared with the other branches, and a redo that went back to it
// would let `a b c` fit xor(loop(a,b), c) -- a loop that ended on its redo
// and then took the other branch, which is no trace of the model.
func TestALoopInsideAChoiceDoesNotLeakIntoTheOtherBranch(t *testing.T) {
	model := tree(OpXor, tree(OpLoop, leaf("a"), leaf("b")), leaf("c"))
	for _, trace := range [][]string{{"a"}, {"a", "b", "a"}, {"c"}} {
		if got := TokenReplay(model, trace); !got.Fits {
			t.Errorf("%v must fit %s; got %+v", trace, treeString(model), got)
		}
	}
	if got := TokenReplay(model, []string{"a", "b", "c"}); got.Fits || got.FirstDeviation != 2 {
		t.Fatalf("a b c must deviate at c; got %+v", got)
	}
}

// TestAParallelBlockAcceptsEitherInterleaving: and(a,b) is both in any order,
// and neither alone, and not one twice.
func TestAParallelBlockAcceptsEitherInterleaving(t *testing.T) {
	model := tree(OpSeq, leaf("s"), tree(OpAnd, leaf("a"), seqOf("b1", "b2")), leaf("e"))
	for _, trace := range [][]string{
		{"s", "a", "b1", "b2", "e"},
		{"s", "b1", "a", "b2", "e"},
		{"s", "b1", "b2", "a", "e"},
	} {
		if got := TokenReplay(model, trace); !got.Fits {
			t.Errorf("%v: every interleaving must fit; got %+v", trace, got)
		}
	}
	if got := TokenReplay(model, []string{"s", "a", "e"}); got.Fits || got.FirstDeviation != 2 {
		t.Errorf("a parallel block missing a branch must not fit, and e is where it shows; got %+v", got)
	}
	if got := TokenReplay(model, []string{"s", "a", "a", "b1", "b2", "e"}); got.Fits || got.FirstDeviation != 2 {
		t.Errorf("a branch taken twice must deviate at its second run; got %+v", got)
	}
}

// TestTheFlowerModelAcceptsAnyOrderOfItsSymbols: the flower is Structure's
// honest "no cut applies" answer, and it must mean exactly that -- any
// non-empty sequence of its symbols, and nothing else.
func TestTheFlowerModelAcceptsAnyOrderOfItsSymbols(t *testing.T) {
	model := flower([]string{"a", "b", "c"})
	for _, trace := range [][]string{{"a"}, {"c", "a", "b", "b", "a"}} {
		if got := TokenReplay(model, trace); !got.Fits {
			t.Errorf("%v must fit the flower; got %+v", trace, got)
		}
	}
	if got := TokenReplay(model, []string{"a", "x"}); got.Fits || got.FirstDeviation != 1 {
		t.Errorf("a symbol outside the flower must deviate; got %+v", got)
	}
	if got := TokenReplay(model, nil); got.Fits {
		t.Error("a loop runs its body at least once, so the empty trace does not fit the flower")
	}
}

// TestPrefixFitsWhileTheRunIsIncomplete is the running check a replay makes
// after every step: tokens still waiting are EXPECTED mid-run, and only an
// event the model could not have produced next is a deviation.
func TestPrefixFitsWhileTheRunIsIncomplete(t *testing.T) {
	model := seqOf("s0", "s1", "s2")
	for _, c := range []struct {
		trace []string
		fits  bool
		at    int
	}{
		{nil, true, -1},
		{[]string{"s0"}, true, -1},
		{[]string{"s0", "s1"}, true, -1},
		{[]string{"s0", "s1", "s2"}, true, -1},
		{[]string{"s0", "x"}, false, 1},
		{[]string{"s0", "s2"}, false, 1},
		{[]string{"s0", "s1!"}, false, 1}, // the runner's mark for a step that ran and mismatched
	} {
		fits, at := PrefixFits(model, c.trace)
		if fits != c.fits || at != c.at {
			t.Errorf("PrefixFits(%v) = %v, %d; want %v, %d", c.trace, fits, at, c.fits, c.at)
		}
	}
	if fits, _ := PrefixFits(retryModel(t), []string{"start", "try", "fail"}); !fits {
		t.Error("a run paused inside a loop's redo is still a prefix of the model")
	}
}

// TestNoModelMeansTheEmptyProcedure: a nil tree has no behaviour, so only the
// empty trace fits it -- never "anything goes".
func TestNoModelMeansTheEmptyProcedure(t *testing.T) {
	if got := TokenReplay(nil, nil); !got.Fits {
		t.Fatalf("the empty trace fits the empty model; got %+v", got)
	}
	if got := TokenReplay(nil, []string{"a"}); got.Fits || got.FirstDeviation != 0 {
		t.Fatalf("any event deviates from the empty model; got %+v", got)
	}
}
