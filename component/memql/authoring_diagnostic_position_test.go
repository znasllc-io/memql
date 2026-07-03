package memql

// Unit coverage for the authored-position mapping primitives (#2375): the LCS
// line map (adapted from sense/linemap.go) and the bundle-anchor locator. Pure,
// no DB / parser dependency.

import "testing"

// A preserved (byte-identical) line maps EXACTLY and reports exact=true; a
// synthesized rewritten-only line anchors within the surrounding authored hunk
// and reports exact=false.
func TestAuthoredLineMap_PreservedAndSynthesized(t *testing.T) {
	authored := "line A\nline B\nline C"
	// Lowering kept A + C verbatim and replaced B with two synthesized lines.
	rewritten := "line A\nSYNTH 1\nSYNTH 2\nline C"

	lm := newAuthoredLineMap(authored, rewritten)

	// Rewritten line 1 ("line A") -> authored 1, exact.
	if got, exact := lm.authoredLine(1); got != 1 || !exact {
		t.Errorf("rewritten 1 -> (%d, exact=%v), want (1, true)", got, exact)
	}
	// Rewritten line 4 ("line C") -> authored 3, exact.
	if got, exact := lm.authoredLine(4); got != 3 || !exact {
		t.Errorf("rewritten 4 -> (%d, exact=%v), want (3, true)", got, exact)
	}
	// Synthesized lines 2/3 anchor within the authored span (line 2 region),
	// never onto an unrelated line, and are non-exact.
	for _, r := range []int{2, 3} {
		got, exact := lm.authoredLine(r)
		if exact {
			t.Errorf("synthesized rewritten %d reported exact", r)
		}
		if got < 1 || got > 3 {
			t.Errorf("synthesized rewritten %d -> authored %d, out of range", r, got)
		}
	}
}

// An identity rewrite maps every line to itself, exactly.
func TestAuthoredLineMap_Identity(t *testing.T) {
	s := "a\nb\nc\nd"
	lm := newAuthoredLineMap(s, s)
	for r := 1; r <= 4; r++ {
		if got, exact := lm.authoredLine(r); got != r || !exact {
			t.Errorf("identity rewritten %d -> (%d, exact=%v), want (%d, true)", r, got, exact, r)
		}
	}
}

// Out-of-range rewritten lines clamp into the authored range rather than
// panicking or returning a line outside it.
func TestAuthoredLineMap_Clamp(t *testing.T) {
	lm := newAuthoredLineMap("only", "only\nextra\nmore")
	if got, _ := lm.authoredLine(99); got < 1 || got > 1 {
		t.Errorf("clamp: rewritten 99 -> %d, want 1", got)
	}
	if got, _ := lm.authoredLine(0); got != 1 {
		t.Errorf("clamp: rewritten 0 -> %d, want 1", got)
	}
}

// bundleAnchorFor locates the slice body verbatim in the bundle and strips a
// prepended import preamble into a line count.
func TestBundleAnchorFor(t *testing.T) {
	bundle := "line 1\nline 2\n\n@description(\"x\")\nquery Foo bar { }\n"
	usePreamble := "use a.b.{ x }\n\n"
	// No preamble prepended: body is found verbatim; preambleLines 0.
	body := "@description(\"x\")\nquery Foo bar { }"
	if bl, pl := bundleAnchorFor(bundle, body, usePreamble); bl != 4 || pl != 0 {
		t.Errorf("no-preamble anchor = (line %d, preamble %d), want (4, 0)", bl, pl)
	}
	// Preamble prepended: it is stripped (2 lines) and the remaining body is
	// located at bundle line 4.
	withPreamble := usePreamble + body
	if bl, pl := bundleAnchorFor(bundle, withPreamble, usePreamble); bl != 4 || pl != 2 {
		t.Errorf("preamble anchor = (line %d, preamble %d), want (4, 2)", bl, pl)
	}
	// A body not present verbatim (e.g. a lowered terse automation) yields
	// bundleLine 0 -> position omitted downstream.
	if bl, _ := bundleAnchorFor(bundle, "nope not here { }", ""); bl != 0 {
		t.Errorf("missing body anchor line = %d, want 0", bl)
	}
}

// bundlePos runs both hops and refuses (ok=false) a slice line that falls inside
// the prepended import preamble -- it is not attributable to the construct body.
func TestBundlePos_PreambleHitOmitted(t *testing.T) {
	c := SandboxConstruct{BundleLine: 10, BundlePreambleLines: 2}
	// Identity line map over a 5-line source.
	lm := newAuthoredLineMap("a\nb\nc\nd\ne", "a\nb\nc\nd\ne")

	// Slice line 1 sits inside the 2-line preamble -> omitted.
	if _, _, ok := c.bundlePos(lm, 1, 1); ok {
		t.Error("slice line 1 (in preamble) should be omitted")
	}
	// Slice line 3 is body line 1 -> bundle line 10.
	if line, _, ok := c.bundlePos(lm, 3, 4); !ok || line != 10 {
		t.Errorf("slice line 3 -> (line %d, ok %v), want (10, true)", line, ok)
	}
}
