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
type ParseError struct {
	Message string
	Pos     int
	Line    int
	Column  int
	Token   *Token
	// Cause is the typed error behind the message, when there is one -- an
	// annotation registry refusal (memql#5359) -- so a caller can recover its
	// stable code with errors.As rather than by matching text.
	Cause error
}

func (e *ParseError) Error() string {
	if e.Token != nil {
		return fmt.Sprintf("parse error at line %d, column %d: %s (got %q)",
			e.Line, e.Column, e.Message, e.Token.Literal)
	}
	if e.Line > 0 {
		return fmt.Sprintf("parse error at line %d, column %d: %s", e.Line, e.Column, e.Message)
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

// newParseError creates a new ParseError.
func newParseError(msg string, tok *Token) *ParseError {
	err := &ParseError{
		Message: msg,
	}
	if tok != nil {
		err.Token = tok
		err.Pos = tok.Pos
		err.Line = tok.Line
		err.Column = tok.Column
	}
	return err
}

// newParseErrorf creates a new ParseError with a formatted message.
func newParseErrorf(tok *Token, format string, args ...any) *ParseError {
	return newParseError(fmt.Sprintf(format, args...), tok)
}
