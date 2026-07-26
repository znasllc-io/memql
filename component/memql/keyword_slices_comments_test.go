package memql

import (
	"strings"
	"testing"
)

// keyword_slices_comments_test.go -- memql#2868.
//
// ExtractKeywordSlices ran its header regex and brace walk over RAW source, so
// a commented-out declaration was extracted and LOADED. It is the extractor
// behind `extractAdapter` in unified_kinds_loader.go, i.e. behind concepts,
// shapes, tools, prompts, providers, specs and builtins.
//
// The blast radius is wider than the automation case (#2861) because these are
// not all inert schema:
//
//   - tool -- a commented-out tool is still offered to the model. An author
//     disabling a capability by commenting it out has not disabled it.
//   - provider -- a commented-out provider registers as a selectable AI vendor
//     lane, including one deliberately parked because its API key is not seeded.
//   - concept -- a commented-out schema is live: it validates writes and
//     appears in the concepts API.
//   - shape / spec / builtin -- same shape, lower stakes.
//
// Note the tree HAS a documented way to do this -- `@disabled` -- and nothing
// warned that the comment form silently does not work. That is the trap #2861
// describes, one loader over.
//
// # The fix pattern, copied rather than re-derived
//
// ExtractFunctionSlices has been comment-aware since #1074 and
// ExtractAutomationSlices since #2866: detect headers and match braces on
// `languageParser.BlankComments(source)` (which preserves byte offsets), keep
// cutting the emitted slice from the ORIGINAL source so authored comments
// survive in the slice text, and keep the `@`-attribute preamble walk on the
// ORIGINAL source.
//
// That last part is the trap, and it is why the walk is NOT moved to the
// blanked view: BlankComments blanks `//` lines too, and the preamble walk
// treats a `//` line as part of the preamble. Walking the blanked view makes it
// stop at such a line and strip the `@`-attributes above it -- silently
// dropping a concept's or tool's annotations. TestPreambleSurvivesALineComment
// below pins that.

