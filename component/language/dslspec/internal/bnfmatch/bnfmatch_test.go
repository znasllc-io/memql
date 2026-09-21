package bnfmatch

// The recognizer's own tests. It is the instrument the grammar is measured
// with, so it needs its own calibration: a recognizer that accepted everything
// would report a perfect grammar, and one that accepted nothing would report a
// broken corpus. Each case below is one property the corpus proof rests on.

import (
	"strings"
	"testing"
)

// tiny is a grammar exercising every shape of the notation: an optional, a
// repetition in both spellings, a group, an alternation, a character range and
// a lexical production.
const tiny = `
(* a comment, which the reader skips
   over both of its lines *)
<file>      ::= <use>* <decl>*
<use>       ::= "use" <name> "{" <name> { "," <name> } "}"
<decl>      ::= "concept" <name> "{" <field>* "}"
<field>     ::= <name> <name> [ "!" ]
<name>      ::= ( "a".."z" | "A".."Z" | "_" ) { "a".."z" | "A".."Z" | "0".."9" | "_" }
<number>    ::= [ "-" ] <digit>+
<digit>     ::= "0".."9"
`

func mustParse(t *testing.T, text string) *Grammar {
	t.Helper()
	g, err := ParseGrammar(text)
	if err != nil {
		t.Fatalf("ParseGrammar: %v", err)
	}
	return g
}

func accepts(t *testing.T, g *Grammar, start, src string) Result {
	t.Helper()
	toks, err := Tokens(src)
	if err != nil {
		t.Fatalf("lex %q: %v", src, err)
	}
	return Match(g, start, toks)
}

func TestRecognizesAndRefuses(t *testing.T) {
	g := mustParse(t, tiny)
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"concept ticket { title string }", true},
		{"concept ticket { title string! }", true},
		{"use common { a, b }\nconcept ticket { }", true},
		{"use common { a b }\nconcept ticket { }", false}, // the comma is not optional here
		{"concept ticket { title }", false},               // a field needs its type
		{"concept ticket {", false},                       // never closed
		{"concept { title string }", false},               // no name
		{"widget ticket { }", false},                      // no production opens on it
	} {
		if got := accepts(t, g, "file", tc.src).OK; got != tc.want {
			t.Errorf("%q: derived=%v, want %v", tc.src, got, tc.want)
		}
	}
}

