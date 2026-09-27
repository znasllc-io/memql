package procedure

import (
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

// countMoves counts an alignment's moves of one kind, with their labels.
func countMoves(a Alignment, kind MoveKind) []string {
	var out []string
	for _, m := range a.Moves {
		if m.Kind == kind {
			out = append(out, m.Label)
		}
	}
	return out
}

func TestAFittingTraceAlignsWithZeroCost(t *testing.T) {
	got := Align(seqOf("a", "b", "c"), []string{"a", "b", "c"})
	want := []Move{{MoveSync, "a", 0}, {MoveSync, "b", 1}, {MoveSync, "c", 2}}
	if got.Cost != 0 || !reflect.DeepEqual(got.Moves, want) {
		t.Fatalf("got %+v, want every event a sync move at cost 0", got)
	}
	if _, deviated := got.FirstDeviation(); deviated {
		t.Fatal("a fitting trace has no deviation")
	}
}

// TestAnInsertedEventIsALogMove: an event the model has no step for is
// something the RUN did that the procedure does not -- a log move -- and it
// is named at its own index.
func TestAnInsertedEventIsALogMove(t *testing.T) {
	got := Align(seqOf("a", "b", "c"), []string{"a", "x", "b", "c"})
	if got.Cost != 1 {
		t.Fatalf("Cost = %d, want 1; moves %+v", got.Cost, got.Moves)
	}
	if logs := countMoves(got, MoveLog); !reflect.DeepEqual(logs, []string{"x"}) {
		t.Fatalf("log moves = %v, want [x]", logs)
	}
	dev, ok := got.FirstDeviation()
	if !ok || dev != (Move{MoveLog, "x", 1}) {
		t.Fatalf("FirstDeviation = %+v, %v; want the log move x at index 1", dev, ok)
	}
}

// TestASkippedStepIsAModelMove: a step the procedure has and the run did not
// take is a model move, placed where the run would have taken it.
func TestASkippedStepIsAModelMove(t *testing.T) {
	got := Align(seqOf("a", "b", "c"), []string{"a", "c"})
	if got.Cost != 1 {
		t.Fatalf("Cost = %d, want 1; moves %+v", got.Cost, got.Moves)
	}
	dev, ok := got.FirstDeviation()
	if !ok || dev != (Move{MoveModel, "b", 1}) {
		t.Fatalf("FirstDeviation = %+v, %v; want the model move b before event 1", dev, ok)
	}
}

// TestASubstitutionIsOneLogAndOneModelMove: doing x where the procedure does b
// costs exactly two -- the x the model cannot explain and the b the run never
// took -- and never less, which is what makes the cost comparable across runs.
func TestASubstitutionIsOneLogAndOneModelMove(t *testing.T) {
	got := Align(seqOf("a", "b", "c"), []string{"a", "x", "c"})
	if got.Cost != 2 {
		t.Fatalf("Cost = %d, want 2; moves %+v", got.Cost, got.Moves)
	}
	if logs, models := countMoves(got, MoveLog), countMoves(got, MoveModel); !reflect.DeepEqual(logs, []string{"x"}) || !reflect.DeepEqual(models, []string{"b"}) {
		t.Fatalf("log moves %v and model moves %v, want [x] and [b]", logs, models)
	}
	if syncs := countMoves(got, MoveSync); !reflect.DeepEqual(syncs, []string{"a", "c"}) {
		t.Fatalf("sync moves = %v, want [a c]", syncs)
	}
}

// TestFirstDeviationNamesTheEarliestNonSyncMove: the diagnosis a replay hands
// the app starts where the run first left the procedure, not at the last or
// the worst place it did.
func TestFirstDeviationNamesTheEarliestNonSyncMove(t *testing.T) {
	got := Align(seqOf("a", "b", "c", "d"), []string{"a", "x", "b", "d"})
	if got.Cost != 2 {
		t.Fatalf("Cost = %d, want 2 (log x, model c); moves %+v", got.Cost, got.Moves)
	}
	dev, ok := got.FirstDeviation()
	if !ok || dev != (Move{MoveLog, "x", 1}) {
		t.Fatalf("FirstDeviation = %+v, %v; want the log move x at 1, before the skipped c", dev, ok)
	}
}

// TestAnAlignmentFollowsTheModelsStructure: loops, choices and parallel
// blocks align like the token replay reads them, at zero cost for a run of
// the model.
func TestAnAlignmentFollowsTheModelsStructure(t *testing.T) {
	for _, c := range []struct {
		model *ProcessTree
		trace []string
		cost  int
	}{
		{retryModel(t), []string{"start", "try", "fail", "try", "fail", "try", "done"}, 0},
		{retryModel(t), []string{"start", "try", "fail", "done"}, 1}, // one more try was owed
		{tree(OpXor, tree(OpLoop, leaf("a"), leaf("b")), leaf("c")), []string{"a", "b", "a"}, 0},
		{tree(OpAnd, leaf("a"), leaf("b")), []string{"b", "a"}, 0},
		{tree(OpAnd, leaf("a"), leaf("b")), []string{"a"}, 1},
	} {
		got := Align(c.model, c.trace)
		if got.Cost != c.cost {
			t.Errorf("%s over %v: Cost = %d, want %d; moves %+v", treeString(c.model), c.trace, got.Cost, c.cost, got.Moves)
		}
	}
}

// TestAnAlignmentOverTheStateCapReturnsItsBestPartialWithCostMinusOne pins the
// search's ceiling. An alignment is a shortest path through (marking, trace
// position), and a model with wide parallel blocks has a state space that
// grows as a product: without a cap one diagnosis could cost more than the
// replay it diagnoses. Over the cap the search stops and returns the furthest
// place it reached, marked Cost -1 so no caller mistakes it for an optimal
// alignment -- and every event it never reached is reported as a log move, so
// the partial can never read as a trace that fit.
func TestAnAlignmentOverTheStateCapReturnsItsBestPartialWithCostMinusOne(t *testing.T) {
	model := tree(OpAnd, seqOf("a1", "a2", "a3"), seqOf("b1", "b2", "b3"), seqOf("c1", "c2", "c3"))
	trace := []string{"z", "c1", "q", "b1", "a1", "a2", "c2", "y", "b2", "c3", "b3", "a3"}

	full := Align(model, trace)
	if full.Cost != 3 {
		t.Fatalf("uncapped: Cost = %d, want 3 (log moves z, q, y); moves %+v", full.Cost, full.Moves)
	}

	got := alignCapped(model, trace, 40)
	if got.Cost != -1 {
		t.Fatalf("over the cap the Cost must be -1; got %d", got.Cost)
	}
	var covered []int
	for _, m := range got.Moves {
		if m.Kind != MoveModel {
			covered = append(covered, m.TraceIndex)
		}
	}
	for i := range trace {
		if i >= len(covered) || covered[i] != i {
			t.Fatalf("a partial alignment must still account for every event in order; covered %v", covered)
		}
	}
	if _, deviated := got.FirstDeviation(); !deviated {
		t.Fatal("a partial alignment must never read as a fitting one")
	}
	// ...and it must END in the model's final marking, so the steps the run
	// never reached are named: one complete run of this model takes each of
	// its nine steps exactly once, as a sync or as a model move.
	took := map[string]int{}
	for _, m := range got.Moves {
		if m.Kind != MoveLog {
			took[m.Label]++
		}
	}
	for _, s := range []string{"a1", "a2", "a3", "b1", "b2", "b3", "c1", "c2", "c3"} {
		if took[s] != 1 {
			t.Fatalf("model step %s is taken %d times in the partial, want exactly once: %+v", s, took[s], got.Moves)
		}
	}
}

// TestAnActionIsAssignedTheSymbolSymbolizeClusteredItInto: a shadow
// comparison symbolizes a NEW recording with the construct's stored symbols,
// and the answer must be the one Symbolize itself would have given -- or the
// recording's trace is written in a different alphabet from the model it is
// checked against.
func TestAnActionIsAssignedTheSymbolSymbolizeClusteredItInto(t *testing.T) {
	actions := []Action{
		execStep("go test ./a"),
		execStep("go test ./b"),
		{Tool: "fs_write", Args: Obj(map[string]*Node{"path": Arr(Lit("tmp"), Lit("out"))})},
		execStep("ls -la"),
		execStep("go test ./c"),
	}
	p := DefaultParams()
	symbols := Symbolize(actions, p)
	want := SymbolSequence(actions, symbols)
	for i, a := range actions {
		got, ok := AssignSymbol(symbols, a, p.SymbolBudget)
		if !ok || got != want[i] {
			t.Errorf("action %d: AssignSymbol = %q, %v; Symbolize put it in %q", i, got, ok, want[i])
		}
	}
	// A new instance of a clustered command joins its cluster.
	if got, ok := AssignSymbol(symbols, execStep("go test ./d"), p.SymbolBudget); !ok || got != want[0] {
		t.Errorf("a new `go test` = %q, %v; want %q", got, ok, want[0])
	}
}

// TestAnActionOfAnUnrelatedToolIsAssignedNoSymbol is the negative control: a
// symbol is never shared across tools, and an action too far from every
// template of its own tool is outside the procedure's alphabet.
func TestAnActionOfAnUnrelatedToolIsAssignedNoSymbol(t *testing.T) {
	actions := []Action{execStep("go test ./a"), execStep("go test ./b")}
	symbols := Symbolize(actions, DefaultParams())
	fetch := Action{Tool: "fetch", Args: execStep("go test ./a").Args}
	if got, ok := AssignSymbol(symbols, fetch, 1000); ok {
		t.Fatalf("an unrelated tool must be assigned nothing at any budget; got %q", got)
	}
	if got, ok := AssignSymbol(symbols, execStep("rm -rf / --no-preserve-root now"), 2); ok {
		t.Fatalf("an exec too far from every template must be assigned nothing; got %q", got)
	}
}

// TestSequenceTreeIsTheProcedureInOrder: the procedure's own model is its
// steps in order, so its own trace fits it with fitness 1 and a replay that
// skipped a step does not.
func TestSequenceTreeIsTheProcedureInOrder(t *testing.T) {
	m := SequenceTree([]string{"s0", "s1", "s0"})
	if got := treeString(m); got != "seq(s0,s1,s0)" {
		t.Fatalf("SequenceTree = %s", got)
	}
	if got := TokenReplay(m, []string{"s0", "s1", "s0"}); !got.Fits {
		t.Fatalf("the procedure's own trace must fit its model; got %+v", got)
	}
	if got := TokenReplay(m, []string{"s0", "s0"}); got.Fits {
		t.Fatal("a replay that skipped a step must not fit")
	}
	if got := SequenceTree(nil); got == nil || got.Op != OpSeq || len(got.Children) != 0 {
		t.Fatalf("the empty procedure is the empty sequence; got %s", treeString(got))
	}
}

// TestAnAlignmentOfALongProcedureIsBoundedInMemory (E4): the state cap counted
// STATES, and every state stores a dense marking -- one int per place of the
// net. A procedure of a thousand steps has a thousand places, so the cap's
// hundred thousand states were most of a gigabyte of markings for ONE
// diagnosis of a run that went its own way halfway through. The cap is now
// scaled by the net's size against a byte budget, and over it the search
// still returns a completed best-effort alignment that names the first
// deviation.
func TestAnAlignmentOfALongProcedureIsBoundedInMemory(t *testing.T) {
	const steps = 1000
	symbols := make([]string, steps)
	for i := range symbols {
		symbols[i] = "s" + strconv.Itoa(i)
	}
	model := SequenceTree(symbols)
	trace := append([]string(nil), symbols[:steps/2]...)
	for i := steps / 2; i < steps; i++ {
		trace = append(trace, "x"+strconv.Itoa(i))
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := Align(model, trace)
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("one alignment of a %d-step procedure allocated %d MiB (cost %d)", steps, allocated>>20, got.Cost)
	if allocated > 100<<20 {
		t.Fatalf("one alignment allocated %d MiB; the budget is ~64 MiB of markings", allocated>>20)
	}
	dev, ok := got.FirstDeviation()
	if !ok || dev.TraceIndex != steps/2 {
		t.Fatalf("FirstDeviation = %+v, %v; want the run's departure at event %d", dev, ok, steps/2)
	}
	var covered int
	for _, m := range got.Moves {
		if m.Kind != MoveModel {
			covered++
		}
	}
	if covered != len(trace) {
		t.Fatalf("the alignment accounts for %d of %d events", covered, len(trace))
	}
}
