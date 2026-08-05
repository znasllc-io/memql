package parser

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// TokenType enumerates lexical token categories.
type TokenType int

const (
	TokenEOF TokenType = iota
	TokenIdentifier
	TokenNumber
	TokenString
	TokenOperator
	TokenParenOpen
	TokenParenClose
	TokenBraceOpen
	TokenBraceClose
	TokenBracketOpen
	TokenBracketClose
	TokenColon
	TokenSemicolon
	TokenComma
	TokenDefine           // :=
	TokenQuestion         // ? for ternary operator
	TokenQuestionDot      // ?. for optional/conditional filters
	TokenQuestionQuestion // ?? for null coalescing
	TokenAt               // @ for Python-style attributes/decorators
	TokenAmpAmp           // && for logical AND
	TokenPipePipe         // || for logical OR
	TokenBang             // ! for boolean negation
	TokenDot              // . between path and `{` in `use path.{ names }`

	// Keywords - Type receivers (used in func receiver syntax)
	TokenKeywordQuery      // Query
	TokenKeywordMutation   // Mutation
	TokenKeywordAutomation // Automation
	TokenKeywordSpec       // Spec
	TokenKeywordTool       // Tool
	TokenKeywordBuiltin    // Builtin

	// Keywords - Go-like control flow
	TokenKeywordFunc     // func
	TokenKeywordFor      // for
	TokenKeywordRange    // range
	TokenKeywordIf       // if
	TokenKeywordElse     // else
	TokenKeywordSwitch   // switch
	TokenKeywordCase     // case
	TokenKeywordDefault  // default
	TokenKeywordContinue // continue
	TokenKeywordBreak    // break
	TokenKeywordReturn   // return
	TokenKeywordNil      // nil
	TokenKeywordRetry    // retry
	TokenKeywordWhen     // when (conditional step execution)
	TokenKeywordAs       // as (forEach iteration variable, use alias)
	TokenKeywordWhere    // where (forEach filter)
	TokenKeywordUse      // use (concept import declaration)
	TokenKeywordImport   // import (file-import block; new model)
	TokenKeywordConcept  // concept (concept definition)
	TokenKeywordIn       // in (membership operator)
	TokenKeywordHas      // has (containment operator)
	TokenKeywordNot      // not (negation, used in "not in")
)

// Token represents a lexical token.
//
// Position fields are 1-indexed (Line, Column) plus a byte offset
// (Pos). EndLine / EndCol / EndPos point to the FIRST position
// after the token -- i.e. half-open [Column, EndCol). For
// single-character tokens, EndCol == Column + 1. For string
// tokens, the end positions span the closing quote AND the
// content (Literal strips quotes + decodes escapes, so its length
// no longer matches the source span -- the EndCol field is the
// authoritative source-length record).
//
// Tokenize() populates the End* fields after each scan; the
// individual scan functions don't have to stamp them themselves.
type Token struct {
	Type    TokenType
	Literal string
	Pos     int
	Line    int
	Column  int
	EndPos  int
	EndLine int
	EndCol  int
}

// String returns a human-readable representation of the token.
func (t Token) String() string {
	return fmt.Sprintf("Token{%v, %q, pos=%d}", t.Type, t.Literal, t.Pos)
}