// TestFurthestIsAnIndexIntoTheCallersTokens is what makes a failure message
// usable: the recognizer splits a fused dotted identifier into segments
// internally, and a position reported in THAT stream would name a token the
// caller never passed and a column the author never wrote.
func TestFurthestIsAnIndexIntoTheCallersTokens(t *testing.T) {
	g := mustParse(t, `<start> ::= <name> "." <name> "." <name> "!"
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	src := "row.lineage.plan"
	toks, err := Tokens(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("the lexer no longer fuses a dotted path into one token (%d tokens); this test is about that fusion", len(toks))
	}
	res := Match(g, "start", toks)
	if res.OK {
		t.Fatal("the grammar needs a trailing `!` the input does not have; it must not derive")
	}
	if res.Furthest != len(toks) {
		t.Errorf("Furthest = %d, want %d (the end of the caller's tokens)", res.Furthest, len(toks))
	}
	if where := res.Where(toks); where != "the end of the input" {
		t.Errorf("Where = %q, want the end of the input", where)
	}
}

// TestALexicalProductionHoldsTheTokensText: a <name> is not just "an
// identifier". The recognizer reads the production character by character, so
// it is never looser than the grammar it is proving -- a hyphen derives only
// where the production spells one.
func TestALexicalProductionHoldsTheTokensText(t *testing.T) {
	withHyphen := mustParse(t, `<start> ::= <name>
<name>  ::= ( "a".."z" ) { "a".."z" | "-" }`)
	noHyphen := mustParse(t, `<start> ::= <name>
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	if !accepts(t, withHyphen, "start", "row-crop").OK {
		t.Error("a <name> whose production spells a hyphen must derive one")
	}
	if accepts(t, noHyphen, "start", "row-crop").OK {
		t.Error("a <name> whose production spells no hyphen must NOT derive one; the recognizer is looser than the grammar")
	}
}

// TestATerminalIsNeverAStringLiteral: `"query"` is the word, not the string.
// Without this a file could derive by having a string in a keyword's place.
func TestATerminalIsNeverAStringLiteral(t *testing.T) {
	g := mustParse(t, `<start> ::= "concept" <name>
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	if !accepts(t, g, "start", "concept ticket").OK {
		t.Fatal("the bare word must derive")
	}
	if accepts(t, g, "start", `"concept" ticket`).OK {
		t.Error("a string literal derived the terminal `concept`")
	}
}

// TestAMultiTokenTerminalMatchesItsTokens: several terminals the MemQL grammar
// writes are more than one token once lexed -- `[]` is two and `@description`
// is `@` and an identifier -- so a terminal is matched as the token sequence
// the lexer reads it as, not as one token.
func TestAMultiTokenTerminalMatchesItsTokens(t *testing.T) {
	g := mustParse(t, `<start> ::= "@description" "[]" <name>
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	if !accepts(t, g, "start", "@description []string").OK {
		t.Error("a multi-token terminal did not match the tokens the lexer reads it as")
	}
	if accepts(t, g, "start", "@ description []string").OK {
		// The lexer reads `@ description` as the same two tokens, so this one
		// SHOULD derive; the assertion is inverted deliberately to record it.
		t.Log("note: `@ description` derives, because the lexer reads the same two tokens; " +
			"whitespace is not the grammar's business")
	}
}

// TestProseAndUndefinedAreReported: the recognizer must never treat a
// production it cannot read as "matches nothing" in silence, or a grammar that
// had rotted into English would read as a corpus that had gone wrong.
func TestProseAndUndefinedAreReported(t *testing.T) {
	g := mustParse(t, `<start>  ::= <english> <missing>
<english> ::= any Unicode code point`)
	res := accepts(t, g, "start", "x")
	if res.OK {
		t.Fatal("nothing can derive from a prose production")
	}
	if len(res.Prose) != 1 || res.Prose[0] != "english" {
		t.Errorf("Prose = %v, want [english]", res.Prose)
	}
	if got := g.Undefined(); len(got) != 1 || got[0] != "missing" {
		t.Errorf("Undefined = %v, want [missing]", got)
	}
}

// TestACharacterProductionReachedAtTokenLevelIsReported. <character>,
// <digit> and <text> describe the CHARACTERS of a token; the real grammar
// reaches them only from <string>, <number> and <doc-comment>, which are
// matched by the token's kind. One reached at token level is a production
// nothing there can match, and must be said rather than read as a corpus that
// has gone wrong.
func TestACharacterProductionReachedAtTokenLevelIsReported(t *testing.T) {
	g := mustParse(t, `<start> ::= <digit>
<digit> ::= "0".."9"`)
	res := accepts(t, g, "start", "7")
	if res.OK {
		t.Fatal("a character-level production matches no token")
	}
	if len(res.CharOnly) != 1 || res.CharOnly[0] != "digit" {
		t.Errorf("CharOnly = %v, want [digit]", res.CharOnly)
	}
}

// TestLeftRecursionIsReportedRatherThanHung: a left-recursive production is a
// grammar defect, and a recognizer that looped on one would time the test out
// with nothing to read.
func TestLeftRecursionIsReportedRatherThanHung(t *testing.T) {
	g := mustParse(t, `<start> ::= <start> "!" | <name>
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	res := accepts(t, g, "start", "x !")
	if len(res.LeftRecursive) != 1 || res.LeftRecursive[0] != "start" {
		t.Errorf("LeftRecursive = %v, want [start]", res.LeftRecursive)
	}
}

// TestInvisibleAndCharacterTerminalsAreTold apart: `///` lexes cleanly to no
// token (the lexer takes a doc comment off on a side channel) while a bare
// quote is REFUSED by the lexer (an unterminated string). They are different
// facts and the caller pins them separately.
func TestInvisibleAndCharacterTerminalsAreTold(t *testing.T) {
	g := mustParse(t, "<start> ::= <doc> <q>\n<doc>   ::= \"///\"\n<q>     ::= '\"'")
	if got := g.InvisibleTerminals(); len(got) != 1 || got[0] != "///" {
		t.Errorf("InvisibleTerminals = %q, want [///]", got)
	}
	if got := g.CharacterTerminals(); len(got) != 1 || got[0] != `"` {
		t.Errorf("CharacterTerminals = %q, want a bare quote", got)
	}
}

// TestNotationParsesTheShapesTheGrammarWrites walks one right-hand side of
// every shape and asserts the tree, so a notation the renderer starts emitting
// cannot be silently misread as something else.
func TestNotationParsesTheShapesTheGrammarWrites(t *testing.T) {
	g := mustParse(t, `<a> ::= "x" [ "y" ] { "z" } ( "p" | "q" ) <b>* <c>+
<b> ::= "b"
<c> ::= "c"`)
	seq, ok := g.Production("a").Expr.(Seq)
	if !ok {
		t.Fatalf("the right-hand side read as %T, not a sequence", g.Production("a").Expr)
	}
	if len(seq) != 6 {
		t.Fatalf("the sequence has %d parts, want 6: %#v", len(seq), seq)
	}
	for i, want := range []string{"Term", "Opt", "Rep", "Alt", "Rep", "Plus"} {
		if got := shapeName(seq[i]); got != want {
			t.Errorf("part %d read as %s, want %s", i, got, want)
		}
	}
}

func shapeName(e Expr) string {
	switch e.(type) {
	case Term:
		return "Term"
	case Ref:
		return "Ref"
	case Opt:
		return "Opt"
	case Rep:
		return "Rep"
	case Plus:
		return "Plus"
	case Alt:
		return "Alt"
	case Seq:
		return "Seq"
	case Range:
		return "Range"
	case Prose:
		return "Prose"
	}
	return "?"
}

// TestAContinuationLineJoinsItsProduction: the published page wraps a long
// alternation onto lines opening with a bar, and a reader that dropped them
// would prove a grammar with most of its alternatives missing -- and would
// pass, because the corpus would simply stop deriving.
func TestAContinuationLineJoinsItsProduction(t *testing.T) {
	g := mustParse(t, `<start>  ::= "a" | "b"
                           | "c"
<other>  ::= "d"`)
	for _, src := range []string{"a", "b", "c"} {
		if !accepts(t, g, "start", src).OK {
			t.Errorf("%q did not derive; the continuation line was dropped", src)
		}
	}
}

// TestALineThatIsNeitherIsAnError: silently skipping a line the reader cannot
// place is how a reader ends up proving half a grammar.
func TestALineThatIsNeitherIsAnError(t *testing.T) {
	_, err := ParseGrammar("<start> ::= \"a\"\nthis line is not a production\n")
	if err == nil {
		t.Fatal("a line that is neither a production nor a continuation must be an error")
	}
	if !strings.Contains(err.Error(), "silently ignore") {
		t.Errorf("the error does not say why it matters: %v", err)
	}
}

// TestTheStackNamesWhereItStopped: the failure message's usefulness is the
// production it names, so the stack has to reach the innermost one.
func TestTheStackNamesWhereItStopped(t *testing.T) {
	g := mustParse(t, `<file>  ::= <decl>*
<decl>  ::= "concept" <name> "{" <field>* "}"
<field> ::= <name> <name>
<name>  ::= ( "a".."z" ) { "a".."z" }`)
	res := accepts(t, g, "file", "concept ticket { title }")
	if res.OK {
		t.Fatal("a field with no type must not derive")
	}
	if got := res.In(); !strings.Contains(got, "<field>") || !strings.Contains(got, "<file>") {
		t.Errorf("In() = %q; it must name the production it stopped in and the path to it", got)
	}
}
