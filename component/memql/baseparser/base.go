// Package baseparser holds the shared scanning + annotation primitives
// every dedicated DSL parser (shape / spec / tool / prompt / provider /
// policy / builtin) embeds via Go struct composition:
//
//	type myConstructParser struct {
//	    baseparser.Base
//	    // construct-specific state
//	}
//
// All scanning primitives (EOF, Peek, Advance, SkipWhitespace*,
// ReadWord, ParseParenString, etc.) live here. Construct-specific
// grammar lives on the embedder. Errorf wraps fmt.Errorf with the
// origin + current line:col prefix so every error from every parser
// has consistent location data.
package baseparser

import (
	"fmt"
	"strings"
	"unicode"
)

// Base is the shared scanning state every DSL parser embeds.
type Base struct {
	Input  string
	Pos    int
	Line   int
	Col    int
	Origin string
}

// Init resets the Base to parse a fresh input.
func (b *Base) Init(input, origin string) {
	b.Input = input
	b.Pos = 0
	b.Line = 1
	b.Col = 1
	b.Origin = origin
}

// EOF reports whether Pos has reached the end of Input.
func (b *Base) EOF() bool { return b.Pos >= len(b.Input) }

// Peek returns the byte at Pos. Returns 0 at EOF.
func (b *Base) Peek() byte {
	if b.EOF() {
		return 0
	}
	return b.Input[b.Pos]
}

// PeekAt returns the byte at Pos+offset. Returns 0 if out of range.
func (b *Base) PeekAt(offset int) byte {
	if b.Pos+offset < 0 || b.Pos+offset >= len(b.Input) {
		return 0
	}
	return b.Input[b.Pos+offset]
}

// HasNext reports whether Pos+1 < len(Input).
func (b *Base) HasNext() bool { return b.Pos+1 < len(b.Input) }

// Advance consumes one byte. Updates Line on '\n' (Col resets to 1),
// otherwise Col is incremented.
func (b *Base) Advance() {
	if b.EOF() {
		return
	}
	if b.Input[b.Pos] == '\n' {
		b.Line++
		b.Col = 1
	} else {
		b.Col++
	}
	b.Pos++
}

// SkipWhitespaceAndComments advances past whitespace, // line comments,
// and /* block comments */.
func (b *Base) SkipWhitespaceAndComments() {
	for !b.EOF() {
		ch := b.Peek()
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			b.Advance()
			continue
		}
		if ch == '/' && b.HasNext() && b.PeekAt(1) == '/' {
			b.SkipToEndOfLine()
			continue
		}
		if ch == '/' && b.HasNext() && b.PeekAt(1) == '*' {
			b.SkipBlockComment()
			continue
		}
		break
	}
}

// SkipWhitespaceInline advances past space and tab only.
func (b *Base) SkipWhitespaceInline() {
	for !b.EOF() {
		ch := b.Peek()
		if ch == ' ' || ch == '\t' {
			b.Advance()
			continue
		}
		break
	}
}

// SkipToEndOfLine advances to the next '\n' (or EOF).
func (b *Base) SkipToEndOfLine() {
	for !b.EOF() && b.Peek() != '\n' {
		b.Advance()
	}
}

// SkipBlockComment consumes a /* ... */ block. Pre: Pos at '/'.
func (b *Base) SkipBlockComment() {
	b.Advance() // /
	b.Advance() // *
	for !b.EOF() {
		if b.Peek() == '*' && b.HasNext() && b.PeekAt(1) == '/' {
			b.Advance() // *
			b.Advance() // /
			return
		}
		b.Advance()
	}
}

// SkipBalancedParens consumes a balanced (...) starting at Pos.
// No-op if Pos is not on '('. Tolerates "..." literals inside.
func (b *Base) SkipBalancedParens() {
	if b.EOF() || b.Peek() != '(' {
		return
	}
	depth := 0
	for !b.EOF() {
		ch := b.Peek()
		if ch == '(' {
			depth++
		} else if ch == ')' {
			depth--
			if depth == 0 {
				b.Advance()
				return
			}
		} else if ch == '"' {
			b.Advance()
			for !b.EOF() && b.Peek() != '"' {
				if b.Peek() == '\\' {
					b.Advance()
				}
				b.Advance()
			}
		}
		b.Advance()
	}
}

