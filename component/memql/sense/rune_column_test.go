package sense

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// memql#2788: a Sense column counts RUNES -- cmd/memql-lsp/internal/position
// states the contract outright ("the lexer scans a []rune and does one
// column++ per rune"). Several authoring rules derived columns from BYTE
// offsets (regexp index functions, byte-wise scan loops) or widths from
// len(identifier), so any non-ASCII earlier on the line pushed the squiggle
// right by one position per extra UTF-8 byte.
//
// Non-ASCII on a filter line is ordinary, and it is not confined to string
// literals: the lexer admits identifiers via unicode.IsLetter
// (component/language/parser/lexer.go:875), so `naïve` is a legal field name.
//
// Each case below pairs an ASCII line with the SAME line carrying a
// multi-byte rune before the flagged token. The reported column must shift by
// the rune count, never the byte count.

// wantRuneColumn is the column a rune-counting implementation must report for
// the first occurrence of tok in line.
func wantRuneColumn(t *testing.T, line, tok string) int {
	t.Helper()
	i := strings.Index(line, tok)
	if i < 0 {
		t.Fatalf("fixture error: %q not found in %q", tok, line)
	}
	return utf8.RuneCountInString(line[:i]) + 1
}

func TestArraySyntaxRuleColumnsCountRunes(t *testing.T) {
	// The multi-byte rune sits inside a string literal. stripStringsAndComments
	// blanks a string byte-for-byte, so the byte offset survives while the rune
	// offset does not -- exactly the divergence this guards.
	line := `  items string @default("ü") array(string)`
	got := arraySyntaxRule(line)
	if len(got) != 1 {
		t.Fatalf("want one deprecated-array-syntax hint, got %+v", got)
	}
	want := wantRuneColumn(t, line, "array(")
	if got[0].Range.Start.Column != want {
		t.Errorf("start column = %d, want %d (rune column; a byte column would be %d)",
			got[0].Range.Start.Column, want, strings.Index(line, "array(")+1)
	}
	// The end must span the match in runes too.
	if wantEnd := want + utf8.RuneCountInString("array(string)"); got[0].Range.End.Column != wantEnd {
		t.Errorf("end column = %d, want %d", got[0].Range.End.Column, wantEnd)
	}
}

func TestDiscardedArgsDescriptionsColumnsCountRunes(t *testing.T) {
	line := `    name string @default("ü") @description("doc")`
	src := "query q {\n  args {\n" + line + "\n  }\n}\n"
	got := DiscardedArgsDescriptions(src)
	if len(got) != 1 {
		t.Fatalf("want one discarded-args-description hint, got %+v", got)
	}
	want := wantRuneColumn(t, line, "@description")
	if got[0].Range.Start.Column != want {
		t.Errorf("start column = %d, want %d (rune column; a byte column would be %d)",
			got[0].Range.Start.Column, want, strings.Index(line, "@description")+1)
	}
}

func TestActorRuleColumnsCountRunes(t *testing.T) {
	// The issue's own repro shape: a non-ASCII value earlier in the filter.
	line := `  filter naive=="ü" && ownerUserId == actor.userId`
	src := "@description(\"owned\")\nquery todo todos {\n" + line + "\n}\n"
	got := actorUndeclaredRule(src)
	if len(got) != 1 {
		t.Fatalf("want one actor-undeclared Error, got %+v", got)
	}
	want := wantRuneColumn(t, line, "actor.")
	if got[0].Range.Start.Line != 3 || got[0].Range.Start.Column != want {
		t.Errorf("anchor = %d:%d, want 3:%d (rune column; a byte column would be %d)",
			got[0].Range.Start.Line, got[0].Range.Start.Column, want,
			strings.Index(line, "actor.")+1)
	}
}

// The unknown-property rule shares the same byte-counting scan loop, so it
// needs its own guard: a non-ASCII value earlier on the line must not shift
// the anchor. (A non-ASCII member NAME is a different matter -- the shared
// ref extractor is ASCII-only by pattern, tracked separately.)
func TestActorUnknownPropertyColumnsCountRunes(t *testing.T) {
	line := `  filter note=="ü" && ownerUserId == actor.bogusProp`
	src := "@actor\nquery todo todos {\n" + line + "\n}\n"
	got := actorUnknownPropertyRule(src)
	if len(got) != 1 {
		t.Fatalf("want one actor-unknown-property Error, got %+v", got)
	}
	want := wantRuneColumn(t, line, "bogusProp")
	if got[0].Range.Start.Column != want {
		t.Errorf("start column = %d, want %d (rune column; a byte column would be %d)",
			got[0].Range.Start.Column, want, strings.Index(line, "bogusProp")+1)
	}
	if wantEnd := want + utf8.RuneCountInString("bogusProp"); got[0].Range.End.Column != wantEnd {
		t.Errorf("end column = %d, want %d", got[0].Range.End.Column, wantEnd)
	}
}

// The offset walks decode runes rather than testing utf8.RuneStart. On INVALID
// UTF-8 the two disagree: RuneStart is false for every continuation byte
// (0x80-0xBF), so such a byte would contribute zero columns, while []rune --
// what the lexer actually scans, and therefore the contract -- yields one
// U+FFFD per bad byte. Counting rune starts would drift the squiggle LEFT of
// where the author sees it, which is worse than the byte-counting bug this
// change fixes. Editors can and do hand the server invalid UTF-8.
func TestActorRuleColumnsSurviveInvalidUTF8(t *testing.T) {
	// 0x97 and 0x93 are lone CP1252 bytes: invalid UTF-8, one replacement
	// rune each to []rune.
	line := "  filter note==\"a\x97b\x93c\" && ownerUserId == actor.userId"
	src := "@description(\"owned\")\nquery todo todos {\n" + line + "\n}\n"
	got := actorUndeclaredRule(src)
	if len(got) != 1 {
		t.Fatalf("want one actor-undeclared Error, got %+v", got)
	}
	// The oracle is []rune, not our own arithmetic.
	idx := strings.Index(line, "actor.")
	want := len([]rune(line[:idx])) + 1
	if got[0].Range.Start.Column != want {
		t.Errorf("column = %d, want %d ([]rune contract; counting rune-start bytes would give %d)",
			got[0].Range.Start.Column, want, want-2)
	}
}

// findInSource anchors several rules through positionFromOffset. That is the
// shared conversion, so it has to count runes too -- otherwise a rule gets a
// byte-based Start with a rune-based End and reports a mixed-unit range.
func TestFindInSourceColumnsCountRunes(t *testing.T) {
	line := `  x := coalesce("naïve", sort("a"))`
	src := "logic l {\n  body {\n" + line + "\n  }\n}\n"
	pos := findInSource(src, "sort(")
	want := wantRuneColumn(t, line, "sort(")
	if pos.Line != 3 || pos.Column != want {
		t.Errorf("findInSource = %d:%d, want 3:%d (rune column; a byte column would be %d)",
			pos.Line, pos.Column, want, strings.Index(line, "sort(")+1)
	}
}

func TestRuneColumnBoundaries(t *testing.T) {
	const line = "aüb" // 4 bytes, 3 runes
	for _, tc := range []struct{ off, want int }{
		{-5, 1}, {0, 1}, {1, 2}, {3, 3}, {4, 4}, {100, 4},
	} {
		if got := runeColumn(line, tc.off); got != tc.want {
			t.Errorf("runeColumn(%q, %d) = %d, want %d", line, tc.off, got, tc.want)
		}
	}
	if got := runeWidth("naïve"); got != 5 {
		t.Errorf("runeWidth(\"naïve\") = %d, want 5", got)
	}
}