func TestExtractKeywordSlicesSkipsBlockCommentedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		keyword string
		src     string
		want    []string
	}{
		{
			"concept",
			"/*\nconcept commentedConcept {\n  a string\n}\n*/\nconcept liveConcept {\n  b string\n}\n",
			[]string{"liveConcept"},
		},
		{
			"tool",
			"/*\ntool commentedTool {\n  a string\n}\n*/\ntool liveTool {\n  b string\n}\n",
			[]string{"liveTool"},
		},
		{
			"provider",
			"/*\nprovider commentedProvider {\n  auth { apiKey env(\"X\") }\n}\n*/\nprovider liveProvider {\n  params { x 1 }\n}\n",
			[]string{"liveProvider"},
		},
		{
			"shape",
			"/*\nshape user commentedShape {\n  row.id\n}\n*/\nshape user liveShape {\n  row.id\n}\n",
			[]string{"liveShape"},
		},
		// A LINE-commented declaration is the same defect in the cheaper
		// spelling, and the one an author is most likely to reach for.
		{
			"concept",
			"// concept lineCommentedConcept {\n//   a string\n// }\nconcept liveAfterLineComment {\n  b string\n}\n",
			[]string{"liveAfterLineComment"},
		},
		// Only the commented one exists -> nothing is extracted. Pinned
		// separately because a fix that merely reorders the slice would still
		// pass the cases above.
		{
			"tool",
			"/*\ntool onlyCommented {\n  a string\n}\n*/\n",
			nil,
		},
	} {
		t.Run(tc.keyword+"/"+strings.Join(tc.want, ","), func(t *testing.T) {
			got := sliceNames(ExtractKeywordSlices(tc.src, tc.keyword))
			if len(got) != len(tc.want) {
				t.Fatalf("extracted %v, want %v -- a commented-out %s must not be loaded (memql#2868)",
					got, tc.want, tc.keyword)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("slice %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPreambleSurvivesALineComment is the guard on the fix's one trap.
//
// BlankComments blanks `//` lines, and the preamble walk treats a `//` line as
// part of the preamble. If the walk runs on the BLANKED view it stops at the
// blanked line and silently strips every `@`-attribute above it -- so a
// concept's or tool's annotations vanish, which is a worse failure than the one
// being fixed (an author's `@disabled` would stop reaching the parser).
//
// #2866 and function_slices.go both make this split; this pins it here.
func TestPreambleSurvivesALineComment(t *testing.T) {
	src := "@enabled\n@description(\"live\")\n// why this exists\ntool annotated {\n  a string\n}\n"

	slices := ExtractKeywordSlices(src, "tool")
	if len(slices) != 1 {
		t.Fatalf("extracted %d slices, want 1: %v", len(slices), sliceNames(slices))
	}
	got := slices[0].Source

	for _, want := range []string{"@enabled", `@description("live")`, "// why this exists"} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted slice lost %q -- the preamble walk must run on the ORIGINAL source, "+
				"not the comment-blanked view, or annotations above a `//` line are dropped "+
				"(memql#2868). Got:\n%s", want, got)
		}
	}
}

// TestExtractKeywordSlicesKeepsAuthoredCommentsInTheSliceText pins the other
// half of the offset-preserving design: the slice is cut from the ORIGINAL, so
// a comment INSIDE a live construct survives into the emitted text rather than
// arriving blanked.
func TestExtractKeywordSlicesKeepsAuthoredCommentsInTheSliceText(t *testing.T) {
	src := "concept live {\n  // the field's reason\n  a string\n}\n"

	slices := ExtractKeywordSlices(src, "concept")
	if len(slices) != 1 {
		t.Fatalf("extracted %d slices, want 1", len(slices))
	}
	if !strings.Contains(slices[0].Source, "// the field's reason") {
		t.Errorf("the emitted slice was cut from the blanked view -- authored comments must "+
			"survive in the slice text. Got:\n%s", slices[0].Source)
	}
}

// TestExtractKeywordSlicesStillFindsCommentAdjacentDeclarations guards against
// over-correcting: a declaration that merely SITS NEXT TO a comment, or has a
// brace-bearing comment inside it, must still be found.
func TestExtractKeywordSlicesStillFindsCommentAdjacentDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{
			"brace inside a comment does not unbalance the walk",
			"concept live {\n  /* } not a real close */\n  a string\n}\nconcept second {\n  b string\n}\n",
			[]string{"live", "second"},
		},
		{
			"doc comment above",
			"/// docs\nconcept live {\n  a string\n}\n",
			[]string{"live"},
		},
		{
			"block comment above, live declaration below",
			"/* parked note, see #123 */\nconcept live {\n  a string\n}\n",
			[]string{"live"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sliceNames(ExtractKeywordSlices(tc.src, "concept"))
			if len(got) != len(tc.want) {
				t.Fatalf("extracted %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("slice %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCapabilitySlicesSkipBlockCommentedDeclarations covers the instance #2868
// named as highest-stakes and explicitly left unverified ("I did not execute
// this one... Someone taking this issue should verify it directly rather than
// trusting the read").
//
// The read was correct: extractCapabilitySlices matched capabilityHeaderRe
// against raw source with no comment blanking, structurally identical to
// ExtractKeywordSlices. It matters more than the schema kinds because
// capabilities back DSL `action`s and deploy scripts -- a commented-out
// capability that still loads is a deploy-surface concern.
func TestCapabilitySlicesSkipBlockCommentedDeclarations(t *testing.T) {
	src := "/*\ncapability deploy.commented {\n  script \"x.sh\"\n}\n*/\n" +
		"capability deploy.live {\n  script \"y.sh\"\n}\n"

	got := extractCapabilitySlices(src)
	if len(got) != 1 {
		t.Fatalf("extracted %d capability slices, want 1 -- a commented-out capability must not "+
			"load (memql#2868); capabilities back DSL actions and deploy scripts.\nGot: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "deploy.live") {
		t.Errorf("extracted the wrong capability: %q", got[0])
	}
	if strings.Contains(got[0], "deploy.commented") {
		t.Errorf("the emitted slice contains the commented-out capability: %q", got[0])
	}
}
