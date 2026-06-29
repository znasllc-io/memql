package parser

import "testing"

// TestArrowLambdaToken locks Story 4 (#2302 / ADR §2.2): the lexer must emit a
// single `=>` operator token so collection-method lambdas
// (`args.members.where(m => m.active)`) can separate params from body.
func TestArrowLambdaToken(t *testing.T) {
	tokens, err := NewLexer("m => m.active").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	var arrow int
	for _, tk := range tokens {
		if tk.Type == TokenOperator && tk.Literal == "=>" {
			arrow++
		}
		if tk.Type == TokenOperator && tk.Literal == "=" {
			t.Errorf("got bare '=' token; expected the arrow to fuse into '=>'")
		}
	}
	if arrow != 1 {
		t.Fatalf("want exactly one '=>' token, got %d; tokens=%+v", arrow, tokens)
	}
}

// TestSingleEqualsStillLexes guards that a lone '=' (assignment / object key
// `=` form) is unaffected by the arrow addition.
func TestSingleEqualsStillLexes(t *testing.T) {
	tokens, err := NewLexer("x = 5").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	var eq int
	for _, tk := range tokens {
		if tk.Type == TokenOperator && tk.Literal == "=" {
			eq++
		}
	}
	if eq != 1 {
		t.Fatalf("want exactly one '=' token, got %d", eq)
	}
}
