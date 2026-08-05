package main

import (
	"strings"
	"testing"
)

// main_test.go pins the source scan this tool rewrites through.
//
// The tool is what the G5 gate's own error message tells authors to run with
// -write against their DSL tree, and it shipped with no tests at all while
// carrying two verbatim copies of the string-scanning bug memql#2949 fixed in
// that same gate (memql#3045). The cases below are written against the two
// consumers separately -- scrub, which decides which fields reach the args
// block, and mapCodeSegments, which decides what gets rewritten -- because a
// scan that is right for one and wrong for the other is exactly the disagreement
// the issue is about.

// ---------------------------------------------------------------------------
// The memql#2949 defect: a literal ending in a COMPLETED `\\` escape.
// ---------------------------------------------------------------------------

// escapedBackslashSrc ends a literal with `\\`. A one-byte lookback reads that
// literal's own closing quote as escaped and runs on to the NEXT quote,
// swallowing the payload read and the `field:` key between them.
const escapedBackslashSrc = `automation a {
  step call {
    path:  "C:\\"
    field: event.payload.status
    label: "done"
  }
}
`

func TestScrubDoesNotSwallowCodeAfterCompletedEscape(t *testing.T) {
	got := scrub(escapedBackslashSrc)

	if !strings.Contains(got, "event.payload.status") {
		t.Fatalf("scrub blanked a real payload read after a literal ending in `\\\\`.\n"+
			"That read is then missing from the args block the tool synthesizes.\ngot:\n%s", got)
	}
	if strings.Contains(got, `C:\\`) {
		t.Errorf("scrub left the literal interior unblanked.\ngot:\n%s", got)
	}
	if strings.Contains(got, "done") {
		t.Errorf("scrub left the trailing literal unblanked.\ngot:\n%s", got)
	}
}

func TestCollectPayloadFieldsSeesReadAfterCompletedEscape(t *testing.T) {
	got := collectPayloadFields(escapedBackslashSrc)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status] -- the read after the `\\\\` literal was lost", got)
	}
}

func TestMapCodeSegmentsRewritesAfterCompletedEscape(t *testing.T) {
	got := mapCodeSegments(escapedBackslashSrc, func(code string) string {
		return strings.ReplaceAll(code, "event.payload.status", "status")
	})

	if strings.Contains(got, "event.payload.status") {
		t.Fatalf("mapCodeSegments treated real code as literal interior and skipped the rewrite.\n"+
			"This is the -write path: the migration silently does not happen.\ngot:\n%s", got)
	}
	if !strings.Contains(got, `"C:\\"`) {
		t.Errorf("mapCodeSegments did not pass the literal through verbatim.\ngot:\n%s", got)
	}
	if !strings.Contains(got, `"done"`) {
		t.Errorf("mapCodeSegments did not pass the trailing literal through verbatim.\ngot:\n%s", got)
	}
}

func TestEscapedQuoteStillEndsNothing(t *testing.T) {
	// The converse of the case above: `\"` IS an escape, so the literal runs
	// on to the real closing quote. A fix that simply stopped tracking escapes
	// would pass the tests above and fail this one.
	src := `x: "he said \"hi\""
y: event.payload.status
`
	got := scrub(src)
	if strings.Contains(got, "hi") {
		t.Errorf("an escaped quote ended the literal early.\ngot:\n%s", got)
	}
	if !strings.Contains(got, "event.payload.status") {
		t.Errorf("scrub over-consumed past the real closing quote.\ngot:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// The memql#2872 gap: `/* ... */` is prose, not code.
// ---------------------------------------------------------------------------

const blockCommentSrc = `automation a {
  /* before #2367 this read event.payload.legacy directly */
  step call {
    field: event.payload.status
  }
}
`

func TestBlockCommentIsNotCollectedAsAField(t *testing.T) {
	got := collectPayloadFields(blockCommentSrc)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status] -- a read named only in a block comment was collected", got)
	}
}

func TestBlockCommentIsNotRewritten(t *testing.T) {
	got := mapCodeSegments(blockCommentSrc, func(code string) string {
		return strings.ReplaceAll(code, "event.payload.legacy", "legacy")
	})
	if !strings.Contains(got, "/* before #2367 this read event.payload.legacy directly */") {
		t.Fatalf("mapCodeSegments rewrote the interior of a block comment.\ngot:\n%s", got)
	}
}

func TestLineCommentIsProse(t *testing.T) {
	src := "// historically event.payload.legacy\nfield: event.payload.status\n"
	got := collectPayloadFields(src)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status]", got)
	}
}

