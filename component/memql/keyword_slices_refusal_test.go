package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// keyword_slices_refusal_test.go -- memql#2868, the over-correction half for the
// kinds this fix actually put at risk.
//
// Review found the existing refusal coverage was pointed at the wrong loader.
// TestOrdinaryCommentsAboveALiveConceptStillLoad exercises conceptSlices ->
// ExtractConceptDecls, which goes through languageParser.ParseFile -- and
// #2872's six refusals were `compileMemQL` raw-text gates over FUNCTION slice
// text (`'$steps.' is not allowed`, `unknown automation annotation @public`,
// `@useConcept(...) is retired`, direct `mutation()` calls). None of those gates
// run on the concept path, so that test passed for a reason unrelated to what
// it claimed, and the kinds reached through ExtractKeywordSlices -- tool,
// provider, shape, builtin -- had no refusal coverage at all.
//
// These drive the real per-kind parsers, which is the pairing that matters: the
// slicer must emit a slice AND that slice must still parse.
//
// A SEVENTH shape is included that the six-shape table never used: CRLF plus a
// comment containing one blank line. That is #2872's shape 6 verbatim, and it
// is the one most likely to break an offset-preserving fix, because CRLF makes
// every line two bytes longer than a naive splitter expects.

// commentShapesFrom2872 are the comment forms that caused BOOT REFUSALS when a
// naive comment fix was attempted on the automation slicer (#2872). Each is an
// ordinary comment an author would really write above a construct.
var commentShapesFrom2872 = []struct{ name, comment string }{
	{"mentions $steps.", "/* the old form used $steps.foo */"},
	{"contains @public", "/*\n@public\n*/"},
	{"contains @useConcept", "/*\n@useConcept(node)\n*/"},
	{"contains a mutation() call", "/*\n x := mutation(concept: \"v1:probe:thing\")\n*/"},
	{"two consecutive blank lines", "/* note\n\n\nstill in the comment */"},
	{"one blank line", "/* note\n\nstill in the comment */"},
	{"CRLF with one blank line", "/* note\r\n\r\nstill in the comment */"},
}

// TestOrdinaryCommentsAboveKeywordConstructsStillParse is the pairing: slice
// emitted, and the slice still accepted by the kind's real parser.
func TestOrdinaryCommentsAboveKeywordConstructsStillParse(t *testing.T) {
	kinds := []struct {
		keyword string
		body    string
		parse   func(string) error
	}{
		{"tool", "tool probeTool {\n  a string\n}\n", func(s string) error {
			_, err := languageParser.ParseToolDecl(s)
			return err
		}},
		{"provider", "@base\n@type(\"OpenAI\")\nprovider probeProvider {\n  auth {\n    apiKey env(\"X\")\n  }\n}\n", func(s string) error {
			_, err := languageParser.ParseProviderDecl(s)
			return err
		}},
		{"builtin", "@executor(\"integration.probe.run\")\nbuiltin probeBuiltin {\n  a string\n}\n", func(s string) error {
			_, err := languageParser.ParseBuiltinDecl(s)
			return err
		}},
	}

	for _, k := range kinds {
		for _, shape := range commentShapesFrom2872 {
			t.Run(k.keyword+"/"+shape.name, func(t *testing.T) {
				src := shape.comment + "\n" + k.body

				slices := ExtractKeywordSlices(src, k.keyword)
				if len(slices) != 1 {
					t.Fatalf("extracted %d slices, want 1 -- an ordinary comment above a live %s "+
						"must not cost the declaration (memql#2872's refusal class).\nsrc:\n%s",
						len(slices), k.keyword, src)
				}
				if err := k.parse(slices[0].Source); err != nil {
					t.Errorf("the emitted slice no longer parses as a %s: %v\n"+
						"This is the #2872 failure mode: a comment body reaching a raw-text gate "+
						"and refusing an otherwise-valid construct.\nslice:\n%s",
						k.keyword, err, slices[0].Source)
				}
			})
		}
	}
}

// TestConceptSlicesIsStringBlind pins a PRE-EXISTING limit rather than a fix, so
// the refactor that closes it has a before/after.
//
// conceptSlices counts braces per line with strings.Count, which cannot tell a
// brace inside a string literal from a real one. The sibling
// ExtractKeywordSlices can, because it balances through the string-aware
// findMatchingCloseBraceRune. So the two slicers disagree, and on the concept
// side the consequence is a SILENT DROP -- not a truncated construct, no
// concept at all.
//
// Latent today: the tree has brace-bearing string literals but none unbalanced.
// One `@description("use } to close")` in a concepts file would delete that
// concept from the registry with no diagnostic.
func TestConceptSlicesIsStringBlind(t *testing.T) {
	src := "concept probeBraceInString {\n  a string @description(\"a } brace\")\n  b string\n}\n"

	got := conceptSlices(src)
	if len(got) == 1 && strings.Contains(got[0], "b string") {
		t.Fatal("conceptSlices is now string-AWARE -- the brace inside the @description no longer " +
			"closes the slice early. That is the fix this test was pinning the absence of: delete " +
			"this test and note the change in concepts_only_extractor.go's KNOWN LIMIT paragraph.")
	}
	t.Logf("string-blind as expected: %d slice(s); a brace inside a string literal closes the "+
		"concept early, so the construct is silently dropped (pre-existing, memql#2868 review)", len(got))
}
