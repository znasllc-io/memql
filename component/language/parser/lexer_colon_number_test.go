package parser

import "testing"

// TestColonNumberNotJoinedIntoIdentifier locks #1118: a bare `identifier:number`
// with no space (e.g. the hand-built object literal `{tokenBudget:300000}`) must
// lex as three tokens -- identifier, ':', number -- NOT a single valueless key.
// The lexer joins ':' into an identifier only when the next char starts a new
// identifier segment (a letter, for concept ids like v1:crm:lead); a ':' before
// a digit is an object-key separator.
func TestColonNumberNotJoinedIntoIdentifier(t *testing.T) {
	tokens, err := NewLexer("{tokenBudget:300000}").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	// Expected meaningful tokens (ignoring any trailing EOF): { tokenBudget : 300000 }
	want := []struct {
		typ TokenType
		lit string
	}{
		{TokenBraceOpen, "{"},
		{TokenIdentifier, "tokenBudget"},
		{TokenColon, ":"},
		{TokenNumber, "300000"},
		{TokenBraceClose, "}"},
	}

	var got []Token
	for _, tk := range tokens {
		if tk.Type == TokenBraceOpen || tk.Type == TokenBraceClose ||
			tk.Type == TokenIdentifier || tk.Type == TokenColon || tk.Type == TokenNumber {
			got = append(got, tk)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("token count = %d, want %d; got=%+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Type != w.typ || got[i].Literal != w.lit {
			t.Errorf("token[%d] = (%v %q), want (%v %q)", i, got[i].Type, got[i].Literal, w.typ, w.lit)
		}
	}
}

// TestConceptIdStillLexesAsSingleIdentifier guards the behavior the colon-join
// exists for: a concept id whose segments all start with letters (v1:crm:lead)
// must still lex as ONE identifier token. The #1118 fix only drops the digit
// case, so this is unaffected.
func TestConceptIdStillLexesAsSingleIdentifier(t *testing.T) {
	for _, id := range []string{"v1:crm:lead", "v1:cognition:space", "v1:cluster:node"} {
		tokens, err := NewLexer(id).Tokenize()
		if err != nil {
			t.Fatalf("tokenize %q: %v", id, err)
		}
		var idents []Token
		for _, tk := range tokens {
			if tk.Type == TokenIdentifier {
				idents = append(idents, tk)
			}
			if tk.Type == TokenColon {
				t.Errorf("%q produced a bare ':' token; concept id should stay one identifier", id)
			}
		}
		if len(idents) != 1 || idents[0].Literal != id {
			t.Errorf("%q lexed as %+v, want a single identifier token", id, idents)
		}
	}
}

// TestColonNumberObjectLiteralParses is the end-to-end #1118 regression: the
// hand-built `{tokenBudget:300000}` object literal must PARSE (previously the
// parser failed at `}` with `expected ':' or '=' after object key`).
func TestColonNumberObjectLiteralParses(t *testing.T) {
	tokens, err := NewLexer("{tokenBudget:300000}").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if _, err := NewParser(tokens).parseExpression(); err != nil {
		t.Fatalf("parseExpression of {tokenBudget:300000}: %v", err)
	}
}
