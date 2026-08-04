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
			// No `@` in the run at all, so nothing was orphaned -- a `//` note
			// explaining WHY something is parked is the likeliest real-world
			// shape here, and reporting it is the false alarm that gets a
			// detector suppressed. Pins the sawAttribute guard, which the
			// #3041 review found survived removal with the whole repo green.
			name: "a line-comment note above a parked declaration, with no annotations",
			src: `// parked until #1234 lands
/*
builtin zzParked {
  a string
}
*/
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

// The shapes the loader ALSO orphans, beyond the one memql#2965 measured.
//
// Every case here was silent before the memql#3041 review: the first four
// because block-comment state was counted textually rather than taken from
// CommentSpans, and the fifth because a line-oriented "does anything follow"
// test skipped the very line the declaration started on. Each is asserted
// against the slicer in the same subtest, so none of them can decay into a
// diagnostic for a defect that no longer exists -- the failure mode
// TestPreambleIsStillOrphanedByABlockComment guards for the measured source.
func TestOrphanedPreamblesReportsWhatTheLoaderAlsoOrphans(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{
			// MemQL block comments do NOT nest -- baseparser leaves the block
			// state at the first `*/`. A depth counter does nest, so a stray
			// `/*` in an earlier banner left it stuck open and suppressed
			// every later orphan in the file.
			name: "an earlier banner comment containing a stray /*",
			src: `/*
 banner with a /* inside
*/
@executor("integration.x.y")
/*
builtin zzParked {
  a string
}
*/
builtin zzLive {
  b string
}
`,
		},
		{
			// A `/*` inside a string literal opens nothing. The lexer knows
			// that; counting `/*` does not.
			name: "a /* inside a string literal earlier in the file",
			src: `@description("a slash star /* in a string")
builtin zzOther {
  a string
}

@executor("integration.x.y")
/*
builtin zzParked {
  b string
}
*/
builtin zzLive {
  c string
}
`,
		},
		{
			name: "a /* inside a backtick literal earlier in the file",
			src: "@description(`a slash star /* in a backtick`)\n" + `builtin zzOther {
  a string
}

@executor("integration.x.y")
/*
builtin zzParked {
  b string
}
*/
builtin zzLive {
  c string
}
`,
		},
		{
			// A `/*` after `//` on the same line is comment text, not an
			// opener -- the old skip only fired when the line STARTED with
			// `//`.
			name: "a trailing line comment containing /*",
			src: `builtin zzOther {  // a slash star /* in a trailing comment
  a string
}

@executor("integration.x.y")
/*
builtin zzParked {
  b string
}
*/
builtin zzLive {
  c string
}
`,
		},
		{
			// The declaration begins on the line that CLOSES the comment.
			// BlankComments turns `*/` into spaces, so the slicer sees
			// `   builtin zzLive {` and really does register it without the
			// executor -- asserted below like every other case here.
			name: "the declaration starts on the line that closes the comment",
			src: `@executor("integration.x.y")
/*
builtin zzParked { a string }
*/ builtin zzLive { b string }
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The premise: the loader really does drop the annotations here.
			// Without this the diagnostic could outlive the defect.
			slices := ExtractDeclarationSlices(tc.src, builtinHeaderRe)
			var live *DeclarationSlice
			for i := range slices {
				if slices[i].Name == "zzLive" {
					live = &slices[i]
				}
			}
			if live == nil {
				t.Fatalf("fixture no longer slices zzLive at all; the premise is gone")
			}
			if strings.Contains(live.Source, "@executor") {
				t.Fatalf("the slicer now carries the @executor across the comment, so this is no "+
					"longer an orphan and reporting it would be a false alarm:\n%s", live.Source)
			}

			got := OrphanedPreambles(tc.src)
			if len(got) != 1 {
				t.Fatalf("the loader orphans this preamble but the detector reported %d -- a SILENT "+
					"MISS, which is the class memql#2965 exists to end: %+v\n\nsource:\n%s",
					len(got), got, tc.src)
			}
			if !strings.Contains(got[0].Attributes, "@executor") {
				t.Errorf("Attributes must carry the orphaned run, got %q", got[0].Attributes)
			}
		})
	}
}

