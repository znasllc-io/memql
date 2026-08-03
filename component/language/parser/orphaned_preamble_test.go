package parser

import (
	"regexp"
	"strings"
	"testing"
)

var builtinHeaderRe = regexp.MustCompile(`(?m)^[ \t]*builtin[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// The measured source from memql#2965, verbatim.
const parkedNeighbourSource = `@executor("integration.workbench.dispatchHost")
@description("does real work")
/*
builtin zzParked {
  a string
}
*/
builtin zzLive {
  b string
}
`

// The behaviour this reports on, asserted first so the diagnostic is anchored
// to a real defect rather than to a hypothesis. If the slicer ever starts
// carrying the preamble across the comment, this fails and the detector should
// be deleted with it -- reporting an orphan that no longer exists is worse than
// not reporting.
func TestPreambleIsStillOrphanedByABlockComment(t *testing.T) {
	slices := ExtractDeclarationSlices(parkedNeighbourSource, builtinHeaderRe)
	if len(slices) != 1 {
		t.Fatalf("expected exactly the live builtin to be sliced, got %d: %+v", len(slices), slices)
	}
	if slices[0].Name != "zzLive" {
		t.Fatalf("the parked builtin was sliced: %q", slices[0].Name)
	}
	if strings.Contains(slices[0].Source, "@executor") {
		t.Errorf("the slice now carries the @executor from above the block comment. That is the "+
			"reattachment memql#2965 chose NOT to do (it would hand zzLive an executor its "+
			"author may have written for zzParked). If it is deliberate now, delete "+
			"OrphanedPreambles with it.\n  slice:\n%s", slices[0].Source)
	}
}

// The diagnostic itself.
func TestOrphanedPreambleIsReported(t *testing.T) {
	got := OrphanedPreambles(parkedNeighbourSource)
	if len(got) != 1 {
		t.Fatalf("the orphaned @executor must be reported exactly once; got %d: %+v", len(got), got)
	}
	if got[0].Line != 1 {
		t.Errorf("Line must point at the FIRST @ line -- where the author edits -- got %d", got[0].Line)
	}
	if got[0].CommentLine != 3 {
		t.Errorf("CommentLine must point at the `/*` that broke the run, got %d", got[0].CommentLine)
	}
	if !strings.Contains(got[0].Attributes, "@executor") ||
		!strings.Contains(got[0].Attributes, "@description") {
		t.Errorf("Attributes must carry the whole orphaned run so the diagnostic can name what "+
			"was lost; got %q", got[0].Attributes)
	}
}

// The other direction, and the one that decides whether this is usable: an
// ordinary tree must produce no diagnostics at all. A detector that cries wolf
// on the shapes people actually write gets suppressed, and then the real case
// goes unreported too.
func TestOrphanedPreamblesStaysQuietOnOrdinarySources(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{
			name: "annotations attached normally",
			src: `@executor("integration.x.y")
@description("d")
builtin zzLive {
  b string
}
`,
		},
		{
			name: "file-level annotations above a declaration",
			src: `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  a string
}
`,
		},
		{
			name: "a header block comment with no annotations above it",
			src: `/*
   A note about this file.
*/
@executor("integration.x.y")
builtin zzLive {
  b string
}
`,
		},
		{
			name: "a block comment BELOW the declaration it documents",
			src: `@executor("integration.x.y")
builtin zzLive {
  b string
}
/*
  A trailing note.
*/
`,
		},
		{
			name: "a parked declaration whose annotations were parked with it",
			src: `/*
@executor("integration.x.y")
builtin zzParked {
  a string
}
*/
@executor("integration.a.b")
builtin zzLive {
  b string
}
`,
		},
		{
			name: "line comments between annotations and the declaration",
			src: `@executor("integration.x.y")
// why this executor
builtin zzLive {
  b string
}
`,
		},
		{
			name: "a trailing block comment at end of file orphans nothing",
			src: `@executor("integration.x.y")
builtin zzLive {
  b string
}

@description("stale note")
/*
  nothing follows
*/
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OrphanedPreambles(tc.src); len(got) != 0 {
				t.Errorf("ordinary source reported %d orphan(s), which is the false positive that "+
					"gets a detector suppressed: %+v\n\nsource:\n%s", len(got), got, tc.src)
			}
		})
	}
}

// The live tree must be clean, or the diagnostic cannot be wired in as an
// error. memql#2965 states zero builtins in dsl/ sit in this shape; this is
// where that stops being a claim.
func TestOrphanedPreamblesFindsNothingInHandWrittenShapes(t *testing.T) {
	// Two shapes the repo genuinely uses, kept here because this package
	// cannot reach the dsl/ tree (it is imported BY the loaders, not the
	// reverse). dsl/ itself is covered by the integrity lane's own test.
	for _, src := range []string{
		"// header comment\n@enabled\n@description(\"d\")\nbuiltin b {\n  a string\n}\n",
		"/* block header */\n\n@enabled\nbuiltin b {\n  a string\n}\n",
	} {
		if got := OrphanedPreambles(src); len(got) != 0 {
			t.Errorf("reported %+v for:\n%s", got, src)
		}
	}
}

// Multiple orphans in one file are each reported -- a file with two parked
// declarations should not report only the first.
func TestOrphanedPreamblesReportsEachOccurrence(t *testing.T) {
	src := parkedNeighbourSource + "\n" + parkedNeighbourSource
	if got := OrphanedPreambles(src); len(got) != 2 {
		t.Errorf("expected 2 orphans, got %d: %+v", len(got), got)
	}
}
