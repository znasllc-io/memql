package compiler

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestParseFileSourceNamesTheAuthoredLine (memql#5356): the rewriter lowers
// the first query to more lines than the author wrote, so the parser meets
// the annotation written on line 12 at a later line of the text it reads. The
// error must name line 12, the line the author wrote.
func TestParseFileSourceNamesTheAuthoredLine(t *testing.T) {
	src := `use demo.concepts.{ item }

@enabled
@description("First query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}

@bogus
@enabled
@description("Second query.")
query item queryOthers {
  args {
    status  string  @required
  }
  filter  status == args.status
}
`
	if got := strings.Split(src, "\n")[11]; got != "@bogus" {
		t.Fatalf("the fixture's line 12 is %q, want @bogus", got)
	}
	_, err := ParseFileSource(src)
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a parse error for @bogus, got %v", err)
	}
	if pe.Line != 12 || pe.Column != 1 || !strings.Contains(err.Error(), "parse error at line 12, column 1:") ||
		!strings.Contains(err.Error(), "unknown annotation @bogus") {
		t.Errorf("the error must name line 12, column 1 and the annotation, got line %d, column %d: %v", pe.Line, pe.Column, err)
	}
}
