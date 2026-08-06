package parser

import (
	"strings"
	"testing"
)

// rowauthz_blanker_multiline_test.go -- memql#3116.
//
// blankCommentsAndStrings ended a string literal at a newline and its comment
// claimed that was "the same recovery the lexer performs". The lexer performs
// no such recovery: scanString writes an interior newline into the builder as
// an ordinary rune and returns ONE token spanning the lines, and an
// unterminated literal is a hard error rather than a recovery. memql#3047's
// fix -- the line counter that advances through exactly such a literal --
// exists because these literals are legal.
//
// The divergence is not cosmetic. When the blanker leaves string state early
// its view of the file INVERTS: the literal's remaining content is scanned as
// code, and the real closing quote OPENS a new string, so genuine code after
// it is blanked as though it were string content. Both errors are visible in
// a single fixture below.

// multiLineLiteralSource is a file that lexes cleanly and contains a
// @description whose literal spans two lines, with the continuation line
// shaped like a concept declaration. Every assertion here uses it, because it
// is the shape that turns the blanker's desync into a wrong answer rather than
// merely a different one.
const multiLineLiteralSource = `@description("a description that wraps
concept phantom { evil string }")
concept real {
  bar string
}
`

// The premise. If this ever fails, the fixture stopped being valid memql and
// every assertion below it is testing nothing.
func TestLexerAcceptsAMultiLineStringLiteral(t *testing.T) {
	tokens, err := NewLexer(multiLineLiteralSource).Tokenize()
	if err != nil {
		t.Fatalf("fixture must lex cleanly, got: %v", err)
	}

	var found bool
	for _, tok := range tokens {
		if tok.Type == TokenString && strings.Contains(tok.Literal, "\n") {
			found = true
			if !strings.Contains(tok.Literal, "concept phantom") {
				t.Errorf("string token does not span the newline: %q", tok.Literal)
			}
		}
	}
	if !found {
		t.Fatal("no multi-line string token: the lexer no longer spans newlines, " +
			"so memql#3116's premise has changed and the blanker should be revisited")
	}
}

// An unterminated literal is an ERROR, not a recovery -- the other half of the
// false claim, and the reason option 2 (keep the line-bounded recovery for
// error tolerance) had nothing to be tolerant of.
func TestLexerRejectsAnUnterminatedStringLiteral(t *testing.T) {
	_, err := NewLexer(`@description("never closed`).Tokenize()
	if err == nil {
		t.Fatal("unterminated string lexed cleanly; the lexer now recovers and " +
			"memql#3116's decision should be revisited")
	}
	if !strings.Contains(err.Error(), "unterminated string") {
		t.Errorf("unexpected error for an unterminated string: %v", err)
	}
}

// The defect, in the direction that exposes string content as code.
func TestBlankerKeepsMultiLineLiteralContentBlanked(t *testing.T) {
	blanked := blankCommentsAndStrings(multiLineLiteralSource)

	if strings.Contains(blanked, "concept phantom") {
		t.Errorf("literal content survived the blanker as code:\n%s\n\n"+
			"A string literal spanning a newline must stay blanked to its closing "+
			"quote, matching the lexer (memql#3116).", blanked)
	}
}

// The defect, in the direction that hides real code as string content. This is
// the one a length-preserving blanker cannot signal: the byte is still there,
// it just says space.
func TestBlankerDoesNotSwallowCodeAfterAMultiLineLiteral(t *testing.T) {
	blanked := blankCommentsAndStrings(multiLineLiteralSource)

	// The `)` closing the @description call, immediately after the literal's
	// true closing quote. Asserted by offset rather than by substring: it is a
	// single byte, and the bug replaces it with a space in a length-preserving
	// output, so nothing about the file's shape reveals the loss.
	closer := strings.Index(multiLineLiteralSource, `")`) + 1
	if closer < 1 {
		t.Fatal("fixture no longer contains the closing quote+paren")
	}
	if blanked[closer] != ')' {
		t.Errorf("code byte at offset %d was blanked as string content: got %q, want ')'\n%s\n\n"+
			"Leaving string state at the newline makes the literal's true closing "+
			"quote OPEN a string, so the code after it is consumed (memql#3116).",
			closer, blanked[closer], blanked)
	}
}

