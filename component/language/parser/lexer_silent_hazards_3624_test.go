package parser

import (
	"strings"
	"testing"
)

// The three silent lexer hazards of memql#3624: each scanned into something
// other than what was written, and returned no error while doing it. A silent
// misread is worse than a rejection, because nothing downstream can tell that
// it happened -- which is the property every test below pins.

// TestUnterminatedBlockCommentIsAnError locks hazard 1. skipBlockComment looped
// to EOF and returned normally, so a stray `/*` deleted the rest of the file:
// ParseFile returned only the definitions BEFORE it with err == nil. Strict
// boot cannot fail on a construct that never appeared, so a whole file's tail
// could go missing from a DSL bundle with no diagnostic anywhere.
func TestUnterminatedBlockCommentIsAnError(t *testing.T) {
	const src = "concept a { x string }\n\n/* note\nconcept b { y string }\n"

	if _, err := NewLexer(src).Tokenize(); err == nil {
		t.Fatal("Tokenize accepted an unterminated block comment; want an error")
	} else if !strings.Contains(err.Error(), "unterminated block comment") {
		t.Errorf("error = %q, want it to name the unterminated block comment", err)
	}

	f, err := ParseFile(src)
	if err == nil {
		n := -1
		if f != nil {
			n = len(f.Definitions)
		}
		t.Fatalf("ParseFile returned err == nil with %d definitions; "+
			"the tail of the file was dropped silently", n)
	}
}

// TestUnterminatedBlockCommentErrorNamesItsPosition keeps the diagnostic
// useful: sense's lexerDiagnostic extracts "at line N, column M" from the
// message and places the squiggle there. Without it the error lands at 1:1 --
// pointing at the top of the file rather than at the `/*` that ate it.
func TestUnterminatedBlockCommentErrorNamesItsPosition(t *testing.T) {
	_, err := NewLexer("concept a { x string }\n\n/* note\n").Tokenize()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "at line 3, column 1") {
		t.Errorf("error = %q, want it to point at line 3, column 1 (the `/*`)", err)
	}
}

// TestTerminatedBlockCommentsStillLex is the other side of hazard 1: only the
// UNTERMINATED form changes. Every terminated shape -- inline, multi-line,
// and one containing a bare `*` or `/` -- must lex exactly as before.
func TestTerminatedBlockCommentsStillLex(t *testing.T) {
	for _, src := range []string{
		"/* note */ concept a { x string }",
		"concept a { x string } /* note */",
		"concept a {\n/* multi\n   line\n   note */\nx string }",
		"/* has * and / inside **/ concept a { x string }",
		"/**/ concept a { x string }",
	} {
		toks, err := NewLexer(src).Tokenize()
		if err != nil {
			t.Errorf("Tokenize(%q): %v", src, err)
			continue
		}
		var got []string
		for _, tk := range toks {
			if tk.Type != TokenEOF {
				got = append(got, tk.Literal)
			}
		}
		want := "concept a { x string }"
		if strings.Join(got, " ") != want {
			t.Errorf("Tokenize(%q) = %q, want %q", src, strings.Join(got, " "), want)
		}
	}
}

// TestLeadingDotDecimalIsRejected locks hazard 2. `case '.'` routed `.` + digit
// to scanIdentifier, so `score > .5` compared against the STRING ".5" while
// `score > 0.5` compared against the float -- two spellings of one threshold
// that meant different things, silently.
//
// The fix REJECTS rather than accepting `.5` as a number. Accepting would have
// had to pick between a `.5` literal (which not every downstream number parser
// reads the same way) and normalising it to `0.5` (which makes the lexer
// rewrite source text, breaking the migrator's token-stream reconstruction).
// An error picks neither, cannot silently reinterpret any spelling that
// already exists, and costs the corpus nothing: `0.5` is already its only
// spelling for a fraction.
func TestLeadingDotDecimalIsRejected(t *testing.T) {
	for _, src := range []string{"score > .5", ".75", "x == .0", "foo().0"} {
		toks, err := NewLexer(src).Tokenize()
		if err == nil {
			var got []string
			for _, tk := range toks {
				if tk.Type != TokenEOF {
					got = append(got, tk.Literal)
				}
			}
			t.Errorf("Tokenize(%q) accepted a leading-dot number, lexed as %q; want an error",
				src, strings.Join(got, " "))
			continue
		}
		if !strings.Contains(err.Error(), "leading digit") {
			t.Errorf("Tokenize(%q) error = %q, want it to name the missing leading digit", src, err)
		}
	}
}

// TestLeadingDotDecimalIsTheSameValueWhenSpelledWithZero is the differential
// half: the two spellings that must mean the same thing now agree, because one
// of them no longer exists. `0.5` keeps lexing as a number.
func TestLeadingDotDecimalIsTheSameValueWhenSpelledWithZero(t *testing.T) {
	toks, err := NewLexer("score > 0.5").Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	var num *Token
	for i := range toks {
		if toks[i].Type == TokenNumber {
			num = &toks[i]
		}
	}
	if num == nil || num.Literal != "0.5" {
		t.Fatalf("want a number token \"0.5\", got %+v", toks)
	}
}