// SkipBalancedBraces consumes a balanced {...} starting at Pos.
// No-op if Pos is not on '{'. Tolerates "..." literals inside.
func (b *Base) SkipBalancedBraces() {
	if b.EOF() || b.Peek() != '{' {
		return
	}
	depth := 0
	for !b.EOF() {
		ch := b.Peek()
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				b.Advance()
				return
			}
		} else if ch == '"' {
			b.Advance()
			for !b.EOF() && b.Peek() != '"' {
				if b.Peek() == '\\' {
					b.Advance()
				}
				b.Advance()
			}
		}
		b.Advance()
	}
}

// SkipOptionalParens skips (...) when Pos is on '(' after an inline
// whitespace skip. Used by annotation drainers that tolerate either
// `@flag` or `@flag(...)`.
func (b *Base) SkipOptionalParens() {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return
	}
	b.SkipBalancedParens()
}

// SkipUseClauseBody consumes the body of a `use ...` statement after
// the `use` keyword has already been matched. Handles both file-top
// import shapes:
//
//	Form A (legacy):   use cognition.participant [as cog]
//	Form B (canonical): use cognition.concepts.{ participant, space }
//
// Form A is single-line: the helper skips to end of line. Form B may
// span multiple lines inside the braces; the helper consumes the
// dotted path, the `.`, and the balanced `{ ... }`. Used by construct
// parsers (shape/spec/builtin/...) that don't need to retain the
// imports themselves -- the languageParser-driven loader records them
// separately. The seed parser, which DOES need to know about Form A
// concept bindings, has its own use-clause parser.
func (b *Base) SkipUseClauseBody() {
	b.SkipWhitespaceInline()
	// Consume the dotted path (letters, digits, '_', '.', '-').
	for !b.EOF() {
		ch := b.Peek()
		if unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' || ch == '.' || ch == '-' {
			b.Advance()
			continue
		}
		break
	}
	b.SkipWhitespaceInline()
	if !b.EOF() && b.Peek() == '{' {
		// Form B brace-list body.
		b.SkipBalancedBraces()
		return
	}
	// Form A: skip rest of line (covers any `as <alias>`).
	b.SkipToEndOfLine()
}

// ReadWord reads an identifier ([A-Za-z0-9_]+). Returns "" when the
// first byte isn't an identifier char.
func (b *Base) ReadWord() string {
	var sb strings.Builder
	for !b.EOF() {
		ch := b.Peek()
		if unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' {
			sb.WriteByte(ch)
			b.Advance()
			continue
		}
		break
	}
	return sb.String()
}

// MatchWord matches the literal at Pos with word-boundary semantics
// (the byte after the literal must not be an identifier char).
// Advances Pos + Col on match; leaves them untouched otherwise.
func (b *Base) MatchWord(word string) bool {
	if b.Pos+len(word) > len(b.Input) {
		return false
	}
	if b.Input[b.Pos:b.Pos+len(word)] != word {
		return false
	}
	next := b.Pos + len(word)
	if next < len(b.Input) {
		ch := b.Input[next]
		if unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' {
			return false
		}
	}
	b.Pos += len(word)
	b.Col += len(word)
	return true
}

// ParseParenString parses `("string")` and returns the unescaped value.
func (b *Base) ParseParenString() (string, error) {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return "", fmt.Errorf("expected '(' for string argument")
	}
	b.Advance()
	b.SkipWhitespaceAndComments()

	val, err := b.ReadQuotedString()
	if err != nil {
		return "", err
	}

	b.SkipWhitespaceAndComments()
	if b.EOF() || b.Peek() != ')' {
		return "", fmt.Errorf("expected ')' after string argument")
	}
	b.Advance()
	return val, nil
}

// ParseParenIdent parses `(ident)` and returns the bare identifier.
func (b *Base) ParseParenIdent() (string, error) {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return "", fmt.Errorf("expected '(' for identifier argument")
	}
	b.Advance()
	b.SkipWhitespaceAndComments()
	ident := b.ReadWord()
	if ident == "" {
		return "", fmt.Errorf("expected identifier inside parens")
	}
	b.SkipWhitespaceAndComments()
	if b.EOF() || b.Peek() != ')' {
		return "", fmt.Errorf("expected ')' after identifier argument")
	}
	b.Advance()
	return ident, nil
}

