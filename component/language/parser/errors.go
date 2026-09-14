package parser

import (
	"errors"
	"fmt"
)

// Parsing errors
var (
	ErrEmptyInput          = errors.New("empty input")
	ErrInvalidSyntax       = errors.New("invalid syntax")
	ErrUnexpectedToken     = errors.New("unexpected token")
	ErrUnexpectedEOF       = errors.New("unexpected end of input")
	ErrUnterminatedString  = errors.New("unterminated string")
	ErrInvalidNumber       = errors.New("invalid number")
	ErrInvalidOperator     = errors.New("invalid operator")
	ErrMissingArgument     = errors.New("missing argument")
	ErrInvalidArgument     = errors.New("invalid argument")
	ErrDuplicateDefinition = errors.New("duplicate definition")
)

// ParseError represents a parsing error with position information.
//
// Line and Column are the failing token's position in the text the parser
// lexed, and EndLine / EndColumn the first position after it. When that text
// is a struct-form lowering carrying position markers (PositionLowering), the
// Authored* fields hold the same token's extent in the author's source, and
// Error prints them: the lowered text is not a text anyone wrote. Position
// and EndPosition read whichever an author should see.
type ParseError struct {
	Message   string
	Pos       int
	Line      int
	Column    int
	EndLine   int
	EndColumn int
	Token     *Token

	// Cause is the typed error behind the message, when there is one -- an
	// annotation registry refusal (memql#5359) -- so a caller can recover its
	// stable code with errors.As rather than by matching text.
	Cause error

	AuthoredLine      int
	AuthoredColumn    int
	AuthoredEndLine   int
	AuthoredEndColumn int
}

// Position is where the error sits in the author's source: the authored
// position when the parsed text carried markers, the lexed one otherwise.
func (e *ParseError) Position() (line, col int) {
	if e.AuthoredLine > 0 {
		return e.AuthoredLine, e.AuthoredColumn
	}
	return e.Line, e.Column
}

// EndPosition is Position for the first position after the failing token;
// zero when the error carries no token extent.
func (e *ParseError) EndPosition() (line, col int) {
	if e.AuthoredLine > 0 {
		return e.AuthoredEndLine, e.AuthoredEndColumn
	}
	return e.EndLine, e.EndColumn
}

func (e *ParseError) Error() string {
	line, col := e.Position()
	if e.Token != nil {
		return fmt.Sprintf("parse error at line %d, column %d: %s (got %q)",
			line, col, e.Message, e.Token.Literal)
	}
	if line > 0 {
		return fmt.Sprintf("parse error at line %d, column %d: %s", line, col, e.Message)
	}
	return fmt.Sprintf("parse error at position %d: %s", e.Pos, e.Message)
}

// Unwrap reports ErrInvalidSyntax, and the typed Cause when there is one.
func (e *ParseError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrInvalidSyntax, e.Cause}
	}
	return []error{ErrInvalidSyntax}
}

// setToken positions e at tok, in both coordinate systems.
func (e *ParseError) setToken(tok Token) {
	e.Pos = tok.Pos
	e.Line = tok.Line
	e.Column = tok.Column
	e.EndLine = tok.EndLine
	e.EndColumn = tok.EndCol
	e.AuthoredLine = tok.AuthoredLine
	e.AuthoredColumn = tok.AuthoredCol
	e.AuthoredEndLine = tok.AuthoredEndLine
	e.AuthoredEndColumn = tok.AuthoredEndCol
}

// setEnd ends e's extent where tok ends, for a refusal that covers a run of
// tokens rather than one.
func (e *ParseError) setEnd(tok Token) {
	e.EndLine = tok.EndLine
	e.EndColumn = tok.EndCol
	if e.AuthoredLine > 0 && tok.AuthoredLine > 0 {
		e.AuthoredEndLine = tok.AuthoredEndLine
		e.AuthoredEndColumn = tok.AuthoredEndCol
	}
}

// newParseError creates a new ParseError.
func newParseError(msg string, tok *Token) *ParseError {
	err := &ParseError{
		Message: msg,
	}
	if tok != nil {
		err.Token = tok
		err.setToken(*tok)
	}
	return err
}

// newParseErrorf creates a new ParseError with a formatted message.
func newParseErrorf(tok *Token, format string, args ...any) *ParseError {
	return newParseError(fmt.Sprintf(format, args...), tok)
}
