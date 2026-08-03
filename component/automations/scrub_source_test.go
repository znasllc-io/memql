package automations

import (
	"strings"
	"testing"
)

// scrub_source_test.go pins the two arms of scrubSourceForPayloadScan that
// diverged from the language (memql#2949). The scrubber blanks string literals
// and both comment forms so the G5 retirement scan never fires on prose; when
// its model of a literal is wrong it does not error, it UNDER-reports -- it
// blanks source that was never inside a literal, and the gate stops seeing the
// reads it exists to refuse.
//
// Neither arm is reachable through compileMemQL on the current tree, which is
// why both went unnoticed. These drive the scrubber directly.

// TestScrubTerminatesOnEscapedBackslash covers the first arm. The escape scan
// used to look one byte back (`s[j-1] == '\\'`), which cannot tell an escaped
// quote from a quote following a COMPLETED `\\` escape. A literal ending in a
// backslash pair therefore read its real closing quote as escaped and kept
// consuming into the following source.
func TestScrubTerminatesOnEscapedBackslash(t *testing.T) {
	src := `x: "a path ending in a backslash \\"
y: "this must not be inside the previous literal"
z: kept`

	got := scrubSourceForPayloadScan(src)

	if !strings.Contains(got, "y:") {
		t.Errorf("the literal ending in `\\\\` swallowed the `y:` that followed it -- "+
			"the escape scan treats the closing quote as escaped, so everything up to "+
			"the NEXT quote is silently blanked and a scan over this view under-reports.\n"+
			"source:\n%s\n\nscrubbed:\n%s", src, got)
	}
	if !strings.Contains(got, "z: kept") {
		t.Errorf("source after the literal was consumed:\n%s", got)
	}
	if strings.Contains(got, "a path ending") {
		t.Errorf("the literal body itself was NOT blanked, so the scrubber stopped "+
			"doing its job:\n%s", got)
	}
	if strings.Contains(got, "this must not be inside") {
		t.Errorf("the second literal's body survived -- it must be blanked as a "+
			"literal in its own right:\n%s", got)
	}
}

// TestScrubStringArmStopsAtNewline covers the second arm: an UNBALANCED quote
// -- one with no closing quote anywhere before EOF -- must cost its own line
// rather than blanking the rest of the file.
//
// It is deliberately scoped to the unbalanced case. A literal that DOES close,
// even several lines later, is valid and must be consumed whole; see
// TestScrubConsumesMultiLineLiteral. An earlier version of this test asserted
// the string arm stops at a newline unconditionally, on the stated grounds that
// "a string literal may not span lines anywhere else in the grammar". That is
// false -- the lexer's scanString has no newline case -- and the arm built on it
// regressed the gate in both directions (memql#2949 review).
//
// Note the lexer refuses unterminated source outright, so compileMemQL never
// reaches the scan with input like this; the behaviour is belt-and-braces for
// callers scanning source the parser has not accepted.
func TestScrubStringArmStopsAtNewline(t *testing.T) {
	src := `a: "unterminated
b: kept
c: also kept`

	got := scrubSourceForPayloadScan(src)

	if !strings.Contains(got, "b: kept") || !strings.Contains(got, "c: also kept") {
		t.Errorf("a single unbalanced quote swallowed the rest of the file -- the string "+
			"arm must stop at a newline like the comment arm does, because the grammar "+
			"does not permit a literal to span lines.\nsource:\n%s\n\nscrubbed:\n%s", src, got)
	}
	if strings.Contains(got, "unterminated") {
		t.Errorf("the unterminated literal's body survived on its own line:\n%s", got)
	}
}

// TestScrubConsumesMultiLineLiteral pins the regression a newline-stopping
// string arm introduced, in BOTH directions (memql#2949 review).
//
// The lexer accepts a literal spanning lines -- `scanString` in
// component/language/parser/lexer.go has no newline case, so `"one<NL>two"` is
// ONE string token. An arm that stopped at `\n` therefore:
//
//   - scanned the literal's 2nd..nth lines as CODE, so prose in a wrapped
//     @description tripped the G5 gate and the diagnostic blamed the automation
//     for a read that exists only inside a string (false refusal -- byte-for-byte
//     the class the `/*` arm was added to fix, memql#2872); and
//   - re-read the literal's real closing quote as an OPENING quote, blanking the
//     genuine source after it, so a real retired read went UNSEEN (fail-open --
//     the very thing this scrubber exists to prevent).
//
// Both are asserted here through eventPayloadReadPattern, the actual consumer,
// because the consequence is what matters rather than the intermediate bytes.
func TestScrubConsumesMultiLineLiteral(t *testing.T) {
	t.Run("prose inside a wrapped literal is not scanned as code", func(t *testing.T) {
		src := "@description(\"before #2367 this read\nevent.payload.status directly\")\nautomation a { }"

		got := scrubSourceForPayloadScan(src)

		if eventPayloadReadPattern.MatchString(got) {
			t.Errorf("the G5 scan fired on prose inside a MULTI-LINE literal.\n"+
				"The lexer accepts a literal spanning lines, so the whole literal must be "+
				"blanked; stopping at the newline leaks its tail as code and refuses an "+
				"automation for a read that only exists inside a string.\nsource:\n%s\n\nscrubbed:\n%s",
				src, got)
		}
		if !strings.Contains(got, "automation a { }") {
			t.Errorf("source after the multi-line literal was consumed:\n%s", got)
		}
	})

	t.Run("a real read after a wrapped literal is still seen", func(t *testing.T) {
		src := "d: \"one\ntwo\" status: event.payload.x"

		got := scrubSourceForPayloadScan(src)

		if !eventPayloadReadPattern.MatchString(got) {
			t.Errorf("the G5 scan MISSED a genuine retired read following a multi-line "+
				"literal -- the gate failed OPEN.\nStopping at the newline makes the "+
				"literal's real closing quote read as an OPENING quote, so the live code "+
				"after it is blanked.\nsource:\n%s\n\nscrubbed:\n%s", src, got)
		}
	})

	t.Run("line accounting survives a wrapped literal", func(t *testing.T) {
		src := "a: \"one\ntwo\nthree\"\nz: kept\n"

		got := scrubSourceForPayloadScan(src)

		if want, have := strings.Count(src, "\n"), strings.Count(got, "\n"); want != have {
			t.Errorf("line count changed from %d to %d across a multi-line literal:\n%s", want, have, got)
		}
		if len(got) != len(src) {
			t.Errorf("length changed from %d to %d, so the view is no longer a positional overlay", len(src), len(got))
		}
	})
}