// TestDottedPathTailStillLexes guards what the rejected branch sits next to.
// A `.` that STARTS a token and is followed by a letter or `_` is a dotted-path
// tail (`foo().payload.value`) and must keep scanning as one identifier; only
// the digit case is refused.
func TestDottedPathTailStillLexes(t *testing.T) {
	cases := map[string]string{
		"foo().payload.value": ".payload.value",
		"foo()._private":      "._private",
	}
	for src, want := range cases {
		toks, err := NewLexer(src).Tokenize()
		if err != nil {
			t.Errorf("Tokenize(%q): %v", src, err)
			continue
		}
		last := toks[len(toks)-2] // -1 is EOF
		if last.Type != TokenIdentifier || last.Literal != want {
			t.Errorf("Tokenize(%q) tail = (%v %q), want (identifier %q)", src, last.Type, last.Literal, want)
		}
	}
	// A numeric segment INSIDE a path is scanned by scanIdentifier, not by the
	// `case '.'` this fix touches, so it is deliberately unaffected.
	toks, err := NewLexer("args.items.0").Tokenize()
	if err != nil {
		t.Fatalf("Tokenize(args.items.0): %v", err)
	}
	if toks[0].Literal != "args.items.0" {
		t.Errorf("args.items.0 lexed as %q, want one identifier", toks[0].Literal)
	}
	// A standalone `.` (the `use path.{ names }` separator) is untouched.
	toks, err = NewLexer("use cognition.shapes.{ spaceCard }").Tokenize()
	if err != nil {
		t.Fatalf("Tokenize(use ...): %v", err)
	}
	var sawDot bool
	for _, tk := range toks {
		if tk.Type == TokenDot {
			sawDot = true
		}
	}
	if !sawDot {
		t.Error("use path.{ names } lost its separator '.'")
	}
}

// TestSurrogatePairDecodesToOneRune locks hazard 3. scanUnicodeEscape returned
// each half of a surrogate pair as a lone surrogate, and strings.Builder
// substitutes U+FFFD for those -- so an escaped emoji produced TWO replacement
// characters instead of one rune. The LITERAL emoji form was always fine; only
// the escaped form corrupted, which is why it went unnoticed.
func TestSurrogatePairDecodesToOneRune(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"\"\\ud83d\\ude00\"", "\U0001F600"},                         // grinning face
		{"\"\\uD83D\\uDE00\"", "\U0001F600"},                         // upper-case hex
		{"\"a\\ud83d\\ude00b\"", "a\U0001F600b"},                     // surrounded by BMP text
		{"\"\\ud83d\\ude00\\ud83d\\ude00\"", "\U0001F600\U0001F600"}, // back to back
		{"\"\\u0041\"", "A"},                                         // BMP escape unchanged
		{"\"\\u00e9\"", "\u00e9"},                                    // BMP escape unchanged
	}
	for _, c := range cases {
		toks, err := NewLexer(c.src).Tokenize()
		if err != nil {
			t.Errorf("Tokenize(%s): %v", c.src, err)
			continue
		}
		got := toks[0].Literal
		if got != c.want {
			t.Errorf("Tokenize(%s) = %q (%d runes), want %q (%d runes)",
				c.src, got, len([]rune(got)), c.want, len([]rune(c.want)))
		}
		if strings.ContainsRune(got, '\uFFFD') {
			t.Errorf("Tokenize(%s) produced U+FFFD replacement characters: %q", c.src, got)
		}
	}
}

// TestSurrogatePairKeepsPositionsExact guards the memql#3047 property while a
// single escape now consumes twice as many source runes: End* is the
// authoritative source-span record, so it must still count every character the
// pair occupied.
func TestSurrogatePairKeepsPositionsExact(t *testing.T) {
	src := "\"\\ud83d\\ude00\" x"
	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	// The string literal spans columns 1..15 (14 source runes, half-open end).
	if toks[0].Column != 1 || toks[0].EndCol != 15 {
		t.Errorf("string span = [%d,%d), want [1,15)", toks[0].Column, toks[0].EndCol)
	}
	// ...so the identifier after it starts at column 16.
	if toks[1].Literal != "x" || toks[1].Column != 16 {
		t.Errorf("next token = (%q col %d), want (\"x\" col 16)", toks[1].Literal, toks[1].Column)
	}
}

// TestLoneSurrogateIsAnError is the other half of hazard 3: half of a pair with
// no mate is not a character, and silently substituting U+FFFD for it is the
// same silent corruption in a different costume.
func TestLoneSurrogateIsAnError(t *testing.T) {
	for _, src := range []string{
		"\"\\ud83d\"",        // high surrogate, no low
		"\"\\ude00\"",        // low surrogate, no high
		"\"\\ud83dx\"",       // high surrogate followed by ordinary text
		"\"\\ud83d\\u0041\"", // high surrogate followed by a NON-surrogate escape
		"\"\\udc00\\ud83d\"", // the pair in the wrong order
	} {
		toks, err := NewLexer(src).Tokenize()
		if err == nil {
			t.Errorf("Tokenize(%s) accepted a lone surrogate, produced %q; want an error", src, toks[0].Literal)
			continue
		}
		if !strings.Contains(err.Error(), "surrogate") {
			t.Errorf("Tokenize(%s) error = %q, want it to name the surrogate", src, err)
		}
	}
}

// TestLiteralAstralCharacterStillLexes records what was never broken, so a
// future change to the escape path cannot quietly break it: an astral
// character written literally is ordinary input to a rune-oriented lexer.
func TestLiteralAstralCharacterStillLexes(t *testing.T) {
	toks, err := NewLexer("\"\U0001F600 ok\"").Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if got := toks[0].Literal; got != "\U0001F600 ok" {
		t.Errorf("literal emoji = %q, want %q", got, "\U0001F600 ok")
	}
}
