package memql

// memql_literal_single_definition_test.go -- memql#3192, the
// duplicate-definition half, for the two renderers this package carries.
//
// Neither was a live %q defect: encodeForMemqlSubstitution encodes with
// json.Marshal and only falls back to %q if that fails (it cannot, for a
// string), and renderDryRunMemQLValue is json.Marshal outright. But both were
// separate DEFINITIONS of the MemQL escape set, and both fed text that is then
// parsed -- a tool handler's query and the authoring dry-run's preview
// statement. A definition that is correct today and unowned drifts; memql#3035
// is what that costs.
//
// The property pinned: there is ONE definition, langparser.QuoteString, and it
// lives beside the lexer whose escape set it targets.

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

var memqlLiteralFixtures = []string{
	"boom \x00 \a \v \t\n end",
	"",
	`quote " backslash \ slash /`,
	// The bytes the definitions disagreed on: HTML metacharacters, which
	// json.Marshal's default escaping unicode-escapes and QuoteString leaves
	// raw. Both parse to the same value; the point is that only one function
	// decides which.
	"<script> & </script>",
	strings.Repeat("\x1b", 4),
}

func TestEncodeForMemqlSubstitution_IsTheOneDefinition(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		if got, want := encodeForMemqlSubstitution(s), languageParser.QuoteString(s); got != want {
			t.Errorf("encodeForMemqlSubstitution(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

func TestRenderDryRunMemQLValue_IsTheOneDefinition(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		if got, want := renderDryRunMemQLValue(s), languageParser.QuoteString(s); got != want {
			t.Errorf("renderDryRunMemQLValue(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

// TestMemqlLiteralRenderers_OutputLexes checks both at the boundary that
// matters: whatever they emit must be readable by the lexer that reads it.
func TestMemqlLiteralRenderers_OutputLexes(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		for name, got := range map[string]string{
			"encodeForMemqlSubstitution": encodeForMemqlSubstitution(s),
			"renderDryRunMemQLValue":     renderDryRunMemQLValue(s),
		} {
			toks, err := languageParser.NewLexer(got).Tokenize()
			if err != nil {
				t.Fatalf("lexer rejected %s(%#v): %v", name, s, err)
			}
			if len(toks) == 0 || toks[0].Type != languageParser.TokenString || toks[0].Literal != s {
				t.Errorf("%s(%#v) did not round-trip: %#v", name, s, toks)
			}
		}
	}
}
