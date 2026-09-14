package dslspec

// punctuation.go -- the delimiters an editor pairs, comments and indents by
// (memql#5362; D25 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The VS Code extension's language configuration -- comment toggling, bracket
// matching, auto-closing and surrounding pairs, and the Enter and re-indent
// rules built from those pairs -- is generated from this table by
// cmd/memql-lsp (internal/grammar/languageconfig.go), the same way the
// TextMate grammar is generated from the keyword tables. Before it, the file
// was written by hand beside a grammar that was generated, so nothing could
// notice the two describing different languages.
//
// The LEXER is the truth this table describes
// (component/language/parser/lexer.go). punctuation_test.go lexes every entry
// and fails when a listed delimiter lexes as something else, and when a
// printable character lexes to one of the lexer's three opening token types
// (brace, bracket, paren) without being listed. What it cannot see is a NEW
// kind of opening token: the lexer's token types are a plain enum with no
// opener/closer pairing to read, so the test names those three, and a lexer
// change adding a fourth must add it to the test's closerFor map as well.

// DelimiterPair is an opening delimiter and the one that closes it.
type DelimiterPair struct {
	Open  string
	Close string
}

// Punctuation is MemQL's lexical punctuation: what opens and closes a comment,
// a nesting pair, and a string.
type Punctuation struct {
	// LineComment starts a comment that runs to the end of its line.
	LineComment string
	// BlockComment delimits a comment that may span lines.
	BlockComment DelimiterPair
	// Brackets are the nesting pairs, in the order an editor lists them:
	// braces (construct bodies and blocks), square brackets (list literals),
	// parentheses (calls and grouping).
	Brackets []DelimiterPair
	// StringQuote opens a string literal and closes it. MemQL has one string
	// delimiter; there is no single-quoted or raw form.
	StringQuote string
}

// LexicalPunctuation returns MemQL's punctuation. Each call builds a fresh
// value, so a caller that edits its copy cannot change the next caller's
// answer.
func LexicalPunctuation() Punctuation {
	return Punctuation{
		LineComment:  "//",
		BlockComment: DelimiterPair{Open: "/*", Close: "*/"},
		Brackets: []DelimiterPair{
			{Open: "{", Close: "}"},
			{Open: "[", Close: "]"},
			{Open: "(", Close: ")"},
		},
		StringQuote: `"`,
	}
}
