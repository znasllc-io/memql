package steps

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#3099: renderMemQLValue rendered string step args with fmt's %q, and Go
// and the MemQL lexer do not agree on the escape set.
//
// %q emits \x00, \a and \v. lexer.go's readString implements the JSON escapes
// and ONLY those (" \ / b f n r t u), so it does not know \x, \a or \v -- and
// it treats an unknown escape as a HARD PARSE ERROR, not a fallback. One
// control byte in a step arg therefore made the whole generated statement fail
// to parse.
//
// These values are resolved from prior step results and event payloads, which
// is arbitrary text by construction -- the same shape as the outbound row that
// memql#3035 got permanently stuck. This is the closest analogue of that bug,
// and the reason the issue named this site first.
//
// The assertions drive the REAL lexer rather than comparing strings, because
// the property under test is "does the engine accept what we generated", and
// only the lexer can answer that.

// lexes reports whether src is accepted by the real MemQL lexer.
func lexes(t *testing.T, src string) error {
	t.Helper()
	_, err := langparser.NewLexer(src).Tokenize()
	return err
}

func TestRenderMemQLValueEmitsLiteralsTheLexerAccepts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		// The three escapes %q emits and the lexer refuses. Each is its own
		// case so a partial regression names which byte came back.
		{"NUL", "boom \x00 end"},
		{"BEL", "boom \a end"},
		{"vertical tab", "boom \v end"},
		{"all three together", "boom \x00 \a \v end"},
		// Escapes both agree on -- these must keep working, or a fix that
		// mangled everything would pass the cases above.
		{"quote", `he said "hi"`},
		{"backslash", `C:\path\to`},
		{"newline and tab", "line\nnext\tcol"},
		{"multi-byte rune", "café ✓ 日本語"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := renderMemQLValue(tc.value)

			// A bare literal is not a statement, so wrap it in one the lexer
			// will walk. The lexer is what rejects the escape, so this is
			// enough to reproduce the defect.
			if err := lexes(t, "query x { filter a=="+rendered+" }"); err != nil {
				t.Errorf("renderMemQLValue produced a literal the MemQL lexer refuses: %v\n"+
					"  input:    %q\n  rendered: %s\n\n"+
					"fmt's %%q and the lexer disagree on the escape set, and the lexer treats an "+
					"unknown escape as a parse error. Render through "+
					"langparser.QuoteString, which lives beside the lexer it targets (memql#3099).",
					err, tc.value, rendered)
			}
		})
	}
}

// The converse of the round-trip: the rendered literal must still MEAN the
// input. A quoter that satisfied the lexer by dropping bytes would pass the
// test above.
func TestRenderMemQLValueRoundTripsThroughTheLexer(t *testing.T) {
	for _, value := range []string{
		"boom \a \v end",
		`he said "hi"`,
		`C:\path\to`,
		"line\nnext\tcol",
		"café ✓",
	} {
		rendered := renderMemQLValue(value)
		toks, err := langparser.NewLexer(rendered).Tokenize()
		if err != nil {
			t.Errorf("%q rendered to %s, which the lexer refuses: %v", value, rendered, err)
			continue
		}
		if len(toks) == 0 || toks[0].Type != langparser.TokenString {
			t.Errorf("%q rendered to %s, which did not lex as a single string token", value, rendered)
			continue
		}
		tok := toks[0]
		if tok.Literal != value {
			t.Errorf("round-trip changed the value.\n  in:  %q\n  out: %q\n  rendered: %s",
				value, tok.Literal, rendered)
		}
	}
}

// A runtime reference must stay unquoted -- the evaluator resolves it. Pinned
// because the fix touches the same switch arm, one line below the guard.
func TestRenderMemQLValueLeavesRuntimeReferencesUnquoted(t *testing.T) {
	for _, ref := range []string{"args.foo", "steps.prior.result"} {
		if !isRuntimeReference(ref) {
			continue // not a reference on this build; the guard decides, not this test
		}
		if got := renderMemQLValue(ref); strings.HasPrefix(got, `"`) {
			t.Errorf("runtime reference %q was quoted to %s -- the evaluator can no longer resolve it", ref, got)
		}
	}
}
