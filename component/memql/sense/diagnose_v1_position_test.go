package sense

// diagnose_v1_position_test.go -- an editor's squiggle for a refusal inside a
// construct the rewriter lowered lands on the token the author wrote, and the
// message names the author's line and column (memql#5364).

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// senseAuthoredAt is where the first occurrence of needle starts in src: the
// 1-based line and rune column Sense reports.
func senseAuthoredAt(t *testing.T, src, needle string) Position {
	t.Helper()
	off := strings.Index(src, needle)
	if off < 0 {
		t.Fatalf("%q is not in the source", needle)
	}
	lineStart := strings.LastIndex(src[:off], "\n") + 1
	return Position{Line: 1 + strings.Count(src[:off], "\n"), Column: 1 + utf8.RuneCountInString(src[lineStart:off])}
}

// TestDiagnose_V1RefusalLandsOnTheAuthorsToken: at every predicate position
// the rewriter moves (a filter into the synthesized return, refine after
// paginate, a terse automation's @filter onto a line of its own) or shifts
// (whatever sits below a lowered construct), the diagnostic's range is exactly
// the offending token, and its message prints that token's position.
func TestDiagnose_V1RefusalLandsOnTheAuthorsToken(t *testing.T) {
	firstQuery := `use probe.concepts.{ thing }

/// Things by a.
query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
  shape thingCard
}
`
	cases := []struct {
		name, src, token string
	}{
		{"filter", `query thing byA {
  args {
    a string @required
  }
  filter row => row.a == args.a && row.name == "名前" && row.b == null
  paginate 10
}
`, "null"},
		{"a filter's continuation line", `query thing byA {
  filter row => row.a == "x" &&
         row.b == null
  paginate 10
}
`, "null"},
		{"the second query in a file", firstQuery + `
query thing second {
  filter row => row.b == null
  paginate 10
}
`, "null"},
		{"refine", `query thing refined {
  filter row => row.a == "x"
  paginate 10
  refine row => row.c == null
}
`, "null"},
		{"refine that is not a lambda", `query thing refined {
  filter row => row.a == "x"
  paginate 10
  refine row.c == nil
}
`, "row.c"},
		{"a hyphen glued into a name", `query thing kebab {
  filter row => row.total-used > 0
}
`, "row.total-used"},
		{"@filter below a query", firstQuery + `
@filter(row => row.b == null)
@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  first := logic other(x: 1)
}
`, "null"},
		{"a terse automation's @filter", `/// Note a thing when it is created.
automation onThing @trigger(event="node.created", concept="v1:probe:thing") @filter(row => row.b == null) => logic noteThing
`, "null"},
		{"a spec below a query", firstQuery + `
spec thing isB = row => row.b == null
`, "null"},
		{"a trait with nothing lowered", `trait isB = row => row.b == null
`, "null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := errorDiags(New(nil).Diagnose(c.src, "probe/queries.memql"))
			if len(errs) != 1 {
				t.Fatalf("want exactly one error diagnostic, got %d: %+v", len(errs), errs)
			}
			d := errs[0]
			start := senseAuthoredAt(t, c.src, c.token)
			end := Position{Line: start.Line, Column: start.Column + utf8.RuneCountInString(c.token)}
			if d.Range.Start != start || d.Range.End != end {
				t.Fatalf("range %d:%d-%d:%d, want %d:%d-%d:%d (the %q the author wrote): %s",
					d.Range.Start.Line, d.Range.Start.Column, d.Range.End.Line, d.Range.End.Column,
					start.Line, start.Column, end.Line, end.Column, c.token, d.Message)
			}
			if at := "at line " + strconv.Itoa(start.Line) + ", column " + strconv.Itoa(start.Column) + ":"; !strings.Contains(d.Message, at) {
				t.Fatalf("the message does not name the author's position %q: %s", at, d.Message)
			}
		})
	}
}