// The payload read must sit on the SAME LINE as the URL literal. With it on the
// next line this test passed even with the string-literal arm removed from
// scanSegments entirely: a spurious `//` comment runs only to end of line, so
// the read on line 2 survived either way and the test proved nothing about the
// behaviour it is named for.
func TestCommentMarkerInsideLiteralIsNotAComment(t *testing.T) {
	src := `url: "https://example.com/a", field: event.payload.status
`
	got := collectPayloadFields(src)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status] -- the `//` in a URL literal started a comment "+
			"and blanked the rest of the line", got)
	}
}

// ---------------------------------------------------------------------------
// Literal shape: multi-line, and the unbalanced-quote bound.
// ---------------------------------------------------------------------------

func TestLiteralMaySpanLines(t *testing.T) {
	// The lexer's scanString has no newline case, so this is ONE token. A scan
	// that stopped at the newline would read the closing quote as an OPENING
	// one and blank the genuine read after it.
	src := "note: \"one\ntwo event.payload.ghost\"\nfield: event.payload.status\n"
	got := collectPayloadFields(src)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status] -- multi-line literal mis-scanned", got)
	}
}

func TestUnbalancedQuoteCostsOnlyItsOwnLine(t *testing.T) {
	src := "note: \"unterminated\nfield: event.payload.status\n"
	got := collectPayloadFields(src)
	if len(got) != 1 || got[0] != "status" {
		t.Fatalf("collectPayloadFields = %v, want [status] -- an unbalanced quote ate the rest of the file", got)
	}
}

// ---------------------------------------------------------------------------
// Invariants the two consumers rely on.
// ---------------------------------------------------------------------------

func TestScrubPreservesLengthAndLineCount(t *testing.T) {
	for _, src := range []string{
		escapedBackslashSrc,
		blockCommentSrc,
		"note: \"one\ntwo\"\nfield: event.payload.status\n",
		"/* a\nb\nc */\nfield: event.payload.status\n",
		"note: \"unterminated\nfield: event.payload.status\n",
	} {
		got := scrub(src)
		if len(got) != len(src) {
			t.Errorf("scrub changed length: %d -> %d\nsrc:\n%s", len(src), len(got), src)
		}
		if a, b := strings.Count(src, "\n"), strings.Count(got, "\n"); a != b {
			t.Errorf("scrub changed line count: %d -> %d\nsrc:\n%s", a, b, src)
		}
	}
}

