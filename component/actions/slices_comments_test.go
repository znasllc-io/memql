package actions

import (
	"strings"
	"testing"
)

// slices_comments_test.go -- memql#2896 defects 1 and 2.
//
// Both slicers in this package scan RAW source, so they are the pre-#2868
// behaviour that #2868 fixed one package over:
//
//   - extractCapabilityDeclSlices (catalog.go) is a SECOND capability slicer,
//     identical to the fixed component/memql one but still raw. That falsifies
//     the comment at component/memql/capability_loader.go asserting "the actions
//     catalog applies the same filter ... so validation and dispatch stay in
//     sync": declaredCapabilityNames() drops a commented capability, so an
//     authored bundle importing it fails Gate-1 with "unresolved reference",
//     while the actions catalog still holds it and would dispatch.
//
//   - extractActionSlices (loader.go) has the truncation bug too. Its
//     matchingCloseBrace is string- and LINE-comment aware but not
//     BLOCK-comment aware, so a `}` inside a block comment in an action body
//     ends the slice early.
//
// The fix is the pattern #2868 established and this issue asks to share rather
// than copy a sixth time: detect headers and match braces on the
// comment-blanked view (offsets preserved), cut the emitted slice from the
// ORIGINAL source so authored comments survive, and keep the @-attribute
// preamble walk on the ORIGINAL source -- see the note in
// component/memql/keyword_slices_comments_test.go for why that last part must
// NOT move to the blanked view.

func TestExtractCapabilityDeclSlicesSkipsBlockCommented(t *testing.T) {
	const src = `
/*
capability integration.foo.parked {
  description "parked in a comment"
}
*/
capability integration.foo.live {
  description "the real one"
}
`
	got := extractCapabilityDeclSlices(src)

	if len(got) != 1 {
		t.Fatalf("got %d slices, want 1 (the block-commented capability was extracted)\nslices: %#v",
			len(got), got)
	}
	if !strings.Contains(got[0], "integration.foo.live") {
		t.Errorf("extracted the wrong declaration: %q", got[0])
	}
	if strings.Contains(got[0], "integration.foo.parked") {
		t.Errorf("slice contains the commented-out capability: %q", got[0])
	}
}

func TestExtractActionSlicesSkipsBlockCommented(t *testing.T) {
	const src = `
/*
action zzCommentedAction {
  capability "noop"
}
*/
action zzLiveAction {
  capability "noop"
}
`
	got := extractActionSlices(src)

	var names []string
	for _, s := range got {
		names = append(names, s.name)
	}
	if len(got) != 1 || (len(got) == 1 && got[0].name != "zzLiveAction") {
		t.Fatalf("got names %v, want [zzLiveAction] -- a block-commented action was loaded", names)
	}
}

// TestExtractActionSlicesNotTruncatedByBraceInBlockComment is the truncation
// half. matchingCloseBrace counts the `}` inside `/* } */` as the action's
// closing brace, cutting the slice mid-body.
func TestExtractActionSlicesNotTruncatedByBraceInBlockComment(t *testing.T) {
	const src = `action zzLive {
  /* } not a real close */
  capability "noop"
}
`
	got := extractActionSlices(src)

	if len(got) != 1 {
		t.Fatalf("got %d slices, want 1", len(got))
	}
	if !strings.Contains(got[0].source, `capability "noop"`) {
		t.Errorf("slice truncated at the brace inside the block comment -- "+
			"the body after it is missing.\ngot:\n%s", got[0].source)
	}
	if !strings.HasSuffix(strings.TrimSpace(got[0].source), "}") {
		t.Errorf("slice does not end at the real closing brace.\ngot:\n%s", got[0].source)
	}
}

// TestActionSlicePreambleSurvivesALineComment pins the trap that keeps the
// preamble walk on the ORIGINAL source. BlankComments blanks `//` lines, and the
// preamble walk treats a `//` line as part of the preamble -- so walking the
// blanked view stops at the blanked line and silently strips the @-attributes
// above it. Mirrors TestPreambleSurvivesALineComment in component/memql.
func TestActionSlicePreambleSurvivesALineComment(t *testing.T) {
	const src = `@description("keep me")
// an explanatory line comment
action zzLive {
  capability "noop"
}
`
	got := extractActionSlices(src)

	if len(got) != 1 {
		t.Fatalf("got %d slices, want 1", len(got))
	}
	if !strings.Contains(got[0].source, `@description("keep me")`) {
		t.Errorf("the @-attribute above a line comment was stripped from the preamble.\ngot:\n%s",
			got[0].source)
	}
}

// TestExtractActionSlicesOnlyTopLevel pins the guard the model implementation
// has and these copies lack: a header nested inside another construct's braces
// is not a standalone declaration.
func TestExtractActionSlicesOnlyTopLevel(t *testing.T) {
	const src = `action zzOuter {
  nested {
    action zzInner {
      capability "noop"
    }
  }
}
`
	got := extractActionSlices(src)

	for _, s := range got {
		if s.name == "zzInner" {
			t.Errorf("extracted a nested action header as a top-level declaration: %q", s.name)
		}
	}
}
