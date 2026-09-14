package dslspec

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// punctuation_test.go pins LexicalPunctuation against the lexer, which is the
// truth it describes. The editor's comment toggling, bracket matching and
// indentation are generated from the table (cmd/memql-lsp/internal/grammar),
// so a table that disagreed with the lexer would teach every editor a
// delimiter MemQL does not have -- or hide one it does.

func lex(t *testing.T, src string) []parser.Token {
	t.Helper()
	toks, err := parser.NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("lex %q: %v", src, err)
	}
	return toks
}

// TestBracketPairsAreTheLexersNestingPairs: each pair's opener and closer lex
// to one of the lexer's matching open/close token types, and no printable
// character the table does not list lexes to an opener.
func TestBracketPairsAreTheLexersNestingPairs(t *testing.T) {
	closerFor := map[parser.TokenType]parser.TokenType{
		parser.TokenBraceOpen:   parser.TokenBraceClose,
		parser.TokenBracketOpen: parser.TokenBracketClose,
		parser.TokenParenOpen:   parser.TokenParenClose,
	}

	listed := map[string]bool{}
	for _, pair := range LexicalPunctuation().Brackets {
		listed[pair.Open] = true
		open := lex(t, pair.Open)[0].Type
		wantClose, isOpener := closerFor[open]
		if !isOpener {
			t.Errorf("%q is listed as an opener but lexes to token type %v", pair.Open, open)
			continue
		}
		if got := lex(t, pair.Close)[0].Type; got != wantClose {
			t.Errorf("%q is listed as the closer of %q but lexes to token type %v, want %v", pair.Close, pair.Open, got, wantClose)
		}
	}

	// The other direction: every printable ASCII character that lexes to an
	// opener is in the table. Characters the lexer refuses on their own are
	// skipped; they open nothing.
	for r := rune(0x21); r < 0x7f; r++ {
		toks, err := parser.NewLexer(string(r)).Tokenize()
		if err != nil || len(toks) == 0 {
			continue
		}
		if _, isOpener := closerFor[toks[0].Type]; isOpener && !listed[string(r)] {
			t.Errorf("%q lexes to an opening token but is not in LexicalPunctuation().Brackets; add it with its closer", string(r))
		}
	}
	if len(listed) != len(closerFor) {
		t.Errorf("the table lists %d bracket pairs and the lexer has %d opening token types", len(listed), len(closerFor))
	}
}

// TestCommentTokensAreTheLexersComments: text after the line-comment token,
// and between the block-comment tokens, produces no token at all.
func TestCommentTokensAreTheLexersComments(t *testing.T) {
	p := LexicalPunctuation()
	for _, src := range []string{
		p.LineComment + " a comment ( [ {\nkept",
		p.BlockComment.Open + " a comment\n( [ { " + p.BlockComment.Close + " kept",
	} {
		toks := lex(t, src)
		if len(toks) != 2 || toks[0].Literal != "kept" || toks[1].Type != parser.TokenEOF {
			t.Errorf("lexing %q gave %d tokens (%v); want only `kept` then EOF -- the comment tokens in the table are not the lexer's", src, len(toks), toks)
		}
	}
}

// TestStringQuoteIsTheLexersStringDelimiter: the quote opens and closes one
// string literal.
func TestStringQuoteIsTheLexersStringDelimiter(t *testing.T) {
	q := LexicalPunctuation().StringQuote
	toks := lex(t, q+"a { b"+q)
	if toks[0].Type != parser.TokenString || toks[0].Literal != "a { b" {
		t.Errorf("lexing %q gave %v %q; want one string literal `a { b`", q+"a { b"+q, toks[0].Type, toks[0].Literal)
	}
}

// TestLexicalPunctuationIsAFreshValue: a caller editing its copy cannot change
// the next caller's answer.
func TestLexicalPunctuationIsAFreshValue(t *testing.T) {
	first := LexicalPunctuation()
	first.Brackets[0] = DelimiterPair{Open: "<", Close: ">"}
	if LexicalPunctuation().Brackets[0].Open == "<" {
		t.Error("LexicalPunctuation shares its bracket slice between callers")
	}
}
