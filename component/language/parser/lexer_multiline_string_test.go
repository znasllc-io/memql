package parser

import (
	"strings"
	"testing"
)

// lexer_multiline_string_test.go -- memql#3047.
//
// scanString consumed a literal newline like any other byte: it wrote it into
// the builder and advanced position and column, but never advanced the LINE
// counter. So every position derived after a multi-line literal was short by
// the number of newlines inside it, and the drift accumulated per literal.
//
// The parse itself was fine. Only the line numbers were wrong -- which is the
// entire value of a diagnostic, and the failure was silent.
//
// Latent when found (a literal-aware scan reported 0 multi-line literals
// across 6,245 string tokens in 199 authored files), but nothing forbids one
// and wrapping a long @description is a natural thing to do.

// tokenNamed returns the first token whose literal matches, so a test can name
// what it is asserting about rather than index into the stream.
func tokenNamed(t *testing.T, toks []Token, literal string) Token {
	t.Helper()
	for _, tk := range toks {
		if tk.Literal == literal {
			return tk
		}
	}
	t.Fatalf("no token with literal %q in the stream", literal)
	return Token{}
}

// The token AFTER a multi-line literal must report its real source line. This
// is the issue's own reproduction.
func TestLexer_LineCounterAdvancesThroughMultiLineString(t *testing.T) {
	src := "@description(\"line one\nline two\")\nautomation foo { }"
	//        line 1 ...........................
	//                                  line 2 ..
	//        line 3: automation foo { }

	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	got := tokenNamed(t, toks, "automation")
	if got.Line != 3 {
		t.Errorf("`automation` is on source line 3, reported as line %d.\n"+
			"scanString consumed the newline inside the literal without advancing the line "+
			"counter, so every position after a multi-line literal drifts (memql#3047).", got.Line)
	}
}

// The multi-line string token's own span must cover the lines it really
// occupies. Tokenize stamps EndLine from the lexer's post-scan line, so this
// follows from the same fix -- and it is what a viewer highlighting the token
// reads.
func TestLexer_MultiLineStringTokenSpansItsLines(t *testing.T) {
	src := "@description(\"line one\nline two\nline three\")"

	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	str := tokenNamed(t, toks, "line one\nline two\nline three")
	if str.Line != 1 {
		t.Errorf("the literal starts on line 1, reported as %d", str.Line)
	}
	if str.EndLine != 3 {
		t.Errorf("the literal ends on line 3, EndLine = %d -- the token's own span "+
			"understates it (memql#3047)", str.EndLine)
	}
}

// Drift accumulates: two multi-line literals must not compound the error.
func TestLexer_LineDriftDoesNotAccumulateAcrossLiterals(t *testing.T) {
	src := strings.Join([]string{
		`@description("a`, // 1-2
		`b")`,
		`@note("c`, // 3-4
		`d")`,
		`automation foo { }`, // 5
	}, "\n")

	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if got := tokenNamed(t, toks, "automation"); got.Line != 5 {
		t.Errorf("`automation` is on line 5, reported as %d -- drift compounds per literal", got.Line)
	}
}

// The control, and the reason it matters: an ESCAPED newline (`\n` in source)
// is two source bytes on ONE line. Advancing the counter for it would break
// every single-line literal in the tree, so the fix must key on a real newline
// byte and not on the decoded rune.
func TestLexer_EscapedNewlineDoesNotAdvanceTheLineCounter(t *testing.T) {
	src := `@description("line one\nline two")` + "\n" + `automation foo { }`

	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	str := tokenNamed(t, toks, "line one\nline two")
	if str.Line != 1 || str.EndLine != 1 {
		t.Errorf("an escaped \\n is one source line: Line=%d EndLine=%d, want 1 and 1",
			str.Line, str.EndLine)
	}
	if got := tokenNamed(t, toks, "automation"); got.Line != 2 {
		t.Errorf("`automation` is on line 2, reported as %d", got.Line)
	}
}

// Column must restart at the new line rather than continuing to climb, or a
// caret pointing at the offending column lands off the end of the line.
func TestLexer_ColumnResetsAfterAnEmbeddedNewline(t *testing.T) {
	src := "@description(\"aaaaaaaaaaaaaaaaaaaa\nx\")\nautomation foo { }"

	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	got := tokenNamed(t, toks, "automation")
	if got.Line != 3 {
		t.Fatalf("`automation` line = %d, want 3", got.Line)
	}
	if got.Column != 1 {
		t.Errorf("`automation` starts at column 1, reported as %d -- the column did not "+
			"reset across the embedded newline", got.Column)
	}
}