// String returns a human-readable name for a TokenType. Used in parse
// error messages so users see "expected '{'" instead of "expected 8".
func (t TokenType) String() string {
	switch t {
	case TokenEOF:
		return "end of input"
	case TokenIdentifier:
		return "identifier"
	case TokenNumber:
		return "number"
	case TokenString:
		return "string"
	case TokenOperator:
		return "operator"
	case TokenParenOpen:
		return "'('"
	case TokenParenClose:
		return "')'"
	case TokenBraceOpen:
		return "'{'"
	case TokenBraceClose:
		return "'}'"
	case TokenBracketOpen:
		return "'['"
	case TokenBracketClose:
		return "']'"
	case TokenColon:
		return "':'"
	case TokenSemicolon:
		return "';'"
	case TokenComma:
		return "','"
	case TokenDefine:
		return "':='"
	case TokenQuestion:
		return "'?'"
	case TokenQuestionDot:
		return "'?.'"
	case TokenQuestionQuestion:
		return "'??'"
	case TokenAt:
		return "'@'"
	case TokenAmpAmp:
		return "'&&'"
	case TokenPipePipe:
		return "'||'"
	case TokenBang:
		return "'!'"
	case TokenDot:
		return "'.'"
	case TokenKeywordQuery:
		return "Query"
	case TokenKeywordMutation:
		return "Mutation"
	case TokenKeywordAutomation:
		return "Automation"
	case TokenKeywordSpec:
		return "Spec"
	case TokenKeywordTool:
		return "Tool"
	case TokenKeywordBuiltin:
		return "Builtin"
	case TokenKeywordFunc:
		return "func"
	case TokenKeywordFor:
		return "for"
	case TokenKeywordRange:
		return "range"
	case TokenKeywordIf:
		return "if"
	case TokenKeywordElse:
		return "else"
	case TokenKeywordSwitch:
		return "switch"
	case TokenKeywordCase:
		return "case"
	case TokenKeywordDefault:
		return "default"
	case TokenKeywordContinue:
		return "continue"
	case TokenKeywordBreak:
		return "break"
	case TokenKeywordReturn:
		return "return"
	case TokenKeywordNil:
		return "nil"
	case TokenKeywordRetry:
		return "retry"
	case TokenKeywordWhen:
		return "when"
	case TokenKeywordAs:
		return "as"
	case TokenKeywordWhere:
		return "where"
	case TokenKeywordUse:
		return "use"
	case TokenKeywordImport:
		return "import"
	case TokenKeywordConcept:
		return "concept"
	case TokenKeywordIn:
		return "in"
	case TokenKeywordHas:
		return "has"
	case TokenKeywordNot:
		return "not"
	default:
		return fmt.Sprintf("unknown-token(%d)", int(t))
	}
}

// Lexer converts a MemQL string into tokens.
type Lexer struct {
	input  []rune
	pos    int
	line   int
	column int

	// docBlocks collects /// doc-comment blocks captured at lex time
	// (memql#2633): consecutive /// lines merge into one block. The token
	// stream is untouched -- ~24 consumers iterate it positionally, the
	// terse-automation migrator compares two streams for equality, and
	// sense emits its own comment tokens -- so doc comments travel on this
	// side channel and reach the parser via SetDocComments.
	docBlocks []DocCommentBlock
	// lastTokenLine is the last line on which a real token ended -- the
	// trailing-comment guard: a comment on a line that already carries
	// code is neither a doc comment nor a transparent line (a code line
	// breaks attachment per ruling 1), so the side channel ignores it.
	lastTokenLine int
	// commentLines marks lines occupied by ordinary (non-///) comments,
	// so the parser's attachment walk can treat them as transparent per
	// the design's ruling 1 (docs/internal/design/
	// doc-comments-description-source.md).
	commentLines map[int]bool
}

// DocCommentBlock is a run of consecutive /// lines. Lines hold each
// line's content after the /// prefix, unstripped; the ruling-2 join
// (JoinDocComment) happens at attachment time.
type DocCommentBlock struct {
	Lines     []string
	StartLine int
	EndLine   int
}

// DocComments returns the /// blocks and ordinary-comment line set the
// lexer captured. Valid after Tokenize.
func (l *Lexer) DocComments() ([]DocCommentBlock, map[int]bool) {
	return l.docBlocks, l.commentLines
}

func (l *Lexer) markCommentLines(from, to int) {
	if l.commentLines == nil {
		l.commentLines = make(map[int]bool)
	}
	for i := from; i <= to; i++ {
		l.commentLines[i] = true
	}
}

// NewLexer creates a new lexer for the given input.
func NewLexer(input string) *Lexer {
	return &Lexer{
		input:  []rune(input),
		pos:    0,
		line:   1,
		column: 1,
	}
}

