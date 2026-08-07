package compiler

// serialized_literal_control_byte_test.go -- regression guard for memql#3192
// on the compiler's serialize/re-parse boundary.
//
// The automation compiler turns a parsed .memql body back INTO MemQL text: an
// expression becomes step config the runtime re-parses, and a mutation
// statement becomes an `insert(...)` string. Every string literal on that
// round trip was re-rendered with fmt.Sprintf("%q"), whose escape set the
// lexer does not implement -- `\x00`, `\a` and `\v` are `invalid escape
// character`, a hard error at tokenize time.
//
// Reachable from authored DSL, not just from runtime data: the lexer DECODES
// \u00XX, so an author writing "" in a .memql literal produces a Go
// string carrying a BEL, which the compiler then re-emitted as `\a` and the
// runtime re-parse rejected. The value survives the first parse and dies on
// the second, which is the worst place for it -- load succeeded, so the
// failure surfaces when the automation runs.

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// compilerControlByteFixture is what the lexer hands back for the authored
// literal "boom \u0000 \u0007 \u000b end".
const compilerControlByteFixture = "boom \x00 \a \v end"

// lexOne tokenizes through the real lexer and returns the first string
// literal it decoded.
func lexOne(t *testing.T, src string) string {
	t.Helper()
	toks, err := parser.NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the serialized form (the #3192 defect): %v\nsource: %#v", err, src)
	}
	for _, tok := range toks {
		if tok.Type == parser.TokenString {
			return tok.Literal
		}
	}
	t.Fatalf("no string token in %#v", src)
	return ""
}

// TestAuthoredUnicodeEscapeDecodesToAControlByte pins the premise: the path
// above is reachable from authored DSL, because the lexer decodes \u00XX into
// a real control byte rather than keeping it escaped.
func TestAuthoredUnicodeEscapeDecodesToAControlByte(t *testing.T) {
	got := lexOne(t, "\"boom \\u0000 \\u0007 \\u000b end\"")
	if got != compilerControlByteFixture {
		t.Fatalf("authored escape decoded to %#v, want %#v", got, compilerControlByteFixture)
	}
}

func TestExpressionToString_ControlByteLexes(t *testing.T) {
	c := &Compiler{}
	src := c.expressionToString(&parser.LiteralExpr{Value: compilerControlByteFixture})

	if got := lexOne(t, src); got != compilerControlByteFixture {
		t.Errorf("literal did not round-trip: got %#v, want %#v", got, compilerControlByteFixture)
	}
}

func TestValueToString_ControlByteLexes(t *testing.T) {
	c := &Compiler{}
	src := c.valueToString(map[string]any{"note": compilerControlByteFixture})

	if got := lexOne(t, src); got != compilerControlByteFixture {
		t.Errorf("literal did not round-trip: got %#v, want %#v", got, compilerControlByteFixture)
	}
}

func TestMutationToString_ControlByteLexes(t *testing.T) {
	c := &Compiler{}
	src := c.mutationToString(&parser.MutationStmt{
		Concept:    "v1:ship:scan",
		IDTemplate: "v1:ship:scan:a\x00b",
	})

	toks, err := parser.NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the serialized mutation (the #3192 defect): %v\nsource: %#v", err, src)
	}
	var literals []string
	for _, tok := range toks {
		if tok.Type == parser.TokenString {
			literals = append(literals, tok.Literal)
		}
	}
	want := []string{"v1:ship:scan", "v1:ship:scan:a\x00b"}
	if len(literals) != len(want) {
		t.Fatalf("decoded %#v, want %#v", literals, want)
	}
	for i := range want {
		if literals[i] != want[i] {
			t.Errorf("literal %d: got %#v, want %#v", i, literals[i], want[i])
		}
	}
}
