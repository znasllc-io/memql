package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// keyword_slices_e2e_test.go -- memql#2868, end to end.
//
// The sibling extractor tests prove ExtractKeywordSlices no longer EMITS a
// block-commented declaration. That is necessary and not sufficient: the claim
// the issue actually made, and measured, is that the commented-out construct
// ends up in the LIVE REGISTRY --
//
//	registered concepts total=102
//	  MATCH: "v1:probecommentedkinds:probeLiveConcept"
//	  MATCH: "v1:probecommentedkinds:probeCommentedConcept"   <-- REGISTERED
//
// so the assertion has to be made against the registry, through the real
// loader, on a real registered domain. An extractor-only test would pass on a
// fix that stopped emitting the slice while some other path still registered
// it.

func e2eDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestLoadUnifiedConcepts_BlockCommentedConceptDoesNotRegister is the issue's
// own repro, as a test.
func TestLoadUnifiedConcepts_BlockCommentedConceptDoesNotRegister(t *testing.T) {
	overlay := fstest.MapFS{"concepts.memql": {Data: []byte(`/*
concept probeCommentedConcept {
  a string
}
*/

concept probeLiveConcept {
  b string
}
`)}}
	const domain = "probecommentedkinds"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	if _, err := LoadUnifiedConcepts(e2eDiscardLogger()); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v -- a block comment above a live concept must not "+
			"refuse the load (memql#2872 enumerates six shapes where a naive comment fix did "+
			"exactly that)", err)
	}

	var live, commented bool
	for _, c := range memoryNodes.DefaultRegistry().List() {
		if c == nil {
			continue
		}
		id := c.Name
		switch {
		case strings.Contains(id, "probeLiveConcept"):
			live = true
		case strings.Contains(id, "probeCommentedConcept"):
			commented = true
		}
	}

	if !live {
		t.Error("the LIVE concept did not register -- the fix must not cost the neighbour " +
			"declaration, which is the over-correction #2872 warns about")
	}
	if commented {
		t.Error("the block-commented concept REGISTERED. It validates writes and appears in the " +
			"concepts API; an author who commented it out has not disabled it (memql#2868). " +
			"Use @disabled for a deliberate off switch.")
	}
}

// TestBlankCommentsIsLengthPreserving pins the invariant BOTH slicers now rely
// on: BlankComments must return a string of the same byte length, and with the
// same newline positions, as its input.
//
// If it ever stops being length-preserving, every slice is cut at the wrong
// offset and constructs corrupt silently. conceptSlices guards this at runtime
// by comparing line counts and returning nil; this test says so up front, over
// the shapes most likely to break it.
func TestBlankCommentsIsLengthPreserving(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"block comment spanning lines", "a\n/*\nb\nc\n*/\nd\n"},
		{"multi-byte utf8 inside a comment", "a\n/* café — ünïcode ✓ */\nb\n"},
		{"crlf line endings", "a\r\n/* note */\r\nb\r\n"},
		{"braces inside a comment", "a\n/* { } { */\nb\n"},
		{"unterminated block comment", "a\n/* never closed\nb\n"},
		{"nested open delimiters", "a\n/* outer /* inner */ b\n"},
		{"line comment", "a\n// note\nb\n"},
		{"comment delimiters inside a string literal", "concept x {\n  a string @description(\"/* not a comment */\")\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := languageParser.BlankComments(tc.src)
			if len(got) != len(tc.src) {
				t.Fatalf("BlankComments changed byte length: %d -> %d.\nEvery slice offset in "+
					"conceptSlices and ExtractKeywordSlices depends on this being stable "+
					"(memql#2868).\nin:  %q\nout: %q", len(tc.src), len(got), tc.src, got)
			}
			if a, b := strings.Count(tc.src, "\n"), strings.Count(got, "\n"); a != b {
				t.Errorf("BlankComments changed the newline count: %d -> %d, so line indices no "+
					"longer align between the blanked and original views", a, b)
			}
		})
	}
}

// TestOrdinaryCommentsAboveALiveConceptStillLoad is the over-correction guard,
// and it is the one that matters most here.
//
// memql#2872 enumerated SIX shapes where a naive comment-awareness fix for the
// automation slicer caused a BOOT REFUSAL on valid, memqllint-clean input --
// because stepping over a comment pulled the comment BODY into the emitted
// slice, and compileMemQL runs raw-text gates over slice text. Each of these is
// an ordinary comment an author would really write above a construct.
//
// This fix takes the other route -- blank for SCANNING, cut from the ORIGINAL
// -- so the comment body was never going to be re-parsed as DSL. These cases
// assert that, because "my approach avoids that class" is a claim, not evidence.
func TestOrdinaryCommentsAboveALiveConceptStillLoad(t *testing.T) {
	for i, comment := range []string{
		"/* the old form used $steps.foo */",
		"/*\n@public\n*/",
		"/*\n@useConcept(node)\n*/",
		"/*\n x := mutation(concept: \"v1:probe:thing\")\n*/",
		"/* two blank lines follow\n\n\nstill in the comment */",
		"/* one blank line follows\n\nstill in the comment */",
	} {
		src := comment + "\nconcept probeStillLoads {\n  a string\n}\n"
		got := conceptSlices(src)
		if len(got) != 1 {
			t.Errorf("case %d: extracted %d slices, want 1 -- an ordinary comment above a live "+
				"concept must not cost the declaration (memql#2872's refusal class).\nsrc:\n%s",
				i, len(got), src)
			continue
		}
		if !strings.Contains(got[0], "concept probeStillLoads") {
			t.Errorf("case %d: emitted slice does not contain the live concept:\n%s", i, got[0])
		}
		// The comment body must not be re-parsed as DSL. It may appear in the
		// slice text (cut from the original, by design); what matters is that
		// the slice still PARSES to exactly one concept.
		if decls := ExtractConceptDecls(src); len(decls) != 1 {
			t.Errorf("case %d: ExtractConceptDecls returned %d decls, want 1 -- the comment body "+
				"reached the parser.\nsrc:\n%s", i, len(decls), src)
		}
	}
}
