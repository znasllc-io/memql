package knowledge

// quote_string_single_definition_test.go -- memql#3192, the
// duplicate-definition half.
//
// quoteString was a hand-rolled escape table -- ", \, \n, \r, \t, everything
// else through raw -- behind ~50 call sites that build the statements this
// package executes. It was not a live parse defect: the lexer accepts a RAW
// control byte inside a literal, so passing one through worked by accident
// rather than by design.
//
// That accident is exactly the shape memql#3035 punished. A hand-rolled table
// is a second definition of the escape set, it is not owned by the lexer, and
// nothing made it move when the lexer's escape set did.
//
// The property pinned: there is ONE definition, langparser.QuoteString, and it
// lives beside the lexer whose escape set it targets.

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestQuoteString_IsTheOneDefinition(t *testing.T) {
	for _, s := range []string{
		"boom \x00 \a \v \t\r\n end",
		"",
		`quote " backslash \ slash /`,
		"<script> & </script>",
		"unicode  café  日本語",
		strings.Repeat("\x1b", 4),
	} {
		if got, want := quoteString(s), langparser.QuoteString(s); got != want {
			t.Errorf("quoteString(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

// TestQuoteString_OutputLexes checks the boundary that matters: whatever this
// emits must be readable by the lexer that reads the statements it builds, and
// must decode back to the bytes that went in.
func TestQuoteString_OutputLexes(t *testing.T) {
	for _, s := range []string{"boom \x00 \a \v end", `has " quote`, "tab\there"} {
		toks, err := langparser.NewLexer(quoteString(s)).Tokenize()
		if err != nil {
			t.Fatalf("lexer rejected quoteString(%#v): %v", s, err)
		}
		if len(toks) == 0 || toks[0].Type != langparser.TokenString || toks[0].Literal != s {
			t.Errorf("quoteString(%#v) did not round-trip: %#v", s, toks)
		}
	}
}
