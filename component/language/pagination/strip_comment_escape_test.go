package pagination

import "testing"

// memql#3120: stripComment used a one-byte lookback and could not tell an
// escaped quote from one following a completed `\\` escape. It now delegates
// to baseparser.StripLineComment; this asserts the delegation is real, because
// a caller that kept its own copy would still compile and still pass every
// test that does not use a backslash pair.
func TestStripCommentHandlesCompletedEscapes(t *testing.T) {
	const line = `filter path=="C:\\" // trailing comment`
	const want = `filter path=="C:\\" `

	if got := stripComment(line); got != want {
		t.Errorf("stripComment(%q)\n got %q\nwant %q", line, got, want)
	}
}

func TestStripCommentKeepsCommentMarkersInsideLiterals(t *testing.T) {
	const line = `filter url=="https://example.com"`

	if got := stripComment(line); got != line {
		t.Errorf("stripComment(%q) = %q, want it unchanged", line, got)
	}
}
