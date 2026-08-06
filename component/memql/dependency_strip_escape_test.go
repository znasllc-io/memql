package memql

import "testing"

// memql#3120: stripDepLineComment was a byte-identical copy of two other
// scanners carrying the same lookback defect. It delegates now; this asserts
// the delegation rather than trusting it.
func TestStripDepLineCommentHandlesCompletedEscapes(t *testing.T) {
	const line = `use common.shapes.{ x } // note "C:\\"`
	const want = `use common.shapes.{ x } `

	if got := stripDepLineComment(line); got != want {
		t.Errorf("stripDepLineComment(%q)\n got %q\nwant %q", line, got, want)
	}
}

func TestStripDepLineCommentKeepsMarkersInsideLiterals(t *testing.T) {
	const line = `x == "a // b"`

	if got := stripDepLineComment(line); got != line {
		t.Errorf("stripDepLineComment(%q) = %q, want it unchanged", line, got)
	}
}
