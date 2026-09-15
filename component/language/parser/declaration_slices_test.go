package parser

import (
	"regexp"
	"strings"
	"testing"
)

// declaration_slices_test.go -- package-level coverage for the ONE shared
// declaration slicer (memql#2896).
//
// These live here rather than only at the call sites because this file is now
// the single point of failure for five load paths (component/actions loader +
// capability catalog, component/memql authoring-bundle / capability-loader /
// keyword slices). Covering it only indirectly, from two consumers, leaves the
// other three asserting nothing about the code they depend on -- and two of the
// five are fed sources this repo does not own: RegisterTree'd product bundles
// arriving via MEMQL_DSL_PATH, and user-authored sandbox text through
// SplitBundleSource. For those, a silent drop is not a test failure anywhere.

var testShapeHeaderRE = regexp.MustCompile(`(?m)^[ \t]*shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

func sliceNames(slices []DeclarationSlice) []string {
	out := make([]string, 0, len(slices))
	for _, s := range slices {
		out = append(out, s.Name)
	}
	return out
}

// TestMatchingCloseBraceHandlesBacktickLiterals is the regression for the hole
// that an inlined second brace walk reintroduced: it tracked double-quoted
// literals only, so a `{` inside a BACKTICK literal ran the depth count to EOF
// and returned -1. On the slice path a -1 is not a loud failure -- the
// declaration is silently skipped, and so is everything after it.
func TestMatchingCloseBraceHandlesBacktickLiterals(t *testing.T) {
	// A backtick literal containing an unbalanced `{`, inside a body.
	src := "shape b { y @d(" + "`" + "{" + "`" + ") }\n"
	scan := BlankComments(src)

	openIdx := strings.IndexByte(src, '{')
	got := MatchingCloseBrace(scan, openIdx)

	want := strings.LastIndexByte(src, '}')
	if got != want {
		t.Fatalf("MatchingCloseBrace = %d, want %d.\n"+
			"A `{` inside a backtick literal was counted as a real brace, so the walk "+
			"never balanced. src=%q", got, want, src)
	}
}

// TestBraceDepthBeforeHandlesBacktickLiterals pins the same rule on the
// top-level guard. If the two disagree about what counts as a brace, a
// declaration can be judged nested by one and top-level by the other over the
// same source.
func TestBraceDepthBeforeHandlesBacktickLiterals(t *testing.T) {
	src := "shape a { x @d(" + "`" + "{" + "`" + ") }\nshape b { y }\n"
	scan := BlankComments(src)

	pos := strings.Index(src, "shape b")
	if got := BraceDepthBefore(scan, pos); got != 0 {
		t.Fatalf("BraceDepthBefore(before `shape b`) = %d, want 0.\n"+
			"The `{` inside the backtick literal leaked into the depth count, so a "+
			"top-level declaration looks nested and gets skipped.", got)
	}
}

// TestBacktickLiteralDoesNotSwallowFollowingDeclarations is the end-to-end
// consequence of the two above, at the level the call sites actually use.
func TestBacktickLiteralDoesNotSwallowFollowingDeclarations(t *testing.T) {
	src := "shape a { x @d(" + "`" + "{" + "`" + ") }\nshape b { y }\n"

	got := sliceNames(ExtractDeclarationSlices(src, testShapeHeaderRE))
	want := []string{"a", "b"}

	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("slices = %v, want %v -- a brace inside a backtick literal dropped a "+
			"declaration and everything below it", got, want)
	}
}

// TestExtractDeclarationSlicesSkipsBlockCommented is the headline rule: a
// declaration that exists only inside a block comment does not exist.
func TestExtractDeclarationSlicesSkipsBlockCommented(t *testing.T) {
	const src = `
/*
shape parked {
  a
}
*/
shape live {
  b
}
`
	got := sliceNames(ExtractDeclarationSlices(src, testShapeHeaderRE))
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("slices = %v, want [live] -- a block-commented declaration was extracted", got)
	}
}

// TestBlockCommentBraceDoesNotTruncateASlice covers the second half of the same
// defect: an unbalanced `}` inside a block comment must not end the slice early.
func TestExtractDeclarationSlicesIgnoresBracesInComments(t *testing.T) {
	const src = `
shape live {
  /* }}} */
  b
}
`
	slices := ExtractDeclarationSlices(src, testShapeHeaderRE)
	if len(slices) != 1 {
		t.Fatalf("got %d slices, want 1", len(slices))
	}
	if !strings.Contains(slices[0].Source, "b") {
		t.Errorf("slice truncated at a brace inside a comment; got %q", slices[0].Source)
	}
}

// TestExtractDeclarationSlicesKeepsPreambleAcrossLineComments pins rule 3, the
// trap: the preamble walk must run on the ORIGINAL source. BlankComments blanks
// `//` lines, so walking the blanked view stops at one and silently strips
// every @-attribute above it.
func TestExtractDeclarationSlicesKeepsPreambleAcrossLineComments(t *testing.T) {
	const src = `
@executor("integration.workbench.dispatchHost")
// an authored note between the attribute and its declaration
@description("does real work")
shape live {
  b
}
`
	slices := ExtractDeclarationSlices(src, testShapeHeaderRE)
	if len(slices) != 1 {
		t.Fatalf("got %d slices, want 1", len(slices))
	}
	if !strings.Contains(slices[0].Source, "@executor") {
		t.Errorf("the @executor above a `//` line was stripped from the slice.\n"+
			"This is rule 3: the preamble walk must run on the original source, not "+
			"the blanked view.\ngot %q", slices[0].Source)
	}
}

// TestExtractDeclarationSlicesSkipsNestedHeaders pins the top-level guard --
// the rule only one of the six original copies had.
func TestExtractDeclarationSlicesSkipsNestedHeaders(t *testing.T) {
	const src = `
shape outer {
  shape inner {
    a
  }
}
`
	got := sliceNames(ExtractDeclarationSlices(src, testShapeHeaderRE))
	if len(got) != 1 || got[0] != "outer" {
		t.Fatalf("slices = %v, want [outer] -- a header nested inside another "+
			"construct's braces is not a standalone declaration", got)
	}
}

// TestExtractDeclarationSlicesCutsFromOriginal pins rule 2: the emitted text
// comes from the ORIGINAL source, so authored comments survive into the slice.
// Cutting from the blanked view would hand the parser a body full of spaces.
func TestExtractDeclarationSlicesCutsFromOriginal(t *testing.T) {
	const src = `
shape live {
  // an authored comment that must survive
  b
}
`
	slices := ExtractDeclarationSlices(src, testShapeHeaderRE)
	if len(slices) != 1 {
		t.Fatalf("got %d slices, want 1", len(slices))
	}
	if !strings.Contains(slices[0].Source, "an authored comment that must survive") {
		t.Errorf("slice was cut from the blanked view, not the original; got %q", slices[0].Source)
	}
}

// TestExtractDeclarationSlicesOffsetsIndexTheOriginal pins that Start/End index
// the original source, which every caller relies on to cut its own text.
func TestExtractDeclarationSlicesOffsetsIndexTheOriginal(t *testing.T) {
	const src = `
@description("x")
shape live {
  b
}
`
	slices := ExtractDeclarationSlices(src, testShapeHeaderRE)
	if len(slices) != 1 {
		t.Fatalf("got %d slices, want 1", len(slices))
	}
	s := slices[0]
	if src[s.Start:s.End] != s.Source {
		t.Errorf("src[Start:End] != Source.\n  src[%d:%d] = %q\n  Source     = %q",
			s.Start, s.End, src[s.Start:s.End], s.Source)
	}
}

// TestExtractDeclarationSlicesSkipsUnbalanced pins that a declaration whose
// braces never close is skipped rather than guessed at.
func TestExtractDeclarationSlicesSkipsUnbalanced(t *testing.T) {
	const src = `
shape broken {
  a
`
	if got := ExtractDeclarationSlices(src, testShapeHeaderRE); len(got) != 0 {
		t.Fatalf("slices = %v, want none -- an unbalanced declaration must not be guessed at",
			sliceNames(got))
	}
}
