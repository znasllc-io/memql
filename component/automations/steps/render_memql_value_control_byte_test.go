package steps

// render_memql_value_control_byte_test.go -- regression guards for memql#3192,
// the memql#3035 defect on the automation step path.
//
// A function step's resolved args are stringified into query text by
// renderMemQLValue and re-parsed by engine.Execute. The string case rendered
// with fmt.Sprintf("%q"), and Go's %q does not agree with the escape set
// scanString implements. scanString knows the JSON escapes and only those --
// `" \ / b f n r t u` -- and anything else is a HARD ERROR, `invalid escape
// character %q at position %d`. %q emits `\x00`, `\a` and `\v`.
//
// So a single control byte in a string arg -- resolved from a prior step
// result or an event payload, i.e. arbitrary text -- made the WHOLE call
// unparseable and the step failed with an error about the query text rather
// than anything to do with the data.
//
// The contract pinned here: whatever bytes go in, the rendered call parses
// through the langparser (the engine's sole runtime parser) and the arg
// round-trips byte-for-byte.

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// controlByteFixture carries one of each byte %q escapes in a form the lexer
// rejects: NUL (\x00), BEL (\a) and VT (\v). Plus a tab and a newline, which
// both encoders handle, so a regression cannot hide behind them.
const controlByteFixture = "boom \x00 \a \v \t\n end"

// TestRenderMemQLValue_ControlByteLexes drives renderMemQLValue's output
// through the REAL lexer. Against %q this fails with `invalid escape
// character 'x'`.
func TestRenderMemQLValue_ControlByteLexes(t *testing.T) {
	rendered := renderMemQLValue(controlByteFixture)

	toks, err := langparser.NewLexer(rendered).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the rendered literal (the #3192 defect): %v\nrendered: %#v", err, rendered)
	}

	var got string
	var found bool
	for _, tok := range toks {
		if tok.Type == langparser.TokenString {
			got, found = tok.Literal, true
			break
		}
	}
	if !found {
		t.Fatalf("no string token in %#v", rendered)
	}
	if got != controlByteFixture {
		t.Errorf("literal did not round-trip:\n  got:  %#v\n  want: %#v", got, controlByteFixture)
	}
}

// TestRenderFunctionArgs_ControlByteParses pins the serialize->re-parse
// boundary the step executor actually crosses: the query text a function step
// builds for a logic call whose string arg carries a control byte must PARSE.
func TestRenderFunctionArgs_ControlByteParses(t *testing.T) {
	args := map[string]any{
		"message": controlByteFixture,
		"tags":    []string{"a\vb"},
		// A control byte in a map KEY is the same defect one position over:
		// object keys render as MemQL string literals too.
		"row": map[string]any{"note": "x\x00y", "k\vey": 1},
	}

	query := "logicStampError(" + renderFunctionArgs(args) + ")"
	parsed, err := langparser.ParseExpression(query)
	if err != nil {
		t.Fatalf("re-parse of the rendered logic call failed (the #3192 defect): %v\nquery: %#v", err, query)
	}
	call, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", parsed)
	}
	if call.Args["message"] != controlByteFixture {
		t.Errorf("message arg did not round-trip: %#v", call.Args["message"])
	}
	tags, ok := call.Args["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "a\vb" {
		t.Errorf("tags arg did not round-trip: %#v", call.Args["tags"])
	}
	row, ok := call.Args["row"].(map[string]any)
	if !ok || row["note"] != "x\x00y" {
		t.Errorf("row arg did not round-trip: %#v", call.Args["row"])
	}
}

// TestRenderPositionalArg_ControlByteParses covers the positional-storage
// branch (S9/#2407): a builtin call whose args compiled to numeric keys goes
// through renderPositionalArg, which is a second entry point into the same
// string rendering.
func TestRenderPositionalArg_ControlByteParses(t *testing.T) {
	args := map[string]any{"0": "plain", "1": controlByteFixture, "2": "tail"}

	query := "cond(" + renderFunctionArgs(args) + ")"
	if _, err := langparser.ParseExpression(query); err != nil {
		t.Fatalf("re-parse of the rendered positional call failed: %v\nquery: %#v", err, query)
	}
}

// TestRenderMemQLValue_MatchesQuoteString pins the single-definition rule: the
// step renderer must not carry its own idea of the MemQL escape set. There is
// exactly one correct definition -- langparser.QuoteString, which lives beside
// the lexer whose escape set it targets -- and a renderer that reimplements it
// drifts the moment that set changes. That drift is what #3035 cost.
func TestRenderMemQLValue_MatchesQuoteString(t *testing.T) {
	for _, s := range []string{
		controlByteFixture,
		"",
		`quote " and backslash \ and slash /`,
		"<html> & \"amp\"",
		"unicode    café",
		strings.Repeat("\x1b", 8),
	} {
		if got, want := renderMemQLValue(s), langparser.QuoteString(s); got != want {
			t.Errorf("renderMemQLValue(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}
