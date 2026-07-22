package sense

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

func TestProbe_DumpTokens(t *testing.T) {
	srcs := []string{
		"mutate Thing doThing {\n  args {\n    query ",
		"concept Thing {\n  query ",
		"logic doThing {\n  x = foo(query ",
	}
	for _, src := range srcs {
		lex := parser.NewLexer(src)
		toks, err := lex.Tokenize()
		t.Logf("SRC=%q err=%v", src, err)
		for _, tk := range toks {
			t.Logf("   type=%d literal=%q", int(tk.Type), tk.Literal)
		}
	}
	// print the relevant token type constants
	t.Logf("TokenBraceOpen=%d TokenBraceClose=%d TokenIdentifier=%d TokenParenOpen=%d",
		int(parser.TokenBraceOpen), int(parser.TokenBraceClose), int(parser.TokenIdentifier), int(parser.TokenParenOpen))
}
