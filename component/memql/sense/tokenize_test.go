package sense

import (
	"testing"
)

// TestTokenize_StringEndCoversQuotes is the regression net for
// memql-cockpit#114. Pre-fix the tokenizer computed
// `endCol = pt.Column + len(pt.Literal)`, but for string tokens
// the lexer strips the quotes from Literal -- so the reported
// End.Column landed two cells before the closing quote. Consumers
// using the Range to drive syntax highlighting then left the last
// 1-2 cells of every quoted string unstyled.
//
// The contract this pins: the string token's Range covers the
// FULL source span, opening quote through closing quote inclusive.
func TestTokenize_StringEndCoversQuotes(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		wantLiteral string
		wantStart   Position // 1-indexed start (opening quote)
		wantEnd     Position // 1-indexed exclusive end (one past closing quote)
	}{
		{
			name:        "simple string",
			source:      `"hello"`,
			wantLiteral: "hello",
			wantStart:   Position{Line: 1, Column: 1},
			wantEnd:     Position{Line: 1, Column: 8}, // 7 source chars + 1
		},
		{
			name:        "string with escaped quote",
			source:      `"a\"b"`,
			wantLiteral: `a"b`,
			wantStart:   Position{Line: 1, Column: 1},
			wantEnd:     Position{Line: 1, Column: 7}, // 6 source chars + 1
		},
		{
			name:        "indented concept-id string",
			source:      "  " + `"v1:agents:agent:abc"`,
			wantLiteral: "v1:agents:agent:abc",
			wantStart:   Position{Line: 1, Column: 3},
			wantEnd:     Position{Line: 1, Column: 24}, // 21 source chars starting at col 3 -> end at col 24
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{}
			tokens := svc.Tokenize(tc.source)

			// Find the string token. Other tokens (whitespace, EOF)
			// aren't emitted by Tokenize, but trailing punctuation
			// might be -- pick by Type.
			var strTok *Token
			for i := range tokens {
				if tokens[i].Type == "string" {
					strTok = &tokens[i]
					break
				}
			}
			if strTok == nil {
				t.Fatalf("no string token emitted for source %q; got %d tokens", tc.source, len(tokens))
			}

			if strTok.Literal != tc.wantLiteral {
				t.Errorf("Literal = %q, want %q", strTok.Literal, tc.wantLiteral)
			}
			if strTok.Range.Start != tc.wantStart {
				t.Errorf("Start = %+v, want %+v", strTok.Range.Start, tc.wantStart)
			}
			if strTok.Range.End != tc.wantEnd {
				t.Errorf("End = %+v, want %+v -- string token end must include the closing quote", strTok.Range.End, tc.wantEnd)
			}

			// Source-span sanity: End.Column - Start.Column should equal
			// the number of source runes consumed by the string literal
			// (quotes + content + escape-bytes). Cross-check with the
			// raw source length to lock the invariant.
			//
			// Note: for the indented case the source has 2 leading
			// spaces, so total runes = 2 + (End.Column - Start.Column).
			gotSpan := tc.wantEnd.Column - tc.wantStart.Column
			wantSpan := len(tc.source) - (tc.wantStart.Column - 1)
			if gotSpan != wantSpan {
				t.Errorf("source span = %d cols, want %d (full source length minus leading offset)", gotSpan, wantSpan)
			}
		})
	}
}

// TestTokenize_IdentifierEndIsExclusive covers the common non-string
// path -- identifiers, where the Literal length already matched the
// source length, but the new End* path should still produce the same
// answer.
func TestTokenize_IdentifierEndIsExclusive(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("foo bar")

	if len(tokens) < 2 {
		t.Fatalf("want >= 2 tokens, got %d: %+v", len(tokens), tokens)
	}
	if tokens[0].Literal != "foo" || tokens[0].Range.Start.Column != 1 || tokens[0].Range.End.Column != 4 {
		t.Errorf("foo token: %+v, want Start.Column=1 End.Column=4", tokens[0].Range)
	}
	if tokens[1].Literal != "bar" || tokens[1].Range.Start.Column != 5 || tokens[1].Range.End.Column != 8 {
		t.Errorf("bar token: %+v, want Start.Column=5 End.Column=8", tokens[1].Range)
	}
}

// TestTokenize_NumberEnd: numbers are non-string but worth pinning
// since the path-through-mapTokenType is the same.
func TestTokenize_NumberEnd(t *testing.T) {
	svc := &Service{}
	tokens := svc.Tokenize("42")
	if len(tokens) != 1 {
		t.Fatalf("want 1 token, got %d", len(tokens))
	}
	if tokens[0].Type != "number" {
		t.Errorf("Type = %q, want number", tokens[0].Type)
	}
	if tokens[0].Range.End.Column != 3 {
		t.Errorf("End.Column = %d, want 3 (one past last digit)", tokens[0].Range.End.Column)
	}
}
