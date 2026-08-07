package parser

// reconstruct_tokens_control_byte_test.go -- regression guard for memql#3192
// inside the parser itself.
//
// reconstructTokens rebuilds a mutation's PayloadRaw from already-lexed
// tokens, re-adding the quotes the lexer stripped. It re-added them with
// fmt.Sprintf("%q") -- an escape set this very package's lexer does not
// implement -- and PayloadRaw is parsed AGAIN downstream
// (parsePayloadRawToTemplate, the compiler's parsePayloadRaw, the step
// executor's parseAndEvaluateObjectLiteral).
//
// So the round trip lexer -> reconstructTokens -> lexer was not a round trip
// at all for a decoded control byte: scanString turns `\u0007` into a BEL,
// %q turned that BEL back into `\a`, and scanString rejects `\a`. A literal
// this package accepted could not be re-read by this package.

import "testing"

func TestReconstructTokens_ControlByteRoundTrips(t *testing.T) {
	// The exact shape a payload literal takes after lexing: an authored
	// \u00XX escape has already been DECODED into a raw control byte, so the
	// token literal carries the byte, not the escape.
	const authored = "{ note: \"boom \\u0000 \\u0007 \\u000b end\" }"

	toks, err := NewLexer(authored).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the authored literal: %v", err)
	}

	p := &Parser{tokens: toks}
	rebuilt := p.reconstructTokens(0, len(toks))

	reToks, err := NewLexer(rebuilt).Tokenize()
	if err != nil {
		t.Fatalf("reconstructed payload does not lex (the #3192 defect): %v\nrebuilt: %#v", err, rebuilt)
	}

	got := stringLiterals(reToks)
	want := stringLiterals(toks)
	if len(got) != len(want) {
		t.Fatalf("literal count changed: got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("literal %d did not round-trip:\n  got:  %#v\n  want: %#v", i, got[i], want[i])
		}
	}
}

func stringLiterals(toks []Token) []string {
	var out []string
	for _, tok := range toks {
		if tok.Type == TokenString {
			out = append(out, tok.Literal)
		}
	}
	return out
}
