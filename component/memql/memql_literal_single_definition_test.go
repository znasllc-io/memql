package memql

// memql_literal_single_definition_test.go -- memql#3192, the
// duplicate-definition half, for the two renderers this package carries: a
// tool handler's call literal (memqlCallLiteral, tool_handler_v1.go) and the
// authoring dry-run's preview statement (renderDryRunMemQLValue).
//
// Neither was a live %q defect when memql#3192 found them -- the handler's
// renderer was then the `$args.` substitution's encoder, json.Marshal with an
// unreachable %q fallback, and renderDryRunMemQLValue was json.Marshal
// outright. But both were separate DEFINITIONS of the MemQL escape set, and
// both fed text that is then parsed. A definition that is correct today and
// unowned drifts; memql#3035 is what that costs.
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

func TestRenderDryRunMemQLValue_IsTheOneDefinition(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		if got, want := renderDryRunMemQLValue(s), languageParser.QuoteString(s); got != want {
			t.Errorf("renderDryRunMemQLValue(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

// TestMemqlLiteralRenderers_OutputLexes checks both at the boundary that
// matters: whatever they emit must be readable by the lexer that reads it.
// (TestMemqlCallLiteral_IsTheOneDefinition holds the handler's renderer to
// QuoteString itself.)
func TestMemqlLiteralRenderers_OutputLexes(t *testing.T) {
	for _, s := range memqlLiteralFixtures {
		callLiteral, err := memqlCallLiteral(s)
		if err != nil {
			t.Fatalf("memqlCallLiteral(%#v): %v", s, err)
		}
		for name, got := range map[string]string{
			"memqlCallLiteral":       callLiteral,
			"renderDryRunMemQLValue": renderDryRunMemQLValue(s),
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