// The consumer outcome, and the reason this is not cosmetic. ConceptHeaders
// documents that "the word concept inside a doc comment or a @description
// string can never be mistaken for a declaration"; memqlmigrate inserts
// @rowAuthz relative to a header's offsets, so a phantom header is a write
// INSIDE a string literal.
func TestConceptHeadersIgnoresAConceptInsideAMultiLineLiteral(t *testing.T) {
	headers := ConceptHeaders(multiLineLiteralSource)

	var names []string
	for _, h := range headers {
		names = append(names, h.Name)
		if h.Name == "phantom" {
			t.Errorf("phantom concept reported at offset %d, which is inside a "+
				"string literal. memqlmigrate would insert @rowAuthz there and "+
				"corrupt the source (memql#3116).", h.Start)
		}
	}
	if len(headers) != 1 || (len(headers) == 1 && headers[0].Name != "real") {
		t.Errorf("headers = %v, want exactly [real]", names)
	}
}

// Control. The blanker's whole job is hiding string content, so a suite that
// only asserted "content stays blanked" would pass against a function that
// blanks the entire file.
func TestBlankerStillExposesOrdinaryCode(t *testing.T) {
	const src = `@description("single line")
concept ordinary {
  bar string
}
`
	blanked := blankCommentsAndStrings(src)

	for _, want := range []string{"concept ordinary", "bar string", "@description("} {
		if !strings.Contains(blanked, want) {
			t.Errorf("blanker hid ordinary code %q:\n%s", want, blanked)
		}
	}
	if strings.Contains(blanked, "single line") {
		t.Errorf("blanker exposed single-line literal content:\n%s", blanked)
	}
}

// Offset preservation is the invariant every consumer depends on -- headers
// are located on the blanked view and sliced out of the raw text. A multi-line
// literal must not change that.
func TestBlankerPreservesLengthAndNewlinesAcrossAMultiLineLiteral(t *testing.T) {
	blanked := blankCommentsAndStrings(multiLineLiteralSource)

	if len(blanked) != len(multiLineLiteralSource) {
		t.Fatalf("length changed: got %d, want %d", len(blanked), len(multiLineLiteralSource))
	}
	if got, want := strings.Count(blanked, "\n"), strings.Count(multiLineLiteralSource, "\n"); got != want {
		t.Errorf("newline count changed: got %d, want %d", got, want)
	}
	for i := range multiLineLiteralSource {
		if multiLineLiteralSource[i] == '\n' && blanked[i] != '\n' {
			t.Fatalf("newline at offset %d became %q", i, blanked[i])
		}
	}
}

// An unterminated literal runs to EOF, which is what matching the lexer means
// at the boundary. Such a file does not lex (asserted above), so no consumer
// meaningfully processes it -- and blanking to EOF is the conservative end of
// the trade: a codemod that sees nothing rewrites nothing.
func TestBlankerRunsAnUnterminatedLiteralToEOF(t *testing.T) {
	const src = `@description("never closed
concept notReallyThere {
  bar string
}
`
	blanked := blankCommentsAndStrings(src)

	if strings.Contains(blanked, "concept notReallyThere") {
		t.Errorf("unterminated literal did not run to EOF:\n%s", blanked)
	}
	if len(blanked) != len(src) {
		t.Errorf("length changed: got %d, want %d", len(blanked), len(src))
	}
	if headers := ConceptHeaders(src); len(headers) != 0 {
		t.Errorf("ConceptHeaders found %d headers in an unlexable file; want none", len(headers))
	}
}
