package sense

import "testing"

// TestLineMap_IdentityWhenUnchanged: when the lowering is a no-op the map is
// the identity and every line is exact.
func TestLineMap_IdentityWhenUnchanged(t *testing.T) {
	src := "line one\nline two\nline three\n"
	lm := newLineMap(src, src)
	for i := 1; i <= 4; i++ { // 3 lines + trailing empty
		got := lm.pos(Position{Line: i, Column: 3})
		if got.Line != i {
			t.Errorf("line %d mapped to %d, want %d", i, got.Line, i)
		}
		if got.Column != 3 {
			t.Errorf("line %d column = %d, want 3 (exact)", i, got.Column)
		}
	}
}

// TestLineMap_PreservedLinesMapExact_SynthesizedMapWithin models the shape of
// a real lowering: a construct block whose interior is replaced by more lines
// than the author wrote, with the surrounding text preserved verbatim.
//
//	authored (4 lines)        rewritten (6 lines)
//	1 @enabled                1 @enabled            (preserved)
//	2 query node q {          2 func (Query) q {    (synthesized)
//	3   filter x == 1         3   arg block line    (synthesized)
//	                          4   more synthesized  (synthesized)
//	4 }                       5 }                    (preserved)
//	                          6 (trailing)           (preserved)
func TestLineMap_PreservedExact_SynthesizedWithin(t *testing.T) {
	authored := "@enabled\nquery node q {\n  filter x == 1\n}\n"
	rewritten := "@enabled\nfunc (Query) q {\n  args stuff\n  more synth\n}\n"

	lm := newLineMap(authored, rewritten)

	// Rewritten line 1 (@enabled) is preserved verbatim -> exact line + col.
	if got := lm.pos(Position{Line: 1, Column: 4}); got.Line != 1 || got.Column != 4 {
		t.Errorf("preserved line: got %+v, want {1,4}", got)
	}
	// Rewritten closing `}` (line 5) is preserved -> maps to authored `}` (4).
	if got := lm.pos(Position{Line: 5, Column: 1}); got.Line != 4 {
		t.Errorf("preserved close brace: got line %d, want 4", got.Line)
	}
	// Synthesized interior lines (2..4) must resolve INSIDE the authored
	// construct span [2,4] and drop their (meaningless) column to 1.
	for _, r := range []int{2, 3, 4} {
		got := lm.pos(Position{Line: r, Column: 9})
		if got.Line < 2 || got.Line > 4 {
			t.Errorf("synthesized rewritten line %d mapped to authored %d, want within [2,4]", r, got.Line)
		}
		if got.Column != 1 {
			t.Errorf("synthesized rewritten line %d column = %d, want 1", r, got.Column)
		}
	}
}

// TestLineMap_NeverOutOfRange: no rewritten position ever maps outside the
// authored line range (the "never worse than no position" guarantee).
func TestLineMap_NeverOutOfRange(t *testing.T) {
	authored := "a\nb\nc\n"                     // 4 lines incl trailing empty
	rewritten := "a\nX\nY\nZ\nW\nb\nc\nEXTRA\n" // grows well past authored
	lm := newLineMap(authored, rewritten)
	nAuth := 4
	for r := 1; r <= 9; r++ {
		got := lm.pos(Position{Line: r, Column: 1})
		if got.Line < 1 || got.Line > nAuth {
			t.Errorf("rewritten line %d mapped to %d, out of authored range [1,%d]", r, got.Line, nAuth)
		}
	}
	// Out-of-range input line is clamped, not panicking.
	if got := lm.pos(Position{Line: 999, Column: 1}); got.Line < 1 || got.Line > nAuth {
		t.Errorf("clamp: got %d, want within [1,%d]", got.Line, nAuth)
	}
}