// A file header separated from the first declaration by a banner comment is
// reported, and that is deliberate: conceptSlices drops `@version` /
// `@namespace` exactly as the builtin path drops an `@executor`, so the concept
// registers at the default version with nothing logged.
//
// Pinned because the rule's own doc comment claimed the opposite until the
// memql#3041 review measured it, and because the shape is one line of
// whitespace away from every file in dsl/.
func TestOrphanedPreamblesReportsAFileHeaderABannerDetaches(t *testing.T) {
	src := `@version("1.0.0")
@namespace("probe")
/* ---------------- concepts ---------------- */
@description("d")
concept thing {
  label string
}
`
	got := OrphanedPreambles(src)
	if len(got) != 1 {
		t.Fatalf("expected the detached file header to be reported once, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Attributes, "@version") ||
		!strings.Contains(got[0].Attributes, "@namespace") {
		t.Errorf("Attributes must name what the file lost, got %q", got[0].Attributes)
	}

	// The conventional blank line after a file header ends the run before the
	// comment does, which is why dsl/ is clean today. If that ever stops being
	// true this test says so rather than the whole corpus going red at once.
	withBlankLine := strings.Replace(src, "@namespace(\"probe\")\n/*", "@namespace(\"probe\")\n\n/*", 1)
	if got := OrphanedPreambles(withBlankLine); len(got) != 0 {
		t.Errorf("a blank line after the file header must end the run, got %+v", got)
	}
}

// Inside a declaration body nothing is orphaned: the whole declaration is one
// slice and the real parser sees every annotation in it. Reporting there is a
// false alarm, and this lane is wired as an ERROR -- so the guard is the
// difference between a clean build and a red one on a source that is fine.
//
// Gated on BraceDepthBefore over the blanked view, exactly as
// ExtractDeclarationSlices gates its own headers.
func TestOrphanedPreamblesIgnoresAnnotationsInsideADeclarationBody(t *testing.T) {
	src := `builtin zzLive {
  @description("d")
  /* parked
     b string
  */
  a string
}
`
	// The premise: the slicer keeps the annotation INSIDE the declaration, so
	// there is nothing detached to report.
	slices := ExtractDeclarationSlices(src, builtinHeaderRe)
	if len(slices) != 1 || !strings.Contains(slices[0].Source, "@description") {
		t.Fatalf("premise gone -- the slicer no longer carries the inner annotation: %+v", slices)
	}
	if got := OrphanedPreambles(src); len(got) != 0 {
		t.Errorf("annotations inside a declaration body are not orphaned; reporting them is the "+
			"false alarm that gets an error-severity lane suppressed: %+v", got)
	}
}

// A KNOWN GAP, pinned so it is recorded rather than rediscovered.
//
// `@description("d") /*` opens a block comment on a line that STARTS with `@`.
// preambleStartOf keeps walking across that line, so the loader really does
// orphan the run -- but the rule only considers a comment that opens its own
// line, and reports nothing. Closing it means deciding whether an annotation
// line that opens a comment terminates its own run, which is a judgement about
// what the author meant rather than a lexing question; memql#2965 chose
// reporting over guessing for exactly that reason.
//
// If this test ever fails because the count became 1, that is the gap being
// closed deliberately -- update it rather than reverting.
func TestOrphanedPreamblesDoesNotSeeATrailingCommentOpener(t *testing.T) {
	src := `@executor("integration.x.y")
@description("d") /* parked
builtin zzParked { a string }
*/
builtin zzLive { b string }
`
	slices := ExtractDeclarationSlices(src, builtinHeaderRe)
	if len(slices) != 1 || strings.Contains(slices[0].Source, "@executor") {
		t.Fatalf("premise gone -- the loader no longer orphans this run: %+v", slices)
	}
	if got := OrphanedPreambles(src); len(got) != 0 {
		t.Errorf("this shape is a documented gap, not a supported case. If it is now detected "+
			"deliberately, update this test and the scope comment in orphaned_preamble.go "+
			"together; got %+v", got)
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