// ParseParenStringList parses `("a", "b", ...)`.
func (b *Base) ParseParenStringList() ([]string, error) {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return nil, fmt.Errorf("expected '(' for string list")
	}
	b.Advance()

	var result []string
	for !b.EOF() {
		b.SkipWhitespaceAndComments()
		if b.Peek() == ')' {
			b.Advance()
			return result, nil
		}

		val, err := b.ReadQuotedString()
		if err != nil {
			return nil, err
		}
		result = append(result, val)

		b.SkipWhitespaceAndComments()
		if !b.EOF() && b.Peek() == ',' {
			b.Advance()
		}
	}
	return nil, fmt.Errorf("unterminated string list")
}

// ParseParenIdentList parses `(a, b, c)` (bare identifiers).
func (b *Base) ParseParenIdentList() ([]string, error) {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return nil, fmt.Errorf("expected '(' for ident list")
	}
	b.Advance()

	var result []string
	for !b.EOF() {
		b.SkipWhitespaceAndComments()
		if b.Peek() == ')' {
			b.Advance()
			return result, nil
		}
		w := b.ReadWord()
		if w == "" {
			return nil, fmt.Errorf("expected identifier, got %q", string(b.Peek()))
		}
		result = append(result, w)
		b.SkipWhitespaceAndComments()
		if !b.EOF() && b.Peek() == ',' {
			b.Advance()
		}
	}
	return nil, fmt.Errorf("unterminated ident list")
}

// ParseParenInt parses `(123)` and returns the integer.
func (b *Base) ParseParenInt() (int64, error) {
	b.SkipWhitespaceInline()
	if b.EOF() || b.Peek() != '(' {
		return 0, fmt.Errorf("expected '(' for integer argument")
	}
	b.Advance()
	b.SkipWhitespaceAndComments()

	var sb strings.Builder
	for !b.EOF() {
		ch := b.Peek()
		if ch >= '0' && ch <= '9' {
			sb.WriteByte(ch)
			b.Advance()
			continue
		}
		break
	}
	if sb.Len() == 0 {
		return 0, fmt.Errorf("expected integer")
	}
	b.SkipWhitespaceAndComments()
	if b.EOF() || b.Peek() != ')' {
		return 0, fmt.Errorf("expected ')' after integer argument")
	}
	b.Advance()

	var n int64
	for _, c := range sb.String() {
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// ReadQuotedString reads a "..." literal at Pos, handling backslash
// escapes (`\"`, `\\`, `\/`, `\n`, `\t`, `\r`; other escapes pass
// through with the backslash preserved).
func (b *Base) ReadQuotedString() (string, error) {
	if b.EOF() || b.Peek() != '"' {
		return "", fmt.Errorf("expected '\"' to start string")
	}
	b.Advance()

	var sb strings.Builder
	for !b.EOF() {
		ch := b.Peek()
		if ch == '"' {
			b.Advance()
			return sb.String(), nil
		}
		if ch == '\\' {
			b.Advance()
			if !b.EOF() {
				escaped := b.Peek()
				switch escaped {
				case '"', '\\', '/':
					sb.WriteByte(escaped)
				case 'n':
					sb.WriteByte('\n')
				case 't':
					sb.WriteByte('\t')
				case 'r':
					sb.WriteByte('\r')
				default:
					sb.WriteByte('\\')
					sb.WriteByte(escaped)
				}
				b.Advance()
				continue
			}
		}
		sb.WriteByte(ch)
		b.Advance()
	}
	return "", fmt.Errorf("unterminated string")
}

// FindClosingBrace returns the index of the '}' that matches an
// opening brace which has already been consumed. start is the byte
// index of the first character inside the body. Tolerates nested
// braces, "..." literals, and // line comments.
func (b *Base) FindClosingBrace(start int) (int, error) {
	depth := 1
	inString := false
	i := start
	for i < len(b.Input) {
		c := b.Input[i]
		if inString {
			if c == '\\' && i+1 < len(b.Input) {
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(b.Input) && b.Input[i+1] == '/' {
				for i < len(b.Input) && b.Input[i] != '\n' {
					i++
				}
				continue
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
		i++
	}
	return -1, fmt.Errorf("unbalanced braces")
}

// PosPlus returns Pos+n clamped to len(Input). Used by error messages
// to print a short preview slice without panicking on EOF.
func (b *Base) PosPlus(n int) int {
	end := b.Pos + n
	if end > len(b.Input) {
		end = len(b.Input)
	}
	return end
}

// Errorf wraps fmt.Errorf with the "origin:line:col: " prefix so every
// parser error carries consistent location data.
func (b *Base) Errorf(format string, args ...any) error {
	return fmt.Errorf("%s:%d:%d: %s", b.Origin, b.Line, b.Col, fmt.Sprintf(format, args...))
}