// TestScrubKeepsEscapedQuotesInsideLiterals is the direction that keeps the fix
// honest: a genuinely escaped quote must still NOT terminate the literal.
func TestScrubKeepsEscapedQuotesInsideLiterals(t *testing.T) {
	src := `msg: "she said \"hi\" twice" tail: kept`

	got := scrubSourceForPayloadScan(src)

	if strings.Contains(got, "twice") {
		t.Errorf("an escaped quote terminated the literal early, so its tail leaked "+
			"through unscrubbed:\n%s", got)
	}
	if !strings.Contains(got, "tail: kept") {
		t.Errorf("source after the literal was consumed:\n%s", got)
	}
}

// TestScrubPreservesLength pins the property every arm shares: the scrubbed view
// is a byte-for-byte overlay of the source, so offsets a caller derives from a
// match still index the original.
func TestScrubPreservesLength(t *testing.T) {
	for _, src := range []string{
		`x: "a path ending in a backslash \\"` + "\n" + `y: "second"`,
		"a: \"unterminated\nb: kept\n",
		`msg: "she said \"hi\" twice"`,
		"// line comment\n/* block\ncomment */\nreal: code",
		`unterminated: "runs to EOF`,
		"/* unterminated block comment",
	} {
		if got := scrubSourceForPayloadScan(src); len(got) != len(src) {
			t.Errorf("scrubbed view is %d bytes for a %d-byte source, so it is no longer "+
				"a positional overlay:\nsource:\n%s\n\nscrubbed:\n%s",
				len(got), len(src), src, got)
		}
	}
}

// TestScrubNewlineAccountingIsUnchanged pins the invariant the block-comment arm
// already documents: blanking must not change the line count, or a diagnostic
// derived from the scrubbed view points at the wrong line.
func TestScrubNewlineAccountingIsUnchanged(t *testing.T) {
	for _, src := range []string{
		`x: "a path ending in a backslash \\"` + "\n" + `y: "second"` + "\n" + `z: kept`,
		"a: \"unterminated\nb: kept\nc: kept\n",
		"/* block\ncomment */\nreal: code\n",
		"// line\nreal: code\n",
	} {
		got := scrubSourceForPayloadScan(src)
		if want, have := strings.Count(src, "\n"), strings.Count(got, "\n"); want != have {
			t.Errorf("scrubbing changed the line count from %d to %d:\nsource:\n%s\n\nscrubbed:\n%s",
				want, have, src, got)
		}
	}
}

// TestScrubbedViewStillFeedsTheG5Gate is the consequence test: the scrubber
// exists to give the retired-`event.payload` scan a prose-free view, so a REAL
// read sitting after a backslash-terminated literal must still be seen. With the
// over-consuming escape scan the read was blanked along with the source between
// the two quotes, and the gate failed open.
func TestScrubbedViewStillFeedsTheG5Gate(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "read after a literal ending in a backslash pair",
			src: `path: "C:\\"
status: event.payload.status`,
			want: true,
		},
		{
			name: "read after an unterminated literal",
			src: `path: "oops
status: event.payload.status`,
			want: true,
		},
		{
			name: "read inside a literal is still prose",
			src:  `note: "this once read event.payload.status"`,
			want: false,
		},
		{
			name: "read inside a comment is still prose",
			src:  `/* this once read event.payload.status */`,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eventPayloadReadPattern.MatchString(scrubSourceForPayloadScan(tc.src))
			if got != tc.want {
				t.Errorf("G5 scan over the scrubbed view = %v, want %v -- the gate %s.\n"+
					"source:\n%s\n\nscrubbed:\n%s", got, tc.want,
					map[bool]string{true: "fired on prose", false: "failed open on a real read"}[got],
					tc.src, scrubSourceForPayloadScan(tc.src))
			}
		})
	}
}
