package parser

import (
	"errors"
	"fmt"
	"testing"
)

// TestAtAuthoredLine: a parse error raised over rewritten text moves onto the
// line the author wrote (memql#5356). A preserved line keeps its column, a
// synthesized one points at column 1, a construct-keyword refusal it carries
// moves with it, and an error with no position is left alone.
func TestAtAuthoredLine(t *testing.T) {
	// The lowering inserted two synthesized lines after "a": authored line 2
	// ("b") is rewritten line 4.
	authored := "a\nb\nc\n"
	rewritten := "a\nX\nY\nb\nc\n"

	preserved := &ParseError{Message: "m", Line: 4, Column: 3,
		Cause: &UnknownConstructKeyword{Line: 4, Keyword: "b", Message: "m"}}
	err := AtAuthoredLine(fmt.Errorf("parser error: %w", preserved), authored, rewritten)
	var pe *ParseError
	if !errors.As(err, &pe) || pe != preserved {
		t.Fatalf("the error must still carry the parse error it was given, got %v", err)
	}
	if pe.Line != 2 || pe.Column != 3 {
		t.Errorf("a preserved line: got line %d column %d, want line 2 column 3", pe.Line, pe.Column)
	}
	if u := pe.Cause.(*UnknownConstructKeyword); u.Line != 2 {
		t.Errorf("the refusal it carries must move with it: got line %d, want 2", u.Line)
	}

	synthesized := &ParseError{Message: "m", Line: 2, Column: 9}
	AtAuthoredLine(synthesized, authored, rewritten)
	if synthesized.Line != 1 || synthesized.Column != 1 {
		t.Errorf("a synthesized line: got line %d column %d, want the construct's line 1 at column 1",
			synthesized.Line, synthesized.Column)
	}

	unpositioned := &ParseError{Message: "no position", Pos: 7}
	for _, e := range []error{ErrEmptyInput, unpositioned} {
		if got := AtAuthoredLine(e, authored, rewritten); got != e {
			t.Errorf("%v must come back as it was, got %v", e, got)
		}
	}
	if unpositioned.Line != 0 || unpositioned.Pos != 7 {
		t.Errorf("an unpositioned parse error must be left alone, got %+v", unpositioned)
	}
}
