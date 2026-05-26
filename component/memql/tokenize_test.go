package memql

import (
	"errors"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestTokenizeSharedLexer is the #242 acceptance regression test:
// the memql runtime parser shares the language parser's lexer AND
// its token-type enum (tokenType is a type alias for
// langparser.TokenType). The only remaining adaptation in tokenize()
// is the three semantic adjustments the runtime grammar needs vs.
// the load-time grammar. This test asserts each one + the baseline
// pass-through.
//
// If the local `tokenType` enum is ever forked off the shared one
// (re-introducing the "legacy token vocabulary" divergence #242
// retired), this test won't directly catch it -- the compiler will,
// because the const block would fail to assign langparser.TokenFoo
// to a non-alias local type. The test is here to lock in the
// behavior contracts above the enum aliasing.
func TestTokenizeSharedLexer(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []token // EOF appended automatically
	}{
		{
			name:  "keywords flatten to identifier",
			input: "in has not",
			want: []token{
				{typ: tokIdentifier, literal: "in", pos: 0},
				{typ: tokIdentifier, literal: "has", pos: 3},
				{typ: tokIdentifier, literal: "not", pos: 7},
			},
		},
		{
			name:  "bang surfaces as operator",
			input: "!",
			want: []token{
				{typ: tokOperator, literal: "!", pos: 0},
			},
		},
		{
			name:  "actor.userId is a single identifier (concept-style dotted path)",
			input: "actor.userId",
			want: []token{
				{typ: tokIdentifier, literal: "actor.userId", pos: 0},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tokenize(tc.input)
			if err != nil {
				t.Fatalf("tokenize(%q): %v", tc.input, err)
			}
			// Strip the trailing EOF for comparison.
			if len(got) == 0 || got[len(got)-1].typ != tokEOF {
				t.Fatalf("expected trailing EOF, got %#v", got)
			}
			got = got[:len(got)-1]
			if len(got) != len(tc.want) {
				t.Fatalf("got %d tokens, want %d: got=%#v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if got[i].typ != want.typ || got[i].literal != want.literal || got[i].pos != want.pos {
					t.Errorf("token[%d] = %#v, want %#v", i, got[i], want)
				}
			}
		})
	}
}

// TestTokenizeRejectsDollar guards the one explicit rejection that
// survived the simplification: `$` is emitted by the shared lexer
// (for `$var` references in the load-time grammar) but has no
// meaning in a standalone runtime query expression, so it's
// rejected with a clean "unexpected token" error rather than
// flowing through as a generic operator.
func TestTokenizeRejectsDollar(t *testing.T) {
	_, err := tokenize("$x")
	if err == nil {
		t.Fatal("expected tokenize to reject `$`, got nil")
	}
	if !errors.Is(err, ErrInvalidQuerySyntax) {
		t.Errorf("error %v is not wrapping ErrInvalidQuerySyntax", err)
	}
}

// TestTokenTypeIsAliasOfLangparser is a compile-time witness that
// the local tokenType is an alias (not a new type) for
// langparser.TokenType. If someone replaces the `type tokenType =
// langparser.TokenType` alias with `type tokenType int` (forking
// the enum), this test stops compiling -- the assignment
// langparser.TokenIdentifier -> tokenType requires the alias.
func TestTokenTypeIsAliasOfLangparser(t *testing.T) {
	var lt langparser.TokenType = langparser.TokenIdentifier
	var mt tokenType = lt // requires tokenType to alias langparser.TokenType
	if mt != tokIdentifier {
		t.Errorf("alias drift: tokIdentifier=%v vs langparser.TokenIdentifier=%v", tokIdentifier, lt)
	}
}
