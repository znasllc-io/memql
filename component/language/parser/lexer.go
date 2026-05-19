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
type Token struct {
	Type    TokenType
	Literal string
	Pos     int
	Line    int
	Column  int
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

// Tokenize converts the entire input into a slice of tokens.
func (l *Lexer) Tokenize() ([]Token, error) {
	var tokens []Token
	for {
		tok, err := l.NextToken()
		if err != nil {
			return nil, err
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
		// Check for negative number
		if l.hasNext() && isDigit(l.peekNext()) {
			return l.scanNumber(start, startLine, startColumn)
		}
		return l.scanIdentifier(start, startLine, startColumn)
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
		// Single / is not a valid token in MemQL
		return Token{}, fmt.Errorf("unexpected '/' at position %d", start)
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
		// Handle colon specially - only include if followed by alphanumeric (for concept names like v1:crm:lead)
		if ch == ':' {
			if l.hasNext() && (unicode.IsLetter(l.peekNext()) || unicode.IsDigit(l.peekNext())) {
				builder.WriteRune(ch)
				l.advance()
				continue
			}
			// Colon followed by non-alphanumeric is a separator, stop here
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
	// Skip to end of line (handles both -- and // comments)
	for !l.eof() && l.peek() != '\n' {
		l.advance()
	}
}

func (l *Lexer) skipBlockComment() {
	// Skip /* ... */ block comment
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

func (l *Lexer) matchSequence(chars ...rune) bool {
	if l.pos+len(chars) > len(l.input) {
		return false
	}
	for i, ch := range chars {
		if l.input[l.pos+i] != ch {
			return false
		}
	}
	l.pos += len(chars)
	l.column += len(chars)
	return true
}

func (l *Lexer) matchSequenceStr(s string) bool {
	runes := []rune(s)
	return l.matchSequence(runes...)
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
