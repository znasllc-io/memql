package procedure

import "strings"

import "testing"

func treeString(t *ProcessTree) string {
	if t == nil {
		return "<nil>"
	}
	if t.Op == OpLeaf {
		return t.Symbol
	}
	parts := make([]string, len(t.Children))
	for i, c := range t.Children {
		parts[i] = treeString(c)
	}
	return string(t.Op) + "(" + strings.Join(parts, ",") + ")"
}

func hasOp(t *ProcessTree, op TreeOp) bool {
	if t == nil {
		return false
	}
	if t.Op == op {
		return true
	}
	for _, c := range t.Children {
		if hasOp(c, op) {
			return true
		}
	}
	return false
}

// TestStructure_ARetryLoopYieldsALoopNodeAndNotALengthSevenPattern is issue
// #5404's second acceptance criterion, stated as the record states it. Mine
// alone reports a retried action seven times as a length-seven pattern, which
// lifts into an automation that hard-codes seven attempts. This stage is why
// that does not happen.
func TestStructure_ARetryLoopYieldsALoopNodeAndNotALengthSevenPattern(t *testing.T) {
	seqs := [][]string{
		{"start", "try", "fail", "try", "fail", "try", "done"},
		{"start", "try", "fail", "try", "done"},
		{"start", "try", "done"},
	}
	// The assertion is the exact tree, not merely "a loop is in there
	// somewhere". The flower fallback is ALSO a loop, so the weaker
	// assertion passes with loopCut deleted -- which is what the first draft
	// of this test did.
	got := Structure(seqs, DefaultParams())
	if want := "seq(start,loop(try,fail),done)"; treeString(got) != want {
		t.Fatalf("got  %s\nwant %s", treeString(got), want)
	}
	// Four leaves -- start, try, fail, done -- against the seven the longest
	// trace has. The loop is what collapses the three attempts into one.
	if n := leafCount(got); n != 4 {
		t.Fatalf("the tree has %d leaves, want 4; a loop must not be unrolled into a length-seven path: %s",
			n, treeString(got))
	}
}

// TestStructure_ASequenceWithNoRepetitionHasNoLoopNode is the NEGATIVE
// CONTROL: a miner that returns a loop for everything passes the test above
// and is useless.
func TestStructure_ASequenceWithNoRepetitionHasNoLoopNode(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c"},
		{"a", "b", "c"},
	}
	got := Structure(seqs, DefaultParams())
	if hasOp(got, OpLoop) {
		t.Fatalf("no symbol repeats; there is nothing to loop: %s", treeString(got))
	}
}

func TestStructure_AStrictSequenceIsASequenceNode(t *testing.T) {
	seqs := [][]string{{"a", "b", "c"}, {"a", "b", "c"}}
	got := Structure(seqs, DefaultParams())
	if got.Op != OpSeq {
		t.Fatalf("got %s, want a seq node at the root", treeString(got))
	}
	if n := leafCount(got); n != 3 {
		t.Fatalf("got %d leaves, want 3: %s", n, treeString(got))
	}
}

func TestStructure_AChoiceBecomesAnExclusiveChoiceNode(t *testing.T) {
	seqs := [][]string{
		{"start", "left", "end"},
		{"start", "right", "end"},
	}
	got := Structure(seqs, DefaultParams())
	if !hasOp(got, OpXor) {
		t.Fatalf("two mutually exclusive middles must become an xor node; got %s", treeString(got))
	}
}

func TestStructure_AnUnstructurableLogReturnsAFlowerNotNil(t *testing.T) {
	// Nil would make an un-structurable corpus indistinguishable from an
	// empty one, and the caller would report "nothing recurred" for a corpus
	// that recurred in a shape the cuts do not name.
	seqs := [][]string{
		{"a", "b", "c", "a", "c", "b", "a"},
		{"c", "a", "b", "b", "a", "c"},
		{"b", "c", "a", "c", "a", "b"},
	}
	got := Structure(seqs, DefaultParams())
	if got == nil {
		t.Fatal("Structure must never return nil for a non-empty log")
	}
}

func TestStructure_AnEmptyLogIsNil(t *testing.T) {
	if got := Structure(nil, DefaultParams()); got != nil {
		t.Fatalf("an empty log has no tree; got %s", treeString(got))
	}
}

func TestStructure_IsDeterministic(t *testing.T) {
	seqs := [][]string{
		{"start", "try", "fail", "try", "done"},
		{"start", "try", "done"},
	}
	a, b := Structure(seqs, DefaultParams()), Structure(seqs, DefaultParams())
	if treeString(a) != treeString(b) {
		t.Fatalf("two runs disagree:\n  %s\n  %s", treeString(a), treeString(b))
	}
}

func leafCount(t *ProcessTree) int {
	if t == nil {
		return 0
	}
	if t.Op == OpLeaf {
		return 1
	}
	n := 0
	for _, c := range t.Children {
		n += leafCount(c)
	}
	return n
}
