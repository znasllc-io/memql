package steps

// json_string_single_definition_test.go -- memql#3192, the duplicate-definition
// half.
//
// jsonString was a SECOND definition of the MemQL escape set in this package:
// json.Marshal with HTML escaping left on, feeding the step-execution record's
// insert. It was never a live parse defect -- json.Marshal emits exactly the
// escapes scanString implements -- but a second definition is what memql#3035
// showed is expensive, and this one sat in the same package as
// renderMemQLValue, which had the %q bug.
//
// The property pinned: there is ONE definition, langparser.QuoteString, and it
// lives beside the lexer whose escape set it targets.

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestJSONString_IsTheOneDefinition(t *testing.T) {
	for _, s := range []string{
		controlByteFixture,
		"",
		`quote " backslash \ slash /`,
		// The bytes the two definitions disagreed on: HTML metacharacters,
		// which json.Marshal's default escaping unicode-escapes and
		// QuoteString leaves raw. Both parse to the same value; the point is
		// that only one function decides.
		"<script> & </script>",
		strings.Repeat("\x1b", 4),
	} {
		if got, want := jsonString(s), langparser.QuoteString(s); got != want {
			t.Errorf("jsonString(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

// TestJSONString_OutputLexes checks the helper's output at the boundary that
// matters. Every string literal RecordStepExecution puts into its insert comes
// from jsonString, and that function needs a live engine to call, so the
// lexer check is applied to the helper directly.
func TestJSONString_OutputLexes(t *testing.T) {
	for _, s := range []string{controlByteFixture, "<script> & </script>", `"` + `\`} {
		toks, err := langparser.NewLexer(jsonString(s)).Tokenize()
		if err != nil {
			t.Fatalf("lexer rejected jsonString(%#v): %v", s, err)
		}
		if len(toks) == 0 || toks[0].Type != langparser.TokenString || toks[0].Literal != s {
			t.Errorf("jsonString(%#v) did not round-trip: %#v", s, toks)
		}
	}
}