func TestScanSegmentsTileTheSourceExactly(t *testing.T) {
	for _, src := range []string{
		escapedBackslashSrc,
		blockCommentSrc,
		`x: "a" // b
/* c */ y: event.payload.z
`,
		"", "\"", "/*", "//", `"\`,
	} {
		segs := scanSegments(src)
		next := 0
		var b strings.Builder
		for _, seg := range segs {
			if seg.start != next {
				t.Fatalf("segments do not tile %q: gap or overlap at %d (want %d)", src, seg.start, next)
			}
			if seg.end <= seg.start {
				t.Fatalf("segments do not tile %q: empty segment at %d", src, seg.start)
			}
			next = seg.end
			b.WriteString(src[seg.start:seg.end])
		}
		if next != len(src) {
			t.Fatalf("segments do not cover %q: stopped at %d of %d", src, next, len(src))
		}
		if b.String() != src {
			t.Fatalf("segments do not reconstruct %q, got %q", src, b.String())
		}
	}
}

func TestMapCodeSegmentsIsIdentityUnderIdentity(t *testing.T) {
	for _, src := range []string{escapedBackslashSrc, blockCommentSrc} {
		if got := mapCodeSegments(src, func(c string) string { return c }); got != src {
			t.Errorf("mapCodeSegments with an identity fn changed the source.\nsrc:\n%s\ngot:\n%s", src, got)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end: the -write path, and its idempotency claim.
// ---------------------------------------------------------------------------

const endToEndSrc = `@trigger(event="node.created", concept="v1:cognition:participant")
automation notifyJoin {
  step notify {
    /* pre-#2367 this read event.payload.legacy */
    spaceId: event.payload.spaceId
    who:     event.payload.userId
    label:   "joined \\"
    kind:    event.payload.kind
  }
}
`

func TestMigrateFileEndToEnd(t *testing.T) {
	out, report := migrateFile(endToEndSrc)

	for _, want := range []string{"spaceId", "userId", "kind"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not mention %q -- field not collected.\nreport:\n%s", want, report)
		}
		if !strings.Contains(out, "    "+want+" any\n") {
			t.Errorf("args block is missing %q any.\ngot:\n%s", want, out)
		}
	}
	if strings.Contains(report, "legacy") {
		t.Errorf("a read named only in a block comment reached the args block.\nreport:\n%s", report)
	}
	if strings.Contains(out, "spaceId: event.payload.spaceId") ||
		strings.Contains(out, "event.payload.kind") {
		t.Errorf("a payload read survived the rewrite.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "/* pre-#2367 this read event.payload.legacy */") {
		t.Errorf("the block comment was rewritten.\ngot:\n%s", out)
	}
	if !strings.Contains(out, `"joined \\"`) {
		t.Errorf("the literal was not passed through verbatim.\ngot:\n%s", out)
	}
	// The `kind:` read sits AFTER the `\\` literal. Note what actually rescues
	// it: with ONLY the one-byte lookback reverted this assertion still passes,
	// because endToEndSrc has no later quote and the newline bound (also added
	// here) stops the over-consumption at end of line. It fails under a full
	// pre-PR revert. So this pins the combination, not the lookback alone --
	// the isolated lookback cases are the two above.
	if !strings.Contains(out, "kind:    kind") {
		t.Errorf("the read after the `\\\\` literal was not rewritten.\ngot:\n%s", out)
	}
}

func TestMigrateFileIsIdempotent(t *testing.T) {
	once, _ := migrateFile(endToEndSrc)
	twice, report := migrateFile(once)
	if twice != once {
		t.Errorf("a second run changed the source.\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
	if report != "" {
		t.Errorf("a second run reported changes: %q", report)
	}
}

// READ THIS BEFORE TRUSTING THIS TEST: it cannot fail on any single-point
// change, and that is a property of the design rather than a gap to be closed
// here.
//
// "Terse automations are untouched" does not come from the `\s*\{` in
// automationHeader. It comes from terse automations having NO BRACES: even with
// automationHeader widened to match `automation t =>`, matchingBrace returns -1
// and migrateFile `continue`s at main.go:105. Measured both ways -- with the
// header widened to `\s*[{=]`, this test passes with the payload read present
// AND absent, and so does the whole suite.
//
// The fixture carries an event.payload read anyway, because without one
// migrateFile short-circuits at `len(fields) == 0` before terseness is even
// reachable, so the read is NECESSARY for the assertion to mean anything --
// just not SUFFICIENT to make it falsifiable. It would bite only under a change
// that made terse forms both header-matched and brace-extractable.
//
// Kept as an executable statement of the ADR requirement, not as a guard.
// Deleting it would be equally defensible; what is not defensible is reading a
// green run here as evidence the terse rule is enforced.
func TestMigrateFileLeavesTerseAutomationsAlone(t *testing.T) {
	src := "@trigger(event=\"node.created\", concept=\"v1:cluster:node\")\n" +
		"automation t => logic doThing(event.payload.spaceId)\n"
	out, report := migrateFile(src)
	if out != src {
		t.Errorf("a terse automation was rewritten.\ngot:\n%s", out)
	}
	if report != "" {
		t.Errorf("a terse automation was reported: %q", report)
	}
}
