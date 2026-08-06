package sense

import "testing"

// memql#3120: isInsideString inferred escape state from the preceding byte, so
// a literal ending in a completed `\\` escape reported every later position as
// "inside a string" -- in an editor, highlighting silently stops for the rest
// of the line.
func TestIsInsideStringHandlesCompletedEscapes(t *testing.T) {
	const line = `path: "C:\\" more code here`

	// Just past the literal's closing quote (index 11), everything is code.
	for _, pos := range []int{12, 15, len(line)} {
		if isInsideString(line, pos) {
			t.Errorf("isInsideString(%q, %d) = true, want false: the literal ends "+
				"at its closing quote, and `\\\\` is a completed escape", line, pos)
		}
	}

	// Control: positions genuinely inside the literal still report true.
	for _, pos := range []int{8, 9} {
		if !isInsideString(line, pos) {
			t.Errorf("isInsideString(%q, %d) = false, want true", line, pos)
		}
	}
}

// The case the lookback got right, kept so the fix does not trade one failure
// for the other: an escaped quote does not end the literal.
func TestIsInsideStringEscapedQuoteDoesNotClose(t *testing.T) {
	const line = `msg: "he said \" still inside"`

	if !isInsideString(line, 20) {
		t.Errorf("isInsideString(%q, 20) = false, want true: `\\\"` does not close", line)
	}
	if isInsideString(line, len(line)) {
		t.Errorf("isInsideString(%q, %d) = true, want false: the literal closed", line, len(line))
	}
}