// Tokenize converts the entire input into a slice of tokens. Each
// token's End* fields are stamped from the lexer's post-scan
// position so consumers can read the source span without
// re-implementing per-token length math. Specifically:
// `len(Literal)` doesn't match the source length for string tokens
// (quotes are stripped, escape sequences are decoded), so a span
// derived from `Column + len(Literal)` would be too short by at
// least 2 (the quotes) -- which surfaced as a trailing-cell color
// bleed in the cockpit's viewer (memql-cockpit#114).
func (l *Lexer) Tokenize() ([]Token, error) {
	var tokens []Token
	for {
		tok, err := l.NextToken()
		if err != nil {
			return nil, err
		}
		// The scan functions advance l.pos / l.line / l.column past
		// the consumed token before returning. Capturing those values
		// HERE -- before the next NextToken's skipWhitespace fires --
		// gives us the half-open end-position of the just-returned
		// token. TokenEOF is its own case: the EOF token has zero
		// width, so End* == start.
		if tok.Type == TokenEOF {
			tok.EndPos = tok.Pos
			tok.EndLine = tok.Line
			tok.EndCol = tok.Column
		} else {
			tok.EndPos = l.pos
			tok.EndLine = l.line
			tok.EndCol = l.column
			l.lastTokenLine = l.line
			// A line that carries a token is CODE, not a transparent
			// comment line -- even when a block comment opened it
			// (`/* note */ use ...`, the leading-side mirror of the
			// trailing-comment guard; memql#2633 review round 2).
			for i := tok.Line; i <= l.line; i++ {
				delete(l.commentLines, i)
			}
		}
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	return tokens, nil
}

// NextToken returns the next token from the input.
func (l *Lexer) NextToken() (Token, error) {
	l.skipWhitespace()

	if l.eof() {
		return Token{Type: TokenEOF, Pos: l.pos, Line: l.line, Column: l.column}, nil
	}

	start := l.pos
	startLine := l.line
	startColumn := l.column
	ch := l.peek()

	makeToken := func(typ TokenType, literal string) Token {
		return Token{Type: typ, Literal: literal, Pos: start, Line: startLine, Column: startColumn}
	}

	switch ch {
	case '$':
		l.advance()
		return makeToken(TokenOperator, "$"), nil
	case '<':
		l.advance()
		if !l.eof() && l.peek() == '=' {
			l.advance()
			return makeToken(TokenOperator, "<="), nil
		}
		return makeToken(TokenOperator, "<"), nil
	case '>':
		l.advance()
		if !l.eof() && l.peek() == '=' {
			l.advance()
			return makeToken(TokenOperator, ">="), nil
		}
		return makeToken(TokenOperator, ">"), nil
	case '(':
		l.advance()
		return makeToken(TokenParenOpen, "("), nil
	case ')':
		l.advance()
		return makeToken(TokenParenClose, ")"), nil
	case ';':
		l.advance()
		return makeToken(TokenSemicolon, ";"), nil
	case ',':
		l.advance()
		return makeToken(TokenComma, ","), nil
	case '{':
		l.advance()
		return makeToken(TokenBraceOpen, "{"), nil
	case '}':
		l.advance()
		return makeToken(TokenBraceClose, "}"), nil
	case '[':
		l.advance()
		return makeToken(TokenBracketOpen, "["), nil
	case ']':
		l.advance()
		return makeToken(TokenBracketClose, "]"), nil
	case ':':
		l.advance()
		if !l.eof() && l.peek() == '=' {
			l.advance()
			return makeToken(TokenDefine, ":="), nil
		}
		return makeToken(TokenColon, ":"), nil
	case '?':
		l.advance()
		if !l.eof() && l.peek() == '.' {
			l.advance()
			return makeToken(TokenQuestionDot, "?."), nil
		}
		if !l.eof() && l.peek() == '?' {
			l.advance()
			return makeToken(TokenQuestionQuestion, "??"), nil
		}
		return makeToken(TokenQuestion, "?"), nil
	case '&':
		l.advance()
		if !l.eof() && l.peek() == '&' {
			l.advance()
			return makeToken(TokenAmpAmp, "&&"), nil
		}
		return Token{}, fmt.Errorf("unexpected '&' at position %d (did you mean '&&'?)", start)
	case '|':
		l.advance()
		if !l.eof() && l.peek() == '|' {
			l.advance()
			return makeToken(TokenPipePipe, "||"), nil
		}
		return Token{}, fmt.Errorf("unexpected '|' at position %d (did you mean '||'?)", start)
	case '@':
		l.advance()
		return makeToken(TokenAt, "@"), nil
	case '.':
		// A `.` followed by an alnum starts a dotted-path identifier
		// (e.g. the `.payload.value` tail after `)`). A `.` followed
		// by anything else is the standalone separator used in the
		// new `use path.{ names }` import syntax.
		if l.hasNext() && (unicode.IsLetter(l.peekNext()) || unicode.IsDigit(l.peekNext()) || l.peekNext() == '_') {
			return l.scanIdentifier(start, startLine, startColumn)
		}
		l.advance()
		return makeToken(TokenDot, "."), nil
	case '"':
		return l.scanString(start, startLine, startColumn)
	case '!':
		return l.scanNotEquals(start, startLine, startColumn)
	case '=':
		return l.scanOperator(start, startLine, startColumn)
	case '-':
		// A `-` immediately followed by a digit is a negative numeric
		// literal (`-5`), preserved exactly as before (#2316). Otherwise
		// `-` is the subtraction / unary-minus operator. Note `-` is also
		// an identifier character (node ids like `bff-local`), so a `-`
		// glued to an identifier with no leading whitespace is absorbed by
		// scanIdentifier and never reaches this case; subtraction therefore
		// needs the `-` surrounded by spaces (`a - b`).
		if l.hasNext() && isDigit(l.peekNext()) {
			return l.scanNumber(start, startLine, startColumn)
		}
		l.advance()
		return makeToken(TokenOperator, "-"), nil
	case '/':
		// Check for Go-style comments (//, /* */)
		if l.hasNext() && l.peekNext() == '/' {
			l.skipLineComment()
			return l.NextToken() // recurse to get next real token
		}
		if l.hasNext() && l.peekNext() == '*' {
			l.skipBlockComment()
			return l.NextToken() // recurse to get next real token
		}
		// A standalone `/` (not `//` or `/*`) is the division operator (#2316).
		l.advance()
		return makeToken(TokenOperator, "/"), nil
	case '+':
		// Addition operator (#2316).
		l.advance()
		return makeToken(TokenOperator, "+"), nil
	case '*':
		// Multiplication operator (#2316).
		l.advance()
		return makeToken(TokenOperator, "*"), nil
	case '%':
		// Modulo operator (#2316).
		l.advance()
		return makeToken(TokenOperator, "%"), nil
	}

	if isDigit(ch) {
		return l.scanNumber(start, startLine, startColumn)
	}

	return l.scanIdentifier(start, startLine, startColumn)
}

func (l *Lexer) scanString(start, startLine, startColumn int) (Token, error) {
	l.advance() // consume opening quote
	var builder strings.Builder

	for !l.eof() {
		ch := l.peek()
		if ch == '"' {
			l.advance()
			return Token{
				Type:    TokenString,
				Literal: builder.String(),
				Pos:     start,
				Line:    startLine,
				Column:  startColumn,
			}, nil
		}
		if ch == '\\' {
			if !l.hasNext() {
				break
			}
			l.advance()
			escaped := l.peek()
			switch escaped {
			case '"', '\\', '/':
				builder.WriteRune(escaped)
			case 'b':
				builder.WriteRune('\b')
			case 'f':
				builder.WriteRune('\f')
			case 'n':
				builder.WriteRune('\n')
			case 'r':
				builder.WriteRune('\r')
			case 't':
				builder.WriteRune('\t')
			case 'u':
				r, err := l.scanUnicodeEscape()
				if err != nil {
					return Token{}, fmt.Errorf("invalid unicode escape at position %d: %w", l.pos, err)
				}
				builder.WriteRune(r)
			default:
				return Token{}, fmt.Errorf("invalid escape character %q at position %d", escaped, l.pos)
			}
			l.advance()
			continue
		}
		// A literal newline INSIDE the string still moves the source cursor to
		// the next line, so the line counter has to move with it (memql#3047).
		// Without this, every position derived after a multi-line literal was
		// short by the newlines inside it and the drift COMPOUNDED per literal
		// -- silently, because the parse was fine and only the reported
		// locations were wrong, which is the whole value of a diagnostic.
		//
		// Keyed on the raw source byte, NOT on the decoded rune: an escaped
		// `\n` is two source bytes on ONE line, and it is handled by the
		// escape arm above, which writes '\n' into the builder without ever
		// reaching here. Advancing on the decoded rune would mis-count every
		// single-line literal in the tree that contains such an escape.
		//
		// Column goes to 0 because the advance() below takes it to 1 -- the
		// same shape skipBlockComment uses for its own newline arm.
		if ch == '\n' {
			l.line++
			l.column = 0
		}
		builder.WriteRune(ch)
		l.advance()
	}

	return Token{}, fmt.Errorf("unterminated string starting at position %d", start)
}

func (l *Lexer) scanUnicodeEscape() (rune, error) {
	if l.pos+4 >= len(l.input) {
		return 0, errors.New("incomplete unicode escape sequence")
	}

	var value rune
	for i := 0; i < 4; i++ {
		next := l.peekAt(i + 1)
		value <<= 4
		switch {
		case next >= '0' && next <= '9':
			value += next - '0'
		case next >= 'a' && next <= 'f':
			value += next - 'a' + 10
		case next >= 'A' && next <= 'F':
			value += next - 'A' + 10
		default:
			return 0, fmt.Errorf("invalid unicode escape %q", string(next))
		}
	}

	l.pos += 4
	l.column += 4
	return value, nil
}

func (l *Lexer) scanNotEquals(start, startLine, startColumn int) (Token, error) {
	l.advance() // consume '!'
	if l.eof() {
		return Token{Type: TokenBang, Literal: "!", Pos: start, Line: startLine, Column: startColumn}, nil
	}

	// !=
	if l.match('=') {
		return Token{Type: TokenOperator, Literal: "!=", Pos: start, Line: startLine, Column: startColumn}, nil
	}

	// Standalone ! for boolean negation
	return Token{Type: TokenBang, Literal: "!", Pos: start, Line: startLine, Column: startColumn}, nil
}

func (l *Lexer) scanOperator(start, startLine, startColumn int) (Token, error) {
	l.advance() // consume '='

	// Check for ==
	if l.match('=') {
		return Token{Type: TokenOperator, Literal: "==", Pos: start, Line: startLine, Column: startColumn}, nil
	}

	// Check for => (arrow lambda, Story 4 / ADR §2.2 collection methods).
	// `args.members.where(m => m.active)` needs `=>` to separate the lambda
	// param list from its body. It is only valid inside a collection method
	// call; the parser rejects it elsewhere.
	if l.match('>') {
		return Token{Type: TokenOperator, Literal: "=>", Pos: start, Line: startLine, Column: startColumn}, nil
	}

	// Single =
	return Token{Type: TokenOperator, Literal: "=", Pos: start, Line: startLine, Column: startColumn}, nil
}

func (l *Lexer) scanNumber(start, startLine, startColumn int) (Token, error) {
	var builder strings.Builder

	// Handle negative sign
	if l.peek() == '-' {
		builder.WriteRune('-')
		l.advance()
	}

	// Integer part
	for !l.eof() && isDigit(l.peek()) {
		builder.WriteRune(l.peek())
		l.advance()
	}

	// Decimal part
	if !l.eof() && l.peek() == '.' && l.hasNext() && isDigit(l.peekNext()) {
		builder.WriteRune('.')
		l.advance()
		for !l.eof() && isDigit(l.peek()) {
			builder.WriteRune(l.peek())
			l.advance()
		}
	}

	// Exponent part
	if !l.eof() && (l.peek() == 'e' || l.peek() == 'E') {
		builder.WriteRune(l.peek())
		l.advance()
		if !l.eof() && (l.peek() == '+' || l.peek() == '-') {
			builder.WriteRune(l.peek())
			l.advance()
		}
		for !l.eof() && isDigit(l.peek()) {
			builder.WriteRune(l.peek())
			l.advance()
		}
	}

	return Token{Type: TokenNumber, Literal: builder.String(), Pos: start, Line: startLine, Column: startColumn}, nil
}

func (l *Lexer) scanIdentifier(start, startLine, startColumn int) (Token, error) {
	var builder strings.Builder

	for !l.eof() {
		ch := l.peek()
		// Handle colon specially - include only when the NEXT char starts a new
		// identifier segment, i.e. a LETTER (concept names like v1:crm:lead).
		// A ':' followed by a DIGIT is an object-key separator before a numeric
		// value -- e.g. the hand-built literal `{tokenBudget:300000}` -- and was
		// previously mis-lexed as a single valueless key `tokenBudget:300000`,
		// failing the parser at `}` (#1118). Concept-id segments in DSL source
		// always start with a letter (a node id's UUID shortId carries hyphens
		// that already terminate the scan), so this never splits a real concept id.
		if ch == ':' {
			if l.hasNext() && unicode.IsLetter(l.peekNext()) {
				builder.WriteRune(ch)
				l.advance()
				continue
			}
			// Colon followed by a non-letter is a separator, stop here.
			break
		}
		// Handle dot specially - only include if followed by an alnum/`_` (for
		// dotted paths like `payload.X`, `cognition.shapes`). A trailing
		// dot (e.g. before `{` in the new file-top `use path.{ ... }`
		// imports form) is NOT consumed -- the lexer stops, leaving the
		// `.` to be tokenised as a separator.
		if ch == '.' {
			if l.hasNext() && (unicode.IsLetter(l.peekNext()) || unicode.IsDigit(l.peekNext()) || l.peekNext() == '_') {
				builder.WriteRune(ch)
				l.advance()
				continue
			}
			break
		}
		if isIdentifierCharNoColon(ch) {
			builder.WriteRune(ch)
			l.advance()
		} else {
			break
		}
	}

	literal := builder.String()
	if literal == "" {
		return Token{}, fmt.Errorf("unexpected character %q at position %d", l.peek(), l.pos)
	}

	// Check for keywords (case-sensitive, Go-style)
	tokenType := TokenIdentifier

	switch literal {
	// Control flow
	case "func":
		tokenType = TokenKeywordFunc
	case "for":
		tokenType = TokenKeywordFor
	case "range":
		tokenType = TokenKeywordRange
	case "if":
		tokenType = TokenKeywordIf
	case "else":
		tokenType = TokenKeywordElse
	case "switch":
		tokenType = TokenKeywordSwitch
	case "case":
		tokenType = TokenKeywordCase
	case "default":
		tokenType = TokenKeywordDefault
	case "continue":
		tokenType = TokenKeywordContinue
	case "break":
		tokenType = TokenKeywordBreak
	case "return":
		tokenType = TokenKeywordReturn
	case "nil":
		tokenType = TokenKeywordNil
	case "retry":
		tokenType = TokenKeywordRetry
	case "when":
		tokenType = TokenKeywordWhen
	case "as":
		tokenType = TokenKeywordAs
	case "where":
		tokenType = TokenKeywordWhere
	case "use":
		tokenType = TokenKeywordUse
	case "import":
		tokenType = TokenKeywordImport
	case "in":
		tokenType = TokenKeywordIn
	case "has":
		tokenType = TokenKeywordHas
	case "not":
		tokenType = TokenKeywordNot
	// "concept" is intentionally NOT a keyword — it's used as a field name
	// in queries (concept==v1:cognition:participant). Will be added as a
	// contextual keyword in Phase 3 (concept definition parser).
	// Type receivers (capitalized like Go types)
	case "Query":
		tokenType = TokenKeywordQuery
	case "Mutation":
		tokenType = TokenKeywordMutation
	case "Automation":
		tokenType = TokenKeywordAutomation
	case "Spec":
		tokenType = TokenKeywordSpec
	case "Tool":
		tokenType = TokenKeywordTool
	case "Builtin":
		tokenType = TokenKeywordBuiltin
	}

	return Token{Type: tokenType, Literal: literal, Pos: start, Line: startLine, Column: startColumn}, nil
}

func (l *Lexer) skipWhitespace() {
	for !l.eof() {
		ch := l.peek()
		if ch == ' ' || ch == '\t' || ch == '\r' {
			l.advance()
		} else if ch == '\n' {
			l.advance()
			l.line++
			l.column = 1
		} else if ch == '/' && l.hasNext() && l.peekNext() == '/' {
			// Go-style line comment (//)
			l.skipLineComment()
		} else if ch == '/' && l.hasNext() && l.peekNext() == '*' {
			// Go-style block comment (/* */)
			l.skipBlockComment()
		} else {
			break
		}
	}
}

func (l *Lexer) skipLineComment() {
	// Skip to end of line, capturing /// doc-comment content on the side
	// channel (memql#2633). Exactly three slashes is a doc comment; //// and
	// longer runs (divider art) stay ordinary comments.
	startLine := l.line
	start := l.pos
	for !l.eof() && l.peek() != '\n' {
		l.advance()
	}
	if startLine == l.lastTokenLine {
		// Trailing comment after code: not a doc comment, and the code
		// line stays opaque so it breaks attachment (memql#2633 review).
		return
	}
	text := string(l.input[start:l.pos])
	if strings.HasPrefix(text, "///") && !strings.HasPrefix(text, "////") {
		content := text[3:]
		if n := len(l.docBlocks); n > 0 && l.docBlocks[n-1].EndLine == startLine-1 {
			l.docBlocks[n-1].Lines = append(l.docBlocks[n-1].Lines, content)
			l.docBlocks[n-1].EndLine = startLine
		} else {
			l.docBlocks = append(l.docBlocks, DocCommentBlock{Lines: []string{content}, StartLine: startLine, EndLine: startLine})
		}
		return
	}
	l.markCommentLines(startLine, startLine)
}

func (l *Lexer) skipBlockComment() {
	// Skip /* ... */ block comment; its full line span is transparent for
	// doc-comment attachment (memql#2633).
	startLine := l.line
	if startLine != l.lastTokenLine {
		defer func() { l.markCommentLines(startLine, l.line) }()
	}
	l.advance() // consume '/'
	l.advance() // consume '*'

	for !l.eof() {
		if l.peek() == '*' && l.hasNext() && l.peekNext() == '/' {
			l.advance() // consume '*'
			l.advance() // consume '/'
			return
		}
		if l.peek() == '\n' {
			l.line++
			l.column = 0 // will be 1 after advance
		}
		l.advance()
	}
}

// Helper methods

func (l *Lexer) eof() bool {
	return l.pos >= len(l.input)
}

func (l *Lexer) peek() rune {
	if l.eof() {
		return 0
	}
	return l.input[l.pos]
}

func (l *Lexer) peekNext() rune {
	if l.pos+1 >= len(l.input) {
		return 0
	}
	return l.input[l.pos+1]
}

func (l *Lexer) peekAt(offset int) rune {
	if l.pos+offset >= len(l.input) {
		return 0
	}
	return l.input[l.pos+offset]
}

func (l *Lexer) hasNext() bool {
	return l.pos+1 < len(l.input)
}

func (l *Lexer) advance() {
	if !l.eof() {
		l.pos++
		l.column++
	}
}

func (l *Lexer) match(expected rune) bool {
	if l.eof() || l.peek() != expected {
		return false
	}
	l.advance()
	return true
}

func isDigit(ch rune) bool {
	return ch >= '0' && ch <= '9'
}

func isIdentifierCharNoColon(ch rune) bool {
	// Like isIdentifierChar but without ':' / '.' - both are handled
	// specially in the scanner so they only join an identifier when
	// the next rune is alphanumeric.
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' || ch == '-'
}
